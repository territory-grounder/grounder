package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// THE EVIDENCE CORPUS HAD NO END (TG-295).
//
// Migration 0053 gave untrusted host output a durable write primitive (`payload text NOT NULL DEFAULT ''`)
// and, with `REVOKE UPDATE, DELETE ... FROM tg_runtime`, no erasure path for ANYONE. Append-only was the
// right call for auditability; permanent was not one anybody made. Verbatim tool output is the purgeable
// operational body (docs/DATA-MODEL.md §5.2, INV-14), not the derived audit spine (§5.1) — it is whatever
// the host printed, screened but not sealed, and before this migration every byte of it was scheduled to
// outlive the deployment.
//
// These oracles are gated on TG_TEST_POSTGRES_DSN (an EMPTY database; they call Migrate() themselves) and
// prove BOTH halves, because only one of them is about the feature working:
//   - the reaper deletes what is past retention;
//   - the reaper deletes NOTHING else, and no other path can delete anything at all.

// evidenceReapFixture migrates the fixture database, opens a pool, and hands back a unique ref prefix.
func evidenceReapFixture(t *testing.T) (context.Context, *Pool, string) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the evidence-retention oracles")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return ctx, p, fmt.Sprintf("evreap-%d-%s", os.Getpid(), t.Name())
}

// seedEvidence inserts one row with an explicit created_at (the column the reaper judges by).
func seedEvidence(t *testing.T, ctx context.Context, p *Pool, ref string, cycle int, id string, createdAt time.Time) {
	t.Helper()
	if _, err := p.Exec(ctx, `
		INSERT INTO agent_step_evidence (external_ref, cycle, evidence_id, tool, payload, created_at)
		VALUES ($1, $2, $3, 'check-host-services', 'seeded evidence payload', $4)`,
		ref, cycle, id, createdAt); err != nil {
		t.Fatalf("seed %s/%s: %v", ref, id, err)
	}
}

// cleanupEvidence removes everything this test wrote, including its journal rows, so the next oracle's
// exact-count assertions are about its own rows. Runs as the superuser fixture role, which is the only
// identity that can — deliberately not a path the application has.
func cleanupEvidence(t *testing.T, ctx context.Context, p *Pool, refPrefix string, since time.Time) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := p.Exec(ctx, `DELETE FROM agent_step_evidence WHERE external_ref LIKE $1`, refPrefix+"%"); err != nil {
			t.Errorf("cleanup evidence: %v", err)
		}
		if _, err := p.Exec(ctx, `DELETE FROM agent_step_evidence_reap WHERE reaped_at >= $1`, since); err != nil {
			t.Errorf("cleanup journal: %v", err)
		}
	})
}

// requireNoStrayRowsOlderThan makes the exact counts below honest. The reaper judges by created_at over the
// WHOLE table, so a row left behind by another oracle would be counted in the returned number and turn a
// precise assertion into a coin flip. Failing here says "the fixture is dirty", which is a different and
// much more useful message than "the reaper deleted 3, want 2".
func requireNoStrayRowsOlderThan(t *testing.T, ctx context.Context, p *Pool, cutoff time.Time, refPrefix string) {
	t.Helper()
	var strays int
	if err := p.QueryRow(ctx, `
		SELECT count(*) FROM agent_step_evidence WHERE created_at < $1 AND external_ref NOT LIKE $2`,
		cutoff, refPrefix+"%").Scan(&strays); err != nil {
		t.Fatalf("count strays: %v", err)
	}
	if strays > 0 {
		t.Fatalf("%d agent_step_evidence row(s) older than the cutoff were not seeded by this test — the "+
			"fixture is dirty and the exact counts below would be measuring someone else's rows", strays)
	}
}

