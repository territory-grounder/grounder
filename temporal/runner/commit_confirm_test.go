package runner

// spec/029 T-029-2 + T-029-3 — the armed-revert oracles. The claims under test, each executed
// red→green:
//
//   1. REQ-2901 fail closed: an eligible class whose window CANNOT be durably armed never reaches
//      the effect (TestRunnerRefusesForwardWhenCommitConfirmUnarmable). KILLING MUTATION (executed
//      2026-08-14, T-029-2): arm-failure fall-through in workflow.go → red with execs=1.
//   2. Arm happens BEFORE the effect; a chain refusal stands the window down as `aborted`
//      (TestRunnerAbortsWindowWhenChainRefuses). KILLING MUTATION (executed 2026-08-14, T-029-2):
//      remove the post-execute abort signal → red with the window dangling.
//   3. REQ-2902 confirm-from-the-terminus-ONLY: the child resolves from the DURABLE per-run
//      execution record — match+verified is the one confirming reading
//      (TestRunnerExecutedEffectConfirmsFromTerminus, TestCommitConfirmChildElapseRoutesByTerminus).
//      KILLING MUTATION (executed 2026-08-14, T-029-3): in ConsultCommitConfirmActivity, treat an
//      unverifiable run as confirmed (`case exec.Unverifiable: → ConsultConfirmed`) — the
//      unknown-launders-as-clean regression REQ-2902 exists to refuse. The unverifiable drill goes
//      red: the window confirms on a verdict nobody observed. Restored, green.
//   4. REQ-2901/2903: a non-confirm FIRES the inverse; every non-clean fire outcome resolves
//      revert_failed, PAGES, and TRIPS the breaker (the deviation/fire drills). KILLING MUTATION
//      (executed 2026-08-14, T-029-3): in commitConfirmFireInverse's `failed` helper, drop the
//      TripMutationBreakerActivity dispatch — REQ-2906's breaker half silently gone. The fire
//      drill goes red on trips=0. Restored, green.
//   5. REQ-2902 amended hold: unverifiable → HOLD+page, the inverse fires only on a POSITIVELY
//      observed deviation, and a failed/absent observation never fires (the hold drills).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// ccT0292Resolve is one recorded Resolve call on the fake.
type ccT0292Resolve struct {
	ActionID, ExternalRef, State, Detail, InverseActionID string
}

// ccT0292Fake is the in-memory CommitConfirmRecorder for this package's drills (and testDeps).
// It also serves the seal activity's row read (Get), returning what Arm stored.
type ccT0292Fake struct {
	mu          sync.Mutex
	arms        []db.CommitConfirmRow
	resolves    []ccT0292Resolve
	failArm     error
	failResolve error
}

func newCCT0292Fake() *ccT0292Fake { return &ccT0292Fake{} }

func (f *ccT0292Fake) ArmCommitConfirm(_ context.Context, r db.CommitConfirmRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failArm != nil {
		return f.failArm
	}
	if r.State == "" {
		r.State = db.CommitConfirmArmed // mirror the real store: an armed row IS in state armed
	}
	f.arms = append(f.arms, r)
	return nil
}

func (f *ccT0292Fake) Resolve(_ context.Context, actionID, externalRef, state, detail, inverseActionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failResolve != nil {
		return f.failResolve
	}
	f.resolves = append(f.resolves, ccT0292Resolve{ActionID: actionID, ExternalRef: externalRef, State: state, Detail: detail, InverseActionID: inverseActionID})
	// Mirror the real store: the stored row's state moves (Get must reflect resolutions — the
	// retry-recovery path reads it to tell "our own retry" from "a different winner").
	for i := range f.arms {
		if f.arms[i].ActionID == actionID && f.arms[i].ExternalRef == externalRef {
			f.arms[i].State, f.arms[i].ResolutionDetail, f.arms[i].InverseActionID = state, detail, inverseActionID
		}
	}
	return nil
}

func (f *ccT0292Fake) Get(_ context.Context, actionID, externalRef string) (db.CommitConfirmRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.arms) - 1; i >= 0; i-- {
		if f.arms[i].ActionID == actionID && f.arms[i].ExternalRef == externalRef {
			return f.arms[i], nil
		}
	}
	return db.CommitConfirmRow{}, errors.New("ccT0292Fake: no such row")
}

func (f *ccT0292Fake) snapshot() (arms []db.CommitConfirmRow, resolves []ccT0292Resolve) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.CommitConfirmRow(nil), f.arms...), append([]ccT0292Resolve(nil), f.resolves...)
}

// ccT0293FakeExecs is the consult's terminus fake: `answer` (when set) is returned for ANY pair —
// the runner drills cannot pre-know content-addressed action ids, and the consult's routing is
// what is under test, not the key plumbing (the DB-side ExecutionFor drills own that).
type ccT0293FakeExecs struct {
	mu     sync.Mutex
	answer *db.ForwardExecution
	err    error
}

func newCCT0293FakeExecs() *ccT0293FakeExecs { return &ccT0293FakeExecs{} }

func (f *ccT0293FakeExecs) set(e db.ForwardExecution) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = &e
}

func (f *ccT0293FakeExecs) ExecutionFor(_ context.Context, actionID, externalRef string) (db.ForwardExecution, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return db.ForwardExecution{}, false, f.err
	}
	if f.answer == nil {
		return db.ForwardExecution{}, false, nil
	}
	e := *f.answer
	e.ActionID, e.ExternalRef = actionID, externalRef
	return e, true, nil
}

// ccT0292CaptureSink records governance-ledger appends (REQ-2906's observability claim).
type ccT0292CaptureSink struct {
	mu      sync.Mutex
	entries []audit.LedgerEntry
}

func (s *ccT0292CaptureSink) Persist(e audit.LedgerEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *ccT0292CaptureSink) decisions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.Decision)
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// Activity-level drills
// ---------------------------------------------------------------------------------------------

func ccT0292ArmInput() ArmCommitConfirmInput {
	return ArmCommitConfirmInput{
		ActionID:    "act-cc-1",
		ExternalRef: "TG-cc-1",
		OpClass:     "restart-service",
		TargetHost:  "web01",
		Site:        "dc1",
		PlanHash:    "ph-1",
		Band:        safety.BandAuto.String(),
		Approved:    false,
		AlertRule:   "NginxDown",
	}
}

// The eligible class arms with the REGISTRY's window, the durable row carries the sealed identity
// AND the fired inverse's authorization basis (band/approved/alert_rule — T-029-3, migration
// 0096), and the arm appends to the governance ledger (REQ-2906).
func TestCommitConfirmArmActivityArmsEligibleClassFromRegistryData(t *testing.T) {
	spec, ok := opschema.Lookup("restart-service")
	if !ok || spec.CommitConfirmed == nil {
		t.Fatal("precondition: restart-service must be commit-confirmed eligible in the registry (T-029-1)")
	}
	fake := newCCT0292Fake()
	sink := &ccT0292CaptureSink{}
	acts := NewActivities(Deps{CommitConfirm: fake, Ledger: audit.NewLedger().WithSink(sink)})

	res, err := acts.ArmCommitConfirmActivity(context.Background(), ccT0292ArmInput())
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if !res.Eligible || res.WindowSeconds != int64(spec.CommitConfirmed.WindowSeconds) {
		t.Fatalf("arm must report the registry's own window (eligible=%v window=%d, registry %d)",
			res.Eligible, res.WindowSeconds, spec.CommitConfirmed.WindowSeconds)
	}
	arms, resolves := fake.snapshot()
	if len(arms) != 1 || len(resolves) != 0 {
		t.Fatalf("exactly one durable arm and no resolve, got %d/%d", len(arms), len(resolves))
	}
	row := arms[0]
	if row.ActionID != "act-cc-1" || row.ExternalRef != "TG-cc-1" || row.OpClass != "restart-service" ||
		row.TargetHost != "web01" || row.WindowSeconds != res.WindowSeconds {
		t.Fatalf("the armed row must carry the sealed identity + window, got %+v", row)
	}
	if row.ForwardBand != safety.BandAuto.String() || row.ForwardApproved || row.AlertRule != "NginxDown" {
		t.Fatalf("the armed row must carry the inverse's authorization basis + the incident signature (0096), got %+v", row)
	}
	if ds := sink.decisions(); len(ds) != 1 || ds[0] != "commit-confirm:armed" {
		t.Fatalf("REQ-2906: the arm must append exactly one commit-confirm:armed ledger entry, got %v", ds)
	}
}

