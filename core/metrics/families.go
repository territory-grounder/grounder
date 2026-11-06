package metrics

// families.go adds two metric families to the deterministic exposition: the agent-loop FIVE-METRIC family
// (OpenAI/Semantic-Kernel "observable by default" — runtime, tool-call count, tool errors, token
// consumption, and the terminal outcome that stands in for accuracy) and the ledger-derived
// governance-decision family (counts by autonomy band and withheld flag, mirroring the classify:<band>
// rows appended to the tamper-evident ledger). core/observe holds the LIVE counters and renders them
// through these constructors; this file owns the NAMES, HELP text, Kind, and — critically — the bounded
// label enums.
//
// Two properties are enforced HERE, by construction, so no caller can violate them:
//   - Bounded cardinality: every label value passes through a Clamp* that folds anything outside a CLOSED
//     enum to "other". A hostname, ref, op string, or any unbounded value can never become a label.
//   - Secret-free: the only things emitted are counts/seconds and clamped enum labels — never a token,
//     credential, arg, host, or free-text span. There is no code path that puts caller free text on the wire.
//
// The metrics are OBSERVE-ONLY: they count what happened; they never gate, never feed a decision, and never
// touch the actuation/breaker/mode chokepoints. Adding them changes no control flow.

// Metric names. The five-metric agent family is tg_agent_* (four counters + the by-outcome runs counter);
// the ledger-derived family is tg_governance_decisions_total.
const (
	// MetricAgentRunSeconds is the cumulative wall-clock time spent in the agent ReAct loop (counter).
	MetricAgentRunSeconds = "tg_agent_run_seconds_total"
	// MetricAgentRuns counts completed agent loops by terminal outcome — the "accuracy"/verdict dimension.
	MetricAgentRuns = "tg_agent_runs_total"
	// MetricAgentToolCalls counts read-only tool calls the loop dispatched.
	MetricAgentToolCalls = "tg_agent_tool_calls_total"
	// MetricAgentToolErrors counts tool calls that returned a non-success result.
	MetricAgentToolErrors = "tg_agent_tool_errors_total"
	// MetricAgentTokensApprox counts APPROXIMATE tokens the orchestrator can see (seed + tool observations,
	// at chars/4). It is an ESTIMATE and it is measurably wrong — read MetricModelTokens instead for spend.
	MetricAgentTokensApprox = "tg_agent_tokens_approx_total"
	// MetricModelTokens counts the tokens the PROVIDER REPORTED, by tier and prompt/completion (TG-44). This
	// is the measured series: it is fed only from a real `usage` block and never from an estimate.
	MetricModelTokens = "tg_model_tokens_total"
	// MetricModelUsageMissing counts billable completions that came back with NO usage block, by tier —
	// the denominator that says how much of the token accounting is a guess. Without it, a fallback to the
	// estimate is indistinguishable from a measurement, which is the exact condition TG-44 was filed for.
	MetricModelUsageMissing = "tg_model_usage_missing_total"
	// MetricVerdicts counts mechanical post-execution verify verdicts by outcome.
	MetricVerdicts = "tg_agent_verdicts_total"
	// MetricDecisions counts governance classification decisions by autonomy band + withheld flag.
	MetricDecisions = "tg_governance_decisions_total"
	// MetricModelCalls counts model-gateway completions by tier + classified outcome (ok/empty/rate_limit/…).
	MetricModelCalls = "tg_model_calls_total"
	// MetricModelCallSeconds is the cumulative wall-clock seconds spent in model-gateway calls, by tier —
	// the signal that makes a slow reasoning model (e.g. a ~50s judge-sized kimi call) visible.
	MetricModelCallSeconds = "tg_model_call_seconds_total"
	// The decision-stage triple (TG-380): per governance decision stage, offered/eligible/acted so a zero is
	// INTERPRETABLE — a stage that only counted its acted events could not distinguish "nothing to act on"
	// from "dead stage" (the TG-377/TG-365 shape). offered ≥ eligible ≥ acted by construction.
	MetricStageOffered  = "tg_stage_offered_total"
	MetricStageEligible = "tg_stage_eligible_total"
	MetricStageActed    = "tg_stage_acted_total"
	// MetricConfidenceSamples is how many paired confidence x verified-outcome samples the calibrator scored.
	// Published because every calibration score below is meaningless without its N — at N=0 the gauges are a
	// flat zero and a reader must be able to tell "perfectly calibrated" from "no evidence yet".
	MetricConfidenceSamples = "tg_confidence_samples"
	// MetricConfidenceBrier is the Brier score of the agent's stated confidence against verified outcomes.
	// LOWER IS BETTER, and 0.25 is the score of always guessing 0.5 — so a value ABOVE 0.25 means the stated
	// confidence is worse than a coin.
	MetricConfidenceBrier = "tg_confidence_brier"
	// MetricConfidenceBaseRate is the observed clean rate — the outcome's climatology. It is the reference a
	// Brier score must be judged against; 0.25 is the COIN reference, not the no-skill one.
	MetricConfidenceBaseRate = "tg_confidence_base_rate"
	// MetricConfidenceSkill is the Brier Skill Score against always stating the base rate. ABOVE 0 means the
	// stated confidence carries information a constant does not; 0 means none; BELOW 0 means it carries LESS
	// than a constant that looks at nothing. WITHHELD when the base rate is degenerate (undefined).
	MetricConfidenceSkill = "tg_confidence_skill"
	// MetricConfidenceECE is Expected Calibration Error: the average gap between stated confidence and
	// observed accuracy. 0.51 means the number is off by ~51 percentage points on average.
	MetricConfidenceECE = "tg_confidence_ece"
	// MetricConfidenceMCE is Maximum Calibration Error — the WORST bin's gap. It is published beside ECE
	// because an average hides the bucket where the agent was most confident and most wrong.
	MetricConfidenceMCE = "tg_confidence_mce"
)

