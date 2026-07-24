package policy

// Per-op-class graduation ladder — spec/015 task T-015-8 (REQ-1514, REQ-1515): the "earned autonomy"
// mechanism. WHILE graduation is enabled for an op-class, that class starts at verdict `approve` and is
// promoted to `auto` ONLY after N consecutive VERIFIED-CLEAN runs, and is demoted back to `approve` on the
// first `deviation` (REQ-1514). Promotion is bound to verify-on-auto (REQ-1515): a run that did not produce
// a clean, verified post-state does NOT count toward promotion, so autonomy is never earned on unverified
// evidence. This is step 5 of the mode/verdict decision procedure (design.md): a class still in `approve`
// graduation state is NOT yet promoted; a class that has met its clean-run bar evaluates at `auto`.
//
// This leaf builds ONLY the ladder state machine, its verify-on-auto counter, the graduation→verdict hook,
// and a store seam + in-memory fake. It does NOT wire the Runner / interceptor / verify pipeline, write a DB
// migration (T-015-12), or touch core/safety / core/verify / core/actuate (zero diff there). The ladder
// CONSUMES verification outcomes — it never re-runs or re-adjudicates verification; the deterministic
// verifier (core/verify, INV-10) remains the SOLE author of the match / partial / deviation verdict, and the
// graduation counter reads ONLY those verdicts, never the acting model.
//
// The constitutional never-auto floor (INV-09) is untouchable and lives BENEATH this engine: a graduated
// `auto` verdict this ladder produces still passes through band composition's floor clamp (band.go /
// core/safety) downstream. This file adds no floor and bypasses none — it only decides whether a class has
// earned the RIGHT to be offered `auto`; the floor still applies beneath.
//
// Provenance: [R] paradigm-rule 4 (graduated ladder) · [O] INV-09 (fail closed), INV-10 (verify-on-auto).
// See spec/015-policy-engine requirements.md REQ-1514/1515.

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/territory-grounder/grounder/core/safety"
)

// DefaultPromoteThreshold is the fallback N (consecutive verified-clean runs required to promote a class from
// `approve` to `auto`) used when a Ladder is constructed with a non-positive threshold. It is deliberately
// conservative — autonomy is earned slowly. An operator/rule may configure a higher per-class N; a lower one
// than 1 is meaningless and clamps to this default.
const DefaultPromoteThreshold = 5

// Level is the per-op-class graduation level: the autonomy a class has EARNED. It is a CLOSED enum whose ZERO
// VALUE is LevelApprove (fail closed) — an un-initialised, absent, or corrupt persisted level resolves to
// `approve` (route to a human vote), NEVER straight to `auto`.
type Level int

const (
	// LevelApprove (the zero value, fail-closed) — the class has NOT earned autonomy: an `auto` rule verdict
	// for this class is downgraded to `approve` (a human vote) by GraduatedVerdict.
	LevelApprove Level = iota
	// LevelAuto — the class has earned autonomy: an `auto` rule verdict is honored. The constitutional
	// never-auto floor (INV-09) still clamps floor-class ops beneath the engine regardless of this level.
	LevelAuto
)

// valid reports whether l is one of the two closed-enum levels. Used to reject a corrupt persisted value —
// an invalid level fails closed to LevelApprove (never LevelAuto).
func (l Level) valid() bool { return l == LevelApprove || l == LevelAuto }

// Verdict maps a graduation level onto the verdict ceiling it permits: LevelAuto permits `auto`; every other
// (including any corrupt) value permits at most `approve`.
func (l Level) Verdict() Verdict {
	if l == LevelAuto {
		return VerdictAuto
	}
	return VerdictApprove
}

// String renders the canonical level name; a corrupt value renders as approve (fail closed).
func (l Level) String() string {
	if l == LevelAuto {
		return "auto"
	}
	return "approve"
}

// RunOutcome is the graduation-relevant classification of ONE completed run of an op-class. It is the small
// enum this package defines at the integration boundary because the canonical verifier type (core/safety.
// Verdict = match / partial / deviation) cannot express the verify-on-auto case "an auto run whose post-state
// was NOT verified at all" (REQ-1515) — that is a policy-layer concept, not a mechanical verdict. Map a
// verifier outcome to a RunOutcome with OutcomeFromVerdict at the boundary; the ladder consumes ONLY this.
type RunOutcome int

