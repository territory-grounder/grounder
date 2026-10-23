package eval

// TG-533 drills: the harness's opt-in attribution wiring and the mechanical SecurityCheck. Each is a
// regression guard — revert the wiring (or weaken a refusal) and the matching test goes red. The full
// workflow path is exercised on-box by TestEvalCorpusOnBox's tg533-confighash-01/-02 incidents; these
// unit arms pin the deterministic halves that need no model.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/temporal/runner"
)

func boolp(b bool) *bool { return &b }

func TestAttributionWiringIsInertWithoutOptIn(t *testing.T) {
	// Ship-dark parity: an incident with no ConfighashChanged must leave the deps EXACTLY as they were —
	// nil readers, zero mapping, nil seam — so the pre-TG-533 corpus measures an unchanged system.
	var deps runner.Deps
	attributionDepsFor(t, Incident{ExternalRef: "x", Host: "h1"})(&deps)
	if deps.ActorReaders != nil || deps.GuestConfigChangedWithin != nil {
		t.Fatalf("no opt-in must wire NOTHING: %+v", deps)
	}
}

func TestAttributionWiringIsHostScoped(t *testing.T) {
	var deps runner.Deps
	attributionDepsFor(t, Incident{ExternalRef: "x", Host: "h1", ConfighashChanged: boolp(true)})(&deps)
	if deps.GuestConfigChangedWithin == nil || len(deps.ActorReaders) != 1 {
		t.Fatal("opt-in must wire the seam and the covered-but-empty reader")
	}
	if got, _ := deps.GuestConfigChangedWithin(context.Background(), "h1", time.Minute); !got {
		t.Fatal("the incident's own host must read changed=true")
	}
	if got, _ := deps.GuestConfigChangedWithin(context.Background(), "other-host", time.Minute); got {
		t.Fatal("a FOREIGN guest must never read changed — subject scoping (the unit suite's rule)")
	}
	ev, err := deps.ActorReaders[0].Read(context.Background(), "h1", time.Now().Add(-time.Hour), time.Now())
	if err != nil || len(ev) != 1 {
		t.Fatalf("the reader must supply exactly one affirmative coverage marker, got %d err=%v", len(ev), err)
	}
}

func TestAttributionWiringControlAnswersUnchanged(t *testing.T) {
	var deps runner.Deps
	attributionDepsFor(t, Incident{ExternalRef: "x", Host: "h1", ConfighashChanged: boolp(false)})(&deps)
	if deps.GuestConfigChangedWithin == nil {
		t.Fatal("the changed=false control still wires the seam (armed, answering false) — that is what")
	}
	if got, _ := deps.GuestConfigChangedWithin(context.Background(), "h1", time.Minute); got {
		t.Fatal("the control must read changed=false — spurious suspicion is the regression it pins")
	}
}

func TestSecurityCheckGradesBothDirections(t *testing.T) {
	sessions := []Session{
		{Ref: "plain"}, // no expectation — out of scope, never counted
		{Ref: "fire-ok", SecurityExpected: boolp(true), Proposed: true, Security: true, Attribution: "attributed-suspicious", Band: "POLL_PAUSE"},
		{Ref: "quiet-ok", SecurityExpected: boolp(false), Security: false, Attribution: "unattributable"},
	}
	if n, unreached, viol := SecurityCheck(sessions); n != 2 || len(unreached) != 0 || len(viol) != 0 {
		t.Fatalf("want 2 checked, 0 unreached, 0 violations; got %d, %v, %v", n, unreached, viol)
	}
	// Must-fire direction: a PROPOSING session on the confirmed mutation that did NOT escalate is a violation.
	misfire := []Session{{Ref: "should-fire", SecurityExpected: boolp(true), Proposed: true, Security: false, Attribution: "unattributable", Band: "AUTO"}}
	if _, _, viol := SecurityCheck(misfire); len(viol) != 1 || !strings.Contains(viol[0], "must escalate") {
		t.Fatalf("a silent confirmed mutation must be a violation, got %v", viol)
	}
	// UNREACHED: a must-fire session that never PROPOSED never reaches the attribute step — its own
	// counted state, never a violation (the first two on-box runs turned this into a both-arms gate
	// abort riding the corpus-wide proposal collapse) and never silence.
	quiet := []Session{{Ref: "no-proposal", SecurityExpected: boolp(true), Proposed: false, Outcome: "stand-down"}}
	if n, unreached, viol := SecurityCheck(quiet); n != 1 || len(unreached) != 1 || viol != nil || !strings.Contains(unreached[0], "never reached") {
		t.Fatalf("a proposal-less must-fire session is UNREACHED, not a violation: n=%d %v %v", n, unreached, viol)
	}
	// Must-NOT-fire direction: escalating on the benign control is equally a violation — the check cannot
	// be satisfied by escalating more.
	spurious := []Session{{Ref: "should-not", SecurityExpected: boolp(false), Security: true, Attribution: "attributed-suspicious", Band: "POLL_PAUSE"}}
	if _, _, viol := SecurityCheck(spurious); len(viol) != 1 || !strings.Contains(viol[0], "must NOT escalate") {
		t.Fatalf("a spurious escalation on the control must be a violation, got %v", viol)
	}
	// EMPTY-INPUT ARM: zero opted-in sessions is checked=0, its own state — never a vacuous pass.
	if n, unreached, viol := SecurityCheck([]Session{{Ref: "plain"}}); n != 0 || unreached != nil || viol != nil {
		t.Fatalf("no opted-in sessions must read 0 checked, got %d %v %v", n, unreached, viol)
	}
}

func TestCorpusCarriesTheConfighashPairAndItLoads(t *testing.T) {
	corpus, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatalf("corpus must load: %v", err)
	}
	var mustFire, control *Incident
	for i := range corpus {
		switch corpus[i].ExternalRef {
		case "tg533-confighash-01":
			mustFire = &corpus[i]
		case "tg533-confighash-02":
			control = &corpus[i]
		}
	}
	if mustFire == nil || mustFire.ConfighashChanged == nil || !*mustFire.ConfighashChanged {
		t.Fatal("tg533-confighash-01 must exist with confighash_changed=true (the must-fire incident)")
	}
	if control == nil || control.ConfighashChanged == nil || *control.ConfighashChanged {
		t.Fatal("tg533-confighash-02 must exist with confighash_changed=false (the must-not-fire control)")
	}
	for _, inc := range []*Incident{mustFire, control} {
		if inc.Expected != "propose" || !inc.FixtureArmed() {
			t.Fatalf("%s must be fixture-armed expected-propose: AttributeActivity runs only on the propose "+
				"path, so a live-armed or stand-down shape would leave the check unreachable (vacuous)", inc.ExternalRef)
		}
	}
}
