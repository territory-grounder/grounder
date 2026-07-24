package db

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestPendingVerificationMigration structurally guards the 0022 pending_verification DDL (spec/017 T-017-8,
// REQ-1711/1712). Unlike the append-only 0020 regime audit tables, this record is MUTABLE — a launch is
// reserved, bound, then transitioned exactly once — so the runtime role KEEPS UPDATE (the status transition
// needs it) but LOSES DELETE (a claimed action_id is retained forever for idempotency + audit). The idempotency
// guard is the PRIMARY KEY on action_id (a second Reserve fails atomically at the DB). Single-org (no
// tenant_id); NON-SECRET (the AWX token is a SecretRef held elsewhere — no argv/host/credential column here);
// schema_version reader-guard. The down migration drops the table.
func TestPendingVerificationMigration(t *testing.T) {
	up := readMigration(t, "0022_regime_pending_verification.up.sql")
	if !strings.Contains(up, "CREATE TABLE pending_verification") {
		t.Fatal("0022 must CREATE TABLE pending_verification")
	}
	upFlat := strings.Join(strings.Fields(up), " ")
	for _, want := range []string{
		// action_id is the PRIMARY KEY — the atomic idempotency guard (REQ-1712): one launch per action_id.
		"action_id text PRIMARY KEY CHECK (length(btrim(action_id)) > 0)",
		"state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'verified', 'unverified'))",
		"terminal_status text NOT NULL DEFAULT '' CHECK (terminal_status = '' OR terminal_status IN ('successful', 'failed', 'error', 'canceled'))",
		"verdict text NOT NULL DEFAULT '' CHECK (verdict = '' OR verdict IN ('match', 'partial', 'deviation'))",
		"schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0)",
	} {
		if !strings.Contains(upFlat, want) {
			t.Errorf("0022 up migration missing %q", want)
		}
	}
	// Mutable current-state, NOT append-only: the runtime role LOSES DELETE (keep for idempotency/audit) but must
	// KEEP UPDATE (the pending → terminal status transition upserts it). So DELETE is revoked, UPDATE is NOT.
	if !strings.Contains(upFlat, "REVOKE DELETE ON pending_verification FROM tg_runtime") {
		t.Error("0022 must REVOKE DELETE on pending_verification from tg_runtime (idempotency: a claimed action_id is never removed)")
	}
	if strings.Contains(upFlat, "REVOKE UPDATE") {
		t.Error("0022 must NOT revoke UPDATE on pending_verification (the deferred-verify status transition needs it)")
	}
	// Single-org (ADR-0010): no tenant_id column may be reintroduced.
	ddlOnly := strings.ToLower(stripSQLComments(up))
	if strings.Contains(ddlOnly, "tenant") {
		t.Error("0022 must NOT introduce a tenant_id (single-org, ADR-0010)")
	}
	// NON-SECRET: no argv/host/credential/password/private_key column — the AWX token is a SecretRef held
	// elsewhere; only the non-secret job_id + the committed prediction land here. Scan the DDL, not the comments.
	for _, forbidden := range []string{"argv", "host", "credential", "secret", "password", "private_key"} {
		if strings.Contains(ddlOnly, forbidden) {
			t.Errorf("0022 must store NO secret/argv/host column — found %q in the DDL", forbidden)
		}
	}
	down := readMigration(t, "0022_regime_pending_verification.down.sql")
	if !strings.Contains(down, "DROP TABLE IF EXISTS pending_verification") {
		t.Error("0022 down migration must drop pending_verification")
	}
}

// TestRegimePendingWriteStoreSatisfiesInterface is the compile-time proof the durable pgx store implements the
// exact regime.PendingStore seam the async-verify channel depends on (the assertion also lives beside the impl;
// this keeps it visible in the test surface).
func TestRegimePendingWriteStoreSatisfiesInterface(t *testing.T) {
	var _ regime.PendingStore = (*RegimePendingWriteStore)(nil)
}

// contractActionIDs are the two ids the store-contract exercises. Prefixed so the DSN-gated pgx variant can
// clean exactly its own rows (the runtime role holds no DELETE, but the migration-owner test DSN does).
func contractActionIDs() []string { return []string{"t8-pending-alpha", "t8-pending-bravo"} }

// TestPendingStoreContract_MemOracle runs the store contract against the in-memory fake — the CI ORACLE (CI has
// no Postgres). It pins the exact idempotency (duplicate → ErrDuplicatePending) and fail-safe (missing →
// ErrNoPending) semantics the durable pgx store is built to mirror.
func TestPendingStoreContract_MemOracle(t *testing.T) {
	pendingStoreContract(t, regime.NewMemPendingStore())
}

// TestPendingStoreContract_Pgx runs the IDENTICAL contract against the REAL pgx store, proving byte-for-byte
// the same behavior at the durable layer: the PRIMARY KEY makes a duplicate Insert a 23505 unique-violation
// mapped to ErrDuplicatePending; a missing row is ErrNoPending; the committed prediction (a NUL-bearing
// RuleKey set) round-trips through jsonb; and a SEPARATE store instance reads a reserved launch back — the
// cross-restart durability the flip requires. Gated on TG_TEST_POSTGRES_DSN (a migrated DB); CI skips it.
func TestPendingStoreContract_Pgx(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the durable pending-store round-trip test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	clean := func() { _, _ = p.Exec(ctx, "DELETE FROM pending_verification WHERE action_id = ANY($1)", contractActionIDs()) }
	clean()
	defer clean()

	store := NewRegimePendingWriteStore(p)
	pendingStoreContract(t, store)

	// Cross-instance / cross-restart durability: a SEPARATE store over the SAME row reads the reserved launch —
	// exactly what a restarted worker sees. (The contract left a1 terminal-verified; assert it survives.)
	sep := NewRegimePendingWriteStore(p)
	got, gerr := sep.Get(ctx, "t8-pending-alpha")
	if gerr != nil {
		t.Fatalf("separate store instance could not read the durable record: %v", gerr)
	}
	if got.State != regime.StateVerified || !got.CleanRun() {
		t.Fatalf("durable record lost its terminal verdict across a fresh store instance: %+v", got)
	}
}

// pendingStoreContract exercises the full regime.PendingStore contract against an EMPTY store — the same
// assertions run against the mem oracle and the pgx store, so proving them for the fake and re-proving them for
// pgx is the "durable store matches the fake" guarantee. It walks a launch through its real lifecycle: reserve
// (Insert) → refuse a second launch (duplicate) → bind a handle (Update, still pending) → adjudicate a terminal
// verdict (Update → verified) — plus the not-found fail-safe and the deterministic state-filtered queues.
func pendingStoreContract(t *testing.T, store regime.PendingStore) {
	t.Helper()
	ctx := context.Background()
	a1, a2 := "t8-pending-alpha", "t8-pending-bravo"
	// Microsecond truncation so the timestamptz round-trip (pgx) equals the in-memory value exactly.
	launched := time.Now().UTC().Truncate(time.Microsecond)

	rec1 := regime.PendingVerification{
		ActionID:   a1,
		OpClass:    "service.restart",
		Lane:       regime.RegimeAWXJob,
		State:      regime.StatePending,
		Prediction: samplePrediction(a1),
		LaunchedAt: launched,
	}

	// Reserve: a NEW pending record inserts cleanly.
	if err := store.Insert(ctx, rec1); err != nil {
		t.Fatalf("Insert(new) = %v, want nil", err)
	}
	// Idempotency (REQ-1712): a SECOND Insert for the same action_id fails closed with ErrDuplicatePending —
	// the no-double-launch guard. (Under pgx this is the PRIMARY KEY 23505; under the fake it is the map guard.)
	if err := store.Insert(ctx, rec1); !errors.Is(err, regime.ErrDuplicatePending) {
		t.Fatalf("Insert(duplicate) = %v, want ErrDuplicatePending", err)
	}

	// Get round-trips every field, including the NUL-bearing prediction.
	got, err := store.Get(ctx, a1)
	if err != nil {
		t.Fatalf("Get(a1) = %v, want nil", err)
	}
	if got.OpClass != rec1.OpClass || got.Lane != rec1.Lane || got.State != regime.StatePending {
		t.Fatalf("Get(a1) core fields drifted: %+v", got)
	}
	if !got.LaunchedAt.Equal(launched) {
		t.Fatalf("Get(a1) launched_at = %v, want %v", got.LaunchedAt, launched)
	}
	if !samePrediction(got.Prediction, samplePrediction(a1)) {
		t.Fatalf("Get(a1) prediction did not round-trip: %+v", got.Prediction)
	}

	// Fail-safe: a missing action_id is ErrNoPending for Get and Update alike (a transition can't invent a launch).
	if _, err := store.Get(ctx, "t8-pending-nonexistent"); !errors.Is(err, regime.ErrNoPending) {
		t.Fatalf("Get(unknown) = %v, want ErrNoPending", err)
	}
	if err := store.Update(ctx, regime.PendingVerification{ActionID: "t8-pending-nonexistent", State: regime.StateVerified}); !errors.Is(err, regime.ErrNoPending) {
		t.Fatalf("Update(unknown) = %v, want ErrNoPending", err)
	}

	// BindHandle-shaped Update: record the job handle; the record stays pending (a handle is a prediction).
	rec1.JobID = "awx-job-42"
	if err := store.Update(ctx, rec1); err != nil {
		t.Fatalf("Update(bind handle) = %v, want nil", err)
	}
	if got, _ := store.Get(ctx, a1); got.JobID != "awx-job-42" || got.State != regime.StatePending {
		t.Fatalf("after bind, Get(a1) = %+v, want job bound + still pending", got)
	}

	// A second pending launch, so the state-filtered queues have something to separate.
	rec2 := regime.PendingVerification{
		ActionID: a2, OpClass: "service.restart", Lane: regime.RegimeAWXJob,
		State: regime.StatePending, Prediction: samplePrediction(a2), LaunchedAt: launched,
	}
	if err := store.Insert(ctx, rec2); err != nil {
		t.Fatalf("Insert(a2) = %v, want nil", err)
	}

	// Adjudicate a1 to a terminal clean run (the ONE transition to verified).
	rec1.State = regime.StateVerified
	rec1.TerminalStatus = regime.JobSuccessful
	rec1.Verdict = safety.VerdictMatch
	rec1.Verified = true
	rec1.ResolvedAt = launched
	if err := store.Update(ctx, rec1); err != nil {
		t.Fatalf("Update(terminal verdict) = %v, want nil", err)
	}
	gotv, _ := store.Get(ctx, a1)
	if gotv.State != regime.StateVerified || gotv.TerminalStatus != regime.JobSuccessful || !gotv.CleanRun() {
		t.Fatalf("after terminal Update, Get(a1) = %+v, want a clean verified run", gotv)
	}

	// State-filtered queues are deterministic (ordered by action_id) and honest.
	pending, err := store.List(ctx, regime.StatePending)
	if err != nil {
		t.Fatalf("List(pending) = %v", err)
	}
	if ids := actionIDs(pending); len(ids) != 1 || ids[0] != a2 {
		t.Fatalf("List(pending) = %v, want [%s] (only the un-adjudicated launch)", ids, a2)
	}
	verified, err := store.List(ctx, regime.StateVerified)
	if err != nil {
		t.Fatalf("List(verified) = %v", err)
	}
	if ids := actionIDs(verified); len(ids) != 1 || ids[0] != a1 {
		t.Fatalf("List(verified) = %v, want [%s]", ids, a1)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List(all) = %v", err)
	}
	if ids := actionIDs(all); len(ids) != 2 || ids[0] != a1 || ids[1] != a2 {
		t.Fatalf("List(all) = %v, want [%s %s] sorted by action_id", ids, a1, a2)
	}
	// An unknown filter value matches nothing (set-membership semantics, identical to the fake).
	none, err := store.List(ctx, regime.VerifyState("no-such-state"))
	if err != nil {
		t.Fatalf("List(unknown-state) = %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("List(unknown-state) = %v, want empty", actionIDs(none))
	}
}

// TestPendingPredictionRoundTrip proves the durable store's prediction serialization — the REAL code the pgx
// store runs, exercised WITHOUT Postgres — round-trips a committed prediction whose PredictedRules keys carry
// the verify.RuleKey NUL separator that jsonb cannot store. It is the CI guard against the "pgx fake hides a
// dropped field" class: a prediction field the SQL forgot to carry would fail HERE, not silently in prod.
func TestPendingPredictionRoundTrip(t *testing.T) {
	p := samplePrediction("act-roundtrip")
	raw, err := marshalPendingPrediction(p)
	if err != nil {
		t.Fatalf("marshalPendingPrediction: %v", err)
	}
	// jsonb cannot hold a NUL — the on-disk form must be NUL-free even though RuleKey uses one internally.
	if strings.ContainsRune(raw, '\x00') {
		t.Fatalf("serialized prediction contains a NUL byte (jsonb-invalid): %q", raw)
	}
	got, err := unmarshalPendingPrediction([]byte(raw))
	if err != nil {
		t.Fatalf("unmarshalPendingPrediction: %v", err)
	}
	if !samePrediction(got, p) {
		t.Fatalf("prediction did not round-trip:\n got  %+v\n want %+v", got, p)
	}
	// An empty (zero) prediction round-trips to a functionally-empty prediction (a launch reserved with none).
	rawEmpty, err := marshalPendingPrediction(verify.Prediction{})
	if err != nil {
		t.Fatalf("marshal empty prediction: %v", err)
	}
	gotEmpty, err := unmarshalPendingPrediction([]byte(rawEmpty))
	if err != nil {
		t.Fatalf("unmarshal empty prediction: %v", err)
	}
	if len(gotEmpty.PredictedHosts) != 0 || len(gotEmpty.PredictedRules) != 0 || gotEmpty.ActionID != "" {
		t.Fatalf("empty prediction did not round-trip empty: %+v", gotEmpty)
	}
}

// samplePrediction builds a committed prediction with a cascading host and a NUL-bearing (host,rule) pair, so
// the serialization test and the store contract both exercise the jsonb NUL-safety path.
func samplePrediction(actionID string) verify.Prediction {
	return verify.Prediction{
		ActionID:       actionID,
		PlanHash:       "plan-" + actionID,
		TargetHost:     "host1",
		Site:           "nllei",
		PredictedHosts: map[string]struct{}{"host2": {}, "host3": {}},
		PredictedRules: map[string]struct{}{verify.RuleKey("host2", "icmp"): {}, verify.RuleKey("host3", "bgp"): {}},
	}
}

// samePrediction compares two predictions by their scalar fields and set membership (treating a nil and an
// empty set as equal — the JSON round-trip yields an empty non-nil set for a nil input, which is functionally
// identical for the verifier).
func samePrediction(a, b verify.Prediction) bool {
	if a.ActionID != b.ActionID || a.PlanHash != b.PlanHash || a.TargetHost != b.TargetHost || a.Site != b.Site {
		return false
	}
	return sameSet(a.PredictedHosts, b.PredictedHosts) && sameSet(a.PredictedRules, b.PredictedRules)
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// actionIDs projects a record slice to its action_ids (already ordered by the store) for queue assertions.
func actionIDs(recs []regime.PendingVerification) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ActionID)
	}
	// The store guarantees action_id order; sort defensively so a mem/pgx ordering nuance never flakes the test.
	sort.Strings(out)
	return out
}
