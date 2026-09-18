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

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// DefaultPromoteThreshold is the fallback N (consecutive verified-clean runs required to promote a class from
// `approve` to `auto`) used when a Ladder is constructed with a non-positive threshold. It is deliberately
// conservative — autonomy is earned slowly. An operator/rule may configure a higher per-class N; a lower one
// than 1 is meaningless and clamps to this default.
const DefaultPromoteThreshold = 5

// DefaultNoticeThreshold is the SECOND bar: the consecutive verified-clean runs a class must accumulate AT
// auto_notice before it may reach the silent `auto` rung (spec/028 REQ-2808). It is deliberately double the
// first bar. The two climbs are not measuring the same thing: the first asks "does this op do what it claims?",
// which a handful of runs can answer; the second asks "is anyone still reading the pages?", and the only
// evidence for that is a long, boring stretch in which a watched autonomy produced nothing worth watching.
const DefaultNoticeThreshold = 10

// PromoteThresholdForTier returns the per-class first-bar N implied by a declared safety tier (spec/028
// REQ-2803): low-reversible ⇒ 5, everything else ⇒ 10. It never returns below DefaultPromoteThreshold, which
// mirrors the `CHECK (promote_threshold >= 5)` on opclass_ratified: ratification may only ever RAISE the bar
// the compiled default sets, never buy a faster climb than the code itself would allow.
func PromoteThresholdForTier(tier string) int {
	if tier == opschema.TierLowReversible {
		return DefaultPromoteThreshold
	}
	return DefaultNoticeThreshold
}

// Level is the per-op-class graduation level: the autonomy a class has EARNED. It is a CLOSED enum whose ZERO
// VALUE is LevelApprove (fail closed) — an un-initialised, absent, or corrupt persisted level resolves to
// `approve` (route to a human vote), NEVER straight to `auto`.
//
// The values are written EXPLICITLY rather than via iota. LevelAutoNotice was inserted between the two
// original rungs, and an iota shift would have silently changed what the integer 1 MEANS. The durable store
// persists the level as text, so no live row was reinterpreted — but a rung whose numeric identity moves under
// an insertion is a trap laid for the next insertion, and the ladder is ordered by autonomy so the ordering
// should be readable in the constants themselves.
type Level int

const (
	// LevelApprove (the zero value, fail-closed) — the class has NOT earned autonomy: an `auto` rule verdict
	// for this class is downgraded to `approve` (a human vote) by GraduatedVerdict.
	LevelApprove Level = 0
	// LevelAutoNotice — "acts and pages" (spec/028 REQ-2807, ADR-0016 decision 2). The class has earned the
	// right to act WITHOUT a human vote, but every action it takes raises a notice. This is the CEILING for a
	// class admitted through the runtime overlay: it may act, but never unobserved.
	//
	// It permits the same VERDICT as LevelAuto — the run is not routed to a vote — and the difference is
	// carried by the risk band (NoticeFloor clamps a computed AUTO to AUTO_NOTICE). Modelling the notice as a
	// band rather than a verdict is deliberate: a verdict decides WHETHER the action happens, and at this rung
	// it does happen. What changes is who finds out.
	LevelAutoNotice Level = 1
	// LevelAuto — the class has earned SILENT autonomy: an `auto` rule verdict is honored with no page. The
	// constitutional never-auto floor (INV-09) still clamps floor-class ops beneath the engine regardless of
	// this level, and a class may only reach this rung if it is EMBEDDED (ADR-0016 decision 2).
	LevelAuto Level = 2
)

// valid reports whether l is one of the closed-enum levels. Used to reject a corrupt persisted value — an
// invalid level fails closed to LevelApprove (never any autonomous rung).
func (l Level) valid() bool { return l == LevelApprove || l == LevelAutoNotice || l == LevelAuto }

// Verdict maps a graduation level onto the verdict ceiling it permits: both autonomous rungs permit `auto`;
// every other (including any corrupt) value permits at most `approve`.
//
// auto_notice sharing the `auto` verdict with auto is the point of the rung, not a leak: the class acts
// without a vote at BOTH rungs. The notice is applied downstream as a band floor (spec/028 REQ-2809), which is
// the only place it can be applied without turning "someone gets paged" into "the action does not happen".
func (l Level) Verdict() Verdict {
	if l == LevelAutoNotice || l == LevelAuto {
		return VerdictAuto
	}
	return VerdictApprove
}

