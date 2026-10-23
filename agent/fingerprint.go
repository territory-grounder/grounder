package agent

import (
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/harnessid"
)

// HarnessParts are the four effective components of the SYSTEM half of this agent's harness (TG-575,
// TG-600; Harness² learnings H2-1/H2-1c): the protocol prose, the tool catalog, the op-class catalog
// INCLUDING any ratified runtime overlay, and the store-composed guidance half, each exactly as the
// model receives it. They are rendered for the canonical STANDARD_AGENT class so the value identifies the
// CONFIGURATION the session was composed under, not the session: a FAST_AGENT session on the same worker
// renders the same parts, while a base-prompt TRIAL ARM (a different guidance half) or a pack-filtered
// tool catalog is a different configuration — exactly what a later reader needs in order not to pool them.
//
// harnessid.Assemble(HarnessParts()) is byte-identical to the system message the canonical class receives
// (the fingerprint test pins it); harnessid.Fingerprint(parts, preambleVersion) is the value persisted as
// session_triage.harness_fp, and the render store persists the parts themselves under it (TG-600). The
// model tiers are deliberately NOT folded in — the investigate tier is chosen per session (class → tier)
// and both already ride the triage row as model_tier / decision_model_tier, so the configuration identity
// is the tuple (harness_fp, model_tier, decision_model_tier, skill_loads). Incident data never enters it
// (the seed is hashed separately as seed_hash). OBSERVABILITY ONLY — nothing reads it on the decision
// path (INV-08).
func (a *Agent) HarnessParts() harnessid.Parts {
	return preambleParts(toolListFor(a.Tools, execclass.StandardAgent), effectiveOpClassCatalog(), a.Guidance)
}
