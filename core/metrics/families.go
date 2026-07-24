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
	// MetricAgentTokensApprox counts APPROXIMATE tokens processed (chars/4; the gateway returns no usage).
	MetricAgentTokensApprox = "tg_agent_tokens_approx_total"
	// MetricVerdicts counts mechanical post-execution verify verdicts by outcome.
	MetricVerdicts = "tg_agent_verdicts_total"
	// MetricDecisions counts governance classification decisions by autonomy band + withheld flag.
	MetricDecisions = "tg_governance_decisions_total"
)

const (
	helpAgentRunSeconds   = "cumulative wall-clock seconds spent in the agent ReAct loop (observe-only)"
	helpAgentRuns         = "agent ReAct loops completed, by terminal outcome (observe-only)"
	helpAgentToolCalls    = "read-only tool calls the agent loop dispatched (observe-only)"
	helpAgentToolErrors   = "agent tool calls that returned a non-success result (observe-only)"
	helpAgentTokensApprox = "approximate tokens processed by the agent loop (chars/4; the gateway returns no usage) (observe-only)"
	helpVerdicts          = "mechanical post-execution verify verdicts, by outcome (observe-only)"
	helpDecisions         = "governance classification decisions, by autonomy band and withheld flag — mirrors the classify:<band> ledger rows (observe-only)"
)

// The CLOSED label enums. Any value outside a set is clamped to "other" (or "unset" for an absent verdict),
// so an unexpected input can never introduce an unbounded label. These mirror agent.Outcome.String(),
// safety.Verdict, and safety.Band.String() — kept as plain literals here so core/metrics stays free of a
// dependency on the safety/agent packages (it is the leaf exposition layer).
var (
	agentOutcomeSet = map[string]bool{"stop": true, "escalate": true, "proposed": true, "hard-halt": true}
	verdictSet      = map[string]bool{"match": true, "partial": true, "deviation": true, "unset": true}
	bandSet         = map[string]bool{"AUTO": true, "AUTO_NOTICE": true, "POLL_PAUSE": true}
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

// AgentTokensApproxSample is the approximate-token count sample (chars/4; no real usage is available).
func AgentTokensApproxSample(v float64) Sample {
	return Sample{Name: MetricAgentTokensApprox, Kind: Counter, Help: helpAgentTokensApprox, Value: v}
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
