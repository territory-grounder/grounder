package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/screen"
)

// protocolPreamble is the system message that teaches the model the ONLY output contract: a single JSON
// directive, the read-only tool allowlist, and the proposal grammar. Without it the model returns prose and
// EVERY session fails closed to "unparseable" — the seed's behavioral skills describe HOW to reason but not
// the WIRE FORMAT this loop parses (an eval over real NL incidents found 0% proposals: bare stops). The tool
// list is built from the LIVE tool set, disclosed for the workflow-decided execution class (TG-215) — every
// class except FAST_AGENT renders today's full flat catalog byte-identically — so the model names only tools
// it may actually call; an unknown tool is still refused (INV-08 — no model token becomes control flow).
// Pure: a deterministic function of the tool set, the class and the compiled op-class registry.
func protocolPreamble(tools *ToolSet, class execclass.Class, guidance string) model.Message {
	toolList := toolListFor(tools, class)
	// The actuatable op-class SCHEMA CATALOG, rendered from the ONE op-class schema registry
	// (core/actuate/opschema) — the SAME source the parser/interceptor/runner/effect leaf read. It publishes,
	// per actuatable op_class, the structured params it requires (name/type/required/example) so the model
	// emits a COMPLETE proposal (e.g. restart-service WITH params.unit) "from its parameters alone" (ACI). This
	// MIRRORS the read-only tool catalog above; it is prompt DATA only — the op_class is validated by an exact
	// registry lookup, never executed as text (INV-08). Without it the model omitted params.unit and the fix
	// only failed at execute-time with an opaque empty argv.
	opClassCatalog := opschema.Catalog()
	if opClassCatalog == "" {
		// Day-zero / empty-catalog posture (spec/026 REQ-2601): free-form proposing is the DECLARED DUTY,
		// and execution stays impossible by construction (nothing unregistered seals; every effect leaf
		// refuses; never-auto floor) — so the text says both, and the model neither stands down for lack
		// of a listed verb nor believes anything it names will run.
		opClassCatalog = emptyOpClassCatalog()
	}
	return model.Message{Role: "system", Content: renderPreamble(toolList, opClassCatalog, guidance)}
}

// toolListFor resolves the preamble's tool-list text for an execution class: the class-keyed catalog
// (ToolSet.CatalogFor — full for every reachable class, progressive disclosure for FAST_AGENT), or the
// no-tools sentinel when nothing is registered. One resolver, shared by protocolPreamble and the
// per-class byte-identity goldens, so the goldens can never pin a different path than the loop runs.
func toolListFor(tools *ToolSet, class execclass.Class) string {
	if cat := tools.CatalogFor(class); cat != "" {
		// The STRUCTURED catalog (name + description + typed params) replaces the bare name list so the
		// model can call a tool "from its description and parameters alone" (Writing Effective Tools, ACI —
		// design-wisdom #5). Dispatch is still an exact name lookup; the catalog is prompt DATA only.
		return cat
	}
	return "none — you cannot gather evidence; propose only if the alert itself is sufficient, else stop"
}

// renderPreamble assembles the base prompt from its parts — extracted pure (TG-472) so the byte-identity
// golden pins the assembly over fixed inputs, independent of the live tool/op-class catalogs. guidance is
// the store-composed guidance half (C-3b); empty — every non-worker caller, and the worker's total
// fallback — renders the embedded half byte-identically to the pre-store assembly.
func renderPreamble(toolList, opClassCatalog, guidance string) string {
	protocol := strings.ReplaceAll(promptPart("base-prompt-protocol"), "{{TOOL_CATALOG}}", toolList)
	protocol = strings.ReplaceAll(protocol, "{{OPCLASS_CATALOG}}", opClassCatalog)
	if strings.TrimSpace(guidance) == "" {
		guidance = promptPart("base-prompt-guidance")
	}
	return protocol + "\n" + guidance
}

// emptyOpClassCatalog is the day-zero posture text (spec/026 REQ-2601) — a function so the embed swap
// (TG-472) can relocate the prose without touching the call site.
func emptyOpClassCatalog() string {
	return promptPart("base-prompt-empty-opclass")
}

// stripFences unwraps a fenced code block (` + "```json … ``` or ``` … ```" + `) that many chat models emit
// around a JSON directive. It is a deterministic UNWRAP of a known envelope, NOT a looser grammar: the
// content inside must still be strictly valid JSON for the loop to act on it, so unparseable content after
// unwrapping still fails closed (INV-08).
func stripFences(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return raw
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // drop the opening fence line (``` or ```json)
	}
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j] // drop the closing fence
	}
	return strings.TrimSpace(s)
}

// Completer is the minimal LLM interface the agent needs; the bundled LiteLLM gateway
// (adapters/model.Gateway) satisfies it. The agent calls ONLY this — it never spawns a subprocess.
type Completer interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// Outcome is the terminal state of an agent run.
type Outcome int

const (
	// OutcomeStop: the agent halted without a usable proposal (low confidence / unparseable / unknown
	// action) — fail closed to a poll. Zero value on purpose.
	OutcomeStop Outcome = iota
	// OutcomeEscalate: a proposal exists but confidence is below the escalate threshold (or a limit was
	// hit) — it requires a human poll, not autonomous action.
	OutcomeEscalate
	// OutcomeProposed: a high-confidence, schema-valid proposal was produced.
	OutcomeProposed
	// OutcomeHardHalt: the cycle limit was reached — the agent hard-halts.
	OutcomeHardHalt
)

// StopReasons is the CLOSED set of orchestrator-computed causes the loop may record for a halt
// (spec/011 REQ-1013). Exported so an oracle asserts against THIS list rather than a hand-copied one.
//
// A parallel list maintained beside its source is the defect shape this codebase keeps finding, and it bit
// here immediately: the first oracle written for the stop reason enumerated SIX of these EIGHT by hand and
// passed only because its fixture happened to produce a listed cause. A test that hand-copies a vocabulary is
// a test that goes stale silently.
//
// Data, never control flow: the reason RECORDS why a session stopped and nothing gates on its value.
func StopReasons() []string {
	return []string{
		"agent requested stop",
		"confidence below stop threshold",
		"model call failed",
		"proposal failed the single grammar",
		"trajectory veto — ",
		"unknown action ",
		"unparseable model output",
		"write tool withheld",
	}
}

func (o Outcome) String() string {
	switch o {
	case OutcomeEscalate:
		return "escalate"
	case OutcomeProposed:
		return "proposed"
	case OutcomeHardHalt:
		return "hard-halt"
	default:
		return "stop"
	}
}

// Limits bound the ReAct loop so an unbounded/looping agent is not reachable. [F] handoff/cycle limits.
type Limits struct {
	HandoffPoll int // at this many cycles without a proposal, force an escalate/poll
	HandoffHalt int // at this many cycles, hard-halt
	// ObservationBudgetBytes caps the total bytes of the transcript sent to the model (TG-47). When it is
	// exceeded, the OLDEST tool OBSERVATIONS are compacted — their payloads elided but their
	// OBSERVATION[<id>] envelope kept verbatim so the id stays citable — while the most recent observations
	// and the preamble+seed stay verbatim. 0 disables compaction (unbounded, the pre-TG-47 behaviour).
	ObservationBudgetBytes int
}

// DefaultLimits: escalate to a poll at 5 cycles, hard-halt at 10.
func DefaultLimits() Limits {
	return Limits{HandoffPoll: 5, HandoffHalt: 10, ObservationBudgetBytes: 64000}
}

// directive is the typed shape a model turn must emit. The agent dispatches on Action by an exact
// switch (never by executing model text); an unknown action fails closed. When Action=="propose" the
// Proposal payload is handed to the single ParseProposal grammar.
type directive struct {
	Action string `json:"action"` // "tool" | "propose" | "stop"
	Tool   string `json:"tool"`
	Args   argMap `json:"args"`
	// Tools is the BATCHED form of a tool directive (TG-49): up to MaxBatchTools INDEPENDENT read-only
	// calls, dispatched concurrently, all within THIS one cycle. Mutually exclusive with Tool — a
	// directive carrying both shapes is refused as ambiguous before anything runs, because a grammar with
	// two simultaneous meanings is exactly what INV-08's exact-dispatch rule exists to forbid.
	Tools      []batchToolCall `json:"tools"`
	Confidence float64         `json:"confidence"`
	Proposal   json.RawMessage `json:"proposal"`
	// Reason + EvidenceIDs ground a STOP: "no action warranted" is a conclusion and needs its evidence
	// like any other (REQ-1008). Both are DATA — they never influence control flow beyond the citation
	// check, and only ids the agent actually captured are kept.
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids"`
	// Thought is an OPTIONAL, size-capped ReAct/CoT reasoning trace the model MAY emit. It is parsed
	// ONLY AS DATA and recorded for audit/forensics (design-wisdom #7); NO token of it becomes control
	// flow — the dispatch switch reads Action alone, so a `thought` saying "stop" while Action=="tool"
	// still runs the tool (INV-08). The preamble reinstates the CoT channel the "one JSON object" rule
	// had suppressed, without letting a model token decide dispatch.
	Thought string `json:"thought"`
}

