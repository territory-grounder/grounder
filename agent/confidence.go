// Package agent is Territory Grounder's native Go ReAct / tool-calling loop. It calls LLM APIs ONLY
// through the bundled LiteLLM gateway — there is no `claude -p` / `claude -r` subprocess — and NO
// model-produced token ever becomes control flow, a command string, or a query fragment: model output
// enters only as typed, validated, delimited data. In Phase 0/1 the agent may call only read-only
// tools; it investigates and proposes, it never executes.
//
// Provenance: [O] INV-08 (no model token becomes control flow), spec/011 · [F] confidence-first
// reasoning discipline; single-loop least-autonomous topology (sub-agent delegation is DEFERRED under an
// evidence-gated HOLD, not shipped — CONSTITUTION §4.14, docs/EXTERNAL-AUDIT-LESSONS.md lesson 9) · [R]
// paradigm-rule 8.
package agent

import (
	"regexp"
	"strconv"
)

// STOP thresholds for the parseable CONFIDENCE scalar. Below StopThreshold the agent halts and
// escalates to a poll; at or below EscalateThreshold (or on a critical incident) it escalates rather
// than proceeding autonomously. [F] "confidence first-class".
const (
	StopThreshold     = 0.5
	EscalateThreshold = 0.7
)

var confidenceRe = regexp.MustCompile(`(?i)confidence[":\s=]+([01](?:\.\d+)?)`)

// ParseConfidence extracts the CONFIDENCE scalar from a model response as DATA. A missing or
// unparseable confidence returns (0, false) — treated as 0, which is below StopThreshold, so the
// absence of a confidence signal fails closed (the agent stops rather than proceeds).
func ParseConfidence(resp string) (float64, bool) {
	m := confidenceRe.FindStringSubmatch(resp)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v < 0 || v > 1 {
		return 0, false
	}
	return v, true
}
