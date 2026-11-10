package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/temporal/moduletest"
)

// THE SCHEDULED PROBE SWEEP — why the TEST button is not enough.
//
// On 2026-08-02, the first time anybody ran the module probes, three real faults fell out at once: both
// site syslog servers refusing every SSH session (so both sites had NO device logs during triage), a
// highest-precedence credential source that had been yielding zero bindings since it was configured, and
// an actuation token whose ACL disagreed with its allowlist. Every one of them had been true for a long
// time. Every one was invisible.
//
// They surfaced because a person pressed a button that had existed for one hour. Nobody presses
// twenty-nine buttons weekly, and the failures that matter here are precisely the ones that produce no
// symptom until the moment they are needed — a log window during an incident, a credential lookup during
// a heal. A probe that only runs when somebody suspects something is a probe that runs after the cost has
// already been paid.
//
// So the sweep runs them on a timer. It is deliberately modest: no new store, no new API surface, no
// dashboard. It reports through the channels TG already has — the log for every run, and the notifier for
// a CHANGE of state.
//
// TWO PROPERTIES DO ALL THE WORK.
//
//  1. IT SKIPS EMITTING PROBES. A notifier is proved by delivering, so its probe posts a real message into
//     an operations room. Running that on a timer is not monitoring; it is noise aimed at the one room
//     that has to stay readable during an incident. desc.TestSpec.Emits declares the property and the
//     sweep skips it — and SAYS how many it skipped, because a silent exclusion is how a monitor comes to
//     cover less than the reader believes.
//
//  2. IT NOTIFIES ON TRANSITIONS, NOT ON STATE. A sweep that paged while a module was down would send the
//     same message every interval until somebody fixed it, and a channel that repeats itself is one people
//     learn to filter — which is the failure mode that makes the whole thing worthless at the moment it
//     finally matters. One notice when a module starts failing, one when it recovers.
type probeSweep struct {
	probers map[string]moduletest.Prober
	// emitting is the set of module keys whose probe posts something a human will see. Excluded from every
	// scheduled run; still reachable from the console's TEST button, where a person is asking for it.
	emitting map[string]bool
	// notify delivers a state-change notice. nil ⇒ the sweep still runs and still logs; it simply has no
	// outbound channel, which is honest rather than fatal.
	notify func(ctx context.Context, body string)
	logf   func(string, ...any)
	// timeout bounds ONE module's probe so a hung backend cannot stall the sweep.
	timeout time.Duration

	mu     sync.Mutex
	failed map[string]string // module key -> the detail of the failure currently being reported
}

func newProbeSweep(probers map[string]moduletest.Prober, emitting map[string]bool,
	notify func(context.Context, string), logf func(string, ...any), timeout time.Duration) *probeSweep {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &probeSweep{
		probers: probers, emitting: emitting, notify: notify, logf: logf,
		timeout: timeout, failed: map[string]string{},
	}
}

// sweepResult is one run's outcome, returned so an oracle can assert on it rather than on log text.
type sweepResult struct {
	Ran, OK, Failed, Skipped int
	// Newly lists modules that changed from passing to failing in THIS run — the only thing worth waking
	// somebody for.
	Newly []string
	// Recovered lists modules that changed from failing to passing.
	Recovered []string
}

// run executes one sweep. It is safe to call concurrently with itself only in the sense that the state map
// is guarded; callers drive it from a single ticker.
func (s *probeSweep) run(ctx context.Context) sweepResult {
	if s == nil {
		return sweepResult{}
	}
	keys := make([]string, 0, len(s.probers))
	for k := range s.probers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var res sweepResult
	outcomes := make([]probeOutcome, 0, len(keys))
	for _, k := range keys {
		if s.emitting[k] {
			res.Skipped++
			continue
		}
		p := s.probers[k]
		if p == nil {
			continue
		}
		res.Ran++
		// Each probe gets its own bounded context. A shared deadline would let the first slow module eat
		// the budget of every module after it, and the sweep would then report failures that are really
		// just queue position.
		pctx, cancel := context.WithTimeout(ctx, s.timeout)
		summary, detail, err := s.probe(pctx, p, k)
		cancel()
		if err != nil {
			d := strings.TrimSpace(detail)
			if d == "" {
				d = err.Error()
			}
			outcomes = append(outcomes, probeOutcome{key: k, detail: d})
			res.Failed++
			continue
		}
		outcomes = append(outcomes, probeOutcome{key: k, detail: summary, ok: true})
		res.OK++
	}

	// Diff against the previously reported state to find the transitions.
	s.mu.Lock()
	for _, o := range outcomes {
		_, wasFailing := s.failed[o.key]
		switch {
		case !o.ok && !wasFailing:
			s.failed[o.key] = o.detail
			res.Newly = append(res.Newly, o.key)
		case !o.ok && wasFailing:
			// Still failing. Update the recorded detail so a recovery notice can quote the current fault,
			// but do NOT re-notify: a channel that repeats itself is one people learn to filter.
			s.failed[o.key] = o.detail
		case o.ok && wasFailing:
			delete(s.failed, o.key)
			res.Recovered = append(res.Recovered, o.key)
		}
	}
	s.mu.Unlock()
	sort.Strings(res.Newly)
	sort.Strings(res.Recovered)

	s.report(ctx, res, outcomes)
	return res
}

// probe calls one prober, converting a panic into a failure. A connector panicking must not take the sweep
// — or the worker — down with it.
func (s *probeSweep) probe(ctx context.Context, p moduletest.Prober, key string) (summary, detail string, err error) {
	defer func() {
		if r := recover(); r != nil {
			summary, detail, err = "", fmt.Sprintf("the probe panicked: %v", r), fmt.Errorf("probe %s panicked", key)
		}
	}()
	return p.Probe(ctx, moduletest.Request{
		Surface:    strings.SplitN(key, "/", 2)[0],
		SourceType: strings.SplitN(key+"/", "/", 3)[1],
		Operator:   "scheduled probe sweep",
	})
}

func (s *probeSweep) report(ctx context.Context, res sweepResult, outcomes []probeOutcome) {
	if s.logf == nil {
		return
	}
	// The skipped count is stated on EVERY run, not just when it is non-zero. A monitor that mentions its
	// own coverage only sometimes is one whose coverage the reader has to reconstruct.
	s.logf("module probe sweep: %d ran, %d ok, %d failed, %d skipped (emitting probes are never run on a "+
		"timer — press TEST for those)", res.Ran, res.OK, res.Failed, res.Skipped)
	for _, o := range outcomes {
		if !o.ok {
			s.logf("module probe sweep: FAIL %s — %s", o.key, o.detail)
		}
	}
	if s.notify == nil {
		return
	}
	if len(res.Newly) > 0 {
		var b strings.Builder
		b.WriteString("Module probe FAILED: ")
		b.WriteString(strings.Join(res.Newly, ", "))
		b.WriteString("\n")
		s.mu.Lock()
		for _, k := range res.Newly {
			b.WriteString("• " + k + " — " + s.failed[k] + "\n")
		}
		s.mu.Unlock()
		b.WriteString("This is a scheduled check of a configured connector, not a governance decision. " +
			"Nothing is proposed and nothing will happen as a result of this message.")
		s.notify(ctx, b.String())
	}
	if len(res.Recovered) > 0 {
		s.notify(ctx, "Module probe recovered: "+strings.Join(res.Recovered, ", ")+
			"\nThis is a scheduled check of a configured connector, not a governance decision.")
	}
}

// probeOutcome is one module's result within a sweep.
type probeOutcome struct {
	key, detail string
	ok          bool
}