// argMap tolerates a model that puts NON-string values (arrays, objects, numbers) in a tool's args — it
// stringifies each value instead of failing the whole directive. A general model prompted to call a tool
// routinely over-specifies args (e.g. "checks":["cmd1","cmd2"]); with a strict map[string]string that
// rejected the ENTIRE tool-call as "unparseable" and stalled every session (found by an eval — the model
// wanted to investigate, the parser refused it). The tool still receives string args (INV-08 unchanged: an
// unknown tool is still refused, no model token is executed); a non-string value keeps its compact JSON.
type argMap map[string]string

func (a *argMap) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*a = nil
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			m[k] = s // a JSON string value
		} else {
			m[k] = strings.TrimSpace(string(v)) // array/object/number → its compact JSON form
		}
	}
	*a = m
	return nil
}

// Result is the outcome of an agent run.
type Result struct {
	Outcome     Outcome
	Proposal    proposal.Proposal // valid iff Outcome is Proposed or Escalate
	Confidence  float64
	Cycles      int
	ToolResults []ToolResult
	Reason      string // TG's mechanical reason (control-plane text, never model output)
	// Conclusion is the model's grounded no-action rationale on a requested stop (REQ-1008) — untrusted
	// DATA for the session record/judge, distinct from the mechanical Reason. ConclusionEvidence keeps
	// only ids the agent ACTUALLY captured (a fabricated citation is dropped, never stored).
	Conclusion         string
	ConclusionEvidence []string
	// Thoughts is the OPTIONAL, size-capped ReAct/CoT reasoning the model emitted per cycle, captured in
	// emission order as untrusted DATA for auditability/forensics (design-wisdom #7). It is NEVER read
	// for control flow — dispatch switches on Action alone (INV-08): a `thought` claiming "stop" while
	// Action is "tool" still runs the tool. Empty thoughts are skipped.
	Thoughts []string
	// ScreenNotes records, per screened OBSERVATION, that a live tool RESULT re-entering the model prompt
	// tripped the input screen — the tool-result analogue of the seed-block provenance notes (REQ-1012).
	// A tool result from a compromised or attacker-influenced host during read-only investigation is a
	// prompt-INJECTION surface (the "lethal trifecta" tool-result path); each result is passed through the
	// SAME neutralize-and-flag screen the seed blocks get (screen.Scrub) before it is appended as an
	// OBSERVATION, so a hostile span is defanged and any leaked secret redacted. This slice is untrusted
	// DATA for the session record only: a screened observation is NEVER by itself a POLL_PAUSE/stop signal
	// (mirroring how secret redaction is deliberately not a Detect signal), so an attacker cannot suppress
	// triage by embedding an injection string — under-triage is the worse failure (INV-08). Empty ⇒ every
	// observation passed the screen byte-clean.
	ScreenNotes []string
	// Steps is the cycle-ALIGNED ReAct transcript for the decision tracer (spec/020 T-020-8): exactly one
	// AgentCycle per loop iteration, in order, carrying the REAL per-cycle ordinal — so a per-cycle tracer row
	// pairs the right thought with the right tool. UNTRUSTED DATA, NEVER read for control flow (INV-08). The
	// sparse Thoughts/ToolResults slices are retained UNCHANGED for their existing consumers (evidence binding,
	// metrics, judge); Steps is a superset transcript, not a replacement.
	Steps []AgentCycle
	// Trajectory is the ordered (tool, args-key) record the loop builds and analyzes for a stuck/thrashing
	// path — the SAME deterministic signal TrajectoryVeto halts on at runtime — surfaced here so the offline
	// eval scorer can grade the ordered tool path (TG-525). UNTRUSTED DATA / observability only, NEVER read for
	// control flow (INV-08). The runner digests every ArgsKey (HashedTrajectory) before it leaves the activity,
	// so no raw argument value is ever persisted or exported.
	Trajectory []TrajectoryStep
	// DecisionTier is the model tier of the LAST completion this run made — the tier that produced the
	// TERMINAL directive, i.e. the model that actually DECIDED (TG-198). It is NOT ModelName: the TG-60
	// decide-nudge switches the forced-decision cycle to DecisionModelName, so a session routinely
	// investigates on "fast" and decides on "primary". Recording only the investigation tier made those two
	// facts one column, and the whole 537-incident corpus attributed every decision to "fast" — so "did the
	// expensive tier decide better?" could not be asked of TG's own history, and the three-arm tier A/B
	// (TG-204) had no dependent variable to measure. Stamped BEFORE each Complete, so it also names the tier
	// that was in flight when a model call FAILED (that stop is attributable too).
	//
	// DecideNudgeFired reports whether the TG-60 poll-limit nudge fired at all — the fact that distinguishes
	// "converged on its own" from "was told to decide now". It is NOT derivable from the two tiers: when an
	// eval arm sets both to the same model (TG-204 runs exactly that arm) they are equal whether or not the
	// nudge fired, and a same-tier arm is precisely where the confound would be invisible.
	//
	// OBSERVABILITY ONLY. Neither re-enters the decision path — they are recorded after the fact and read
	// no gate (INV-08).
	DecisionTier     string
	DecideNudgeFired bool
	// ReconRefusals counts the estate reads the read-lane budget refused this session, and ReconRefusalReason
	// is the FIRST refusal's explanation (TG-165). They exist so a bounded investigation can never be
	// mistaken for an empty one: a session that hit the budget gathered less than it wanted to, and the
	// operator/judge/console must be able to tell that from "the estate had nothing to say". Recorded after
	// the fact; neither gates anything (INV-08).
	ReconRefusals      int
	ReconRefusalReason string
	// DecideSamples is the TG-46 self-consistency record: the structured (kind, op_class, target) of EVERY
	// forced-decision sample drawn, in draw order — nil whenever the sampled path did not run (DecideSampleN
	// ≤ 1, or the session never reached a forced decision), so the un-sampled record is byte-identical to
	// before this field existed. DecideDisagreement counts the samples whose vote key differs from the
	// winner's (an undecided tool/invalid sample always counts); DecideTieBroken records that no strict
	// majority existed and the conservative resolution selected the winner — the loud-marker signal. ALL
	// THREE ARE DATA ONLY (INV-08): recorded for the session's provenance channel, read by no gate.
	DecideSamples      []DecideSample
	DecideDisagreement int
	DecideTieBroken    bool
}

// AgentCycle is one ReAct cycle's cycle-aligned transcript record (spec/020 T-020-8, REQ-2008): its real
// ordinal, the optional CoT thought, the action taken, the tool name (tool cycles only), a short NON-SECRET
// observation summary, and the per-cycle outcome. DATA-only for the tracer — never control flow (INV-08); every
// text field is run through screen.Scrub before it is persisted to agent_step.
type AgentCycle struct {
	Cycle       int
	Thought     string
	Action      string
	Tool        string
	Observation string
	Outcome     string
	// EvidenceID and Evidence carry the GROUND TRUTH behind this cycle (TG-272): the tool result's own id and
	// the SCREENED payload the model was actually shown. Observation above is only a reference — the string
	// "observed lnms-alerts-dc1pve01" — so before these fields existed, what the tool returned was computed,
	// screened, handed to the model and then dropped. The console's "ground truth <tool>" citation had nothing
	// behind it, and 3241 recorded sessions hold no tool output at all.
	//
	// Evidence is the output of screenToolOutput (injection spans neutralized, secrets redacted), NEVER the raw
	// result: a tool result is attacker-influenceable data (INV-08) and can carry a leaked token (INV-13). Empty
	// on non-tool cycles and on tool errors, where there is no observation to stand behind.
	EvidenceID string
	Evidence   string
}

