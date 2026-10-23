package estate

import (
	"testing"
	"time"
)

func hostNode(n string) Entity { return Entity{Type: TypeHost, Name: n} }

// The EXACT tier is byte-identical to the original resolver: a name that resolved before resolves to the SAME
// entity, an unknown name still fails, and the most-specific typed node still beats the generic host twin.
// (a) no regression.
func TestResolveExactTierUnchanged(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("z"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.register(hostNode("dc1pve01")) // a generic host twin of the same name

	// exact bare name → the concrete node, not the host twin.
	if got, ok := g.Resolve("dc1pve01"); !ok || got.Type != TypePVENode {
		t.Fatalf("exact resolve must return the pve_node, got %+v ok=%v", got, ok)
	}
	// exact domain-stripped name (same case) → resolves, as before.
	if got, ok := g.Resolve("dc1pve01.example.net"); !ok || got.Type != TypePVENode {
		t.Fatalf("domain-stripped exact resolve must return the pve_node, got %+v ok=%v", got, ok)
	}
	// a genuinely unknown name still fails at every tier — the resolver never invents a node.
	if _, ok := g.Resolve("unknown-host"); ok {
		t.Fatal("an unknown name must not resolve through any tier")
	}
}

// The ALIAS tier recovers case- and domain-qualification variants that the case-sensitive exact index misses —
// the "disconnected twin" the design-wisdom map flagged. Most-specific type still wins. (a) previously-missing
// forms now resolve.
func TestResolveAliasTierRecoversCaseAndDomain(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("z"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.register(hostNode("dc1pve01")) // generic twin — the alias tier must still prefer the pve_node

	for _, form := range []string{
		"NLLEI01PVE01",          // all-caps (a NetBox device name)
		"Nllei01Pve01",          // mixed case
		"NLLEI01PVE01.MGMT.LAN", // caps + fully-qualified
		"dc1pve01.mgmt.lan", // fqdn only
	} {
		got, ok := g.Resolve(form)
		if !ok {
			t.Fatalf("alias tier must resolve %q (it is the same machine as dc1pve01)", form)
		}
		if got.Type != TypePVENode || got.Name != "dc1pve01" {
			t.Fatalf("alias resolve of %q must return the concrete pve_node, got %+v", form, got)
		}
	}
}

// The FUZZY tier recovers separator variants (a host written with a dash/underscore where the graph has none),
// but ONLY when the fold is UNAMBIGUOUS — two different entities folding to the same token resolve to nothing
// (fail-closed; the resolver never guesses). (a) previously-missing forms + (b) safety of the fuzzy tier.
func TestResolveFuzzyTierSeparatorsAndAmbiguityFailsClosed(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("g"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	// unique separator variant folds to the one machine.
	for _, form := range []string{"dc1-pve01", "dc1_pve01", "NLLEI01-PVE01"} {
		if got, ok := g.Resolve(form); !ok || got.Name != "dc1pve01" {
			t.Fatalf("fuzzy tier must resolve separator variant %q → dc1pve01, got %+v ok=%v", form, got, ok)
		}
	}

	// ambiguity: two DISTINCT entities whose separator-folds collide ("tg01") — a fuzzy query must fail closed.
	amb := NewGraph()
	amb.register(pveNode("tg-01"))
	amb.register(lxc("tg_01"))
	if _, ok := amb.Resolve("tg01"); ok {
		t.Fatal("an ambiguous fuzzy fold (two distinct entities) must resolve to nothing, not guess one")
	}
	// but each exact/alias form still resolves to its own node (the ambiguity is only at the folded query).
	if got, ok := amb.Resolve("TG-01"); !ok || got.Name != "tg-01" {
		t.Fatalf("the alias tier must still resolve the unambiguous TG-01, got %+v ok=%v", got, ok)
	}
}

// RegisterAlias teaches an EXTERNAL reference form (an IP, a CNAME) that shares no token with its host — the
// case the self-populated tiers cannot derive from graph names. Operator intent overrides a self-populated
// collision. (a) alias resolution for a form that previously could never resolve.
func TestRegisterAliasResolvesIPAndAlternateName(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: hostNode("dc1tg01"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	tg, ok := g.Resolve("dc1tg01")
	if !ok {
		t.Fatal("precondition: the host must resolve by its canonical name")
	}

	g.RegisterAlias("10.20.0.5", tg)        // an IP that denotes the host
	g.RegisterAlias("tg01.example.org", tg) // a CNAME alternate name

	for _, form := range []string{"10.20.0.5", "tg01.example.org", "TG01.EXAMPLE.ORG"} {
		got, ok := g.Resolve(form)
		if !ok || got.Name != "dc1tg01" {
			t.Fatalf("RegisterAlias must make %q resolve to dc1tg01, got %+v ok=%v", form, got, ok)
		}
	}
	// a blank alias or a nameless target is a harmless no-op.
	g.RegisterAlias("", tg)
	g.RegisterAlias("nowhere", Entity{})
	if _, ok := g.Resolve("nowhere"); ok {
		t.Fatal("registering a nameless target must not create a resolvable alias")
	}
}

// (c) decay-on-disproof lowers a CONTRADICTED learned belief while leaving a still-supported one untouched, and
// (integration with this change) the alias/fuzzy resolution tiers SURVIVE the decay clone that the holder swaps
// in — a decayed snapshot must not silently lose alias resolution until the next full rebuild.
func TestDecayLeavesSupportedBeliefAndPreservesAliasResolution(t *testing.T) {
	nowT := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return nowT }))
	// two LEARNED edges; only one is named by the disproof.
	g.Upsert(Edge{From: hostNode("app01"), To: hostNode("db01"), Rel: RelDependsOn, Confidence: 0.75, Source: SourceIncident})
	g.Upsert(Edge{From: hostNode("cache01"), To: hostNode("db01"), Rel: RelDependsOn, Confidence: 0.75, Source: SourceIncident})

	newG, rep := g.DecayOnDisproof(Disproof{Hosts: []string{"app01"}, At: nowT}, DecayOptions{Factor: 0.5})
	if rep.Decayed != 1 {
		t.Fatalf("only the contradicted learned edge (app01→db01) must decay, got %+v", rep)
	}
	if c := edgeConf(newG, "app01", "db01"); c < 0.374 || c > 0.376 {
		t.Fatalf("the contradicted belief must halve 0.75 → 0.375, got %.4f", c)
	}
	// the still-supported learned belief (cache01→db01) is NOT named by the disproof and must be untouched.
	if c := edgeConf(newG, "cache01", "db01"); c != 0.75 {
		t.Fatalf("a still-supported learned belief must NOT decay, got %.4f", c)
	}

	// alias/fuzzy resolution must still work on the decayed snapshot (clone carried aliasNorm).
	if got, ok := newG.Resolve("APP01"); !ok || got.Name != "app01" {
		t.Fatalf("alias resolution must survive the decay clone, got %+v ok=%v", got, ok)
	}
	if got, ok := newG.Resolve("cache-01"); !ok || got.Name != "cache01" {
		t.Fatalf("fuzzy resolution must survive the decay clone, got %+v ok=%v", got, ok)
	}
}
