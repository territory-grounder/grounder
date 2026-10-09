package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
)

// TG-394 SLICE 3 — the per-capability REACHABILITY / DEGRADED signal (fix-direction parts 2 & 4).
//
// These run without a DB: the estate graph is built in memory. They pin the property the pve03 cascade needed
// and TG lacked — a LIVE signal that one of TG's OWN dependency capabilities is degraded — over the graph TG
// ALREADY holds, with no active network probe.
//
// THE PRODUCTION SILENT-HYPERVISOR PATH (the shape TG-394 exists to catch). A fresh-ONLY reachability check is
// a BUG: when a hypervisor goes silent the estate does not drop its guests' placements, it TOMBSTONES them —
// carryForwardUnreachable re-inserts each runs_on edge FRESH (ValidUntil = now + TombstoneTTL) at the
// decayed TombstoneConfidence, so blast-radius still explains the outage. TestHostReachable_TombstoneIsNotLive
// builds exactly that (asserting the tombstone IS fresh, so the old len(Parents())>0 would have read reachable
// = the bug) and TestCapabilityDegraded_EmbedUnreachable places the embed backend as a tombstone — the way
// production manifests it, not a synthetic past-ValidUntil — and asserts the capability reads degraded.
//
// KILLING MUTATION (executed 2026-08-13): change hostReachable to `return true` (compiles; scoped to the one
// prober). The tombstoned embed backend then reads reachable, tg_capability_degraded{embed} stays 0 when it
// must be 1, and the degraded SET drops embed — TestCapabilityDegraded_EmbedUnreachable goes RED on both the
// gauge and the stamp. Confirmed RED, then reverted.
//
// VACUITY GUARD: TestSelfDepReachCapabilities_InventoryNonEmpty asserts the declared capability inventory is
// non-empty and embed is always present — an empty inventory must never pass as "nothing degraded" (the
// 19-modules-described-but-unconfigured failure mode).

// reachEnv is a getenv stub over a fixed map (default when a key is absent).
func reachEnv(m map[string]string) func(string, string) string {
	return func(k, d string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return d
	}
}

// placeHost adds a LIVE-CONFIRMED runs_on placement for host on a hypervisor (fresh, ground-truth 0.95 —
// above the tombstone floor). fresh=false expires the edge (ValidUntil in the past): the node still EXISTS
// (Upsert never drops an endpoint, so resolveDepHosts finds it) but Parents() filters the expired edge, so it
// reads UNREACHABLE — a host whose placement genuinely aged out (distinct from the tombstone path below).
func placeHost(g *estate.Graph, host, parent string, now time.Time, fresh bool) {
	e := estate.Edge{
		From: estate.Entity{Type: estate.TypeLXC, Name: host},
		To:   estate.Entity{Type: estate.TypePVENode, Name: parent},
		Rel:  estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE,
	}
	if !fresh {
		e.ValidUntil = now.Add(-time.Hour)
	}
	g.Upsert(e)
}

// tombstonePlace reproduces the PRODUCTION silent-hypervisor shape: exactly what carryForwardUnreachable
// (core/estate/tombstone.go) writes when host's hypervisor goes quiet — the guest's runs_on edge carried
// forward FRESH (ValidUntil = now + TombstoneTTL, in the future) at the decayed TombstoneConfidence. The edge
// passes g.fresh(), so len(Parents())>0 (the old check) reads it reachable; only the confidence floor catches it.
func tombstonePlace(g *estate.Graph, host, parent string, now time.Time) {
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeLXC, Name: host},
		To:   estate.Entity{Type: estate.TypePVENode, Name: parent},
		Rel:  estate.RelRunsOn, Confidence: estate.TombstoneConfidence, Source: estate.SourcePVE,
		ValidUntil: now.Add(estate.TombstoneTTL),
	})
}

// The embed backend's host is derived from TG_LITELLM_URL (the gateway the fused retriever dials). Setting it
// to a named host lets the test place that host in the estate and flip its freshness.
func reachCapsUnderTest() []selfDepCapability {
	return selfDepReachCapabilities(
		reachEnv(map[string]string{"TG_LITELLM_URL": "http://embedhost:4000"}),
		[]string{"jrnl-*"},
	)
}