// Agent is the native Go ReAct / tool-calling loop over the LiteLLM gateway.
type Agent struct {
	Model     Completer
	Tools     *ToolSet
	Limits    Limits
	ModelName string
	// DecisionModelName is the tier used for the ONE forced-decision cycle at the poll limit (TG-60): the
	// low-latency ModelName ("fast") is right for the many-call investigation, but too weak to reliably obey
	// the "decide now" nudge, so the agent kept investigating and handed off empty. A single reasoning-tier
	// call there converges far more often (its latency is paid once, not per cycle). Empty ⇒ same as ModelName.
	DecisionModelName string
	User              string
	// Recon is the read-lane VOLUME bound (TG-165). nil ⇒ unbounded, which is the pre-TG-165 behaviour and
	// the safe default for every caller that is not the worker (oracles, the offline eval harness); the
	// worker wires *safety.ReconGovernor and cmd/worker pins that wiring with its own oracle, because an
	// unwired bound is this repository's most-repeated defect.
	Recon ReconLimiter
	// Guidance is the STORE-COMPOSED base-prompt guidance half (C-3b): the worker resolves the ClassPrompt
	// row (production or trial arm) and threads the body here; empty — every non-worker caller (oracles,
	// the offline eval harness) and the worker's total fallback — renders the EMBEDDED half, byte-identical
	// to the pre-store assembly. Rendering input only; which body composes is decided by the store's
	// audited production/trial state, never by any model token (INV-08).
	Guidance string
	// Class is the workflow-decided execution class (TG-42), threaded by the activity so the preamble's
	// tool catalog is disclosed per class (TG-215): FAST_AGENT gets the namespaced progressive-disclosure
	// render; every other value — including this zero value, which is what every non-activity caller
	// (oracles, the offline eval harness) leaves — renders the full flat catalog byte-identical to before
	// this field existed. Rendering input only: it never gates a tool, a cycle, or a dispatch (INV-08).
	Class execclass.Class
	// DecideSampleN is the TG-46 self-consistency width: how many INDEPENDENT samples the ONE forced-
	// decision cycle draws before a MECHANICAL majority vote selects the decision (decide_vote.go). It
	// applies to that cycle ONLY — investigation cycles always make exactly one call. 0 or 1 — the zero
	// value every non-activity caller leaves — keeps the single-call path byte-identical to before this
	// field existed; the activity resolves the width per class/severity (deep/critical sample, everything
	// else stays 1). The extra draws spend from the SAME session rails as every other completion: the
	// per-completion output cap, the gateway's per-session token budget (TG-48) and the per-session token
	// tally (TG-44) all see them, because they go through the same wrapped Completer on the same stamped ctx.
	DecideSampleN int
	// Checkpoint wires TG-47 durable per-turn checkpointing: when non-nil, Run restores from Checkpoint.Resume
	// (if set) before the loop and emits a cycle-boundary snapshot via Checkpoint.Emit at the top of every
	// cycle. nil — the zero value every non-activity caller (and the worker with the flag OFF) leaves — is
	// byte-identical to before this field existed: no restore, no snapshot. The Temporal activity wires it only
	// when TG_INVESTIGATE_DURABLE_CHECKPOINT is armed (durable_checkpoint.go).
	Checkpoint *CheckpointHooks
}

// ReconLimiter bounds how many estate READS an investigation — and the process as a whole — may dispatch.
//
// It is an interface here, satisfied by *core/safety.ReconGovernor, for the same reason safety's own kill
// seams are interfaces: the agent loop must not import the safety core to be bounded by it, and an oracle
// must be able to drive the bound without building a governor.
//
// The read lane fails OPEN on ERROR by law (docs/CONSTITUTION.md §3.3) and that is unchanged — this is a
// bound on VOLUME, not on any individual read. It says "not this many, this fast", never "not this one".
type ReconLimiter interface {
	// Admit reports whether one more estate read may be dispatched for this session. A non-nil error is a
	// REFUSAL whose message must be shown to the model and recorded for the operator — it names the bound,
	// so a bounded investigation is never mistaken for an empty estate.
	Admit(session string) error
	// Record meters one DISPATCHED read: session, tool, and the estate object it was aimed at (TG-166's
	// target). Called even when the tool then errors — an errored probe still touched the estate, and a
	// probe that finds nothing is exactly what enumeration looks like.
	Record(session, tool, target string)
}

