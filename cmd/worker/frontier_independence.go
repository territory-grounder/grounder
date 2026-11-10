package main

import (
	"context"
	"fmt"
	"sync"

	coregov "github.com/territory-grounder/grounder/core/governance"
)

// independenceGatedPairs re-verifies frontier/local judge INDEPENDENCE until it actually resolves, then
// caches the answer — instead of deciding once at boot and living with whatever it got (TG-356).
//
// THE DEFECT THIS CLOSES. The boot-time check is `Gateway.SameUpstreamModel`, and it is correct. But it
// runs ONCE, in the same startup window in which litellm may not yet be listening, and on failure it
// falls through to the tier-NAME comparison — which is precisely what TG-356 was filed to replace.
// Measured on dc1tg01 across two consecutive boots (01:37:37 and again at 02:01:43 after a fresh
// deploy, so this is deterministic rather than a one-off):
//
//	frontier cross-check: could not resolve "fallback-mistral" and "primary" to upstream models
//	  (model-info: dial tcp 172.23.0.3:4000: connect: connection refused)
//	  — arming on the NAME check alone; independence is UNVERIFIED … (TG-356)
//	frontier cross-check: armed on the INDEPENDENT tier "fallback-mistral" (local judge "primary")
//
// The second line announces success without qualifying it, and nothing ever revisits the first. Probed
// from the worker's own network AFTER boot, `http://litellm:4000/v1/model/info` answers HTTP 401 — the
// gateway is up and resolvable within seconds. The information needed was available; the code had simply
// stopped asking.
//
// WHY GATE THE PAIR SOURCE RATHER THAN RETRY AT BOOT. Blocking startup on a dependency trades one failure
// for another, and a background goroutine that mutates an armed monitor races the scheduler. The monitor's
// Run reads Pairs FIRST and returns on its error, so the pair source is the natural chokepoint: every
// scheduled run re-attempts the resolution until it succeeds, and a confirmed same-model pair REFUSES
// there — the anchor cannot grade the judge with itself even for one run.
//
// FAIL-OPEN ON UNRESOLVABLE IS DELIBERATE AND UNCHANGED. An unreachable gateway must not silence a
// cross-check that may well be independent; it must only stop the claim being reported as verified.
type independenceGatedPairs struct {
	inner           coregov.PairSource
	resolve         func(ctx context.Context) (same bool, resolved bool, err error)
	logf            func(string, ...any)
	frontier, local string

	mu      sync.Mutex
	settled bool // resolution has succeeded at least once; `same` is then authoritative
	same    bool
}

func (p *independenceGatedPairs) RecentCrossCheckPairs(ctx context.Context) ([]coregov.CrossCheckPair, error) {
	p.mu.Lock()
	settled, same := p.settled, p.same
	p.mu.Unlock()

	if !settled {
		s, resolved, err := p.resolve(ctx)
		switch {
		case err == nil && resolved:
			p.mu.Lock()
			p.settled, p.same = true, s
			p.mu.Unlock()
			settled, same = true, s
			if s {
				p.logf("frontier cross-check: independence RESOLVED LATE and it is NOT independent — %q and %q "+
					"are one upstream model under two aliases. The anchor is REFUSING to run; it was armed at "+
					"boot on the name check alone because the gateway was unreachable then (TG-356).",
					p.frontier, p.local)
			} else {
				p.logf("frontier cross-check: independence VERIFIED on a later run — %q and %q resolve to "+
					"different upstream models. The boot-time claim was UNVERIFIED, not wrong (TG-356).",
					p.frontier, p.local)
			}
		default:
			// Still unreachable. Say so per run rather than once at boot, so a permanently-degraded anchor
			// is visible in the log of the run that was degraded, not only in a startup line scrolled away.
			p.logf("frontier cross-check: independence still UNVERIFIED — could not resolve %q and %q to "+
				"upstream models (%v). Running on the name check alone; two aliases can be one model (TG-356).",
				p.frontier, p.local, err)
		}
	}

	if settled && same {
		return nil, fmt.Errorf("frontier cross-check: refusing to run — %q and %q resolve to the SAME "+
			"upstream model, so the anchor would be the judge grading itself (TG-356)", p.frontier, p.local)
	}
	return p.inner.RecentCrossCheckPairs(ctx)
}
