package policy

// This file implements spec/015 task T-015-4: the four global autonomy modes and their gated + audited
// transitions (REQ-1500, REQ-1502, REQ-1519). It builds the mode state machine STANDALONE — it is NOT
// yet wired to the real actuation chokepoint and it does NOT touch core/safety.MutationGate or
// TG_MUTATION_ENABLED. The absorption of the retired gate into this mode (REQ-1520/1521) is the separate
// safety-core refactor T-015-13; here MayAutoActuate is a pure, tested predicate that T-015-13 will wire.
//
// Two fail-closed properties live in the mode by construction (INV-09):
//  1. The ZERO VALUE of Mode is ModeShadow — the most restrictive, read-only mode — so an un-initialised,
//     absent, or corrupt persisted mode resolves to Shadow (suggest-only), never to an actuating mode.
//  2. A transition INTO an actuating mode (Semi-auto / Full-auto) is gated on the spec/013 green preflight;
//     a red preflight refuses the escalation and leaves the mode unchanged.
//
// Provenance: [R] paradigm-rule 4 (four-mode ladder) · [O] INV-09 (fail closed), INV-19 (audited
// transitions). See spec/015-policy-engine requirements.md REQ-1500/1502/1519/1520.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// Mode is the four-mode global autonomy state machine (REQ-1500). It is a CLOSED enum: exactly one mode is
// active at any instant, and the ZERO VALUE is ModeShadow so any un-initialised or corrupt value fails
// closed to read-only. The mode governs ONLY the actuation branch (REQ-1501) — it never alters the
// investigation pipeline (ingest → reason → rationale → propose → risk-classify).
type Mode int

const (
	// ModeShadow (the zero value, fail-closed) — read-only: the engine logs, suggests, and records
	// rationale but NEVER actuates. A default, absent, or corrupt persisted mode resolves here (REQ-1519).
	ModeShadow Mode = iota
	// ModeHITL — human-in-the-loop: the engine is off and EVERY candidate action routes to a human vote;
	// nothing auto-executes.
	ModeHITL
	// ModeSemiAuto — the engine is on and decides auto / approve / deny per action; auto is permitted ONLY
	// for op-classes the graduation engine (T-015-8) has promoted, everything else routes to approval.
	ModeSemiAuto
	// ModeFullAuto — the engine is on and applies the auto-approve preset: any allow/auto verdict actuates,
	// except the constitutional never-auto floor (INV-09) which still clamps beneath the engine.
	ModeFullAuto
)

// String renders the canonical mode name (the wire + ledger spelling). An out-of-range value renders as
// Shadow — fail closed — so a corrupt persisted int is never displayed (or treated) as an actuating mode.
func (m Mode) String() string {
	switch m {
	case ModeHITL:
		return "HITL"
	case ModeSemiAuto:
		return "Semi-auto"
	case ModeFullAuto:
		return "Full-auto"
	default:
		return "Shadow" // covers ModeShadow and any invalid value → fail closed
	}
}

// valid reports whether m is one of the four closed-enum modes. Used to reject a corrupt persisted value.
func (m Mode) valid() bool {
	switch m {
	case ModeShadow, ModeHITL, ModeSemiAuto, ModeFullAuto:
		return true
	default:
		return false
	}
}

// ParseMode resolves a canonical mode name to a Mode, failing closed: an unknown string returns
// (ModeShadow, error) so a caller that ignores the error still gets the safe read-only mode.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "Shadow":
		return ModeShadow, nil
	case "HITL":
		return ModeHITL, nil
	case "Semi-auto":
		return ModeSemiAuto, nil
	case "Full-auto":
		return ModeFullAuto, nil
	default:
		return ModeShadow, fmt.Errorf("%w: unknown mode %q", ErrUnknownMode, s)
	}
}