const (
	helpAgentRunSeconds   = "cumulative wall-clock seconds spent in the agent ReAct loop (observe-only)"
	helpAgentRuns         = "agent ReAct loops completed, by terminal outcome (observe-only)"
	helpAgentToolCalls    = "read-only tool calls the agent loop dispatched (observe-only)"
	helpAgentToolErrors   = "agent tool calls that returned a non-success result (observe-only)"
	helpAgentTokensApprox = "ESTIMATED tokens the orchestrator can see — the seed prompt plus captured tool observations at chars/4. It is NOT the billed figure and it reads LOW: measured against the live gateway on 2026-08-04 the same exchanges reported 1.9x (3.4k-char prompt) to 13.8x (47-char prompt) more tokens than this estimate, because per-message and system-prompt overhead is invisible to a character count. Use tg_model_tokens_total for spend (observe-only)"
	helpModelTokens       = "tokens the provider REPORTED for gateway completions, by model tier and prompt/completion (from the response usage block; an absent block is counted in tg_model_usage_missing_total, never estimated into this series) (observe-only)"
	helpModelUsageMissing = "billable gateway completions whose response carried NO usage block, by model tier — how much of the token/cost accounting is falling back to an estimate (observe-only)"
	helpVerdicts          = "mechanical post-execution verify verdicts, by outcome (observe-only)"
	helpDecisions         = "governance classification decisions, by autonomy band and withheld flag — mirrors the classify:<band> ledger rows (observe-only)"
	helpModelCalls        = "model-gateway completions, by model tier and classified outcome (observe-only)"
	helpModelCallSeconds  = "cumulative wall-clock seconds spent in model-gateway calls, by model tier (observe-only)"
	helpStageOffered      = "decisions OFFERED to a governance stage (the denominator), by stage — every alert/action the stage saw (TG-380, observe-only)"
	helpStageEligible     = "decisions ELIGIBLE for the stage to act on, by stage — offered minus those short-circuited before the stage's real logic; a zero here means 'nothing to act on', distinct from a dead stage (TG-380)"
	helpStageActed        = "decisions the stage ACTED on, by stage — the numerator; read beside offered/eligible so a zero is interpretable, never mistaken for a healthy idle stage (TG-380)"
	helpConfidenceSamples = "paired confidence x verified-outcome samples the calibrator scored — every score below is meaningless without this N (observe-only)"
	helpConfidenceBrier   = "Brier score of stated confidence vs the outcome named by the `outcome` label; LOWER is better. Read tg_confidence_skill and tg_confidence_base_rate beside it: 0.25 is the always-guess-0.5 COIN reference, NOT the no-skill reference (observe-only, gates nothing)"
	helpConfidenceECE     = "expected calibration error: mean gap between stated confidence and observed accuracy (observe-only, gates nothing)"
	helpConfidenceMCE     = "maximum calibration error: the WORST bin's gap — an average hides the bucket where the agent was most confident and most wrong (observe-only, gates nothing)"
	helpConfidenceBase    = "observed clean rate over the scored samples — the outcome's base rate, and the reference the Brier score must be judged against (observe-only)"
	helpConfidenceSkill   = "Brier Skill Score vs always stating the base rate: >0 the stated confidence beats a constant, 0 ties it, <0 it carries LESS information than looking at nothing (observe-only, gates nothing)"
)