// Run drives the loop over a seeded conversation. Each turn: call the model through the gateway, parse
// the response as a typed directive; if a tool call, dispatch to the allowlisted READ-ONLY tool and
// append the orchestrator-captured result as an observation; if a proposal, parse it via the single
// ParseProposal grammar. Confidence below StopThreshold stops; the cycle limits force an escalate then
// a hard-halt. ANY unparseable output or unknown tool/action fails closed — model text is never
// executed. [O] INV-08, spec/011.
func (a *Agent) Run(ctx context.Context, seed []model.Message) (res Result, err error) {
	// STAMP THE SESSION (TG-297). A tool carrying a SESSION-level budget — search-host-logs is the first —
	// can only key it off ctx, because Invoke is handed nothing else that differs between investigations.
	// This is the one place that knows a session is starting, so it is the one place the stamp belongs; an
	// unstamped ctx makes every caller share one budget (see SessionFrom), which over-binds loudly rather
	// than silently never binding.
	ctx = WithSession(ctx, NewSessionID(a.User))
	// The same id keys the read-lane recon budget (TG-165). Read once here, from the ctx just stamped, so
	// the budget and the tools that carry their own session caps are keyed on exactly the same session.
	sessionID := SessionFrom(ctx)
	// Prepend the output protocol so the model knows the directive/proposal WIRE FORMAT + the read-only tool
	// allowlist. Without it the model returns prose → every session fails closed to "unparseable". The seed's
	// skills teach HOW to reason; this teaches the format the loop parses (spec/011).
	msgs := append([]model.Message{protocolPreamble(a.Tools, a.Class, a.Guidance)}, seed...)
	seedLen := len(msgs) // the preamble+seed prefix is the incident's fixed context — never compacted (TG-47)
	lim := a.Limits
	if lim.HandoffHalt == 0 {
		lim = DefaultLimits()
	}
	var traj []TrajectoryStep // the record of tool calls, analyzed for a stuck loop (deterministic, INV-08)
	// TG-525: attach the ordered trajectory to the result on EVERY exit path (the loop returns from ~8 points),
	// so the offline eval scorer grades the same path TrajectoryVeto analyzed. traj is captured by reference, so
	// this deferred read sees its final value; the runner digests each ArgsKey before it leaves the activity.
	defer func() { res.Trajectory = traj }()
	stopNudged := false   // the grounded-stop nudge (REQ-1008) fires at most once — never grind the safe exit
	decideNudged := false // the poll-limit decide-now nudge (TG-60) fires at most once — converge before handing off
	decideCycle := false  // the cycle immediately AFTER the nudge is the forced decision — run it on the capable tier

	// TG-47 durable resume: restore the loop's evolving state from a prior activity attempt's last checkpoint,
	// so a crashed investigation continues from the cycle it reached instead of re-running from cycle 1. A nil
	// hooks pointer or nil Resume ⇒ a fresh run (startCycle 1), byte-identical to before this existed.
	startCycle := 1
	if a.Checkpoint != nil && a.Checkpoint.Resume != nil {
		cp := a.Checkpoint.Resume
		msgs, seedLen, res, traj = cp.Msgs, cp.SeedLen, cp.Res, cp.Traj
		stopNudged, decideNudged, decideCycle = cp.StopNudged, cp.DecideNudged, cp.DecideCycle
		startCycle = cp.Cycle
		// Restore the SAME session identity (TG-297) the crashed attempt ran under, so the recon budget
		// (TG-165) and per-session tool caps are keyed on one logical investigation across the resume, and
		// logs/spans correlate — rather than the fresh id minted above. (The governors' per-session COUNTS
		// are process-local and still reset on a cross-process resume; see Checkpoint.SessionID.)
		if cp.SessionID != "" {
			ctx = WithSession(ctx, cp.SessionID)
			sessionID = cp.SessionID
		}
	}

	for cycle := startCycle; cycle <= lim.HandoffHalt; cycle++ {
		// TG-47: snapshot the loop at the cycle boundary — BEFORE the model call — so a crash mid-cycle resumes
		// from a CLEAN cycle, re-issuing only idempotent read-only estate probes (never a mutation: actuation is
		// a separate, later step). Emit MUST snapshot/serialize immediately; the loop mutates msgs/res/traj below.
		if a.Checkpoint != nil && a.Checkpoint.Emit != nil {
			a.Checkpoint.Emit(Checkpoint{
				Cycle: cycle, Msgs: msgs, SeedLen: seedLen, Res: res, Traj: traj,
				StopNudged: stopNudged, DecideNudged: decideNudged, DecideCycle: decideCycle,
				SessionID: sessionID,
			})
		}
		res.Cycles = cycle
		// spec/020 T-020-8: one cycle-aligned transcript record per iteration, updated IN PLACE via a pointer
		// (never re-appended within this cycle, so the pointer stays valid) — captured even when the cycle
		// returns early. DATA-only (INV-08); it changes no dispatch.
		res.Steps = append(res.Steps, AgentCycle{Cycle: cycle})
		step := &res.Steps[len(res.Steps)-1]
		investigatedThisCycle := false // true iff this cycle was a tool call — the TG-60 decide-nudge targets ONLY that
		modelName := a.ModelName
		decideSampleN := 0
		if decideCycle {
			decideCycle = false
			if a.DecisionModelName != "" {
				modelName = a.DecisionModelName // TG-60: the forced-decision cycle runs on the capable tier
			}
			// TG-46: the N-sample majority applies to THIS one forced-decision cycle only. The knob is read
			// here and nowhere else in the loop, so an investigation cycle can never sample by construction.
			decideSampleN = a.DecideSampleN
		}
		// TG-198: stamp the tier this cycle is about to run on. The LAST value stamped is the tier that
		// produced the TERMINAL directive — the decision. Stamped before the call, not after, so a model-call
		// failure still names the tier that failed rather than leaving the terminus unattributed.
		res.DecisionTier = modelName
		// TG-47: keep the transcript within the observation budget before every model call — elide the OLDEST
		// tool-observation payloads (their ids kept citable) so a long DeepInvestigation does not grow the
		// context unbounded. Idempotent, and a no-op under budget or when the budget is 0.
		msgs = compactObservationBudget(msgs, seedLen, lim.ObservationBudgetBytes)
		raw, err := a.Model.Complete(ctx, a.User, modelName, msgs)
		if err != nil {
			res.Outcome, res.Reason = OutcomeStop, "model call failed"
			return res, err
		}
		if decideSampleN > 1 {
			// TG-46 SELF-CONSISTENCY DRAW. The call above IS the first sample — kept as the plain call so a
			// first-draw failure takes exactly the single-call error path. The remaining draws re-ask the SAME
			// prompt (msgs is not touched between draws; divergence comes from the gateway's own sampling); a
			// mid-draw error stops drawing and the vote runs over the samples already held — the width is
			// best-effort, the CONTENT is not, and a feature meant to harden deep/critical decisions must not
			// multiply the session's failure surface by the sample count. Selection is decideByMajority —
			// counting over structured fields, never a model choosing among samples (INV-08) — and the winning
			// sample's full text re-enters the unchanged parse path below as `raw`, so every downstream gate
			// sees exactly what a single draw of that text would have shown it. The draw record is DATA on the
			// Result; the activity surfaces it on the session's provenance channel.
			raws := make([]string, 1, decideSampleN)
			raws[0] = raw
			for len(raws) < decideSampleN {
				r, derr := a.Model.Complete(ctx, a.User, modelName, msgs)
				if derr != nil {
					break
				}
				raws = append(raws, r)
			}
			raw, res.DecideSamples, res.DecideDisagreement, res.DecideTieBroken = decideByMajority(raws)
			// The cycle-aligned transcript names the draw (spec/020 shape). A winning tool call overwrites
			// this with its own observation below — the sampling record itself persists on Result regardless.
			step.Observation = fmt.Sprintf("decide-samples: %d drawn, %d dissent", len(raws), res.DecideDisagreement)
		}

		var d directive
		// Unwrap a ```json … ``` fence if the model wrapped its directive (common for general chat models
		// like Mistral). A deterministic unwrap of a known envelope — NOT a looser grammar: the content must
		// still be strictly valid JSON, so unparseable content after unwrapping still fails closed (INV-08).
		// Decode the FIRST JSON value and ignore trailing bytes — some models append hallucinated content
		// (a fabricated OBSERVATION, a stray token) after their directive. The directive grammar itself is
		// still strict (unknown fields are ignored by the loop's typed struct; a non-JSON first value still
		// fails closed); only trailing noise after a valid directive is tolerated.
		if json.NewDecoder(strings.NewReader(stripFences(raw))).Decode(&d) != nil {
			// unparseable model output — fail closed, do NOT fall back to a looser grammar
			res.Outcome, res.Reason = OutcomeStop, "unparseable model output"
			return res, nil
		}
		// #7 thought-as-data: record the OPTIONAL CoT trace for audit BEFORE any dispatch decision, so it is
		// captured on every parsed cycle (even one that then stops on low confidence) and it is provably NOT
		// consulted for control flow — the switch below reads d.Action alone (INV-08). A `thought` claiming a
		// different action changes nothing that runs.
		if th := clipThought(d.Thought); th != "" {
			res.Thoughts = append(res.Thoughts, th)
			step.Thought = th // cycle-aligned copy for the tracer (spec/020 T-020-8); Thoughts kept as-is
		}
		step.Action = d.Action
		conf := d.Confidence
		if conf == 0 {
			// no typed confidence — try the parseable prose scalar; still absent ⇒ 0 ⇒ fail closed
			if v, ok := ParseConfidence(raw); ok {
				conf = v
			}
		}
		d.Confidence = conf
		res.Confidence = conf
		if conf < StopThreshold {
			res.Outcome, res.Reason = OutcomeStop, "confidence below stop threshold"
			return res, nil
		}

		switch d.Action {
		case "tool":
			if len(d.Tools) > 0 {
				// TG-49 — BATCHED read-only dispatch: one directive, up to MaxBatchTools independent
				// reads, ONE cycle. Every check the single-tool path runs below is run per entry inside
				// (exact allowlist lookup, read-only, trajectory, ACI args, recon Admit+Record), each
				// result is screened and enveloped exactly as a single call's would be, and the combined
				// observation lists results in DIRECTIVE order regardless of completion order. halt=true
				// is a session stop the batch decided exactly as the single path would have (write tool
				// withheld / trajectory veto), already recorded on res. NOTE: `step` may dangle after
				// this call (the batch appends sibling Steps rows) — it is not touched again this cycle.
				if a.runToolBatch(ctx, sessionID, raw, cycle, d, &res, &msgs, &traj) {
					return res, nil
				}
				investigatedThisCycle = true
				break
			}
			step.Tool = d.Tool           // cycle-aligned tool name for the tracer (spec/020 T-020-8)
			t, ok := a.Tools.Get(d.Tool) // exact allowlist lookup — never execute model text
			if !ok {
				// #4 recoverable unknown-tool: a mis-named tool (e.g. the model reaching for
				// "get-host-services" when the real diagnostic is "check-host-services") becomes an
				// actionable TOOL_ERROR that lists the tools that DO exist, NOT a session abort — the model
				// retries with a valid name (the SAME ReAct recovery as the arg-validation and tool-invoke
				// errors below). The unknown name is NEVER dispatched, so fail-closed / INV-08 is preserved
				// (no model token becomes control flow); the loop stays bounded by the same cycle/thrash
				// limits. (Formerly this OutcomeStop-ed the whole session on the FIRST tool-name miss — an
				// empty stand-down with no grounding, e.g. a service-fault triage that could not reach
				// check-host-services and stood down without investigating.)
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_ERROR[%s]: no such tool. Available tools: %s", d.Tool, strings.Join(a.Tools.Names(), ", "))})
				step.Observation, step.Outcome = "TOOL_ERROR (unknown tool)", "tool-error"
				investigatedThisCycle = true
				break // recover: fall through to the cycle-limit check — the session continues, never aborts
			}
			if !t.ReadOnly() {
				res.Outcome, res.Reason = OutcomeStop, "write tool withheld"
				return res, nil
			}
			// Record the step and veto a stuck trajectory — a loop re-asking the same question, ignoring its
			// observations, is halted BEFORE it burns the full cycle budget (a deterministic check of the
			// agent's own actions; no model token is consulted, INV-08). Recorded BEFORE dispatch so a tool
			// call that repeatedly ERRORS with the same args (below, #6) is still thrash-bounded, not just a
			// call that returns.
			traj = append(traj, TrajectoryStep{Tool: d.Tool, ArgsKey: ArgsKey(d.Args)})
			if veto, reason := TrajectoryVeto(traj); veto {
				res.Outcome, res.Reason = OutcomeStop, "trajectory veto — "+reason
				return res, nil
			}
			// #5 poka-yoke: screen the args against the tool's declared ACI schema (missing-required /
			// out-of-enum) BEFORE invoking. A bad call is NOT executed against the estate — it becomes an
			// actionable TOOL_ERROR observation the model can act on (fix the call or pick another tool). A
			// tool with no schema validates trivially, so existing tools are unaffected.
			if verr := ValidateArgs(t, d.Args); verr != nil {
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_ERROR[%s]: %s", d.Tool, verr.Error())})
				step.Observation, step.Outcome = "TOOL_ERROR (arg validation)", "tool-error"
				investigatedThisCycle = true
				break // recover: fall through to the cycle-limit check — the session continues, never aborts
			}
			// THE READ-LANE VOLUME BOUND (TG-165). Consulted HERE — after the allowlist, the read-only
			// check and the arg screen, immediately before dispatch — so a refused read never reaches the
			// estate, and so a call that was going to be refused for its ARGS does not spend recon budget.
			//
			// A refusal is an OBSERVATION, not a session abort: the model is told, in words, that the read
			// budget was reached and that it must conclude from the evidence it already holds. That is the
			// whole point of the design — a bound that silently returned less would produce a confident
			// stand-down over an investigation that never happened, which is worse than either a refused
			// read or a slow one. The cycle limits still bound the session, so a repeatedly-refused agent
			// halts on the same rails as any other.
			if a.Recon != nil {
				if rerr := a.Recon.Admit(sessionID); rerr != nil {
					msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
					msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_REFUSED[%s]: %s", d.Tool, rerr.Error())})
					step.Observation, step.Outcome = "TOOL_REFUSED (recon budget)", "recon-refused"
					res.ReconRefusals++
					if res.ReconRefusalReason == "" {
						res.ReconRefusalReason = rerr.Error()
					}
					investigatedThisCycle = true
					break // the session continues on what it has — bounded, and said so
				}
			}
			tr, err := t.Invoke(ctx, d.Args)
			// Meter the DISPATCH, before the error check: an errored or empty-handed probe still reached the
			// estate, and enumeration looks exactly like a long run of probes that find nothing. The target
			// comes from the ARGUMENTS (TG-166's rule: the orchestrator knows what it asked for) so fan-out
			// is metered even when a tool forgets to stamp its result.
			if a.Recon != nil {
				a.Recon.Record(sessionID, d.Tool, TargetOf(d.Args))
			}
			if err != nil {
				// #6 recoverable tool-error: a tool's Go-error becomes an actionable observation, NOT a session
				// abort — the model may fix the call or try a DIFFERENT tool (ReAct exception handling). The
				// error text is DATA (delimited), never control flow; the loop stays bounded by the same
				// cycle/thrash limits as any other cycle. (Formerly this OutcomeStop-ed the whole session on the
				// first transient tool failure.)
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_ERROR[%s]: %s", d.Tool, err.Error())})
				step.Observation, step.Outcome = "TOOL_ERROR", "tool-error"
				investigatedThisCycle = true
				break // recover: fall through to the cycle-limit check — the session continues, never aborts
			}
			// Stamp the target from the ARGUMENTS, not from Output (TG-166). A tool may forget to set it;
			// the orchestrator knows what it asked for, and relevance must rest on that rather than on text
			// the target produced.
			if tr.Target == "" {
				tr.Target = TargetOf(d.Args)
			}
			res.ToolResults = append(res.ToolResults, tr)
			step.Observation, step.Outcome = "observed "+tr.ID, "investigate" // non-secret evidence id; Scrub'd on persist
			if !tr.Success {
				// TG-199: a FAILED call recorded as a bare "observed trk-9" hides from the operator and the
				// judge exactly what it hid from the model — that the lookup never landed. The Outcome value
				// stays "investigate": the agent_step/console vocabulary is a separate surface and widening it
				// is not this fix's business.
				step.Observation = "observed " + tr.ID + " (tool call FAILED)"
			}
			// A live tool RESULT is attacker-influenceable DATA — a compromised or hostile host during
			// read-only investigation can return text that tries to hijack the loop (the "lethal trifecta"
			// tool-result injection surface). Screen the payload through the SAME neutralize-and-flag input
			// screen the seed blocks pass (mirrors temporal/runner.screenSeedBlock) BEFORE it re-enters the
			// model prompt as an OBSERVATION: an injection span is defanged to its [SCREENED:<cat>] marker and
			// any leaked secret redacted (REQ-1012). The OBSERVATION[<id>] envelope and the real tr.ID — the
			// evidence anchor — are kept VERBATIM; only the payload is screened. A detection is RECORDED on
			// ScreenNotes but NEVER drops the observation or stops the loop: under-triage is the worse failure
			// and screening is data hygiene, not a Detect/POLL_PAUSE signal (INV-08 — mechanical policy in
			// code, no model token decides anything).
			screened, notes := screenToolOutput(tr.ID, tr.Output)
			res.ScreenNotes = append(res.ScreenNotes, notes...)
			// THE GROUND TRUTH, KEPT (TG-272). This is the exact bytes the model was shown — post-screen, so
			// storing it can never un-redact what the screen removed, and pre-prompt, so it is what the cycle
			// actually reasoned over rather than a re-derivation. Recording it HERE, beside the line that feeds
			// the model, is what makes the console's citation and the model's input provably the same artifact.
			step.EvidenceID, step.Evidence = tr.ID, screened
			// append the captured observation as DATA (delimited, screened, and STAMPED with the orchestrator's
			// own succeeded/failed verdict — TG-199), then continue reasoning
			msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
			msgs = append(msgs, model.Message{Role: "user", Content: observationEnvelope(d.Tool, tr, screened)})
			investigatedThisCycle = true

		case "propose":
			p, perr := proposal.ParseProposal(d.Proposal)
			if perr != nil {
				// A malformed proposal is a FAILURE to produce a clean fix, not a grounded "no action
				// warranted" — recording it as a bare stop with no conclusion yields a VACUOUS session the
				// judge scores 1 across dimensions. Carry an honest conclusion synthesized from the gathered
				// observations (deterministic, DATA-only, INV-08; the outcome stays a non-mutating stop).
				res.Outcome, res.Reason = OutcomeStop, "proposal failed the single grammar"
				res.Conclusion, res.ConclusionEvidence = synthesizeHandoffConclusion(res.ToolResults)
				return res, nil
			}
			// Mechanical citation gate (REQ-1007): when the agent gathered OBSERVATIONS, its proposal must
			// cite at least one REAL one — an evidence_ids list that is empty, or that names only ids the
			// agent never captured, is not grounding. The preamble mandates this; here we ENFORCE it: an
			// uncited/fabricated-citation proposal is re-prompted (not accepted), and a repeat offender
			// escalates at the poll-handoff limit below rather than landing an ungrounded auto-proposal. A
			// deterministic check of the agent's OWN captured evidence — no model token becomes control flow
			// (INV-08). When no tool was available/called there is nothing to cite, so the gate does not fire.
			if len(res.ToolResults) > 0 && !citesGatheredEvidence(p.EvidenceIDs, res.ToolResults) {
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: "REJECTED — ungrounded proposal. You gathered OBSERVATION(s) but evidence_ids cites none of them. Re-emit the SAME proposal with evidence_ids listing the exact bracketed OBSERVATION id(s) your diagnosis relied on (e.g. \"" + firstToolID(res.ToolResults) + "\"), copied verbatim. Do not invent ids."})
				break // fall through to the poll-handoff check — a repeat offender escalates, not grinds
			}
			// BIND THE TYPED CLAIM TO WHAT WAS ACTUALLY GATHERED (TG-201). Cited is decided HERE, against
			// the orchestrator's own ToolResult ids, never by the model that authored the citation — a
			// plausible, well-formed, fabricated id is exactly the failure INV-11 exists for. Binding is
			// additive: a proposal with no diagnosis is untouched.
			if p.Diagnosis.Present() {
				p.Diagnosis = p.Diagnosis.BindEvidence(gatheredIDs(res.ToolResults))
				// A model that has seen disconfirming evidence and proposes ANYWAY now says so in a field
				// instead of leaving it in an unread transcript. This is the A2 case verbatim: the
				// predecessor sees a guest was stopped deliberately and stands down; TG held the same
				// observation and proposed a restart because nothing bound evidence to the assertion.
				//
				// LOGGED, NOT VETOED. A contradiction is DATA (INV-08) and the model can be wrong about
				// what contradicts what; the gate and the judge decide what it is worth. Silently
				// discarding the proposal here would replace one invisible failure with another.
				if p.Diagnosis.HasContradiction() {
					res.ScreenNotes = append(res.ScreenNotes,
						"diagnosis:contradicted — the proposal cites GROUNDED evidence against its own root cause")
				}
			}
			res.Proposal = p
			if d.Confidence < EscalateThreshold {
				res.Outcome, res.Reason = OutcomeEscalate, "confidence below escalate threshold"
			} else {
				res.Outcome, res.Reason = OutcomeProposed, "high-confidence proposal"
			}
			step.Outcome = res.Outcome.String()
			return res, nil

		case "stop":
			// REQ-1008 — a stop after gathered observations is a CONCLUSION ("no action warranted") and
			// should ground itself like a proposal. Nudge an uncited stop ONCE; a repeat is accepted —
			// a stop is the fail-safe end state and must never be blocked into grinding on.
			if len(res.ToolResults) > 0 && !citesGatheredEvidence(d.EvidenceIDs, res.ToolResults) && !stopNudged {
				stopNudged = true
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: "UNGROUNDED stop. You gathered OBSERVATION(s) but stated no grounded reason. Re-emit the stop as {\"action\":\"stop\",\"confidence\":...,\"reason\":\"<one sentence: why no action is warranted, from the observed facts>\",\"evidence_ids\":[\"" + firstToolID(res.ToolResults) + "\", ...]} citing the exact bracketed OBSERVATION id(s) your conclusion relies on. Do not invent ids."})
				break // fall through to the poll-handoff check, mirroring the proposal gate
			}
			res.Outcome, res.Reason = OutcomeStop, "agent requested stop"
			res.Conclusion = clipConclusion(d.Reason)
			res.ConclusionEvidence = gatheredOnly(d.EvidenceIDs, res.ToolResults)
			// An ungrounded stop (empty reason even after the one-shot nudge, which a repeat is allowed to
			// bypass) would record a VACUOUS session the judge scores 1. When observations exist, synthesize an
			// honest conclusion from them so every stop is grounded — deterministic, DATA-only (INV-08); the
			// stop outcome is unchanged. A model-given reason always wins (only fills a BLANK).
			if res.Conclusion == "" && len(res.ToolResults) > 0 {
				res.Conclusion, res.ConclusionEvidence = synthesizeHandoffConclusion(res.ToolResults)
			}
			step.Outcome = res.Outcome.String()
			return res, nil

		default:
			// #4b recoverable unknown-ACTION — the exact mirror of the unknown-TOOL recovery above (#4). An
			// unrecognized action (e.g. the model putting a TOOL name — "check-host-services" — in the action
			// field instead of {"action":"tool","tool":"check-host-services"}) becomes an actionable ACTION_ERROR
			// listing the valid actions, NOT the empty stand-down that formerly OutcomeStop-ed the session on the
			// first action-name miss and recorded a vacuous no-proposal:stop with no grounding (TG-552: the TG-464
			// rollback drill 2026-08-27 — a librespeed01 Service-up/down stood down "unknown action
			// check-host-services" without investigating). The unknown action is NEVER dispatched, so fail-closed
			// / INV-08 is preserved (no model token becomes control flow); the loop stays bounded by the same
			// cycle/thrash limits. The hint fires when the miss is a KNOWN TOOL name — the common shape of the slip.
			hint := ""
			if _, isTool := a.Tools.Get(strings.TrimSpace(d.Action)); isTool {
				hint = fmt.Sprintf(" %q is a TOOL, not an action — call it as {\"action\":\"tool\",\"tool\":%q,\"args\":{...}}.", d.Action, d.Action)
			}
			msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
			msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("ACTION_ERROR[%s]: no such action.%s Valid actions: tool, propose, stop.", d.Action, hint)})
			step.Observation, step.Outcome = "ACTION_ERROR (unknown action)", "action-error"
			investigatedThisCycle = true
			break // recover: fall through to the cycle-limit check — the session continues, never aborts
		}

		if cycle >= lim.HandoffPoll {
			// TG-60: at the poll limit the agent has usually GATHERED enough but kept calling tools, so it
			// hands off with NO conclusion. Give it ONE deadline cycle to CONVERGE — decide from its
			// observations (a grounded stop or a proposal) rather than escalate empty. Fires at most once
			// (never grind); if it still doesn't decide next cycle it escalates as before. Safe-direction: a
			// grounded decision beats an empty hand-off, and the citation/confidence gates + mutation-OFF still
			// govern any forced proposal. Deterministic (cycle count, not model text) — INV-08 holds.
			if investigatedThisCycle && !decideNudged {
				decideNudged = true
				res.DecideNudgeFired = true // TG-198: the session was TOLD to decide — record it, it is not derivable downstream
				decideCycle = true          // TG-60: run the next (decision) cycle on the capable DecisionModelName tier
				msgs = append(msgs, model.Message{Role: "user", Content: "DECISION REQUIRED — you have investigated enough; do NOT call another tool. From your OBSERVATION(s), either PROPOSE one conservative reversible action (with evidence_ids citing the observation id(s)) or STOP with a grounded reason (with evidence_ids). This is your final cycle before hand-off."})
				continue
			}
			// still no decision after the nudge — escalate, but with a SYNTHESIZED rationale so the hand-off
			// is never conclusion-less (TG-60 option 2): deterministic, doesn't depend on the model deciding.
			res.Conclusion, res.ConclusionEvidence = synthesizeHandoffConclusion(res.ToolResults)
			res.Outcome, res.Reason = OutcomeEscalate, "handoff poll limit reached"
			return res, nil
		}
	}

	// Hard-halt: same guarantee — record WHAT was investigated so a budget-exhausted hand-off carries context.
	res.Conclusion, res.ConclusionEvidence = synthesizeHandoffConclusion(res.ToolResults)
	res.Outcome, res.Reason = OutcomeHardHalt, "cycle hard-halt limit reached"
	return res, nil
}