// KILLING MUTATION (executed): in migration 0055, change the reaper's predicate from
// `WHERE created_at < cutoff` to `WHERE true`. RED here —
//
//	"evidence <ref>-inside-1 (created 300d ago, retention 365d) was DELETED: a reaper that over-collects
//	 destroys the ground truth behind steps the console is still listing"
//
// The over-collection half is the one that matters. A reaper that deletes everything also satisfies "old
// rows are gone", and its failure mode is silent: the console keeps listing the step, the "ground truth"
// citation opens on "not recorded", and the operator concludes the tool returned nothing.
func TestEvidenceReaperDeletesOnlyRowsPastRetention(t *testing.T) {
	ctx, p, ref := evidenceReapFixture(t)
	start := time.Now().UTC()
	cleanupEvidence(t, ctx, p, ref, start)

	const retention = 365 * 24 * time.Hour
	now := start
	cutoff := now.Add(-retention)
	requireNoStrayRowsOlderThan(t, ctx, p, cutoff, ref)

	expired := map[string]time.Time{
		ref + "-ancient": now.Add(-800 * 24 * time.Hour), // two years of a session nobody will ever re-read
		ref + "-expired": now.Add(-366 * 24 * time.Hour), // one day past the bound
	}
	kept := map[string]time.Time{
		ref + "-inside-1": now.Add(-300 * 24 * time.Hour), // inside retention: still citable from the console
		ref + "-inside-2": now.Add(-364 * 24 * time.Hour), // one day INSIDE the bound — the off-by-one case
		ref + "-fresh":    now.Add(-2 * time.Hour),        // this morning's incident
	}
	cycle := 1
	for id, at := range expired {
		seedEvidence(t, ctx, p, ref, cycle, id, at)
		cycle++
	}
	for id, at := range kept {
		seedEvidence(t, ctx, p, ref, cycle, id, at)
		cycle++
	}

	store := NewAgentStepEvidenceStore(p)
	n, err := store.ReapEvidenceOlderThan(ctx, cutoff, DefaultEvidenceReapBatch)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != int64(len(expired)) {
		t.Errorf("reaped %d row(s), want %d", n, len(expired))
	}

	for id, at := range expired {
		if evidenceRowExists(t, ctx, p, ref, id) {
			t.Errorf("evidence %s (created %s ago, retention %s) SURVIVED the sweep — the corpus of unsealed "+
				"host output is still unbounded, which is the defect TG-295 exists to close",
				id, now.Sub(at).Round(time.Hour), retention)
		}
	}
	// THE CONTROL, and it is the one that matters: deleting everything would also pass the loop above.
	for id, at := range kept {
		if !evidenceRowExists(t, ctx, p, ref, id) {
			t.Errorf("evidence %s (created %s ago, retention %s) was DELETED: a reaper that over-collects "+
				"destroys the ground truth behind steps the console is still listing, and the citation then "+
				"renders \"not recorded\" — indistinguishable to an operator from a tool that returned nothing",
				id, now.Sub(at).Round(time.Hour), retention)
		}
	}

	// The purge must be reconstructable from the journal alone.
	var (
		rows           int64
		gotCutoff      time.Time
		oldest, newest time.Time
		invokedBy      string
	)
	if err := p.QueryRow(ctx, `
		SELECT rows_deleted, cutoff, oldest_deleted, newest_deleted, invoked_by
		FROM agent_step_evidence_reap WHERE reaped_at >= $1 ORDER BY id DESC LIMIT 1`, start).
		Scan(&rows, &gotCutoff, &oldest, &newest, &invokedBy); err != nil {
		t.Fatalf("read the reap journal: %v — a deletion that leaves no record is exactly what the "+
			"SECURITY DEFINER path exists to make impossible", err)
	}
	if rows != n {
		t.Errorf("journal says %d row(s) deleted, the reaper returned %d — the audit record and the deletion "+
			"disagree, so the journal cannot be used to reconstruct what left", rows, n)
	}
	if !gotCutoff.UTC().Round(time.Second).Equal(cutoff.Round(time.Second)) {
		t.Errorf("journal cutoff %s, want %s", gotCutoff.UTC(), cutoff)
	}
	if oldest.After(newest) {
		t.Errorf("journal span is inverted: oldest %s newer than newest %s", oldest, newest)
	}
	if invokedBy == "" {
		t.Error("journal recorded no invoker — the audit answers 'what left' but not 'who asked', and " +
			"the whole point of one privileged path is being able to name who walked it")
	}
}