const (
	// OutcomeUnverified (the zero value, fail-safe) — the run did NOT produce a clean, verified `match`. This
	// covers BOTH verify-on-auto violations (an `auto` run whose post-state could not be / was not verified,
	// REQ-1515) AND a verifier `partial` (a run that IS verified but is not a clean match and is not a
	// deviation). It NEITHER promotes NOR demotes: it breaks the consecutive-clean streak (resets the count)
	// but never drops an already-earned level on its own. Conservative by design — it never grants autonomy.
	OutcomeUnverified RunOutcome = iota
	// OutcomeVerifiedClean — a VERIFIED `match` (REQ-1514 "verified match run"). The ONLY promoting outcome:
	// at LevelApprove it increments the clean-run count toward N.
	OutcomeVerifiedClean
	// OutcomeDeviated — a VERIFIED `deviation` (REQ-1514). The ONLY demoting outcome: at ANY level it drops the
	// class to LevelApprove and resets the count — a deviation always drops autonomy.
	OutcomeDeviated
)

// String renders the canonical outcome name for logging + the audit projection.
func (o RunOutcome) String() string {
	switch o {
	case OutcomeVerifiedClean:
		return "verified_clean"
	case OutcomeDeviated:
		return "deviated"
	default:
		return "unverified"
	}
}

// OutcomeFromVerdict maps a core/verify mechanical verdict (safety.Verdict) plus whether the post-state was
// actually verified onto a graduation RunOutcome — THE boundary between the deterministic verifier (INV-10)
// and the graduation ladder. It is the single translation point; the ladder never imports verify semantics
// beyond this.
//
//   - verified == false → OutcomeUnverified. An `auto` execution whose post-state was not verified NEVER
//     counts as clean (verify-on-auto, REQ-1515), regardless of any verdict value.
//   - safety.VerdictMatch → OutcomeVerifiedClean (the only promoting outcome).
//   - safety.VerdictDeviation → OutcomeDeviated (the only demoting outcome).
//   - safety.VerdictPartial (or any invalid verdict) → OutcomeUnverified. A partial IS verified but is NOT a
//     clean match and NOT a deviation, so per REQ-1514 it neither promotes (not a `match` run) nor demotes
//     (not a `deviation`); it is treated as a non-promoting, non-demoting run that breaks the clean streak.
//     (Boundary note: this is the one place a `partial` — a verified verdict — shares the `unverified`
//     bucket; both are "not clean, not a deviation" and drive the identical safe ladder effect.)
func OutcomeFromVerdict(v safety.Verdict, verified bool) RunOutcome {
	if !verified {
		return OutcomeUnverified
	}
	switch v {
	case safety.VerdictMatch:
		return OutcomeVerifiedClean
	case safety.VerdictDeviation:
		return OutcomeDeviated
	default:
		return OutcomeUnverified // partial or any invalid verdict — verified but not clean, non-promoting
	}
}

// ClassState is the durable per-op-class ladder state (REQ-1514): the earned Level, the running count of
// CONSECUTIVE verified-clean runs at LevelApprove, and the last outcome recorded. It is the unit the
// GraduationStore persists and the ladder mutates. A zero ClassState is LevelApprove with count 0 — the
// fail-closed start every class takes when no durable state exists.
type ClassState struct {
	OpClass       string
	Level         Level
	CleanRunCount int
	LastOutcome   RunOutcome
}

// RecordResult is the NON-SECRET projection of one Record — what the ladder saw and how the class moved. It
// carries no argv, host, or credential: only the op-class label, the outcome, the level transition, the
// resulting count, and Promoted/Demoted flags. A caller (a later leaf) appends this to the governance ledger
// as the graduation promote/demote record (design.md); this leaf only returns it (no I/O).
type RecordResult struct {
	OpClass       string
	Outcome       RunOutcome
	From          Level
	To            Level
	CleanRunCount int
	Threshold     int
	Promoted      bool
	Demoted       bool
	Reason        string
}