// MaxBatchTools bounds how many read-only calls ONE batched directive may carry (TG-49).
//
// WHY FOUR. (a) It is the fan-out the live catalog actually serves: the four point reads a
// confirm-and-decide pass makes against one host (status / eventlog / active-alerts / estate-context —
// the same four TG-215 fast-discloses), or one probe each against the three-to-four hosts of a correlated
// cascade — the N whose sequential round-trips this ticket collapses into one turn. (b) It caps the estate
// amplification a SINGLE model turn can command at four reads — each still individually admitted and
// metered by the recon governor, whose per-session budget (default 25) keeps binding at ~6 full batches,
// well before the 10-cycle hard-halt could compound the product to 40. (c) It is a control-plane constant:
// the model cannot negotiate it, and a directive over the bound is refused with the bound NAMED — never
// partially dispatched, never silently truncated to a prefix the model did not ask for (INV-08).
const MaxBatchTools = 4

// batchToolCall is ONE entry of a batched tool directive (TG-49): the same tool-name + args pair the
// single-tool grammar carries, nested so one directive can ask several INDEPENDENT read-only questions at
// once. Dispatch is still an exact per-entry allowlist lookup — the batch widens how many reads one turn
// may ask for, never what a read is allowed to be (INV-08).
type batchToolCall struct {
	Tool string `json:"tool"`
	Args argMap `json:"args"`
}