func TestSelfDepReachCapabilities_InventoryNonEmpty(t *testing.T) {
	caps := reachCapsUnderTest()
	// VACUITY GUARD: an empty inventory would make every "not degraded" assertion below pass vacuously.
	if len(caps) == 0 {
		t.Fatal("the declared self-dependency capability inventory is EMPTY — an empty inventory silently passes " +
			"as 'nothing degraded' (the 19-described-but-unconfigured failure mode)")
	}
	byName := map[string][]string{}
	for _, c := range caps {
		byName[c.Name] = c.Globs
	}
	// embed is ALWAYS present (its gateway defaults to the compose endpoint) and carries a resolvable host here.
	if g, ok := byName[selfDepCapabilityEmbed]; !ok || len(g) != 1 || g[0] != "embedhost" {
		t.Fatalf("embed capability must be present with host [embedhost], got %v (present=%v)", g, ok)
	}
	if g, ok := byName[selfDepCapabilityJournal]; !ok || len(g) != 1 || g[0] != "jrnl-*" {
		t.Fatalf("journal-evidence capability must carry the declared glob [jrnl-*], got %v (present=%v)", g, ok)
	}
}

// TestHostReachable_TombstoneIsNotLive is the regression that pins the tombstone defect directly: it proves
// the fix discriminates EXACTLY the edge the old `len(Parents())>0` check could not.
func TestHostReachable_TombstoneIsNotLive(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))
	tombstonePlace(g, "silent-guest", "pve03", now) // hypervisor went silent → runs_on carried forward as a tombstone
	placeHost(g, "live-guest", "pve04", now, true)  // healthy → live-confirmed placement

	// The tombstone is FRESH — precisely the surface the OLD `len(Parents())>0` check PASSED, reading the silent
	// hypervisor's guest as reachable for up to TombstoneTTL (7 days). If this ever reads 0, the test no longer
	// exercises the bug (the old check would already have caught it) and the RED confirmation below is vacuous.
	if n := len(g.Parents(estate.Entity{Name: "silent-guest"})); n == 0 {
		t.Fatal("the tombstone edge must be FRESH (present in Parents) — the old len(Parents())>0 logic would have " +
			"read it reachable; without that, this test proves nothing about the fix")
	}
	// GREEN with the confidence floor: a tombstone (Confidence <= TombstoneConfidence) is NOT a live placement.
	if hostReachable(g, "silent-guest") {
		t.Error("a tombstoned guest (silent hypervisor) must read UNREACHABLE — the defect was that a FRESH " +
			"tombstone read reachable for up to 7 days, so the feature missed its own motivating incident")
	}
	// The fix does not over-reject: a genuinely live placement (fresh runs_on at 0.95) still reads reachable.
	if !hostReachable(g, "live-guest") {
		t.Error("a live-confirmed placement (fresh runs_on above the tombstone confidence) must read reachable")
	}

	// ROBUSTNESS BEYOND THE LITERAL FLOOR: a guest whose runs_on is tombstoned but whose NetBox site membership
	// OUTLIVES the silent hypervisor (member_of, fresh, 0.90) must STILL read unreachable. A naive "any fresh
	// edge > tombstone confidence" floor would let that surviving inventory edge mask the silent placement and
	// re-open the exact defect via a different edge; the confirmed-live check keys reachability on the runs_on
	// PLACEMENT, so a decayed placement is unreachable regardless of a surviving non-placement edge.
	tombstonePlace(g, "silent-guest-netbox", "pve03", now)
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeLXC, Name: "silent-guest-netbox"},
		To:   estate.Entity{Type: estate.TypeSite, Name: "nl"},
		Rel:  estate.RelMemberOf, Confidence: 0.90, Source: estate.SourceNetbox,
	})
	if hostReachable(g, "silent-guest-netbox") {
		t.Error("a tombstoned guest with a SURVIVING NetBox site edge must still read UNREACHABLE — a surviving " +
			"inventory edge must not mask a silent-hypervisor placement, or the tombstone defect re-opens")
	}
}

