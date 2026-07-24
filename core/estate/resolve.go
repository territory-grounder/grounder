package estate

// Alias / hybrid resolution for the estate graph (design-wisdom #11; OpenAI Retrieval 5.1 — hybrid retrieval).
// The exact resolver (Resolve tier 1) matches a query only against the DOMAIN-STRIPPED, case-SENSITIVE
// canonical name index (`names[canonName]`). The port-fidelity design-wisdom map flagged the miss: "a host
// referenced under an alias or IP lands on a disconnected twin". A NetBox device typed in caps
// (`NLLEI01PVE01`), a LibreNMS sysName in a different case, a fully-qualified `dc1pve01.mgmt.lan`, or a
// separator variant (`dc1-pve01`) all fail the exact tier today even though the machine IS in the graph —
// so the prediction gate deems the host prediction-INELIGIBLE and the agent's estate-context window comes back
// empty, when the correct node was one case-fold away.
//
// This adds the fuzzy/alias tiers BEHIND the existing Resolve interface, as a pure, deterministic function:
// Resolve keeps its exact tier byte-identical and returns on the first hit, so a name that resolves today
// resolves to the SAME entity (no regression); only a name that PREVIOUSLY MISSED now gets a second and third
// chance. The strategy is exact → alias(normalized) → fuzzy(separator-folded), most-specific-type wins at
// every tier, and an AMBIGUOUS fuzzy match resolves to nothing (fail-closed — the resolver never guesses
// between two candidates). It is a read-model lookup only: it never mutates the estate, never actuates, and
// carries no per-request state. Mutation stays OFF.

import "strings"

// normalizeName is the alias-tier identity: the canonical (domain-stripped, trimmed) name LOWER-CASED. It folds
// the case and domain-qualification variations that the exact `canonName` index preserves and therefore
// misses, so `NLLEI01PVE01`, `dc1pve01.mgmt.lan` and `dc1pve01` share one alias key. Empty in ⇒ empty
// out (an empty alias is never indexed or matched).
func normalizeName(name string) string {
	return strings.ToLower(canonName(name))
}

// foldName is the fuzzy-tier identity: the normalized name with the host-name separators removed, so
// `dc1-pve01`, `dc1_pve01` and `dc1pve01` collapse to one fuzzy key. It is deliberately CONSERVATIVE
// — it folds only separators (never edit-distance), so it can never confuse `tg01` with `tg02`; the only names
// it unifies are genuine punctuation variants of the same token.
func foldName(name string) string {
	n := normalizeName(name)
	n = strings.ReplaceAll(n, "-", "")
	n = strings.ReplaceAll(n, "_", "")
	n = strings.ReplaceAll(n, " ", "")
	return n
}

// indexAlias records an entity under its normalized (alias-tier) name, keeping the MOST-SPECIFIC typed node
// when several entities share one normalized name — mirroring Resolve's exact-tier tie-break (a concrete
// pve_node/vm/lxc/physical_host beats the generic `host` twin). It is called for every entity registered into
// the graph, so the alias tier is self-populated from the multi-source names already flowing through the graph
// (no adapter change required). An empty name is skipped.
func (g *Graph) indexAlias(e Entity) {
	nk := normalizeName(e.Name)
	if nk == "" {
		return
	}
	if cur, ok := g.aliasNorm[nk]; !ok || typeRank(e.Type) > typeRank(cur.Type) {
		g.aliasNorm[nk] = e
	}
}

// RegisterAlias teaches the resolver that an EXTERNAL reference form (an IP address, a DNS CNAME, an
// operator-known alternate name) denotes an existing estate entity — the case the self-populated tiers cannot
// derive from graph names alone (an IP shares no token with its host). The alias is matched at the alias tier
// (normalized) and, transitively, the fuzzy tier; operator intent OVERRIDES a self-populated collision on the
// same normalized key. A blank alias or a nameless target is a no-op. This only extends the read-model lookup;
// it never adds a graph node (Export still derives nodes from edges) and never mutates the estate.
func (g *Graph) RegisterAlias(alias string, target Entity) {
	nk := normalizeName(alias)
	if nk == "" || strings.TrimSpace(target.Name) == "" {
		return
	}
	g.aliasNorm[nk] = target
}

// resolveHybrid is the fallback resolver Resolve consults ONLY when the exact tier misses. It applies, in
// order: the alias tier (an O(1) normalized-name lookup, covering case / domain / registered aliases / IPs),
// then the fuzzy tier (a separator-folded scan that resolves only when EXACTLY ONE distinct entity matches —
// an ambiguous fold fails closed). It returns ok=false when no tier resolves, preserving Resolve's "unknown
// name ⇒ ok=false" contract.
func (g *Graph) resolveHybrid(name string) (Entity, bool) {
	nk := normalizeName(name)
	if nk == "" {
		return Entity{}, false
	}
	if e, ok := g.aliasNorm[nk]; ok {
		return e, true
	}
	// Fuzzy tier: fold separators and accept a match only if it is UNIQUE across the alias index. Collecting
	// distinct entities by key makes the outcome independent of map-iteration order (deterministic).
	target := foldName(name)
	var hit Entity
	distinct := map[string]Entity{}
	for k, e := range g.aliasNorm {
		if foldName(k) != target { // k is already normalized, so foldName just strips its separators
			continue
		}
		distinct[e.key()] = e
		hit = e
	}
	if len(distinct) == 1 {
		return hit, true
	}
	return Entity{}, false
}