// batchEntryStatus is what the serial pre-flight decided for one batch entry. Only batchDispatch entries
// reach the estate; every other status was refused BEFORE dispatch and carries its actionable message.
type batchEntryStatus int

const (
	batchUnknownTool  batchEntryStatus = iota // no such registered tool — the name is never dispatched
	batchBadArgs                              // the ACI arg screen refused the call (poka-yoke, #5)
	batchReconRefused                         // the read-lane volume bound refused this read (TG-165)
	batchDispatch                             // admitted + metered — this entry is invoked
)

// batchEntry is the working state of one batched call: its parsed directive entry, the resolved tool, the
// pre-flight decision, and — for dispatched entries — the invoke result the assembly renders in directive
// order. Each goroutine writes ONLY its own entry's tr/err; everything else is written serially.
type batchEntry struct {
	call    batchToolCall
	tool    Tool
	status  batchEntryStatus
	section string // the pre-composed TOOL_ERROR/TOOL_REFUSED message of a refused entry
	tr      ToolResult
	err     error
}

// runToolBatch executes a batched tool directive (TG-49): N independent read-only calls in ONE cycle,
// dispatched concurrently, observed deterministically. It returns true when the batch decided a SESSION
// stop (write tool withheld / trajectory veto) — res.Outcome/Reason are then already set, exactly as the
// single-tool path would have set them.
//
// THE SINGLE-TOOL CONTRACT, PER ENTRY. Nothing about batching relaxes a check: each entry passes the same
// exact allowlist lookup, the same ReadOnly withhold, the same trajectory record + veto, the same ACI arg
// screen and the same recon Admit — and a refused entry becomes the same actionable TOOL_ERROR /
// TOOL_REFUSED observation text the single path emits, without failing its siblings. A batch of one is
// byte-identical in observable behaviour to the single-tool shape.
//
// FOUR PHASES, AND WHY THE PHASES ARE ORDERED THIS WAY:
//
//  1. SHAPE (serial): ambiguous both-fields, over-bound, and duplicate-entry directives are refused whole,
//     before ANY side effect — a malformed batch never partially runs (INV-08: the grammar has exactly one
//     meaning or nothing runs). A duplicate (same tool + same args twice) is refused rather than dispatched
//     because it would spend two metered estate reads on one answer and hand the trajectory analyzer a
//     phantom repeat the model did not reason its way into.
//  2. WITHHELD-WRITE SCAN (serial): if ANY entry names a registered non-read-only tool the session stops
//     closed before a single sibling is admitted, metered or dispatched — the batch analogue of the single
//     path's pre-dispatch ReadOnly check, run first so a stop can never leave metered-but-undispatched
//     reads for a tool the loop refused on principle. Unreachable today by construction (RegisterFrom
//     refuses mutating tools — the invariant the batch rests on, pinned by its own oracles), kept as
//     defense in depth for the day a registration gate regresses.
//  3. PRE-FLIGHT (serial, directive order): per entry — allowlist lookup, trajectory record + veto, ACI
//     arg screen, recon Admit, recon Record. Admit/Record stay INTERLEAVED per entry and on the loop
//     goroutine, so entry k's Admit sees entries <k already metered (a batch can never pierce the session
//     bound by admitting against a stale count) and the governor is never called concurrently. Record runs
//     at the admit — the entry's dispatch is committed here, and the single path already meters dispatches
//     whose invoke then errors; on the rare trajectory-veto stop mid-batch an earlier committed entry may
//     be metered yet never invoked, which errs the safe direction (the meter over-counts toward refusal,
//     never under-counts an estate touch).
//  4. DISPATCH (concurrent) + ASSEMBLY (serial, directive order): the committed entries run under one
//     errgroup — invoke errors are captured per entry, NEVER group errors, so one failed call cannot
//     cancel or hide its siblings. Tool.Invoke must be goroutine-safe: the live tools are stateless reads
//     over HTTP/SSH clients, and the two that carry per-session budgets (syslogng search, openobserve
//     correlate) hold their own mutex. Results are then screened, recorded and enveloped IN DIRECTIVE
//     ORDER — completion order is deliberately unobservable, so the transcript, ToolResults, Steps and the
//     evidence ids are deterministic for the tracer, the citation gate and the judge.
//
// CYCLE + TRACER ACCOUNTING. The whole batch is ONE cycle. spec/020's AgentCycle holds one tool per step,
// so a batch records N steps SHARING the cycle ordinal (the agent_step schema indexes, not uniques,
// (external_ref, cycle)) — entry 1 reuses the cycle's already-appended step (which carries the directive's
// one thought), entries 2..N append siblings with an empty Thought so the CoT text is stored once.
func (a *Agent) runToolBatch(ctx context.Context, sessionID, raw string, cycle int, d directive, res *Result, msgs *[]model.Message, traj *[]TrajectoryStep) bool {
	first := len(res.Steps) - 1 // the cycle's step, appended by Run before the dispatch switch
	// Parity with the single path's unconditional `step.Tool = d.Tool` stamp: name the batch's FIRST
	// requested call on the cycle step up front, so every early stop/refusal below (shape refusal,
	// withheld write, trajectory veto) still records WHICH call the directive led with — exactly the stop
	// paths an auditor most wants named. The write-scan and the veto overwrite it with the OFFENDING
	// entry's name; Phase 4b overwrites it per entry on the normal path.
	res.Steps[first].Tool = d.Tools[0].Tool

	// refuse rejects the WHOLE batch as one actionable observation — the same recover-don't-abort contract
	// as the single path's TOOL_ERROR (#4): nothing was dispatched, the model re-emits, the cycle limits
	// still bound the session.
	refuse := func(label, msg string) {
		*msgs = append(*msgs, model.Message{Role: "assistant", Content: raw})
		*msgs = append(*msgs, model.Message{Role: "user", Content: msg})
		res.Steps[first].Observation, res.Steps[first].Outcome = "TOOL_ERROR ("+label+")", "tool-error"
	}

	// Phase 1 — SHAPE.
	if d.Tool != "" {
		refuse("ambiguous batch", fmt.Sprintf(`TOOL_ERROR[batch]: ambiguous directive — it carries BOTH "tool" (%q) and "tools" (%d entries). Use "tool"+"args" for one call OR "tools":[{"tool":...,"args":...},...] for a batch, never both. Re-emit exactly one shape.`, d.Tool, len(d.Tools)))
		return false
	}
	if len(d.Tools) > MaxBatchTools {
		refuse("batch over bound", fmt.Sprintf("TOOL_ERROR[batch]: %d tool calls in one directive exceeds the batch bound of %d. Re-emit with at most %d independent calls, or spread the investigation across cycles.", len(d.Tools), MaxBatchTools, MaxBatchTools))
		return false
	}
	seen := make(map[TrajectoryStep]bool, len(d.Tools))
	for _, c := range d.Tools {
		k := TrajectoryStep{Tool: c.Tool, ArgsKey: ArgsKey(c.Args)}
		if seen[k] {
			refuse("duplicate batch call", fmt.Sprintf("TOOL_ERROR[batch]: duplicate call — %q appears more than once with identical args. Each batched call must ask a DIFFERENT question; re-emit without duplicates.", c.Tool))
			return false
		}
		seen[k] = true
	}

	// Phase 2 — WITHHELD-WRITE SCAN.
	for _, c := range d.Tools {
		if t, ok := a.Tools.Get(c.Tool); ok && !t.ReadOnly() {
			res.Steps[first].Tool = c.Tool // the tracer names the OFFENDING call, not the batch's first
			res.Outcome, res.Reason = OutcomeStop, "write tool withheld"
			return true
		}
	}

	// Phase 3 — PRE-FLIGHT.
	entries := make([]batchEntry, len(d.Tools))
	for i, c := range d.Tools {
		entries[i].call = c
		t, ok := a.Tools.Get(c.Tool)
		if !ok {
			entries[i].status = batchUnknownTool
			entries[i].section = fmt.Sprintf("TOOL_ERROR[%s]: no such tool. Available tools: %s", c.Tool, strings.Join(a.Tools.Names(), ", "))
			continue
		}
		entries[i].tool = t
		*traj = append(*traj, TrajectoryStep{Tool: c.Tool, ArgsKey: ArgsKey(c.Args)})
		if veto, reason := TrajectoryVeto(*traj); veto {
			res.Steps[first].Tool = c.Tool // the tracer names the VETOED call, not the batch's first
			res.Outcome, res.Reason = OutcomeStop, "trajectory veto — "+reason
			return true
		}
		if verr := ValidateArgs(t, c.Args); verr != nil {
			entries[i].status = batchBadArgs
			entries[i].section = fmt.Sprintf("TOOL_ERROR[%s]: %s", c.Tool, verr.Error())
			continue
		}
		if a.Recon != nil {
			if rerr := a.Recon.Admit(sessionID); rerr != nil {
				entries[i].status = batchReconRefused
				entries[i].section = fmt.Sprintf("TOOL_REFUSED[%s]: %s", c.Tool, rerr.Error())
				res.ReconRefusals++
				if res.ReconRefusalReason == "" {
					res.ReconRefusalReason = rerr.Error()
				}
				continue
			}
			a.Recon.Record(sessionID, c.Tool, TargetOf(c.Args))
		}
		entries[i].status = batchDispatch
	}

	// Phase 4a — CONCURRENT DISPATCH. Plain errgroup.Group, deliberately not WithContext: an invoke error
	// is this entry's observation, never a reason to cancel a sibling mid-read.
	var g errgroup.Group
	for i := range entries {
		if entries[i].status != batchDispatch {
			continue
		}
		e := &entries[i]
		g.Go(func() error {
			e.tr, e.err = e.tool.Invoke(ctx, e.call.Args)
			return nil
		})
	}
	_ = g.Wait() // always nil — errors live on the entries

	// Phase 4b — ASSEMBLY, in directive order. Steps are addressed by INDEX, never by a held pointer,
	// because each append may reallocate the slice.
	sections := make([]string, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		idx := first
		if i > 0 {
			res.Steps = append(res.Steps, AgentCycle{Cycle: cycle, Action: "tool"})
			idx = len(res.Steps) - 1
		}
		res.Steps[idx].Tool = e.call.Tool
		switch {
		case e.status == batchUnknownTool:
			sections = append(sections, e.section)
			res.Steps[idx].Observation, res.Steps[idx].Outcome = "TOOL_ERROR (unknown tool)", "tool-error"
		case e.status == batchBadArgs:
			sections = append(sections, e.section)
			res.Steps[idx].Observation, res.Steps[idx].Outcome = "TOOL_ERROR (arg validation)", "tool-error"
		case e.status == batchReconRefused:
			sections = append(sections, e.section)
			res.Steps[idx].Observation, res.Steps[idx].Outcome = "TOOL_REFUSED (recon budget)", "recon-refused"
		case e.err != nil:
			// #6 recoverable tool-error, batch form: this entry's Go-error becomes its observation; the
			// siblings' results stand. Not appended to ToolResults — same as the single path.
			sections = append(sections, fmt.Sprintf("TOOL_ERROR[%s]: %s", e.call.Tool, e.err.Error()))
			res.Steps[idx].Observation, res.Steps[idx].Outcome = "TOOL_ERROR", "tool-error"
		default:
			tr := e.tr
			if tr.Target == "" {
				tr.Target = TargetOf(e.call.Args) // TG-166: the target rests on the ARGUMENTS, never Output
			}
			res.ToolResults = append(res.ToolResults, tr)
			res.Steps[idx].Observation, res.Steps[idx].Outcome = "observed "+tr.ID, "investigate"
			if !tr.Success {
				res.Steps[idx].Observation = "observed " + tr.ID + " (tool call FAILED)" // TG-199
			}
			// The SAME screen every observation passes (REQ-1012), per result — a batch is N injection
			// surfaces, each defanged independently before any of them re-enters the prompt.
			screened, notes := screenToolOutput(tr.ID, tr.Output)
			res.ScreenNotes = append(res.ScreenNotes, notes...)
			res.Steps[idx].EvidenceID, res.Steps[idx].Evidence = tr.ID, screened
			sections = append(sections, observationEnvelope(e.call.Tool, tr, screened))
		}
	}
	// ONE assistant echo + ONE combined user observation per cycle, keeping the transcript strictly
	// alternating for every gateway model. Sections are joined in directive order; a batch of one renders
	// byte-identically to the single-tool path's message.
	*msgs = append(*msgs, model.Message{Role: "assistant", Content: raw})
	*msgs = append(*msgs, model.Message{Role: "user", Content: strings.Join(sections, "\n\n")})
	return false
}

