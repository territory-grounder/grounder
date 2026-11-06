package policy

// Oracle tests for spec/015 T-015-11: audit-every-decision (REQ-1518), the warn-don't-block guard (REQ-1517),
// and the admin engine toggle (REQ-1519). Pure-Go, no DB, -race clean. The load-bearing properties proven here:
//   - every Decide (auto / approve / deny / engine-off) emits EXACTLY ONE audit record — no decision path is
//     un-audited (the negative control);
//   - a sink append failure does NOT change the decision but IS traced (never silently swallowed);
//   - WarnFor flags allow-all / removed-floor / Full-auto with a NON-BLOCKING warning and never converts it to
//     a block;
//   - the admin toggle requires authority (unauthenticated → refused) and emits an audit record;
//   - the engine DISABLED still fails closed — a never-auto action never resolves to auto, and an otherwise-auto
//     action is routed to approve (engine-off is not a bypass of the never-auto floor / mode chokepoint).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/safety"
)

// ---- test seams ------------------------------------------------------------------------------------------

// fakeAuthz admits or rejects an actor (the AuthorityChecker seam mode.go uses, reused by the toggle).
type fakeAuthz struct{ admit bool }

func (a fakeAuthz) HasModeChangeAuthority(context.Context, string) error {
	if a.admit {
		return nil
	}
	return errors.New("not an admin")
}

// logCapture is a concurrency-safe logf sink for asserting a governance-audit gap was traced (never silent).
type logCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
}

func (l *logCapture) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.msgs, "\n")
}

// hostGlobSel builds a wildcard host-glob selector (the allow-all shape).
func hostGlobSel(pat string) *credential.Selector {
	s := credential.Selector{Kind: credential.KindHostGlob, Pattern: pat}
	return &s
}

func exactHostSel(host string) *credential.Selector {
	s := credential.Selector{Kind: credential.KindHost, Pattern: host}
	return &s
}

// engineWith builds an engine over a single operator rule (no injected pieces — nil-safe pass-throughs).
func engineWith(t *testing.T, r Rule) *Engine {
	t.Helper()
	rule, err := NewRule(r)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	e, err := NewEngine(context.Background(), RuleSet{Rules: []Rule{rule}})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// reversibleAutoIn is a reversible, high-confidence, AUTO-band action an auto rule resolves to auto.
func reversibleAutoIn(host string) EvalInput {
	return EvalInput{Host: host, OpClass: "service-restart", Reversible: true, Confidence: 0.95, Band: safety.BandAuto, Mode: ModeFullAuto}
}

// ---- audit-every-decision (REQ-1518) ---------------------------------------------------------------------

func TestAuditedEngine_EmitsExactlyOneRecordPerVerdict(t *testing.T) {
	cases := []struct {
		name    string
		verdict Verdict
		want    Verdict
	}{
		{"auto", VerdictAuto, VerdictAuto},
		{"approve", VerdictApprove, VerdictApprove},
		{"deny", VerdictDeny, VerdictDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := engineWith(t, Rule{ID: "r", Match: Match{Selector: exactHostSel("h1")}, Verdict: tc.verdict})
			sink := NewMemAuditSink()
			ae := NewAuditedEngine(e, sink)

			dec, err := ae.Decide(context.Background(), reversibleAutoIn("h1"))
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if dec.Verdict() != tc.want {
				t.Fatalf("verdict = %q, want %q", dec.Verdict(), tc.want)
			}
			if sink.Len() != 1 {
				t.Fatalf("sink recorded %d, want exactly one audit record", sink.Len())
			}
			if got := sink.Records()[0].Verdict(); got != tc.want {
				t.Fatalf("recorded verdict = %q, want %q", got, tc.want)
			}
		})
	}
}

