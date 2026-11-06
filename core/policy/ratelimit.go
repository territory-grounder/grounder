package policy

// rate_limit governor — spec/015 task T-015-3 (REQ-1508). WHILE a rule declares a rate_limit of N executions
// per window, the engine counts AUTO executions matching that rule (keyed by op-class, or "global") within the
// trailing window and clamps the (N+1)th matching auto in that window auto→approve (route to a human). This is
// the second half of step 3 of the mode/verdict decision procedure (design.md); the first half is the
// confidence clamp in confidence.go.
//
// The governor is CONCURRENCY-SAFE (a mutex guards the per-key event log) and uses an INJECTED CLOCK SEAM
// (NewRateGovernor(now)) so the trailing-window arithmetic is DETERMINISTIC under test — the codebase forbids
// nondeterministic time in oracles (a nil clock defaults to time.Now for production). It only ever TIGHTENS
// (auto→approve), never loosens, and it only counts autos that actually WOULD execute: an already-tightened
// approve/deny is never charged against the budget, and a clamped (over-cap) auto is NOT recorded either
// (it did not auto-execute), so the window measures real auto-executions, not attempts.
//
// Durable rate state across restarts is OUT OF SCOPE (T-015-12); this leaf holds the window in memory.
//
// Provenance: [F]. See spec/015-policy-engine REQ-1508.

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// defaultRateWindow is the trailing window the governor counts auto-executions over. REQ-1508 phrases the
// conservative governor as "N per minute" (REQ-1512), so the window is one minute.
const defaultRateWindow = time.Minute

// rateKey normalises a governor key: an op-class (trimmed, lower-cased) or the sentinel "global" for an
// unscoped governor. An empty key is the global bucket.
func rateKey(opClass string) string {
	k := strings.ToLower(strings.TrimSpace(opClass))
	if k == "" {
		return "global"
	}
	return k
}

// RateRecord is the NON-SECRET projection of one rate-governor decision (the audit trail for the rate half of
// step 3). Every field is a plain verdict/number/bool/string — no credential, argv, or host-secret.
type RateRecord struct {
	VerdictIn     Verdict // the verdict presented to the governor.
	VerdictOut    Verdict // the verdict after the governor (== VerdictIn unless Clamped).
	Key           string  // the governor key counted against (op-class or "global").
	Limit         int     // the resolved rate_limit per window (0/unset ⇒ no governor).
	CountInWindow int     // prior auto executions in the trailing window (before this action).
	Clamped       bool    // true WHEN the governor tightened auto→approve.
	Reason        string  // human-readable explanation for the console packet-tracer.
}

// RateGovernor caps AUTO executions per key within a trailing window. It is concurrency-safe. The zero value is
// NOT ready — build one with NewRateGovernor so the clock and maps are initialised; a nil *RateGovernor is
// tolerated by clamp/Refine as a no-op governor (defense against an uninitialised caller).
type RateGovernor struct {
	mu     sync.Mutex
	now    func() time.Time
	window time.Duration
	events map[string][]time.Time // per-key timestamps of ADMITTED auto executions in/near the window.

	// ── OBSERVABILITY (TG-339) ────────────────────────────────────────────────────────────────────────
	// This governor was DARK for its whole life: WithRateGovernor had one caller and it was a spec
	// acceptance test, so `rateGov` was nil in every production worker while
	// core/policy/templates/conservative.json advertised `"rate_limit": 30`. It is wired now
	// (cmd/worker/main.go, pinned by policy_rate_governor_wired_test.go) and still published nothing, so
	// there was no way to answer from outside the process whether it had ever clamped anything.
	//
	// `ungoverned` is the load-bearing counter and the reason this is three numbers rather than one. A
	// limit that never reaches clamp() takes the `limit <= 0` branch and returns the verdict untouched —
	// the control is inert while reading as armed, which is exactly the state TG-339 found it in. Counting
	// only clamps cannot tell "nothing needed clamping" from "no limit was ever in force"; the ratio of
	// governed to ungoverned can.
	governed   uint64 // calls where a positive limit was in force (auto verdicts only)
	ungoverned uint64 // calls where limit <= 0 — the governor was consulted and had nothing to enforce
	clamped    uint64 // auto -> approve tightenings
}