// A class without a commit_confirmed declaration is NOT eligible and the activity touches nothing.
func TestCommitConfirmArmActivityNotEligibleTouchesNothing(t *testing.T) {
	if spec, ok := opschema.Lookup("start-service"); !ok || spec.CommitConfirmed != nil {
		t.Fatal("precondition: start-service must exist in the registry WITHOUT commit_confirmed")
	}
	fake := newCCT0292Fake()
	sink := &ccT0292CaptureSink{}
	acts := NewActivities(Deps{CommitConfirm: fake, Ledger: audit.NewLedger().WithSink(sink)})
	in := ccT0292ArmInput()
	in.OpClass = "start-service"

	res, err := acts.ArmCommitConfirmActivity(context.Background(), in)
	if err != nil || res.Eligible {
		t.Fatalf("not-eligible must be a clean no-op: res=%+v err=%v", res, err)
	}
	arms, resolves := fake.snapshot()
	if len(arms) != 0 || len(resolves) != 0 || len(sink.decisions()) != 0 {
		t.Fatalf("not-eligible must write NOTHING (arms=%d resolves=%d ledger=%v)", len(arms), len(resolves), sink.decisions())
	}
}

// REQ-2901 fail closed at the activity: empty identity, a nil store on an ELIGIBLE class, and a
// store failure each ERROR — never a quiet skip.
func TestCommitConfirmArmActivityFailsClosed(t *testing.T) {
	ledger := audit.NewLedger()

	empty := ccT0292ArmInput()
	empty.ActionID = ""
	if _, err := NewActivities(Deps{CommitConfirm: newCCT0292Fake(), Ledger: ledger}).
		ArmCommitConfirmActivity(context.Background(), empty); err == nil {
		t.Fatal("an empty action_id must refuse to arm")
	}
	if _, err := NewActivities(Deps{Ledger: ledger}).
		ArmCommitConfirmActivity(context.Background(), ccT0292ArmInput()); err == nil || !strings.Contains(err.Error(), "fail closed") {
		t.Fatalf("nil store + eligible class must fail closed, got %v", err)
	}
	failing := newCCT0292Fake()
	failing.failArm = errors.New("pg down")
	if _, err := NewActivities(Deps{CommitConfirm: failing, Ledger: ledger}).
		ArmCommitConfirmActivity(context.Background(), ccT0292ArmInput()); err == nil {
		t.Fatal("a durable-write failure must propagate — the workflow refuses the forward on it")
	}
}

// The already-resolved path distinguishes two cases (the T-029-4 round-2 review's finding #1):
// a DIFFERENT-state duplicate is quiet success with no ledger entry (the winner's own attempt
// owns its tail); a SAME-state hit is OUR OWN RETRY — the transition landed, the tail did not —
// and the tail (ledger + feed) RUNS NOW. A real store failure still propagates.
//
// KILLING MUTATION (executed 2026-08-15): restore the silent early-return (`return nil` on
// ErrCommitConfirmResolved unconditionally — the pre-fix shape). The retry half goes red: a
// transient ledger blip after the transition permanently and silently drops the ledger entry
// AND the graduation feed for a confirmed window. Restored, green.
func TestCommitConfirmResolveActivityDuplicateVsRetryRecovery(t *testing.T) {
	// DIFFERENT-state duplicate: quiet, no ledger, no feed.
	sink := &ccT0292CaptureSink{}
	dup := newCCT0292Fake()
	if err := dup.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", OpClass: "restart-service", TargetHost: "web01", WindowSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	if err := dup.Resolve(context.Background(), "act-cc-1", "TG-cc-1", db.CommitConfirmConfirmed, "winner", ""); err != nil {
		t.Fatal(err)
	}
	dup.failResolve = db.ErrCommitConfirmResolved
	feeds := 0
	acts := NewActivities(Deps{CommitConfirm: dup, Ledger: audit.NewLedger().WithSink(sink),
		RecordGraduation: func(context.Context, string, string, bool) error { feeds++; return nil }})
	if err := acts.ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", State: db.CommitConfirmAborted, Detail: "late duplicate"}); err != nil {
		t.Fatalf("a different-state duplicate is quiet success, got %v", err)
	}
	if len(sink.decisions()) != 0 || feeds != 0 {
		t.Fatalf("a different-state duplicate must write NOTHING (ledger=%v feeds=%d)", sink.decisions(), feeds)
	}

	// SAME-state retry: the transition landed earlier (ledger blip killed the tail) — the retry
	// must RECOVER the tail: ledger entry + graduation feed, exactly once.
	if err := acts.ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", State: db.CommitConfirmConfirmed, Detail: "retry"}); err != nil {
		t.Fatalf("the same-state retry must succeed running the tail, got %v", err)
	}
	if ds := sink.decisions(); len(ds) != 1 || ds[0] != "commit-confirm:confirmed" {
		t.Fatalf("the retry must recover the LEDGER entry, got %v", ds)
	}
	if feeds != 1 {
		t.Fatalf("the retry must recover the graduation FEED exactly once, got %d", feeds)
	}

	real := newCCT0292Fake()
	real.failResolve = errors.New("pg down")
	acts = NewActivities(Deps{CommitConfirm: real, Ledger: audit.NewLedger().WithSink(sink)})
	if err := acts.ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", State: db.CommitConfirmAborted, Detail: "x"}); err == nil {
		t.Fatal("a real store failure must propagate")
	}
}

// The consult's routing table, REQ-2902 over durable state: match+verified is the ONLY confirming
// reading; unverifiable holds; partial/deviation route to the inverse; nothing-at-the-terminus is
// pending; and the executed-belt (manifest chain says executed, per-run record missing) is
// unverifiable, never aborted — the record write is best-effort and its absence must not stand
// down a window over a mutation that provably ran.
func TestConsultCommitConfirmRoutesTheTerminus(t *testing.T) {
	execsFor := func(e *db.ForwardExecution) *ccT0293FakeExecs {
		f := newCCT0293FakeExecs()
		if e != nil {
			f.set(*e)
		}
		return f
	}
	cases := []struct {
		name string
		exec *db.ForwardExecution
		want string
	}{
		{"nothing recorded", nil, ConsultPending},
		{"match verified confirms", &db.ForwardExecution{Verdict: "match"}, ConsultConfirmed},
		{"partial routes to the inverse", &db.ForwardExecution{Verdict: "partial"}, ConsultDeviation},
		{"deviation routes to the inverse", &db.ForwardExecution{Verdict: "deviation"}, ConsultDeviation},
		{"unverifiable holds", &db.ForwardExecution{Unverifiable: true}, ConsultUnverifiable},
		{"garbage verdict holds, never confirms", &db.ForwardExecution{Verdict: "weird"}, ConsultUnverifiable},
	}
	for _, tc := range cases {
		acts := NewActivities(Deps{Executions: execsFor(tc.exec), Ledger: audit.NewLedger()})
		got, err := acts.ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: "a", ExternalRef: "r"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Disposition != tc.want {
			t.Fatalf("%s: disposition %q, want %q", tc.name, got.Disposition, tc.want)
		}
	}
	// Nil reader refuses (the child then keeps the window armed rather than guessing).
	if _, err := NewActivities(Deps{Ledger: audit.NewLedger()}).
		ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: "a", ExternalRef: "r"}); err == nil {
		t.Fatal("a consult with no execution reader must refuse")
	}
}

