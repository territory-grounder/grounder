package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/execclass"
)

// Runs against a REAL Postgres (TG_TEST_DSN) for the same reason correlation_test.go does: the whole claim
// here is a DB-identity one — that a durable alert_cluster row, joined by upsert, makes 40 members of one
// storm share ONE id no matter the arrival order — and a fake would prove only that the Go compiles.

// clusterGraphTopo adapts a real estate.Graph to correlate.Topology, exactly as the shipped
// temporal/runner.GraphTopology does (core/db cannot import temporal/runner — it would be an import cycle),
// so this test exercises the SAME estate reads (InDegree / RunsOnParent) production elects over.
type clusterGraphTopo struct{ g *estate.Graph }

func (t clusterGraphTopo) InDegree(host string) int { return t.g.InDegree(estate.Entity{Name: host}) }
func (t clusterGraphTopo) RunsOnParent(host string) string {
	if p, ok := t.g.RunsOnParent(estate.Entity{Name: host}); ok {
		return p.Name
	}
	return ""
}

// THE KILLING TEST for the CASCADE COLLAPSE (TG-385 / TG-376). It builds a 40-member cascade whose causal
// parent (a hypervisor) has a ref that sorts LAST (zz-...), runs the correlation stage's cluster-join +
// election + record for every member in arrival order over a real DB and a real estate graph, and asserts:
//
//	(i)   all 40 exec_class_decision rows carry the SAME cluster_id (the durable identity collapsed the storm);
//	(ii)  exactly ONE member is the elected subject (elected_ref == its own ref) — the one session that opens;
//	(iii) that elected subject is the PARENT, by estate in-degree, NOT the lexicographically-first ref.
//
// KILLING MUTATION 2 (executed, RED): make core/correlate.ClusterAnchor return the SUBJECT instead of the
// window's earliest member ("remove the cluster-id join so each member recomputes its window"). Each member
// then anchors on itself, inserts a distinct (bucket, first_seen_ref) row, and assertion (i) — one distinct
// cluster_id — goes RED with 40 distinct ids.
//
// KILLING MUTATION 1 (executed, RED, also caught here): flip the election to sorted[0]. Assertion (iii) —
// elected_ref == parent — goes RED (it becomes guest-00), and (ii)'s "the one elected is the parent" fails too.
//
// VACUITY GUARD: 40 members > MaxMembers(32), and the parent is truncated OUT of the audit list, so a
// never-truncated 5-member cluster cannot stand in for this proof.
func TestCascadeCollapsesToOneClusterAndOneElectedSubject(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	const parentHost = "dc1pve03"
	prefix := fmt.Sprintf("cascade-it-%d-", os.Getpid())
	parentRef := prefix + "zz-pve03-node-down" // sorts AFTER every guest ref, and past the MaxMembers=32 cut

	// A real estate graph: 39 guests run on the one hypervisor, so InDegree(parent)=39 and every guest's
	// RunsOnParent is the parent — the topology the causal election reads.
	g := estate.NewGraph()
	for i := 0; i < 39; i++ {
		g.Upsert(estate.Edge{
			From: estate.Entity{Type: estate.TypeLXC, Name: fmt.Sprintf("dc1vm%02d", i)},
			To:   estate.Entity{Type: estate.TypePVENode, Name: parentHost},
			Rel:  estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE,
		})
	}
	topo := clusterGraphTopo{g}

	// The 40-member window (parent first, guests trickling in over the first fifth of the span).
	span := 10 * time.Minute
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	obs := make([]correlate.Observation, 0, 40)
	obs = append(obs, correlate.Observation{ExternalRef: parentRef, Host: parentHost, SourceType: "librenms", AlertRule: "pve-node-down", At: base})
	for i := 0; i < 39; i++ {
		obs = append(obs, correlate.Observation{
			ExternalRef: fmt.Sprintf("%sguest-%02d-down", prefix, i),
			Host:        fmt.Sprintf("dc1vm%02d", i),
			SourceType:  "librenms", AlertRule: "guest-down",
			At: base.Add(time.Duration(i+1) * span / 200),
		})
	}
	window := correlate.Window{Span: span, Observations: obs}

	cleanup := func() {
		_, _ = p.Exec(ctx, "DELETE FROM exec_class_decision WHERE external_ref LIKE $1", prefix+"%")
		_, _ = p.Exec(ctx, "DELETE FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%")
	}
	cleanup()
	defer cleanup()

	clusterStore := NewAlertClusterStore(p)
	execStore := NewExecClassStore(p)

	// Drive the correlation stage for EVERY member in arrival order — the exact sequence CorrelateActivity
	// runs (Assess -> ClusterAnchor -> Join -> Elect -> Record), each unit the shipped one.
	for _, subj := range obs {
		v := correlate.Assess(subj, window)
		if !v.Correlated {
			t.Fatalf("member %q assessed NOT correlated over a 40-host storm — the collapse never engages", subj.ExternalRef)
		}
		anchorRef, anchorAt := correlate.ClusterAnchor(subj, window)
		cid, err := clusterStore.Join(ctx, correlate.ClusterBucket(anchorAt), anchorRef, anchorAt, span)
		if err != nil {
			t.Fatalf("cluster join for %q: %v", subj.ExternalRef, err)
		}
		if cid == 0 {
			t.Fatalf("cluster join for %q returned id 0", subj.ExternalRef)
		}
		el := correlate.Elect(subj, window, topo)
		if err := execStore.Record(ctx, correlate.Decision{
			ExternalRef: subj.ExternalRef,
			ExecClass:   execclass.DeepInvestigation,
			Inputs:      execclass.Input{Correlated: true},
			Verdict:     v,
			ClusterID:   cid,
			Election:    el,
			DecidedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("record decision for %q: %v", subj.ExternalRef, err)
		}
	}

	// VACUITY FLOOR — 40 rows landed, or every assertion below is over a short population.
	var total int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM exec_class_decision WHERE external_ref LIKE $1", prefix+"%").Scan(&total); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if total != 40 {
		t.Fatalf("recorded %d decision rows, want 40 (the >MaxMembers=%d vacuity guard)", total, correlate.MaxMembers)
	}

	// (i) ONE durable cluster identity across all 40 members — the collapse. This is the assertion Killing
	// Mutation 2 reddens: a per-subject anchor mints 40 distinct ids here.
	var distinctClusters, oneCluster int64
	if err := p.QueryRow(ctx, `SELECT count(DISTINCT cluster_id), min(cluster_id) FROM exec_class_decision WHERE external_ref LIKE $1`, prefix+"%").
		Scan(&distinctClusters, &oneCluster); err != nil {
		t.Fatalf("distinct cluster read: %v", err)
	}
	if distinctClusters != 1 {
		t.Fatalf("the 40 members carry %d distinct cluster ids, want 1 — the storm did NOT collapse to one "+
			"durable identity (each subject recomputed its own cluster, the TG-385 defect)", distinctClusters)
	}
	if oneCluster == 0 {
		t.Fatal("the shared cluster_id is 0 — a correlated cascade recorded no durable identity")
	}

	// (ii) exactly ONE elected subject (elected_ref == its own external_ref) — the one investigation session
	// that opens; every other member collapses to evidence.
	var electedSubjects int
	var theElected string
	if err := p.QueryRow(ctx, `SELECT count(*) FROM exec_class_decision WHERE external_ref LIKE $1 AND elected_ref = external_ref`, prefix+"%").
		Scan(&electedSubjects); err != nil {
		t.Fatalf("elected-subject count: %v", err)
	}
	if electedSubjects != 1 {
		t.Fatalf("%d members are their own elected subject, want exactly 1 — the collapse would open %d sessions, not one", electedSubjects, electedSubjects)
	}

	// (iii) the elected subject is the PARENT, by estate in-degree — not the lexicographically-first ref.
	// Every row must name the parent as elected_ref (they all agreed), and it must be the parent.
	var namedParent int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM exec_class_decision WHERE external_ref LIKE $1 AND elected_ref = $2`, prefix+"%", parentRef).
		Scan(&namedParent); err != nil {
		t.Fatalf("elected==parent count: %v", err)
	}
	if namedParent != 40 {
		t.Fatalf("%d of 40 rows elected the parent %q; want all 40 — the members disagree on the causal subject", namedParent, parentRef)
	}
	if err := p.QueryRow(ctx, `SELECT external_ref FROM exec_class_decision WHERE external_ref LIKE $1 AND elected_ref = external_ref`, prefix+"%").Scan(&theElected); err != nil {
		t.Fatalf("read the elected subject: %v", err)
	}
	if theElected != parentRef {
		t.Fatalf("the one session opened for %q, want the causal parent %q — the collapse elected a symptom, not the cause", theElected, parentRef)
	}

	// The elect_rule + runner_up are recorded (a wrong election must be reviewable, not silent).
	var rule, runnerUp string
	if err := p.QueryRow(ctx, `SELECT elect_rule, runner_up_ref FROM exec_class_decision WHERE external_ref = $1`, parentRef).Scan(&rule, &runnerUp); err != nil {
		t.Fatalf("read elect_rule: %v", err)
	}
	if rule != correlate.ElectRuleIndegree {
		t.Fatalf("elect_rule recorded %q, want %q — the parent won on estate in-degree", rule, correlate.ElectRuleIndegree)
	}
	if runnerUp == "" || runnerUp == parentRef {
		t.Fatalf("runner_up_ref = %q — the runner-up must be a real, different member for the election to be reviewable", runnerUp)
	}
}

// The 0085 migration creates the durable cluster identity and the routing-decision columns the collapse
// records on, and the down migration removes exactly what the up added (pairing intact). Static — reads the
// file, so it runs without a database and guards the schema even where the DSN-gated tests skip.
func TestAlertClusterMigrationShapeAndPairing(t *testing.T) {
	up := readMigration(t, "0085_alert_cluster.up.sql")
	for _, want := range []string{
		"CREATE TABLE alert_cluster",
		"first_seen_ref",                             // the storm anchor identity
		"CREATE UNIQUE INDEX alert_cluster_key_uidx", // the durable (window_bucket, first_seen_ref) key
		"(window_bucket, first_seen_ref)",            // ...on exactly those two columns
		"REVOKE DELETE ON alert_cluster",             // a cluster identity is not request-path deletable
		"ADD COLUMN IF NOT EXISTS cluster_id",        // the decision row joins the cluster
		"ADD COLUMN IF NOT EXISTS elected_ref",       // ...and records who investigates
		"ADD COLUMN IF NOT EXISTS runner_up_ref",     // ...and the runner-up
		"ADD COLUMN IF NOT EXISTS elect_rule",        // ...and why (which tie-break)
	} {
		if !containsAll(up, want) {
			t.Errorf("0085 up migration missing %q", want)
		}
	}
	down := readMigration(t, "0085_alert_cluster.down.sql")
	for _, want := range []string{
		"DROP TABLE IF EXISTS alert_cluster",
		"DROP COLUMN IF EXISTS cluster_id",
		"DROP COLUMN IF EXISTS elected_ref",
	} {
		if !containsAll(down, want) {
			t.Errorf("0085 down migration does not reverse %q", want)
		}
	}
}

// The alert_cluster upsert is idempotent under the SAME storm key and concurrency-safe: two members that
// compute the same anchor get the SAME id, and a retry of one member's join never mints a second row. This
// is the property that makes the collapse robust to Temporal's at-least-once activity delivery.
func TestAlertClusterJoinIsIdempotentOnTheStormKey(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	ref := fmt.Sprintf("cluster-idem-%d-anchor", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM alert_cluster WHERE first_seen_ref = $1", ref) }()

	store := NewAlertClusterStore(p)
	at := time.Now().UTC().Truncate(time.Second)
	bucket := correlate.ClusterBucket(at)

	first, err := store.Join(ctx, bucket, ref, at, 10*time.Minute)
	if err != nil {
		t.Fatalf("first join: %v", err)
	}
	// A second member computing the same anchor, and a retry of the first, must both return the SAME id.
	for i := 0; i < 3; i++ {
		again, err := store.Join(ctx, bucket, ref, at, 10*time.Minute)
		if err != nil {
			t.Fatalf("re-join %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("re-join %d returned id %d, want the shared %d — the storm key is not collapsing to one row", i, again, first)
		}
	}
	var rows int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM alert_cluster WHERE first_seen_ref = $1", ref).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the storm key minted %d cluster rows, want 1 (idempotent upsert)", rows)
	}
	// An empty anchor is refused by name (not left to surface as an opaque CHECK violation in a log).
	if _, err := store.Join(ctx, bucket, "", at, time.Minute); err == nil {
		t.Fatal("a cluster join with an empty first_seen_ref was accepted")
	}
}

// STRADDLE-TOLERANT JOIN (TG-465 part 1). A storm WIDER than the correlation span FRAGMENTS: a straggler
// whose symmetric +/-span window no longer reaches the true-earliest alert computes a LATER anchor
// (a different first_seen_ref), so its (window_bucket, first_seen_ref) key differs and — before this fix — it
// mints a SECOND alert_cluster row for one storm. One wide storm then reads as 2+ separate "collapsed
// cluster" investigations instead of one.
//
// This test builds exactly that shape at the DB seam: an earlier cluster C1 anchored at `base`, then a
// straggler whose anchor arrival falls 7 minutes later — still INSIDE C1's real window [base, base+span]
// (span=10m) — but on a DIFFERENT ref in an ADJACENT bucket. A straddle-tolerant Join must recognise the
// temporal containment and RETURN C1's id (one cluster). Membership only: it never touches the causal-gated
// collapse decision downstream.
//
// RED before the fix: the plain upsert keys on (window_bucket, first_seen_ref), so the straggler's distinct
// ref inserts a SECOND row with a SECOND id — the assertion `strag == c1` fails and the row count is 2.
// GREEN after: the fold-probe finds C1's window contains the straggler's arrival and returns C1's id (1 row).
func TestAlertClusterJoinFoldsStraddlingStraggler(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	prefix := fmt.Sprintf("straddle-fold-%d-", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%") }()
	_, _ = p.Exec(ctx, "DELETE FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%")

	store := NewAlertClusterStore(p)
	const span = 10 * time.Minute

	// C1: the storm's true-earliest anchor, on a bucket boundary so the arithmetic is hand-checkable.
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	earliestRef := prefix + "aa-true-earliest"
	c1, err := store.Join(ctx, correlate.ClusterBucket(base), earliestRef, base, span)
	if err != nil {
		t.Fatalf("join earliest: %v", err)
	}
	if c1 == 0 {
		t.Fatal("earliest anchor returned cluster id 0")
	}

	// The straggler: it arrived so late that C1's earliest alert fell OUT of its +/-span window, so it
	// anchored on a LATER ref (a different first_seen_ref) in the NEXT bucket. But its own arrival, base+7m,
	// still falls INSIDE C1's real window [base, base+10m] — the straddle. Its bucket is base's + 1 (7m/5m).
	stragAt := base.Add(7 * time.Minute)
	stragRef := prefix + "zz-late-straggler"
	if correlate.ClusterBucket(stragAt) == correlate.ClusterBucket(base) {
		t.Fatalf("test wiring: straggler must land in an ADJACENT bucket, not the same one as the anchor")
	}
	strag, err := store.Join(ctx, correlate.ClusterBucket(stragAt), stragRef, stragAt, span)
	if err != nil {
		t.Fatalf("join straggler: %v", err)
	}

	// (i) The straggler folded onto C1 — one durable identity for the one storm.
	if strag != c1 {
		t.Fatalf("the straggler minted cluster id %d, want C1's %d — a storm wider than the span FRAGMENTED "+
			"into two 'collapsed cluster' investigations (the TG-465 defect). Its arrival %s falls inside "+
			"C1's window [%s, %s] and it must JOIN C1, not create a second row", strag, c1,
			stragAt.Format(time.RFC3339), base.Format(time.RFC3339), base.Add(span).Format(time.RFC3339))
	}
	// (ii) Exactly one row exists for this storm — the fold created no second alert_cluster.
	var rows int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the wide storm minted %d cluster rows, want 1 — the straddler did not fold onto the earlier cluster", rows)
	}
}

// NO OVER-MERGE — the SAFETY guard (TG-465 part 1). The fold MUST require REAL temporal containment. Two
// GENUINELY-SEPARATE storms — windows more than a span apart — must stay TWO clusters, or the cascade-collapse
// that this identity feeds would group unrelated incidents and could SILENCE one of them. Storm B here arrives
// 11 minutes after storm A (span=10m), so B's arrival is OUTSIDE A's window [A, A+10m] — but the two anchors
// sit in buckets only 2 apart (within the fold-probe's span-bounded bucket scan), so a probe that checked
// merely "an adjacent bucket has a cluster" WOULD wrongly merge them.
//
// This test is GREEN before AND after the fix (before: no probe at all, so B inserts; after: the probe finds
// A in the bucket range but its real window does NOT contain B's arrival, so B still inserts).
//
// KILLING MUTATION for the guard: weaken the containment predicate in AlertClusterStore.foldTarget to a bare
// "an adjacent bucket has a cluster" — drop the `first_seen_at <= $ AND $ <= first_seen_at + span_seconds`
// clauses (keep only the window_bucket BETWEEN range). B then folds onto A, this test sees ONE cluster id, and
// it goes RED. That is the exact over-merge this predicate exists to prevent.
func TestAlertClusterJoinDoesNotMergeSeparateStorms(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	prefix := fmt.Sprintf("no-overmerge-%d-", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%") }()
	_, _ = p.Exec(ctx, "DELETE FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%")

	store := NewAlertClusterStore(p)
	const span = 10 * time.Minute

	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	// Storm A: window [base+1m, base+11m].
	aAt := base.Add(1 * time.Minute)
	aRef := prefix + "aa-storm-a"
	a, err := store.Join(ctx, correlate.ClusterBucket(aAt), aRef, aAt, span)
	if err != nil {
		t.Fatalf("join storm A: %v", err)
	}
	// Storm B: arrives at base+12m — 11m after A, so B's arrival is OUTSIDE A's window (ends at base+11m) and
	// the two are genuinely separate. Yet their buckets are only 2 apart (12m/5m vs 1m/5m), inside the probe's
	// span-bounded bucket scan, so ONLY the real temporal check keeps them apart.
	bAt := base.Add(12 * time.Minute)
	bRef := prefix + "bb-storm-b"
	if bAt.Before(aAt.Add(span)) || bAt.Equal(aAt.Add(span)) {
		t.Fatalf("test wiring: storm B (%s) must arrive AFTER storm A's window ends (%s)", bAt, aAt.Add(span))
	}
	gap := correlate.ClusterBucket(bAt) - correlate.ClusterBucket(aAt)
	if gap < 1 || gap > 2 {
		t.Fatalf("test wiring: the two anchors must sit 1-2 buckets apart to exercise the bucket-scan guard, got %d", gap)
	}
	b, err := store.Join(ctx, correlate.ClusterBucket(bAt), bRef, bAt, span)
	if err != nil {
		t.Fatalf("join storm B: %v", err)
	}

	// Two separate storms keep two durable identities — the fold never guessed a merge across the gap.
	if a == 0 || b == 0 {
		t.Fatalf("a storm recorded cluster id 0 (a=%d b=%d)", a, b)
	}
	if a == b {
		t.Fatalf("two storms %s and %s (arrivals %s apart, > span %s) collapsed onto ONE cluster id %d — an "+
			"OVER-MERGE that would let the cascade-collapse silence one of two unrelated incidents",
			aRef, bRef, bAt.Sub(aAt), span, a)
	}
	var rows int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM alert_cluster WHERE first_seen_ref LIKE $1", prefix+"%").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Fatalf("two separate storms produced %d cluster rows, want 2 — the containment check over-merged", rows)
	}
}