// A sink append failure must NOT change the decision but MUST be traced (never silently swallowed).
func TestAuditedEngine_SinkFailureDoesNotChangeDecisionButIsTraced(t *testing.T) {
	e := engineWith(t, Rule{ID: "r", Match: Match{Selector: exactHostSel("h1")}, Verdict: VerdictAuto})
	sink := NewMemAuditSink().WithAppendError(errors.New("db down"))
	lc := &logCapture{}
	ae := NewAuditedEngine(e, sink).WithLogf(lc.logf)

	dec, err := ae.Decide(context.Background(), reversibleAutoIn("h1"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Verdict() != VerdictAuto {
		t.Fatalf("verdict = %q, want auto (the sink failure must not change the decision)", dec.Verdict())
	}
	if sink.Len() != 1 {
		t.Fatalf("decision was not offered to the sink (len=%d)", sink.Len())
	}
	if !strings.Contains(lc.joined(), "audit append FAILED") {
		t.Fatalf("sink failure was not traced; logs = %q", lc.joined())
	}
}

// Negative control: a decision with NO sink is not silently un-audited — it is traced.
func TestAuditedEngine_MissingSinkIsTraced(t *testing.T) {
	e := engineWith(t, Rule{ID: "r", Match: Match{Selector: exactHostSel("h1")}, Verdict: VerdictApprove})
	lc := &logCapture{}
	ae := NewAuditedEngine(e, nil).WithLogf(lc.logf)

	if _, err := ae.Decide(context.Background(), reversibleAutoIn("h1")); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !strings.Contains(lc.joined(), "NO audit sink") {
		t.Fatalf("missing sink was not traced; logs = %q", lc.joined())
	}
}

// The decision reaches the REAL tamper-evident governance ledger through the adapter, and re-walks clean.
func TestLedgerAuditSink_AppendsToGovernanceLedger(t *testing.T) {
	e := engineWith(t, Rule{ID: "r", Match: Match{Selector: exactHostSel("h1")}, Verdict: VerdictAuto})
	led := audit.NewLedger()
	ae := NewAuditedEngine(e, NewLedgerAuditSink(led))

	if _, err := ae.Decide(context.Background(), reversibleAutoIn("h1")); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if led.Len() != 1 {
		t.Fatalf("ledger has %d rows, want exactly one policy-decision record", led.Len())
	}
	if got := led.Entries()[0].Decision; got != "policy:auto" {
		t.Fatalf("ledger decision = %q, want policy:auto", got)
	}
	if led.Entries()[0].Withheld {
		t.Fatalf("an auto decision must not be recorded as withheld")
	}
	if err := led.Verify(); err != nil {
		t.Fatalf("governance ledger does not re-walk clean: %v", err)
	}
}

// ---- warn-don't-block guard (REQ-1517) -------------------------------------------------------------------

func TestWarnFor_FlagsAllowAllRemovedFloorAndFullAuto_NeverBlocks(t *testing.T) {
	// An allow-all rule (wildcard host-glob) + the conservative floor with one entry removed-with-warning.
	rs := RuleSet{Rules: []Rule{{ID: "bare-allow-all", Match: Match{Selector: hostGlobSel("*")}, Verdict: VerdictAuto}}}

	fl, err := NewFloor(FloorEntry{ID: "deny-mkfs", Match: Match{ArgvPattern: "mkfs"}})
	if err != nil {
		t.Fatalf("NewFloor: %v", err)
	}
	if _, err := fl.RemoveFloorEntry("deny-mkfs", "operator:alice", Warning{Text: "I own the dial", Acknowledged: true, DoubleConfirm: true}); err != nil {
		t.Fatalf("RemoveFloorEntry: %v", err)
	}

	warns := WarnFor(rs, fl, ModeFullAuto)
	got := map[WarnCode]bool{}
	for _, w := range warns {
		got[w.Code] = true
	}
	for _, want := range []WarnCode{WarnAllowAllRule, WarnFloorRemoved, WarnFullAuto} {
		if !got[want] {
			t.Fatalf("WarnFor did not surface %q; got %v", want, warns)
		}
	}

	// Warn-don't-block: WarnFor returns warnings, never an error and never a mutation — the posture still stands.
	if len(warns) < 3 {
		t.Fatalf("expected at least three warnings, got %d", len(warns))
	}
	// The allow-all rule is STILL in the rule set (WarnFor did not remove/refuse it).
	if len(rs.Rules) != 1 {
		t.Fatalf("WarnFor mutated the rule set (len=%d) — it must be a pure read", len(rs.Rules))
	}
}

// A deny rule is a protection, not a permissive risk — WarnFor must not warn about it.
func TestWarnFor_DenyAndNarrowRulesAreNotWarned(t *testing.T) {
	rs := RuleSet{Rules: []Rule{
		{ID: "wildcard-deny", Match: Match{Selector: hostGlobSel("*")}, Verdict: VerdictDeny},
		{ID: "narrow-auto", Match: Match{Selector: exactHostSel("h1")}, Verdict: VerdictAuto},
	}}
	if w := WarnFor(rs, nil, ModeSemiAuto); len(w) != 0 {
		t.Fatalf("expected no warnings for a deny + a narrow auto in Semi-auto, got %v", w)
	}
}

// ---- admin engine toggle (REQ-1519) ----------------------------------------------------------------------

func TestEngineToggle_RequiresAuthority(t *testing.T) {
	led := audit.NewLedger()
	tog := NewEngineToggle(fakeAuthz{admit: false}, led)
	_, err := tog.Override(context.Background(), "mallory", true, ModeShadow, Warning{Text: "force on", Acknowledged: true})
	if !errors.Is(err, ErrUnauthorizedEngineToggle) {
		t.Fatalf("unauthenticated toggle err = %v, want ErrUnauthorizedEngineToggle", err)
	}
	if led.Len() != 0 {
		t.Fatalf("an unauthorized toggle appended %d ledger rows, want zero", led.Len())
	}
}

func TestEngineToggle_ForceOnInShadowWarns_DisableInSemiDoubleWarns(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0) }

	// Forcing the engine ON in Shadow (a read-only mode) records a single warning.
	led := audit.NewLedger()
	tog := NewEngineToggle(fakeAuthz{admit: true}, led).WithClock(clock)
	rec, err := tog.Override(context.Background(), "admin:root", true, ModeShadow, Warning{Text: "run engine in shadow", Acknowledged: true})
	if err != nil {
		t.Fatalf("force-on override: %v", err)
	}
	if !rec.ForcedOn || rec.Warning.Code != WarnEngineForcedOn {
		t.Fatalf("force-on record = %+v, want ForcedOn + WarnEngineForcedOn", rec)
	}
	if !tog.Effective(ModeShadow) {
		t.Fatalf("engine should be forced ON in Shadow after the override")
	}

	// Disabling the engine in Semi-auto (an actuating mode) REQUIRES a red double-confirmation.
	if _, err := tog.Override(context.Background(), "admin:root", false, ModeSemiAuto, Warning{Text: "disable", Acknowledged: true}); !errors.Is(err, ErrEngineToggleNotConfirmed) {
		t.Fatalf("disable without double-confirm err = %v, want ErrEngineToggleNotConfirmed", err)
	}
	// WITH the double-confirmation the disable is permitted (warn-don't-block) and double-warned.
	rec2, err := tog.Override(context.Background(), "admin:root", false, ModeSemiAuto, Warning{Text: "disable engine", Acknowledged: true, DoubleConfirm: true})
	if err != nil {
		t.Fatalf("disable override: %v", err)
	}
	if !rec2.Disabled || !rec2.DoubleConfirm || rec2.Warning.Code != WarnEngineDisabled {
		t.Fatalf("disable record = %+v, want Disabled + DoubleConfirm + WarnEngineDisabled", rec2)
	}
	if tog.Effective(ModeSemiAuto) {
		t.Fatalf("engine should be DISABLED in Semi-auto after the override")
	}

	// Every successful toggle emitted a governance-ledger record, and the chain re-walks clean.
	if led.Len() != 2 {
		t.Fatalf("ledger has %d toggle records, want 2 (force-on + disable)", led.Len())
	}
	if err := led.Verify(); err != nil {
		t.Fatalf("toggle ledger does not re-walk clean: %v", err)
	}
}

