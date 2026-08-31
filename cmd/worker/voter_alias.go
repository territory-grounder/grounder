package main

// TG-463 (owner ruling TG-488 B26): the VOTER-ALIAS NORMALIZER — the one sound actor-side identity
// operation the frozen-at-gate ruling permits. The approve_by set is expanded to CONCRETE user entries at
// GATE time and frozen into workflow history (cmd/worker/approve_by_wiring.go); admission is the PURE
// runner.VoterAdmitted string test. But the Matrix inbound lane presents the voter as an MXID
// ("@kyriakos:example.net") while the frozen entries carry console logins ("kyriakos") — so an owner's
// chat ballot string-compared as a stranger. Normalization maps a PRESENTED identity to its CANONICAL
// login BEFORE the signal is sent (surface-side, never in workflow code), which is deterministic-safe:
// it rewrites the voter's own spelling and can never widen the frozen set — an alias that resolves to a
// login outside approve_by is still refused by VoterAdmitted exactly as before.
//
// FAIL DIRECTIONS, all toward refusal, never invention:
//   - an unknown presented identity passes through UNCHANGED (the frozen set then refuses as today);
//   - a malformed config pair is SKIPPED LOUDLY (a broken alias never half-applies);
//   - a duplicate alias with a CONFLICTING canonical refuses the WHOLE config (empty map, passthrough,
//     loud) — an ambiguous identity table on an authorization surface must not guess.
//
// Config, not code (the concrete owner identities are estate data and the repo is public-mirrored):
// TG_VOTER_ALIASES="alias=canonical[,alias=canonical...]", e.g. "@kyriakos:example.net=kyriakos".
// "LDAP federation is a resolution path, not a wider set" (B26): a future directory-backed alias SOURCE
// may populate this same map; the semantics here do not change.

import (
	"log"
	"strings"
)

// parseVoterAliases parses TG_VOTER_ALIASES into presented→canonical. Alias matching is case-insensitive
// (keys are lower-cased; VoterAdmitted itself matches EqualFold, so case never decides authorization).
func parseVoterAliases(env string) map[string]string {
	env = strings.TrimSpace(env)
	if env == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(env, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.IndexByte(pair, '=')
		if i <= 0 || i == len(pair)-1 {
			log.Printf("voter aliases: malformed pair %q SKIPPED (want alias=canonical) — that alias will pass through unnormalized", pair)
			continue
		}
		alias := strings.ToLower(strings.TrimSpace(pair[:i]))
		canonical := strings.TrimSpace(pair[i+1:])
		if alias == "" || canonical == "" {
			log.Printf("voter aliases: blank half in pair %q SKIPPED", pair)
			continue
		}
		if prev, dup := out[alias]; dup && !strings.EqualFold(prev, canonical) {
			log.Printf("voter aliases: alias %q maps to BOTH %q and %q — ambiguous identity table REFUSED in full (all votes pass through unnormalized)", alias, prev, canonical)
			return nil
		}
		out[alias] = canonical
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeVoter maps a presented voter identity to its canonical login, or returns it UNCHANGED when no
// alias matches — it never invents an identity and never empties one.
func normalizeVoter(aliases map[string]string, presented string) string {
	p := strings.TrimSpace(presented)
	if p == "" || len(aliases) == 0 {
		return presented
	}
	if c, ok := aliases[strings.ToLower(p)]; ok {
		return c
	}
	return presented
}
