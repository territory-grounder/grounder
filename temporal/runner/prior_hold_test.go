package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/holds"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// THE HOLD IS REMEMBERED (TG-667). Three facts, each pinned by a test the pre-TG-667 code fails:
//  1. a still-open STICKY hold for the incident's (host, rule) makes ClassifyActivity poll with
//     prior-hold-still-open, and the autonomous fallback approver then HOLDS — never acts;
//  2. a NON-sticky prior hold (the proposal was the problem, not the estate) changes nothing;
//  3. RecordVoteActivity writes the hold row for a hold/deny and for nothing else.

type fakeHoldRecorder struct {
	rows []holds.HoldDecision
	err  error
}

func (f *fakeHoldRecorder) Record(_ context.Context, h holds.HoldDecision) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, h)
	return nil
}

func priorHoldReturning(h holds.HoldDecision, found bool, err error) func(context.Context, string, string, time.Time) (holds.HoldDecision, bool, error) {
	return func(context.Context, string, string, time.Time) (holds.HoldDecision, bool, error) {
		return h, found, err
	}
}

// cleanAutoInput is the cleanest AUTO shape ClassifyActivity can see: reversible, predicted, graduated, not
// novel. The eligibility oracle and the ladder are left nil, which this activity reads as eligible/graduated.
func cleanAutoInput() ClassifyInput {
	return ClassifyInput{ExternalRef: "INC-2", ActionID: "a2", PlanHash: "p2", RiskLevel: "low",
		OpClass: "start-container", Op: "start", Reversible: true, Host: "app01", IncidentHost: "app01", AlertRule: "Service-up/down"}
}

func classifyWith(t *testing.T, d Deps) risk.Decision {
	t.Helper()
	d.Ledger = audit.NewLedger()
	d.AutonomousResolver = true
	// The eligibility oracle fails CLOSED when nil (no-committed-prediction polls everything); the test is
	// about the hold, so the subject is declared prediction-eligible.
	d.PredictionEligible = func(string) bool { return true }
	a := &Activities{D: d}
	dec, err := a.ClassifyActivity(context.Background(), cleanAutoInput())
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	return dec
}

func TestClassifyControlIsAutoWithoutAPriorHold(t *testing.T) {
	dec := classifyWith(t, Deps{PriorHold: priorHoldReturning(holds.HoldDecision{}, false, nil)})
	if dec.Band != safety.BandAuto {
		t.Fatalf("control: the clean input must band AUTO so the hold is the only variable; got %s %v", dec.Band, dec.Signals)
	}
}

func TestStickyPriorHoldPollsAndTheFallbackApproverHoldsAgain(t *testing.T) {
	prior := holds.HoldDecision{ExternalRef: "INC-1", Host: "app01", AlertRule: "Service-up/down",
		ReasonKey: "actor-attributed-authorized", DecidedBy: AutonomousApproverPrincipal,
		DecidedAt: time.Now().Add(-40 * time.Minute), ExpiresAt: time.Now().Add(23 * time.Hour)}
	dec := classifyWith(t, Deps{PriorHold: priorHoldReturning(prior, true, nil)})
	if dec.Band != safety.BandPollPause || dec.Signals["poll_reason"] != "prior-hold-still-open" {
		t.Fatalf("a sticky prior hold must POLL_PAUSE with prior-hold-still-open; got %s %v", dec.Band, dec.Signals)
	}
	if !strings.Contains(dec.Signals["prior_hold"], "INC-1") {
		t.Fatalf("the audit row must name the honoured decision: %v", dec.Signals)
	}
	r := autonomousResolutionOf(dec)
	if !r.Resolved || r.Approve || r.Key != "prior-hold-still-open" {
		t.Fatalf("the fallback approver must HOLD on a prior hold (it clears only the first-look inputs); got %+v", r)
	}
}

func TestNonStickyPriorHoldIsForgotten(t *testing.T) {
	prior := holds.HoldDecision{ExternalRef: "INC-1", ReasonKey: "rationale-names-a-different-host",
		DecidedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(23 * time.Hour)}
	dec := classifyWith(t, Deps{PriorHold: priorHoldReturning(prior, true, nil)})
	if dec.Band != safety.BandAuto {
		t.Fatalf("a non-sticky prior hold is about the old PROPOSAL, not the estate — it must not poll; got %s %v", dec.Band, dec.Signals)
	}
}

