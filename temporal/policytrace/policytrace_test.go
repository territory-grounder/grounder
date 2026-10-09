package policytrace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// stubDecider is the in-memory PolicyDecider twin: it returns a canned decision/error and records the
// EvalInput it received, so a test can assert BOTH that the Result maps the decision through AND that the
// Request was flattened onto the engine's input faithfully.
type stubDecider struct {
	dec   policy.PolicyDecision
	err   error
	gotIn policy.EvalInput
	calls int
}

func (s *stubDecider) Decide(_ context.Context, in policy.EvalInput) (policy.PolicyDecision, error) {
	s.calls++
	s.gotIn = in
	return s.dec, s.err
}

// knownDecision builds a fully-specified PolicyDecision with distinctive, checkable field values.
func knownDecision(v policy.Verdict, approveBy []string) policy.PolicyDecision {
	audit := policy.DecisionAudit{
		NeverAutoFloor: true,
		BundleVersion:  "sha256:deadbeefcafe0001",
		MatchedRules: []policy.MatchedRule{
			{ID: "deny-cisco-write", Verdict: policy.VerdictDeny},
			{ID: "auto-restart", Verdict: policy.VerdictAuto},
		},
	}
	return policy.NewPolicyDecision(v, "deny-cisco-write", safety.BandPollPause, approveBy, policy.ModeShadow, "base reason text", audit)
}

