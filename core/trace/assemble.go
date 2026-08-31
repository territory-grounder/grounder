package trace

import (
	"context"
	"fmt"
	"strconv"
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
	Regime         RegimeRecord         // regime_audit (TG-412) — the actuation-regime-select boundary (lane + op-class)
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

// RegimeRecord is the actuation-REGIME-SELECT boundary (regime_audit, action_id-keyed; TG-412/REQ-2001): the
// effect LANE the actuation regime engine resolved for a sealed action (native-ssh / gitops-mr / awx-job / k8s)
// and its op-class. NON-SECRET — a lane name + op-class, never a job token, argv, or host secret (INV-13).
// Present is false when no regime row exists for the action (the session stopped before actuation, or predates
// the regime engine). It sits after policy Decide + credential Resolve and before verify.
type RegimeRecord struct {
	Present   bool
	Lane      string
	OpClass   string
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
	// DecisionTier is the tier that produced the TERMINAL proposal/stop (session_triage.decision_model_tier,
	// migration 0057, TG-198). ModelTier above is the INVESTIGATION tier: on a session the TG-60 nudge pushed
	// to the reasoner they differ, and the propose step used to name only the first — telling the operator
	// the cheap model authored a decision it did not make. Empty for a pre-0057 session (unattributable).
	DecisionTier string
	// Attribution is the actor-attribution taxonomy this session resolved (session_triage.actor_attribution,
	// spec/023) — attributed-self | attributed-authorized | authorized-test | attributed-suspicious |
	// unattributable. EMPTY on a session recorded before attribution existed, and an empty value emits NO
	// attribute step rather than a fabricated "unattributable": "nobody asked" and "asked and could not
	// tell" are different facts, and only one of them is about the estate (REQ-2311).
	Attribution string
	// AttributionEvidence are the DOMAIN-NATIVE references the attribution rested on ("pve:UPID:...",
	// "journal:<cursor>") — never the actor identity, never the raw record. The taxonomy is the finding;
	// these are the pointers an operator follows to check it (INV-13: non-secret identifiers only).
	AttributionEvidence []string
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
	// EvidenceID names the stored ground truth for this cycle (agent_step_evidence, TG-272), or "" when none
	// was recorded — which is the honest answer for every session predating migration 0053. It is read from
	// the evidence table by join, NOT parsed out of Observation: Observation happens to render as
	// "observed <id>" today, and a console citation that only works while that sentence keeps its shape is a
	// link waiting to break silently.
	EvidenceID string
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
	// Margin is the signed value−threshold this gate decided by (TG-178, migration 0076), nil for a binary
	// gate with no numeric threshold and for every row written before the column existed. Surfaced on the
	// per-action tracer walk so an operator opening one action's gate chain sees HOW CLOSE each numeric gate
	// was, not only which fired.
	Margin *float64
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
		st.EvidenceID = cy.EvidenceID
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
	// The ATTRIBUTE boundary (spec/023 REQ-2311), emitted BEFORE propose because attribution is an INPUT to
	// the decision rather than a report on it: a carve-out heals a manufactured pool fault, and a suspicious
	// actor forces POLL_PAUSE with the security escalation. Omitted entirely when the session recorded no
	// taxonomy — an absent determination must not render as "unattributable", which is itself a verdict.
	if rec.Triage.Present && strings.TrimSpace(rec.Triage.Attribution) != "" {
		tr := rec.Triage
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepAttribute, Label: "Actor attribution: " + tr.Attribution, At: tr.CreatedAt,
			Verdict: tr.Attribution,
			Reason:  attributionReason(tr.Attribution),
			Tools:   append([]string(nil), tr.AttributionEvidence...),
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
			// TG-198: the DECISION tier rides here too — this step is the decision, and naming only the
			// investigation tier credited the fast model for proposals the reasoner authored.
			Prompts: proposeProvenance(tr.PromptVersion, tr.SeedHash, tr.ModelTier, tr.DecisionTier),
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
			Gate: g.Gate, Verdict: g.Verdict, Reason: g.Reason, GateMargin: g.Margin,
		})
		seq++
	}
	if rec.Regime.Present {
		// The actuation-regime-select boundary (TG-412, REQ-2001): which effect lane the regime engine resolved
		// for the sealed action, after policy/credential and before verify. Observe-only projection of the
		// durable regime audit row — a lane name + op-class, never a job token (INV-13).
		label := "Regime select"
		if rec.Regime.Lane != "" {
			label = "Regime select: " + rec.Regime.Lane
		}
		reason := "actuation regime engine resolved the effect lane"
		if rec.Regime.OpClass != "" {
			reason += " for op-class " + rec.Regime.OpClass
		}
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepRegime, Label: label, At: rec.Regime.DecidedAt, Reason: reason,
		})
		seq++
	}
	if rec.Verdict.Present {
		t.Steps = append(t.Steps, Step{
			Seq: seq, Kind: StepVerify, Label: "Mechanical verdict", At: rec.Verdict.CreatedAt,
			Verdict: rec.Verdict.Verdict,
			// action_verdict is keyed by the content-hashed action_id alone and is append-only first-wins, so ONE
			// row serves every incident that ever proposed the same action shape (the same collapse deriveStatus
			// already guards for the executed-lifecycle, measured live 22/30 on 2026-07-29). The lifecycle no
			// longer inherits from a stranger, but this display step still shows that shared row's verdict + `At`,
			// so it is labelled honestly: an auditor must not read the timestamp as this session's own execution
			// when an earlier session sealed the identical action first (TG-142).
			Reason: "deterministic verifier (verdict is content-addressed by action_id — shared across any session " +
				"that proposed this identical action; the timestamp is its first recording, not necessarily this session's)",
		})
		seq++
	}
	return t
}

