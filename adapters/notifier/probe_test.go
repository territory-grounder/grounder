package notifier

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/selftest"
)

type probeSink struct {
	got Notice
	err error
	n   int
}

func (p *probeSink) SourceType() string { return "fake" }
func (p *probeSink) Notify(_ context.Context, n Notice) error {
	p.n++
	p.got = n
	return p.err
}
func (p *probeSink) ResolveVote(context.Context, []byte) (Vote, error) {
	return Vote{}, errors.New("no")
}

// The probe posts a REAL message through the REAL sink — that is the whole point. And it is never a poll:
// a ballot nobody should answer is worse than a message.
//
// KILLING MUTATION: set Approval: true in the probe notice. RED.
func TestProbePostsARealNonPollMessage(t *testing.T) {
	s := &probeSink{}
	res, err := ProbeDelivery(context.Background(), s, "@ops:example", "!room:example")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if s.n != 1 {
		t.Fatalf("the real sink was called %d times, want 1", s.n)
	}
	if s.got.Approval {
		t.Fatal("the probe posted an APPROVAL POLL — a ballot arriving from a settings dialog that " +
			"nobody should answer is worse than a plain message")
	}
	if !strings.Contains(s.got.Body, selftest.BodyMarker) {
		t.Fatalf("the posted message is not marked as a test: %q", s.got.Body)
	}
	// The operator who pressed TEST must be named: a marked message with no author is an unexplained
	// event in a room people watch during incidents.
	if !strings.Contains(s.got.Body, "@ops:example") {
		t.Errorf("the probe message does not name who triggered it: %q", s.got.Body)
	}
	// A probe must never reuse a live decision id: its poll could be answered, or answered BY the probe.
	if s.got.DecisionID != "tg-config-test" {
		t.Errorf("the probe used decision id %q — it must not collide with a real decision", s.got.DecisionID)
	}
	if !strings.Contains(res.Summary, "!room:example") {
		t.Errorf("the operator is not told where it went: %q", res.Summary)
	}
}

// THE PROBE MUST ACTUALLY SEND. This is the oracle that separates a probe from a mock: a credential- or
// config-shaped check would report success without ever calling the sink.
//
// KILLING MUTATION: replace the ProbeDelivery body with `return selftest.Result{Summary: "ok"}, nil`. RED
// on both assertions — the sink is never called, and a failing sink reports success.
func TestProbeIsNotAConfigurationCheck(t *testing.T) {
	s := &probeSink{err: errors.New("dial tcp: connection refused")}
	res, err := ProbeDelivery(context.Background(), s, "@ops:example", "!room:example")
	if s.n != 1 {
		t.Fatalf("the sink was called %d times — a probe that does not send proves nothing about delivery", s.n)
	}
	if err == nil {
		t.Fatal("a probe against an unreachable server reported SUCCESS — this is a mock wearing a " +
			"test's name: it would pass against a revoked token and a server down for a week")
	}
	if !strings.Contains(res.Detail, "could not be reached") {
		t.Errorf("the failure detail is not actionable: %q", res.Detail)
	}
}

// THE FAILURE MUST BE ACTIONABLE. "error" tells an operator nothing; the failures they actually hit each
// need a different fix, and the probe must say which one happened.
//
// KILLING MUTATION: make ClassifyDeliveryFailure return err.Error() unconditionally. RED.
func TestFailuresAreClassifiedIntoSomethingActionable(t *testing.T) {
	for _, tc := range []struct{ err, want string }{
		{"matrix: status 401 M_UNKNOWN_TOKEN", "revoked"},
		{"matrix: status 403 M_FORBIDDEN", "not a member"},
		{"matrix: status 404 M_NOT_FOUND", "does not exist"},
		{"matrix: resolve token: bao unreachable", "secret backend"},
		{"dial tcp: no such host", "could not be reached"},
	} {
		got := ClassifyDeliveryFailure(errors.New(tc.err))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%q classified as %q, want it to mention %q", tc.err, got, tc.want)
		}
	}
	// An unrecognised failure falls through to the raw error rather than inventing a diagnosis.
	raw := "something entirely new"
	if got := ClassifyDeliveryFailure(errors.New(raw)); got != raw {
		t.Errorf("an unclassified error was given an invented diagnosis: %q", got)
	}
	if got := ClassifyDeliveryFailure(nil); got != "" {
		t.Errorf("a nil error was classified as %q", got)
	}
}

// A delivery failure is reported as a failure, with the classification carried through.
func TestProbeReportsDeliveryFailure(t *testing.T) {
	s := &probeSink{err: errors.New("matrix: status 403 M_FORBIDDEN")}
	res, err := ProbeDelivery(context.Background(), s, "", "")
	if err == nil {
		t.Fatal("a failed delivery reported success")
	}
	if !strings.Contains(res.Detail, "not a member") {
		t.Fatalf("the failure detail is not actionable: %q", res.Detail)
	}
}

// A nil sink must not panic on a request an operator triggered.
func TestProbeWithNoSinkIsRefused(t *testing.T) {
	if _, err := ProbeDelivery(context.Background(), nil, "", ""); err == nil {
		t.Fatal("a probe with no wired notifier reported success")
	}
}

// An unnamed destination degrades to a truthful phrase rather than an empty one — "delivered to " with
// nothing after it reads as a bug and tells the operator nothing.
func TestUnnamedDestinationStillReadsTruthfully(t *testing.T) {
	s := &probeSink{}
	res, err := ProbeDelivery(context.Background(), s, "@ops:example", "   ")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(res.Summary, "the configured destination") {
		t.Errorf("an unnamed destination produced %q", res.Summary)
	}
}