// MayAutoActuate is the PURE actuation-branch predicate (REQ-1500/1520): it answers "may an action
// auto-execute while this mode is active?". True ONLY for Semi-auto and Full-auto; Shadow and HITL never
// auto-actuate. In Semi-auto this is the mode-level gate — the per-op-class graduation gate (T-015-8) is
// layered ABOVE it, so a Semi-auto class not yet promoted still routes to approval. This is the predicate
// T-015-13 will wire to the real chokepoint in place of the retired MutationGate.Enabled(); here it is a
// standalone, tested function and is NOT wired into core/actuate.
func (m Mode) MayAutoActuate() bool {
	return m == ModeSemiAuto || m == ModeFullAuto
}

// RequiresHumanVote reports whether the mode routes EVERY candidate action to a human approval vote. True
// only for HITL. Shadow suggests only (it does not route to a vote); Semi-auto routes non-graduated actions
// to a vote per-class (not "every" action, so false at the mode level); Full-auto routes none.
func (m Mode) RequiresHumanVote() bool { return m == ModeHITL }

// ---------------------------------------------------------------------------------------------------------
// Seams (interfaces + fakes). The real RBAC, boot preflight, and durable pgx store are LATER leaves; this
// leaf carries them as narrow interfaces so the state machine is testable without a DB or an auth backend.
// ---------------------------------------------------------------------------------------------------------

var (
	// ErrUnknownMode is returned when a mode name/value is not one of the four closed-enum modes.
	ErrUnknownMode = errors.New("policy: unknown mode")
	// ErrModeAbsent is returned by a ModeStore whose active mode has never been persisted. The controller
	// treats it as fail-closed → Shadow (REQ-1519).
	ErrModeAbsent = errors.New("policy: persisted mode absent")
	// ErrUnauthorizedModeChange is returned when the acting principal is not an authenticated operator with
	// mode-change authority (REQ-1502).
	ErrUnauthorizedModeChange = errors.New("policy: actor lacks mode-change authority")
	// ErrPreflightNotGreen is returned when a transition INTO Semi-auto or Full-auto is attempted while the
	// spec/013 boot preflight is not green (REQ-1520). The escalation fails closed; the mode is unchanged.
	ErrPreflightNotGreen = errors.New("policy: cannot escalate mode — boot preflight is not green")
	// ErrStaleMode is returned when a Transition's declared `from` no longer matches the active mode — a
	// concurrent transition already moved it. The caller must re-read and retry (compare-and-swap).
	ErrStaleMode = errors.New("policy: stale from-mode — active mode changed concurrently")
)

// AuthorityChecker verifies that an actor is an authenticated operator holding mode-change authority
// (REQ-1502). It is the seam onto the real RBAC/auth surface (spec/006); this leaf does NOT build RBAC —
// tests inject a fake. HasModeChangeAuthority returns nil to admit the actor, else a non-nil error.
type AuthorityChecker interface {
	HasModeChangeAuthority(ctx context.Context, actor string) error
}

// PreflightChecker reports whether the spec/013 boot preflight is green (the interception chain is wired).
// It is the seam onto the real preflight prover; a transition INTO Semi-auto or Full-auto is gated on it
// (REQ-1520). PreflightGreen returns nil when green, else a non-nil error (escalation refused, fail closed).
type PreflightChecker interface {
	PreflightGreen(ctx context.Context) error
}

// ModeStore persists the single active mode. The durable pgx impl + migration is a later leaf (T-015-12);
// this leaf ships only the in-memory MemModeStore for oracles. Load returns ErrModeAbsent (or any error)
// when the mode cannot be read — the controller resolves that to Shadow (fail closed, REQ-1519).
type ModeStore interface {
	Load(ctx context.Context) (Mode, error)
	Save(ctx context.Context, m Mode) error
}

// ledgerAppender is the narrow slice of core/audit.Ledger the controller depends on: append ONE immutable,
// hash-chained governance record. *audit.Ledger satisfies it — the controller REUSES the existing
// tamper-evident ledger (spec/006) and does not rebuild an audit trail.
type ledgerAppender interface {
	Append(audit.GovDecision) (audit.LedgerEntry, error)
}