// applyOutcome is the PURE ladder state machine (REQ-1514/1515): no I/O, no mutation of shared state,
// deterministic for identical inputs. Given the current ClassState, a RunOutcome, and the promote threshold
// N, it returns the next ClassState and a RecordResult describing the transition.
//
//	OutcomeVerifiedClean @ approve : count++ ; WHEN count reaches N → promote to auto (count reset to 0).
//	OutcomeVerifiedClean @ auto    : stays auto (already graduated); count stays 0.
//	OutcomeDeviated       @ any     : demote to approve + reset count to 0 (a deviation always drops autonomy).
//	OutcomeUnverified     @ any     : NO promote, NO demote; reset the consecutive-clean count to 0 (an
//	                                  unverified/partial run breaks the streak — verify-on-auto, REQ-1515).
func applyOutcome(st ClassState, outcome RunOutcome, threshold int) (ClassState, RecordResult) {
	if threshold < 1 {
		threshold = DefaultPromoteThreshold
	}
	if !st.Level.valid() {
		st.Level = LevelApprove // fail closed — a corrupt in-hand level never behaves as auto.
	}
	if st.CleanRunCount < 0 {
		st.CleanRunCount = 0
	}
	from := st.Level
	res := RecordResult{OpClass: st.OpClass, Outcome: outcome, From: from, Threshold: threshold}

	switch outcome {
	case OutcomeVerifiedClean:
		if st.Level == LevelApprove {
			st.CleanRunCount++
			if st.CleanRunCount >= threshold {
				st.Level = LevelAuto
				st.CleanRunCount = 0 // graduated — the count is spent; auto no longer counts toward promotion.
				res.Promoted = true
			}
		}
		// At LevelAuto a clean run just confirms graduation; nothing changes.
	case OutcomeDeviated:
		if st.Level == LevelAuto {
			res.Demoted = true
		}
		st.Level = LevelApprove
		st.CleanRunCount = 0 // a deviation always drops autonomy and resets the climb.
	case OutcomeUnverified:
		// verify-on-auto (REQ-1515): an unverified auto run — or a verified-but-not-clean partial — does NOT
		// count as clean. It never promotes and never demotes; it only breaks the consecutive-clean streak.
		st.CleanRunCount = 0
	default:
		// An unknown outcome is treated as unverified (fail safe): no promotion, streak reset.
		st.CleanRunCount = 0
	}

	st.LastOutcome = outcome
	res.To = st.Level
	res.CleanRunCount = st.CleanRunCount
	res.Reason = recordReason(res)
	return st, res
}

func recordReason(res RecordResult) string {
	switch {
	case res.Promoted:
		return fmt.Sprintf("op-class %q promoted approve→auto after %d consecutive verified-clean runs", res.OpClass, res.Threshold)
	case res.Demoted:
		return fmt.Sprintf("op-class %q demoted auto→approve on a %s outcome — autonomy dropped", res.OpClass, res.Outcome)
	case res.Outcome == OutcomeVerifiedClean:
		return fmt.Sprintf("op-class %q verified-clean run %d/%d toward promotion (level %s)", res.OpClass, res.CleanRunCount, res.Threshold, res.To)
	default:
		return fmt.Sprintf("op-class %q %s run — clean streak reset (level %s, count %d)", res.OpClass, res.Outcome, res.To, res.CleanRunCount)
	}
}

// graduatedVerdict is the PURE graduation→verdict hook (REQ-1514, design.md step 5). It gates ONLY an `auto`
// rule verdict on whether the class has EARNED autonomy:
//
//	ruleVerdict == auto  , level == auto    → auto     (graduated — honor it)
//	ruleVerdict == auto  , level == approve → approve  (not yet graduated — downgrade to a human vote)
//	ruleVerdict == approve               → approve  (unchanged)
//	ruleVerdict == deny                  → deny     (a deny is NEVER affected by graduation)
//
// This is how Semi-auto mode uses the ladder: Semi-auto permits `auto` ONLY for graduated classes; everything
// else routes to approval. It never LOOSENS a verdict — it can only downgrade an ungraduated `auto`.
func graduatedVerdict(level Level, ruleVerdict Verdict) Verdict {
	if ruleVerdict != VerdictAuto {
		return ruleVerdict // approve and deny pass through untouched; graduation never affects a deny.
	}
	if level == LevelAuto {
		return VerdictAuto
	}
	return VerdictApprove // the class has not earned auto yet — route to a human vote.
}