// A hold-store read error holds on doubt and says why — never a silent "no prior hold".
func TestPriorHoldReadErrorHoldsOnDoubt(t *testing.T) {
	dec := classifyWith(t, Deps{PriorHold: priorHoldReturning(holds.HoldDecision{}, false, errors.New("db down"))})
	if dec.Band != safety.BandPollPause || !strings.Contains(dec.Signals["prior_hold"], "read failed") {
		t.Fatalf("a read error must hold and name the failure; got %s %v", dec.Band, dec.Signals)
	}
}

func TestNoHoldStoreIsByteIdenticalToBefore(t *testing.T) {
	dec := classifyWith(t, Deps{})
	if dec.Band != safety.BandAuto {
		t.Fatalf("no PriorHold seam ⇒ the pre-TG-667 band; got %s", dec.Band)
	}
	if _, ok := dec.Signals["prior_hold"]; ok {
		t.Fatal("no prior_hold signal without a store")
	}
}

func TestRecordVoteRemembersHoldsAndDeniesOnly(t *testing.T) {
	rec := &fakeHoldRecorder{}
	a := &Activities{D: Deps{Ledger: audit.NewLedger(), Holds: rec, HoldRedetectWindow: 6 * time.Hour}}
	base := RecordVoteInput{ActionID: "a1", ExternalRef: "INC-9", Host: "app01", AlertRule: "Devices-up/down",
		OpClass: "start-guest", Why: "Held: a person stopped it", ActorAttribution: "attributed-authorized"}
	for _, decision := range []string{"human:approve", "human:timeout", "fallback:autonomous-approve:ood-novel-incident",
		"human:vote-admitted-unconfigured", "human:poll-abandoned:excess-refused-votes"} {
		in := base
		in.Decision = decision
		if _, err := a.RecordVoteActivity(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", decision, err)
		}
	}
	if len(rec.rows) != 0 {
		t.Fatalf("approvals, timeouts and bookkeeping must write NO hold; got %d rows", len(rec.rows))
	}
	hold := base
	hold.Decision, hold.Voter = "fallback:autonomous-hold:actor-attributed-authorized", AutonomousApproverPrincipal
	if _, err := a.RecordVoteActivity(context.Background(), hold); err != nil {
		t.Fatal(err)
	}
	deny := base
	deny.Decision, deny.Voter = "human:deny", "operator:alice"
	if _, err := a.RecordVoteActivity(context.Background(), deny); err != nil {
		t.Fatal(err)
	}
	if len(rec.rows) != 2 {
		t.Fatalf("want 2 hold rows (one hold, one deny), got %d", len(rec.rows))
	}
	h := rec.rows[0]
	if h.ReasonKey != "actor-attributed-authorized" || h.Host != "app01" || h.AlertRule != "Devices-up/down" || h.Why == "" ||
		h.DecidedBy != AutonomousApproverPrincipal || h.ExpiresAt.Sub(h.DecidedAt) != 6*time.Hour {
		t.Fatalf("hold row lost its content: %+v", h)
	}
	if d := rec.rows[1]; d.ReasonKey != holds.ReasonHumanDeny || d.DecidedBy != "operator:alice" {
		t.Fatalf("deny row: %+v", d)
	}
}

// The projection is best-effort: a failing hold store never fails the session that is standing down, and
// the ledger row is still appended.
func TestRecordVoteSurvivesAHoldStoreFailure(t *testing.T) {
	l := audit.NewLedger()
	a := &Activities{D: Deps{Ledger: l, Holds: &fakeHoldRecorder{err: errors.New("db down")}}}
	res, err := a.RecordVoteActivity(context.Background(), RecordVoteInput{Decision: "fallback:autonomous-hold:stateful-workload-mutation",
		ActionID: "a1", ExternalRef: "INC-3", Voter: AutonomousApproverPrincipal})
	if err != nil || !res.Recorded {
		t.Fatalf("the ledger record must still succeed: res=%+v err=%v", res, err)
	}
	if len(l.Entries()) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(l.Entries()))
	}
}

// A RE-HOLD IS NEVER RECORDED (fresh-eyes review): honouring a prior hold must not write a new sticky row with a
// fresh expiry, or a flapping alert would renew its own hold forever. The same guard keeps a hold-store read
// failure (classified prior-hold-still-open) from becoming a durable hold.
func TestReHoldIsNotRecordedAsANewHold(t *testing.T) {
	rec := &fakeHoldRecorder{}
	a := &Activities{D: Deps{Ledger: audit.NewLedger(), Holds: rec, HoldRedetectWindow: time.Hour}}
	in := RecordVoteInput{Decision: "fallback:autonomous-hold:" + holds.ReasonPriorHold, ActionID: "a1", ExternalRef: "INC-2",
		Voter: AutonomousApproverPrincipal, Host: "app01", AlertRule: "Service-up/down", Why: "Held: prior hold"}
	if _, err := a.RecordVoteActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("a re-hold must write no row (the original row's expiry governs), got %d", len(rec.rows))
	}
}

