package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/execclass"
)

// ToolResult is the orchestrator-captured result of a read-only tool call. It is DATA for the model,
// and its ID is what later evidence-binding checks (INV-11, silent_cognition_guard) cite. The
// orchestrator captures it — it is never trusted agent free-text.
type ToolResult struct {
	ID      string
	Tool    string
	Output  string
	Success bool
	// Target is the estate object this call was made AGAINST, taken from the invocation arguments — not
	// parsed out of Output. Empty when the tool takes no target (an estate-wide read).
	//
	// ★ WHY IT EXISTS (TG-166). Target relevance used to be decided by scanning Output for the incident
	// host as a SUBSTRING: `host == "" || strings.Contains(lower(tr.Output), lower(host))`. Two ways that
	// is wrong, and the actuation gate rested on it:
	//
	//   1. An estate-wide read that merely MENTIONS the host — an alert list, a neighbour's log line
	//      naming it — scored as target-relevant. Evidence about the fleet became evidence about the box.
	//   2. `host == ""` was an unconditional PASS. An incident with no resolved host marked EVERY cited
	//      observation relevant, which is the opposite of what an absent target means.
	//
	// Recording the target at the call site makes relevance an equality check on a fact the orchestrator
	// KNOWS, instead of an inference from text the target itself produced. It is set by the loop from the
	// tool's own arguments (agent.TargetOf), never by the model and never from Output — Output is
	// attacker-influenceable and must not decide whether a mutating action may proceed.
	Target string
}

// TargetOf extracts the estate object a call is aimed at from its arguments, using the same key set the
// tool modules already accept ("host", "target", "device", "hostname" — see modules/*/tools.go hostArg).
// One implementation so the orchestrator's idea of a call's target cannot drift from the tools' own.
//
// An empty result means "this call names no target", which is a fact, not a failure: estate-wide reads
// are legitimate evidence — they are simply not evidence ABOUT a particular host.
func TargetOf(args map[string]string) string {
	for _, k := range []string{"host", "target", "device", "hostname"} {
		if v := strings.TrimSpace(args[k]); v != "" {
			return v
		}
	}
	return ""
}

// Tool is a capability the agent may call. In Phase 0/1 every registered tool is read-only
// (get/describe/logs class); ReadOnly() must return true or registration is refused.
type Tool interface {
	Name() string
	ReadOnly() bool
	Invoke(ctx context.Context, args map[string]string) (ToolResult, error)
}

// ParamSpec is the typed schema of ONE argument a tool accepts — the Agent-Computer-Interface (ACI)
// contract that lets the model call a tool "from its description and parameters alone" (Anthropic,
// Writing Effective Tools). It is rendered into the preamble as prompt DATA and is the poka-yoke the
// loop validates a call against (a missing Required arg, or a value outside a non-empty Enum, is
// refused with an actionable message). No field here EVER becomes control flow (INV-08): the schema
// steers and screens the model's args; it is never a dispatch key.
type ParamSpec struct {
	Name        string   // the argument key the tool reads from args
	Type        string   // a human-facing type hint rendered to the model (e.g. "string", "host")
	Required    bool     // WHEN true, an absent/blank value is an actionable error, not a silent pass
	Enum        []string // WHEN non-empty, the value MUST be one of these (else an actionable error)
	Example     string   // a concrete example value that steers the model toward a valid call
	Description string   // one line: what this argument selects
}

// ACITool is the OPTIONAL ACI extension of Tool: a tool that ALSO publishes a human-facing Description
// and a typed parameter schema. The catalog renderer surfaces both into the preamble and the loop
// validates args against Params(). A plain Tool that does not implement it still works — it is listed
// by name only and has no arg schema to validate — so existing read-only tools need NO change; adopting
// the schema per tool is a follow-on port. Neither method's output becomes control flow (INV-08): the
// description/params are prompt DATA and a validation gate, never a dispatch decision.
type ACITool interface {
	Tool
	Description() string
	Params() []ParamSpec
}