// The CLOSED label enums. Any value outside a set is clamped to "other" (or "unset" for an absent verdict),
// so an unexpected input can never introduce an unbounded label. These mirror agent.Outcome.String(),
// safety.Verdict, and safety.Band.String() — kept as plain literals here so core/metrics stays free of a
// dependency on the safety/agent packages (it is the leaf exposition layer).
var (
	agentOutcomeSet = map[string]bool{"stop": true, "escalate": true, "proposed": true, "hard-halt": true}
	verdictSet      = map[string]bool{"match": true, "partial": true, "deviation": true, "unset": true}
	bandSet         = map[string]bool{"AUTO": true, "AUTO_NOTICE": true, "POLL_PAUSE": true}
	// The model tier + call-outcome enums. Tiers are the model NAMES the Go side selects (adapters/model);
	// outcomes are the ModelError classes plus ok/empty. Anything else folds to "other".
	// modelTierSet is the CLOSED set of model-tier labels (TG-303). It listed only {primary, fast, embed},
	// while the Go side selects several tiers that were not in it — arm-haiku, arm-opus, opus-cc, judge —
	// so every one of them collapsed into "other". The TG-204 experiment arms are among them, which means
	// the A/B could not tell its own arms apart in the exposition layer: two arms, one label.
	//
	// The set stays CLOSED on purpose. Its job is to stop an unbounded label (a model name straight from a
	// gateway response would be attacker- or config-influenced cardinality), so the fix is to enumerate the
	// aliases TG actually selects, not to pass the input through. These mirror the model_name aliases in
	// deploy/litellm-config.yaml; a NEW alias must be added here as well, and until it is, its metrics
	// honestly read "other" rather than silently inventing a series.
	modelTierSet = map[string]bool{
		"primary": true, "fast": true, "embed": true,
		"opus-cc": true, "arm-haiku": true, "arm-opus": true, "judge": true,
		"fallback-deepseek": true, "fallback-mistral": true, "fallback-zai": true,
		"embed-nomic": true, "embed-mistral": true,
	}
	// KEEP IN SYNC WITH THE PRODUCER. Every outcome class adapters/model can emit must appear here or it
	// folds to "other" — the bucket that means "we do not know". TestEveryModelOutcomeClassIsInTheEnum
	// scans the adapter's own source and fails on a class this set is missing, because the last time this
	// drifted it did so silently: `breaker_open` was defined with a comment saying it exists precisely so an
	// operator "sees 'TG refused to call' rather than 'the provider failed'", and this set discarded it.
	// Measured on dc1tg01 2026-08-06: 216 of 636 primary calls and 234 embed calls were sitting in
	// `other` — a third of the model plane's traffic, unexplained on the dashboard and fully explained in
	// the log the dashboard does not read.
	// rate_limit_retry is the interim outcome for a 429 that CompleteWithUsage backed off and retried (TG-534),
	// distinct from a surfaced rate_limit (retries exhausted): it lets tg_model_calls_total{outcome=
	// "rate_limit_retry"} show how much throttle the gateway is silently absorbing, which is otherwise only in
	// the log the dashboard does not read. (Unlike the Class:/return outcomes, this one is emitted as an
	// observe() argument, a shape the producer scan does not match — so it is pinned by name below instead.)
	modelOutcomeSet = map[string]bool{"ok": true, "empty": true, "rate_limit": true, "rate_limit_retry": true,
		"timeout": true, "bad_request": true, "auth": true, "provider_error": true, "transport": true,
		"breaker_open": true, "session_budget": true, "concurrency_wait_timeout": true}
)