// observationEnvelope renders ONE captured tool result for the model: the ORCHESTRATOR's verdict on whether
// that call actually SUCCEEDED, on its own line, followed by the OBSERVATION envelope itself.
//
// WHY (TG-199, A2/A7 — a proposal could be built on a FAILED observation). ToolResult.Success was captured
// and honoured downstream — temporal/runner.buildEvidence / actuateEvidence set Successful from it, and
// core/actuate refuses a mutating action with no BOUND evidence — but it was never SHOWN to the model. So a
// failed lookup re-entered the prompt as a bare OBSERVATION[<id>] carrying text that reads exactly like a
// fact: get-tracker-history returns this shape LIVE (modules/tracker/trackerhistory: an unreadable corpus is
// Success=false with prose the model cannot distinguish from "no prior incidents"), and so do the
// host-unreachable / empty-result sentinels of the other read tools. The in-loop citation gate checks id
// MEMBERSHIP only (citesGatheredEvidence), so it passed; the proposal was built on a lookup that never
// landed; and the evidence gate refused it later with no feedback reaching the model, burning the session.
// The model could not tell a failed call from a successful one because nothing in its input said so.
//
// THE VERDICT LEADS THE PAYLOAD, DELIBERATELY. The TOOL_OUTCOME line is orchestrator-authored; the
// observation beneath it is attacker-influenceable data (INV-08, the same trust boundary screenToolOutput
// exists for), so the trusted statement is never placed downstream of the untrusted text that would like to
// contradict it — the same trusted-preamble-first discipline the seed composer uses (design-wisdom #4).
//
// THE ID IS UNTOUCHED. `OBSERVATION[<id>]: <data>` stays byte-for-byte what it was (REQ-1012): the id is the
// anchor the citation gate, INV-11 evidence-binding and the console's ground-truth citation all resolve
// against, which is exactly why screenToolOutput screens only the payload and never the envelope.
//
// DATA, NOT A GATE. A FAILED observation is still captured, still cited-able, still counted as a cycle and
// still recorded as a TrajectoryStep — so the failure mode this text creates (a model that reads "FAILED"
// and retries) is bounded by the SAME cycle and trajectory limits as any other cycle (REQ-1004/REQ-1010),
// not by anything new. Nothing rendered here becomes control flow (INV-08).
func observationEnvelope(tool string, tr ToolResult, screened string) string {
	if tool == "" {
		tool = tr.Tool
	}
	if tr.Success {
		return fmt.Sprintf("TOOL_OUTCOME[%s]: SUCCEEDED — the %s call returned; the OBSERVATION below is a real reading you may cite.\nOBSERVATION[%s]: %s", tr.ID, tool, tr.ID, screened)
	}
	return fmt.Sprintf("TOOL_OUTCOME[%s]: FAILED — the %s call did NOT succeed. What follows is a failure message, not a reading of the estate: it may read like a real answer, but it proves nothing, so do NOT cite %s as evidence in a proposal or a stop. Fix the call or try a DIFFERENT tool; if it keeps failing, say so and stop — this attempt already spent one of your limited cycles.\nOBSERVATION[%s]: %s", tr.ID, tool, tr.ID, tr.ID, screened)
}

