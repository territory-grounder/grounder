// Package trace assembles one incident's governed decision path into an ordered, non-secret, read-only
// per-step walk — the decision-tracer read model (spec/020, REQ-2000/2011). It is OBSERVE-ONLY: it sits
// OFF every actuation chokepoint, authorizes nothing, and changes no mutation posture (REQ-2002). It reads
// only the durable correlation spine that the decision path already committed and never re-enters that path;
// it imports no safety, policy, or actuation package by construction, so no value it returns can gate an
// actuator. Every credential-bearing field it carries is a reference or a scheme, never a secret value
// (INV-13).
//
// COARSE-FIRST. The walk is stitched from the decision BOUNDARIES that leave a durable, external_ref-joinable
// row today: classify (session_risk_audit), propose (session_triage — with the confidence scalar, migration
// 0024), predict (infragraph_prediction — with external_ref, migration 0026, and the LLM-free falsify score
// = the verified outcome), and verify (action_verdict). The finer per-interceptor-gate and per-ReAct-cycle
// rows (T-020-7/8/9) enrich the SAME Step record additively as they land — the record type below already
// carries every decision-grade field, so a richer source fills a field that is zero today without any schema
// or DTO rewrite. This is the two-layer, additive-enrichment design (REQ-2017): the record is the stable
// contract; the assembler fills what the spine durably holds.
package trace

import "time"

// StepKind names a decision boundary in the governed walk, in decision-boundary order. Each landed source
// contributes its boundary; unbuilt sources simply contribute no step yet (never a fabricated one — INV-15).
type StepKind string

const (
	// StepClassify is the risk classifier's committed admission decision (session_risk_audit): band, risk
	// level, the content-hashed action, and the signals that drove the classification.
	StepClassify StepKind = "classify"
	// StepPropose is the agent's parsed proposal (session_triage): the op, the conclusion, the stated
	// confidence scalar, and whether a machine prediction was committed alongside it.
	StepPropose StepKind = "propose"
	// StepPredict is the committed consequence prediction (infragraph_prediction) made OUTSIDE the LLM, plus
	// the LLM-free mechanical verified outcome (the falsify tp/fp/fn score) once the observation window scored
	// it — the agent NEVER adjudicates its own outcome (INV-10).
	StepPredict StepKind = "predict"
	// StepVerify is the deterministic verifier's mechanical verdict for the sealed action (action_verdict):
	// match | partial | deviation, or empty when no verdict exists yet.
	StepVerify StepKind = "verify"

	// StepAgentCycle is one agent ReAct investigation cycle (agent_step, T-020-8): the SCRUBBED thought, the
	// tool invoked, the scrubbed observation, and the per-cycle outcome. These sit BETWEEN classify and propose
	// — the reasoning the agent ran before it proposed. Non-secret by construction (Scrub ran before the write,
	// INV-08/INV-13); the thought is DATA the inspector shows, never control flow re-entered.
	StepAgentCycle StepKind = "agent-cycle"
	// StepPolicy is the policy engine's authorization decision for the sealed action (policy_decision, T-020-3):
	// the composed verdict, the in-force ruleset bundle_version, the FULL deny-overrides matched-rule list, and
	// the packet-tracer reason — the WHY of the authorization. It sits at execute time, after predict.
	StepPolicy StepKind = "policy"
	// StepGate is one interceptor gate in the ordered execute chain (interceptor_gate_verdict, T-020-7):
	// admission → never-auto-floor → structure → evidence → territory → verifiability → policy → mode-chokepoint
	// → execute → verify, each with its pass/refuse/mechanical verdict and non-secret reason. Present only after
	// a governed actuation traversed the interceptor.
	StepGate StepKind = "gate"
	// StepCredential is one machine-plane credential resolution (credential_resolution, spec/016 REQ-1617): the
	// non-secret identity the agent's investigation resolved for a target — the resolved user, the connection
	// scheme, and the SCHEME of the key reference only (env/file/…), NEVER key material or a full ref value
	// (INV-13). It sits with the investigation, after the agent cycles.
	StepCredential StepKind = "credential"
	// StepScreen is the admission-time screen (session_risk_audit.signals_json, migration 0003): the signals
	// that drove the band — the never-auto floor reason, blast-radius / stateful / input-injection screen, and
	// the poll_reason for a POLL_PAUSE. DATA-only projection of the classifier's committed signals (INV-08),
	// no secret (INV-13). Rendered at the screen slot; absent when the classifier recorded no signals.
	StepScreen StepKind = "screen"
	// StepRag is the retrieval-context boundary (session_triage.skill_loads, migration 0010): the composed-seed
	// provenance the proposal was built from — the skill/precedent artifacts retrieved and composed into the
	// agent's seed (name@version#id:origin[:arm], spec/014). DATA-only projection of the committed provenance
	// strings (non-secret by construction, INV-13); it sits at the retrieval stage, before the agent loop.
	// Absent when the proposal recorded no composed-seed loads.
	StepRag StepKind = "rag"
	// StepIngest is the front-door boundary (ingest_alert, migration 0033): the accepted, normalized alert as
	// the ingest tier admitted it — the source, rule, severity, host/site and operational summary. DATA-only
	// projection of the durable alert record (non-secret normalized envelope fields only, INV-13); it is the
	// FIRST boundary. Absent when no durable front-door record exists for the ref (e.g. a pull-minted session
	// that bypassed the front door), in which case the console shows the honest light scaffold.
	StepIngest StepKind = "ingest"
	// StepCorrelate is the deterministic suppression/correlation boundary (governance_ledger, action_id
	// "suppress:"+external_ref): the dedup → severity-floor → known-pattern → blast-radius → scheduled-reboot →
	// active-memory chain the runner ran BEFORE the model was spent, and its outcome (escalate = proceeded to
	// investigation; suppressed/notice otherwise) + the phase:reason. A tracer session escalated (a suppressed
	// alert has no session). DATA-only projection of the committed governance decision (non-secret, INV-13); it
	// sits between ingest and classify. Absent for a session that predates the suppression-ledger wiring.
	StepCorrelate StepKind = "correlate"
)

