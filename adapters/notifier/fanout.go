package notifier

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// FANOUT — deliver one notice to every configured channel (MECH-719).
//
// Until now the worker bound a notifier only when EXACTLY ONE was enabled. With several enabled it
// delivered NOTHING and said so in its own comment: "strictly worse than one". So an operator who
// configured matrix AND email AND SMS — wanting redundancy on the page that wakes them — got silence,
// and the more channels they added the more certain the silence became.
//
// THE ONE PROPERTY THAT MATTERS. For an escalation, reaching SOMEONE beats reaching everyone: if matrix
// is down and SMS delivers, the human is awake and the page did its job. So a partial delivery is a
// SUCCESS. But an all-channel failure must be an ERROR, loudly — a fan-out that returned nil when every
// channel failed would reproduce, at N channels, the exact defect this replaces: a page reporting
// success while the notice reached nobody. That asymmetry is the whole design; everything else serves it.
type Fanout struct {
	// sinks are the configured channels, ordered by source type so error text and logs are stable
	// between boots.
	sinks []Notifier
}

// NewFanout builds a composite over the given notifiers.
func NewFanout(sinks ...Notifier) *Fanout {
	live := make([]Notifier, 0, len(sinks))
	for _, s := range sinks {
		if s != nil { // a nil sink is not a channel; counting it would inflate the denominator in Notify
			live = append(live, s)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].SourceType() < live[j].SourceType() })
	return &Fanout{sinks: live}
}

// Len reports how many channels this fan-out will attempt, so a caller can distinguish "no channels"
// from "channels that failed". The composite refuses to paper over the first.
func (f *Fanout) Len() int {
	if f == nil {
		return 0
	}
	return len(f.sinks)
}

// Sources names the channels, in attempt order, for the wiring log.
func (f *Fanout) Sources() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.sinks))
	for _, s := range f.sinks {
		out = append(out, s.SourceType())
	}
	return out
}

// SourceType names the composite. Deliberately not one member's type: a notice delivered by a fan-out
// did not come from "matrix", and a log line claiming it did would misattribute every delivery.
func (f *Fanout) SourceType() string { return "fanout" }

// Report is the per-channel outcome of one delivery.
type Report struct {
	Attempted int
	Delivered int
	// Failures names each channel that failed, with its error. Present even on SUCCESS: a partial
	// delivery is a success, so the casualties can only reach a log through a non-error channel.
	Failures []string
}

// Notify delivers to every channel and returns nil if AT LEAST ONE succeeded.
func (f *Fanout) Notify(ctx context.Context, n Notice) error {
	_, err := f.NotifyReport(ctx, n)
	return err
}

// NotifyReport is Notify with the per-channel outcome returned alongside. Identical error contract —
// there is exactly one delivery path here, so the two cannot disagree about what happened.
//
// Delivery is CONCURRENT: these are independent calls to unrelated vendors, and a serial fan-out costs
// the sum of its worst channels. On a page, latency is the product. One wedged channel must not delay
// the others; the caller's context bounds them all.
func (f *Fanout) NotifyReport(ctx context.Context, n Notice) (Report, error) {
	rep := Report{Attempted: f.Len()}
	if f == nil || len(f.sinks) == 0 {
		// NOT nil. Zero channels means nothing was delivered, and reporting that as success is precisely
		// the defect this type exists to remove.
		return rep, fmt.Errorf("notifier fanout: no channels configured — nothing was delivered")
	}

	errs := make([]error, len(f.sinks))
	var wg sync.WaitGroup
	for i, s := range f.sinks {
		wg.Add(1)
		go func(i int, s Notifier) {
			defer wg.Done()
			// A panicking adapter must not take the worker down, and must never be counted as delivered.
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("panicked: %v", r)
				}
			}()
			errs[i] = s.Notify(ctx, n)
		}(i, s)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: %v", f.sinks[i].SourceType(), err))
			continue
		}
		rep.Delivered++
	}
	if rep.Delivered == 0 {
		// EVERY channel failed. This is the case that must never be silent.
		return rep, fmt.Errorf("notifier fanout: ALL %d channel(s) failed, notice delivered to nobody — %s",
			rep.Attempted, strings.Join(rep.Failures, "; "))
	}
	return rep, nil
}

// ResolveVote answers from the FIRST channel that recognises the response.
//
// A vote arrives on ONE channel — the one the operator replied on — so this is a lookup, not a fan-out.
// Channels are asked in the same deterministic order and the first non-error answer wins. A response no
// channel recognises is an ERROR, never a zero-value Vote: an unrecognised reply that returned an empty
// Vote would be indistinguishable from a real vote that decided nothing (INV-12).
func (f *Fanout) ResolveVote(ctx context.Context, raw []byte) (Vote, error) {
	if f == nil || len(f.sinks) == 0 {
		return Vote{}, fmt.Errorf("notifier fanout: no channels configured")
	}
	var tried []string
	for _, s := range f.sinks {
		v, err := s.ResolveVote(ctx, raw)
		if err == nil {
			return v, nil
		}
		tried = append(tried, fmt.Sprintf("%s: %v", s.SourceType(), err))
	}
	return Vote{}, fmt.Errorf("notifier fanout: no channel recognised the response — %s", strings.Join(tried, "; "))
}