// screenToolOutput runs the input screen over ONE live tool RESULT before it re-enters the model prompt as
// an OBSERVATION — the tool-result analogue of temporal/runner.screenSeedBlock, so the loop's two screening
// sites (the seed on the way IN, the tool results that re-enter the loop) share one discipline. Clean output
// passes through byte-identical (screen.Scrub allocates nothing on the no-detection path). A detection
// NEUTRALIZES the payload in place — screen.Scrub defangs each injection span with its [SCREENED:<category>]
// marker over the normalized fold (so a homoglyph / zero-width disguise cannot survive) AND redacts any
// leaked secret to a [REDACTED:<kind>] marker — and returns an `input-screened:tool-result[<id>]:<categories>`
// note the caller records on Result.ScreenNotes. The observation is NEVER dropped: an attacker must not be
// able to suppress triage by embedding an injection string in a tool result (under-triage is the worse
// failure), and a screened result is data hygiene — deliberately NOT a POLL_PAUSE/stop signal (INV-08). The
// id is the evidence anchor and is passed through untouched; only the payload is screened.
func screenToolOutput(id, output string) (string, []string) {
	clean, hits := screen.Scrub(output)
	if len(hits) == 0 {
		return output, nil
	}
	return clean, []string{"input-screened:tool-result[" + id + "]:" + screenCategories(hits)}
}

// screenCategories joins the distinct categories of a detection set in Scrub's stable order — the compact
// per-result tag the ScreenNotes entry carries (mirrors temporal/runner.screenCategories).
func screenCategories(ms []screen.Match) string {
	seen := make(map[screen.Category]bool, len(ms))
	var out []string
	for _, m := range ms {
		if !seen[m.Category] {
			seen[m.Category] = true
			out = append(out, string(m.Category))
		}
	}
	return strings.Join(out, ",")
}

// synthesizeHandoffConclusion builds a deterministic, honest hand-off rationale from the observations the
// agent gathered when it reached a cycle limit WITHOUT deciding (TG-60). It records HOW MANY observations
// were investigated and that no grounded decision was reached — giving the operator + judge the context an
// empty hand-off never did — and cites the distinct real observation ids. Pure: no model token becomes the
// text (INV-08); the count/ids come from the agent's OWN captured evidence.
func synthesizeHandoffConclusion(results []ToolResult) (string, []string) {
	seen := map[string]bool{}
	var ids []string
	for _, tr := range results {
		if tr.ID != "" && !seen[tr.ID] {
			seen[tr.ID] = true
			ids = append(ids, tr.ID)
		}
	}
	if len(ids) == 0 {
		return "Reached the investigation budget without a gathered observation or a grounded decision; escalated for human review.", nil
	}
	return fmt.Sprintf("Investigated %d observation(s) but reached the cycle budget without a grounded stop or safe reversible proposal; escalated for human decision.", len(ids)), ids
}

// clipConclusion bounds the model's stop rationale before it enters the session record — untrusted DATA,
// size-capped so a runaway generation cannot bloat the record (it is never parsed or executed).
func clipConclusion(s string) string {
	s = strings.TrimSpace(s)
	const maxConclusion = 500
	if len(s) > maxConclusion {
		return s[:maxConclusion] + "…"
	}
	return s
}

// clipThought bounds the model's optional ReAct/CoT reasoning trace before it enters the session record
// (design-wisdom #7) — untrusted DATA, size-capped so a runaway generation cannot bloat the record. It
// is only recorded for audit/forensics: never parsed, never executed, never a dispatch input (INV-08).
func clipThought(s string) string {
	s = strings.TrimSpace(s)
	const maxThought = 1000
	if len(s) > maxThought {
		return s[:maxThought] + "…"
	}
	return s
}

// gatheredOnly filters the model's cited ids down to observations the agent ACTUALLY captured — a
// fabricated id is dropped, never stored (INV-08/INV-11: the record only ever references real evidence).
func gatheredOnly(cited []string, gathered []ToolResult) []string {
	real := make(map[string]bool, len(gathered))
	for _, tr := range gathered {
		real[tr.ID] = true
	}
	var out []string
	for _, id := range cited {
		// Trim like citesGatheredEvidence does — the two must agree, or an id that PASSES the gate
		// ("tr-1 " with a stray space) would be silently dropped here, recording a stop that claims
		// grounding with an empty evidence list.
		if t := strings.TrimSpace(id); real[t] {
			out = append(out, t)
		}
	}
	return out
}

// citesGatheredEvidence reports whether the proposal's evidence_ids cite at least one observation the agent
// ACTUALLY captured — the grounding check behind the citation gate. Citing nothing, or citing only ids the
// agent never gathered (a fabricated citation), is not grounding and returns false.
func citesGatheredEvidence(cited []string, gathered []ToolResult) bool {
	if len(cited) == 0 || len(gathered) == 0 {
		return false
	}
	ids := make(map[string]struct{}, len(gathered))
	for _, tr := range gathered {
		ids[tr.ID] = struct{}{}
	}
	for _, c := range cited {
		if _, ok := ids[strings.TrimSpace(c)]; ok {
			return true
		}
	}
	return false
}

// firstToolID returns the id of the first captured observation, to name a concrete example in the re-prompt.
func firstToolID(gathered []ToolResult) string {
	if len(gathered) > 0 {
		return gathered[0].ID
	}
	return ""
}

// gatheredIDs is the set of observation ids the ORCHESTRATOR actually captured — the authority a citation
// is checked against. It is built from res.ToolResults rather than from anything the model said, which is
// the whole property: the model cannot author the set its own citations are validated against.
func gatheredIDs(gathered []ToolResult) map[string]struct{} {
	ids := make(map[string]struct{}, len(gathered))
	for _, tr := range gathered {
		ids[tr.ID] = struct{}{}
	}
	return ids
}