// The live-observation half of the consult: ObservedAlerting is POSITIVE-ONLY — set true only when
// the observer read OK and the target itself alerts; nil when unobservable. The hold-watch fires
// on true alone, so a monitoring outage can never fire a revert (the forwardEffectPresent
// inversion trap, refused by construction).
func TestConsultCommitConfirmObservesPositively(t *testing.T) {
	mk := func(observe func(context.Context, string, string) ([]verify.ObservedAlert, bool)) *Activities {
		f := newCCT0293FakeExecs()
		f.set(db.ForwardExecution{Verdict: "match", TargetHost: "web01", Site: "dc1"})
		return NewActivities(Deps{Executions: f, PostStateObserve: observe, Ledger: audit.NewLedger()})
	}
	in := ConsultCommitConfirmInput{ActionID: "a", ExternalRef: "r"}

	got, err := mk(func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown"}}, true
	}).ConsultCommitConfirmActivity(context.Background(), in)
	if err != nil || got.ObservedAlerting == nil || !*got.ObservedAlerting {
		t.Fatalf("target alerting must read ObservedAlerting=true, got %+v err=%v", got.ObservedAlerting, err)
	}
	got, err = mk(func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return []verify.ObservedAlert{{Host: "other01", Rule: "X"}}, true
	}).ConsultCommitConfirmActivity(context.Background(), in)
	if err != nil || got.ObservedAlerting == nil || *got.ObservedAlerting {
		t.Fatalf("only the TARGET's own alert counts, got %+v err=%v", got.ObservedAlerting, err)
	}
	got, err = mk(func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
		return nil, false
	}).ConsultCommitConfirmActivity(context.Background(), in)
	if err != nil || got.ObservedAlerting != nil {
		t.Fatalf("a failed read must leave ObservedAlerting nil (never fires anything), got %+v err=%v", got.ObservedAlerting, err)
	}
}

