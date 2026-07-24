package trace

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SpineRecords is the durable correlation-spine state for ONE external_ref, as plain values — the raw input
// to the pure Assemble. Each sub-record's Present flag says whether that decision boundary left a durable row
// (a queued session has only Classification; an executed one has all four). This is the seam the CI oracle
// drives with an in-memory value and the pgx SpineReader fills from Postgres — the same repository-interface
// + in-memory-fake discipline the rest of the read surfaces use, so the assembler is testable with no
// database.
type SpineRecords struct {
	Ingest         IngestRecord         // ingest_alert (migration 0033) — the accepted front-door alert
	Correlate      CorrelateRecord      // governance_ledger "suppress:"+ref — the deterministic suppression/correlation decision
	Classification ClassificationRecord // session_risk_audit (the admission decision)
	Triage         TriageRecord         // session_triage (the parsed proposal + confidence)
	AgentCycles    []AgentCycleRecord   // agent_step (T-020-8) — the ReAct investigation cycles, cycle-ordered
	Credentials    []CredentialRecord   // credential_resolution (REQ-2015) — the non-secret identities resolved
	Prediction     PredictionRecord     // infragraph_prediction (the out-of-LLM consequence prediction + score)
	Policy         PolicyRecord         // policy_decision (T-020-3) — the authorization detail (verdict/bundle/rules)
	GateVerdicts   []GateVerdictRecord  // interceptor_gate_verdict (T-020-7) — the ordered execute-gate chain
	Verdict        VerdictRecord        // action_verdict (the mechanical verdict)
	Commit         CommitRecord         // action_manifest.action (INV-07) — the sealed end-state plan ops
}

// CorrelateRecord is the deterministic suppression/correlation decision committed to the governance ledger
// (governance_ledger, action_id "suppress:"+external_ref) BEFORE any model was spent: the dedup/flap/burst/
// correlation + suppression chain outcome (escalate | suppressed | notice) and its phase:reason. Non-secret by
// construction (a governance decision projection, INV-13). Present is false when no such row exists (a session
// that predates the suppression-ledger wiring).
type CorrelateRecord struct {
	Present   bool
	Outcome   string // the suppression outcome — escalate (proceeded) | suppressed | notice
	Reason    string // phase:reason — the stage that decided + its detail
	DecidedAt time.Time
}

// IngestRecord is the durable front-door record of the accepted, normalized alert (ingest_alert, migration
// 0033) — the ingest boundary. Every field is a NON-SECRET normalized envelope field (source/rule/severity/
// host/site/summary), never the raw payload or a credential (INV-13). Present is false when no durable
// front-door record exists for the ref (e.g. a pull-minted session that bypassed the front door).
type IngestRecord struct {
	Present    bool
	SourceType string
	AlertRule  string
	Severity   string
	Host       string
	Site       string
	Summary    string
	ReceivedAt time.Time
}

// CommitRecord carries the sealed action projected to structured end-state ops (action_manifest.action jsonb,
// content-hashed by action_id, INV-07). Present is false when no manifest was sealed; the ops are a projection
// of the committed action, never a fabricated before→after delta (INV-15).
type CommitRecord struct {
	Present bool
	PlanOps []PlanOp
}

// ClassificationRecord is the risk classifier's committed admission row (session_risk_audit).
type ClassificationRecord struct {
	Present      bool
	Band         string
	RiskLevel    string
	ActionID     string
	PlanHash     string
	Signals      map[string]string
	AutoApproved bool
	CreatedAt    time.Time
}

// TriageRecord is the agent's parsed proposal row (session_triage), carrying the stated confidence scalar
// (migration 0024) and the composed-seed skill-load provenance.
type TriageRecord struct {
	Present    bool
	Host       string
	AlertRule  string
	Band       string
	Outcome    string
	Proposed   bool
	Op         string
	Conclusion string
	Confidence float64
	SkillLoads []string
	Predicted  bool
	Prediction string
	CreatedAt  time.Time
	// composed-seed identity (session_identity, migration 0027) — the prompt/seed/model provenance the propose
	// step surfaces (REQ-2009/REQ-2000). Non-secret identifiers (a version string, a seed HASH, a tier name).
	PromptVersion string
	SeedHash      string
	ModelTier     string
}

