package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// Runs against a REAL Postgres (TG_TEST_DSN) for the same reason axis_read_test.go does: every risk here is
// SQL semantics — whether the window bound actually excludes what it claims to exclude, and whether the
// append-only grant + ON CONFLICT really make a retry idempotent. A pgx fake would prove only that the Go
// beside the query compiles.

// correlationSeed writes N ingest_alert rows around `at` and returns a cleanup. Every ref is prefixed so
// the fixture can be removed from a shared golden database without touching another test's rows.
func correlationSeed(ctx context.Context, t *testing.T, p *Pool, prefix string, at time.Time) func() {
	t.Helper()
	log := NewAlertLogStore(p)
	add := func(ref, source, host string, sev string, off time.Duration) {
		log.Append(ctx, httpapi.AlertRecord{
			ExternalRef: prefix + ref, SourceType: source, AlertRule: "rule-" + ref, Severity: sev,
			Host: host, Site: "dc1", Summary: "seeded", ReceivedAt: at.Add(off),
		})
	}
	// The subject, plus a two-host cross-source cluster inside the window...
	add("subject", "librenms", "web01", "warning", 0)
	add("near-a", "crowdsec", "db01", "warning", 45*time.Second)
	// ...a REPEAT of the subject's own host (breadth, not volume: it must not add a host)...
	add("near-b", "librenms", "web01", "critical", 90*time.Second)
	// ...and one an hour on EACH side, which are different incidents whatever they say. BOTH bounds are
	// seeded deliberately: a one-sided fixture only exercises one half of the WHERE clause, and a mutation
	// widening the other half passes it — which is exactly what happened while writing this test.
	add("far-future", "librenms", "cache01", "critical", 61*time.Minute)
	add("far-past", "librenms", "cache02", "critical", -61*time.Minute)
	return func() {
		_, _ = p.Exec(ctx, "DELETE FROM ingest_alert WHERE external_ref LIKE $1", prefix+"%")
	}
}

// The correlation window is a real bound over real rows, and it feeds the pure rule.
//
// VACUITY FLOOR: this test SELECTs, and a SELECT that matches nothing is the classic false green — an
// empty result satisfies "nothing outside the window came back" perfectly. So it asserts the seeded rows
// are PRESENT by ref before it asserts anything about what is absent, and fails naming the seed if not.
//
// KILLING MUTATION (executed): widen the reader's lower bound to `at.Add(-span*100)`. RED — the
// hour-away alert joins the window, a quiet estate is retroactively declared a three-host cascade, and
// every incident TG triages routes to the deep path on evidence from an unrelated hour.
func TestCorrelationWindowBoundsTheEvidence(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	prefix := fmt.Sprintf("corr-it-%d-", os.Getpid())
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	defer correlationSeed(ctx, t, p, prefix, at)()

	obs, err := NewCorrelationStore(p).Window(ctx, at, 10*time.Minute)
	if err != nil {
		t.Fatalf("window: %v", err)
	}

	got := map[string]correlate.Observation{}
	for _, o := range obs {
		got[o.ExternalRef] = o
	}
	// VACUITY FLOOR — the seeded in-window rows must be here, or every assertion below is vacuous.
	for _, ref := range []string{"subject", "near-a", "near-b"} {
		if _, ok := got[prefix+ref]; !ok {
			t.Fatalf("the window returned none of the seeded row %q (got %d rows) — the read matched "+
				"nothing, so every exclusion assertion below would pass over an empty set", prefix+ref, len(obs))
		}
	}
	for _, ref := range []string{"far-future", "far-past"} {
		if _, ok := got[prefix+ref]; ok {
			t.Fatalf("%q — an alert an HOUR outside the window came back; that bound is not bounding", prefix+ref)
		}
	}
	// The projection must carry what the rule reads: source and host, or the cross-source rule is blind.
	if o := got[prefix+"near-a"]; o.SourceType != "crowdsec" || o.Host != "db01" {
		t.Fatalf("observation projection lost its identifiers: %+v", o)
	}

	// End to end through the REAL rule: two sources, two hosts ⇒ cross-source correlated; the same-host
	// repeat must not inflate the host count.
	subject := got[prefix+"subject"]
	v := correlate.Assess(subject, correlate.Window{Span: 10 * time.Minute, Observations: obs})
	if !v.Correlated || v.Reason != correlate.ReasonCrossSource {
		t.Fatalf("the seeded two-source/two-host cluster assessed %v/%q, want correlated/%q (hosts=%v sources=%v)",
			v.Correlated, v.Reason, correlate.ReasonCrossSource, v.Hosts, v.Sources)
	}
	for _, h := range v.Hosts {
		if h == "cache01" || h == "cache02" {
			t.Fatalf("the out-of-window host %q reached the verdict's evidence — with three hosts the "+
				"MULTI-HOST rule fires and a quiet estate is declared a cascade by an unrelated hour", h)
		}
	}
}