// ValidateArgs screens a tool call's args against the tool's declared ParamSpec schema (poka-yoke): a
// Required parameter that is absent or blank, or a value outside a declared Enum, is refused with a
// SINGLE actionable message the model can act on (Writing Effective Tools). A tool that publishes no
// schema (not an ACITool) has nothing to validate, so its args pass unchanged (backward compatible).
// This is a deterministic function of the declared schema and the captured args — no model token
// becomes control flow (INV-08); the loop turns a refusal into a TOOL_ERROR observation rather than
// executing the bad call against the estate.
func ValidateArgs(t Tool, args map[string]string) error {
	at, ok := t.(ACITool)
	if !ok {
		return nil
	}
	for _, p := range at.Params() {
		v, present := args[p.Name]
		if p.Required && (!present || strings.TrimSpace(v) == "") {
			if p.Example != "" {
				return fmt.Errorf("missing required arg %q (%s) — %s; e.g. %q", p.Name, p.Type, p.Description, p.Example)
			}
			return fmt.Errorf("missing required arg %q (%s) — %s", p.Name, p.Type, p.Description)
		}
		if present && len(p.Enum) > 0 && !containsStr(p.Enum, v) {
			return fmt.Errorf("arg %q=%q is not one of the allowed values [%s]", p.Name, v, strings.Join(p.Enum, ", "))
		}
	}
	return nil
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ErrWriteToolWithheld is returned when a non-read-only tool is registered while mutation is off. Write
// tools are structurally absent from the Phase-0/1 agent's tool set (INV-08, least-autonomous topology).
var ErrWriteToolWithheld = errors.New("agent: write/mutating tools are structurally withheld while mutation is off (INV-08)")

// ToolSet is the agent's registered, allowlisted tools. A tool is dispatched only by an exact,
// validated name lookup here — never by executing model text — so no model token becomes control flow.
type ToolSet struct {
	tools map[string]Tool
	// sources maps a registered tool name to its SOURCE NAMESPACE — the coarse plane the composition
	// root declared at registration ("librenms", "host", "estate", "history") — so the class-keyed
	// catalog (TG-215) can GROUP its rendering by source. Rendering metadata ONLY: dispatch, validation
	// and the read-only guarantee never read it, and a tool keeps its exact registered name — the
	// directive grammar accepts the same names byte-for-byte whether or not a source was declared.
	sources map[string]string
}

// NewReadOnlyToolSet returns an empty tool set that refuses to register a mutating tool.
func NewReadOnlyToolSet() *ToolSet {
	return &ToolSet{tools: map[string]Tool{}, sources: map[string]string{}}
}

// Register adds a tool. A non-read-only tool is refused (Phase 0/1 read-only guarantee).
func (s *ToolSet) Register(t Tool) error { return s.RegisterFrom("", t) }

// RegisterFrom adds a tool under a declared SOURCE NAMESPACE (TG-215). The source is a short rendering
// label the composition root supplies because it — not this package — knows which module a tool came
// from; an empty source is legal and groups the tool under "other" in a namespaced render. The tool's
// NAME is untouched: source is display grouping for the class-keyed catalog, never part of the dispatch
// key, so every existing directive keeps resolving byte-for-byte.
func (s *ToolSet) RegisterFrom(source string, t Tool) error {
	if !t.ReadOnly() {
		return ErrWriteToolWithheld
	}
	s.tools[t.Name()] = t
	if source != "" {
		s.sources[t.Name()] = source
	}
	return nil
}

// SubsetFor returns a NEW read-only ToolSet holding only the named tools, preserving their registration
// sources, plus the names that did not resolve (reported, never silently dropped — a pack that names a
// missing tool degrades visibly, the modules ErrNoExecutionPath discipline). The subset is built through
// RegisterFrom, so the ErrWriteToolWithheld refusal runs on every member: a subset can never smuggle a
// mutating tool past the guarantee its parent enforced — "read verbs are the allowlisted default" is a
// property of the type, not a rule an author must remember. A nil receiver subsets the empty set.
func (s *ToolSet) SubsetFor(names []string) (*ToolSet, []string) {
	out := NewReadOnlyToolSet()
	var missing []string
	if s == nil {
		return out, append(missing, names...)
	}
	for _, n := range names {
		t, ok := s.tools[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		if err := out.RegisterFrom(s.sources[n], t); err != nil {
			missing = append(missing, n)
		}
	}
	return out, missing
}

// Get looks up a tool by exact name. A miss returns ok=false; the caller must fail closed rather than
// execute the unknown name.
func (s *ToolSet) Get(name string) (Tool, bool) {
	t, ok := s.tools[name]
	return t, ok
}

// Names returns the registered tool names, sorted.
func (s *ToolSet) Names() []string {
	out := make([]string, 0, len(s.tools))
	for n := range s.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Catalog renders the registered read-only tools as a STRUCTURED catalog for the preamble: each tool's
// name, its one-line description, and its typed parameters (name, type, required/optional, enum,
// example) so the model can call it "from its description and parameters alone" (Writing Effective
// Tools) — this replaces the bare comma-joined name list, which told the model a tool EXISTED but not
// HOW to call it (the ACI gap, design-wisdom #5). A tool that publishes no ACI schema is listed by name
// only (backward compatible). Pure DATA — nothing rendered here becomes control flow (INV-08); dispatch
// is still an exact Get(name) lookup. A nil/empty set renders "" so the caller can fall back to its
// no-tools guidance.
func (s *ToolSet) Catalog() string {
	if s == nil || len(s.tools) == 0 {
		return ""
	}
	var b strings.Builder
	for _, name := range s.Names() {
		writeCatalogEntry(&b, name, s.tools[name])
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeCatalogEntry renders ONE tool's FULL catalog entry — name, description, typed params — exactly as
// Catalog() always has. Factored out (TG-215) so the flat catalog and the class-keyed catalog render a
// fully-disclosed tool from the SAME bytes: the reachable-class byte-identity golden pins this shape.
func writeCatalogEntry(b *strings.Builder, name string, t Tool) {
	b.WriteString("- ")
	b.WriteString(name)
	at, ok := t.(ACITool)
	if !ok {
		b.WriteByte('\n')
		return
	}
	if d := strings.TrimSpace(at.Description()); d != "" {
		b.WriteString(": ")
		b.WriteString(d)
	}
	b.WriteByte('\n')
	for _, p := range at.Params() {
		b.WriteString("    - ")
		b.WriteString(p.Name)
		b.WriteString(" (")
		b.WriteString(p.Type)
		if p.Required {
			b.WriteString(", required")
		} else {
			b.WriteString(", optional")
		}
		b.WriteByte(')')
		if d := strings.TrimSpace(p.Description); d != "" {
			b.WriteString(" — ")
			b.WriteString(d)
		}
		if len(p.Enum) > 0 {
			b.WriteString(" [one of: ")
			b.WriteString(strings.Join(p.Enum, ", "))
			b.WriteByte(']')
		}
		if p.Example != "" {
			b.WriteString(" e.g. ")
			b.WriteString(p.Example)
		}
		b.WriteByte('\n')
	}
}

// fastDisclosed is the CLOSED set of tools whose FULL schema a FAST_AGENT preamble discloses (TG-215) —
// the point reads a recurrent, isolated, known-pattern incident (execclass.FastAgent's own definition)
// actually starts from. Everything else renders as a one-line INDEX entry: still listed, still callable,
// schema undisclosed. Keyed by exact registered name.
//
// IN — the fast confirm-and-decide reads:
//   - get-device-status: the first move on a recurrence — is the device up, when was it last polled; the
//     fast class exists for exactly this confirm-then-decide shape.
//   - get-device-eventlog: the recurrence timeline — what the poller observed changing (a reboot, a flap)
//     confirms or refutes a known pattern without any deep log excavation.
//   - get-active-alerts: the isolation check — FAST_AGENT is only ever decided for an ISOLATED incident
//     (execclass.Classify), and this is the cheap live test that the isolation still holds.
//   - get-estate-context: the lean seed (TG-42) omits the <estate> pre-fetch block for this class; the
//     TOOL is the remaining door to the causal neighbourhood, so thinning its schema here would narrow
//     BOTH doors at once — exactly what TG-42's lean-compose rationale promised not to do.
//
// OUT — index-listed (a disclosure reduction, NEVER a capability removal — see CatalogFor):
//   - get-host-logs / search-host-logs: raw device-log excavation diagnoses an UNKNOWN fault; a
//     known-pattern recurrence starts from its prior, not from paging syslog.
//   - check-host-disk / -memory / -services / -load: SSH root-causing breadth for a novel fault; the
//     fast class carries a verified prior — these stay one call away when the "known" pattern surprises.
//   - correlate-logs: cross-host cascade correlation; a correlated incident routes DEEP_INVESTIGATION by
//     construction (execclass.Classify), so it is never the fast path's opening move.
//   - get-incident-history / get-tracker-history: precedent recall; the class is only decided when a
//     high-confidence prior already informed the routing (execclass.Input.KnownPattern), so recall here
//     is confirmatory, not primary.
//   - get-actor-evidence: actor attribution for an ambiguous or suspicious change; ambiguity routes
//     HUMAN_LED, never FAST_AGENT.
//
// A name missing from this set fails SAFE: a new or renamed tool renders as an index line — reduced
// disclosure, never lost capability — and every non-FAST class ignores this set entirely.
var fastDisclosed = map[string]bool{
	"get-device-status":   true,
	"get-device-eventlog": true,
	"get-active-alerts":   true,
	"get-estate-context":  true,
}

// fullyDisclosed is the pure, deterministic (execclass, tool) → disclosure selection (TG-215) — the same
// selector pattern the skill registry uses (agent/skills.AppliesWhen): typed signals in, a rendering
// decision out, no model token anywhere near it (INV-08). Only an affirmative FAST_AGENT classification
// earns the reduced disclosure; every other value — DEEP_INVESTIGATION, STANDARD_AGENT, HUMAN_LED, the
// empty class of an unthreaded caller, or outright garbage — discloses everything (fail toward MORE
// context, the classifier's own fail-UP rule kept at its consumer).
func fullyDisclosed(c execclass.Class, name string) bool {
	if c != execclass.FastAgent {
		return true
	}
	return fastDisclosed[name]
}

// indexNote is appended to a class-keyed catalog that index-listed at least one tool. It exists because
// the reduced listing must never read as a reduced CAPABILITY: the model is told, in words, that an
// index entry is callable exactly like a fully-disclosed tool.
const indexNote = "Entries above WITHOUT a parameter list are INDEX entries: their parameter schemas are omitted to keep this preamble compact, but each is a real, callable tool — invoke it by its EXACT name like any other, and a call with missing or invalid parameters is refused with a TOOL_ERROR naming what to fix."

// CatalogFor renders the read-only tool catalog DISCLOSED FOR an execution class (TG-215). Every class
// except FAST_AGENT returns Catalog() — the flat, fully-disclosed render, byte-identical to what every
// class received before this existed (the reachable-class goldens pin that). FAST_AGENT gets the
// namespaced progressive-disclosure render: tools grouped under their registration source ("librenms:",
// "host:", …), the fastDisclosed point reads carrying their full schema entry, and every other tool a
// one-line "name — purpose" index entry derived from its own Description (no parallel prose to rot).
//
// A DISCLOSURE REDUCTION, NOT A CAPABILITY REMOVAL. Dispatch is still an exact Get(name) lookup over the
// FULL registered set: the loop accepts a directive naming an index-listed tool exactly as it accepts a
// fully-disclosed one (proven by TestIndexOnlyListedToolStillExecutes), ValidateArgs still screens its
// args against the tool's real schema, and an UNKNOWN name is still refused (INV-08 unchanged — the
// preamble is prompt DATA; nothing rendered here is control flow).
func (s *ToolSet) CatalogFor(class execclass.Class) string {
	if s == nil || len(s.tools) == 0 {
		return ""
	}
	if class != execclass.FastAgent {
		return s.Catalog()
	}
	// Group by source namespace, sources sorted, tool names sorted within each (all deterministic).
	bySource := map[string][]string{}
	for _, name := range s.Names() {
		src := s.sources[name]
		if src == "" {
			src = "other"
		}
		bySource[src] = append(bySource[src], name)
	}
	srcs := make([]string, 0, len(bySource))
	for src := range bySource {
		srcs = append(srcs, src)
	}
	sort.Strings(srcs)
	var b strings.Builder
	indexed := false
	for _, src := range srcs {
		b.WriteString(src)
		b.WriteString(":\n")
		for _, name := range bySource[src] {
			if fullyDisclosed(class, name) {
				writeCatalogEntry(&b, name, s.tools[name])
				continue
			}
			indexed = true
			writeIndexEntry(&b, name, s.tools[name])
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if indexed {
		out += "\n\n" + indexNote
	}
	return out
}

// writeIndexEntry renders ONE tool's one-line INDEX entry: "- <name> — <one-line purpose>", the purpose
// derived from the tool's own Description (oneLineSummary) so the index can never drift from the schema
// it summarizes. A tool with no ACI description is listed by bare name — the same degradation the full
// catalog has always had.
func writeIndexEntry(b *strings.Builder, name string, t Tool) {
	b.WriteString("- ")
	b.WriteString(name)
	if at, ok := t.(ACITool); ok {
		if sum := oneLineSummary(at.Description()); sum != "" {
			b.WriteString(" — ")
			b.WriteString(sum)
		}
	}
	b.WriteByte('\n')
}

// oneLineSummary reduces a tool's Description to its head clause for an index entry: the text before the
// first ": ", " — ", ". " or newline — which for every live tool description is the "what this answers"
// clause — trimmed of trailing punctuation and rune-safely capped. Pure text derivation from the tool's
// own words: there is no hand-maintained summary table to go stale.
func oneLineSummary(desc string) string {
	s := strings.TrimSpace(desc)
	cut := len(s)
	for _, sep := range []string{": ", " — ", ". ", "\n"} {
		if i := strings.Index(s, sep); i >= 0 && i < cut {
			cut = i
		}
	}
	s = strings.TrimRight(strings.TrimSpace(s[:cut]), ".:")
	const maxSummary = 140
	if r := []rune(s); len(r) > maxSummary {
		s = strings.TrimRight(string(r[:maxSummary]), " ") + "…"
	}
	return s
}

// AllReadOnly reports whether every registered tool is read-only (always true for a ToolSet built via
// NewReadOnlyToolSet; a defensive check for the oracle).
func (s *ToolSet) AllReadOnly() bool {
	for _, t := range s.tools {
		if !t.ReadOnly() {
			return false
		}
	}
	return true
}