func TestCapabilityDegraded_EmbedUnreachable(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))
	tombstonePlace(g, "embedhost", "pve03", now) // embed backend on a SILENT hypervisor: tombstoned ⇒ unreachable
	placeHost(g, "jrnl-a", "pve04", now, true)   // journal host: live-confirmed placement ⇒ reachable

	caps := reachCapsUnderTest()
	// VACUITY GUARD (again, at the point of use): the inventory the assertions run over must be non-empty.
	if len(caps) == 0 {
		t.Fatal("empty capability inventory — refusing to assert 'degraded' over nothing")
	}
	samples := selfDepReachabilitySamples(g, caps)

	// (a) the rollup: embed degraded (its backing host's placement is a silent-hypervisor tombstone, not live),
	// journal-evidence NOT.
	if s, ok := findSample(samples, "tg_capability_degraded", map[string]string{"capability": selfDepCapabilityEmbed}); !ok || s.Value != 1 {
		t.Errorf("want tg_capability_degraded{embed}=1 (its embed backend's placement is only a tombstone), got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_capability_degraded", map[string]string{"capability": selfDepCapabilityJournal}); !ok || s.Value != 0 {
		t.Errorf("want tg_capability_degraded{journal-evidence}=0 (its host is live-confirmed), got %+v ok=%v", s, ok)
	}
	// (b) the per-host reachability: embedhost reads 0.
	if s, ok := findSample(samples, "tg_self_dependency_reachable", map[string]string{"capability": selfDepCapabilityEmbed, "host": "embedhost"}); !ok || s.Value != 0 {
		t.Errorf("want tg_self_dependency_reachable{embed,embedhost}=0, got %+v ok=%v", s, ok)
	}
	// (c) the coverage denominator: embed had exactly one host to check (so degraded=1 is MEASURED, not vacuous).
	if s, ok := findSample(samples, "tg_self_dependency_hosts_checked", map[string]string{"capability": selfDepCapabilityEmbed}); !ok || s.Value != 1 {
		t.Errorf("want tg_self_dependency_hosts_checked{embed}=1, got %+v ok=%v", s, ok)
	}
	// (d) the SESSION STAMP set (part 4): the degraded set carries embed and not journal-evidence.
	set := degradedCapabilitySet(g, caps)
	if !contains(set, selfDepCapabilityEmbed) {
		t.Errorf("degraded set must include %q, got %v", selfDepCapabilityEmbed, set)
	}
	if contains(set, selfDepCapabilityJournal) {
		t.Errorf("degraded set must NOT include journal-evidence (its host is fresh), got %v", set)
	}
}

func TestCapabilityDegraded_AllFresh(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))
	placeHost(g, "embedhost", "pve03", now, true) // all backing hosts FRESH ⇒ reachable
	placeHost(g, "jrnl-a", "pve04", now, true)

	caps := reachCapsUnderTest()
	samples := selfDepReachabilitySamples(g, caps)

	// Every degraded rollup reads 0.
	for _, s := range samples {
		if s.Name == "tg_capability_degraded" && s.Value != 0 {
			t.Errorf("with every dependency host fresh, tg_capability_degraded{%s} must be 0, got %v", s.Labels["capability"], s.Value)
		}
	}
	// And the embed rollup is MEASURED (a host was checked), so the 0 is "healthy", not "unmeasured".
	if s, ok := findSample(samples, "tg_self_dependency_hosts_checked", map[string]string{"capability": selfDepCapabilityEmbed}); !ok || s.Value < 1 {
		t.Errorf("want tg_self_dependency_hosts_checked{embed}>=1 so the healthy 0 is measured, got %+v ok=%v", s, ok)
	}
	if set := degradedCapabilitySet(g, caps); len(set) != 0 {
		t.Errorf("with every host fresh the degraded set must be EMPTY, got %v", set)
	}
}

func TestSelfDepReachability_NilAndEmptySafe(t *testing.T) {
	if s := selfDepReachabilitySamples(nil, reachCapsUnderTest()); s != nil {
		t.Errorf("a nil graph must emit nothing, got %v", s)
	}
	if set := degradedCapabilitySet(nil, reachCapsUnderTest()); set != nil {
		t.Errorf("a nil graph must yield no degraded set, got %v", set)
	}
	if r := startSelfDepReachabilityJob(nil, reachCapsUnderTest())(); r != nil {
		t.Errorf("a nil holder job must emit nothing, got %v", r)
	}
	// An endpoint that resolves to no estate host (a compose-internal gateway) is UNMEASURED, never degraded.
	g := estate.NewGraph(estate.WithClock(func() time.Time { return time.Now() }))
	caps := selfDepReachCapabilities(reachEnv(map[string]string{"TG_LITELLM_URL": "http://litellm:4000"}), nil)
	if set := degradedCapabilitySet(g, caps); len(set) != 0 {
		t.Errorf("a compose-internal gateway resolves to no estate host — it must be unmeasured, not degraded; got %v", set)
	}
	if s, ok := findSample(selfDepReachabilitySamples(g, caps), "tg_self_dependency_hosts_checked", map[string]string{"capability": selfDepCapabilityEmbed}); !ok || s.Value != 0 {
		t.Errorf("compose-internal embed gateway must report hosts_checked=0 (unmeasured), got %+v ok=%v", s, ok)
	}
}

