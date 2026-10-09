package actuate

// limiter.go — TG-166(a): the ACTUATION-FREQUENCY governor. Until this existed there was NO rate limit and NO
// concurrency cap at ANY scope on the mutating path, so an in-grammar, allowlisted, reversible, evidence-bound
// restart could be emitted over and over and every one of them would execute. Each individual actuation passed
// every gate honestly — the failure is that the gates are all per-ACTION and none of them can see a SEQUENCE.
// The chain proved "this restart is safe" a hundred times and never asked "why is this the hundredth?".
//
// ★ WHY THIS IS NOT core/policy's RateGovernor (core/policy/ratelimit.go). That governor exists and is a
// perfectly good control for what it does, but it does not — and structurally cannot — close this hole:
//
//  1. IT IS DARK. `Engine.WithRateGovernor` has exactly one caller in the tree and it is
//     spec/015-policy-engine/acceptance/acceptance_test.go. cmd/worker/main.go builds the engine with
//     policy.NewEngine(...).WithGraduation(...) and NEVER attaches a governor, so `rateGov` is nil in every
//     production worker and Refine "degrades to the confidence clamp alone" (confidence.go). The
//     `"rate_limit": 30` in core/policy/templates/conservative.json has been reading like an armed control
//     while counting nothing.
//  2. WRONG SCOPE. Its key is the op-class (or "global"). "restart-service, 30/min, fleet-wide" cannot express
//     "this ONE session must not actuate five times" or "this ONE host must not be restarted five times",
//     which is exactly the shape of a subverted-agent loop: one session, one target, one op-class.
//  3. WRONG LEVER. It clamps auto→approve. A POLL_PAUSE-band action that already carries a recorded human
//     approval is never charged and never clamped, so the loop simply runs on the approved lane. And it charges
//     at policy-DECIDE time, which is five gates and two monitoring reads before the effect — an action clamped
//     there may never have executed, and an action admitted there may still be refused by the breaker, the mode
//     chokepoint, the host-match gate or the baseline gate.
//
// So this is a DIFFERENT control at a DIFFERENT altitude, not a second copy: it sits at the actuation
// chokepoint immediately before the effect fires, it counts EFFECTS rather than verdicts, and it REFUSES
// rather than re-routes. The policy governor stays what it is (a verdict-shaping clamp); when someone wires it,
// the two compose — it tightens auto→approve up-stream, this one refuses outright at the door.
//
// FAIL CLOSED, THREE WAYS:
//   - There is no way to spell "unlimited". A non-positive limit in ActuationLimits takes the conservative
//     DEFAULT, it does not disable the scope. The vacuous-zero shape ("0 means off") is the exact bug class
//     this codebase keeps finding, so the type simply cannot express it.
//   - There is no nil path in production: NewInterceptor installs a default limiter by construction and
//     WithActuationLimiter ignores a nil argument. If a nil somehow reaches the gate it REFUSES (see Admit).
//   - An absent scope key does not exempt an actuation, it SHARES a bucket. An action with no session ref
//     counts against one "(unattributed)" session budget with every other unattributed action, so losing the
//     key can only ever make the governor stricter — never buy unlimited actuation, which is what a
//     `key == "" ⇒ pass` would have done.
//
// THE BUDGET IS CHARGED AT ADMISSION TO THE EFFECT, NOT AT SUCCESS. An actuation that reaches this gate and
// then fails at execute has still consumed an attempt against the estate: it opened a connection, ran a
// command, and was refused by the target. Charging only successes would hand a subverted agent unlimited
// retries in exchange for making each one fail, which inverts the control.
//
// SCOPE OF THE GUARANTEE — say it plainly: the window is held IN PROCESS. Two workers each hold their own
// budget, so the fleet-wide cap is (workers × cap), not (cap). That is a real limit and it is stated here
// rather than implied away; the durable cross-process version is the same follow-on the breaker needed
// (core/safety/mutation_breaker.go took a shared store for exactly this reason). Per-process is nonetheless
// load-bearing today: the subverted-agent loop this ticket describes runs a SINGLE session on a SINGLE
// worker, and that loop is stopped here.
//
// Provenance: [O] INV-09 (mutation floor), INV-21/S8-5 (wired-by-construction, fail loud, no dark control) ·
// TG-166 (TG-153 Medium#11, TG-154 threat-modeling partial: "no action-frequency governor").

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ActuationLimits is the per-scope budget the governor enforces. Every field is a POSITIVE cap; a
// non-positive value means "use the default", NOT "no limit" (see DefaultActuationLimits) — the type
// deliberately cannot express an unlimited scope.
type ActuationLimits struct {
	// Window is the trailing window the per-scope actuation counts are taken over.
	Window time.Duration
	// SessionPerWindow caps actuations admitted to the effect for ONE session (external_ref) in Window. A
	// session legitimately heals once, or twice if the first attempt was partial; a third is a loop.
	SessionPerWindow int
	// TargetPerWindow caps actuations admitted to the effect against ONE target host in Window. Restarting the
	// same box repeatedly inside a few minutes is a flap that a human should look at, not a heal to repeat.
	TargetPerWindow int
	// SessionInFlight caps CONCURRENT in-flight actuations for one session.
	SessionInFlight int
	// TargetInFlight caps CONCURRENT in-flight actuations against one target host. Two simultaneous mutations
	// on one box is the case no per-action gate can see, because each of them is individually admissible.
	TargetInFlight int
}

