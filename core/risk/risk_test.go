package risk

import (
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// autoEligible is a base input that (absent a restrictive signal) classifies AUTO.
func autoEligible() GatedInput {
	return GatedInput{
		ExternalRef: "TG-1", ActionID: "act-1", PlanHash: "plan-1", RiskLevel: "low",
		OpClass: "restart-service", Reversible: Reversible, HasPrediction: true,
	}
}

func TestClassifyAUTO(t *testing.T) {
	d := Classify(autoEligible())
	if d.Band != safety.BandAuto || !d.AutoApproved || !d.AutoResolve {
		t.Fatalf("expected AUTO+auto-resolve, got %+v", d)
	}
	if d.AutoProceedOnTimeout {
		t.Fatalf("auto_proceed_on_timeout must always be false")
	}
}

func TestClassifyAUTONotice(t *testing.T) {
	// reversible-mixed on a criticality-tier host
	in := autoEligible()
	in.Reversible = ReversibleMixed
	in.CriticalityTier = true
	d := Classify(in)
	if d.Band != safety.BandAutoNotice || !d.NotifyRequired || !d.AutoApproved {
		t.Fatalf("expected AUTO_NOTICE+notify, got %+v", d)
	}
	// wide blast-radius alone also → AUTO_NOTICE
	in2 := autoEligible()
	in2.BlastRadiusWide = true
	if d2 := Classify(in2); d2.Band != safety.BandAutoNotice || !d2.NotifyRequired {
		t.Fatalf("wide blast-radius must be AUTO_NOTICE, got %+v", d2)
	}
}

func TestClassifyPOLLPAUSE(t *testing.T) {
	cases := map[string]func(*GatedInput){
		"irreversible":     func(i *GatedInput) { i.Reversible = Irreversible },
		"never-auto op":    func(i *GatedInput) { i.OpClass = "reboot" },
		"no prediction":    func(i *GatedInput) { i.HasPrediction = false },
		"deviation":        func(i *GatedInput) { i.HasVerdict = true; i.Verdict = safety.VerdictDeviation },
		"novel incident":   func(i *GatedInput) { i.NovelIncident = true },
		"unknown op class": func(i *GatedInput) { i.OpClass = "some-unrecognized-op"; i.Reversible = Irreversible },
		"jailbreak":        func(i *GatedInput) { i.Jailbreak = true }, // a screened prompt-injection forces POLL_PAUSE
		"stateful restart": func(i *GatedInput) { i.StatefulTarget = true; i.Reversible = ReversibleMixed }, // etcd rollout-restart etc.
		"hidden destructive": func(i *GatedInput) { i.ServerDestructive = true }, // model declared restart, op is dropdb
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			in := autoEligible()
			mut(&in)
			d := Classify(in)
			if d.Band != safety.BandPollPause {
				t.Fatalf("expected POLL_PAUSE, got %s (%+v)", d.Band, d)
			}
			if d.AutoApproved || d.AutoResolve || d.AutoProceedOnTimeout {
				t.Fatalf("POLL must withhold autonomy: %+v", d)
			}
		})
	}
}

// TestSafetyCompositionFloorWins asserts an irreversible op that otherwise looks AUTO-eligible still
// polls — the mechanical floor cannot be composed away (I1).
func TestSafetyCompositionFloorWins(t *testing.T) {
	in := autoEligible() // has prediction, reversible, low-risk
	in.OpClass = "dropdb"
	in.Reversible = Reversible // even if mislabeled reversible, the floor op forces POLL
	if d := Classify(in); d.Band != safety.BandPollPause {
		t.Fatalf("never-auto floor op must POLL despite AUTO-eligible context, got %s", d.Band)
	}
}

// TestCriticalityTierNeverSilentAuto is the regression for the adversarial-review finding: a fully
// reversible action on a criticality-tier (P0) host must never be silent AUTO — REQ-001 admits AUTO
// only OFF a criticality-tier host, so a P0 host reaches at most AUTO_NOTICE.
func TestCriticalityTierNeverSilentAuto(t *testing.T) {
	in := autoEligible() // reversible, prediction-backed, low-risk, narrow blast
	in.CriticalityTier = true
	d := Classify(in)
	if d.Band == safety.BandAuto {
		t.Fatalf("a P0-host action must never be silent AUTO; got %s", d.Band)
	}
	if d.Band != safety.BandAutoNotice || !d.NotifyRequired {
		t.Fatalf("a reversible P0-host action must be AUTO_NOTICE+notify, got %+v", d)
	}
}

