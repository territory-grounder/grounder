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

// Description and Params publish the ACI schema (agent.ACITool) — rendered into the tool preamble as DATA,
// and the schema the loop screens a call against before it runs. ADOPTED IN TG-197: this tool shipped with
// neither, so the catalog listed a bare "- get-estate-context" and the model had to GUESS that it takes a
// `host` — inside a 5-cycle poll budget against a 5.4-step live mean, a guessed argument name costs a cycle
// the investigation does not have. Neither method's output becomes control flow (INV-08): dispatch is still
// an exact name lookup, and the host arg is still resolved by canonical-name lookup, never executed.
func (contextTool) Description() string {
	return "Read a host's place in the estate's causal graph: what it DEPENDS ON (upstream), what depends on " +
		"it (blast radius), and which hosts share an infrastructure parent with it (common-cause siblings). Use " +
		"it to tell an isolated fault from a cascade — it NAMES the related hosts to probe, which nothing else " +
		"in the tool set can. Topology only, from an in-memory graph: it reports no live health, so confirm a " +
		"named neighbour with get-active-alerts before concluding a shared cause."
}

func (contextTool) Params() []agent.ParamSpec {
	return []agent.ParamSpec{{
		Name: "host", Type: "host", Required: true, Example: "app01",
		// ParamSpec carries no Aliases field (same constraint the actor-evidence tool documents), so the
		// tolerated alternatives are stated here. `host` is the key the SCHEMA requires — a call that names
		// only an alias is refused by the loop's arg screen with an actionable message, not executed.
		Description: "the host whose topology to read — pass it under the key `host` (target/device/hostname " +
			"are read as fallbacks)",
	}}
}

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

	// UPSTREAM — Parent carries Source directly, so provenance is exact per edge.
	var upObs, upLearned []string
	for _, p := range g.Parents(ent) {
		note := ""
		if p.Rel == estate.RelMemberOf {
			note = " — a grouping, not a probeable host"
		}
		line := fmt.Sprintf("%s %s via %s (confidence %.2f)%s", p.Entity.Type, p.Entity.Name, p.Rel, p.Confidence, note)
		if p.Source == estate.SourceIncident {
			upLearned = append(upLearned, line)
		} else {
			upObs = append(upObs, line)
		}
	}
	emitSection(&sb, "UPSTREAM (what it depends on — probe the infrastructure entries with get-active-alerts when you suspect a shared cause)", upObs, upLearned)

	// DEPENDENTS — BlastRadius confidence is a path product, so provenance rides Impact.Learned, not the number.
	var depObs, depLearned []string
	for _, d := range g.BlastRadius(ent, 3) {
		line := fmt.Sprintf("%s %s (confidence %.2f, distance %d)", d.Entity.Type, d.Entity.Name, d.Confidence, d.Distance)
		if d.Learned {
			depLearned = append(depLearned, line)
		} else {
			depObs = append(depObs, line)
		}
	}
	emitSection(&sb, fmt.Sprintf("DEPENDENTS (blast radius if %s fails, depth<=3)", ent.Name), depObs, depLearned)

	// COMMON-CAUSE SIBLINGS — the penalty puts even authoritative siblings below GroundTruthCutoff, so the
	// number cannot recover provenance; Impact.Learned carries it.
	var sibObs, sibLearned []string
	for _, s := range g.Siblings(ent) {
		line := fmt.Sprintf("%s %s (confidence %.2f)", s.Entity.Type, s.Entity.Name, s.Confidence)
		if s.Learned {
			sibLearned = append(sibLearned, line)
		} else {
			sibObs = append(sibObs, line)
		}
	}
	emitSection(&sb, "COMMON-CAUSE SIBLINGS (share an infrastructure parent — if several also alert, suspect that parent even if it is silent)", sibObs, sibLearned)

	res.Success = true
	res.Output = sb.String()
	return res, nil
}

// learnedMark is the EXACT token that flags a rendered adjacency as a co-occurrence GUESS rather than topology
// ground truth. TG-391: the tool used to print a 0.75 learned edge identically to a 0.95 PVE fact, so the agent
// went from "kube-etcd is not in the estate graph — do not guess its topology" to offering 37 fabricated
// parents on the same host in 15 minutes. The oracle anchors on this exact string so a RENAME reds it (a
// Contains check on a superstring would survive a deletion) — hence a named constant, never an inline literal.
const learnedMark = "learned-from-cooccurrence"

// emitSection writes one context section split into observed (ground-truth) and learned (co-occurrence guess)
// blocks, each capped and counted. Splitting is load-bearing, not cosmetic: TG-391 (a) marks the guesses AS
// guesses, (b) gives them their own count and cap so a wall of 0.75 co-occurrences can never crowd authoritative
// truth out of the top-8, and (c) when a section has ONLY guesses it says so in as many words and refuses to
// render a dependency tree — restoring the honest "not known" stance the tool had before the incident taught it.
func emitSection(sb *strings.Builder, header string, observed, learned []string) {
	fmt.Fprintf(sb, "\n%s: %d observed, %d %s (guessed)", header, len(observed), len(learned), learnedMark)
	if len(observed) == 0 && len(learned) == 0 {
		sb.WriteString("\n  (none known)")
		return
	}
	if len(observed) > 0 {
		sb.WriteString("\n  observed (ground truth):")
		emitCapped(sb, observed)
	}
	if len(learned) > 0 {
		if len(observed) == 0 {
			fmt.Fprintf(sb, "\n  NO OBSERVED TOPOLOGY — every entry below is %s (co-occurred during an incident; a GUESS, not a dependency). Treat this as \"not known\", not as topology:", learnedMark)
		} else {
			fmt.Fprintf(sb, "\n  %s (co-occurred during an incident; a GUESS, not topology):", learnedMark)
		}
		emitCapped(sb, learned)
	}
}

// emitCapped writes up to listCap lines, then an elision marker with the remainder — applied PER block so the
// observed and learned lists are bounded independently.
func emitCapped(sb *strings.Builder, lines []string) {
	for i, ln := range lines {
		if i == listCap {
			fmt.Fprintf(sb, "\n    … %d more", len(lines)-listCap)
			return
		}
		sb.WriteString("\n    - ")
		sb.WriteString(ln)
	}
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