// DefaultActuationLimits is the conservative budget an unconfigured limiter enforces. It is deliberately
// sized so it is INVISIBLE to the legitimate live shape (one incident → one heal, occasionally a second after
// a partial) and bites immediately on a loop. Being more restrictive than the previous behaviour (no limit at
// any scope) is the SAFE direction for a default: the failure mode of a too-tight cap is a refused heal that
// an operator can see and raise, and the failure mode of no cap is the estate being restarted in a loop.
var DefaultActuationLimits = ActuationLimits{
	Window:           10 * time.Minute,
	SessionPerWindow: 2,
	TargetPerWindow:  3,
	SessionInFlight:  1,
	TargetInFlight:   1,
}

// withDefaults fills every non-positive field from DefaultActuationLimits. A caller that wants a scope
// loosened must say a number; there is no value that switches a scope off.
func (l ActuationLimits) withDefaults() ActuationLimits {
	if l.Window <= 0 {
		l.Window = DefaultActuationLimits.Window
	}
	if l.SessionPerWindow <= 0 {
		l.SessionPerWindow = DefaultActuationLimits.SessionPerWindow
	}
	if l.TargetPerWindow <= 0 {
		l.TargetPerWindow = DefaultActuationLimits.TargetPerWindow
	}
	if l.SessionInFlight <= 0 {
		l.SessionInFlight = DefaultActuationLimits.SessionInFlight
	}
	if l.TargetInFlight <= 0 {
		l.TargetInFlight = DefaultActuationLimits.TargetInFlight
	}
	return l
}

// unattributed is the shared bucket an actuation with no session ref / no target lands in. It is NOT an
// exemption: every unattributed actuation of that scope counts against the SAME budget, so an absent key can
// only tighten the governor.
const unattributed = "(unattributed)"

// scopeKey normalises one scope's key. An empty/blank key becomes the shared unattributed bucket.
func scopeKey(scope, key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		k = unattributed
	}
	return scope + ":" + k
}

// ActuationLimiter is the per-session / per-target actuation-frequency and in-flight-concurrency governor.
// It is concurrency-safe and uses an INJECTED CLOCK SEAM so the trailing-window arithmetic is deterministic
// under oracle (this codebase forbids nondeterministic time in tests). Build it with NewActuationLimiter;
// the zero value is not ready and Admit refuses on a nil receiver.
type ActuationLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	limits   ActuationLimits
	admitted map[string][]time.Time // scope-key → times of actuations ADMITTED to the effect, in/near the window
	inFlight map[string]int         // scope-key → actuations currently between Admit and Release

	// LIFETIME TALLIES, so the governor's silence stops being ambiguous.
	//
	// This limiter was wired on the real path (interceptor.go calls Admit before every effect) and
	// published NOTHING. For a rate governor that is a specific kind of blind: it is SUPPOSED to be quiet,
	// so "has never needed to refuse" and "is admitting everything because its window is misconfigured"
	// produce identical evidence — none. Neither can a leaked lease (Admit without Release) be seen, and
	// that one wedges the lane silently.
	//
	// Counters rather than gauges: they are monotonic facts about what this process decided. The in-flight
	// count is derived on read from inFlight, which is the live state.
	totalAdmitted uint64
	totalRefused  uint64
}

