package notifier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSink is a scriptable channel. Every field is read under mu because Notify runs concurrently.
type fakeSink struct {
	src     string
	err     error
	panics  bool
	before  func() // runs inside Notify, before the result is decided — used to prove concurrency
	mu      sync.Mutex
	got     []Notice
	voteFor string // ResolveVote succeeds only when raw equals this
}

func (f *fakeSink) SourceType() string { return f.src }

func (f *fakeSink) Notify(_ context.Context, n Notice) error {
	if f.before != nil {
		f.before()
	}
	if f.panics {
		panic("adapter exploded")
	}
	f.mu.Lock()
	f.got = append(f.got, n)
	f.mu.Unlock()
	return f.err
}

func (f *fakeSink) ResolveVote(_ context.Context, raw []byte) (Vote, error) {
	if f.voteFor != "" && string(raw) == f.voteFor {
		return Vote{DecisionID: "D-1", Sender: f.src, Approve: true}, nil
	}
	return Vote{}, fmt.Errorf("%s does not recognise this response", f.src)
}

func (f *fakeSink) received() []Notice {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Notice, len(f.got))
	copy(out, f.got)
	return out
}

// THE PROPERTY THIS TYPE EXISTS FOR: every channel failing must be an ERROR. A fan-out that returned
// nil here would page nobody and report success — the defect the composite replaces, reproduced at N
// channels instead of one.
func TestFanoutAllChannelsFailingIsAnError(t *testing.T) {
	a := &fakeSink{src: "matrix", err: errors.New("homeserver 502")}
	b := &fakeSink{src: "email", err: errors.New("smtp refused")}
	c := &fakeSink{src: "twilio-sms", err: errors.New("no credit")}

	rep, err := NewFanout(a, b, c).NotifyReport(context.Background(), Notice{DecisionID: "D-1", Body: "page"})
	if err == nil {
		t.Fatal("all three channels failed and the fan-out reported SUCCESS — the notice reached nobody " +
			"and nothing anywhere says so")
	}
	if rep.Delivered != 0 || rep.Attempted != 3 {
		t.Fatalf("report should be 0 delivered of 3 attempted, got %d of %d", rep.Delivered, rep.Attempted)
	}
	// The error must name WHICH channels failed: "delivery failed" alone leaves an operator with no
	// starting point at 3am.
	for _, want := range []string{"matrix", "email", "twilio-sms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name the failed channel %q: %v", want, err)
		}
	}
}

// The other half of the asymmetry: reaching SOMEONE is a success. A fan-out that failed the delivery
// because one vendor was down would escalate on a page that actually worked.
func TestFanoutPartialDeliveryIsSuccessAndStillNamesTheCasualties(t *testing.T) {
	down := &fakeSink{src: "matrix", err: errors.New("homeserver 502")}
	up := &fakeSink{src: "twilio-sms"}

	rep, err := NewFanout(down, up).NotifyReport(context.Background(), Notice{DecisionID: "D-2", Body: "page"})
	if err != nil {
		t.Fatalf("sms delivered, so the human was reached — this must be a success, got: %v", err)
	}
	if rep.Delivered != 1 {
		t.Fatalf("want 1 delivered, got %d", rep.Delivered)
	}
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "matrix") {
		t.Fatalf("a successful partial delivery must still report the dead channel, got %v", rep.Failures)
	}
	if len(up.received()) != 1 {
		t.Fatalf("the live channel received %d notices, want 1", len(up.received()))
	}
}

// Zero channels is "nothing was delivered", not "nothing needed delivering".
func TestFanoutWithNoChannelsIsAnError(t *testing.T) {
	if _, err := NewFanout().NotifyReport(context.Background(), Notice{DecisionID: "D-3"}); err == nil {
		t.Fatal("an empty fan-out reported success; a caller cannot distinguish that from a delivered page")
	}
	// A nil sink is not a channel. Counting it would inflate Attempted and let an all-nil fan-out claim
	// it tried.
	f := NewFanout(nil, nil)
	if f.Len() != 0 {
		t.Fatalf("nil sinks must not be counted as channels, Len()=%d", f.Len())
	}
	if _, err := f.NotifyReport(context.Background(), Notice{DecisionID: "D-3"}); err == nil {
		t.Fatal("a fan-out of nil sinks reported success")
	}
}

// EVERY channel gets the notice — not just the first one that works. Redundancy is the entire reason an
// operator configures three channels.
func TestFanoutDeliversToEveryChannel(t *testing.T) {
	sinks := []*fakeSink{{src: "matrix"}, {src: "email"}, {src: "twilio-sms"}}
	n := Notice{DecisionID: "D-4", Body: "approve host reboot?", Approval: true}
	if err := NewFanout(sinks[0], sinks[1], sinks[2]).Notify(context.Background(), n); err != nil {
		t.Fatalf("all channels healthy, got error: %v", err)
	}
	for _, s := range sinks {
		got := s.received()
		if len(got) != 1 {
			t.Fatalf("%s received %d notices, want exactly 1", s.src, len(got))
		}
		// Field-wise: Notice now carries a []Choice, so it is no longer comparable with != — and a
		// reflect.DeepEqual here would quietly stop checking the day someone adds an unexported field.
		if got[0].DecisionID != n.DecisionID || got[0].Body != n.Body || got[0].Approval != n.Approval {
			t.Errorf("%s received a mutated notice: %+v", s.src, got[0])
		}
		if len(got[0].Choices) != len(n.Choices) {
			t.Errorf("%s received %d choice(s), want %d — a poll that loses its options in transit is a "+
				"poll an approver cannot answer", s.src, len(got[0].Choices), len(n.Choices))
		}
	}
}