// KILLING MUTATION (executed): drop the `LIMIT max_rows` from the reaper's doomed CTE in migration 0055.
// RED here — "reaped 5 row(s) with a batch cap of 2". An unbounded DELETE on this table is the operational
// hazard the cap exists for: the first sweep after an operator shortens retention holds locks over the
// corpus the agent is concurrently writing to, and it does it for as long as the delete takes.
func TestEvidenceReaperHonoursItsBatchCap(t *testing.T) {
	ctx, p, ref := evidenceReapFixture(t)
	start := time.Now().UTC()
	cleanupEvidence(t, ctx, p, ref, start)

	cutoff := start.Add(-365 * 24 * time.Hour)
	requireNoStrayRowsOlderThan(t, ctx, p, cutoff, ref)
	for i := 0; i < 5; i++ {
		seedEvidence(t, ctx, p, ref, i+1, fmt.Sprintf("%s-batch-%d", ref, i), start.Add(-500*24*time.Hour))
	}

	store := NewAgentStepEvidenceStore(p)
	first, err := store.ReapEvidenceOlderThan(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("reap 1: %v", err)
	}
	if first != 2 {
		t.Fatalf("reaped %d row(s) with a batch cap of 2 — an uncapped DELETE over the evidence corpus holds "+
			"locks on the table the agent is still writing to for the whole of a large purge", first)
	}
	second, err := store.ReapEvidenceOlderThan(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("reap 2: %v", err)
	}
	third, err := store.ReapEvidenceOlderThan(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("reap 3: %v", err)
	}
	if first+second+third != 5 {
		t.Errorf("three capped sweeps removed %d of 5 expired rows — a cap that does not DRAIN is a bound "+
			"that never finishes, so the corpus stays unbounded on a slower timer", first+second+third)
	}
	// A fourth sweep must be a no-op AND must not journal: a journal row is a claim that evidence left.
	fourth, err := store.ReapEvidenceOlderThan(ctx, cutoff, 2)
	if err != nil {
		t.Fatalf("reap 4: %v", err)
	}
	if fourth != 0 {
		t.Errorf("a sweep with nothing to do reaped %d — the reaper is not idempotent", fourth)
	}
	var journalled int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM agent_step_evidence_reap WHERE reaped_at >= $1`, start).
		Scan(&journalled); err != nil {
		t.Fatalf("count journal: %v", err)
	}
	if journalled != 3 {
		t.Errorf("the journal holds %d row(s) for 3 sweeps that deleted and 1 that did not — a journal that "+
			"gains a row per TICK is a timer, and it would need a retention bound of its own", journalled)
	}
}

// KILLING MUTATION (executed): delete the `IF cutoff > now() - interval '24 hours' THEN RAISE` block from
// migration 0055. RED here — "reap_agent_step_evidence accepted a cutoff of now(): the runtime role can
// erase the last 24 hours of evidence, which is the window that would contain whatever it just did".
//
// The floor is what keeps the ONE privileged path from being a general erase. A retention reaper never
// needs to reach into the last day; a compromised runtime does, and this is the only thing standing in
// front of the evidence of its own steps.
func TestEvidenceReaperRefusesACutoffInsideTheFloor(t *testing.T) {
	ctx, p, ref := evidenceReapFixture(t)
	start := time.Now().UTC()
	cleanupEvidence(t, ctx, p, ref, start)

	recent := ref + "-this-hour"
	seedEvidence(t, ctx, p, ref, 1, recent, start.Add(-1*time.Hour))

	store := NewAgentStepEvidenceStore(p)
	for _, cutoff := range []struct {
		when time.Time
		why  string
	}{
		{start, "now() — erase everything recorded so far"},
		{start.Add(-23 * time.Hour), "23h ago — one hour inside the floor"},
	} {
		n, err := store.ReapEvidenceOlderThan(ctx, cutoff.when, DefaultEvidenceReapBatch)
		if err == nil {
			t.Errorf("reap_agent_step_evidence accepted a cutoff of %s and deleted %d row(s): the runtime role "+
				"can erase the most recent evidence, which is the window that would contain whatever it just "+
				"did — the one privileged path is then a general erase with extra steps", cutoff.why, n)
			continue
		}
		if n != 0 {
			t.Errorf("a refused sweep still reported %d deletion(s)", n)
		}
	}
	if !evidenceRowExists(t, ctx, p, ref, recent) {
		t.Error("the row inside the floor was deleted despite the refusal — the exception did not roll the " +
			"delete back, so the floor reports a refusal it did not enforce")
	}
	// Vacuity floor: the two refusals above prove nothing if this store cannot reap at all.
	if _, err := store.ReapEvidenceOlderThan(ctx, start.Add(-365*24*time.Hour), DefaultEvidenceReapBatch); err != nil {
		t.Fatalf("a LEGAL cutoff was also refused (%v) — the two refusals above would then be a broken "+
			"reaper rather than an enforced floor", err)
	}
}

// KILLING MUTATION (executed): replace the SECURITY DEFINER function's grant in migration 0055 with
// `GRANT DELETE ON agent_step_evidence TO tg_runtime` (the "distinct role with DELETE" design). RED here —
// "tg_runtime DELETED an evidence row directly: the audit journal is then a convention the caller can skip,
// and the grant covers every row including the one an attacker wants gone".
//
// This is the oracle for the design decision in 0055's header, and it is stated as four properties because
// the point of choosing SECURITY DEFINER over a DELETE grant is exactly that all four hold at once.
func TestOnlyTheSecurityDefinerPathCanDeleteEvidence(t *testing.T) {
	ctx, p, ref := evidenceReapFixture(t)
	start := time.Now().UTC()
	cleanupEvidence(t, ctx, p, ref, start)

	cutoff := start.Add(-365 * 24 * time.Hour)
	requireNoStrayRowsOlderThan(t, ctx, p, cutoff, ref)
	target := ref + "-expired"
	seedEvidence(t, ctx, p, ref, 1, target, start.Add(-500*24*time.Hour))

	// SET ROLE needs a session, so take one connection and keep it. Through the pool these statements could
	// land on different connections and the role would not be the one under test.
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE tg_runtime"); err != nil {
		t.Fatalf("SET ROLE tg_runtime: %v — this oracle is about what the RUNTIME role can do; without the "+
			"role it would be asserting the superuser's privileges and would prove nothing", err)
	}
	defer conn.Exec(ctx, "RESET ROLE")

	// VACUITY FLOOR, and it is load-bearing here: in a database where tg_runtime holds no privileges at all
	// (any database whose tables were not created by tg_migration, which includes this fixture before
	// migration 0055 granted them explicitly), every "permission denied" below would pass for the wrong
	// reason. Prove the append path WORKS first, so the refusals that follow are privilege decisions.
	if _, err := conn.Exec(ctx, `
		INSERT INTO agent_step_evidence (external_ref, cycle, evidence_id, tool, payload)
		VALUES ($1, 99, $2, 'check-host-services', 'appended as tg_runtime')`, ref, ref+"-appended"); err != nil {
		t.Fatalf("tg_runtime could not APPEND evidence (%v) — the append path is the whole reason this table "+
			"exists, and every refusal asserted below would otherwise be vacuous", err)
	}
	var readable int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM agent_step_evidence WHERE external_ref = $1`, ref).
		Scan(&readable); err != nil || readable == 0 {
		t.Fatalf("tg_runtime could not READ back what it appended (count=%d err=%v) — the console's citation "+
			"reads through this role", readable, err)
	}

	// 1. No direct DELETE. This is 0053's guarantee and it must survive the reaper existing.
	_, err = conn.Exec(ctx, `DELETE FROM agent_step_evidence WHERE external_ref = $1`, ref)
	assertPermissionDenied(t, err, "tg_runtime DELETED evidence rows directly. A DELETE grant is a privilege "+
		"over every row — including the one that records what an attacker did — and it makes the audit "+
		"journal a convention the caller can skip rather than a transaction it cannot escape")

	// 2. No UPDATE either: 0053's append-only property is unchanged by this migration.
	_, err = conn.Exec(ctx, `UPDATE agent_step_evidence SET payload = 'rewritten' WHERE external_ref = $1`, ref)
	assertPermissionDenied(t, err, "tg_runtime REWROTE a payload — evidence that can be revised after an "+
		"operator has read it is not evidence")

	// 3. The one privileged path DOES work for this role, and deletes the expired row.
	var deleted int64
	if err := conn.QueryRow(ctx, `SELECT reap_agent_step_evidence($1, $2)`, cutoff, DefaultEvidenceReapBatch).
		Scan(&deleted); err != nil {
		t.Fatalf("tg_runtime could not call reap_agent_step_evidence: %v — the retention bound is then wired "+
			"to a path the worker cannot walk, which is a reaper that exists and never runs", err)
	}
	if deleted != 1 {
		t.Errorf("the privileged path deleted %d row(s), want 1", deleted)
	}
	if evidenceRowExists(t, ctx, p, ref, target) {
		t.Error("the expired row survived a successful call — the function reported work it did not do")
	}

	// 4. The journal is not writable by the caller: a purge cannot be forged, padded, or erased.
	var journalled int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM agent_step_evidence_reap WHERE reaped_at >= $1`, start).
		Scan(&journalled); err != nil {
		t.Fatalf("tg_runtime cannot read the reap journal: %v — an audit record nobody can read is not an "+
			"audit record", err)
	}
	if journalled != 1 {
		t.Fatalf("the journal holds %d row(s) after exactly one purge", journalled)
	}
	_, err = conn.Exec(ctx, `
		INSERT INTO agent_step_evidence_reap (cutoff, rows_deleted, oldest_deleted, newest_deleted, invoked_by)
		VALUES ($1, 999, $1, $1, 'not-me')`, cutoff)
	assertPermissionDenied(t, err, "tg_runtime WROTE its own journal row — a purge record the deleting role "+
		"can author is a record it can also fabricate, and the journal stops being evidence about deletion")
	_, err = conn.Exec(ctx, `DELETE FROM agent_step_evidence_reap`)
	assertPermissionDenied(t, err, "tg_runtime ERASED the purge journal — a deletion that can be un-recorded "+
		"is an unaudited deletion with a delay")

	// The journal names the CALLER, not the function's owner. current_user inside a SECURITY DEFINER body is
	// the owner, so recording that would put the same name on every line and answer nobody's question.
	var invokedBy string
	if err := conn.QueryRow(ctx, `
		SELECT invoked_by FROM agent_step_evidence_reap WHERE reaped_at >= $1 ORDER BY id DESC LIMIT 1`, start).
		Scan(&invokedBy); err != nil {
		t.Fatalf("read invoked_by: %v", err)
	}
	if invokedBy != "tg_runtime" {
		t.Errorf("the journal attributes the purge to %q, but tg_runtime made the call — under SECURITY "+
			"DEFINER current_user is the function's OWNER, so recording that would name the same identity on "+
			"every line and the journal could not answer who asked for a deletion", invokedBy)
	}
}

// KILLING MUTATION (executed): make ClampEvidenceRetention the identity function. RED —
// "TG_EVIDENCE_RETENTION=1h would be passed to the reaper verbatim". The database refuses any cutoff inside
// 24h, so an unclamped misconfiguration does not shorten retention: it makes EVERY sweep raise, forever,
// which is a deployment with no retention bound at all and a log line about a knob instead.
//
// Not DSN-gated on purpose: this is the arithmetic between an operator's env var and a control the database
// enforces, and it must run in `make all` on a box with no Postgres.
func TestRetentionShorterThanTheFloorIsClampedRatherThanObeyed(t *testing.T) {
	for _, tc := range []struct {
		in, want time.Duration
		why      string
	}{
		{time.Hour, EvidenceRetentionFloor, "an hour is inside the floor the SQL refuses"},
		{EvidenceRetentionFloor - time.Second, EvidenceRetentionFloor, "one second inside the floor is still inside it"},
		{EvidenceRetentionFloor, EvidenceRetentionFloor, "exactly the floor is legal and must pass through"},
		{DefaultEvidenceRetention, DefaultEvidenceRetention, "a generous retention must NOT be shortened"},
		{0, EvidenceRetentionFloor, "an unset/zero value must not become an immediate purge of everything"},
		{-time.Hour, EvidenceRetentionFloor, "a negative retention is a cutoff in the FUTURE — the whole table"},
	} {
		if got := ClampEvidenceRetention(tc.in); got != tc.want {
			t.Errorf("ClampEvidenceRetention(%s) = %s, want %s — %s", tc.in, got, tc.want, tc.why)
		}
	}
}

// evidenceRowExists answers whether one seeded row is still in the table.
func evidenceRowExists(t *testing.T, ctx context.Context, p *Pool, ref, evidenceID string) bool {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, `
		SELECT count(*) FROM agent_step_evidence WHERE external_ref = $1 AND evidence_id = $2`,
		ref, evidenceID).Scan(&n); err != nil {
		t.Fatalf("exists %s/%s: %v", ref, evidenceID, err)
	}
	return n > 0
}

// assertPermissionDenied fails unless err is Postgres' insufficient_privilege (42501). The SQLSTATE and not
// the message text: a message match would also accept an error that merely mentions permissions, and would
// break on a translated server.
func assertPermissionDenied(t *testing.T, err error, consequence string) {
	t.Helper()
	if err == nil {
		t.Errorf("the statement SUCCEEDED. %s", consequence)
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("the statement failed with %v, which is not insufficient_privilege (42501) — it was refused "+
			"for some other reason, so this proves nothing about the grant. %s", err, consequence)
	}
}
