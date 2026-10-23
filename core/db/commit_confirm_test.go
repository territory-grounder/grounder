package db

// spec/029 T-029-2 — the commit_confirm STORE drills, against a REAL migrated Postgres (0095
// applied) because every risk here is SQL semantics: the ON CONFLICT arm-idempotence read-back,
// the `WHERE state IN ('armed','held_unverifiable')` once-only transition guard, the CHECK
// vocabulary, and the server-computed deadline. They run in the same throwaway drill database the
// distillate-chain drills use (chainDrillDB — fully migrated, dropped after), so nothing here can
// touch another worktree's shared fixture.
//
// KILLING MUTATION (executed 2026-08-14): in Resolve's UPDATE, drop the state guard (`AND state
// IN (...)`) — the realistic regression is someone "fixing" a stuck row by widening the WHERE.
// TestCommitConfirmStoreResolvesOnceOnly then goes red: the second resolve overwrites the first
// and returns success, i.e. a late duplicate signal can rewrite history. Restored, green.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
)

func ccDrillRow(action, ref string) CommitConfirmRow {
	return CommitConfirmRow{
		ActionID:        action,
		ExternalRef:     ref,
		OpClass:         "restart-service",
		TargetHost:      "web01",
		Site:            "dc1",
		PlanHash:        "ph-1",
		WindowSeconds:   600,
		ForwardBand:     "AUTO",
		ForwardApproved: true,
		AlertRule:       "NginxDown",
	}
}

func TestCommitConfirmStoreArmRoundtripAndRetryIdempotence(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewCommitConfirmStore(p)

	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-act-1", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	got, err := s.Get(ctx, "cc-act-1", "cc-ref-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CommitConfirmArmed || got.OpClass != "restart-service" || got.WindowSeconds != 600 ||
		got.ResolvedAt != nil || got.InverseActionID != "" {
		t.Fatalf("armed row round-trip: %+v", got)
	}
	// 0096 (T-029-3): the fired inverse's authorization basis + the hold-watch signature ride the row.
	if got.ForwardBand != "AUTO" || !got.ForwardApproved || got.AlertRule != "NginxDown" {
		t.Fatalf("authorization-basis round-trip (0096): %+v", got)
	}
	// The deadline is SERVER-computed from the window — the row the console and the T-029-3
	// elapse consult read must agree with the timer's arithmetic.
	if d := got.DeadlineAt.Sub(got.ArmedAt); d != 600*time.Second {
		t.Fatalf("deadline must be armed_at + window (got delta %v)", d)
	}

	// A Temporal activity RETRY re-arms the identical window: success, still one row, still armed.
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-act-1", "cc-ref-1")); err != nil {
		t.Fatalf("an identical re-arm (activity retry) must be idempotent success: %v", err)
	}

	// The SAME key with a DIFFERENT window is NOT a retry — refusing is the fail-closed reading.
	diff := ccDrillRow("cc-act-1", "cc-ref-1")
	diff.WindowSeconds = 900
	if err := s.ArmCommitConfirm(ctx, diff); err == nil {
		t.Fatal("a conflicting arm with a different window must refuse")
	}

	// The same ACTION in a DIFFERENT incident arms its own row (action_id is content-addressed
	// and reused across incidents — the composite key is the point).
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-act-1", "cc-ref-2")); err != nil {
		t.Fatalf("same action, new incident must arm its own window: %v", err)
	}
}

func TestCommitConfirmStoreResolvesOnceOnly(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewCommitConfirmStore(p)

	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-act-2", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := s.Resolve(ctx, "cc-act-2", "cc-ref-1", CommitConfirmAborted, "chain refused", ""); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	got, err := s.Get(ctx, "cc-act-2", "cc-ref-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CommitConfirmAborted || got.ResolvedAt == nil || got.ResolutionDetail != "chain refused" {
		t.Fatalf("resolved row: %+v", got)
	}

	// The once-only guard: a late/duplicate signal must NOT rewrite the resolution.
	err = s.Resolve(ctx, "cc-act-2", "cc-ref-1", CommitConfirmConfirmed, "late duplicate", "")
	if !errors.Is(err, ErrCommitConfirmResolved) {
		t.Fatalf("a second resolve must report ErrCommitConfirmResolved, got %v", err)
	}
	if again, _ := s.Get(ctx, "cc-act-2", "cc-ref-1"); again.State != CommitConfirmAborted {
		t.Fatalf("the first resolution must stand, got %q", again.State)
	}

	// And a resolved window refuses to re-arm — re-arming would erase the record.
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-act-2", "cc-ref-1")); err == nil {
		t.Fatal("re-arming over a resolved window must refuse")
	}
}