// CONCURRENCY, proven structurally rather than by a stopwatch: every channel blocks until ALL of them
// have entered Notify. A serial fan-out can never satisfy that — the first sink waits for a second that
// has not been called yet — so this test times out instead of passing. A timing threshold would be
// flaky on a loaded box; a barrier cannot be.
func TestFanoutDeliversConcurrently(t *testing.T) {
	const n = 3
	var wg sync.WaitGroup
	wg.Add(n)
	arrived := func() {
		wg.Done() // I am here
		wg.Wait() // ...and I will not leave until everyone else is too
	}
	sinks := make([]Notifier, n)
	for i := range sinks {
		sinks[i] = &fakeSink{src: fmt.Sprintf("ch-%d", i), before: arrived}
	}

	done := make(chan error, 1)
	go func() { done <- NewFanout(sinks...).Notify(context.Background(), Notice{DecisionID: "D-5"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("healthy channels, got error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delivery did not complete: the channels are being called SERIALLY, so one slow vendor " +
			"delays every other channel on a page")
	}
}

// A panicking adapter must not take the worker down, and must never be counted as delivered.
func TestFanoutPanickingChannelCountsAsFailedNotDelivered(t *testing.T) {
	boom := &fakeSink{src: "matrix", panics: true}
	ok := &fakeSink{src: "email"}

	rep, err := NewFanout(boom, ok).NotifyReport(context.Background(), Notice{DecisionID: "D-6"})
	if err != nil {
		t.Fatalf("email delivered, so this is a success: %v", err)
	}
	if rep.Delivered != 1 {
		t.Fatalf("a panicking adapter was counted as delivered: %d of %d", rep.Delivered, rep.Attempted)
	}
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "matrix") {
		t.Fatalf("the panic was not reported as a channel failure: %v", rep.Failures)
	}

	// And when the panic is the ONLY channel, the error must survive — not be swallowed into success.
	if _, err := NewFanout(&fakeSink{src: "matrix", panics: true}).NotifyReport(context.Background(), Notice{}); err == nil {
		t.Fatal("a fan-out whose only channel panicked reported SUCCESS")
	}
}

// A vote arrives on ONE channel. The composite must find the one that recognises it.
func TestFanoutResolveVoteAnswersFromTheRecognisingChannel(t *testing.T) {
	f := NewFanout(
		&fakeSink{src: "matrix"},
		&fakeSink{src: "twilio-sms", voteFor: "YES D-1"},
	)
	v, err := f.ResolveVote(context.Background(), []byte("YES D-1"))
	if err != nil {
		t.Fatalf("sms recognised this response; the fan-out must return it: %v", err)
	}
	if v.Sender != "twilio-sms" || v.DecisionID != "D-1" || !v.Approve {
		t.Fatalf("wrong vote resolved: %+v", v)
	}
}

// An unrecognised response must be an ERROR, never a zero Vote — a zero Vote is indistinguishable from
// a real vote that decided nothing, and INV-12 binds a vote to the decision it answers.
func TestFanoutResolveVoteUnrecognisedIsAnErrorNotAnEmptyVote(t *testing.T) {
	f := NewFanout(&fakeSink{src: "matrix"}, &fakeSink{src: "email"})
	v, err := f.ResolveVote(context.Background(), []byte("lgtm"))
	if err == nil {
		t.Fatalf("an unrecognised response resolved to a vote: %+v", v)
	}
	if v != (Vote{}) {
		t.Fatalf("a rejected response must carry no vote payload, got %+v", v)
	}
}

// The composite is not "matrix". A log line attributing a fan-out delivery to one member would
// misattribute every delivery it made.
func TestFanoutSourceTypeIsNotAMemberChannel(t *testing.T) {
	f := NewFanout(&fakeSink{src: "matrix"}, &fakeSink{src: "email"})
	if got := f.SourceType(); got == "matrix" || got == "email" {
		t.Fatalf("SourceType() reports a member channel %q — every fan-out delivery would be misattributed", got)
	}
	// Attempt order is deterministic so error text and boot logs do not reshuffle between restarts.
	if got := strings.Join(f.Sources(), ","); got != "email,matrix" {
		t.Fatalf("channel order is not deterministic: %q", got)
	}
}

// The interface is what the composition root binds; a Fanout that does not satisfy it cannot be wired.
var _ Notifier = (*Fanout)(nil)