// deriveStatus reads the session's lifecycle state off the durable rows alone (never a model self-assertion).
//
// "Executed" is answered by THIS incident's execute gate, not by the presence of a mechanical verdict.
// action_verdict is keyed by the content-hashed action_id alone and is append-only first-wins, so one row
// serves every incident that ever proposed the same action shape; keying the lifecycle off it meant a session
// inherited "executed" from a stranger. Measured live 2026-07-29: 22 of 30 consecutive sessions read
// "executed", and all 22 were reading a verdict written before their own proposal.
//
// The ref-scoped gate chain is the honest source — interceptor_gate_verdict carries external_ref, so an
// `execute` gate that passed belongs to THIS incident and to no other. The verdict arm stays as a fallback
// for sessions predating the gate ledger (rows exist from 2026-07-21), and the reader now admits a verdict
// only when it can prove the row is this incident's, so that arm can no longer import a foreign one.
func deriveStatus(rec SpineRecords) Status {
	// gate='execute' AND verdict='pass' is the established executed-for-real predicate, not a new one: the axis
	// reader at core/db/axis_read.go:495 already uses it as its anti-join and records that it was verified live
	// against the estate ("zero executed action_ids lack one").
	for _, g := range rec.GateVerdicts {
		if g.Gate == "execute" && g.Verdict == "pass" {
			return StatusExecuted
		}
	}
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

// attributionReason renders the OPERATIONAL consequence of a taxonomy, not a restatement of it. The
// taxonomy value already rides on Verdict; a reason that only spelled it out again would add a line and
// no information. What an operator needs at the boundary is what it MEANT for the decision.
//
// An unrecognised value is named rather than defaulted: a taxonomy this function has not been taught is a
// vocabulary drift, and rendering it as one of the known meanings would be a confident mistranslation.
func attributionReason(taxonomy string) string {
	switch strings.ToLower(strings.TrimSpace(taxonomy)) {
	case "attributed-self":
		return "TG's own actuation identity caused this change — the fault is already remediated by us"
	case "attributed-authorized":
		return "a sanctioned non-TG principal caused this change — stand down rather than heal over an operator"
	case "authorized-test":
		return "a temporally-bounded carve-out matched — a manufactured fault the learning regime may heal"
	case "attributed-suspicious":
		return "an unsanctioned actor caused this change — POLL_PAUSE with security escalation, never auto-heal"
	case "unattributable":
		return "no admissible evidence named an actor — absence of evidence, not evidence of absence"
	default:
		return "unrecognised attribution taxonomy " + strconv.Quote(taxonomy) + " — the vocabulary has drifted"
	}
}