// String renders the canonical level name; a corrupt value renders as approve (fail closed). These spellings
// are the durable wire format — they must match the `level` CHECK in migration 0050.
func (l Level) String() string {
	switch l {
	case LevelAuto:
		return "auto"
	case LevelAutoNotice:
		return "auto_notice"
	default:
		return "approve"
	}
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
	// OutcomeSeeded — a curated OUT-OF-BOX class placed at a level by SeedDefaults, NOT a run outcome. It is
	// PROVENANCE, not a verification: the class was seeded (e.g. to `auto` as the reversible baseline), it has
	// NOT executed+verified. It NEVER promotes or demotes — applyOutcome's fail-safe default treats it as an
	// unverified run — so a seeded class earns nothing until a REAL run records OutcomeVerifiedClean. Its only
	// job is to keep the durable `last_outcome` HONEST: a seeded class must never masquerade as `verified_clean`
	// (a verification that never happened). Added by TG-183.
	OutcomeSeeded
)

// String renders the canonical outcome name for logging + the audit projection.
func (o RunOutcome) String() string {
	switch o {
	case OutcomeVerifiedClean:
		return "verified_clean"
	case OutcomeDeviated:
		return "deviated"
	case OutcomeSeeded:
		return "seeded"
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
	// NoticeRunCount is the SECOND climb's streak: consecutive verified-clean runs accumulated AT
	// LevelAutoNotice toward LevelAuto (spec/028 REQ-2804). It is a separate counter rather than a reuse of
	// CleanRunCount because a demotion has to be unambiguous about which climb the count belonged to — a
	// single shared counter would leave "3" meaning either "3 of 5 toward acting" or "3 of 10 toward acting
	// silently", and the operator reading the ladder could not tell which.
	NoticeRunCount int
	LastOutcome    RunOutcome
	// Version is the optimistic-concurrency token carried with the persisted row (TG-146 S3/S4, migration
	// 0101). A store Load sets it; Record threads it back to Save unchanged (applyOutcome, a pure function over
	// the domain fields, never touches it), so the durable write lands only if the row has not moved since the
	// read — a peer worker's demotion is not clobbered by this worker's stale cache. Zero means "not read from a
	// durable row": a fresh/absent class, or an authoritative unconditional write (the ratify reset) — both of
	// which the store applies WITHOUT a CAS guard. The persisted value is always >= 1.
	Version int64
}

// RecordResult is the NON-SECRET projection of one Record — what the ladder saw and how the class moved. It
// carries no argv, host, or credential: only the op-class label, the outcome, the level transition, the
// resulting count, and Promoted/Demoted flags. A caller (a later leaf) appends this to the governance ledger
// as the graduation promote/demote record (design.md); this leaf only returns it (no I/O).
type RecordResult struct {
	OpClass        string
	Outcome        RunOutcome
	From           Level
	To             Level
	CleanRunCount  int
	NoticeRunCount int
	Threshold      int
	NoticeThresh   int
	Promoted       bool
	Demoted        bool
	// CeilingHeld reports that the class completed the SECOND climb but was held at auto_notice because it is
	// not embedded (ADR-0016 decision 2). It is surfaced so the console can say "this class has earned auto and
	// is waiting on an embed-export MR" rather than silently stalling a streak at 10/10 forever, which reads
	// to an operator as a bug in the ladder.
	CeilingHeld bool
	Reason      string
}

// applyOutcome is the PURE ladder state machine (REQ-1514/1515): no I/O, no mutation of shared state,
// deterministic for identical inputs. Given the current ClassState, a RunOutcome, and the promote threshold
// N, it returns the next ClassState and a RecordResult describing the transition.
//
//	OutcomeVerifiedClean @ approve     : count++ ; WHEN count reaches N → promote to AUTO_NOTICE (count spent).
//	OutcomeVerifiedClean @ auto_notice : noticeCount++ ; WHEN it reaches M **AND the class is EMBEDDED** →
//	                                     promote to AUTO. An overlay-only class HOLDS at auto_notice with its
//	                                     streak pinned at M (CeilingHeld) — the ceiling is not a reset.
//	OutcomeVerifiedClean @ auto        : stays auto (already graduated); counts stay 0.
//	OutcomeDeviated       @ any        : demote to approve + reset BOTH counts (a deviation always drops
//	                                     autonomy — all the way to the bottom, not one rung).
//	OutcomeUnverified     @ any        : NO promote, NO demote; reset both consecutive-clean counts (an
//	                                     unverified/partial run breaks the streak — verify-on-auto, REQ-1515).
//
// A deviation drops to approve rather than one rung because the two rungs answer different questions: the
// climb to auto_notice established that the op works, and a verified deviation is exactly the evidence that it
// does not. There is nothing left standing to fall one rung onto.
func applyOutcome(st ClassState, outcome RunOutcome, threshold, noticeThreshold int) (ClassState, RecordResult) {
	if threshold < 1 {
		threshold = DefaultPromoteThreshold
	}
	if noticeThreshold < 1 {
		noticeThreshold = DefaultNoticeThreshold
	}
	if !st.Level.valid() {
		st.Level = LevelApprove // fail closed — a corrupt in-hand level never behaves as auto.
	}
	if st.CleanRunCount < 0 {
		st.CleanRunCount = 0
	}
	if st.NoticeRunCount < 0 {
		st.NoticeRunCount = 0
	}
	from := st.Level
	res := RecordResult{OpClass: st.OpClass, Outcome: outcome, From: from, Threshold: threshold, NoticeThresh: noticeThreshold}

	switch outcome {
	case OutcomeVerifiedClean:
		switch st.Level {
		case LevelApprove:
			st.CleanRunCount++
			// Defence in depth: a REGISTERED class whose safety tier forbids autonomy must not accumulate a
			// durable autonomous row either — otherwise the ladder reports a graduation no decision will ever
			// honor, and an operator reading the table believes the class is autonomous when it is not.
			// Scoped deliberately to a DECLARED-ineligible tier, not to "absent from the registry": an
			// unregistered slug has no tier to violate, cannot actuate at all (no compiled argv builder), and
			// is already floored at the decision point. Blocking its promotion here would only couple the
			// ladder's bookkeeping to registry membership for no safety gain.
			if st.CleanRunCount >= threshold && !tierForbidsAuto(st.OpClass) {
				st.Level = LevelAutoNotice
				st.CleanRunCount = 0  // the first climb is spent
				st.NoticeRunCount = 0 // the second climb starts at zero
				res.Promoted = true
			}
		case LevelAutoNotice:
			st.NoticeRunCount++
			if st.NoticeRunCount >= noticeThreshold && !tierForbidsAuto(st.OpClass) {
				// THE AUTO CEILING, STRUCTURALLY (ADR-0016 decision 2). The silent rung requires membership in
				// the EMBEDDED, lockstep-hashed registry — a code release. A class admitted through the runtime
				// overlay has earned every run it has made, and it still stops here: the difference is not how
				// well it performed but which tamper domain grants it, and the rung where NO HUMAN WATCHES must
				// live in the domain whose contents cannot be changed by a runtime write.
				//
				// The streak is PINNED at the threshold rather than reset. A reset would make the console show
				// a class endlessly re-climbing a ladder it can never finish; pinned + CeilingHeld says the true
				// thing — it has earned auto and is waiting on an embed-export MR.
				if isEmbeddedClass(st.OpClass) {
					st.Level = LevelAuto
					st.NoticeRunCount = 0
					res.Promoted = true
				} else {
					st.NoticeRunCount = noticeThreshold
					res.CeilingHeld = true
				}
			}
		}
		// At LevelAuto a clean run just confirms graduation; nothing changes.
	case OutcomeDeviated:
		if st.Level == LevelAuto || st.Level == LevelAutoNotice {
			res.Demoted = true
		}
		st.Level = LevelApprove
		st.CleanRunCount = 0 // a deviation always drops autonomy and resets BOTH climbs.
		st.NoticeRunCount = 0
	case OutcomeUnverified:
		// verify-on-auto (REQ-1515): an unverified auto run — or a verified-but-not-clean partial — does NOT
		// count as clean. It never promotes and never demotes; it only breaks the consecutive-clean streaks.
		st.CleanRunCount = 0
		st.NoticeRunCount = 0
	default:
		// An unknown outcome is treated as unverified (fail safe): no promotion, streaks reset.
		st.CleanRunCount = 0
		st.NoticeRunCount = 0
	}

	st.LastOutcome = outcome
	res.To = st.Level
	res.CleanRunCount = st.CleanRunCount
	res.NoticeRunCount = st.NoticeRunCount
	res.Reason = recordReason(res)
	return st, res
}

func recordReason(res RecordResult) string {
	switch {
	case res.Promoted:
		return fmt.Sprintf("op-class %q promoted %s→%s after %d consecutive verified-clean runs",
			res.OpClass, res.From, res.To, promotionBar(res))
	case res.CeilingHeld:
		return fmt.Sprintf("op-class %q completed %d/%d clean runs at auto_notice but is NOT in the embedded "+
			"registry — held at auto_notice (the silent rung requires an embed-export code release; ADR-0016)",
			res.OpClass, res.NoticeRunCount, res.NoticeThresh)
	case res.Demoted:
		return fmt.Sprintf("op-class %q demoted %s→approve on a %s outcome — autonomy dropped", res.OpClass, res.From, res.Outcome)
	case res.Outcome == OutcomeVerifiedClean && res.To == LevelAutoNotice:
		return fmt.Sprintf("op-class %q verified-clean run %d/%d toward SILENT auto (level auto_notice)", res.OpClass, res.NoticeRunCount, res.NoticeThresh)
	case res.Outcome == OutcomeVerifiedClean:
		return fmt.Sprintf("op-class %q verified-clean run %d/%d toward promotion (level %s)", res.OpClass, res.CleanRunCount, res.Threshold, res.To)
	default:
		return fmt.Sprintf("op-class %q %s run — clean streak reset (level %s, count %d)", res.OpClass, res.Outcome, res.To, res.CleanRunCount)
	}
}

// promotionBar reports which of the two bars the promotion in res cleared, so the ledger reason names the
// number the class actually met rather than whichever threshold happens to be in scope.
func promotionBar(res RecordResult) int {
	if res.To == LevelAuto {
		return res.NoticeThresh
	}
	return res.Threshold
}

// graduatedVerdict is the PURE graduation→verdict hook (REQ-1514, design.md step 5). It gates ONLY an `auto`
// rule verdict on whether the class has EARNED autonomy:
//
//	ruleVerdict == auto  , level == auto        → auto     (graduated — honor it, silently)
//	ruleVerdict == auto  , level == auto_notice → auto     (graduated to ACT; the notice is a BAND floor
//	                                                        applied downstream, spec/028 REQ-2809)
//	ruleVerdict == auto  , level == approve     → approve  (not yet graduated — downgrade to a human vote)
//	ruleVerdict == approve                      → approve  (unchanged)
//	ruleVerdict == deny                         → deny     (a deny is NEVER affected by graduation)
//
// This is how Semi-auto mode uses the ladder: Semi-auto permits `auto` ONLY for graduated classes; everything
// else routes to approval. It never LOOSENS a verdict — it can only downgrade an ungraduated `auto`.
func graduatedVerdict(level Level, ruleVerdict Verdict) Verdict {
	if ruleVerdict != VerdictAuto {
		return ruleVerdict // approve and deny pass through untouched; graduation never affects a deny.
	}
	return level.Verdict() // both autonomous rungs act; approve and any corrupt value route to a vote.
}

// ---------------------------------------------------------------------------------------------------------
// Store seam + in-memory fake. The durable pgx store + migration is a later leaf (T-015-12); this leaf ships
// only the in-memory fake for oracles (CI has no DB).
// ---------------------------------------------------------------------------------------------------------

// ErrClassAbsent is returned by a GraduationStore whose per-op-class state has never been persisted. The
// ladder resolves it fail-closed to a fresh LevelApprove state (REQ-1514), which is also the correct start.
var ErrClassAbsent = errors.New("policy: graduation class state absent")

// ErrConcurrentModification is returned by a GraduationStore.Save whose optimistic-concurrency guard failed:
// the durable row's version no longer matches the version the caller read (a peer worker wrote it in between).
// It is NOT a failure — it is the signal to reload the fresh durable state and re-decide, which Ladder.Record
// does automatically (bounded). A Save guards only a POSITIVE in-hand version; an unconditional (version 0)
// Save — a fresh class or the ratify reset — never returns it. See migration 0101 and the durable-breaker
// CompareAndOpen (core/db/breaker_write.go) whose ErrNoRows-means-lost idiom the pgx store mirrors.
var ErrConcurrentModification = errors.New("policy: graduation state changed concurrently — reload and retry")

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

// Seed pre-persists a class state (to test durable reload and the "corrupt persisted level → approve" path). A
// seed represents a row already in the durable store, so its version is forced to at least 1 (the store never
// persists version 0) — this lets an oracle Seed a class and then exercise the compare-and-set on Save.
func (s *MemGraduationStore) Seed(st ClassState) *MemGraduationStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.Version < 1 {
		st.Version = 1
	}
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

// Save persists st keyed by its OpClass, mirroring the durable store's optimistic-concurrency guard (TG-146
// S3/S4) so oracles exercise the same compare-and-set the pgx store enforces: a POSITIVE st.Version must still
// match the stored row's version or Save returns ErrConcurrentModification; version 0 is an UNCONDITIONAL write
// (a fresh/absent class, or the ratify reset). The stored version is always >= 1 (a fresh row starts at 1;
// every write bumps it), so a subsequent Load hands the next writer the token to guard on.
func (s *MemGraduationStore) Save(_ context.Context, st ClassState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.states[st.OpClass]
	if ok && st.Version > 0 && st.Version != existing.Version {
		return fmt.Errorf("%w: %q (have %d, want %d)", ErrConcurrentModification, st.OpClass, existing.Version, st.Version)
	}
	if ok {
		st.Version = existing.Version + 1
	} else {
		st.Version = 1
	}
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
	// noticeThreshold is the SECOND bar (auto_notice → auto). Ladder-wide; the per-CLASS variation lives in
	// the first bar, which is what ratification sets from tier.
	noticeThreshold int
	// perClass resolves an op-class's own first-bar N (the `promote_threshold` a ratification stored). It may
	// be nil, in which case every class uses the ladder-wide threshold. A resolved value BELOW the ladder-wide
	// threshold is IGNORED rather than honored: this mirrors `CHECK (promote_threshold >= 5)` on
	// opclass_ratified in Go, so the clamp holds even if a row predating the CHECK, or a future non-DB
	// resolver, tries to hand back a faster climb than the compiled default allows. A per-class override may
	// only ever RAISE the bar.
	perClass func(opClass string) (int, bool)
	store    GraduationStore
	states   map[string]ClassState
	logf     func(format string, args ...any)
}

// NewLadder builds a ladder with a promote threshold N and a store. A non-positive threshold clamps to
// DefaultPromoteThreshold. store may be nil (in-memory only; every class starts fail-closed at approve). logf
// is optional (nil → silent).
func NewLadder(threshold int, store GraduationStore, logf func(string, ...any)) *Ladder {
	if threshold < 1 {
		threshold = DefaultPromoteThreshold
	}
	return &Ladder{
		threshold:       threshold,
		noticeThreshold: DefaultNoticeThreshold,
		store:           store,
		states:          map[string]ClassState{},
		logf:            logf,
	}
}

// WithPerClassThreshold installs a per-op-class first-bar resolver — in production, a read of the
// `promote_threshold` a ratification stored on opclass_ratified (spec/028 REQ-2803). Returns l for chaining.
// A resolver that reports a threshold BELOW the ladder-wide one is ignored; see the field comment.
func (l *Ladder) WithPerClassThreshold(f func(opClass string) (int, bool)) *Ladder {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perClass = f
	return l
}

// Threshold returns the ladder's configured promote threshold N (the ladder-wide first bar).
func (l *Ladder) Threshold() int { return l.threshold }

// NoticeThreshold returns the second bar N (auto_notice → auto).
func (l *Ladder) NoticeThreshold() int { return l.noticeThreshold }

// thresholdForLocked resolves the first bar for one op-class. Caller holds l.mu.
func (l *Ladder) thresholdForLocked(opClass string) int {
	if l.perClass == nil {
		return l.threshold
	}
	n, ok := l.perClass(opClass)
	if !ok || n <= l.threshold {
		// Not configured, or configured LOWER — a per-class value may only raise the bar (see the field
		// comment). Falling back rather than honoring it means a corrupt or hostile row buys nothing.
		return l.threshold
	}
	return n
}

// stateLocked returns the current state for opClass, loading it fail-closed on FIRST touch and then serving it
// from the per-process cache. The READ path (GraduatedVerdict/State/LevelOf/GraduationMargin) uses this: it
// tolerates a bounded staleness after a peer worker's durable write, closed on this worker's next Record (which
// reload-on-Records) or a Forget. The WRITE path (Record) uses loadFreshLocked instead, so an actuation
// decision is never persisted over a row a sibling already moved. Caller holds l.mu.
func (l *Ladder) stateLocked(ctx context.Context, opClass string) ClassState {
	if st, ok := l.states[opClass]; ok {
		return st
	}
	return l.loadFreshLocked(ctx, opClass)
}

// loadFreshLocked re-reads opClass's durable state from the store, BYPASSING the first-touch cache, and
// refreshes the cache with what it finds — the reload-on-Record half of TG-146 S3/S4. The durable store is
// authoritative (mirroring the durable breaker, which holds no in-memory state), not a warm cache trusted until
// restart. An absent / unreadable / corrupt persisted state resolves fail-closed to a fresh LevelApprove class
// (never LevelAuto), exactly as the first-touch load always did. With no store the cache IS the state
// (in-memory-only mode): return the cached class, or a fresh approve. Caller holds l.mu.
func (l *Ladder) loadFreshLocked(ctx context.Context, opClass string) ClassState {
	st := ClassState{OpClass: opClass, Level: LevelApprove}
	if l.store == nil {
		if cached, ok := l.states[opClass]; ok {
			return cached
		}
		l.states[opClass] = st
		return st
	}
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
		if st.NoticeRunCount < 0 {
			st.NoticeRunCount = 0
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

// Forget drops opClass's CACHED ladder state so the next read reloads it from the durable store. It is the
// coherence hook the composed-registry refresher fires when an overlay class is (re)admitted (TG-177). The
// ratify verb resets a re-ratified/renamed slug's DURABLE graduation to approve, but that write goes to the
// store directly and bypasses THIS per-process cache — so without an eviction the enforcement path
// (GraduatedVerdict → stateLocked) would keep serving the pre-reset level from a warm cache until the
// process restarted, and the fail-closed reset would never reach the gate that actuates. Evicting is always
// safe: Record writes through to the store before returning, so the durable row is authoritative and nothing
// in-flight is lost; the next touch reloads it (a missing/corrupt row reloads fail-closed to approve). A
// no-op for a slug that was never cached. Concurrent-safe (serializes on the same mutex as every read/write).
func (l *Ladder) Forget(opClass string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.states, opClass)
}

// maxRecordCASRetries bounds how many times Record reloads + re-decides when the durable store reports a
// compare-and-set miss (a peer worker moved the row between our reload and our save). A miss is rare — it needs
// two workers touching the SAME op-class in the same instant — and each retry acts on fresher state, so a small
// bound converges; exhaustion falls through to the fail-closed persistence policy (never a blind clobber).
const maxRecordCASRetries = 5

// Record advances the ladder for opClass by one run outcome and returns the transition (REQ-1514/1515). It is
// concurrency-safe: concurrent Record calls serialize on l.mu, so no torn state can be observed. The state
// machine itself is the PURE applyOutcome; Record adds the reload-on-Record read, the compare-and-set save, and
// the fail-closed persistence policy:
//
//   - RELOAD-ON-RECORD (TG-146 S3/S4): every Record re-reads the durable row (loadFreshLocked) rather than
//     trusting the first-touch cache, so a peer worker's durable demotion is seen, not overwritten from a warm
//     `auto` cache. The durable store is authoritative, mirroring the durable breaker.
//   - COMPARE-AND-SET (TG-146 S3/S4): the save lands only if the row's version still matches what we read; a
//     miss (ErrConcurrentModification) means a sibling wrote in between — reload + re-decide, bounded.
//   - A PROMOTION (approve→auto) that cannot be persisted is REFUSED — the in-memory level stays approve and
//     ErrPromotionNotPersisted is returned, so autonomy is never granted on state that would vanish on restart.
//   - A demotion / non-promoting change whose Save fails is still applied in-memory (fail closed toward
//     safety — always drop autonomy) and the wrapped Save error is returned alongside the applied result.
func (l *Ladder) Record(ctx context.Context, opClass string, outcome RunOutcome) (RecordResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for attempt := 0; ; attempt++ {
		cur := l.loadFreshLocked(ctx, opClass) // reload-on-Record: decide on the freshest durable state
		threshold := l.thresholdForLocked(opClass)
		next, res := applyOutcome(cur, outcome, threshold, l.noticeThreshold)

		if l.store == nil {
			l.states[opClass] = next
			l.log("graduation: %s", res.Reason)
			return res, nil
		}

		err := l.store.Save(ctx, next)
		if err == nil {
			l.states[opClass] = next
			l.log("graduation: %s", res.Reason)
			return res, nil
		}
		if errors.Is(err, ErrConcurrentModification) && attempt < maxRecordCASRetries {
			// A sibling worker moved the durable row between our reload and our save. Evict the stale cache
			// entry so the next loadFreshLocked re-reads, and re-decide on the fresh state — never clobber.
			delete(l.states, opClass)
			continue
		}
		// Persist failed (a real store error, or CAS exhaustion under sustained contention). Apply the
		// fail-closed persistence policy, exactly as before the guard was added.
		if res.Promoted {
			// Refuse to grant autonomy that would not survive a restart. Keep the pre-promotion state. This
			// holds for BOTH climbs: an unpersisted approve→auto_notice and an unpersisted auto_notice→auto are
			// equally refused, because either would have the class acting at a rung the durable record does not
			// show it earned.
			l.states[opClass] = cur
			return RecordResult{
				OpClass: opClass, Outcome: outcome, From: cur.Level, To: cur.Level,
				CleanRunCount: cur.CleanRunCount, NoticeRunCount: cur.NoticeRunCount,
				Threshold: threshold, NoticeThresh: l.noticeThreshold,
				Reason: fmt.Sprintf("promotion persist failed — autonomy withheld, class stays %s", cur.Level),
			}, fmt.Errorf("%w: %v", ErrPromotionNotPersisted, err)
		}
		// Demotion / non-promoting change: apply in-memory (fail closed toward safety), surface the error.
		l.states[opClass] = next
		l.log("graduation: %s", res.Reason)
		return res, fmt.Errorf("policy: graduation state persist failed, applied in-memory: %w", err)
	}
}

// GraduatedVerdict is the graduation→verdict hook (REQ-1514, design.md step 5): it downgrades an ungraduated
// `auto` rule verdict to `approve`, honors a graduated `auto`, and leaves `approve` / `deny` untouched. It
// reads the class's earned level fail-closed (an unknown/unreadable class is LevelApprove). Concurrent-safe.
// The produced verdict still passes through the band-composition never-auto floor (INV-09) downstream — this
// hook decides only whether the class has earned the RIGHT to `auto`, never lifting the floor beneath it.
func (l *Ladder) GraduatedVerdict(ctx context.Context, opClass string, ruleVerdict Verdict) Verdict {
	// TIER FLOOR (spec/013 REQ-1223), applied BEFORE the earned level is consulted: an op-class whose safety
	// tier is not auto-eligible can never be handed `auto`, whatever the ladder says. Clean runs answer "did
	// it work?"; these tiers pose "what if it does not?" — a prune that succeeded a thousand times has proved
	// nothing about the run that deletes the wrong thing. An UNREGISTERED op-class is also not auto-eligible,
	// so an unknown slug fails closed here exactly as it does in the registry.
	if !autoEligibleOpClass(opClass) {
		return graduatedVerdict(LevelApprove, ruleVerdict)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.stateLocked(ctx, opClass)
	return graduatedVerdict(st.Level, ruleVerdict)
}

// GraduationRecord is the NON-SECRET projection of the graduation gate's boundary distance for one decision
// (TG-178, step 5). Present is false — and Margin 0 — when there is no rung to measure against (a fully
// graduated class, or one not auto-eligible); the interceptor emits a margin only when Present, so a
// margin-less graduation row is not mistaken for an at-threshold (0) boundary. Carries no secret: only the
// signed run-count distance to the next rung.
type GraduationRecord struct {
	Margin  int
	Present bool
}

// GraduationMargin reports how many verified-clean runs opClass is from its NEXT rung, as a SIGNED count in
// the same value−threshold convention every gate margin uses (TG-178): the running clean count minus the bar
// it must reach. A climbing class has count < threshold (reaching the bar promotes and resets the count to
// zero), so the margin is always ≤ −1, and exactly −1 is the boundary case the ticket names — "one
// verified-clean outcome short of graduation". `present` is false, and margin 0, when there is no rung to
// measure a distance to: a class already at LevelAuto (fully graduated — no next rung), or one that is not
// auto-eligible (unregistered or a tier that forbids autonomy — GraduatedVerdict floors it without ever
// consulting a rung, so it is not climbing). Observe-only: reads the same fail-closed cached state the
// verdict hook does and mutates nothing. Concurrent-safe (serializes on the ladder's own mutex).
func (l *Ladder) GraduationMargin(ctx context.Context, opClass string) (margin int, present bool) {
	if !autoEligibleOpClass(opClass) {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.stateLocked(ctx, opClass)
	switch st.Level {
	case LevelApprove:
		return st.CleanRunCount - l.thresholdForLocked(opClass), true
	case LevelAutoNotice:
		return st.NoticeRunCount - l.noticeThreshold, true
	default: // LevelAuto — already graduated; no further rung to be short of
		return 0, false
	}
}

// autoEligibleOpClass resolves an op-class slug to its declared safety tier and reports whether that tier may
// ever reach autonomy. An op-class absent from the registry is NOT eligible (fail closed) — it has no tier,
// and a missing declaration must never read as permission.
// opClassTier resolves an op-class slug to its declared safety tier. It is a package var ONLY so an oracle
// can substitute a registry containing an auto-forbidding tier: every class shipped today is auto-eligible,
// so without this seam the floor below is UNPROVABLE — and an unprovable safety property is how this floor
// shipped unreachable in the first place. Production always uses the real registry.
var opClassTier = func(opClass string) (tier string, registered bool) {
	spec, ok := opschema.Lookup(opClass)
	if !ok {
		return "", false
	}
	return spec.SafetyTier, true
}

func autoEligibleOpClass(opClass string) bool {
	tier, ok := opClassTier(opClass)
	if !ok {
		return false
	}
	return opschema.AutoEligible(tier)
}

// tierForbidsAuto reports whether opClass is REGISTERED and its declared tier forbids autonomy. An
// unregistered slug returns false — it has no tier to violate and is floored at the decision point instead.
func tierForbidsAuto(opClass string) bool {
	tier, ok := opClassTier(opClass)
	return ok && !opschema.AutoEligible(tier)
}

// isEmbeddedClass reports whether an op-class lives in the EMBEDDED, lockstep-hashed registry — the predicate
// the AUTO ceiling rests on (ADR-0016 decision 2, spec/028 REQ-2808).
//
// It deliberately does NOT go through opschema.Lookup. Lookup answers over the COMPOSED registry, which
// includes runtime-ratified overlay classes; asking it here would let a ratification lift its own ceiling and
// collapse the two tamper domains into one. A package var for the same reason opClassTier is one: every class
// shipped today is embedded, so without the seam the ceiling is UNPROVABLE — and an unprovable safety property
// is how a control ships unreachable. Production always uses the real embedded registry.
var isEmbeddedClass = opschema.IsEmbedded

func (l *Ladder) log(format string, args ...any) {
	if l.logf != nil {
		l.logf(format, args...)
	}
}