// TestInvalidVerdictTreatedAsDeviation is the regression for the review's robustness finding: a
// present-but-invalid verdict (not one of match/partial/deviation) is treated as a deviation → poll.
func TestInvalidVerdictTreatedAsDeviation(t *testing.T) {
	in := autoEligible()
	in.HasVerdict = true
	in.Verdict = safety.Verdict("bogus") // not a valid mechanical verdict
	if d := Classify(in); d.Band != safety.BandPollPause {
		t.Fatalf("an invalid verdict must be treated as deviation → POLL_PAUSE, got %s", d.Band)
	}
	// a valid, non-deviation verdict (match/partial) still proceeds
	in.Verdict = safety.VerdictMatch
	if d := Classify(in); d.Band != safety.BandAuto {
		t.Fatalf("a match verdict should not block AUTO, got %s", d.Band)
	}
	in.Verdict = safety.VerdictPartial
	if d := Classify(in); d.Band != safety.BandAuto {
		t.Fatalf("a partial verdict should not block AUTO (only deviation does), got %s", d.Band)
	}
}

func TestEvidenceStripping(t *testing.T) {
	in := autoEligible()
	in.SilentCognitionGuard = true
	in.AutoResolveMarked = true
	// no bound evidence → strip + poll
	if d := Classify(in); d.Band != safety.BandPollPause || d.AutoResolve {
		t.Fatalf("unbound evidence must strip AUTO-RESOLVE and poll, got %+v", d)
	}
	// one bound evidence ref → passes to AUTO
	in.Evidence = []EvidenceRef{{ToolResultID: "tr-1", Captured: true, Successful: true, RecentlyObserved: true, TargetRelevant: true}}
	if d := Classify(in); d.Band != safety.BandAuto {
		t.Fatalf("bound evidence should admit AUTO, got %s", d.Band)
	}
	// a non-captured (agent free-text) ref is not bound
	in.Evidence = []EvidenceRef{{ToolResultID: "tr-2", Captured: false, Successful: true, RecentlyObserved: true, TargetRelevant: true}}
	if d := Classify(in); d.Band != safety.BandPollPause {
		t.Fatalf("agent free-text evidence must not admit AUTO, got %s", d.Band)
	}
}

func TestClassifyAndAuditAppendsOneRow(t *testing.T) {
	l := audit.NewLedger()
	d, ra, entry, err := ClassifyAndAudit(l, autoEligible())
	if err != nil {
		t.Fatal(err)
	}
	if l.Len() != 1 {
		t.Fatalf("exactly one audit row per classification, got %d", l.Len())
	}
	if ra.Band != d.Band || entry.ActionID != "act-1" {
		t.Fatalf("audit row not consistent with decision: ra=%+v entry=%+v", ra, entry)
	}
	if ra.Band != safety.BandAuto {
		t.Fatalf("expected AUTO audit row, got %s", ra.Band)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("ledger must verify: %v", err)
	}
}

// A high-risk alert category forces POLL_PAUSE even when the action would otherwise classify AUTO
// (reversible, prediction-eligible, below threshold, low risk). Safe-direction clamp — P2-22.
func TestHighRiskCategoryForcesPoll(t *testing.T) {
	in := autoEligible() // this classifies AUTO on its own
	if Classify(in).Band != safety.BandAuto {
		t.Fatal("precondition: base input must classify AUTO")
	}
	in.HighRiskCategory = true
	d := Classify(in)
	if d.Band != safety.BandPollPause || d.AutoApproved || d.AutoResolve {
		t.Fatalf("a high-risk category must force POLL_PAUSE, got %+v", d)
	}
	if d.Signals["poll_reason"] != "high-risk-category-default" {
		t.Fatalf("expected high-risk-category poll_reason, got %q", d.Signals["poll_reason"])
	}
}

// A restart targeting a self-protected control-plane service forces POLL_PAUSE even when reversible +
// prediction-eligible (would otherwise be AUTO). P2-21 self-protected-restart guard.
func TestSelfProtectedRestartForcesPoll(t *testing.T) {
	in := autoEligible()
	in.SelfProtectedRestart = true
	d := Classify(in)
	if d.Band != safety.BandPollPause || d.AutoApproved {
		t.Fatalf("a self-protected restart must force POLL_PAUSE, got %+v", d)
	}
	if d.Signals["poll_reason"] != "self-protected-control-plane-restart" {
		t.Fatalf("expected self-protected poll_reason, got %q", d.Signals["poll_reason"])
	}
}