// TestConsultCommitConfirmDurableSubstituteConfirmTG499 pins REQ-2902's durable-substitute carve-out: a
// STATE-PRECONDITIONED guest heal whose terminus was unobservable (exec.Unverifiable) confirms ONLY on a
// FRESH POSITIVE guest_liveness re-read that the guest holds its desired end state, and STILL HOLDs on
// every unobservable path. The fail-closed cases (ii)-(v) are each RED under the naive
// "unverifiable→confirmed" mutation (the T-029-3 killing mutation) — which is what makes the carve-out a
// disciplined durable-substitute confirm and not the unknown-launders-as-clean regression REQ-2902 bars.
func TestConsultCommitConfirmDurableSubstituteConfirmTG499(t *testing.T) {
	// start-guest: RequiresTargetState "not-running" (precondition) ⇒ DESIRED END STATE = running.
	mk := func(t *testing.T, op, opClass, target string, params map[string]string,
		gr func(context.Context, string) (bool, string, bool)) (*Activities, string) {
		sink := &fakeManifestSink{}
		m, err := manifest.New(manifest.Action{Op: op, OpClass: opClass, Target: target, Params: params, Reversible: true},
			safety.BandAuto, "ph-499", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Seal(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		execs := newCCT0293FakeExecs()
		execs.set(db.ForwardExecution{Unverifiable: true, TargetHost: target, Site: "dc1"})
		d := Deps{Executions: execs, Manifests: sink, Ledger: audit.NewLedger()}
		if gr != nil {
			d.Gate = &predict.PredictionGate{GuestRunning: gr}
		}
		return NewActivities(d), m.ActionID
	}
	disp := func(a *Activities, id string) string {
		got, err := a.ConsultCommitConfirmActivity(context.Background(), ConsultCommitConfirmInput{ActionID: id, ExternalRef: "r"})
		if err != nil {
			t.Fatal(err)
		}
		return got.Disposition
	}
	guest := map[string]string{"guest": "librespeed01"}

	// (i) fresh POSITIVE guest_liveness reads running (= the desired end state) ⇒ CONFIRMED.
	a, id := mk(t, "start", "start-guest", "librespeed01", guest,
		func(context.Context, string) (bool, string, bool) { return true, "guest_liveness", true })
	if d := disp(a, id); d != ConsultConfirmed {
		t.Fatalf("(i) fresh POSITIVE guest_liveness (running) must confirm the unverifiable start-guest, got %q", d)
	}
	// (ii) THE critical fail-closed case: an UNREADABLE/stale guest_liveness (ok=false) must NOT confirm.
	a, id = mk(t, "start", "start-guest", "librespeed01", guest,
		func(context.Context, string) (bool, string, bool) { return false, "guest_liveness read error", false })
	if d := disp(a, id); d != ConsultUnverifiable {
		t.Fatalf("(ii) an unreadable/stale guest_liveness must HOLD — never confirm on absence, got %q", d)
	}
	// (ii') the freshness guard in ISOLATION: a reading that SHOWS the desired state but is NOT fresh
	// (ok=false) must NOT confirm — pins that obsOK, not just the running bit, gates the confirm, so a
	// stale projection can never launder as a live confirm even if it happens to show the wanted state.
	a, id = mk(t, "start", "start-guest", "librespeed01", guest,
		func(context.Context, string) (bool, string, bool) { return true, "guest_liveness (stale)", false })
	if d := disp(a, id); d != ConsultUnverifiable {
		t.Fatalf("(ii') a stale guest_liveness showing running but ok=false must HOLD (obsOK gates the confirm), got %q", d)
	}
	// (iii) a fresh read but the guest is NOT in the desired end state (still down) ⇒ HOLD.
	a, id = mk(t, "start", "start-guest", "librespeed01", guest,
		func(context.Context, string) (bool, string, bool) { return false, "guest_liveness", true })
	if d := disp(a, id); d != ConsultUnverifiable {
		t.Fatalf("(iii) a target NOT in the desired end state must HOLD, got %q", d)
	}
	// (iv) no guest_liveness reader wired at all ⇒ HOLD — the SAME posture the existing T-029-3
	// "unverifiable holds" case asserts (no Gate → no upgrade), so that drill stays green.
	a, id = mk(t, "start", "start-guest", "librespeed01", guest, nil)
	if d := disp(a, id); d != ConsultUnverifiable {
		t.Fatalf("(iv) no guest_liveness reader → unverifiable must HOLD, got %q", d)
	}
	// (v) a NON-state-preconditioned op (restart-service, no RequiresTargetState) ⇒ the substitute does
	// not apply; HOLD even with a positive guest_liveness. The carve-out is scoped to guest-lifecycle.
	a, id = mk(t, "restart", "restart-service", "web01", map[string]string{"unit": "nginx"},
		func(context.Context, string) (bool, string, bool) { return true, "guest_liveness", true })
	if d := disp(a, id); d != ConsultUnverifiable {
		t.Fatalf("(v) a non-state-preconditioned op must NOT confirm on guest_liveness, got %q", d)
	}
}

// TestConsultCommitConfirmDurableSubstituteConfirmServiceTG461 pins the SERVICE-fault half of REQ-2902's
// durable-substitute carve-out: a NON-state-preconditioned commit-confirm-eligible heal (restart-service)
// whose terminus was unobservable confirms ONLY on a POSITIVE captured ingest_transition recovery
// (RecoveredSince true) scoped to the incident's rule, and STILL HOLDs on every other path. Each fail-closed
// case is RED under a naive "confirm on the absence of an open incident" mutation — which is what keeps the
// carve-out a disciplined durable-substitute confirm and not the unknown-launders-as-clean regression.
func TestConsultCommitConfirmDurableSubstituteConfirmServiceTG461(t *testing.T) {
	execTime := time.Unix(1_700_000_000, 0)
	mk := func(t *testing.T, op, opClass, target string,
		rs func(context.Context, string, string, time.Time) (bool, error)) (*Activities, string) {
		sink := &fakeManifestSink{}
		m, err := manifest.New(manifest.Action{Op: op, OpClass: opClass, Target: target,
			Params: map[string]string{"unit": "nginx"}, Reversible: true}, safety.BandAuto, "ph-461", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Seal(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		execs := newCCT0293FakeExecs()
		execs.set(db.ForwardExecution{Unverifiable: true, TargetHost: target, Site: "dc1", ExecutedAt: execTime})
		d := Deps{Executions: execs, Manifests: sink, Ledger: audit.NewLedger()}
		if rs != nil {
			d.RecoveredSince = rs
		}
		return NewActivities(d), m.ActionID
	}
	disp := func(a *Activities, id, alertRule string) string {
		got, err := a.ConsultCommitConfirmActivity(context.Background(),
			ConsultCommitConfirmInput{ActionID: id, ExternalRef: "r", AlertRule: alertRule})
		if err != nil {
			t.Fatal(err)
		}
		return got.Disposition
	}
	yes := func(context.Context, string, string, time.Time) (bool, error) { return true, nil }

	// (i) a POSITIVE captured recovery ⇒ CONFIRMED.
	a, id := mk(t, "restart", "restart-service", "web01", yes)
	if d := disp(a, id, "ServiceDown"); d != ConsultConfirmed {
		t.Fatalf("(i) a POSITIVE captured ingest_transition recovery must confirm the unverifiable service heal, got %q", d)
	}
	// (ii) THE critical fail-closed case: NO captured recovery ⇒ HOLD — never confirm on the absence of one.
	a, id = mk(t, "restart", "restart-service", "web01",
		func(context.Context, string, string, time.Time) (bool, error) { return false, nil })
	if d := disp(a, id, "ServiceDown"); d != ConsultUnverifiable {
		t.Fatalf("(ii) NO captured recovery must HOLD — never confirm on absence, got %q", d)
	}
	// (iii) a recovery-reader error ⇒ HOLD (fail-closed on a read error).
	a, id = mk(t, "restart", "restart-service", "web01",
		func(context.Context, string, string, time.Time) (bool, error) { return false, context.DeadlineExceeded })
	if d := disp(a, id, "ServiceDown"); d != ConsultUnverifiable {
		t.Fatalf("(iii) a recovery read error must HOLD, got %q", d)
	}
	// (iv) no recovery reader wired ⇒ HOLD.
	a, id = mk(t, "restart", "restart-service", "web01", nil)
	if d := disp(a, id, "ServiceDown"); d != ConsultUnverifiable {
		t.Fatalf("(iv) no RecoveredSince reader → unverifiable must HOLD, got %q", d)
	}
	// (v) an EMPTY alert rule ⇒ HOLD — the belt cannot scope to the incident, so it cannot confirm.
	a, id = mk(t, "restart", "restart-service", "web01", yes)
	if d := disp(a, id, ""); d != ConsultUnverifiable {
		t.Fatalf("(v) an empty alert rule must HOLD — the durable belt cannot scope, got %q", d)
	}
	// (vi) a STATE-PRECONDITIONED guest class must NOT route through the service recovery path (it belongs to
	// the guest_liveness slice), even with a positive recovery + no Gate — pins no cross-serve between slices.
	a, id = mk(t, "start", "start-guest", "librespeed01", yes)
	if d := disp(a, id, "Device-Down"); d != ConsultUnverifiable {
		t.Fatalf("(vi) a state-preconditioned guest class must NOT confirm via the service recovery path, got %q", d)
	}
}

// The breaker-trip seam fails loudly on a nil wire — a deployment that fires inverses but cannot
// halt on their failure is missing the half of the control that makes failure safe.
func TestTripMutationBreakerActivityFailsLoudOnNilSeam(t *testing.T) {
	if err := NewActivities(Deps{}).TripMutationBreakerActivity(context.Background(), "x"); err == nil {
		t.Fatal("nil breaker seam must error, not skip")
	}
	tripped := ""
	acts := NewActivities(Deps{BreakerTrip: func(_ context.Context, reason string) error { tripped = reason; return nil }})
	if err := acts.TripMutationBreakerActivity(context.Background(), "why"); err != nil || tripped != "why" {
		t.Fatalf("wired trip must pass through, got err=%v reason=%q", err, tripped)
	}
}

// The auto-fired execute carries the FORWARD's recorded basis; the manual lane keeps its
// vote-gate constant; and the request band is the manifest's own sealed band (pure function —
// asserted directly, the same way the manual lane's tests assert this request).
func TestBuildRollbackRequestCarriesTheSealedBandAndBasis(t *testing.T) {
	inverse := inverseActionFor(RollbackInput{ForwardActionID: "fwd-1", ForwardOpClass: "restart-service",
		ForwardOp: "restart", ForwardTarget: "web01", ForwardParams: map[string]string{"unit": "nginx"},
		ForwardReversible: true, RollbackExternalRef: "ccrevert-r1"})
	m, err := manifest.New(inverse, safety.BandAuto, "ph-cc-1", "")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	req := buildRollbackRequest(m, []string{"systemctl", "restart", "nginx"},
		RollbackInput{ForwardActionID: "fwd-1", RollbackExternalRef: "ccrevert-r1"},
		false /* the auto lane's unapproved-forward basis */, nil, nil, nil, nil, nil)
	if req.Band != safety.BandAuto {
		t.Fatalf("the request band must be the manifest's SEALED band (the auto lane seals at the forward's band), got %v", req.Band)
	}
	if req.Approved {
		t.Fatal("an unapproved forward's basis must reach the interceptor as Approved=false — the chain, not this lane, decides what that refuses")
	}
	if req.InvertsActionID != "fwd-1" {
		t.Fatalf("the inverse must name its forward (TG-404), got %q", req.InvertsActionID)
	}
}

// ---------------------------------------------------------------------------------------------
// Child-workflow drills (the dead-man window itself)
// ---------------------------------------------------------------------------------------------

type ccT0293ChildDeps struct {
	fake  *ccT0292Fake
	execs *ccT0293FakeExecs
	sink  *ccT0292CaptureSink
	trips *[]string
	deps  Deps
}

func ccT0293ChildEnv(t *testing.T, mut func(*Deps)) (*testsuite.TestWorkflowEnvironment, *ccT0293ChildDeps) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	d := &ccT0293ChildDeps{fake: newCCT0292Fake(), execs: newCCT0293FakeExecs(), sink: &ccT0292CaptureSink{}}
	trips := []string{}
	d.trips = &trips
	d.deps = Deps{
		CommitConfirm: d.fake,
		Executions:    d.execs,
		Ledger:        audit.NewLedger().WithSink(d.sink),
		BreakerTrip:   func(_ context.Context, reason string) error { trips = append(trips, reason); return nil },
	}
	if mut != nil {
		mut(&d.deps)
	}
	acts := NewActivities(d.deps)
	env.RegisterWorkflow(CommitConfirmWorkflow)
	env.RegisterActivity(acts.ResolveCommitConfirmActivity)
	env.RegisterActivity(acts.ConsultCommitConfirmActivity)
	env.RegisterActivity(acts.SealCommitConfirmInverseActivity)
	env.RegisterActivity(acts.TripMutationBreakerActivity)
	env.RegisterActivity(acts.NotifyActivity)
	env.RegisterActivity(acts.SealRollbackExecuteActivity)
	return env, d
}

func ccT0292ChildInput() CommitConfirmInput {
	return CommitConfirmInput{ActionID: "act-cc-1", ExternalRef: "TG-cc-1", WindowSeconds: 600}
}

// An abort signal (the forward provably did not execute) resolves the window `aborted` — once.
func TestCommitConfirmChildAbortResolvesAborted(t *testing.T) {
	env, d := ccT0293ChildEnv(t, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommitConfirmSignalName, CommitConfirmResolve{Kind: "abort", Detail: "chain refused"})
	}, time.Second)
	env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("child must complete cleanly: %v", env.GetWorkflowError())
	}
	_, resolves := d.fake.snapshot()
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmAborted {
		t.Fatalf("exactly one resolve to aborted, got %+v", resolves)
	}
	if ds := d.sink.decisions(); len(ds) != 1 || ds[0] != "commit-confirm:aborted" {
		t.Fatalf("REQ-2906: the abort must append commit-confirm:aborted, got %v", ds)
	}
}