// REQ-2902: held_unverifiable is the ONE non-terminal resolution — the window holds, pages, and
// may still fire on a later observed deviation (T-029-3). The store must allow armed → held →
// reverted, and nothing after that.
func TestCommitConfirmStoreHeldUnverifiableIsNonTerminal(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewCommitConfirmStore(p)

	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-act-3", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := s.Resolve(ctx, "cc-act-3", "cc-ref-1", CommitConfirmHeldUnverifiable, "verified=false — HOLD+page", ""); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := s.Resolve(ctx, "cc-act-3", "cc-ref-1", CommitConfirmReverted, "observed deviation — inverse fired", "cc-act-3-inv"); err != nil {
		t.Fatalf("a held window must still be able to revert (REQ-2902): %v", err)
	}
	got, err := s.Get(ctx, "cc-act-3", "cc-ref-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CommitConfirmReverted || got.InverseActionID != "cc-act-3-inv" {
		t.Fatalf("reverted-from-held row: %+v", got)
	}
	if err := s.Resolve(ctx, "cc-act-3", "cc-ref-1", CommitConfirmConfirmed, "no", ""); !errors.Is(err, ErrCommitConfirmResolved) {
		t.Fatalf("reverted IS terminal, got %v", err)
	}
}

// The empty-input arms and the vocabulary guards — Go-side and SQL-side both refuse.
func TestCommitConfirmStoreRefusesGarbage(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewCommitConfirmStore(p)

	empty := ccDrillRow("", "cc-ref-1")
	if err := s.ArmCommitConfirm(ctx, empty); err == nil {
		t.Fatal("empty action_id must refuse")
	}
	zero := ccDrillRow("cc-act-4", "cc-ref-1")
	zero.WindowSeconds = 0
	if err := s.ArmCommitConfirm(ctx, zero); err == nil {
		t.Fatal("non-positive window must refuse")
	}
	if err := s.Resolve(ctx, "cc-act-4", "cc-ref-1", CommitConfirmArmed, "", ""); err == nil {
		t.Fatal("'armed' is not a resolvable state")
	}
	if err := s.Resolve(ctx, "cc-act-4", "cc-ref-1", "bogus", "", ""); err == nil {
		t.Fatal("an unknown state must refuse in Go before it ever reaches SQL")
	}
	if _, err := s.Get(ctx, "cc-missing", "cc-ref-1"); err == nil {
		t.Fatal("a missing row is an error, not a zero value")
	}
	// The schema's own guard, independent of the Go store: the CHECK vocabulary refuses a state
	// the code never sends (defense against a future writer bypassing the store).
	if _, err := p.Exec(ctx, `
		INSERT INTO commit_confirm (action_id, external_ref, op_class, target_host, state, window_seconds, deadline_at)
		VALUES ('cc-act-5', 'cc-ref-1', 'restart-service', 'web01', 'bogus', 600, now())`); err == nil {
		t.Fatal("the CHECK constraint must refuse an out-of-vocabulary state")
	}
}