// ---------------------------------------------------------------------------------------------------------
// Store seam + in-memory fake. The durable pgx store + migration is a later leaf (T-015-12); this leaf ships
// only the in-memory fake for oracles (CI has no DB).
// ---------------------------------------------------------------------------------------------------------

// ErrClassAbsent is returned by a GraduationStore whose per-op-class state has never been persisted. The
// ladder resolves it fail-closed to a fresh LevelApprove state (REQ-1514), which is also the correct start.
var ErrClassAbsent = errors.New("policy: graduation class state absent")

// GraduationStore persists per-op-class ladder state. Load returns ErrClassAbsent (or any error) when the
// class state cannot be read — the ladder resolves that fail-closed to LevelApprove and NEVER loads a class
// straight into LevelAuto from an absent/errored/corrupt store. The durable pgx impl + migration is T-015-12.
type GraduationStore interface {
	Load(ctx context.Context, opClass string) (ClassState, error)
	Save(ctx context.Context, st ClassState) error
}

// MemGraduationStore is the in-memory GraduationStore fake for oracle tests. It reports ErrClassAbsent for an
// unknown class and can be primed with a load error (to exercise the "unreadable → approve" fail-closed path)
// and with a pre-seeded state (to exercise the "corrupt → approve" path or durable-auto reload).
type MemGraduationStore struct {
	mu      sync.Mutex
	states  map[string]ClassState
	loadErr error
}

// NewMemGraduationStore returns an empty in-memory store (no class persisted → Load fails closed to approve).
func NewMemGraduationStore() *MemGraduationStore {
	return &MemGraduationStore{states: map[string]ClassState{}}
}

// WithLoadError primes the store to fail every Load with err (to test the "unreadable → approve" path).
func (s *MemGraduationStore) WithLoadError(err error) *MemGraduationStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
	return s
}

// Seed pre-persists a class state (to test durable reload and the "corrupt persisted level → approve" path).
func (s *MemGraduationStore) Seed(st ClassState) *MemGraduationStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[st.OpClass] = st
	return s
}

// Load returns the persisted state for opClass, or ErrClassAbsent when none / a primed error.
func (s *MemGraduationStore) Load(_ context.Context, opClass string) (ClassState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ClassState{}, s.loadErr
	}
	st, ok := s.states[opClass]
	if !ok {
		return ClassState{}, fmt.Errorf("%w: %q", ErrClassAbsent, opClass)
	}
	return st, nil
}

// Save persists st keyed by its OpClass.
func (s *MemGraduationStore) Save(_ context.Context, st ClassState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[st.OpClass] = st
	return nil
}

// ---------------------------------------------------------------------------------------------------------
// Ladder — the concurrency-safe controller over the pure state machine + the store.
// ---------------------------------------------------------------------------------------------------------

// ErrPromotionNotPersisted is returned by Record when a PROMOTION (approve→auto) could not be durably saved.
// The ladder REFUSES to grant unpersisted autonomy: the in-memory level stays approve (fail closed), because
// a promotion that would vanish on restart must not take effect. A demotion or a non-promoting change is
// always applied in-memory even if its Save fails (fail closed toward safety) and surfaces a wrapped error.
var ErrPromotionNotPersisted = errors.New("policy: graduation promotion not persisted — autonomy withheld")

// Ladder owns per-op-class graduation state and serializes every mutation (REQ-1514). It caches loaded states
// and writes through to the store. A load error / absent / corrupt persisted state resolves fail-closed to a
// fresh LevelApprove class (REQ-1514) — a class is NEVER loaded straight into LevelAuto from a bad store.
type Ladder struct {
	mu        sync.Mutex
	threshold int
	store     GraduationStore
	states    map[string]ClassState
	logf      func(format string, args ...any)
}

// NewLadder builds a ladder with a promote threshold N and a store. A non-positive threshold clamps to
// DefaultPromoteThreshold. store may be nil (in-memory only; every class starts fail-closed at approve). logf
// is optional (nil → silent).
func NewLadder(threshold int, store GraduationStore, logf func(string, ...any)) *Ladder {
	if threshold < 1 {
		threshold = DefaultPromoteThreshold
	}
	return &Ladder{threshold: threshold, store: store, states: map[string]ClassState{}, logf: logf}
}

// Threshold returns the ladder's configured promote threshold N.
func (l *Ladder) Threshold() int { return l.threshold }