// RateGovernorStats is one reading of the governor's lifetime counters, for /metrics.
type RateGovernorStats struct {
	Governed   uint64
	Ungoverned uint64
	Clamped    uint64
	Window     time.Duration
}

// Stats reports the governor's counters. Safe on a nil governor: a deployment that never built one reports
// zeros rather than panicking, and the ABSENCE of the series is what the vacuity alert watches.
func (g *RateGovernor) Stats() RateGovernorStats {
	if g == nil {
		return RateGovernorStats{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return RateGovernorStats{Governed: g.governed, Ungoverned: g.ungoverned, Clamped: g.clamped, Window: g.window}
}

// NewRateGovernor builds a governor over an injected clock seam (nil ⇒ time.Now). The window defaults to one
// minute (REQ-1508); override with WithWindow.
func NewRateGovernor(now func() time.Time) *RateGovernor {
	if now == nil {
		now = time.Now
	}
	return &RateGovernor{now: now, window: defaultRateWindow, events: map[string][]time.Time{}}
}

// WithWindow overrides the trailing window (oracle use) and returns the governor.
func (g *RateGovernor) WithWindow(w time.Duration) *RateGovernor {
	if g == nil {
		return g
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if w > 0 {
		g.window = w
	}
	return g
}

// Clamp tightens an `auto` verdict to `approve` WHEN admitting it would exceed the limit auto-executions in the
// trailing window for key (REQ-1508); otherwise it ADMITS the auto and records the execution so it counts
// toward the window. It only ever tightens. A non-positive/unset limit means no governor; a non-`auto` verdict
// is never governed (already at/above the human bar) and never charged. Concurrency-safe.
func (g *RateGovernor) Clamp(verdict Verdict, key string, limit int) (Verdict, RateRecord) {
	return g.clamp(verdict, key, limit)
}

// clamp is the internal implementation shared by Clamp and Refine.
func (g *RateGovernor) clamp(verdict Verdict, key string, limit int) (Verdict, RateRecord) {
	k := rateKey(key)
	rec := RateRecord{VerdictIn: verdict, VerdictOut: verdict, Key: k, Limit: limit}

	// No governor: an unset/non-positive limit, or a nil/uninitialised governor.
	if limit <= 0 || g == nil {
		if g != nil {
			g.mu.Lock()
			g.ungoverned++
			g.mu.Unlock()
		}
		rec.Reason = "no rate governor (rate_limit unset / <= 0)"
		return verdict, rec
	}
	// Only `auto` is governed; approve/deny are already at/above the human bar and are never charged.
	if verdict != VerdictAuto {
		rec.Reason = fmt.Sprintf("rate governor does not apply to %q", verdict)
		return verdict, rec
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	cutoff := now.Add(-g.window)
	ev := pruneBefore(g.events[k], cutoff)
	rec.CountInWindow = len(ev)

	if len(ev) >= limit {
		// Over the cap: clamp auto→approve and do NOT record — this action did not auto-execute.
		g.events[k] = ev
		g.governed++
		g.clamped++
		rec.VerdictOut = VerdictApprove
		rec.Clamped = true
		rec.Reason = fmt.Sprintf("%d auto executions in the trailing %s meets the %d/window cap → clamped auto→approve (route to a human)",
			len(ev), g.window, limit)
		return VerdictApprove, rec
	}

	// Under the cap: admit the auto and record it so it counts toward the window.
	ev = append(ev, now)
	g.events[k] = ev
	g.governed++
	rec.Reason = fmt.Sprintf("auto admitted (%d/%d in the trailing %s)", len(ev), limit, g.window)
	return verdict, rec
}

// pruneBefore drops timestamps at or before cutoff (outside the trailing window) and returns the survivors. It
// reuses the input's backing array; callers hold the lock and reassign the slice, so the aliasing is safe.
func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