// The elapse consult routes the four terminus readings (REQ-2902 over durable state): nothing →
// aborted; match+verified → confirmed; unverifiable → held (with page); deviation → the fire path
// (here: seal refuses with no manifest store ⇒ revert_failed + page + BREAKER TRIP).
func TestCommitConfirmChildElapseRoutesByTerminus(t *testing.T) {
	t.Run("nothing at the terminus HOLDS, never a silent stand-down", func(t *testing.T) {
		// Review finding #1's consequence guard: absence at elapse is NOT provable non-execution
		// (a chain-gap run can lose every terminus write in a store outage), so the window HOLDS
		// and pages — provable non-execution resolves only via the parent's abort signal.
		env, d := ccT0293ChildEnv(t, nil)
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmHeldUnverifiable {
			t.Fatalf("nothing-at-the-terminus must HOLD (operator-owned), never abort over a possibly-live mutation, got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a hold must not trip the breaker, got %v", *d.trips)
		}
	})
	t.Run("match verified confirms", func(t *testing.T) {
		env, d := ccT0293ChildEnv(t, nil)
		d.execs.set(db.ForwardExecution{Verdict: "match"})
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmConfirmed {
			t.Fatalf("want confirmed, got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a confirm must not touch the breaker, got %v", *d.trips)
		}
	})
	t.Run("deviation fires the inverse and a refused fire pages and trips", func(t *testing.T) {
		env, d := ccT0293ChildEnv(t, nil)
		d.execs.set(db.ForwardExecution{Verdict: "deviation"})
		// Arm the row the seal activity will load; no Manifests store is wired, so the fire path
		// exercises the seal-refusal arm — the "unrevertable armed revert is an incident" claim.
		if err := d.fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
			ActionID: "act-cc-1", ExternalRef: "TG-cc-1", OpClass: "restart-service",
			TargetHost: "web01", WindowSeconds: 600}); err != nil {
			t.Fatal(err)
		}
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmRevertFailed {
			t.Fatalf("a refused fire must resolve revert_failed, got %+v", resolves)
		}
		if len(*d.trips) != 1 {
			t.Fatalf("REQ-2906: revert_failed must trip the mutation breaker exactly once, got %v", *d.trips)
		}
	})
	t.Run("unverifiable holds and an exhausted watch stays held", func(t *testing.T) {
		env, d := ccT0293ChildEnv(t, nil)
		d.execs.set(db.ForwardExecution{Unverifiable: true})
		env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
		if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
			t.Fatalf("child: %v", env.GetWorkflowError())
		}
		_, resolves := d.fake.snapshot()
		if len(resolves) != 1 || resolves[0].State != db.CommitConfirmHeldUnverifiable {
			t.Fatalf("want held_unverifiable (and ONLY that — no fire without an observed deviation), got %+v", resolves)
		}
		if len(*d.trips) != 0 {
			t.Fatalf("a hold must not trip the breaker, got %v", *d.trips)
		}
	})
}

// REQ-2902's amended hold, the firing half: a POSITIVELY observed deviation during the watch
// fires the inverse (held → revert_failed here, since the seal refuses without a manifest store —
// the transition out of held is the claim, plus the breaker trip on the failed fire).
func TestCommitConfirmChildHoldFiresOnObservedDeviation(t *testing.T) {
	env, d := ccT0293ChildEnv(t, func(deps *Deps) {
		deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) {
			return []verify.ObservedAlert{{Host: "web01", Rule: "NginxDown"}}, true
		}
	})
	d.execs.set(db.ForwardExecution{Unverifiable: true, TargetHost: "web01", Site: "dc1"})
	if err := d.fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", OpClass: "restart-service",
		TargetHost: "web01", WindowSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	env.ExecuteWorkflow(CommitConfirmWorkflow, ccT0292ChildInput())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("child: %v", env.GetWorkflowError())
	}
	_, resolves := d.fake.snapshot()
	if len(resolves) != 2 || resolves[0].State != db.CommitConfirmHeldUnverifiable || resolves[1].State != db.CommitConfirmRevertFailed {
		t.Fatalf("want held then the fired arm (revert_failed via seal refusal), got %+v", resolves)
	}
	if len(*d.trips) != 1 {
		t.Fatalf("the failed fire out of the hold must trip the breaker, got %v", *d.trips)
	}
}

