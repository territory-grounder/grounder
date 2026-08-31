// Package sessionspan turns a COMPLETED investigation into the ordered, bounded, secret-free spans that
// are shipped to the trace store, and tallies the per-session token usage those spans report.
//
// WHY IT EXISTS (TG-44). modules/observability/openobserve has had an ExportSpans method, and tracing
// default-ON, since spec/008 — and no composition root ever called it. Its own descriptor says so in
// prose: "Span export exists in the module but no worker path calls it today, so no traces ship." So
// INV-14 ("the session trajectory is reconstructable") was satisfied by a method, not by a trace: TG could
// tell you a session happened but not, from outside TG, what it did or what it cost. This package is the
// missing middle — it takes what the investigate activity ALREADY holds (the cycle-aligned transcript from
// spec/020, the terminal decision tier from TG-198, the wall-clock the activity already measured) and
// renders it into the string spans the exporter contract takes.
//
// THREE RULES GOVERN WHAT MAY GO ON THE WIRE, and they are the same rules core/metrics enforces for labels,
// for the same reason — an exporter ships to a third-party store TG does not control:
//
//  1. BOUNDED. Every field is a count, a duration, or a value clamped to a closed enum. The tool name is
//     clamped against the ACTUALLY REGISTERED tool set, because agent/loop.go assigns the model's
//     requested tool name to the transcript BEFORE the allowlist lookup — on an unknown-tool cycle that
//     field holds model-authored text, and model text must never reach an export sink verbatim (INV-08).
//  2. SECRET-FREE. No thought, observation, evidence payload, host, ref or arg is rendered. Those live in
//     agent_step / agent_step_evidence behind TG's own authz, and a trace store is a different trust
//     boundary with a different audience.
//  3. HONEST ABOUT WHAT IS UNKNOWN. A session whose token count was never measured renders
//     tokens_source=unknown and NO token numbers, rather than a confident zero. A zero on a session that
//     made three model calls is not a small error, it is a wrong number that a cost dashboard will average.
//
// Pure and deterministic: same session in, same spans out — so a diff of two exported trajectories is a
// diff of the investigations, not of the formatter. It gates nothing (INV-08).
package sessionspan

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxSpans bounds one session's exported trajectory. The agent's own cycle limit is far below this, so it
// is a backstop, not a policy: an exporter must not be handed an unbounded batch because a future loop
// limit was raised, and a truncated trajectory says it was truncated rather than ending silently.
const MaxSpans = 128

// Tokens is a session's token accounting as it will be published.
//
// Source is the field that keeps it honest. It is NOT derivable from the numbers: a session with 0 measured
// tokens and a session whose provider reported nothing both have Total 0, and only one of them is a fact.
type Tokens struct {
	Prompt     int
	Completion int
	Total      int
	// Calls is the number of model completions the session made; Measured is how many of those reported a
	// provider usage block. Measured < Calls means the total below is partly estimated, and the rendered
	// tokens_source says so instead of averaging the distinction away.
	Calls    int
	Measured int
}

// Source classifies the token total: "measured" (every call reported usage), "partial" (some did),
// "unknown" (none did, or no call was made). Callers render this beside the numbers; the numbers are
// withheld entirely when it is "unknown".
func (t Tokens) Source() string {
	switch {
	case t.Calls > 0 && t.Measured == t.Calls:
		return "measured"
	case t.Measured > 0:
		return "partial"
	default:
		return "unknown"
	}
}

// Step is one ReAct cycle as it will be published: its ordinal, the action the loop dispatched on, the tool
// it called (tool cycles only) and the cycle's outcome. Every one is clamped before it is rendered.
type Step struct {
	Cycle   int
	Action  string
	Tool    string
	Outcome string
}

// Session is the completed investigation to render. It is a VALUE built by the caller from what it already
// holds — this package reads no store and calls no model.
type Session struct {
	Outcome       string // agent.Outcome.String(): stop | escalate | proposed | hard-halt
	DecisionTier  string // the tier that produced the TERMINAL directive (TG-198)
	Duration      time.Duration
	Cycles        int
	ToolCalls     int
	ToolErrors    int
	ReconRefusals int // estate reads the read budget refused (TG-165) — an incomplete investigation, not an empty one
	Tokens        Tokens
	Steps         []Step
}