// TestPolicyTraceActivityMapsDecisionFaithfully proves the activity projects EVERY field of a known
// PolicyDecision onto the Result, and flattens the Request onto the engine's EvalInput — the whole point of a
// FAITHFUL packet-tracer is that what it reports is what the engine decided, not a re-derivation.
func TestPolicyTraceActivityMapsDecisionFaithfully(t *testing.T) {
	stub := &stubDecider{dec: knownDecision(policy.VerdictDeny, nil)}
	acts := &Activities{Decider: stub}

	req := Request{
		OpClass: "network.acl.write", Host: "dc1fw01", Argv: "conf t",
		Groups: []string{"edge"}, DeviceClass: "cisco-asa", Territory: "nl",
		Reversible: false, Confidence: 0.91, Band: "AUTO", Mode: "Shadow",
	}
	res, err := acts.PolicyTraceActivity(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The Request → EvalInput flattening reached the engine intact.
	if stub.calls != 1 {
		t.Fatalf("decider called %d times, want exactly 1", stub.calls)
	}
	if stub.gotIn.OpClass != "network.acl.write" || stub.gotIn.Host != "dc1fw01" || stub.gotIn.Argv != "conf t" {
		t.Errorf("request op-class/host/argv did not reach the engine input: %+v", stub.gotIn)
	}
	if stub.gotIn.DeviceClass != "cisco-asa" || stub.gotIn.Territory != "nl" || len(stub.gotIn.Groups) != 1 || stub.gotIn.Confidence != 0.91 {
		t.Errorf("request dimensions did not reach the engine input faithfully: %+v", stub.gotIn)
	}
	if stub.gotIn.Band != safety.BandAuto {
		t.Errorf("Band %q must parse to BandAuto, got %v", req.Band, stub.gotIn.Band)
	}
	if stub.gotIn.Mode != policy.ModeShadow {
		t.Errorf("Mode %q must parse to ModeShadow, got %v", req.Mode, stub.gotIn.Mode)
	}

	// The PolicyDecision → Result projection carried every field.
	if res.Verdict != "deny" {
		t.Errorf("Verdict = %q, want deny", res.Verdict)
	}
	if res.MatchedRuleID != "deny-cisco-write" {
		t.Errorf("MatchedRuleID = %q, want deny-cisco-write", res.MatchedRuleID)
	}
	if res.ComposedBand != "POLL_PAUSE" {
		t.Errorf("ComposedBand = %q, want POLL_PAUSE", res.ComposedBand)
	}
	if res.Mode != "Shadow" {
		t.Errorf("Mode = %q, want Shadow", res.Mode)
	}
	if !res.NeverAutoFloor {
		t.Error("NeverAutoFloor must carry through as true")
	}
	if res.BundleVersion != "sha256:deadbeefcafe0001" {
		t.Errorf("BundleVersion = %q, want the decision's bundle", res.BundleVersion)
	}
	if len(res.MatchedRules) != 2 || res.MatchedRules[0].RuleID != "deny-cisco-write" || res.MatchedRules[0].Verdict != "deny" || res.MatchedRules[1].RuleID != "auto-restart" || res.MatchedRules[1].Verdict != "auto" {
		t.Errorf("MatchedRules did not project the full deny-overrides set: %+v", res.MatchedRules)
	}
	// The base reason must survive AND carry the honest rate-governor note.
	if !strings.Contains(res.Reason, "base reason text") {
		t.Errorf("Reason dropped the engine's own text: %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "rate-governor runtime state is NOT simulated") {
		t.Errorf("Reason must disclose the rate-governor is not simulated: %q", res.Reason)
	}
	if res.RateGovernorSimulated {
		t.Error("RateGovernorSimulated must be FALSE — the trace decider carries no rate governor")
	}
}

// TestPolicyTraceActivityVerdictIsNotHardcoded is the killing check: the Result verdict must FOLLOW the
// decision the engine returned, not a constant. A stub returning deny ⇒ deny; auto ⇒ auto; approve ⇒ approve.
// If resultFrom ever pinned a literal verdict, exactly one of these rows reddens.
func TestPolicyTraceActivityVerdictIsNotHardcoded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verdict   policy.Verdict
		approveBy []string
		want      string
	}{
		{"deny", policy.VerdictDeny, nil, "deny"},
		{"auto", policy.VerdictAuto, nil, "auto"},
		{"approve", policy.VerdictApprove, []string{"group:sre-oncall"}, "approve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acts := &Activities{Decider: &stubDecider{dec: knownDecision(tc.verdict, tc.approveBy)}}
			res, err := acts.PolicyTraceActivity(context.Background(), Request{OpClass: "x", Host: "h"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Verdict != tc.want {
				t.Errorf("Verdict = %q, want %q — the verdict must flow from the decision, not a constant", res.Verdict, tc.want)
			}
			// approve carries the matched rule's approve_by; deny/auto carry none.
			if tc.want == "approve" {
				if len(res.ApproveBy) != 1 || res.ApproveBy[0] != "group:sre-oncall" {
					t.Errorf("approve must carry approve_by principals, got %v", res.ApproveBy)
				}
			} else if len(res.ApproveBy) != 0 {
				t.Errorf("%s must carry no approve_by, got %v", tc.want, res.ApproveBy)
			}
		})
	}
}

// TestPolicyTraceActivitySurfacesDeciderError proves an evaluator error is surfaced (the grounder maps it to
// 503), never swallowed into a fabricated verdict.
func TestPolicyTraceActivitySurfacesDeciderError(t *testing.T) {
	boom := errors.New("rego evaluation error")
	acts := &Activities{Decider: &stubDecider{err: boom}}
	res, err := acts.PolicyTraceActivity(context.Background(), Request{OpClass: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the decider's error surfaced", err)
	}
	if res.Verdict != "" {
		t.Errorf("a failed trace must return an empty Result, not a fabricated verdict: %+v", res)
	}
}

// TestPolicyTraceActivityNilDeciderFailsClosed proves a mis-constructed lane returns a clean error rather
// than panicking inside a worker activity.
func TestPolicyTraceActivityNilDeciderFailsClosed(t *testing.T) {
	if _, err := (&Activities{}).PolicyTraceActivity(context.Background(), Request{OpClass: "x"}); !errors.Is(err, ErrNoDecider) {
		t.Fatalf("nil decider must return ErrNoDecider, got %v", err)
	}
}

// TestParseBandFailsClosed proves an empty/unknown band never reads as a permissive AUTO — it fails closed to
// the most-restrictive POLL_PAUSE, matching safety.Band's own zero-value contract.
func TestParseBandFailsClosed(t *testing.T) {
	for _, s := range []string{"", "  ", "garbage", "poll_pause", "POLL_PAUSE"} {
		if got := parseBand(s); got != safety.BandPollPause {
			t.Errorf("parseBand(%q) = %v, want BandPollPause (fail closed)", s, got)
		}
	}
	if parseBand("auto") != safety.BandAuto || parseBand("AUTO_NOTICE") != safety.BandAutoNotice {
		t.Error("parseBand must accept the canonical AUTO / AUTO_NOTICE spellings case-insensitively")
	}
}

// TestParseModeFailsClosed proves an empty/unknown mode never reads as an actuating one — it fails closed to
// Shadow (read-only).
func TestParseModeFailsClosed(t *testing.T) {
	for _, s := range []string{"", "nonsense"} {
		if got := parseMode(s); got != policy.ModeShadow {
			t.Errorf("parseMode(%q) = %v, want ModeShadow (fail closed)", s, got)
		}
	}
}