// The orphan sweep's read (T-029-3; the TG-82 review-#1 obligation): only ARMED rows past
// deadline+slack qualify — resolved rows never, in-window rows never. KILLING MUTATION (executed
// 2026-08-14): drop the `deadline_at < now() - make_interval(...)` conjunct — every armed row
// becomes "orphaned" the moment it arms and the sweeper double-adopts live windows; this drill
// goes red on the in-window row appearing. Restored, green.
func TestCommitConfirmStoreOverdueArmedFindsOnlyStaleArmedRows(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewCommitConfirmStore(p)

	// An armed row whose deadline is well past (arm normally, then age it in SQL — the store has
	// no test-facing clock, and the aging is the fixture's own arrangement, not the code under test).
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-stale-1", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE commit_confirm SET deadline_at = now() - interval '10 minutes' WHERE action_id = 'cc-stale-1'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	// An armed row still inside its window (deadline far out).
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-live-1", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// The BOUNDARY rows the first cut of this drill lacked (its sign-flip mutation SURVIVED —
	// caught 2026-08-14, the a-green-that-proves-nothing class): one armed row whose deadline is
	// NEAR-FUTURE (inside a flipped `now() + slack` net, outside the correct filter), and one just
	// past deadline but still INSIDE slack (excluded correctly; a dropped-slack mutation admits it).
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-nearfuture-1", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE commit_confirm SET deadline_at = now() + interval '60 seconds' WHERE action_id = 'cc-nearfuture-1'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-inslack-1", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE commit_confirm SET deadline_at = now() - interval '30 seconds' WHERE action_id = 'cc-inslack-1'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	// A RESOLVED row past deadline — must never be re-adopted.
	if err := s.ArmCommitConfirm(ctx, ccDrillRow("cc-done-1", "cc-ref-1")); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE commit_confirm SET deadline_at = now() - interval '10 minutes' WHERE action_id = 'cc-done-1'`); err != nil {
		t.Fatalf("age: %v", err)
	}
	if err := s.Resolve(ctx, "cc-done-1", "cc-ref-1", CommitConfirmConfirmed, "done", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rows, err := s.OverdueArmed(ctx, 2*time.Minute, 50)
	if err != nil {
		t.Fatalf("overdue scan: %v", err)
	}
	if len(rows) != 1 || rows[0].ActionID != "cc-stale-1" {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ActionID+":"+r.State)
		}
		t.Fatalf("exactly the stale ARMED row must surface, got %v", ids)
	}
	if rows[0].ForwardBand != "AUTO" || rows[0].AlertRule != "NginxDown" {
		t.Fatalf("the sweep's rows must carry the full basis (the adopted child seals from them), got %+v", rows[0])
	}
}

// The consult's terminus read demands BOTH keys and answers per incident — the TG-142
// sibling-collision guard repeated on the newest reader: the same content-addressed action under
// ANOTHER incident must be invisible to this incident's consult.
func TestActionExecutionExecutionForIsIncidentScoped(t *testing.T) {
	ctx := context.Background()
	p := chainDrillDB(t, ctx)
	s := NewActionExecutionStore(p)

	if err := s.Record(ctx, "cc-exec-1", "incident-A", "web01", "dc1", safety.VerdictMatch, true, ""); err != nil {
		t.Fatalf("record A: %v", err)
	}
	if err := s.Record(ctx, "cc-exec-1", "incident-B", "web01", "dc1", safety.VerdictDeviation, true, ""); err != nil {
		t.Fatalf("record B: %v", err)
	}

	a, found, err := s.ExecutionFor(ctx, "cc-exec-1", "incident-A")
	if err != nil || !found || a.Verdict != "match" {
		t.Fatalf("incident-A must see ITS run (match), got found=%v verdict=%q err=%v", found, a.Verdict, err)
	}
	b, found, err := s.ExecutionFor(ctx, "cc-exec-1", "incident-B")
	if err != nil || !found || b.Verdict != "deviation" {
		t.Fatalf("incident-B must see ITS run (deviation), got found=%v verdict=%q err=%v", found, b.Verdict, err)
	}
	if _, found, err = s.ExecutionFor(ctx, "cc-exec-1", "incident-C"); err != nil || found {
		t.Fatalf("an incident that never executed must read found=false, got found=%v err=%v", found, err)
	}
	// The unverifiable pairing survives the read: NULL verdict + unverifiable=true.
	if err := s.Record(ctx, "cc-exec-2", "incident-A", "web01", "dc1", "", false, ""); err != nil {
		t.Fatalf("record unverifiable: %v", err)
	}
	u, found, err := s.ExecutionFor(ctx, "cc-exec-2", "incident-A")
	if err != nil || !found || !u.Unverifiable || u.Verdict != "" {
		t.Fatalf("the unverifiable run must read back as (verdict='', unverifiable=true), got %+v", u)
	}
	// Both keys demanded (the hash-only lookup is refused, never answered wrongly).
	if _, _, err := s.ExecutionFor(ctx, "cc-exec-1", ""); err == nil {
		t.Fatal("a blank external_ref must refuse")
	}
}