// ClampAgentOutcome bounds the agent-loop outcome label to the closed agent-outcome enum.
func ClampAgentOutcome(s string) string {
	if agentOutcomeSet[s] {
		return s
	}
	return "other"
}

// ClampVerdict bounds the verify-verdict label; "" (no verdict was written) folds to "unset".
func ClampVerdict(s string) string {
	if s == "" {
		return "unset"
	}
	if verdictSet[s] {
		return s
	}
	return "other"
}

// ClampBand bounds the classification-band label to the closed autonomy-band enum.
func ClampBand(s string) string {
	if bandSet[s] {
		return s
	}
	return "other"
}

// ClampModelTier bounds the model-tier label to the closed set of tiers the Go side selects.
func ClampModelTier(s string) string {
	if modelTierSet[s] {
		return s
	}
	return "other"
}

// ClampModelOutcome bounds the model-call outcome label to the closed outcome enum.
func ClampModelOutcome(s string) string {
	if modelOutcomeSet[s] {
		return s
	}
	return "other"
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// AgentRunSecondsSample is the cumulative agent-loop seconds counter sample.
func AgentRunSecondsSample(v float64) Sample {
	return Sample{Name: MetricAgentRunSeconds, Kind: Counter, Help: helpAgentRunSeconds, Value: v}
}

// AgentRunsSample is the by-outcome agent-loop count sample; the outcome label is clamped to the enum.
func AgentRunsSample(outcome string, v float64) Sample {
	return Sample{Name: MetricAgentRuns, Kind: Counter, Help: helpAgentRuns, Value: v,
		Labels: map[string]string{"outcome": ClampAgentOutcome(outcome)}}
}

// AgentToolCallsSample is the read-only tool-call count sample.
func AgentToolCallsSample(v float64) Sample {
	return Sample{Name: MetricAgentToolCalls, Kind: Counter, Help: helpAgentToolCalls, Value: v}
}

// AgentToolErrorsSample is the tool-error count sample.
func AgentToolErrorsSample(v float64) Sample {
	return Sample{Name: MetricAgentToolErrors, Kind: Counter, Help: helpAgentToolErrors, Value: v}
}

// AgentTokensApproxSample is the ESTIMATED-token count sample (chars/4 over the seed + tool observations).
// Kept beside the measured family rather than deleted: it counts a different thing (what the orchestrator
// itself handled) and dashboards read it — but its HELP now names the measured series and the size of the
// error, so nobody bills it by accident.
func AgentTokensApproxSample(v float64) Sample {
	return Sample{Name: MetricAgentTokensApprox, Kind: Counter, Help: helpAgentTokensApprox, Value: v}
}

// The token-kind label enum: the two halves of a provider usage block. Anything else folds to "other".
var tokenKindSet = map[string]bool{TokenKindPrompt: true, TokenKindCompletion: true}

// The closed token-kind label values.
const (
	TokenKindPrompt     = "prompt"
	TokenKindCompletion = "completion"
)

// ClampTokenKind bounds the token-kind label to the closed prompt/completion enum.
func ClampTokenKind(s string) string {
	if tokenKindSet[s] {
		return s
	}
	return "other"
}

// ModelTokensSample is the MEASURED token counter sample, by tier and prompt/completion. Both labels are
// clamped. Callers must only feed it a provider-reported figure: the whole point of a separate series from
// tg_agent_tokens_approx_total is that one of them can be trusted for spend.
func ModelTokensSample(tier, kind string, v float64) Sample {
	return Sample{Name: MetricModelTokens, Kind: Counter, Help: helpModelTokens, Value: v,
		Labels: map[string]string{"model": ClampModelTier(tier), "kind": ClampTokenKind(kind)}}
}

// ModelUsageMissingSample counts billable completions that reported no usage, by tier; the label is clamped.
func ModelUsageMissingSample(tier string, v float64) Sample {
	return Sample{Name: MetricModelUsageMissing, Kind: Counter, Help: helpModelUsageMissing, Value: v,
		Labels: map[string]string{"model": ClampModelTier(tier)}}
}

// VerdictsSample is the by-outcome verify-verdict count sample; the outcome label is clamped.
func VerdictsSample(verdict string, v float64) Sample {
	return Sample{Name: MetricVerdicts, Kind: Counter, Help: helpVerdicts, Value: v,
		Labels: map[string]string{"outcome": ClampVerdict(verdict)}}
}

// DecisionsSample is the governance-decision count sample by band + withheld; the band label is clamped.
func DecisionsSample(band string, withheld bool, v float64) Sample {
	return Sample{Name: MetricDecisions, Kind: Counter, Help: helpDecisions, Value: v,
		Labels: map[string]string{"band": ClampBand(band), "withheld": boolLabel(withheld)}}
}

// ModelCallsSample is the by-tier, by-outcome model-call count sample; both labels are clamped to enums.
func ModelCallsSample(tier, outcome string, v float64) Sample {
	return Sample{Name: MetricModelCalls, Kind: Counter, Help: helpModelCalls, Value: v,
		Labels: map[string]string{"model": ClampModelTier(tier), "outcome": ClampModelOutcome(outcome)}}
}

// ModelCallSecondsSample is the cumulative model-call seconds sample, by tier; the tier label is clamped.
// CalibrationSamples renders the confidence-calibration family. They are GAUGES, not counters: a reliability
// score is the current state of a curve, not an accumulating total, and rendering a curve as a counter would
// make every dashboard show a meaningless monotonic climb.
//
// N is emitted ALONGSIDE the scores and never folded into them, because at N=0 the three scores are a flat
// zero — indistinguishable, on a graph, from perfect calibration. A reader must be able to tell "no evidence
// yet" from "calibrated", and only the denominator says which.
// CalibrationReading is one confidence-reliability curve as published. It lives here, not in core/observe,
// because core/observe imports this package; observe aliases the type so the Emitter seam stays readable.
type CalibrationReading struct {
	N               int
	Brier, ECE, MCE float64
	BaseRate        float64
	Skill           float64
	SkillDefined    bool
	// Outcome NAMES the variable the confidence was scored against, and is published as a label on every
	// gauge below. It is not decoration. Measured 2026-07-27: the same stated confidences score Brier 0.4633
	// against blast-radius EXACTNESS (fp=0 AND fn=0) and Brier 0.0555 against DIAGNOSIS CORRECTNESS — the
	// first reads as "worse than a coin", the second is good and slightly UNDER-confident. A calibration
	// number without its outcome variable is not interpretable, and the unlabelled version was already
	// misread once, by its author, in the alert he wrote for it. Clamped to a closed enum on render.
	Outcome string
}

// The CLOSED set of outcome variables a calibration curve may be scored against. Anything else clamps to
// "other" rather than being published verbatim: an unrecognised outcome silently redefines what every score
// beside it MEANS, and a label that can take any value is not a reference class.
const (
	OutcomeBlastRadiusExact = "blast_radius_exact"
	OutcomeDiagnosisCorrect = "diagnosis_correct"
	OutcomeOther            = "other"
)

// ClampCalibrationOutcome bounds the outcome label to the closed set above.
func ClampCalibrationOutcome(s string) string {
	switch s {
	case OutcomeBlastRadiusExact, OutcomeDiagnosisCorrect:
		return s
	default:
		return OutcomeOther
	}
}

func CalibrationSamples(c CalibrationReading) []Sample {
	lbl := map[string]string{"outcome": ClampCalibrationOutcome(c.Outcome)}
	out := []Sample{{Name: MetricConfidenceSamples, Kind: Gauge, Help: helpConfidenceSamples, Value: float64(c.N), Labels: lbl}}
	if c.N == 0 {
		// Publish the denominator and NOTHING else. Emitting 0.0 for Brier/ECE/MCE over an empty sample set
		// would render as a flawless calibration on every dashboard that reads them.
		return out
	}
	out = append(out,
		Sample{Name: MetricConfidenceBrier, Kind: Gauge, Help: helpConfidenceBrier, Value: c.Brier, Labels: lbl},
		Sample{Name: MetricConfidenceECE, Kind: Gauge, Help: helpConfidenceECE, Value: c.ECE, Labels: lbl},
		Sample{Name: MetricConfidenceMCE, Kind: Gauge, Help: helpConfidenceMCE, Value: c.MCE, Labels: lbl},
		Sample{Name: MetricConfidenceBaseRate, Kind: Gauge, Help: helpConfidenceBase, Value: c.BaseRate, Labels: lbl},
	)
	// The skill score is WITHHELD when undefined, for the same reason the scores are withheld at N=0: a
	// degenerate base rate makes the ratio meaningless, and publishing 0 there would read as "no skill"
	// when the truth is "unmeasurable". Absence is the honest rendering; the base rate above says why.
	if c.SkillDefined {
		out = append(out, Sample{Name: MetricConfidenceSkill, Kind: Gauge, Help: helpConfidenceSkill, Value: c.Skill, Labels: lbl})
	}
	return out
}

func ModelCallSecondsSample(tier string, v float64) Sample {
	return Sample{Name: MetricModelCallSeconds, Kind: Counter, Help: helpModelCallSeconds, Value: v,
		Labels: map[string]string{"model": ClampModelTier(tier)}}
}

// StageOfferedSample / StageEligibleSample / StageActedSample render the TG-380 decision-stage triple for
// ONE stage. The stage label is clamped to the closed DecisionStages set — an unknown stage folds to
// "other" rather than minting an unbounded series. All three are emitted TOGETHER for every stage that has
// seen any traffic (the denominator discipline: a bare acted counter cannot say whether a zero means idle
// or dead).
func StageOfferedSample(stage string, v float64) Sample {
	return Sample{Name: MetricStageOffered, Kind: Counter, Help: helpStageOffered, Value: v,
		Labels: map[string]string{"stage": ClampStage(stage)}}
}

func StageEligibleSample(stage string, v float64) Sample {
	return Sample{Name: MetricStageEligible, Kind: Counter, Help: helpStageEligible, Value: v,
		Labels: map[string]string{"stage": ClampStage(stage)}}
}

func StageActedSample(stage string, v float64) Sample {
	return Sample{Name: MetricStageActed, Kind: Counter, Help: helpStageActed, Value: v,
		Labels: map[string]string{"stage": ClampStage(stage)}}
}

// DecisionStages is the CLOSED set of governance decision stages TG-380 instruments. A stage added to the
// pipeline must join this set AND wire its triple — the producer-scan guard (core/observe) exercises each
// member's real decision and fails if the tally never moved. Stages are added per slice; "suppress" is
// slice 1, "correlate" slice 3, "predict" slice 4, "gate" slice 5. The rest are the coverage frontier so the
// set is the single source of truth.
var DecisionStages = []string{"suppress", "correlate", "predict", "gate"}

// PendingDecisionStages names the stages TG-380 will instrument in later slices — the honest frontier, so a
// reader (and the guard) can tell "not yet wired" from "silently missing". Moving a stage from here to
// DecisionStages requires wiring its triple + an exercise in the guard.
//
// SLICE-2 NOTES (from the slice-1 review): (a) the producer-scan guard proves a wired stage increments at
// BUILD time, but a stage that stops being called in PRODUCTION (an upstream routing bug that never
// constructs the gate) emits no series and reads identically to "idle" — if runtime-silence detection is
// wanted, add a periodic zero-heartbeat per wired stage rather than the current emit-only-on-traffic. (b) a
// finer eligible breakdown could split "no stage matched" from "learned-reversal escalate" (see suppress.go).
//
// SLICE-3 NOTE (why "classify" stays here): the exec-class classifier (core/execclass) routes on Novel,
// Ambiguous, criticality, KnownProcedure/Pattern, Reversible — but in the LIVE pipeline execclass.Classify
// is only ever called with `Correlated` wired (temporal/runner/correlate.go:91,152), so its verdict is a
// pure function of the correlate stage's result (DeepInvestigation iff Correlated, else StandardAgent). A
// classify triple would therefore move IDENTICALLY to the correlate triple — a second series that looks
// independent but is not, the exact declared-but-dead-adjacent shape this instrument exists to avoid.
// Promote it only once the classifier's other inputs are actually wired (a separate gap: the classifier is
// richer than its call site). (Slice 5 wired "gate"; "breaker" is a gauge, not a triple — see the slice-5 note.)
//
// SLICE-4 NOTE (predict, now wired at the RUNNER boundary): the predict triple is booked in GateActivity
// (temporal/runner/activities.go), NOT in core/predict — the gate itself is a protected path + lockstep-bound
// to spec/002, so the observe-only Record lives at its unprotected call site. eligible = the state precondition
// could be ESTABLISHED (the gate did not refuse for an unestablished/violated precondition,
// ErrPreconditionUnestablished/Violated); a downstream Commit failure still counts as eligible-but-not-acted.
// acted = a prediction was committed (Commit returned a gated proposal). Unlike classify this is a GENUINELY
// independent series: the precondition + commit outcome is not a pure function of any earlier stage's verdict.
//
// SLICE-5 NOTE (gate wired; breaker reclassified): "gate" is booked from the UNPROTECTED caller
// (temporal/runner/activities.go ExecuteActivity, after Interceptor.Do/LaneEffect.Apply) — the interceptor gate
// chain is protected + lockstep (spec/013), so the observe-only triple is recorded where its Outcome lands, not
// inside the chain. offered = the chain produced a verdict; eligible = it was not short-circuited by the
// actuation-frequency governor (out.RateLimited — the one non-per-action refusal the coarse Outcome
// distinguishes; a finer per-gate split would consume the gate-verdict sink, a later slice); acted =
// out.Executed. In Shadow (the production default) the mode chokepoint refuses every mutation, so acted is ~0 —
// the interpretable offered>0/acted=0 case, not a dead stage.
//
// "breaker" is NOT here: a circuit breaker is a 3-STATE GAUGE (open/closed/half-open, circuit_breaker_state),
// not a monotonic offered/eligible/acted triple, so it cannot use this rail or the producer-scan exerciser. Its
// tg_breaker_state{name} gauge-name migration is tracked separately (TG-452). "classify" stays pending (above).
var PendingDecisionStages = []string{"classify"}

var stageSet = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range DecisionStages {
		m[s] = true
	}
	return m
}()

// ClampStage bounds the stage label to the closed DecisionStages set; an unknown stage folds to "other".
func ClampStage(s string) string {
	if stageSet[s] {
		return s
	}
	return "other"
}
