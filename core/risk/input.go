// Package risk implements Territory Grounder's three-band RiskClassifier: a typed, deterministic
// admission gate that emits AUTO / AUTO_NOTICE / POLL_PAUSE and writes exactly one session_risk_audit
// row per classification. Its zero value and every error path fail closed to POLL_PAUSE by
// construction, composing over the inviolable core/safety primitives.
//
// Provenance: [F] spec/001 (BEH-1), the predecessor scripts/classify-session-risk.py band engine,
// re-expressed under the typed spine · [O] INV-06/INV-07/INV-09/INV-10/INV-11 · [R] paradigm-rule 2/8
// (single-org approver graph; the mechanical never-auto floor is non-configurable). The classifier is
// Phase 2 behavior; it is built and tested read-only here and only enforces once mutation is earned.
package risk

import (
	"github.com/territory-grounder/grounder/core/safety"
)

// Reversibility is the reversibility class of the proposed action. The zero value is Irreversible —
// the safest default — so an unclassified action is treated as irreversible and can never auto-resolve
// (REQ-004: unknown action-class implies the never-auto ceiling).
type Reversibility int

const (
	Irreversible    Reversibility = iota // zero value — never-auto; the safe default for an unknown class
	ReversibleMixed                      // may modify but is reversible/recoverable
	Reversible                           // fully reversible (read-only or trivially undone)
)

func (r Reversibility) String() string {
	switch r {
	case Reversible:
		return "reversible"
	case ReversibleMixed:
		return "reversible-mixed"
	default:
		return "irreversible"
	}
}

// EvidenceRef is an orchestrator-captured ToolResult reference. A claim is admissible only if it cites
// at least one ref that is captured (not agent free-text), successful, recent, and target-relevant —
// a bare fenced block is rejected by construction (REQ-008, INV-11).
type EvidenceRef struct {
	ToolResultID     string
	Captured         bool // captured by the orchestrator (never trusted agent free-text)
	Successful       bool
	RecentlyObserved bool
	TargetRelevant   bool
}

// Bound reports whether this evidence ref is admissible on all four axes.
func (e EvidenceRef) Bound() bool {
	return e.Captured && e.Successful && e.RecentlyObserved && e.TargetRelevant
}