// BreakerRearmer clears a tripped deviation breaker as part of an owner-gated escalation back into an
// actuating mode (spec/015 REQ-1523). It is the RECOVERY counterpart to the breaker→Shadow trip (REQ-1520):
// a deviation trip forces Shadow, and the ONLY governed way out of Shadow is a deliberate operator mode
// transition — so THAT transition is where the breaker is re-armed. Without this, a single (possibly false)
// trip leaves the breaker durably open and every actuation refuses forever, even after the mode is restored,
// because the mode chokepoint and the breaker are independent gates. Optional and injected: a controller
// with no rearmer bound (a read-only console-side controller, or a worker with no armed breaker) simply
// skips the re-arm. The implementation lives in the worker (the single process holding the armed breaker,
// its shared store, and the ledger); core/policy depends only on this narrow interface (no import cycle).
type BreakerRearmer interface {
	// Rearm closes the deviation breaker and records the re-arm to the audit ledger. It returns an error
	// WITHOUT clearing the breaker when the audit append fails — the caller keeps the breaker open (fail-safe).
	Rearm(ctx context.Context) error
}

// MemModeStore is the in-memory ModeStore fake for oracle tests (CI has no DB). It reports ErrModeAbsent
// until a mode is saved, and can be primed with a load error to exercise the "unreadable → Shadow" path.
type MemModeStore struct {
	mu      sync.Mutex
	mode    Mode
	set     bool
	loadErr error
}

// NewMemModeStore returns an empty in-memory store (no mode persisted yet → Load fails closed).
func NewMemModeStore() *MemModeStore { return &MemModeStore{} }

// WithLoadError primes the store to fail every Load with err (to test the "unreadable → Shadow" path).
func (s *MemModeStore) WithLoadError(err error) *MemModeStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
	return s
}

// Load returns the persisted mode, or ErrModeAbsent when none has been saved / a primed error.
func (s *MemModeStore) Load(_ context.Context) (Mode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ModeShadow, s.loadErr
	}
	if !s.set {
		return ModeShadow, ErrModeAbsent
	}
	return s.mode, nil
}

// Save persists m as the active mode.
func (s *MemModeStore) Save(_ context.Context, m Mode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode, s.set = m, true
	return nil
}

// ---------------------------------------------------------------------------------------------------------
// ModeController — exactly-one-active, gated + audited transitions, fail-closed to Shadow.
// ---------------------------------------------------------------------------------------------------------

// ModeController owns the single active mode and serializes every transition. Exactly one mode is active
// (REQ-1500); a read never observes a torn state. A transition is gated on an authenticated,
// authority-checked operator and, when escalating into an actuating mode, on the green preflight; it
// appends one immutable mode-transition record to the governance ledger BEFORE the new mode takes effect,
// then persists and activates it (REQ-1502). Any gate failure — unauthorized actor, red preflight, or a
// ledger-append failure — leaves the mode unchanged (fail closed).
type ModeController struct {
	mu        sync.RWMutex
	current   Mode
	store     ModeStore
	ledger    ledgerAppender
	authz     AuthorityChecker
	preflight PreflightChecker
	rearmer   BreakerRearmer // optional — re-arms the deviation breaker on an owner-gated actuating escalation (REQ-1523)
	logf      func(format string, args ...any)
}

// BindBreakerRearmer attaches the deviation-breaker re-armer used on an escalation into an actuating mode
// (REQ-1523). It is set once at worker wiring time — after the durable breaker + its shared store exist —
// before the controller serves any transition, so it needs no lock. A nil rearmer (never bound, or bound to
// nil) simply disables the re-arm: a controller with no armed breaker (the read-only console side) transitions
// exactly as before. It returns the controller for fluent wiring.
func (c *ModeController) BindBreakerRearmer(r BreakerRearmer) *ModeController {
	c.rearmer = r
	return c
}

// NewModeController builds a controller and resolves its initial mode from the store, failing closed to
// Shadow when the persisted mode is absent, unreadable, or corrupt (REQ-1519). ledger, authz, and
// preflight are required to perform a transition; store may be nil (in-memory only, starts Shadow). logf
// is optional (nil → silent).
func NewModeController(ctx context.Context, store ModeStore, ledger ledgerAppender, authz AuthorityChecker, preflight PreflightChecker, logf func(string, ...any)) *ModeController {
	c := &ModeController{store: store, ledger: ledger, authz: authz, preflight: preflight, logf: logf}
	c.current = c.resolveFromStore(ctx)
	return c
}

