// Package execclass is the execution-topology decision made BEFORE expensive context construction: it maps
// an incident to one of five execution classes so a cheap incident does not pay the full ceremonial
// lifecycle (RAG + prediction + large prompt + verification) that a hard correlated incident needs.
//
// Provenance: [F] the external quality audit's top-3 recommendation — "introduce deterministic, fast-agent,
// standard and deep-agent execution classes … a major latency and token reduction without reducing
// capability … made before expensive context construction." The predecessor only APPROXIMATED this with
// categories, model hints and risk bands; TG makes it an explicit typed decision. See
// docs/EXTERNAL-AUDIT-LESSONS.md #3.
//
// The classifier is pure and deterministic (INV-08: no model token chooses the topology) and fail-safe:
// when the signals are ambiguous it routes UP to the more thorough class, never down — an unclassifiable
// incident gets the full treatment, never a shortcut.
package execclass

import "strings"

// Class is the execution topology chosen for an incident.
type Class string

const (
	// Deterministic runs a known, bounded procedure with NO agent (a known scheduled reboot, a read-only
	// status request, a recurrent incident with a verified fixed procedure).
	Deterministic Class = "DETERMINISTIC"
	// FastAgent uses a compact prompt and cached context with a single verification — a recurrent,
	// reversible, isolated incident with a high-confidence prior.
	FastAgent Class = "FAST_AGENT"
	// StandardAgent is the normal single-agent path (full-but-not-deep context).
	StandardAgent Class = "STANDARD_AGENT"
	// DeepInvestigation is the full path: deep RAG, graph traversal, extended verification (novel or
	// correlated/cascading incidents).
	DeepInvestigation Class = "DEEP_INVESTIGATION"
	// HumanLed means the agent gathers evidence and alternatives but does NOT own the decision (ambiguous
	// or incomplete events, or where the operator must lead).
	HumanLed Class = "HUMAN_LED"
)

// Input is the set of signals available BEFORE any expensive context is built — cheap, deterministic facts
// about the incident. It deliberately excludes anything that requires RAG, an LLM, or the estate graph.
type Input struct {
	// KnownProcedure is true when the incident matches a registered, bounded, verified remediation
	// procedure (a scheduled-reboot promotion, a known-transient, a read-only status probe).
	KnownProcedure bool
	// ReadOnly is true when the requested work mutates nothing (a status/inspection request).
	ReadOnly bool
	// KnownPattern is true when a high-confidence prior solution exists for this (host, rule) — a recurrent
	// incident (e.g. a tier-1 known-pattern match).
	KnownPattern bool
	// Novel is true when no prior solution is known.
	Novel bool
	// Correlated is true when the incident spans multiple systems / looks like a cascade.
	Correlated bool
	// Reversible is the incident's proposed-remediation reversibility hint (conservative default: false).
	Reversible bool
	// Ambiguous is true when the triggering event is ambiguous or incomplete (missing fields, unclear
	// scope) — the human should lead.
	Ambiguous bool
	// CriticalityTier is the target's criticality ("host"/"service"/…); a P0/host-tier incident is never
	// fast-pathed.
	CriticalityTier string
}

// Classify chooses the execution class. It is fail-safe: a correlated or novel incident, an ambiguous event,
// or a high-criticality target always routes to the thorough/human path even if other signals suggest a
// shortcut. Only an unambiguous, isolated, reversible, non-critical incident earns a fast or deterministic
// path.
func Classify(in Input) Class {
	critical := isHostTier(in.CriticalityTier)

	// Ambiguous/incomplete events: the human leads regardless of anything else.
	if in.Ambiguous {
		return HumanLed
	}
	// Correlated or novel incidents always get the deep path — a shortcut here re-opens the exact class of
	// multi-system failure the deep path exists for.
	if in.Correlated || in.Novel {
		return DeepInvestigation
	}
	// A registered bounded procedure or a pure read-only request needs no agent — but never for a
	// high-criticality target (a P0 host gets an agent even for a "known" procedure).
	if (in.KnownProcedure || in.ReadOnly) && !critical {
		return Deterministic
	}
	// A recurrent, reversible, isolated, non-critical incident with a high-confidence prior: fast path.
	if in.KnownPattern && in.Reversible && !critical {
		return FastAgent
	}
	// Everything else: the normal single-agent path.
	return StandardAgent
}

func isHostTier(tier string) bool {
	t := strings.ToLower(strings.TrimSpace(tier))
	return t == "host" || t == "p0" || t == "critical"
}

// SkipsAgent reports whether a class runs without an agent session at all (no prompt, no model spend).
func SkipsAgent(c Class) bool { return c == Deterministic }

// NeedsDeepContext reports whether a class warrants the full RAG + graph-traversal context build. The fast
// and deterministic classes deliberately do not (the whole point of the routing).
func NeedsDeepContext(c Class) bool { return c == DeepInvestigation || c == StandardAgent }

// HumanOwnsDecision reports whether the class requires the human to own the final decision (the agent only
// gathers evidence).
func HumanOwnsDecision(c Class) bool { return c == HumanLed }

// Valid reports whether c is a known class.
func Valid(c Class) bool {
	switch c {
	case Deterministic, FastAgent, StandardAgent, DeepInvestigation, HumanLed:
		return true
	default:
		return false
	}
}