// The routing decision persists, survives a retry without duplicating, and keeps its INPUTS — the audit
// trail the topology decision never had (TG-169, migration 0058).
//
// KILLING MUTATION (executed): drop `inputs_json` from the INSERT column list. RED — the row records the
// class but not the premises, so a decision cannot be re-derived against a future classifier and "the rule
// changed" is indistinguishable from "the estate changed", which is the whole reason the column exists.
func TestExecClassDecisionPersistsItsInputsAndIsRetrySafe(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	ref := fmt.Sprintf("execclass-it-%d-ref", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM exec_class_decision WHERE external_ref = $1", ref) }()

	store := NewExecClassStore(p)
	d := correlate.Decision{
		ExternalRef: ref,
		ExecClass:   execclass.DeepInvestigation,
		Inputs:      execclass.Input{Correlated: true, CriticalityTier: "service"},
		Verdict: correlate.Verdict{
			Correlated: true, Reason: correlate.ReasonMultiHost, Span: 10 * time.Minute,
			Hosts: []string{"db01", "web01", "web02"}, Sources: []string{"librenms"},
			Members: []string{ref, "m2", "m3"}, MemberCount: 3,
		},
		DecidedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := store.Record(ctx, d); err != nil {
		t.Fatalf("record: %v", err)
	}
	// A retry of the activity must not append a second, unremovable row (DELETE is revoked).
	if err := store.Record(ctx, d); err != nil {
		t.Fatalf("retry record: %v", err)
	}

	var (
		n              int
		class, reason  string
		correlated     bool
		hosts, sources int
		members        int
		windowSec      int
		inputs         []byte
		evidence       []byte
	)
	if err := p.QueryRow(ctx, `
		SELECT count(*) OVER (), exec_class, reason, correlated, distinct_hosts, distinct_sources,
		       member_count, window_seconds, inputs_json, evidence_json
		FROM exec_class_decision WHERE external_ref = $1`, ref).
		Scan(&n, &class, &reason, &correlated, &hosts, &sources, &members, &windowSec, &inputs, &evidence); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 1 {
		t.Fatalf("a retry created %d rows on an append-only table (idempotency broken)", n)
	}
	if class != string(execclass.DeepInvestigation) || reason != correlate.ReasonMultiHost || !correlated {
		t.Fatalf("decision read back as class=%q reason=%q correlated=%v", class, reason, correlated)
	}
	if hosts != 3 || sources != 1 || members != 3 || windowSec != 600 {
		t.Fatalf("evidence counts wrong: hosts=%d sources=%d members=%d window=%ds", hosts, sources, members, windowSec)
	}
	// THE INPUTS ARE THE POINT: a decision must be RE-DERIVABLE, not merely readable. Asserted by
	// round-tripping the blob back through the classifier — if the premises survived, re-classifying them
	// must reproduce the class that was stored, which is exactly the operation a future review performs.
	var back execclass.Input
	if err := json.Unmarshal(inputs, &back); err != nil {
		t.Fatalf("inputs_json is not a classifier input (%s): %v", inputs, err)
	}
	if back != d.Inputs {
		t.Fatalf("inputs_json round-tripped to %+v, want %+v — the classifier premises did not survive, so "+
			"this decision cannot be replayed against a future classifier", back, d.Inputs)
	}
	if got := execclass.Classify(back); string(got) != class {
		t.Fatalf("re-classifying the persisted inputs yields %q but the row says %q — the recorded premises "+
			"do not explain the recorded conclusion", got, class)
	}
	if !containsAll(string(evidence), `"web02"`, `"librenms"`) {
		t.Fatalf("evidence_json = %s — the hosts/sources behind the routing claim were not persisted", evidence)
	}

	// A decision with no class is not a decision: the writer refuses it by name rather than letting the
	// CHECK constraint surface as an opaque Postgres error in a log nobody reads.
	if err := store.Record(ctx, correlate.Decision{ExternalRef: ref + "-bad"}); err == nil {
		t.Fatal("a decision with an empty exec_class was accepted")
	}
}

// The routing-decision table is APPEND-ONLY evidence (REQ-2016): the runtime role may INSERT and SELECT
// and holds no UPDATE/DELETE, exactly like ingest_alert (0033) and the accountability spine (0015).
//
// KILLING MUTATION (executed): delete the REVOKE line from 0058. RED — a routing decision becomes
// rewritable after the fact, which is the one property that makes it worth reviewing.
func TestExecClassDecisionIsAppendOnlyForTheRuntimeRole(t *testing.T) {
	sql := readMigration(t, "0058_exec_class_decision.up.sql")
	for _, want := range []string{
		"CREATE TABLE exec_class_decision",
		"inputs_json",   // the premises, not just the conclusion
		"evidence_json", // the hosts/sources behind the correlation claim
		"degraded",      // 'could not look' is not 'looked and saw nothing'
		"CREATE UNIQUE INDEX exec_class_decision_ref_uidx",
		"REVOKE UPDATE, DELETE ON exec_class_decision FROM tg_runtime",
	} {
		if !containsAll(sql, want) {
			t.Errorf("0058 migration missing %q", want)
		}
	}
	if down := readMigration(t, "0058_exec_class_decision.down.sql"); !containsAll(down, "DROP TABLE IF EXISTS exec_class_decision") {
		t.Error("0058 down migration does not drop what the up creates")
	}
}

// containsAll is the "every one of these substrings is present" predicate the assertions above read as one
// sentence. It is NOT vacuous on an empty list by accident: every call site passes a literal, non-empty set.
func containsAll(s string, subs ...string) bool {
	if len(subs) == 0 {
		return false // an empty demand must never read as satisfied
	}
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