// resolveFromStore reads the persisted mode fail-closed: an absent, unreadable, or corrupt mode resolves to
// ModeShadow and is logged (REQ-1519). A nil store means in-memory only → Shadow.
func (c *ModeController) resolveFromStore(ctx context.Context) Mode {
	if c.store == nil {
		return ModeShadow
	}
	m, err := c.store.Load(ctx)
	if err != nil {
		c.log("mode: persisted mode unreadable (%v) — failing closed to Shadow", err)
		return ModeShadow
	}
	if !m.valid() {
		c.log("mode: persisted mode corrupt (%d) — failing closed to Shadow", int(m))
		return ModeShadow
	}
	return m
}

// ResolveMode re-reads the active mode from the store fail-closed and returns it WITHOUT mutating the
// controller's cached mode (a pure read for the "absent → Shadow" oracle). Current() returns the cached
// active mode set at construction / last successful transition.
func (c *ModeController) ResolveMode(ctx context.Context) Mode { return c.resolveFromStore(ctx) }

// Current returns the single active mode. Concurrent-safe; never observes a torn transition.
func (c *ModeController) Current() Mode {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

// Transition changes the active mode under the full gate (REQ-1502). It (a) validates `to` is a real mode
// and the actor holds mode-change authority; (b) confirms `from` still matches the active mode
// (compare-and-swap, so concurrent transitions serialize and none is lost); (c) when escalating INTO an
// actuating mode (Semi-auto / Full-auto) requires the green preflight (REQ-1520); (d) appends ONE immutable
// mode-transition record — prior mode, new mode, actor, reason — to the governance ledger BEFORE the new
// mode takes effect; and only THEN persists and activates it. Any failure leaves the mode unchanged (fail
// closed): an unauthorized actor, a red preflight, or a ledger-append failure appends nothing and changes
// nothing.
func (c *ModeController) Transition(ctx context.Context, from, to Mode, actor, reason string) error {
	if !to.valid() {
		return fmt.Errorf("%w: %d", ErrUnknownMode, int(to))
	}
	if c.authz == nil {
		return ErrUnauthorizedModeChange
	}
	if err := c.authz.HasModeChangeAuthority(ctx, actor); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorizedModeChange, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Compare-and-swap on the active mode: a stale `from` means a concurrent transition already moved it.
	if c.current != from {
		return fmt.Errorf("%w: from=%s but active=%s", ErrStaleMode, from, c.current)
	}

	// Escalation gate: entering an actuating mode requires the spec/013 green preflight (REQ-1520). A red
	// preflight refuses the escalation BEFORE any ledger record is written — fail closed, mode unchanged.
	if to.MayAutoActuate() {
		if c.preflight == nil {
			return ErrPreflightNotGreen
		}
		if err := c.preflight.PreflightGreen(ctx); err != nil {
			return fmt.Errorf("%w: %v", ErrPreflightNotGreen, err)
		}
	}

	// Append the immutable mode-transition record BEFORE the new mode takes effect (REQ-1502). Withheld is
	// true when the transition withholds autonomy (moving to a non-actuating mode). A ledger failure fails
	// the transition closed — the mode is not advanced onto an unrecorded change.
	if c.ledger == nil {
		return errors.New("policy: nil ledger — refusing an unaudited mode transition")
	}
	rec := audit.GovDecision{
		Decision: fmt.Sprintf("mode-transition:%s->%s", from, to),
		Reason:   fmt.Sprintf("actor=%s reason=%s", actor, reason),
		ActionID: fmt.Sprintf("mode-transition:%s->%s:%s", from, to, actor),
		Withheld: !to.MayAutoActuate(),
	}
	if _, err := c.ledger.Append(rec); err != nil {
		return fmt.Errorf("policy: mode transition audit failed, mode unchanged: %w", err)
	}

	// Only now persist + activate. A persistence failure leaves the in-memory mode unchanged (fail closed);
	// the appended ledger record stands as evidence of the attempted, unactivated transition.
	if c.store != nil {
		if err := c.store.Save(ctx, to); err != nil {
			return fmt.Errorf("policy: mode transition persist failed, mode unchanged: %w", err)
		}
	}
	c.current = to
	c.log("mode: transition %s -> %s by %s (%s)", from, to, actor, reason)

	// Owner-gated breaker recovery (REQ-1523): escalating INTO an actuating mode (Semi-auto / Full-auto) is
	// the operator's deliberate "resume actuation" decision, so it re-arms a deviation breaker a prior trip
	// left durably open — otherwise the restored mode is nullified by the independent open breaker and every
	// actuation keeps refusing (the "one false trip permanently kills actuation" gap). It runs AFTER the mode
	// is audited + activated, and is BEST-EFFORT: a re-arm failure leaves the breaker OPEN (fail-safe —
	// actuation stays halted, never half-enabled) and is logged, but never unwinds the recorded, activated
	// transition. Same escalation predicate as the preflight gate above, so it fires only on the already
	// most-gated direction; a transition to Shadow/HITL, or one with no rearmer bound, never touches it.
	if to.MayAutoActuate() && c.rearmer != nil {
		if err := c.rearmer.Rearm(ctx); err != nil {
			c.log("mode: deviation-breaker re-arm on escalation to %s FAILED (breaker stays open, actuation still halted): %v", to, err)
		} else {
			c.log("mode: deviation breaker re-armed on escalation to %s by %s", to, actor)
		}
	}
	return nil
}

// SeedInitialMode establishes the deploy-time initial mode on a FRESH deployment — the mode analogue of the
// curated ruleset/graduation seed (SeedDefaults). It is ABSENT-ONLY and fail-closed:
//   - it applies `configured` ONLY when NO mode has ever been persisted (store.Load reports ErrModeAbsent);
//     an operator-set or previously-seeded mode is NEVER overridden (so it is a no-op on an existing estate);
//   - an unset/invalid/Shadow `configured`, a nil store (in-memory), or any store/ledger error leaves the
//     mode at its fail-closed Shadow default;
//   - seeding INTO an actuating mode (Semi-auto / Full-auto) still requires the spec/013 green preflight
//     (REQ-1520) exactly as a runtime escalation does — a red/absent preflight refuses the seed and stays
//     Shadow — and the establishment is appended to the governance ledger BEFORE it takes effect (REQ-1502).
// Authority is the deployer's control of the config itself (they own the deploy), so there is no runtime
// authz check; the mode chokepoint (MayActuate = mode ∧ preflight-green) plus the never-auto floor,
// graduation, novelty, and band gates still govern every actuation, so a seeded actuating mode is exactly as
// safe as an operator flipping to it at runtime. Returns nil on a clean seed OR a deliberate no-op; returns
// an error only when a declared non-Shadow seed could not be safely applied (caller logs and stays Shadow).
func (c *ModeController) SeedInitialMode(ctx context.Context, configured Mode, reason string) error {
	if c.store == nil {
		return nil // in-memory only → stays Shadow
	}
	if _, err := c.store.Load(ctx); !errors.Is(err, ErrModeAbsent) {
		return nil // a mode is already persisted (or unreadable → resolveFromStore already failed closed); never override
	}
	if !configured.valid() || configured == ModeShadow {
		return nil // nothing to seed — the resolved default is already Shadow
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Re-confirm absent UNDER the lock (defense-in-depth). Every other mode writer (Transition, ForceShadow)
	// holds c.mu before persisting, so re-reading the store here serializes the seed with them: a mode set
	// between the pre-lock Load and now is observed and the seed no-ops rather than race-overwriting it. Boot
	// is single-threaded today (the Temporal worker + breaker are not live yet), so this is belt-and-braces
	// that keeps absent-only robust regardless of future caller ordering.
	if _, err := c.store.Load(ctx); !errors.Is(err, ErrModeAbsent) {
		return nil
	}

	// Escalation gate: seeding into an actuating mode requires the green preflight (REQ-1520), same as a
	// runtime transition. A red/absent preflight refuses the seed BEFORE any ledger record — fail closed.
	if configured.MayAutoActuate() {
		if c.preflight == nil {
			return ErrPreflightNotGreen
		}
		if err := c.preflight.PreflightGreen(ctx); err != nil {
			return fmt.Errorf("%w: %v", ErrPreflightNotGreen, err)
		}
	}

	// Audit the establishment BEFORE it takes effect (REQ-1502). A ledger failure fails the seed closed.
	if c.ledger == nil {
		return errors.New("policy: nil ledger — refusing an unaudited mode seed")
	}
	rec := audit.GovDecision{
		Decision: fmt.Sprintf("mode-seed:%s->%s", ModeShadow, configured),
		Reason:   fmt.Sprintf("deploy-time initial mode; reason=%s", reason),
		ActionID: fmt.Sprintf("mode-seed:%s->%s:deploy-config", ModeShadow, configured),
		Withheld: !configured.MayAutoActuate(),
	}
	if _, err := c.ledger.Append(rec); err != nil {
		return fmt.Errorf("policy: mode seed audit failed, mode unchanged: %w", err)
	}
	if err := c.store.Save(ctx, configured); err != nil {
		return fmt.Errorf("policy: mode seed persist failed, mode unchanged: %w", err)
	}
	c.current = configured
	c.log("mode: seeded initial mode %s on a fresh deployment via deploy-time config (%s)", configured, reason)
	return nil
}

func (c *ModeController) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Actuation-chokepoint absorption (spec/015 T-015-13, REQ-1520/1521). The ModeController IS the mode authority
// the mechanical actuation chokepoint (core/safety.Chokepoint) consults: the active mode is the single source
// of "may actuate?", and ForceShadow is the successor to the retired MutationGate.Disable(). These two methods
// make *ModeController satisfy safety.ModeAuthority WITHOUT the safety core importing core/policy (the mode
// enters the chokepoint through safety's narrow interface).
// ---------------------------------------------------------------------------------------------------------

// CurrentActuationMode returns the single active mode as safety.ActuationMode — the narrow "may this mode
// auto-actuate?" view the chokepoint's MayActuate consults. Mode's MayAutoActuate is true only for
// Semi-auto/Full-auto, and the zero value is Shadow, so an un-initialised controller is fail-closed read-only.
func (c *ModeController) CurrentActuationMode() safety.ActuationMode { return c.Current() }

// ForceShadow drops the active mode to Shadow UNCONDITIONALLY — the mode-chokepoint successor to the retired
// MutationGate.Disable(): the runtime kill the deviation breaker and the /halt kill-switch call. It is SAFE
// (only ever MORE restrictive), IDEMPOTENT, and NEVER refused — turning autonomy OFF is never gated by
// authority or preflight. Mirroring the /halt handler's discipline, the in-memory active mode is set to Shadow
// FIRST so the kill takes effect even if the durable persist fails; the persist is best-effort after (an
// unpersisted Shadow still reads Shadow in-process, and a restart resolves fail-closed to Shadow regardless —
// REQ-1519). It appends NO ledger record of its own: the caller (the breaker's TripRecorder, the /halt handler)
// owns the audit note, exactly as gate.Disable() left auditing to its callers — so the audit surface is
// unchanged by the absorption.
func (c *ModeController) ForceShadow(reason string) {
	c.mu.Lock()
	prev := c.current
	c.current = ModeShadow
	c.mu.Unlock()
	if c.store != nil {
		// Persist Shadow so a restart cannot resurrect an actuating mode after a kill (fail closed). Best-effort:
		// the in-memory Shadow above already holds even if this fails.
		_ = c.store.Save(context.Background(), ModeShadow)
	}
	if prev != ModeShadow {
		c.log("mode: FORCED to Shadow (was %s) — safety kill: %s", prev, reason)
	}
}
