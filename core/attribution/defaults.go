package attribution

import _ "embed"

// defaultConfigJSON is the GENERIC out-of-box actor_attribution ruleset (loadable data, never a Go
// literal — REQ-2308, the prose-loadable rule). It is deliberately install-neutral: it carries ONLY the
// portable taxonomy→disposition mapping. It names NO sanctioned principals and NO carve-outs — those are
// site-specific (a realm's admins, a lab's test-pool hostnames) and must NEVER be baked into the shipped
// binary. An operator supplies them at deploy time via a complete override document (the worker loads it
// from TG_ATTRIBUTION_CONFIG, replacing this default when present). With the generic default alone, a
// non-self actor reads attributed-suspicious until the operator declares their sanctioned principals —
// the safe, evidence-gated direction (fail toward human review, never toward silent auto-heal).
//
//go:embed default_config.json
var defaultConfigJSON []byte

// DefaultConfigDocument returns a COPY of the GENERIC default actor_attribution document — the
// install-neutral governed baseline a fresh deployment runs on when no operator override exists. A COPY
// is returned so a caller can never mutate the embedded doc.
func DefaultConfigDocument() []byte {
	out := make([]byte, len(defaultConfigJSON))
	copy(out, defaultConfigJSON)
	return out
}