// Review finding #2's structural close: the forward's human vote extends ONLY to a SELF-INVERSE
// (rollback_template — the identical action shape the vote authorized). A CLASS inverse
// (rollback_op_class, e.g. start-guest→stop-guest) NEVER inherits the vote — it earns autonomy
// through its own inverse_only policy rule or it polls/refuses+pages at the chain.
func TestSealCommitConfirmInverseVoteExtendsOnlyToSelfInverse(t *testing.T) {
	mkDeps := func(opClass, target string) (Deps, *ccT0292Fake, *fakeManifestSink) {
		fake := newCCT0292Fake()
		if err := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
			ActionID: "act-basis-1", ExternalRef: "TG-basis-1", OpClass: opClass,
			TargetHost: target, WindowSeconds: 600, ForwardBand: safety.BandAuto.String(),
			ForwardApproved: true, // the forward WAS human-approved — the bit under test
		}); err != nil {
			t.Fatal(err)
		}
		sink := &fakeManifestSink{}
		return Deps{CommitConfirm: fake, Manifests: sink, ManifestSink: sink, Ledger: audit.NewLedger()}, fake, sink
	}
	sealForward := func(t *testing.T, sink *fakeManifestSink, op, opClass, target string, params map[string]string) string {
		m, err := manifest.New(manifest.Action{Op: op, OpClass: opClass, Target: target, Params: params, Reversible: true},
			safety.BandAuto, "ph-basis", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Seal(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		return m.ActionID
	}

	// SELF-INVERSE (restart-service, rollback_template): the vote carries.
	deps, fake, sink := mkDeps("restart-service", "web01")
	fwdID := sealForward(t, sink, "restart", "restart-service", "web01", map[string]string{"unit": "nginx"})
	// re-key the armed row to the sealed manifest's id
	fake.arms[0].ActionID = fwdID
	res, err := NewActivities(deps).SealCommitConfirmInverseActivity(context.Background(),
		SealCommitConfirmInverseInput{ActionID: fwdID, ExternalRef: "TG-basis-1"})
	if err != nil || !res.Sealed {
		t.Fatalf("self-inverse seal must succeed: sealed=%v reason=%q err=%v", res.Sealed, res.Reason, err)
	}
	if !res.ApprovedBasis {
		t.Fatal("a SELF-inverse of a voted forward carries the vote (the identical action shape was authorized)")
	}

	// CLASS INVERSE (start-guest→stop-guest): the vote must NOT carry. GuestRunning answers
	// running=true so the blind-stop precondition passes and the basis bit is the only variable.
	deps2, fake2, sink2 := mkDeps("start-guest", "librespeed01")
	deps2.Gate = &predict.PredictionGate{GuestRunning: func(context.Context, string) (bool, string, bool) {
		return true, "drill: observed running", true
	}}
	fwdID2 := sealForward(t, sink2, "start", "start-guest", "librespeed01", map[string]string{"guest": "librespeed01"})
	fake2.arms[0].ActionID = fwdID2
	res2, err := NewActivities(deps2).SealCommitConfirmInverseActivity(context.Background(),
		SealCommitConfirmInverseInput{ActionID: fwdID2, ExternalRef: "TG-basis-1"})
	if err != nil || !res2.Sealed {
		t.Fatalf("class-inverse seal must succeed: sealed=%v reason=%q err=%v", res2.Sealed, res2.Reason, err)
	}
	if res2.ApprovedBasis {
		t.Fatal("a CLASS inverse is a DIFFERENT action — the forward's vote must NOT authorize it (INV-12 stays narrow); it earns autonomy via its inverse_only policy rule instead")
	}
}

// A non-positive window refuses to start at all.
func TestCommitConfirmChildRefusesNonPositiveWindow(t *testing.T) {
	env, _ := ccT0293ChildEnv(t, nil)
	in := ccT0292ChildInput()
	in.WindowSeconds = 0
	env.ExecuteWorkflow(CommitConfirmWorkflow, in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() == nil {
		t.Fatal("a zero window must fail the child, not arm a nothing")
	}
}

// ---------------------------------------------------------------------------------------------
// Full-RunnerWorkflow drills (the wiring: armed BEFORE the effect, refuse-forward, abort, confirm)
// ---------------------------------------------------------------------------------------------

func ccT0292ProposeScript() []string {
	return []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-cc-e2e","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
}

func ccT0292Envelope() ingest.IncidentEnvelope {
	return ingest.IncidentEnvelope{ExternalRef: "TG-cc-e2e", Host: "web01", AlertRule: "NginxDown",
		Severity: ingest.SeverityWarning, Site: "dc1"}
}

// REQ-2901's core sentence, end to end: the window could not be durably armed, so the FORWARD
// EFFECT NEVER HAPPENS. See the file header for this drill's executed killing mutation.
func TestRunnerRefusesForwardWhenCommitConfirmUnarmable(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps := testDeps(ccT0292ProposeScript()...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	fake := newCCT0292Fake()
	fake.failArm = errors.New("pg down: cannot record the armed window")
	deps.CommitConfirm = fake
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	env.ExecuteWorkflow(RunnerWorkflow, ccT0292Envelope())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("the refusal is a clean terminal, not a workflow error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Outcome != "refused:commit-confirm-unarmable — forward effect withheld (REQ-2901 fail closed)" {
		t.Fatalf("the session must terminate as the commit-confirm refusal, got outcome %q", res.Outcome)
	}
	if act.execs != 0 {
		t.Fatalf("REQ-2901 fail closed: the effect ran %d time(s) with NO durably armed revert", act.execs)
	}
	if res.Mutated {
		t.Fatal("a refused forward must not report a mutation")
	}
}

// The armed-then-refused path: mutation OFF, the chain refuses INSIDE ExecuteActivity — the window
// stands down as `aborted`, and the arm row exists even though the effect never did (armed BEFORE).
func TestRunnerAbortsWindowWhenChainRefuses(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(ccT0292ProposeScript()...)
	fake := newCCT0292Fake()
	deps.CommitConfirm = fake
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	env.ExecuteWorkflow(RunnerWorkflow, ccT0292Envelope())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.Mutated {
		t.Fatal("precondition broken: mutation OFF must mean no execution")
	}
	arms, resolves := fake.snapshot()
	if len(arms) != 1 {
		t.Fatalf("the window must have been durably armed BEFORE the (refused) effect, got %d arm(s)", len(arms))
	}
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmAborted {
		t.Fatalf("a refused chain must stand the window down as aborted, got %+v", resolves)
	}
}

// The T-029-3 headline, end to end: an EXECUTED effect whose terminus records match+verified is
// CONFIRMED — from the durable record, via the parent's nudge (fast path; the mock clock would
// otherwise deliver the same answer at elapse). Order is proven from the activity-start history:
// arm BEFORE execute (REQ-2901), and the consult only after.
func TestRunnerExecutedEffectConfirmsFromTerminus(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps := testDeps(ccT0292ProposeScript()...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	fake := newCCT0292Fake()
	deps.CommitConfirm = fake
	execs := newCCT0293FakeExecs()
	execs.set(db.ForwardExecution{Verdict: "match"}) // the terminus: executed, verified, match
	deps.Executions = execs
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	var order []string
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		order = append(order, info.ActivityType.Name)
	})

	env.ExecuteWorkflow(RunnerWorkflow, ccT0292Envelope())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete: %v", env.GetWorkflowError())
	}
	if act.execs != 1 {
		t.Fatalf("precondition: the effect must actually have executed (execs=%d)", act.execs)
	}
	armAt, execAt := -1, -1
	for i, name := range order {
		if name == "ArmCommitConfirmActivity" && armAt == -1 {
			armAt = i
		}
		if name == "ExecuteActivity" && execAt == -1 {
			execAt = i
		}
	}
	if armAt == -1 || execAt == -1 || armAt >= execAt {
		t.Fatalf("REQ-2901 order violated: arm must start BEFORE execute (arm@%d execute@%d in %v)", armAt, execAt, order)
	}
	arms, resolves := fake.snapshot()
	if len(arms) != 1 {
		t.Fatalf("an executed eligible effect must have armed exactly once, got %d", len(arms))
	}
	if arms[0].ForwardBand == "" {
		t.Fatal("the armed row must carry the forward's live band (the inverse's authorization basis)")
	}
	if len(resolves) != 1 || resolves[0].State != db.CommitConfirmConfirmed {
		t.Fatalf("REQ-2902: match+verified at the terminus must CONFIRM the window (and nothing else may resolve it), got %+v", resolves)
	}
}

// Eligibility is the registry's, not the workflow's: a class WITHOUT commit_confirmed never arms.
func TestRunnerNotEligibleClassNeverArms(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-cc-e2e","target":"web01","op_class":"start-service","op":"start","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
	deps := testDeps(script...)
	fake := newCCT0292Fake()
	deps.CommitConfirm = fake
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	env.ExecuteWorkflow(RunnerWorkflow, ccT0292Envelope())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete: %v", env.GetWorkflowError())
	}
	arms, resolves := fake.snapshot()
	if len(arms) != 0 || len(resolves) != 0 {
		t.Fatalf("a not-eligible class must never touch the commit-confirm surface, got arms=%d resolves=%d", len(arms), len(resolves))
	}
}

// ---------------------------------------------------------------------------------------------
// T-029-4 drills: the canary mandate (REQ-2905) and confirmed-only graduation (REQ-2907)
// ---------------------------------------------------------------------------------------------