// learnedEdge adds a THIN LEARNED co-occurrence edge (estate.SourceIncident, a `depends_on` at
// LearnedConfidence(count) — count<=2 ⇒ <=0.5, BELOW the tombstone floor) from host to a co-occurrence
// primary, exactly as core/estate/learned.go emits them. This is the provenance whose low confidence encodes
// LEARNING STRENGTH, not an outage — the false-degraded input TG-460 excludes.
func learnedEdge(g *estate.Graph, host, primary string, count int) {
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypeHost, Name: host},
		To:   estate.Entity{Type: estate.TypeHost, Name: primary},
		Rel:  estate.RelDependsOn, Confidence: estate.LearnedConfidence(count), Source: estate.SourceIncident,
	})
}

// authoritativeBareEdge adds an AUTHORITATIVE, live-confirmed adjacency (NetBox member_of at 0.90, above the
// tombstone floor) for a NON-GUEST host (no runs_on) — a bare/physical dependency host a real source still
// observes. This is what the non-guest fallback must accept as reachable.
func authoritativeBareEdge(g *estate.Graph, host string) {
	g.Upsert(estate.Edge{
		From: estate.Entity{Type: estate.TypePhysicalHost, Name: host},
		To:   estate.Entity{Type: estate.TypeSite, Name: "nl"},
		Rel:  estate.RelMemberOf, Confidence: 0.90, Source: estate.SourceNetbox,
	})
}

// TG-460 — the non-guest reachability refinement, at the classification level. A non-guest dependency host is
// confirmed live ONLY by an AUTHORITATIVE-source edge above the tombstone floor; a host whose sole estate
// evidence is a thin LEARNED co-occurrence edge is EXCLUDED (its sub-floor confidence is about co-occurrence
// strength, not an outage), never read as degraded. Guest paths (steps 1-2, TG-394 slice 3) are unchanged.
//
// RED WITHOUT THE FIX: with authoritativeSource neutered to `return true` (the pre-TG-460 behavior — every
// source, including the learned tier, treated as an observation), the learned-only host reads reachDegraded
// instead of reachExcluded and this test fails on that case. Confirmed RED 2026-08-13, then restored.
func TestHostReachState_NonGuestAuthoritativeVsLearned(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))

	authoritativeBareEdge(g, "bare-auth")               // (1) non-guest, authoritative live edge → reachLive
	learnedEdge(g, "learned-only", "co-primary", 1)     // (2) non-guest, ONLY a thin learned edge → reachExcluded
	learnedEdge(g, "learned-and-auth", "co-primary", 1) // (3) both a learned edge AND ...
	authoritativeBareEdge(g, "learned-and-auth")        //     ... an authoritative live edge → the authority wins
	placeHost(g, "live-guest", "pve04", now, true)      // (4) guest, live runs_on → reachLive (unchanged)
	tombstonePlace(g, "silent-guest", "pve03", now)     // (4) guest, tombstoned runs_on only → reachDegraded (unchanged)

	// SANITY: the learned edge is FRESH and genuinely below the tombstone floor — so the OLD non-guest fallback
	// ("any fresh parent above the floor is reachable; else degraded") really did read this host as degraded.
	// Without this, the RED confirmation below would be vacuous.
	lp := g.Parents(estate.Entity{Name: "learned-only"})
	if len(lp) != 1 || lp[0].Source != estate.SourceIncident {
		t.Fatalf("learned-only must have exactly one fresh learned parent, got %+v", lp)
	}
	if lp[0].Confidence > estate.TombstoneConfidence {
		t.Fatalf("the learned edge must sit BELOW the tombstone floor (that is the defect), got confidence %v", lp[0].Confidence)
	}

	for _, tc := range []struct {
		host string
		want reachState
	}{
		{"bare-auth", reachLive},
		{"learned-only", reachExcluded},
		{"learned-and-auth", reachLive},
		{"live-guest", reachLive},
		{"silent-guest", reachDegraded},
	} {
		if got := hostReachState(g, tc.host); got != tc.want {
			t.Errorf("hostReachState(%q) = %d, want %d (reachLive=%d reachDegraded=%d reachExcluded=%d)",
				tc.host, got, tc.want, reachLive, reachDegraded, reachExcluded)
		}
	}
}

