package safety

// THE READ LANE HAD NO VOLUME BOUND AT ALL (TG-165).
//
// TG's advisory/triage/read lane fails OPEN by design (docs/CONSTITUTION.md §3.3; core/territory rules
// "read-only action — never gated"). That is correct as a FAIL DIRECTION — an investigation that errors must
// degrade to pre-feature behaviour, never deny — and it was silently doing a second job it was never
// designed for: standing in for a VOLUME bound. Measured 2026-08-04, before this file existed:
//
//   - no read counter fed any kill path. The only throttles on the whole read lane were per-invocation
//     (a `tail -n`, a `grep -m`, a byte cap, a context deadline) plus $-spend (core/cost). Dollars are not
//     scope: a thousand cheap enumeration probes cost less than one long reasoning cycle;
//   - `/halt` flipped the MUTATION chokepoint only (ForceShadow drops the mode to Shadow). Recon ran
//     straight THROUGH a halt — the operator's kill switch stopped the half that was already off in Shadow
//     and did nothing to the half an intruder actually wants;
//   - the anti-thrash veto (agent/trajectory.go) keys on identical (tool, ArgsKey), so ten DISTINCT probes
//     per session were free, and there was NO cross-session bound of any kind: a retried session mints a
//     fresh id (agent/session.go) and therefore a fresh everything.
//
// A hijacked-but-read-only TG could therefore enumerate the estate at full rate entirely within policy —
// the pre-actuation half of the ATLAS Discovery/Lateral chain, and the half where the July-2026 HuggingFace
// intrusion did most of its damage before any destructive write. docs/THREAT-MODEL.md §5.2 names the class.
//
// WHAT THIS IS AND IS NOT. It is a VOLUME meter with three bounds and one alarm. It is NOT a policy gate:
// it never decides whether a particular read is allowed (territory/policy/plane-split already answer that),
// only whether THIS MANY reads in THIS WINDOW may still be served. Every bound is sized so a real
// investigation cannot reach it (see the constants), and every refusal is EXPLICIT — a typed *ReconRefusal
// naming the bound, the count and the window — because the failure mode to avoid here is not a refused read,
// it is a triage that quietly returns less and reads as "nothing found". Under-triage is the worse failure
// (INV-08's standing preference), so the meter is loud rather than tight.
//
// Provenance: [O] TG-165 (dual-use / TG-as-offensive-agent), building on TG-297 (per-session identity),
// TG-166 (ToolResult.Target — the estate object a call was made against) and TG-295 (agent_step_evidence,
// the durable record of what was read). OWASP Agentic ASI10; MITRE ATLAS AML.T0098/T0025.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The default bounds. Every one of them is deliberately ABOVE what this estate's own recorded history
// produces, because a recon bound that ever truncates a real investigation would be removed within a week —
// and an uninstalled control protects nobody.
const (
	// DefaultReconPerSession is how many estate reads ONE investigation may dispatch.
	//
	// The ReAct loop's own hard cycle limit is 10 (agent.DefaultLimits), so a session cannot reach 25 by
	// running the loop: this is deliberately a BACKSTOP, not the operative bound on the loop. It binds the
	// read paths the cycle limit does not govern — a deployment that raises the limits, a tool that fans out
	// internally, or any future non-ReAct reader that takes a session id. Stated plainly so nobody later
	// reads it as the session's real ceiling: for the shipped loop it is un-hittable by construction, and
	// that is the intent.
	DefaultReconPerSession = 25

	// DefaultReconPerHour is how many estate reads ALL sessions together may dispatch in a rolling hour.
	// This is the bound that did not exist in any form: every previous limit was per-call or per-session,
	// and a fresh session was one Temporal retry away.
	//
	// Sized against the corpus this system actually produced: 3241 sessions / 17759 recorded steps as of
	// 2026-08-03 (migration 0053), i.e. ~5.5 evidence-bearing reads per session. 500 reads/hour is ~90
	// full-cycle-budget investigations in one hour — an order of magnitude above anything this estate has
	// ever produced in an hour — while making enumeration (the thousands of probes a sweep needs)
	// structurally impossible.
	DefaultReconPerHour = 500

	// DefaultReconBurst is the RATE alarm: this many reads inside DefaultReconBurstWindow is a recon BURST,
	// a first-class anomaly rather than a busy hour. It fires the kill (ForceShadow) and refuses further
	// reads until the window drains.
	//
	// 150 reads in 5 minutes is ~15 concurrent full-budget investigations all starting at once, which this
	// estate does not do; the predecessor's own cross-host fan-out detector used the same 5-minute frame
	// (MECH-707). The window is short and SELF-DRAINING on purpose: a burst must not be a permanent
	// blinding, because the operator's answer to a burst may well be "investigate it", which needs reads.
	DefaultReconBurst = 150

	// DefaultReconBurstWindow is the burst frame.
	DefaultReconBurstWindow = 5 * time.Minute

	// DefaultReconFanoutObserve is the per-session distinct-target count at/above which the read lane raises an
	// OBSERVE-ONLY fan-out flag (TG-325). It catches the SHAPE the volume bounds cannot: an actor operating
	// UNDER the per-session read cap by touching many DISTINCT hosts a few times each — a methodical estate
	// sweep whose total read count is unremarkable but whose composition is not. It NEVER refuses a read and
	// NEVER forces Shadow (unlike the burst alarm): a shape signal must earn its way in on live evidence before
	// it is allowed to act, exactly as TG-165's volume bounds shipped observe-first. 12 distinct estate objects
	// in one investigation is broad relative to a normal triage (its alert host plus a few neighbours) while
	// well under the 25-read per-session cap, so a session reading 12 distinct hosts once each never trips a
	// volume bound but is exactly the sweep this flag is for.
	DefaultReconFanoutObserve = 12

	// reconHourWindow is the rolling frame for PerHour. Fixed rather than configurable: "per hour" is the
	// unit the bound is named and reasoned in, and a second knob that silently redefines the first is how a
	// bound ends up meaning something nobody expects.
	reconHourWindow = time.Hour
)