// REQ-2905: a canary/staged-postured class WITHOUT commit-confirmed eligibility may not execute.
// Both posture sources drilled; eligibility SATISFIES the mandate (the armed revert is the point);
// ordinary ineligible classes stay untouched.
//
// KILLING MUTATION (executed 2026-08-15): in ArmCommitConfirmActivity, drop the mandate block
// (return {Eligible:false} unconditionally for ineligible classes — the pre-T-029-4 shape). Both
// posture halves go red: a staged class executes with no armed revert, the pve03 shape on the
// exact runs the canary law watches most.
func TestCommitConfirmArmActivityEnforcesTheCanaryMandate(t *testing.T) {
	base := func() Deps {
		return Deps{CommitConfirm: newCCT0292Fake(), Ledger: audit.NewLedger()}
	}
	in := ccT0292ArmInput()
	in.OpClass = "start-service" // registered, NOT commit-confirmed eligible

	// Canary-pinned (host, op_class) → the mandate refuses.
	d := base()
	d.CanaryPinned = func(host, opClass string) (bool, string) {
		return host == "web01" && opClass == "start-service", "drill-pin"
	}
	res, err := NewActivities(d).ArmCommitConfirmActivity(context.Background(), in)
	if err != nil || !res.MandateRefused || res.Eligible {
		t.Fatalf("canary-pinned + ineligible must refuse via the mandate, got %+v err=%v", res, err)
	}

	// The earned-but-observed AUTO_NOTICE rung → the mandate refuses.
	d = base()
	d.LadderRungFor = func(string) LadderRung { return RungAutoNotice }
	res, err = NewActivities(d).ArmCommitConfirmActivity(context.Background(), in)
	if err != nil || !res.MandateRefused {
		t.Fatalf("AUTO_NOTICE + ineligible must refuse via the mandate, got %+v err=%v", res, err)
	}

	// RungApprove is ordinary pre-graduation polling, NOT a stage — no mandate.
	d = base()
	d.LadderRungFor = func(string) LadderRung { return RungApprove }
	res, err = NewActivities(d).ArmCommitConfirmActivity(context.Background(), in)
	if err != nil || res.MandateRefused || res.Eligible {
		t.Fatalf("RungApprove must be a plain not-eligible, got %+v err=%v", res, err)
	}

	// An ELIGIBLE class in canary posture ARMS — eligibility satisfies the mandate.
	d = base()
	d.CanaryPinned = func(string, string) (bool, string) { return true, "drill-pin" }
	res, err = NewActivities(d).ArmCommitConfirmActivity(context.Background(), ccT0292ArmInput()) // restart-service
	if err != nil || res.MandateRefused || !res.Eligible {
		t.Fatalf("an eligible class satisfies the mandate by ARMING, got %+v err=%v", res, err)
	}
}

// REQ-2907, the window-side feed: confirmed is the ONE promoting outcome; a fired or failed
// revert demotes; aborted/elapsed/held feed NOTHING; a duplicate resolve feeds nothing twice.
//
// KILLING MUTATION (executed 2026-08-15): in ResolveCommitConfirmActivity's feed switch, add
// db.CommitConfirmReverted to the clean=true arm — a FIRED REVERT then PROMOTES the class that
// just proved it needed reverting, the exact laundering REQ-2907 refuses. The reverted case
// goes red on clean=true.
func TestResolveCommitConfirmFeedsTheLadderConfirmedOnly(t *testing.T) {
	type feed struct {
		opClass string
		clean   bool
	}
	run := func(state string, preArm bool) (feeds []feed, err error) {
		fake := newCCT0292Fake()
		if preArm {
			if aerr := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
				ActionID: "act-cc-1", ExternalRef: "TG-cc-1", OpClass: "restart-service",
				TargetHost: "web01", WindowSeconds: 600}); aerr != nil {
				t.Fatal(aerr)
			}
		}
		d := Deps{CommitConfirm: fake, Ledger: audit.NewLedger(),
			RecordGraduation: func(_ context.Context, opClass, _ string, clean bool) error {
				feeds = append(feeds, feed{opClass, clean})
				return nil
			}}
		err = NewActivities(d).ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
			ActionID: "act-cc-1", ExternalRef: "TG-cc-1", State: state, Detail: "drill"})
		return feeds, err
	}

	if feeds, err := run(db.CommitConfirmConfirmed, true); err != nil || len(feeds) != 1 || !feeds[0].clean || feeds[0].opClass != "restart-service" {
		t.Fatalf("confirmed must feed clean=true with the row's op-class, got %+v err=%v", feeds, err)
	}
	if feeds, err := run(db.CommitConfirmReverted, true); err != nil || len(feeds) != 1 || feeds[0].clean {
		t.Fatalf("a fired revert must DEMOTE (clean=false), got %+v err=%v", feeds, err)
	}
	if feeds, err := run(db.CommitConfirmRevertFailed, true); err != nil || len(feeds) != 1 || feeds[0].clean {
		t.Fatalf("a failed revert must DEMOTE, got %+v err=%v", feeds, err)
	}
	for _, state := range []string{db.CommitConfirmAborted, db.CommitConfirmElapsedUnconfirmed, db.CommitConfirmHeldUnverifiable} {
		if feeds, err := run(state, true); err != nil || len(feeds) != 0 {
			t.Fatalf("%s must feed NOTHING (armed-never-counts), got %+v err=%v", state, feeds, err)
		}
	}
	// A DIFFERENT-state duplicate (the row resolved aborted by its winner) feeds nothing.
	fake := newCCT0292Fake()
	if aerr := fake.ArmCommitConfirm(context.Background(), db.CommitConfirmRow{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", OpClass: "restart-service", TargetHost: "web01", WindowSeconds: 600}); aerr != nil {
		t.Fatal(aerr)
	}
	if rerr := fake.Resolve(context.Background(), "act-cc-1", "TG-cc-1", db.CommitConfirmAborted, "winner", ""); rerr != nil {
		t.Fatal(rerr)
	}
	fake.failResolve = db.ErrCommitConfirmResolved
	feeds := []feed{}
	d := Deps{CommitConfirm: fake, Ledger: audit.NewLedger(),
		RecordGraduation: func(_ context.Context, opClass, _ string, clean bool) error {
			feeds = append(feeds, feed{opClass, clean})
			return nil
		}}
	if err := NewActivities(d).ResolveCommitConfirmActivity(context.Background(), ResolveCommitConfirmInput{
		ActionID: "act-cc-1", ExternalRef: "TG-cc-1", State: db.CommitConfirmConfirmed, Detail: "dup"}); err != nil || len(feeds) != 0 {
		t.Fatalf("a duplicate resolve must feed nothing (exactly-once), got %+v err=%v", feeds, err)
	}
}

// REQ-2907, the terminus-side WITHHOLD: an eligible class's session terminus claims NOTHING (the
// window owns the outcome); a non-eligible class keeps today's terminus credit exactly.
//
// KILLING MUTATION (executed 2026-08-15): drop the withhold (remove the opschema consult from
// recordGraduationCredit) — an eligible clean-looking session then claims at terminus AND its
// window claims at confirm: double promotion per heal, or worse, a terminus promotion for a run
// whose window later fires the revert. This drill goes red on the eligible feed appearing.
func TestTerminusWithholdsGraduationForCommitConfirmedClasses(t *testing.T) {
	feeds := []string{}
	d := Deps{RecordGraduation: func(_ context.Context, opClass, _ string, _ bool) error {
		feeds = append(feeds, opClass)
		return nil
	}}
	acts := NewActivities(d)
	session := func(opClass string) ReconcileInput {
		return ReconcileInput{ExternalRef: "TG-cc-t", OpClass: opClass, HasTerminalResult: true,
			HasVerdict: true, Verdict: safety.VerdictMatch, ConfirmedClear: true, Executed: true}
	}
	if err := acts.GraduationActivity(context.Background(), session("restart-service")); err != nil || len(feeds) != 0 {
		t.Fatalf("an ELIGIBLE class's terminus must withhold (the window owns the outcome), got feeds=%v err=%v", feeds, err)
	}
	if err := acts.GraduationActivity(context.Background(), session("start-service")); err != nil || len(feeds) != 1 || feeds[0] != "start-service" {
		t.Fatalf("a non-eligible class keeps the terminus credit, got feeds=%v err=%v", feeds, err)
	}
}

