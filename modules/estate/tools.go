// Package estatetools exposes the estate causal graph to the agent as a READ-ONLY investigation tool. The
// competence skills tell the agent to discriminate an isolated fault from a cascade by probing the alerting
// host's RELATED hosts — but get-active-alerts probes one named host at a time, and until this tool the agent
// had no way to NAME them (the CMDB record carries attributes, not topology). get-estate-context closes that:
// it answers "what does this host depend on, who depends on it, and who shares its infrastructure parent"
// from the multi-source graph TG already builds — a pure in-memory query, no I/O, no credentials, and no
// model token becomes control flow (the host arg is resolved by exact canonical-name lookup, never executed).
package estatetools

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/estate"
)

// listCap bounds each section of the context block: enough neighbors to reason with, small enough that a
// densely-connected core switch cannot flood the prompt.
const listCap = 8

// New returns the estate-context tool bound to a live graph provider (the worker passes estateHolder.Graph,
// so every invocation sees the freshest refresh — never a boot-time snapshot).
func New(provider func() *estate.Graph) []agent.Tool {
	return []agent.Tool{contextTool{provider: provider}}
}

type contextTool struct {
	provider func() *estate.Graph
}

func (contextTool) Name() string   { return "get-estate-context" }
func (contextTool) ReadOnly() bool { return true }

func (t contextTool) Invoke(_ context.Context, args map[string]string) (agent.ToolResult, error) {
	host := hostArg(args)
	res := agent.ToolResult{ID: "estate-ctx-" + sanitizeID(host), Tool: t.Name()}
	if host == "" {
		res.Output = "no host given — call with {\"host\": \"<name>\"}"
		return res, nil
	}
	g := t.provider()
	if g == nil || g.Len() == 0 {
		res.Output = "estate graph is empty (topology sources not seeded yet) — fall back to the CMDB record and escalate if the cascade question matters"
		return res, nil
	}
	ent, ok := g.Resolve(host)
	if !ok {
		// %q — the unresolved name is MODEL-CHOSEN text; quoting keeps a hostile arg (newlines, fake
		// section headers) visibly inert inside the observation instead of forging structure (INV-08).
		res.Output = fmt.Sprintf("%q is not in the estate graph (%d edges known) — fall back to the CMDB record; do not guess its topology", host, g.Len())
		return res, nil
	}

	var sb strings.Builder
	// ent.Name is graph-sourced (trusted); the raw model-chosen host arg is never echoed unquoted.
	fmt.Fprintf(&sb, "estate context for %s %q:", ent.Type, ent.Name)

	parents := g.Parents(ent)
	sb.WriteString("\nUPSTREAM (what it depends on — probe the infrastructure entries with get-active-alerts when you suspect a shared cause):")
	if len(parents) == 0 {
		sb.WriteString("\n  (none known)")
	}
	for i, p := range parents {
		if i == listCap {
			fmt.Fprintf(&sb, "\n  … %d more", len(parents)-listCap)
			break
		}
		note := ""
		if p.Rel == estate.RelMemberOf {
			note = " — a grouping, not a probeable host"
		}
		fmt.Fprintf(&sb, "\n  - %s %s via %s (confidence %.2f)%s", p.Entity.Type, p.Entity.Name, p.Rel, p.Confidence, note)
	}

	deps := g.BlastRadius(ent, 3)
	fmt.Fprintf(&sb, "\nDEPENDENTS (blast radius if %s fails, depth<=3): %d entit%s", ent.Name, len(deps), plural(len(deps)))
	for i, d := range deps {
		if i == listCap {
			fmt.Fprintf(&sb, "\n  … %d more", len(deps)-listCap)
			break
		}
		fmt.Fprintf(&sb, "\n  - %s %s (confidence %.2f, distance %d)", d.Entity.Type, d.Entity.Name, d.Confidence, d.Distance)
	}

	sibs := g.Siblings(ent)
	sb.WriteString("\nCOMMON-CAUSE SIBLINGS (share an infrastructure parent — if several also alert, suspect that parent even if it is silent):")
	if len(sibs) == 0 {
		sb.WriteString("\n  (none known)")
	}
	for i, s := range sibs {
		if i == listCap {
			fmt.Fprintf(&sb, "\n  … %d more", len(sibs)-listCap)
			break
		}
		fmt.Fprintf(&sb, "\n  - %s %s (confidence %.2f)", s.Entity.Type, s.Entity.Name, s.Confidence)
	}

	res.Success = true
	res.Output = sb.String()
	return res, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// hostArg mirrors the LibreNMS tools' argument convention so the agent can use the same shape everywhere.
func hostArg(args map[string]string) string {
	for _, k := range []string{"host", "target", "device", "hostname"} {
		if v := strings.TrimSpace(args[k]); v != "" {
			return v
		}
	}
	return ""
}

// sanitizeID keeps the observation id printable and stable for the citation gate (lowercased, spaces and
// unexpected runes collapsed to '-').
func sanitizeID(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}