// ReconBudget is the set of volume bounds on the read lane. A non-positive field means "use the default" —
// never "unlimited". There is deliberately NO value that disables a bound: an operator who blanks or
// fat-fingers a key gets the sane bound back, matching the convention core/db and the syslog-ng session cap
// already use. To run without a bound, do not wire a governor at all — that is a visible decision at the
// composition root, not a typo in a config store.
type ReconBudget struct {
	PerSession    int           // reads one investigation may dispatch
	PerHour       int           // reads ALL investigations together may dispatch in a rolling hour
	Burst         int           // reads inside BurstWindow that constitute a recon burst (the anomaly)
	BurstWindow   time.Duration // the burst frame
	FanoutObserve int           // per-session distinct-target count that raises an OBSERVE-ONLY fan-out flag; 0 disables (see sane)
}

// DefaultReconBudget returns the shipped bounds.
func DefaultReconBudget() ReconBudget {
	return ReconBudget{
		PerSession:    DefaultReconPerSession,
		PerHour:       DefaultReconPerHour,
		Burst:         DefaultReconBurst,
		BurstWindow:   DefaultReconBurstWindow,
		FanoutObserve: DefaultReconFanoutObserve,
	}
}

// sane replaces any non-positive GATING field with its default. See the ReconBudget doc for why zero is not
// "unlimited" for a gate. FanoutObserve is the ONE exception: it is OBSERVE-ONLY (it never refuses a read), so
// 0 legitimately means "do not raise the fan-out flag" — disabling an observation is not disabling a guard, so
// unlike the gates it is not forced back to a default. A negative value clamps to 0 (off).
func (b ReconBudget) sane() ReconBudget {
	d := DefaultReconBudget()
	if b.PerSession <= 0 {
		b.PerSession = d.PerSession
	}
	if b.PerHour <= 0 {
		b.PerHour = d.PerHour
	}
	if b.Burst <= 0 {
		b.Burst = d.Burst
	}
	if b.BurstWindow <= 0 {
		b.BurstWindow = d.BurstWindow
	}
	if b.FanoutObserve < 0 {
		b.FanoutObserve = 0
	}
	return b
}

// ErrReconRefused is the sentinel every recon refusal matches under errors.Is, so a caller can distinguish
// "the read lane is bounded and said so" from any other error without depending on the message text.
var ErrReconRefused = errors.New("safety: estate read refused by the recon budget")