// No subject, no memory: the manual-rollback poll records a hold with no host/rule; it must not litter the list.
func TestHoldWithoutASubjectIsNotRecorded(t *testing.T) {
	rec := &fakeHoldRecorder{}
	a := &Activities{D: Deps{Ledger: audit.NewLedger(), Holds: rec, HoldRedetectWindow: time.Hour}}
	in := RecordVoteInput{Decision: "fallback:autonomous-hold:manual-rollback-irreversible", ActionID: "inv1", ExternalRef: "RB-1", Voter: AutonomousApproverPrincipal}
	if _, err := a.RecordVoteActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("a hold with no (host, rule) must not be recorded, got %d", len(rec.rows))
	}
}

// TG_HOLD_REDETECT_WINDOW=0 is OFF: nothing is remembered even with a store wired.
func TestZeroWindowRecordsNothing(t *testing.T) {
	rec := &fakeHoldRecorder{}
	a := &Activities{D: Deps{Ledger: audit.NewLedger(), Holds: rec, HoldRedetectWindow: 0}}
	in := RecordVoteInput{Decision: "fallback:autonomous-hold:stateful-workload-mutation", ActionID: "a1", ExternalRef: "INC-7",
		Voter: AutonomousApproverPrincipal, Host: "db01", AlertRule: "Service-up/down"}
	if _, err := a.RecordVoteActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("window 0 means off, got %d row(s)", len(rec.rows))
	}
}

// THE OWNER'S CARVE-OUT HAS PRECEDENCE: an authorized-test attribution is fresh positive evidence that THIS change
// is a sanctioned injection, so a remembered hold about an earlier change on the same subject does not apply.
func TestAuthorizedTestAttributionOutranksAPriorHold(t *testing.T) {
	prior := holds.HoldDecision{ExternalRef: "INC-1", Host: "app01", AlertRule: "Service-up/down", ReasonKey: "actor-attributed-authorized",
		DecidedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(23 * time.Hour)}
	d := Deps{PriorHold: priorHoldReturning(prior, true, nil), Ledger: audit.NewLedger(), AutonomousResolver: true,
		PredictionEligible: func(string) bool { return true }}
	in := cleanAutoInput()
	in.Attribution = "authorized-test"
	dec, err := (&Activities{D: d}).ClassifyActivity(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Band != safety.BandAuto {
		t.Fatalf("an authorized-test re-detection must not be held by a prior hold; got %s %v", dec.Band, dec.Signals)
	}
	in.Attribution = "unattributable"
	if dec, _ := (&Activities{D: d}).ClassifyActivity(context.Background(), in); dec.Band != safety.BandPollPause {
		t.Fatalf("control: the same re-detection without the carve-out IS held, got %s", dec.Band)
	}
}

func TestAPriorHoldStopsTheHealBeforeTheActuationChain(t *testing.T) {
	control := fallbackApproverRun(t, true, proposeCanaryReversible)
	if control.execs != 1 || !fallbackLedgerHas(control.ledger, "fallback:autonomous-approve:") || len(control.gateLedger) == 0 {
		t.Fatalf("control: with no hold the same incident must clear the chain and execute once, execs=%d gates=%d ledger=%+v", control.execs, len(control.gateLedger), control.ledger)
	}
	prior := holds.HoldDecision{ExternalRef: "INC-1", Host: "web01", AlertRule: "NginxDown", ReasonKey: "actor-attributed-authorized",
		DecidedBy: AutonomousApproverPrincipal, DecidedAt: time.Now().Add(-40 * time.Minute), ExpiresAt: time.Now().Add(23 * time.Hour)}
	held := fallbackApproverRun(t, true, proposeCanaryReversible, func(d *Deps) { d.PriorHold = priorHoldReturning(prior, true, nil) })
	if !fallbackLedgerHas(held.ledger, "fallback:autonomous-hold:prior-hold-still-open") {
		t.Fatalf("the hold must be decided for the prior-hold reason, ledger=%+v", held.ledger)
	}
	if held.execs != 0 {
		t.Fatalf("a held incident must never execute, execs=%d", held.execs)
	}
	if len(held.gateLedger) != 0 {
		t.Fatalf("the hold must act before the chain, so no gate (breaker included) can be what stopped it: %+v", held.gateLedger)
	}
}
