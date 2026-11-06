package trace

import "context"

// AgentStep is one NON-SECRET, SCRUBBED row of the per-ReAct-cycle agent transcript (spec/020 T-020-8,
// REQ-2008): the 1-based cycle ordinal, the model's CoT thought for that cycle, the tool it invoked, a summary
// of the tool observation, and the per-cycle outcome. Keyed by external_ref so it joins the decision-tracer
// walk. Every text field is the OUTPUT of core/screen.Scrub (secrets redacted, injections neutralized) — this
// record NEVER carries a value-shaped secret (INV-13), and the thought is DATA only, never control flow (INV-08).
type AgentStep struct {
	ExternalRef string
	Cycle       int
	Thought     string
	Tool        string
	Observation string
	Outcome     string
}

// AgentStepSink records one scrubbed agent-step row per ReAct cycle. OBSERVE-ONLY by contract: the Runner emits
// into it as a pure side effect and NEVER reads it back to make a decision, and an Emit error MUST NOT change
// the investigation outcome. A nil sink is a no-op — the loop behaves identically without it.
type AgentStepSink interface {
	Emit(ctx context.Context, s AgentStep) error
}