// Step is one decision boundary in a session's governed walk, carrying every decision-grade field the
// inspector shows for a step (REQ-2000). A field is the zero value when the boundary does not carry it or its
// durable source has not landed yet — never a fabricated placeholder. Credential fields are a reference
// (SecretRef) or a scheme name only; a raw secret value must never reach this struct (INV-13).
type Step struct {
	Seq   int       `json:"seq"`   // decision-boundary order, 0-based, ascending
	Kind  StepKind  `json:"kind"`  // which boundary this is
	Label string    `json:"label"` // human-readable boundary label
	At    time.Time `json:"at"`    // when the boundary's row was committed

	// Decision-grade fields (REQ-2000). Empty/zero when not applicable to this boundary or not yet sourced.
	Rule          string   `json:"rule,omitempty"`           // the rule that fired at this boundary
	MatchedRules  []string `json:"matched_rules,omitempty"`  // the matched-rule ids (policy Decide)
	Reason        string   `json:"reason,omitempty"`         // the rationale for this boundary's outcome
	Tools         []string `json:"tools,omitempty"`          // tools invoked (agent cycles)
	Prompts       []string `json:"prompts,omitempty"`        // prompt/skill provenance strings
	Skills        []string `json:"skills,omitempty"`         // composed-seed skill loads
	Confidence    float64  `json:"confidence"`               // the stated 0..1 confidence at this boundary
	MinConfidence float64  `json:"min_confidence"`           // the policy min_confidence threshold in force
	Band          string   `json:"band,omitempty"`           // the governance band at this boundary
	Verdict       string   `json:"verdict,omitempty"`        // the mechanical verdict at this boundary
	Gate          string   `json:"gate,omitempty"`           // the interceptor gate name for a gate boundary
	BundleVersion string   `json:"bundle_version,omitempty"` // the in-force ACL/ruleset bundle version

	// Credential provenance — references and schemes only (INV-13). Never a secret value. On a credential-resolve
	// step (StepCredential) these carry the resolved identity's SCHEMES: CredentialScheme is the connection scheme
	// (e.g. ssh) and CredentialRef is the key-reference SCHEME (e.g. env/file), the token before the first ':' —
	// never the ref path or key material.
	CredentialRef    string `json:"credential_ref,omitempty"`    // a key-reference SCHEME (env/file/…) or SecretRef scheme, never a path or value
	CredentialScheme string `json:"credential_scheme,omitempty"` // the connection scheme (ssh) or credential-source scheme — a scheme, never a value

	// PlanOps is the sealed action projected to structured end-state ops (the commit/prediction boundary). Each
	// op is add|change|destroy + a human-readable target — a projection of the durable committed action
	// (action_manifest.action, INV-07), NEVER a fabricated before→after delta (INV-15).
	PlanOps []PlanOp `json:"plan_ops,omitempty"`
}

// PlanOp is one structured end-state operation the sealed action commits to: Op is add|change|destroy (the
// mutation polarity, classified from the op-class), T is the human-readable target. Non-secret by construction
// (a projection of the committed action's target/op-class/params, never key material — INV-13).
type PlanOp struct {
	Op string `json:"op"`
	T  string `json:"t"`
}

// Status is a session's lifecycle state, derived SOLELY from which durable spine rows exist — never asserted
// by the model about itself. It lets the inspector cover queued, running, and executed sessions (REQ-2010's
// read half) without inventing an outcome for a session that has none.
type Status string

const (
	StatusReceived   Status = "received"   // accepted at the front door (ingest_alert), not yet classified
	StatusClassified Status = "classified" // classified, no proposal yet (queued)
	StatusProposed   Status = "proposed"   // a proposal exists, no verdict yet (running/awaiting)
	StatusExecuted   Status = "executed"   // a mechanical verdict exists (executed + verified)
	StatusStopped    Status = "stopped"    // the agent proposed no action (a STOP)
)

// SessionTrace is one incident stitched into an ordered, non-secret, read-only per-step walk (REQ-2011). The
// header fields summarize the session; Steps are the decision boundaries in ascending Seq order. Verdict is
// the sealed action's mechanical verdict, empty when none exists yet (the inspector renders "pending", never
// a fabricated outcome — INV-15).
type SessionTrace struct {
	ExternalRef  string    `json:"external_ref"`
	Host         string    `json:"host,omitempty"`
	AlertRule    string    `json:"alert_rule,omitempty"`
	Band         string    `json:"band,omitempty"`
	RiskLevel    string    `json:"risk_level,omitempty"`
	ActionID     string    `json:"action_id,omitempty"`
	PlanHash     string    `json:"plan_hash,omitempty"`
	Status       Status    `json:"status"`
	Confidence   float64   `json:"confidence"`
	Verdict      string    `json:"verdict,omitempty"`
	ClassifiedAt time.Time `json:"classified_at"`
	Steps        []Step    `json:"steps"`
}