// PredictionRecord is the committed consequence prediction (infragraph_prediction), joined by external_ref
// (migration 0026). Scored reports whether the observation window has written the LLM-free falsify score
// (tp/fp/fn) back onto it — the verified outcome (INV-10).
type PredictionRecord struct {
	Present     bool
	ActionID    string
	PlanHash    string
	Scored      bool
	TP, FP, FN  int
	CommittedAt time.Time
}

// VerdictRecord is the deterministic verifier's mechanical verdict (action_verdict): match | partial |
// deviation, Present=false when no verdict exists yet.
type VerdictRecord struct {
	Present   bool
	Verdict   string
	CreatedAt time.Time // the verifier's stamp (action_verdict.created_at) — zero when no verdict yet
}

// AgentCycleRecord is one agent ReAct cycle (agent_step, T-020-8) — the scrubbed thought, the tool invoked,
// the scrubbed observation, and the per-cycle outcome for cycle N. Every field is non-secret by construction
// (Scrub ran before the write, INV-08/INV-13); the reader hands plain values so core/trace imports no agent
// or policy package.
type AgentCycleRecord struct {
	Cycle       int
	Thought     string
	Tool        string
	Observation string
	Outcome     string
	CreatedAt   time.Time
}

// PolicyRecord is the policy engine's Decide detail for the sealed action (policy_decision, T-020-3): the
// composed verdict, the in-force ruleset bundle version, the FULL deny-overrides matched-rule list projected
// NON-SECRET to "id → verdict" strings (never argv/host/credential, INV-13), the packet-tracer reason, and the
// active mode. Present=false when no policy decision was recorded for the action.
type PolicyRecord struct {
	Present       bool
	Verdict       string
	BundleVersion string
	MatchedRules  []string
	Reason        string
	Mode          string
	MinConfidence float64 // the min_confidence threshold in force for this decision (migration 0019), REQ-2000
	CreatedAt     time.Time
}

// GateVerdictRecord is one interceptor-gate verdict (interceptor_gate_verdict, T-020-7) in the ordered execute
// chain: its 1-based ordinal, the gate name, its pass/refuse/mechanical verdict, and a non-secret reason.
// Present only after a governed actuation traversed the interceptor.
type GateVerdictRecord struct {
	Ordinal   int
	Gate      string
	Verdict   string
	Reason    string
	CreatedAt time.Time
}

// CredentialRecord is one machine-plane credential resolution (credential_resolution, REQ-2015/REQ-1617): the
// NON-SECRET identity the investigation resolved for a target — the target label, the outcome, the resolved
// user, the connection scheme, and the SCHEME of the key reference only (env/file/…), plus the winning source.
// It NEVER carries key material or a full SecretRef value (INV-13, the write path is secret-free by construction).
type CredentialRecord struct {
	Target       string
	Outcome      string
	User         string
	Scheme       string
	KeyRefScheme string
	Source       string
	CreatedAt    time.Time
}

// SpineReader loads the durable spine for one external_ref. Authority is resolved inside the implementation
// against the principal at the call site (INV-12); this interface is the pure seam the assembler and its
// oracle share. NotFound is signalled by an ErrNotFound return so the handler can 404 rather than serve an
// empty fabricated walk.
type SpineReader interface {
	Load(ctx context.Context, externalRef string) (SpineRecords, error)
}

// ErrNotFound is returned by a SpineReader when no session exists for the external_ref (no classification row
// and nothing to assemble) — the detail endpoint maps it to 404, never to an empty 200 body.
var ErrNotFound = fmt.Errorf("trace: session not found")