// ---- engine-off is NOT a bypass of the never-auto floor / mode chokepoint (REQ-1517/1519, INV-09) ---------

func TestAuditedEngine_DisabledDefersToFloor_NeverBypassesNeverAuto(t *testing.T) {
	led := audit.NewLedger()
	tog := NewEngineToggle(fakeAuthz{admit: true}, led)
	// Disable the engine in Full-auto (double-confirmed) — the most permissive actuating posture.
	if _, err := tog.Override(context.Background(), "admin:root", false, ModeFullAuto, Warning{Text: "kill switch", Acknowledged: true, DoubleConfirm: true}); err != nil {
		t.Fatalf("disable override: %v", err)
	}

	// An engine that, ENABLED, WOULD resolve this reversible action to auto (allow-all auto rule, Full-auto).
	e := engineWith(t, Rule{ID: "allow", Match: Match{Selector: hostGlobSel("*")}, Verdict: VerdictAuto})
	sink := NewMemAuditSink()
	ae := NewAuditedEngine(e, sink).WithToggle(tog)

	// (a) An otherwise-auto action must route to approve while the engine is disabled — never auto.
	dec, err := ae.Decide(context.Background(), reversibleAutoIn("h1"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Verdict() == VerdictAuto {
		t.Fatalf("engine-off resolved to AUTO — disabling the engine must never open a path to auto")
	}
	if dec.Verdict() != VerdictApprove {
		t.Fatalf("engine-off verdict = %q, want approve (route to a human)", dec.Verdict())
	}

	// (b) A never-auto action (reboot) with the engine disabled — still not auto; the floor is recorded beneath.
	neverAuto := EvalInput{Host: "h1", OpClass: "reboot", Reversible: false, Confidence: 0.99, Band: safety.BandAuto, Mode: ModeFullAuto}
	decNA, err := ae.Decide(context.Background(), neverAuto)
	if err != nil {
		t.Fatalf("Decide (never-auto): %v", err)
	}
	if decNA.Verdict() == VerdictAuto {
		t.Fatalf("a never-auto action resolved to AUTO with the engine disabled — floor bypass")
	}
	if !decNA.Audit().NeverAutoFloor {
		t.Fatalf("engine-off decision did not record the never-auto floor as applying — it must prove the floor is beneath")
	}
	// Sanity: an ENABLED engine also never autos a never-auto action (the floor is beneath the engine too).
	enabledDec, err := e.Decide(context.Background(), neverAuto)
	if err != nil {
		t.Fatalf("enabled Decide (never-auto): %v", err)
	}
	if enabledDec.Verdict() == VerdictAuto {
		t.Fatalf("enabled engine autoed a never-auto action — constitutional floor breached")
	}

	// Both engine-off decisions were audited (audit-every-decision holds on the engine-off path too).
	if sink.Len() != 2 {
		t.Fatalf("engine-off decisions audited = %d, want 2", sink.Len())
	}
}