// The closed enums. They mirror agent.Outcome.String() and the per-cycle action/outcome vocabulary in
// agent/loop.go, kept as plain literals so this leaf package does not import the agent.
var (
	outcomeSet = map[string]bool{"stop": true, "escalate": true, "proposed": true, "hard-halt": true}
	actionSet  = map[string]bool{"tool": true, "propose": true, "decide": true, "stop": true, "escalate": true}
	// cycleOutcomeSet mirrors the step outcomes agent/loop.go writes to agent_step.
	cycleOutcomeSet = map[string]bool{"ok": true, "tool-error": true, "stop": true, "escalate": true,
		"proposed": true, "hard-halt": true, "screened": true}
	// tierSet is the closed set of tiers the Go side selects. An operator-set eval-arm tier
	// (TG_EVAL_ARM_INVESTIGATE) is unbounded by design and folds to "other" — a label a deployment can
	// choose is not a label a trace store should be asked to hold.
	tierSet = map[string]bool{"primary": true, "fast": true, "embed": true}
)

func clamp(set map[string]bool, s string) string {
	if set[s] {
		return s
	}
	if s == "" {
		return "unset"
	}
	return "other"
}

// Build renders the session as ordered span strings: one session-summary span first, then one span per
// ReAct cycle in transcript order.
//
// knownTools is the ACTUALLY REGISTERED tool allowlist (agent.ToolSet.Names()). A tool name outside it is
// rendered "other" — that is not defensive tidiness, it is the INV-08 boundary: the transcript's Tool field
// holds the model's requested name, so on an unknown-tool cycle (the recoverable TOOL_ERROR path) it is
// attacker-influenceable text. An EMPTY knownTools folds every tool to "other" rather than passing them
// through, because a caller that forgot to pass the allowlist must lose fidelity, never containment.
func Build(s Session, knownTools []string) []string {
	allow := make(map[string]bool, len(knownTools))
	for _, n := range knownTools {
		allow[n] = true
	}
	spans := make([]string, 0, len(s.Steps)+2)
	spans = append(spans, summarySpan(s))
	for _, st := range s.Steps {
		if len(spans) >= MaxSpans {
			// Say that the trajectory was cut, rather than ending on a cycle that looks terminal. A trace
			// that silently stops short is read as a session that silently stopped short.
			spans = append(spans, "name=trajectory.truncated cycles_recorded="+strconv.Itoa(len(spans)-1)+
				" cycles_total="+strconv.Itoa(len(s.Steps)))
			break
		}
		spans = append(spans, cycleSpan(st, allow))
	}
	return spans
}

// summarySpan renders the one-per-session span carrying cost, tokens, latency and the terminal outcome.
func summarySpan(s Session) string {
	var b strings.Builder
	b.WriteString("name=session.investigate")
	b.WriteString(" outcome=" + clamp(outcomeSet, s.Outcome))
	b.WriteString(" decision_tier=" + clamp(tierSet, s.DecisionTier))
	b.WriteString(" duration_ms=" + strconv.FormatInt(s.Duration.Milliseconds(), 10))
	b.WriteString(" cycles=" + strconv.Itoa(s.Cycles))
	b.WriteString(" tool_calls=" + strconv.Itoa(s.ToolCalls))
	b.WriteString(" tool_errors=" + strconv.Itoa(s.ToolErrors))
	b.WriteString(" recon_refusals=" + strconv.Itoa(s.ReconRefusals))
	b.WriteString(" model_calls=" + strconv.Itoa(s.Tokens.Calls))
	src := s.Tokens.Source()
	b.WriteString(" tokens_source=" + src)
	// WITHHELD, not zeroed, when nothing was measured — the same discipline core/metrics applies to a
	// calibration curve at N=0. A trace that reports tokens_total=0 for a session that made model calls
	// teaches a dashboard that investigations are free.
	if src != "unknown" {
		b.WriteString(" tokens_total=" + strconv.Itoa(s.Tokens.Total))
		b.WriteString(" tokens_prompt=" + strconv.Itoa(s.Tokens.Prompt))
		b.WriteString(" tokens_completion=" + strconv.Itoa(s.Tokens.Completion))
		b.WriteString(" tokens_measured_calls=" + strconv.Itoa(s.Tokens.Measured))
	}
	return b.String()
}

// cycleSpan renders one ReAct cycle. Only clamped enums reach the string.
func cycleSpan(st Step, allow map[string]bool) string {
	tool := "none"
	if st.Tool != "" {
		tool = "other"
		if allow[st.Tool] {
			tool = st.Tool
		}
	}
	return "name=cycle cycle=" + strconv.Itoa(st.Cycle) +
		" action=" + clamp(actionSet, st.Action) +
		" tool=" + tool +
		" outcome=" + clamp(cycleOutcomeSet, st.Outcome)
}

// KnownTools returns a sorted copy of names, so a caller can hand Build a stable allowlist.
func KnownTools(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}