// stateLocked returns the current state for opClass, loading it fail-closed on first touch. Caller holds l.mu.
// An absent / unreadable / corrupt persisted state resolves to a fresh LevelApprove class (never LevelAuto).
func (l *Ladder) stateLocked(ctx context.Context, opClass string) ClassState {
	if st, ok := l.states[opClass]; ok {
		return st
	}
	st := ClassState{OpClass: opClass, Level: LevelApprove}
	if l.store != nil {
		loaded, err := l.store.Load(ctx, opClass)
		switch {
		case err != nil:
			l.log("graduation: state for %q unreadable (%v) — fail-closed to approve", opClass, err)
		case !loaded.Level.valid():
			l.log("graduation: persisted level for %q corrupt (%d) — fail-closed to approve", opClass, int(loaded.Level))
		default:
			st = loaded
			st.OpClass = opClass
			if st.CleanRunCount < 0 {
				st.CleanRunCount = 0
			}
		}
	}
	l.states[opClass] = st
	return st
}

// State returns the current ladder state for opClass, resolving it fail-closed on first touch. Concurrent-safe.
func (l *Ladder) State(ctx context.Context, opClass string) ClassState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stateLocked(ctx, opClass)
}

// LevelOf returns the earned graduation level for opClass (fail-closed to LevelApprove). Concurrent-safe.
func (l *Ladder) LevelOf(ctx context.Context, opClass string) Level {
	return l.State(ctx, opClass).Level
}

// Record advances the ladder for opClass by one run outcome and returns the transition (REQ-1514/1515). It is
// concurrency-safe: concurrent Record calls serialize on l.mu, so no torn state can be observed. The state
// machine itself is the PURE applyOutcome; Record adds only the load/save and the fail-closed persistence
// policy:
//
//   - A PROMOTION (approve→auto) that cannot be persisted is REFUSED — the in-memory level stays approve and
//     ErrPromotionNotPersisted is returned, so autonomy is never granted on state that would vanish on restart.
//   - A demotion / non-promoting change whose Save fails is still applied in-memory (fail closed toward
//     safety — always drop autonomy) and the wrapped Save error is returned alongside the applied result.
func (l *Ladder) Record(ctx context.Context, opClass string, outcome RunOutcome) (RecordResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cur := l.stateLocked(ctx, opClass)
	next, res := applyOutcome(cur, outcome, l.threshold)

	if l.store != nil {
		if err := l.store.Save(ctx, next); err != nil {
			if res.Promoted {
				// Refuse to grant autonomy that would not survive a restart. Keep the pre-promotion state.
				l.states[opClass] = cur
				return RecordResult{
					OpClass: opClass, Outcome: outcome, From: cur.Level, To: cur.Level,
					CleanRunCount: cur.CleanRunCount, Threshold: l.threshold,
					Reason: "promotion persist failed — autonomy withheld, class stays approve",
				}, fmt.Errorf("%w: %v", ErrPromotionNotPersisted, err)
			}
			// Demotion / non-promoting change: apply in-memory (fail closed toward safety), surface the error.
			l.states[opClass] = next
			l.log("graduation: %s", res.Reason)
			return res, fmt.Errorf("policy: graduation state persist failed, applied in-memory: %w", err)
		}
	}

	l.states[opClass] = next
	l.log("graduation: %s", res.Reason)
	return res, nil
}

// GraduatedVerdict is the graduation→verdict hook (REQ-1514, design.md step 5): it downgrades an ungraduated
// `auto` rule verdict to `approve`, honors a graduated `auto`, and leaves `approve` / `deny` untouched. It
// reads the class's earned level fail-closed (an unknown/unreadable class is LevelApprove). Concurrent-safe.
// The produced verdict still passes through the band-composition never-auto floor (INV-09) downstream — this
// hook decides only whether the class has earned the RIGHT to `auto`, never lifting the floor beneath it.
func (l *Ladder) GraduatedVerdict(ctx context.Context, opClass string, ruleVerdict Verdict) Verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.stateLocked(ctx, opClass)
	return graduatedVerdict(st.Level, ruleVerdict)
}

func (l *Ladder) log(format string, args ...any) {
	if l.logf != nil {
		l.logf(format, args...)
	}
}