// GatedInput is the typed, already-validated input to the classifier. It is constructible only
// downstream of ingest validation (spec/006) and the prediction gate (spec/002), so the classifier
// never sees raw model text — it sees derived, typed signals. Producing it requires the load-bearing
// fields; missing safety-relevant fields default to their most-restrictive value.
type GatedInput struct {
	ExternalRef string // correlation key (ADR-0010)
	ActionID    string // the content-hashed action this classification binds to (INV-07)
	PlanHash    string // joins to the committed prediction (spec/002)
	RiskLevel   string // e.g. "low", "high", "novel-incident" — the signal summary for the audit row

	OpClass    string        // op class, matched against the mechanical never-auto floor
	Reversible Reversibility // reversibility class (zero = Irreversible = safe)

	Jailbreak    bool // the untrusted input tripped the prompt-injection/jailbreak screen (core/screen) → POLL_PAUSE
	CanaryPinned bool // a deployment-declared canary (host,op) — force POLL_PAUSE so the FIRST staged mutations require a human vote (REQ-009). Set by the activity from the loaded policy; only ever RAISES review, inert when unconfigured.
	// UngraduatedClass reports that this op-class has NOT earned autonomy on the graduation ladder, so the
	// policy engine WILL compose a verdict of `approve` (needs a recorded human approval) at execute time.
	// Forcing POLL_PAUSE here is what makes that approval ASKABLE. Without it the band decides whether a poll
	// exists while the policy verdict decides whether approval is needed, and an ungraduated class landing in
	// an AUTO-banded incident is refused with nobody ever asked — measured 13 sessions in 24h, each a wasted
	// fault and a graduation opportunity the class can never get back. Only ever RAISES review; inert when the
	// resolver is unwired, exactly like CanaryPinned.
	UngraduatedClass bool
	// RationaleHostMismatch reports that the model's STATED rationale named at least one host and none of
	// them is the sealed target (TG-317, TG-154 §2/T7). A HEURISTIC, and it only ever RAISES review.
	//
	// The seam: a proposal whose rationale reads "restart nginx on web01" can carry target db01 and every
	// gate passes — grammar valid, op-class allowlisted, argv built deterministically, evidence bound by
	// target equality, prediction committed. The rationale is the ONE field a human on a POLL_PAUSE
	// actually reads, so a notice can say one thing while the sealed action does another, and the vote
	// authorizes the action rather than the prose.
	//
	// It escalates and never refuses: a refusal on a text heuristic takes the estate offline on a wording
	// change. Naming NO host abstains — silence is not evidence, and treating it as disagreement would
	// poll everything and get the check disabled.
	RationaleHostMismatch bool
	// RationaleMismatchDetail renders the disagreement for the poll notice — which host the prose named
	// against which host the action targets. A poll that says "the rationale disagrees" without saying HOW
	// is a poll nobody can adjudicate, and adjudicating it is the whole reason this escalates.
	RationaleMismatchDetail string
	// BandFloor / BandFloorApplies / BandFloorReason — THE ONE COMPOSITION SEAM for a declared band floor
	// (spec/028 REQ-2809, spec/026 REQ-2611). The classifier composes it over the computed band in the SAFE
	// DIRECTION ONLY: a floor may raise the approval bar and may never lower it.
	//
	// Two producers, deliberately one field. The graduation ladder declares an AUTO_NOTICE floor for a class
	// sitting at the auto_notice rung — it has earned the right to act, not the right to act unobserved. The
	// actor-evidence policy (proposal.FloorFor) declares a POLL_PAUSE floor for authored-action evidence — a
	// human already touched this, so ask. Giving each its own bespoke field would put band adjustment in two
	// places, and a second place bands can be adjusted is a second place they can be adjusted DOWNWARD.
	//
	// BandFloorApplies exists because safety.Band's zero value is BandPollPause — the MOST restrictive band.
	// An unset BandFloor would therefore clamp every action in the estate to a human poll. The explicit
	// applies-flag mirrors proposal.FloorFor's own (Band, bool) signature, so "no floor declared" survives
	// the trip through this struct instead of decaying into "floor everything".
	BandFloor        safety.Band
	BandFloorApplies bool
	BandFloorReason  string // why the floor was declared, recorded on the audit row (e.g. "ladder-auto-notice")
	// Actor-attribution dispositions (spec/023, REQ-2301/2304/2310) — set by the attribute activity from
	// typed reader evidence, mapped by rules-as-data. Like the canary pin they only ever RAISE review;
	// unattributable sets none of them (the pre-feature ladder, REQ-2303).
	AttributionStandDown bool // the change is attributed to a sanctioned non-TG principal → coordinate, never undo it (poll "actor-attributed-authorized")
	AttributionSecurity  bool // an unsanctioned actor / a mutation with no audit entry → security escalation (poll "actor-attributed-suspicious" + security_escalation signal)
	AttributionEscalate  bool // an unmapped disposition or a non-suspicious contradiction → escalate to the approver graph (poll "actor-attribution-escalate")
	StatefulTarget       bool // the action targets a stateful workload (DB/queue/store/statefulset) — a mutation of it never auto-resolves
	ServerDestructive    bool // the ACTUAL op (server-derived, not the model's declared class) is irreversible/destructive → never auto
	SelfProtectedRestart bool // a restart/reload targeting the platform's OWN control-plane service — never auto (would orphan the session)

	HasPrediction bool // a committed plan_hash-keyed prediction exists for (alert_rule, host)
	// HasVerdict / Verdict carry the target's most recent RELEVANT durable mechanical verdict (REQ-015):
	// relevant meaning RULE-FAMILY scoped (folded through the one family authority, core/knowledge.CanonicalRule)
	// and RECENCY BOUNDED by an operator-declared window (TG_PRIOR_VERDICT_WINDOW). It is authored only by the
	// deterministic verifier (INV-10) and read from the durable ACTUATION ledger — never from the propose-path
	// world-model scores, which measure a different claim (migration 0042).
	//
	// The pair is FAIL-TOWARD-CAUTION by construction: absent, unknown, or unreadable evidence sets
	// HasVerdict=false, which leaves the classification byte-identical to the pre-feature ladder. The branch it
	// drives can only ever RAISE review — a deviation (or any verdict the verifier did not validly produce)
	// polls; a match or partial changes nothing — so no state of this input can make a band LAXER.
	HasVerdict bool
	Verdict    safety.Verdict
	// VerdictKey is the EVIDENCE behind a prior-verdict poll: the `host|canonical-rule-family` signature the
	// deciding verdict was read under. Without it the classifier records THAT the rule fired and never what it
	// read, and the fold (raw rule → family) is not reconstructible from the audit row alone — the same
	// unauditability REQ-014 exists to close for novelty. Empty ⇒ omitted, never written blank.
	VerdictKey    string
	NovelIncident bool // ood:novel-incident — no learned prior class
	// NoveltyKey and NoveltyCount are the EVIDENCE behind NovelIncident: the signature the prior-incident
	// count was read under, and the count it returned. Without them a novelty poll is unauditable — the
	// classifier records THAT it fired and never what it read, and the corpus it consulted is a mutable file
	// with no history, so nobody can reconstruct afterwards whether the poll was right. Measured 2026-07-28:
	// 140 such polls in 7 days, ZERO of which mutated, and no way to tell how many were correct.
	NoveltyKey       string
	NoveltyCount     int
	CriticalityTier  bool // the target host is on an org criticality tier (P0)
	BlastRadiusWide  bool // the predicted blast-radius exceeds the configured threshold
	HighRiskCategory bool // the alert category (maintenance/security-incident/deployment) forces a poll by default (safety.HighRiskCategory)

	SilentCognitionGuard bool          // the silent_cognition_guard policy is active
	AutoResolveMarked    bool          // the proposal carried an [AUTO-RESOLVE] marker
	Evidence             []EvidenceRef // orchestrator-captured ToolResult evidence

	Signals map[string]string // normalized signals recorded on the audit row
}

// Decision is the required-field output of the classifier. Producing a Decision with a missing field
// is a compile error, which is how the persistence contract (INV-19) is enforced at the type level.
type Decision struct {
	Band                 safety.Band
	RiskLevel            string
	AutoApproved         bool // true only for {AUTO, AUTO_NOTICE}
	NotifyRequired       bool // AUTO_NOTICE sets this
	AutoProceedOnTimeout bool // ALWAYS false — a poll never proceeds on timeout
	AutoResolve          bool // the [AUTO-RESOLVE] marker is retained
	Signals              map[string]string
	ActionID             string
	PlanHash             string
}