// ReconRefusal is the typed refusal. It carries the bound that fired, the count that reached it and the
// window it was measured over, because the ONE thing this control must never do is make an investigation
// look empty. The message is written to be read by a model (which must conclude from what it already has)
// and by an operator (who must be able to tell a bound from a broken tool).
type ReconRefusal struct {
	Bound   string        // "session" | "hour" | "burst" | "halt"
	Session string        // the investigation that was refused ("" ⇒ the shared unstamped bucket)
	Count   int           // reads already served in the window (0 for a halt)
	Limit   int           // the bound that was reached (0 for a halt)
	Window  time.Duration // the frame Count was measured over (0 for the per-session bound and for a halt)
	Reason  string        // the halt reason, for Bound=="halt"
}

// Error states the bound, the evidence for it, and the consequence — an INCOMPLETE investigation, never an
// empty one.
func (r *ReconRefusal) Error() string {
	switch r.Bound {
	case "halt":
		return fmt.Sprintf("estate reads are HALTED (%s) — the read lane is stopped, so this investigation is "+
			"INCOMPLETE, not empty: conclude from the evidence already gathered and say that reads were halted", r.Reason)
	case "session":
		return fmt.Sprintf("estate read refused: this investigation has already made %d reads and its per-session "+
			"recon budget is %d — no further reads will be served to it. The investigation is INCOMPLETE, not empty: "+
			"conclude from the evidence already gathered and say that the read budget was reached", r.Count, r.Limit)
	default:
		return fmt.Sprintf("estate read refused: %d reads in the last %s across all investigations reached the %s "+
			"recon bound of %d — reads resume as that window drains. The investigation is INCOMPLETE, not empty: "+
			"conclude from the evidence already gathered and say that the read budget was reached",
			r.Count, r.Window, r.Bound, r.Limit)
	}
}

// Is makes every refusal match ErrReconRefused.
func (r *ReconRefusal) Is(target error) bool { return target == ErrReconRefused }

// ReconLedger is the durable record of reads the governor SEEDS its rolling hour from at boot:
// agent_step_evidence (TG-295), which holds one row per recorded read with its created_at.
// core/db.AgentStepEvidenceStore satisfies it. It is an interface so the safety core keeps no database
// import, and so the seed is testable without Postgres.
type ReconLedger interface {
	// ReadsSince returns the timestamps of the reads recorded at or after `since`, oldest first.
	ReadsSince(ctx context.Context, since time.Time) ([]time.Time, error)
}

// reconRead is one metered read: when it happened, whose session it belonged to, and what estate object it
// was aimed at (TG-166's Target). The target is what makes a burst legible as RECON — 150 reads against one
// host is a poll, 150 reads against 150 hosts is a sweep — and it is reported, never gated on.
type reconRead struct {
	at      time.Time
	session string
	target  string
}

// reconSession is one investigation's consumed read budget. lastUsed drives the idle sweep only.
type reconSession struct {
	reads     int
	targets   map[string]struct{}
	lastUsed  time.Time
	fanoutHot bool // the observe-only fan-out flag has already fired for this session (fire ONCE, not per read)
}

// ReconGovernor meters the read lane and is the seam by which a recon burst reaches the kill switch.
//
// It is nil-safe throughout: a nil governor admits everything and records nothing, so a caller that has not
// been wired behaves exactly as it did before TG-165. That is a deliberate compromise with house rule 5 (no
// behaviour change for existing deployments without a safe default) and it is why the composition root's
// wiring is pinned by its own oracle — an unwired bound is this repository's most-repeated defect, so the
// counter it publishes (tg_recon_reads_total) is the thing to look at when asking whether it binds at all.
type ReconGovernor struct {
	budget ReconBudget
	kill   ShadowForcer                 // the /halt seam: a burst forces the mode to Shadow. nil ⇒ meter only.
	now    func() time.Time             // swappable clock so the windows are testable without sleeping
	logf   func(string, ...interface{}) // nil ⇒ silent (tests); the worker passes log.Printf

	mu       sync.Mutex
	window   []reconRead              // the rolling hour, pruned on every touch
	sessions map[string]*reconSession // per-investigation spend
	halted   bool                     // operator/anomaly halt — one-directional, never re-enabling
	haltWhy  string
	burstHot bool // a burst episode is open; used so the alarm fires ONCE per episode, not per read

	reads       int64 // metered reads (the counter that proves this thing counts at all)
	refusals    int64 // refused reads, by any bound
	bursts      int64 // burst episodes opened
	fanoutFlags int64 // observe-only per-session fan-out flags raised (a sweep under the volume bound), TG-325
}

