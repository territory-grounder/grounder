package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ToolResult is the orchestrator-captured result of a read-only tool call. It is DATA for the model,
// and its ID is what later evidence-binding checks (INV-11, silent_cognition_guard) cite. The
// orchestrator captures it — it is never trusted agent free-text.
type ToolResult struct {
	ID      string
	Tool    string
	Output  string
	Success bool
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
}

// NewReadOnlyToolSet returns an empty tool set that refuses to register a mutating tool.
func NewReadOnlyToolSet() *ToolSet { return &ToolSet{tools: map[string]Tool{}} }

// Register adds a tool. A non-read-only tool is refused (Phase 0/1 read-only guarantee).
func (s *ToolSet) Register(t Tool) error {
	if !t.ReadOnly() {
		return ErrWriteToolWithheld
	}
	s.tools[t.Name()] = t
	return nil
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
		t := s.tools[name]
		b.WriteString("- ")
		b.WriteString(name)
		at, ok := t.(ACITool)
		if !ok {
			b.WriteByte('\n')
			continue
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
	return strings.TrimRight(b.String(), "\n")
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
