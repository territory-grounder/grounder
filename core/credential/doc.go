// Package credential is Territory Grounder's Credential / Identity Engine — the AUTHENTICATION half of
// governed actuation and the sibling of the Policy Engine (spec/015, which is AUTHORIZATION). The policy
// engine answers "may TG act on this target?"; this engine answers "with what identity does TG authenticate
// to this target?". They compose at the actuation chokepoint:
//
//	agent proposes {op-class, target} → POLICY: may this? (authZ) → CREDENTIAL: resolve identity (authN) → actuate
//
// This file set implements the FIRST build slice (spec/016 tasks T-016-1..4): the native, standalone
// credential resolver core — a per-target `Resolve(Target) → (Bundle, error)` entry point backed by an
// in-memory native rule table (the config-not-code fallback when nothing is synced). Sync, the external
// connectors (AWX/Vault/LDAP/…), the two-plane router, the policy compose seam, and the console UX are
// LATER slices (T-016-5..13) and are intentionally NOT built here.
//
// The load-bearing safety property is FAIL-CLOSED BY CONSTRUCTION (REQ-1602, INV-09): the resolver's zero
// value is "unresolved / refuse", a Bundle's zero value is invalid, and every error, unmatched, or ambiguous
// path returns the zero Bundle with a typed error — there is NEVER a fallback to a default, global, or
// last-used identity. Every secret-bearing field is a core/config.SecretRef (env:/file:/store:/…), never a
// plaintext value (INV-13). The target-matching primitive (Selector/Target/Match) is a SHARED estate
// object-model built once here and consumed by BOTH this resolver and the policy engine (spec/015 REQ-1505,
// REQ-1605) — there is deliberately no second, divergent inventory grammar.
//
// Provenance: [F] generalizes the hostdiag `site|hostglob|sshuser|keyref` read-only access allowlist
// (modules/observability/hostdiag) into a full per-target resolver · [R] paradigm-rule 4 (config-not-code)
// · [O] INV-09/INV-13/INV-17. See spec/016-credential-engine/{requirements,design}.md.
package credential