// ReconOption configures a governor without widening the constructor.
type ReconOption func(*ReconGovernor)

// WithReconClock swaps the clock (tests drive the windows without sleeping an hour).
func WithReconClock(now func() time.Time) ReconOption {
	return func(g *ReconGovernor) {
		if now != nil {
			g.now = now
		}
	}
}

// WithReconLogf gives the governor a logger. A burst is an anomaly an operator must see in the log even if
// nobody is scraping /metrics at that minute.
func WithReconLogf(logf func(string, ...interface{})) ReconOption {
	return func(g *ReconGovernor) { g.logf = logf }
}

// NewReconGovernor builds the read-lane meter over a budget and the kill seam a burst drives.
//
// kill may be nil (meter-only, e.g. the grounder, which never actuates and holds a read-only chokepoint);
// *Chokepoint satisfies ShadowForcer.
func NewReconGovernor(b ReconBudget, kill ShadowForcer, opts ...ReconOption) *ReconGovernor {
	g := &ReconGovernor{
		budget:   b.sane(),
		kill:     kill,
		now:      time.Now,
		sessions: map[string]*reconSession{},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Budget returns the bounds in force (for the boot log and the console posture read).
func (g *ReconGovernor) Budget() ReconBudget {
	if g == nil {
		return ReconBudget{}
	}
	return g.budget
}

// Admit answers "may this investigation dispatch one more estate read?" — consulted BEFORE the read, so a
// refused read never reaches the estate. It returns nil, or a *ReconRefusal that says exactly which bound
// fired and what the caller must do about it.
//
// Order of checks is halt → session → hour → burst: the most specific and most permanent reason wins, so a
// halted worker never reports "busy hour" for what is actually a kill switch.
func (g *ReconGovernor) Admit(session string) error {
	if g == nil {
		return nil // unwired ⇒ pre-TG-165 behaviour (see the type doc)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.prune(now)

	if g.halted {
		g.refusals++
		return &ReconRefusal{Bound: "halt", Session: session, Reason: g.haltWhy}
	}
	if s := g.sessions[session]; s != nil && s.reads >= g.budget.PerSession {
		g.refusals++
		return &ReconRefusal{Bound: "session", Session: session, Count: s.reads, Limit: g.budget.PerSession}
	}
	if n := len(g.window); n >= g.budget.PerHour {
		g.refusals++
		return &ReconRefusal{Bound: "hour", Session: session, Count: n, Limit: g.budget.PerHour, Window: reconHourWindow}
	}
	if n := g.countSince(now.Add(-g.budget.BurstWindow)); n >= g.budget.Burst {
		g.refusals++
		return &ReconRefusal{Bound: "burst", Session: session, Count: n, Limit: g.budget.Burst, Window: g.budget.BurstWindow}
	}
	return nil
}

// Record meters one DISPATCHED estate read: the session that made it, the tool, and the estate object it was
// aimed at (TG-166's Target; "" when the call named none).
//
// It is called for every dispatch, INCLUDING one whose tool then errored or returned nothing — that is the
// difference between this meter and the agent_step_evidence corpus it is seeded from, and it matters: a probe
// that finds nothing writes no evidence row, and "returned nothing" is precisely what most enumeration probes
// look like. The durable table can therefore only ever UNDER-report recon; the live meter counts the attempt.
//
// Crossing the burst bound opens a burst episode: the alarm fires ONCE (ForceShadow + a log line), not on
// every subsequent read, so a sustained burst does not drown the log or re-kill an already-killed mode.
func (g *ReconGovernor) Record(session, tool, target string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	now := g.now()
	g.prune(now)
	g.window = append(g.window, reconRead{at: now, session: session, target: target})
	g.reads++
	s := g.sessions[session]
	if s == nil {
		s = &reconSession{targets: map[string]struct{}{}}
		g.sessions[session] = s
	}
	s.reads++
	s.lastUsed = now
	if target != "" {
		s.targets[target] = struct{}{}
	}
	// OBSERVE-ONLY fan-out flag (TG-325): a session that reaches the distinct-target ceiling is touching many
	// DIFFERENT estate objects — a methodical sweep whose composition is anomalous even when its total read
	// count is under every volume bound. Fire ONCE per session (like the burst episode), count it, and log it.
	// It NEVER refuses a read and NEVER forces Shadow: a shape signal reports, it does not gate — the volume
	// bounds above are the guards; this earns its way toward acting on live evidence first.
	fanoutFire := false
	var fanoutN int
	if g.budget.FanoutObserve > 0 && !s.fanoutHot && len(s.targets) >= g.budget.FanoutObserve {
		s.fanoutHot = true
		g.fanoutFlags++
		fanoutFire = true
		fanoutN = len(s.targets)
	}
	// The burst test runs on the SAME lock and the same pruned window, so the number in the alarm is the
	// number the next Admit will refuse on — an alarm that cites a different count than the refusal is an
	// alarm an operator cannot act on.
	n := g.countSince(now.Add(-g.budget.BurstWindow))
	fire := false
	var hosts int
	if n >= g.budget.Burst {
		if !g.burstHot {
			g.burstHot = true
			g.bursts++
			fire = true
			hosts = g.distinctTargetsSince(now.Add(-g.budget.BurstWindow))
		}
	} else {
		g.burstHot = false
	}
	kill, logf, win := g.kill, g.logf, g.budget.BurstWindow
	burstLimit := g.budget.Burst
	fanoutCeiling := g.budget.FanoutObserve
	g.mu.Unlock()

	// Observe-only fan-out notice — logged (never gated), independent of the burst path below.
	if fanoutFire && logf != nil {
		logf("safety: RECON FAN-OUT — session %s reached %d distinct estate targets (observe ceiling %d) — "+
			"observe-only, a possible methodical sweep UNDER the volume bounds; reported, not refused (TG-325)",
			session, fanoutN, fanoutCeiling)
	}

	if !fire {
		return
	}
	// THE ANOMALY FEEDS THE KILL SWITCH. A recon burst is the pre-actuation half of the attack chain, so the
	// posture it must change is the actuating one: force the mode to Shadow, exactly as an operator's POST
	// /halt does. ForceShadow is safe, idempotent, one-directional and never refused — it can only make the
	// posture more restrictive, so firing it on a false positive costs an operator a mode escalation, while
	// NOT firing it on a true positive costs the estate. Reads are refused too, but only until the burst
	// window drains (see Admit): a burst must not be a permanent blinding.
	reason := fmt.Sprintf("recon burst: %d estate reads across %d distinct targets in %s (bound %d) — read-lane anomaly, TG-165",
		n, hosts, win, burstLimit)
	if logf != nil {
		logf("safety: RECON BURST — %s; forcing the mode to Shadow and refusing further reads until the window drains", reason)
	}
	if kill != nil {
		kill.ForceShadow(reason)
	}
}

// Halt stops the READ lane — the half `/halt` never reached. It is the mirror of ForceShadow and holds the
// same contract: safe (only ever more restrictive), idempotent, never refused, and NEVER re-enabling. The
// operator's kill switch calls both, so one POST stops mutation AND recon; before TG-165 a halted worker
// went on enumerating the estate at full rate.
func (g *ReconGovernor) Halt(reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	first := !g.halted
	g.halted = true
	if g.haltWhy == "" {
		g.haltWhy = reason
	}
	logf := g.logf
	g.mu.Unlock()
	if first && logf != nil {
		logf("safety: READ LANE HALTED — %s; every further estate read is refused with an explicit reason (TG-165)", reason)
	}
}

// Halted reports whether the read lane is stopped (the /metrics posture read).
func (g *ReconGovernor) Halted() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.halted
}

// SeedFromLedger pre-loads the rolling hour from the durable read record (agent_step_evidence) so a RESTART
// does not hand whatever was mid-burst a brand-new hour. It returns how many reads were adopted.
//
// The seed is a FLOOR, not the truth: only reads that produced a payload leave a row (see Record), so the
// table under-reports. Under-reporting after a restart is the safe direction — it can only ever admit reads
// a perfect meter would have admitted, and this control's stated failure preference is under-binding over
// blinding a real investigation. A ledger error is returned, never swallowed: booting with an unseeded
// window is a decision for the caller to log, not a silence.
func (g *ReconGovernor) SeedFromLedger(ctx context.Context, l ReconLedger) (int, error) {
	if g == nil || l == nil {
		return 0, nil
	}
	now := g.now()
	ats, err := l.ReadsSince(ctx, now.Add(-reconHourWindow))
	if err != nil {
		return 0, fmt.Errorf("safety: seeding the recon window from the evidence ledger: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, at := range ats {
		if at.After(now) {
			at = now // a clock-skewed row must not sit in the future and never drain
		}
		g.window = append(g.window, reconRead{at: at, session: "<seeded>"})
		n++
	}
	g.prune(now)
	return n, nil
}

// ReconSnapshot is the read-lane posture for /metrics and the console: what the meter has counted, what it
// refused, and how hot the windows are. Fan-out (distinct estate objects touched in the last hour) is
// REPORTED, never gated on — it is the number that tells an operator whether a busy hour was one host being
// polled or the estate being swept.
type ReconSnapshot struct {
	Reads         int64
	Refusals      int64
	Bursts        int64
	FanoutFlags   int64 // observe-only per-session fan-out flags raised (TG-325); 0 when the ceiling is off
	Halted        bool
	ReadsHour     int
	ReadsBurst    int
	TargetsHour   int
	PerHourLimit  int
	BurstLimit    int
	SessionLimit  int
	FanoutObserve int // the per-session distinct-target ceiling in force (0 ⇒ the fan-out flag is disabled)
	LiveSessions  int
	BurstWindowIs time.Duration
}

// Snapshot returns the current read-lane posture. Read-only; it prunes the windows as any other touch does.
func (g *ReconGovernor) Snapshot() ReconSnapshot {
	if g == nil {
		return ReconSnapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.prune(now)
	return ReconSnapshot{
		Reads:         g.reads,
		Refusals:      g.refusals,
		Bursts:        g.bursts,
		FanoutFlags:   g.fanoutFlags,
		Halted:        g.halted,
		ReadsHour:     len(g.window),
		ReadsBurst:    g.countSince(now.Add(-g.budget.BurstWindow)),
		TargetsHour:   g.distinctTargetsSince(now.Add(-reconHourWindow)),
		PerHourLimit:  g.budget.PerHour,
		BurstLimit:    g.budget.Burst,
		SessionLimit:  g.budget.PerSession,
		FanoutObserve: g.budget.FanoutObserve,
		LiveSessions:  len(g.sessions),
		BurstWindowIs: g.budget.BurstWindow,
	}
}

// prune drops reads older than the rolling hour and forgets sessions idle for longer than that. Callers hold
// the lock.
//
// A session's counter is dropped only after an hour of INACTIVITY, never on a timer: a sweep that could
// clear a LIVE session's spend would hand the agent a fresh budget mid-investigation, which is a bound that
// silently stops binding — the exact failure this file exists to remove.
func (g *ReconGovernor) prune(now time.Time) {
	cut := now.Add(-reconHourWindow)
	keep := 0
	for _, r := range g.window {
		if r.at.After(cut) {
			g.window[keep] = r
			keep++
		}
	}
	// Release the tail so a drained window does not pin an hour of peak-rate reads forever.
	for i := keep; i < len(g.window); i++ {
		g.window[i] = reconRead{}
	}
	g.window = g.window[:keep]
	for id, s := range g.sessions {
		if s.lastUsed.Before(cut) {
			delete(g.sessions, id)
		}
	}
}

// countSince counts metered reads at or after `from`. Callers hold the lock.
func (g *ReconGovernor) countSince(from time.Time) int {
	n := 0
	for _, r := range g.window {
		if !r.at.Before(from) {
			n++
		}
	}
	return n
}

// distinctTargetsSince counts distinct non-empty estate objects read since `from` — the fan-out signal.
// Callers hold the lock.
func (g *ReconGovernor) distinctTargetsSince(from time.Time) int {
	seen := map[string]struct{}{}
	for _, r := range g.window {
		if r.target != "" && !r.at.Before(from) {
			seen[r.target] = struct{}{}
		}
	}
	return len(seen)
}