// TestCapabilityDegraded_LearnedOnlyHostExcluded is the CALLER-LEVEL killing test for TG-460: a capability
// whose backing host resolves to ONLY a thin learned co-occurrence edge must read degraded=0 with
// hosts_checked=0 (EXCLUDED / unmeasured) — NOT degraded=1 — and must not enter the degraded set. WITHOUT the
// fix the learned-only host reads unreachable, so degraded=1 and hosts_checked=1: a false positive with a
// denominator that misleadingly claims the host was measured. A sibling capability on a genuine silent-
// hypervisor tombstone STILL degrades (the exclusion does not mask a real outage), and an authoritative-live
// capability reads healthy-and-measured. This proves BOTH the numerator (degraded) and the denominator
// (hosts_checked) drop the excluded host together — no TG-449-style numerator/denominator membership mismatch.
func TestCapabilityDegraded_LearnedOnlyHostExcluded(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))

	learnedEdge(g, "learnedhost", "co-primary", 1) // sole evidence: a thin SourceIncident edge below the floor
	authoritativeBareEdge(g, "barehost")           // authoritative, live-confirmed (NetBox 0.90, no runs_on)
	tombstonePlace(g, "tombhost", "pve03", now)    // genuinely degraded: a silent-hypervisor tombstone

	caps := []selfDepCapability{
		{Name: "learnedcap", Globs: []string{"learnedhost"}},
		{Name: "authcap", Globs: []string{"barehost"}},
		{Name: "tombcap", Globs: []string{"tombhost"}},
	}
	samples := selfDepReachabilitySamples(g, caps)

	// (2) learnedcap: EXCLUDED — degraded=0, hosts_checked=0 (unmeasured), and NO per-host reachable series.
	if s, ok := findSample(samples, "tg_capability_degraded", map[string]string{"capability": "learnedcap"}); !ok || s.Value != 0 {
		t.Errorf("a learned-only host must NOT degrade its capability: want tg_capability_degraded{learnedcap}=0, got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_hosts_checked", map[string]string{"capability": "learnedcap"}); !ok || s.Value != 0 {
		t.Errorf("a learned-only host must be EXCLUDED from the coverage denominator: want tg_self_dependency_hosts_checked{learnedcap}=0, got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_reachable", map[string]string{"capability": "learnedcap", "host": "learnedhost"}); ok {
		t.Errorf("a learned-only host must emit NO per-host reachable series (neither reachable nor degraded), got %+v", s)
	}

	// (1) authcap: reachable AND measured — degraded=0, hosts_checked=1, reachable=1.
	if s, ok := findSample(samples, "tg_capability_degraded", map[string]string{"capability": "authcap"}); !ok || s.Value != 0 {
		t.Errorf("want tg_capability_degraded{authcap}=0 (authoritative live host), got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_hosts_checked", map[string]string{"capability": "authcap"}); !ok || s.Value != 1 {
		t.Errorf("an authoritative-live host must be counted: want tg_self_dependency_hosts_checked{authcap}=1, got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_reachable", map[string]string{"capability": "authcap", "host": "barehost"}); !ok || s.Value != 1 {
		t.Errorf("want tg_self_dependency_reachable{authcap,barehost}=1, got %+v ok=%v", s, ok)
	}

	// (4) tombcap: the exclusion must NOT mask a real outage — an observed tombstone still degrades and is measured.
	if s, ok := findSample(samples, "tg_capability_degraded", map[string]string{"capability": "tombcap"}); !ok || s.Value != 1 {
		t.Errorf("a silent-hypervisor tombstone must still degrade: want tg_capability_degraded{tombcap}=1, got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_hosts_checked", map[string]string{"capability": "tombcap"}); !ok || s.Value != 1 {
		t.Errorf("want tg_self_dependency_hosts_checked{tombcap}=1 (a real tombstone is measured), got %+v ok=%v", s, ok)
	}

	// The session STAMP set (part 4) matches the gauge EXACTLY: tombcap degraded; learnedcap and authcap not.
	set := degradedCapabilitySet(g, caps)
	if !contains(set, "tombcap") {
		t.Errorf("degraded set must include tombcap (a real tombstone), got %v", set)
	}
	if contains(set, "learnedcap") {
		t.Errorf("degraded set must NOT include learnedcap (learned-only host is excluded, not degraded), got %v", set)
	}
	if contains(set, "authcap") {
		t.Errorf("degraded set must NOT include authcap (its host is live-confirmed), got %v", set)
	}
}