// Assemble stitches the durable spine into an ordered, non-secret per-step walk. It is PURE and
// deterministic (no I/O, no clock, no side effect) so it sits off every chokepoint by construction (REQ-2002)
// and is fully CI-testable. Steps are emitted in fixed decision-boundary order — classify → [agent ReAct
// cycles] → propose → predict → [policy authorization] → [interceptor gate chain] → verify — and only for
// boundaries whose durable row is Present (or, for the cycle/gate lists, one step per row the reader holds),
// so a queued/running session yields its partial prefix rather than a fabricated tail (REQ-2010 read half,
// INV-15). The finer agent-cycle/policy/gate rows (T-020-7/8/3) enrich the walk additively: absent them, the
// coarse classify→propose→predict→verify prefix is unchanged (the two-layer additive design, REQ-2017/REQ-2001).
func Assemble(externalRef string, rec SpineRecords) SessionTrace {
	t := SessionTrace{ExternalRef: externalRef, Steps: []Step{}}

	// Header — sourced from the durable rows, never asserted by the model.
	if rec.Classification.Present {
		t.Band = rec.Classification.Band
		t.RiskLevel = rec.Classification.RiskLevel
		t.ActionID = rec.Classification.ActionID
		t.PlanHash = rec.Classification.PlanHash
		t.ClassifiedAt = rec.Classification.CreatedAt
	}
	if rec.Triage.Present {
		t.Host = rec.Triage.Host
		t.AlertRule = rec.Triage.AlertRule
		t.Confidence = rec.Triage.Confidence
		if t.Band == "" {
			t.Band = rec.Triage.Band
		}
	}
	// Fall back to the front-door record for the header identity when no classification/triage row exists yet
	// (an ingest-only session): the durable ingest record already carries the host and rule.
	if t.Host == "" {
		t.Host = rec.Ingest.Host
	}
	if t.AlertRule == "" {
		t.AlertRule = rec.Ingest.AlertRule
	}
	if rec.Verdict.Present {
		t.Verdict = rec.Verdict.Verdict
	}
	t.Status = deriveStatus(rec)

	// Steps — fixed decision-boundary order; each emitted only when its durable row exists.
	seq := 0
	// ingest (migration 0033 ingest_alert) — the accepted front-door alert, the FIRST boundary. DATA-only
	// projection of the durable record (non-secret normalized envelope fields, INV-13); emitted only when a
	// durable front-door record exists (a pull-minted session that bypassed the front door has none, and the
	// console shows its light scaffold instead — never a fabricated ingest).
	if rec.Ingest.Present {
		ing := rec.Ingest
		reason := ing.AlertRule
		if ing.Severity != "" {
			reason += " · " + ing.Severity
		}
		if ing.Host != "" {
			reason += " · " + ing.Host
		}
		if ing.Summary != "" {
			reason += "\n" + ing.Summary
		}
		label := "Ingested"
		if ing.SourceType != "" {
			label = "Ingested (" + ing.SourceType + ")"
		}
		t.Steps = append(t.Steps, Step{Seq: seq, Kind: StepIngest, Label: label, At: ing.ReceivedAt, Reason: reason})
		seq++
	}
	// correlate (governance_ledger "suppress:"+ref) — the deterministic dedup/flap/burst/correlation +
	// suppression chain the runner ran before the model. DATA-only projection of the committed governance
	// decision; emitted only when the decision was durably recorded (a session predating the wiring has none →
	// the console shows the honest scaffold). A tracer session escalated (proceeded); the outcome + phase:reason
	// are shown, never a fabricated correlation.
	if rec.Correlate.Present {
		c := rec.Correlate
		reason := c.Reason
		verdict := ""
		switch c.Outcome {
		case "escalate":
			verdict = "clean" // proceeded to investigation — no duplicate/known-pattern suppressed it
		case "suppressed", "notice":
			verdict = c.Outcome
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepCorrelate, Label: "Correlate / suppression", At: c.DecidedAt,
			Reason: reason, Verdict: verdict,
		})
		seq++
	}
	if rec.Classification.Present {
		c := rec.Classification
		reason := "admitted"
		if c.AutoApproved {
			reason = "admitted (auto-approved)"
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepClassify, Label: "Risk classification", At: c.CreatedAt,
			Band: c.Band, Rule: c.RiskLevel, Reason: reason,
		})
		seq++
		// screen (migration 0003 signals_json) — the admission screen. Emitted for EVERY classified session: when
		// the classifier recorded pause/floor signals (poll_reason / never-auto-floor / blast-radius) show them;
		// otherwise (a clean AUTO admission that recorded no pause signal) PROJECT the durable admission decision
		// from the classification row — band, auto-approval, risk — so the screen boundary reports the clean pass
		// it actually made rather than a false "no data yet". DATA-only, never a fabricated signal (INV-15).
		{
			reason := ""
			if screen := screenSignals(c.Signals); len(screen) > 0 {
				reason = strings.Join(screen, " · ")
			} else {
				reason = "clean admission — no pause/floor/novelty signal"
				if c.AutoApproved {
					reason += " · auto-approved"
				}
				if c.RiskLevel != "" {
					reason += " · risk: " + c.RiskLevel
				}
			}
			t.Steps = append(t.Steps, Step{
				Seq: seq, Kind: StepScreen, Label: "Admission screen", At: c.CreatedAt, Reason: reason,
			})
			seq++
		}
	}
	// rag retrieval context (migration 0010 skill_loads) — the composed-seed provenance the proposal was built
	// from: the skill/precedent artifacts retrieved and composed into the agent's seed. DATA-only projection of
	// the committed provenance strings (non-secret, INV-13); emitted only when the proposal recorded loads
	// (never a fabricated retrieval). Retrieval happens BEFORE the agent loop, so the step is anchored to the
	// classification stamp (matching screen) rather than the later triage-commit stamp — otherwise its
	// timestamp would run backwards relative to its Seq position (it precedes the agent cycles). Falls back to
	// the triage stamp when there is no classification row.
	if loads := rec.Triage.SkillLoads; len(loads) > 0 {
		ragAt := rec.Classification.CreatedAt
		if ragAt.IsZero() {
			ragAt = rec.Triage.CreatedAt
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepRag, Label: "Retrieval context", At: ragAt,
			Reason: fmt.Sprintf("composed %d artifact(s) into the seed", len(loads)), Skills: loads,
		})
		seq++
	}
	// agent ReAct cycles (T-020-8) — the investigation the agent ran BEFORE it proposed, cycle-ordered. Each
	// carries the scrubbed thought → observation as the reason, the tool invoked, and the per-cycle outcome as
	// its verdict. Emitted only for cycles the durable agent_step rows hold (never a fabricated cycle).
	for _, cy := range rec.AgentCycles {
		label := fmt.Sprintf("ReAct cycle %d", cy.Cycle)
		if cy.Tool != "" {
			label = fmt.Sprintf("ReAct cycle %d — %s", cy.Cycle, cy.Tool)
		}
		reason := cy.Thought
		if cy.Observation != "" {
			if reason != "" {
				reason += " → "
			}
			reason += cy.Observation
		}
		st := Step{Seq: seq, Kind: StepAgentCycle, Label: label, At: cy.CreatedAt, Reason: reason, Verdict: cy.Outcome}
		if cy.Tool != "" {
			st.Tools = []string{cy.Tool}
		}
		t.Steps = append(t.Steps, st)
		seq++
	}
	// credential resolutions (REQ-2015) — the non-secret machine identities the investigation resolved for its
	// targets, in read order. The reason names the resolved user + connection scheme + winning source; the
	// credential fields carry the connection scheme and the SCHEME of the key reference only, NEVER a value (INV-13).
	for _, cr := range rec.Credentials {
		reason := cr.Outcome
		if cr.User != "" {
			reason = "resolved " + cr.User + " via " + cr.Scheme
			if cr.Source != "" {
				reason += " (source: " + cr.Source + ")"
			}
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepCredential, Label: "Credential resolve: " + cr.Target, At: cr.CreatedAt,
			Reason: reason, Verdict: cr.Outcome, CredentialScheme: cr.Scheme, CredentialRef: cr.KeyRefScheme,
		})
		seq++
	}
	if rec.Triage.Present {
		tr := rec.Triage
		label := "Proposal"
		if !tr.Proposed {
			label = "Stop (no action proposed)"
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepPropose, Label: label, At: tr.CreatedAt,
			Band: tr.Band, Rule: tr.Op, Reason: tr.Conclusion,
			Confidence: tr.Confidence, // skill-load provenance is projected on the rag (retrieval) boundary, not here
			// prompts provenance (REQ-2009/REQ-2000): the prompt/seed/model identity that composed this proposal —
			// non-secret identifiers (version, seed HASH, tier). Only the fields the session recorded.
			Prompts: proposeProvenance(tr.PromptVersion, tr.SeedHash, tr.ModelTier),
		})
		seq++
	}
	// commit boundary — the sealed end-state + (when present) the out-of-LLM consequence prediction and its
	// verified falsify score. Emitted whenever EITHER durable artifact exists: a sealed manifest
	// (action_manifest → rec.Commit.PlanOps, INV-07) OR a committed prediction. Decoupling from prediction
	// presence keeps the sealed plan-ops from being dropped when a session sealed an action but committed no
	// consequence prediction — and lets the console render the commit boundary from real durable data rather
	// than fabricating one from the classification's action_id (INV-15).
	if rec.Prediction.Present || rec.Commit.Present {
		p := rec.Prediction
		label := "Consequence prediction"
		reason := "committed, awaiting verification window"
		verdict := ""
		at := p.CommittedAt
		if rec.Prediction.Present {
			if p.Scored {
				reason = fmt.Sprintf("scored tp=%d fp=%d fn=%d", p.TP, p.FP, p.FN)
				if p.FP == 0 && p.FN == 0 && p.TP > 0 {
					verdict = "clean"
				} else {
					verdict = "deviation"
				}
			}
		} else {
			// a manifest was sealed but no consequence prediction was committed — show the sealed end-state
			// honestly, never a fabricated prediction (INV-15). Anchor to the classification stamp (best available).
			label = "Sealed action"
			reason = "end-state committed (no consequence prediction)"
			at = rec.Classification.CreatedAt
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepPredict, Label: label, At: at,
			Reason: reason, Verdict: verdict,
			// the sealed action's structured end-state plan ops (action_manifest.action, INV-07). Present whenever
			// a manifest was sealed — the console renders these on the commit boundary.
			PlanOps: rec.Commit.PlanOps,
		})
		seq++
	}
	// policy authorization (T-020-3) — the WHY of the decision: composed verdict, in-force bundle version, the
	// FULL matched-rule list, and the packet-tracer reason. Sits at execute time, after predict.
	if rec.Policy.Present {
		pol := rec.Policy
		label := "Policy decision"
		if pol.Verdict != "" {
			label = "Policy decision: " + pol.Verdict
		}
		// Surface the in-force autonomy mode alongside the packet-tracer reason (the mode is the governance
		// context the decision composed under — non-secret; the Step has no dedicated mode field).
		reason := pol.Reason
		if pol.Mode != "" {
			if reason != "" {
				reason += " "
			}
			reason += "[mode: " + pol.Mode + "]"
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepPolicy, Label: label, At: pol.CreatedAt,
			Verdict: pol.Verdict, BundleVersion: pol.BundleVersion, MatchedRules: pol.MatchedRules,
			Reason: reason, MinConfidence: pol.MinConfidence, // the confidence threshold in force (REQ-2000)
		})
		seq++
	}
	// interceptor gate chain (T-020-7) — one step per ordered gate a governed actuation traversed (the reader
	// returns them ordinal-ascending). Each carries the gate name + its pass/refuse/mechanical verdict.
	for _, g := range rec.GateVerdicts {
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepGate, Label: "Gate: " + g.Gate, At: g.CreatedAt,
			Gate: g.Gate, Verdict: g.Verdict, Reason: g.Reason,
		})
		seq++
	}
	if rec.Verdict.Present {
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepVerify, Label: "Mechanical verdict", At: rec.Verdict.CreatedAt,
			Verdict: rec.Verdict.Verdict, Reason: "deterministic verifier",
		})
		seq++
	}
	return t
}

// deriveStatus reads the session's lifecycle state off the durable rows alone (never a model self-assertion).
func deriveStatus(rec SpineRecords) Status {
	switch {
	case rec.Verdict.Present:
		return StatusExecuted
	case rec.Triage.Present && !rec.Triage.Proposed:
		return StatusStopped
	case rec.Triage.Present:
		return StatusProposed
	case rec.Classification.Present:
		return StatusClassified
	default:
		// ingested but not yet classified (accepted at the front door; the async Runner has not written the
		// classification row yet, or the alert was screened out) — never reported as "classified" (INV-15).
		return StatusReceived
	}
}
