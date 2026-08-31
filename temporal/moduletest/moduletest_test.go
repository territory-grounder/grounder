package moduletest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeProber struct {
	summary, detail string
	err             error
	n               int
	sawOperator     string
}

func (f *fakeProber) Probe(_ context.Context, req Request) (string, string, error) {
	f.n++
	f.sawOperator = req.Operator
	return f.summary, f.detail, f.err
}

func acts(p Prober) *Activities {
	return &Activities{D: Deps{Probers: map[string]Prober{"notifier/matrix": p}}}
}

// A FAILED TEST IS A RESULT, NOT AN ERROR. Returning an activity error would make Temporal retry — which
// for a notifier means posting the same test message repeatedly into an operator's room while they watch
// a spinner.
//
// KILLING MUTATION: return the probe's error from TestModuleActivity. RED.
func TestAFailedProbeIsAResultNotAnActivityError(t *testing.T) {
	p := &fakeProber{err: errors.New("403 not in room"), summary: "could not post", detail: "the bot is not a member of that room"}
	res, err := acts(p).TestModuleActivity(context.Background(), Request{Surface: "notifier", SourceType: "matrix"})
	if err != nil {
		t.Fatalf("a failed probe surfaced as an activity error (Temporal would retry it): %v", err)
	}
	if res.OK {
		t.Fatal("a failed probe reported OK")
	}
	// The detail must be ACTIONABLE. "error" tells an operator nothing.
	if !strings.Contains(res.Detail, "not a member") {
		t.Fatalf("the failure is not actionable: %q", res.Detail)
	}
}

// A module with no probe must SAY so. Reporting a pass would certify something nobody checked — the exact
// shape of every defect this codebase has spent two days removing.
//
// KILLING MUTATION: make the missing-prober branch return OK: true. RED.
func TestAModuleWithNoProbeDoesNotReportAPass(t *testing.T) {
	res, err := (&Activities{D: Deps{}}).TestModuleActivity(context.Background(),
		Request{Surface: "tracker", SourceType: "jira"})
	if err != nil {
		t.Fatalf("unexpected activity error: %v", err)
	}
	if res.OK {
		t.Fatal("a module with NO probe reported a passing test — it certifies something nobody checked")
	}
	if res.Detail == "" {
		t.Fatal("the operator is not told why there is no result")
	}
}

// A passing probe reports OK and times the real call. A test that passes in 9 seconds is itself a finding.
func TestAPassingProbeReportsOKAndTiming(t *testing.T) {
	p := &fakeProber{summary: "posted to !room:example"}
	res, _ := acts(p).TestModuleActivity(context.Background(),
		Request{Surface: "notifier", SourceType: "matrix", Operator: "@ops:example"})
	if !res.OK || res.Summary != "posted to !room:example" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if p.n != 1 {
		t.Fatalf("the probe ran %d times, want exactly 1 — a probe with a visible side effect must not "+
			"be repeated", p.n)
	}
	if p.sawOperator != "@ops:example" {
		t.Errorf("the probe was not told who triggered it: %q", p.sawOperator)
	}
	if res.ElapsedMS < 0 {
		t.Error("elapsed time was not recorded")
	}
}

// THE MARKER IS A SAFETY PROPERTY. A probe message that reads like a governance decision, arriving because
// someone opened a settings dialog, is an operator acting on a decision TG never made.
//
// KILLING MUTATION: drop TestBodyMarker from ProbeBody. RED.
func TestProbeBodyCannotBeMistakenForAGovernanceDecision(t *testing.T) {
	body := ProbeBody("@ops:example")
	if !strings.Contains(body, TestBodyMarker) {
		t.Fatalf("the probe body carries no test marker — a human reading the room could act on it:\n%s", body)
	}
	for _, forbidden := range []string{"approve ", "deny ", "POLL_PAUSE", "proposed:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the probe body contains %q, which reads as a real approval request", forbidden)
		}
	}
	if !strings.Contains(body, "@ops:example") {
		t.Error("the room cannot see who triggered the test")
	}
	// An unattributed test must still be clearly a test.
	if !strings.Contains(ProbeBody(""), TestBodyMarker) {
		t.Error("an unattributed probe lost its marker")
	}
}

// One attempt only: the retry policy is part of the safety argument, not a tuning detail.
func TestProbeIsNotRetried(t *testing.T) {
	if got := activityOpts().RetryPolicy.MaximumAttempts; got != 1 {
		t.Fatalf("MaximumAttempts = %d — a retried probe posts the test message again, and three "+
			"identical probes in a room look like a malfunction", got)
	}
}