// LimiterStats is a point-in-time reading of the governor. Returned as a struct rather than published from
// here so core/actuate keeps no dependency on the metrics layer.
type LimiterStats struct {
	// Admitted and Refused are lifetime counts for this process.
	Admitted, Refused uint64
	// InFlight is how many actuations are currently between Admit and Release across every scope. A value
	// that only ever grows is a leaked lease, which no other reading here would reveal.
	InFlight int
	// Window and the budget, echoed so a reader can judge the counts against the limits that produced
	// them. A refusal count is unreadable without knowing the budget it was measured against.
	Window                            time.Duration
	SessionPerWindow, TargetPerWindow int
	SessionInFlight, TargetInFlight   int
}

// Stats returns the governor's current tallies. Safe on a nil receiver: an unwired limiter reports zeroes
// rather than panicking a metrics scrape.
func (l *ActuationLimiter) Stats() LimiterStats {
	if l == nil {
		return LimiterStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var inFlight int
	for _, n := range l.inFlight {
		inFlight += n
	}
	// inFlight is keyed by BOTH scopes (session and target) and Admit charges both, so every live
	// actuation appears twice. Halve it, or the gauge reads double and a reader chasing a leak is chasing
	// an artefact of the bookkeeping.
	return LimiterStats{
		Admitted:         l.totalAdmitted,
		Refused:          l.totalRefused,
		InFlight:         inFlight / 2,
		Window:           l.limits.Window,
		SessionPerWindow: l.limits.SessionPerWindow,
		TargetPerWindow:  l.limits.TargetPerWindow,
		SessionInFlight:  l.limits.SessionInFlight,
		TargetInFlight:   l.limits.TargetInFlight,
	}
}

// NewActuationLimiter builds a governor over an injected clock (nil ⇒ time.Now) with
// DefaultActuationLimits. Override the budget with WithLimits.
func NewActuationLimiter(now func() time.Time) *ActuationLimiter {
	if now == nil {
		now = time.Now
	}
	return &ActuationLimiter{
		now:      now,
		limits:   DefaultActuationLimits,
		admitted: map[string][]time.Time{},
		inFlight: map[string]int{},
	}
}

// WithLimits sets the budget (each non-positive field falls back to its conservative default) and returns the
// limiter for chaining. A nil receiver is returned unchanged.
func (l *ActuationLimiter) WithLimits(in ActuationLimits) *ActuationLimiter {
	if l == nil {
		return l
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = in.withDefaults()
	return l
}

// Limits reports the effective budget (oracle/diagnostic use).
func (l *ActuationLimiter) Limits() ActuationLimits {
	if l == nil {
		return ActuationLimits{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limits
}

// ActuationLease is one admitted actuation's hold on the in-flight budget. Release MUST be called (the
// interceptor defers it the instant it is granted) or the scope's concurrency slot leaks and every later
// actuation against that session/target is refused — fail-closed, but a leak is still a bug, so the lease is
// deliberately small and has exactly one method.
type ActuationLease struct {
	l    *ActuationLimiter
	keys []string
	done bool
	// headroom is the tightest RATE-budget slack that remained AFTER this admission was charged: the minimum,
	// over the session and target scopes, of (per-window cap − trailing-window count). Zero means this
	// actuation consumed the last slot before the frequency throttle — the boundary case a reviewer wants to
	// see. It deliberately tracks the rate budget, NOT the in-flight concurrency cap: that cap is a binary
	// mutex (default 1), so its slack is always zero on the pass path and carries no "how close to the
	// throttle" information. OBSERVE-ONLY (TG-178): read by the interceptor solely to stamp the gate-margin on
	// the observe-only verdict trail; nothing gates actuation on it.
	headroom int
}

// Release returns the in-flight slots this lease holds. It is idempotent and nil-safe. It does NOT refund the
// rate budget: the actuation was admitted to the effect and is charged whether or not it succeeded.
func (a *ActuationLease) Release() {
	if a == nil || a.l == nil || a.done {
		return
	}
	a.done = true
	a.l.mu.Lock()
	defer a.l.mu.Unlock()
	for _, k := range a.keys {
		if a.l.inFlight[k] > 0 {
			a.l.inFlight[k]--
		}
		if a.l.inFlight[k] == 0 {
			delete(a.l.inFlight, k)
		}
	}
}

// Admit asks whether ONE more actuation may reach the effect for (session, target) right now. It returns
// either a lease (refusal == "") or a NON-EMPTY, operator-legible refusal naming the scope, the key, the
// count and the cap — the caller must be able to tell a throttled actuation from an unrelated failure, which
// is the whole point of surfacing the numbers.
//
// On refusal NOTHING is recorded: the refused actuation did not reach the effect, so it neither consumes the
// window nor holds a slot. On admission BOTH scopes are charged and BOTH in-flight counters are incremented
// atomically — a partial charge would let one scope drift out of step with the other.
//
// A nil receiver REFUSES. It is unreachable by construction (NewInterceptor always installs a limiter and
// WithActuationLimiter ignores nil), and it refuses rather than passes precisely so that if some future
// construction path does leave it nil, the hole shows up as a refused mutation instead of an ungoverned one.
func (l *ActuationLimiter) Admit(session, target string) (*ActuationLease, string) {
	if l == nil {
		return nil, "actuation limiter is not wired — refusing (fail closed: an ungoverned actuation rate is the TG-166 defect)"
	}
	sKey, tKey := scopeKey("session", session), scopeKey("target", target)

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.limits.Window)

	// Prune first so the counts below are the true trailing-window counts, then check EVERY scope before
	// mutating anything (an all-or-nothing admission).
	sEvents := pruneAtOrBefore(l.admitted[sKey], cutoff)
	tEvents := pruneAtOrBefore(l.admitted[tKey], cutoff)
	l.admitted[sKey], l.admitted[tKey] = sEvents, tEvents

	for _, c := range []struct {
		scope, key       string
		inFlight, cap    int
		count, rateLimit int
	}{
		{"session", session, l.inFlight[sKey], l.limits.SessionInFlight, len(sEvents), l.limits.SessionPerWindow},
		{"target", target, l.inFlight[tKey], l.limits.TargetInFlight, len(tEvents), l.limits.TargetPerWindow},
	} {
		shown := c.key
		if strings.TrimSpace(shown) == "" {
			shown = unattributed
		}
		if c.inFlight >= c.cap {
			l.totalRefused++
			return nil, fmt.Sprintf("%s: %s %q already has %d actuation(s) in flight (cap %d) — refusing to run a second mutation concurrently against the same %s",
				RefusalRateLimited, c.scope, shown, c.inFlight, c.cap, c.scope)
		}
		if c.count >= c.rateLimit {
			l.totalRefused++
			return nil, fmt.Sprintf("%s: %s %q has already actuated %d time(s) in the trailing %s (cap %d) — refusing; this is a throttle, not an execution failure, and the action was NOT run",
				RefusalRateLimited, c.scope, shown, c.count, l.limits.Window, c.rateLimit)
		}
	}

	l.admitted[sKey] = append(sEvents, now)
	l.admitted[tKey] = append(tEvents, now)
	l.inFlight[sKey]++
	l.inFlight[tKey]++
	l.totalAdmitted++
	// Rate-budget slack remaining AFTER this admission (charged above), tightest of the two scopes. Both
	// terms are >= 0 here: the loop already refused any scope whose count had reached its per-window cap, so
	// the pre-charge count is at most cap−1 and (cap − (count+1)) >= 0. TG-178 observe-only margin.
	headroom := l.limits.SessionPerWindow - len(sEvents) - 1
	if th := l.limits.TargetPerWindow - len(tEvents) - 1; th < headroom {
		headroom = th
	}
	return &ActuationLease{l: l, keys: []string{sKey, tKey}, headroom: headroom}, ""
}

// pruneAtOrBefore drops timestamps at or before cutoff (outside the trailing window). It reuses the input's
// backing array; the caller holds the lock and reassigns the slice, so the aliasing is safe.
//
// It is the same shape as core/policy's pruneBefore, and duplicated rather than shared for a boring reason:
// that one is UNEXPORTED. This package does import core/policy (for the PolicyDecider seam's EvalInput), so
// the dependency direction would allow reuse — there is simply nothing exported to reuse, and exporting a
// four-line slice helper across a package boundary to save four lines would be the worse trade.
func pruneAtOrBefore(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
