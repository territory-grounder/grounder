package estate

// Site identity — the ESTATE-DERIVED host→site mapping the verdict author's coincidental-cross-site filter
// keys on (spec/002 REQ-107; PORT-FIDELITY finding: the predecessor's `_host_site()` derived site from the
// HOST IDENTITY over a closed vocabulary and excluded an alert only when BOTH sites were known AND differed —
// unknown-site hosts were NEVER excluded, so the filter could remove coincidental noise without ever hiding a
// genuine cross-site cascade). TG re-expresses the closed vocabulary as GRAPH DATA, not code: the sites the
// filter knows are exactly the `site`-typed entities the estate carries (declared config / CMDB seeding),
// so the vocabulary stays config-not-code and an unseeded estate derives NO site for any host (the verdict
// then excludes nothing — the fail-closed posture the verifier had before this mapping existed).
//
// Two derivation tiers, explicit-over-inferred:
//
//  1. MEMBERSHIP — the host has a `member_of` edge to a site entity. Operator/CMDB-declared, authoritative.
//  2. NAMING — the estate's host names encode the site as a NAME PREFIX (the NetBox convention this estate
//     uses: dc1pve01 / dc2fw01 carry their site prefix). A host whose normalized name begins with a
//     REGISTERED site entity's normalized name derives that site; the LONGEST matching site name wins (a
//     more specific site name is a more specific claim). A prefix shorter than minSitePrefix never matches —
//     a 1-byte site name would claim most of an estate by coincidence.
//
// Both tiers answer from data already in the graph; SiteOf never guesses. Unknown ⇒ ("", false), and the
// verdict author treats unknown as never-excluded (fail closed).

import "strings"

// minSitePrefix is the shortest site name the NAMING tier will match as a host-name prefix. Two bytes ("nl",
// "gr") is the smallest real vocabulary the predecessor carried; a single byte is indistinguishable from
// coincidence.
const minSitePrefix = 2

// SiteOf reports the site a host belongs to, derived from the estate graph: the host's `member_of` site
// (tier 1), else the longest registered site entity whose name prefixes the host's name (tier 2). ok=false
// when the estate holds no site claim for the host — the caller must treat an unknown site as "never
// exclude", exactly as the predecessor treated its site-less hosts. Pure read-model lookup: no mutation, no
// I/O, deterministic for a given graph.
func (g *Graph) SiteOf(host string) (string, bool) {
	if g == nil || strings.TrimSpace(host) == "" {
		return "", false
	}
	// Tier 1 — declared membership: a member_of edge from any typed twin of this host to a site entity.
	// Parents() already walks edges OUT of the canonical name and resolves each parent to its authoritative
	// type, so a site seen under a generic twin still answers.
	for _, p := range g.Parents(Entity{Type: TypeHost, Name: host}) {
		if p.Rel == RelMemberOf && p.Entity.Type == TypeSite && strings.TrimSpace(p.Entity.Name) != "" {
			return p.Entity.Name, true
		}
	}
	// Tier 2 — naming: the longest registered site whose normalized name prefixes the host's normalized name.
	// Sites are enumerated from the graph's own entity index, so the vocabulary is CLOSED over seeded data —
	// an estate that never declared a site derives nothing here.
	hn := normalizeName(host)
	bestName, bestLen := "", 0
	for _, ents := range g.names {
		for _, e := range ents {
			if e.Type != TypeSite {
				continue
			}
			sn := normalizeName(e.Name)
			if len(sn) < minSitePrefix || !strings.HasPrefix(hn, sn) {
				continue
			}
			// The host IS the site node itself (hn == sn): membership of a site in itself is not a host claim.
			if hn == sn {
				continue
			}
			// Longest normalized prefix wins; at equal length the lexicographically smallest raw name wins, so
			// the answer is deterministic even if one site was seeded under two case spellings.
			if len(sn) > bestLen || (len(sn) == bestLen && e.Name < bestName) {
				bestName, bestLen = e.Name, len(sn)
			}
		}
	}
	if bestLen > 0 {
		return bestName, true
	}
	return "", false
}
