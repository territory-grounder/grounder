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

	Jailbreak            bool // the untrusted input tripped the prompt-injection/jailbreak screen (core/screen) → POLL_PAUSE
	CanaryPinned         bool // a deployment-declared canary (host,op) — force POLL_PAUSE so the FIRST staged mutations require a human vote (REQ-009). Set by the activity from the loaded policy; only ever RAISES review, inert when unconfigured.
	// Actor-attribution dispositions (spec/023, REQ-2301/2304/2310) — set by the attribute activity from
	// typed reader evidence, mapped by rules-as-data. Like the canary pin they only ever RAISE review;
	// unattributable sets none of them (the pre-feature ladder, REQ-2303).
	AttributionStandDown bool // the change is attributed to a sanctioned non-TG principal → coordinate, never undo it (poll "actor-attributed-authorized")
	AttributionSecurity  bool // an unsanctioned actor / a mutation with no audit entry → security escalation (poll "actor-attributed-suspicious" + security_escalation signal)
	AttributionEscalate  bool // an unmapped disposition or a non-suspicious contradiction → escalate to the approver graph (poll "actor-attribution-escalate")
	StatefulTarget       bool // the action targets a stateful workload (DB/queue/store/statefulset) — a mutation of it never auto-resolves
	ServerDestructive    bool // the ACTUAL op (server-derived, not the model's declared class) is irreversible/destructive → never auto
	SelfProtectedRestart bool // a restart/reload targeting the platform's OWN control-plane service — never auto (would orphan the session)

	HasPrediction   bool           // a committed plan_hash-keyed prediction exists for (alert_rule, host)
	HasVerdict      bool           // a post-execution verdict exists
	Verdict         safety.Verdict // the mechanical verdict, if HasVerdict
	NovelIncident    bool          // ood:novel-incident — no learned prior class
	CriticalityTier  bool          // the target host is on an org criticality tier (P0)
	BlastRadiusWide  bool          // the predicted blast-radius exceeds the configured threshold
	HighRiskCategory bool          // the alert category (maintenance/security-incident/deployment) forces a poll by default (safety.HighRiskCategory)

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