// ---------------------------------------------------------------------------------------------
// T-029-5 drills: the empty-diff no-op guard (REQ-2908)
// ---------------------------------------------------------------------------------------------

// REQ-2908 at the activity: a state-preconditioned eligible class whose target ALREADY holds the
// desired end state at ARM time refuses as a free no-op — before anything arms. Unestablished or
// unwired reads fail toward arming (the window is the protection), and the not-desired state arms.
//
// KILLING MUTATION (executed 2026-08-15): drop the REQ-2908 block from ArmCommitConfirmActivity —
// the TOCTOU drill goes red: a guest that self-recovered during the vote wait gets a fresh
// (pointless) start armed against it instead of a free no-op.
func TestCommitConfirmArmActivityRefusesTheFreeNoOp(t *testing.T) {
	armFor := func(guestRunning func(context.Context, string) (bool, string, bool)) (ArmCommitConfirmResult, *ccT0292Fake, error) {
		fake := newCCT0292Fake()
		d := Deps{CommitConfirm: fake, Ledger: audit.NewLedger()}
		if guestRunning != nil {
			d.Gate = &predict.PredictionGate{GuestRunning: guestRunning}
		}
		in := ccT0292ArmInput()
		in.OpClass = "start-guest" // eligible since T-029-3, requires not-running
		in.TargetHost = "librespeed01"
		res, err := NewActivities(d).ArmCommitConfirmActivity(context.Background(), in)
		return res, fake, err
	}

	// Already desired (guest RUNNING for a start): free no-op, nothing armed.
	res, fake, err := armFor(func(context.Context, string) (bool, string, bool) { return true, "drill: running", true })
	if err != nil || !res.NoOpRefused || res.Eligible {
		t.Fatalf("already-desired must refuse as a no-op, got %+v err=%v", res, err)
	}
	if arms, _ := fake.snapshot(); len(arms) != 0 {
		t.Fatalf("a no-op must arm NOTHING, got %d arm(s)", len(arms))
	}

	// Not desired (guest down): arms normally.
	res, fake, err = armFor(func(context.Context, string) (bool, string, bool) { return false, "drill: down", true })
	if err != nil || res.NoOpRefused || !res.Eligible {
		t.Fatalf("a genuinely-down target must ARM, got %+v err=%v", res, err)
	}
	if arms, _ := fake.snapshot(); len(arms) != 1 {
		t.Fatalf("expected the window armed, got %d", len(arms))
	}

	// Unestablished read: fail toward arming (the seal established once; the window protects).
	res, _, err = armFor(func(context.Context, string) (bool, string, bool) { return false, "drill: unknown", false })
	if err != nil || res.NoOpRefused || !res.Eligible {
		t.Fatalf("an unestablished arm-time read must not fabricate a no-op, got %+v err=%v", res, err)
	}

	// No reader wired: same direction.
	res, _, err = armFor(nil)
	if err != nil || res.NoOpRefused || !res.Eligible {
		t.Fatalf("no reader must not fabricate a no-op, got %+v err=%v", res, err)
	}
}

// The DIVERGENT-TARGET drill (round-2 review HIGH finding): Action.Target and params["guest"]
// are independently LLM-populated. The no-op read must ask about the GUEST (params-first, the
// same resolution as the seal gate and the inverse seal) — a no-op declared off the wrong
// entity's state would refuse a genuinely-needed remediation as fabricated.
//
// KILLING MUTATION (executed 2026-08-15): revert the resolution to bare in.TargetHost — the
// asked-entity assert goes red (the read queries the PVE node, not the guest), and the outcome
// flips to a fabricated no-op on the node's liveness.
func TestCommitConfirmNoOpReadsTheGuestParamNotTheTarget(t *testing.T) {
	asked := ""
	fake := newCCT0292Fake()
	d := Deps{CommitConfirm: fake, Ledger: audit.NewLedger(),
		Gate: &predict.PredictionGate{GuestRunning: func(_ context.Context, target string) (bool, string, bool) {
			asked = target
			// The GUEST is down (the heal is needed); the NODE would read running.
			if target == "librespeed01" {
				return false, "drill: guest down", true
			}
			return true, "drill: node up", true
		}}}
	in := ccT0292ArmInput()
	in.OpClass = "start-guest"
	in.TargetHost = "pve-node-01"     // the diverged Action.Target
	in.GuestParam = "librespeed01"    // the entity the precondition is about
	res, err := NewActivities(d).ArmCommitConfirmActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if asked != "librespeed01" {
		t.Fatalf("the no-op read must ask about the GUEST (params-first), asked %q", asked)
	}
	if res.NoOpRefused || !res.Eligible {
		t.Fatalf("the guest is down — the heal is needed and must ARM, got %+v", res)
	}
	// And the fallback half: no guest param → Target is the read (the single-name classes).
	asked = ""
	in.GuestParam = ""
	if _, err := NewActivities(d).ArmCommitConfirmActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if asked != "pve-node-01" {
		t.Fatalf("with no guest param the fallback is Action.Target, asked %q", asked)
	}
}

// The TOCTOU end-to-end: the seal gate sees not-running (the proposal seals), the guest recovers
// during the (mock) vote/latency window, and the ARM-time re-check catches it — the session ends
// as the refused no-op, the effect never executes, nothing arms.
func TestRunnerRefusesNoOpWhenTargetRecoversAfterSeal(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint()
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	script := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-cc-e2e","target":"librespeed01","op_class":"start-guest","op":"start","params":{"guest":"librespeed01"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}
	deps := testDeps(script...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	deps.ClearObserve = faultedUntilHealed("librespeed01", "Device-Down", act)
	// THE FLIP: not-running at the SEAL read, running by the ARM read — the guest self-recovered
	// in between (the exact race REQ-2908 closes at apply time).
	reads := 0
	deps.Gate.GuestRunning = func(context.Context, string) (bool, string, bool) {
		reads++
		if reads == 1 {
			return false, "drill: down at seal", true
		}
		return true, "drill: self-recovered before arm", true
	}
	fake := newCCT0292Fake()
	deps.CommitConfirm = fake
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	env.ExecuteWorkflow(RunnerWorkflow, ccT0292Envelope())
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("the no-op is a clean terminal: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Outcome, "refused:no-op") {
		t.Fatalf("the session must end as the refused no-op, got %q", res.Outcome)
	}
	if act.execs != 0 || res.Mutated {
		t.Fatalf("a no-op must never execute (execs=%d mutated=%v)", act.execs, res.Mutated)
	}
	if arms, resolves := fake.snapshot(); len(arms) != 0 || len(resolves) != 0 {
		t.Fatalf("a no-op must touch NO window state, got arms=%d resolves=%d", len(arms), len(resolves))
	}
	if reads < 2 {
		t.Fatalf("precondition: the flip must actually have been read twice (seal then arm), got %d read(s)", reads)
	}
}
