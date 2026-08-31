package acceptance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/screen"
)

// world is the per-scenario state. A fresh one is created for every scenario because
// InitializeScenario is invoked once per scenario.
type world struct {
	band    safety.Band
	lane    safety.FailLane
	opClass string

	// classifier (REQ-001/002/003/007/008) oracle state.
	input    risk.GatedInput
	decision risk.Decision

	// input-screen redaction (REQ-010) oracle state.
	untrusted string
	scrubbed  string
	scrubHits []screen.Match
}

// TestRiskClassificationAcceptance runs the spec/001 acceptance feature. @pending scenarios (the
// not-yet-built classifier behavior) are excluded; the executed set drives real core/safety code and
// must pass strictly. This is the objective, author-independent definition-of-done for this spec.
func TestRiskClassificationAcceptance(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "spec/001 risk-classification",
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"."},
			Tags:     "~@pending",
			Strict:   true,
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/001 acceptance scenarios failed")
	}
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	sc.Step(`^a freshly zero-initialised autonomy band$`, func() error {
		w.band = safety.Band(0)
		return nil
	})
	sc.Step(`^an out-of-range band value (\d+)$`, func(v int) error {
		w.band = safety.Band(v)
		return nil
	})
	sc.Step(`^the band is "([^"]*)"$`, func(want string) error {
		if got := w.band.String(); got != want {
			return fmt.Errorf("band = %q, want %q", got, want)
		}
		return nil
	})

	sc.Step(`^the operation class "([^"]*)"$`, func(op string) error {
		w.opClass = op
		return nil
	})
	sc.Step(`^the operation is on the never-auto floor$`, func() error {
		if !safety.IsNeverAuto(w.opClass) {
			return fmt.Errorf("operation class %q is not on the never-auto floor but must be", w.opClass)
		}
		return nil
	})
	sc.Step(`^the operation is not on the never-auto floor$`, func() error {
		if safety.IsNeverAuto(w.opClass) {
			return fmt.Errorf("operation class %q is on the never-auto floor but must not be", w.opClass)
		}
		return nil
	})

	sc.Step(`^a freshly zero-initialised fail lane$`, func() error {
		w.lane = safety.FailLane(0)
		return nil
	})
	sc.Step(`^the lane is the fail-closed remediation lane$`, func() error {
		if w.lane != safety.LaneRemediation {
			return fmt.Errorf("zero fail lane = %d, want LaneRemediation (%d)", w.lane, safety.LaneRemediation)
		}
		return nil
	})

	// --- classifier scenarios (core/risk.Classify) ---

	// An AUTO-eligible baseline: reversible, prediction-backed, not critical, below threshold.
	autoEligible := func() risk.GatedInput {
		return risk.GatedInput{
			ExternalRef: "TG-1", ActionID: "act-1", PlanHash: "plan-1", RiskLevel: "low",
			OpClass: "restart-service", Reversible: risk.Reversible, HasPrediction: true,
		}
	}

	sc.Step(`^a low-risk reversible action below the org blast-radius threshold on a non-critical host$`, func() error {
		w.input = autoEligible()
		return nil
	})
	sc.Step(`^a reversible-mixed action on a criticality-tier host$`, func() error {
		in := autoEligible()
		in.Reversible = risk.ReversibleMixed
		in.CriticalityTier = true
		w.input = in
		return nil
	})
	sc.Step(`^a high-risk irreversible action$`, func() error {
		in := autoEligible()
		in.RiskLevel = "high"
		in.Reversible = risk.Irreversible
		in.OpClass = "reboot"
		w.input = in
		return nil
	})
	sc.Step(`^an incident class with no learned prior for its alert rule and host$`, func() error {
		in := autoEligible()
		in.HasPrediction = false
		w.input = in
		return nil
	})
	sc.Step(`^the silent_cognition_guard policy is active$`, func() error {
		in := autoEligible()
		in.SilentCognitionGuard = true
		w.input = in
		return nil
	})
	sc.Step(`^an AUTO-RESOLVE response with no bound ToolResult evidence$`, func() error {
		w.input.AutoResolveMarked = true
		w.input.Evidence = nil // a bare marker with no orchestrator-captured evidence
		return nil
	})

	sc.Step(`^a canary-pinned action that would otherwise be AUTO-eligible$`, func() error {
		in := autoEligible()
		in.CanaryPinned = true
		w.input = in
		return nil
	})

	// REQ-015 — the prior-verdict band. The fixtures set the input the way the REAL classify path sets it: a
	// found verdict carries both the verdict and the (host, canonical family) signature it was read under; an
	// absent one carries neither, which is byte-identical to the pre-feature input.
	sc.Step(`^an otherwise-AUTO-eligible action whose target carries a recent same-rule-family deviation verdict$`, func() error {
		in := autoEligible()
		in.HasVerdict = true
		in.Verdict = safety.VerdictDeviation
		in.VerdictKey = "web01|device-down"
		w.input = in
		return nil
	})
	sc.Step(`^an otherwise-AUTO-eligible action whose target carries no prior verdict$`, func() error {
		w.input = autoEligible() // HasVerdict stays false — absent evidence is the pre-feature input
		return nil
	})
	sc.Step(`^an otherwise-AUTO-eligible action whose target carries a recent match verdict$`, func() error {
		in := autoEligible()
		in.HasVerdict = true
		in.Verdict = safety.VerdictMatch
		in.VerdictKey = "web01|device-down"
		w.input = in
		return nil
	})
	sc.Step(`^the decision records the prior-verdict signature it read$`, func() error {
		if got := w.decision.Signals["poll_reason"]; got != "verdict-deviation-or-invalid" {
			return fmt.Errorf("poll_reason = %q, want verdict-deviation-or-invalid", got)
		}
		if got := w.decision.Signals["prior_verdict_key"]; got != "web01|device-down" {
			return fmt.Errorf("prior_verdict_key = %q, want the (host, family) signature the verdict was read under", got)
		}
		if got := w.decision.Signals["prior_verdict"]; got != string(safety.VerdictDeviation) {
			return fmt.Errorf("prior_verdict = %q, want deviation", got)
		}
		return nil
	})
	sc.Step(`^the decision records no prior-verdict signature$`, func() error {
		if got, ok := w.decision.Signals["prior_verdict_key"]; ok {
			return fmt.Errorf("a decision this rule did not drive must carry no prior-verdict evidence, got %q", got)
		}
		return nil
	})

	sc.Step(`^the classifier admits the session$`, func() error {
		w.decision = risk.Classify(w.input)
		w.band = w.decision.Band // let the shared `the band is "X"` step read it
		return nil
	})

	sc.Step(`^the band is "AUTO" and the proposal is marked AUTO-RESOLVE$`, func() error {
		if w.decision.Band != safety.BandAuto {
			return fmt.Errorf("band = %s, want AUTO", w.decision.Band)
		}
		if !w.decision.AutoResolve {
			return fmt.Errorf("proposal must be marked AUTO-RESOLVE")
		}
		return nil
	})
	sc.Step(`^the band is "AUTO_NOTICE" and notify_required is true$`, func() error {
		if w.decision.Band != safety.BandAutoNotice {
			return fmt.Errorf("band = %s, want AUTO_NOTICE", w.decision.Band)
		}
		if !w.decision.NotifyRequired {
			return fmt.Errorf("notify_required must be true for AUTO_NOTICE")
		}
		return nil
	})
	sc.Step(`^the band is "POLL_PAUSE" and the session does not proceed on timeout$`, func() error {
		if w.decision.Band != safety.BandPollPause {
			return fmt.Errorf("band = %s, want POLL_PAUSE", w.decision.Band)
		}
		if w.decision.AutoProceedOnTimeout {
			return fmt.Errorf("a poll must never proceed on timeout")
		}
		return nil
	})
	sc.Step(`^the AUTO-RESOLVE marker is stripped and the session is downgraded to a poll$`, func() error {
		if w.decision.Band != safety.BandPollPause {
			return fmt.Errorf("session must be downgraded to POLL_PAUSE, got %s", w.decision.Band)
		}
		if w.decision.AutoResolve {
			return fmt.Errorf("the AUTO-RESOLVE marker must be stripped")
		}
		return nil
	})

	// --- input-screen secret/PII redaction (REQ-010, core/screen.Scrub) ---

	// The bearer token is a CONSTRUCTED fake shape (no literal real credential).
	bearerCred := "Bearer " + strings.Repeat("A", 22)

	sc.Step(`^untrusted alert text carrying a leaked bearer credential$`, func() error {
		w.untrusted = "Web check failed on web01. Authorization: " + bearerCred + " while polling upstream."
		return nil
	})
	sc.Step(`^untrusted alert text with only hostnames, IPs, rule names and numbers$`, func() error {
		w.untrusted = "BFD session nl-frr01 is down on 192.0.2.1; rule bgp-neighbor-down; disk at 92%."
		return nil
	})
	sc.Step(`^the input screen scrubs the text$`, func() error {
		w.scrubbed, w.scrubHits = screen.Scrub(w.untrusted)
		return nil
	})
	sc.Step(`^the credential is redacted and the redaction is flagged$`, func() error {
		if strings.Contains(w.scrubbed, strings.Repeat("A", 22)) {
			return fmt.Errorf("the credential must be redacted out of the emitted text: %q", w.scrubbed)
		}
		if !strings.Contains(w.scrubbed, screen.RedactMarker(screen.SecretBearer)) {
			return fmt.Errorf("the [REDACTED:bearer-token] marker must be present: %q", w.scrubbed)
		}
		flagged := false
		for _, m := range w.scrubHits {
			if m.Category == screen.CategorySecretRedaction {
				flagged = true
			}
		}
		if !flagged {
			return fmt.Errorf("the redaction must be flagged so the caller emits the scrubbed text, got %v", w.scrubHits)
		}
		return nil
	})
	sc.Step(`^the text passes through unchanged with no redaction$`, func() error {
		if w.scrubbed != w.untrusted {
			return fmt.Errorf("a benign body must not be mangled: %q -> %q", w.untrusted, w.scrubbed)
		}
		for _, m := range w.scrubHits {
			if m.Category == screen.CategorySecretRedaction {
				return fmt.Errorf("a benign body must not be redacted, got %v", w.scrubHits)
			}
		}
		return nil
	})
}
