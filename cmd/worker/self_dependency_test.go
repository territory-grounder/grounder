package main

import (
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
)

// TG-394: the self-dependency concentration exporter. These run without a DB — the estate graph is built in
// memory — and pin the two properties that make the signal honest: (1) the coverage pair is ALWAYS emitted
// when the graph is present, so a scrape distinguishes "no concentration" from "exporter gone"; (2) a parent
// is only reported once 2+ of TG's dependency hosts land on it (the single-point-of-failure), and it reports
// the true count.
//
// KILLING MUTATIONS (executed 2026-08-11):
//   - drop `if len(grp.Hosts) < 2 { continue }`: pve04 (1 host) emits a concentration series and
//     TestSelfDepConcentration_OnlyParentsWith2Plus goes RED.
//   - stop emitting hosts_resolved when there is no concentration: TestSelfDep_CoveragePairAlwaysEmitted RED.
//   - match globs against non-host entity types (drop depHostType): the same-named site resolves as a
//     phantom dependency host and TestResolveDepHosts_MatchesHostEntitiesOnly RED.

func gtestGraph(t *testing.T) *estate.Graph {
	t.Helper()
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))
	// 3 dependency hosts on pve03, 1 on pve04 — the TG-394 concentration shape.
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "dep-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve03"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "dep-b"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve03"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeVM, Name: "dep-c"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve03"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "dep-d"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve04"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	return g
}

func findSample(samples []metrics.Sample, name string, wantLabels map[string]string) (metrics.Sample, bool) {
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range wantLabels {
			if s.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

func TestSelfDepConcentration_OnlyParentsWith2Plus(t *testing.T) {
	g := gtestGraph(t)
	globs := []string{"dep-*"}
	samples := selfDepConcentrationSamples(g, "journal-evidence", globs)

	// pve03 carries 3 dependency hosts — the concentration (the ticket's killing mutation: count 3).
	if s, ok := findSample(samples, "tg_self_dependency_concentration", map[string]string{"parent": "pve03"}); !ok || s.Value != 3 {
		t.Errorf("want tg_self_dependency_concentration{parent=pve03}=3, got %+v (ok=%v)", s, ok)
	}
	// pve04 carries only 1 — NOT a concentration, so it must NOT emit a concentration series.
	if s, ok := findSample(samples, "tg_self_dependency_concentration", map[string]string{"parent": "pve04"}); ok {
		t.Errorf("pve04 has a single dependency host and must NOT report a concentration; got %+v", s)
	}
	// the capability label rides every concentration series.
	if s, ok := findSample(samples, "tg_self_dependency_concentration", map[string]string{"parent": "pve03"}); ok && s.Labels["capability"] != "journal-evidence" {
		t.Errorf("concentration series missing capability label, got %v", s.Labels)
	}
}

func TestSelfDep_CoveragePairAlwaysEmitted(t *testing.T) {
	g := gtestGraph(t)
	// A window with NO concentration: one glob resolving to one host. The coverage pair must STILL ship so a
	// consumer sees the check ran ("resolved 1, no concentration") rather than an absent series it reads as
	// the exporter being gone.
	samples := selfDepConcentrationSamples(g, "journal-evidence", []string{"dep-a"})
	if s, ok := findSample(samples, "tg_self_dependency_globs_declared", map[string]string{"capability": "journal-evidence"}); !ok || s.Value != 1 {
		t.Errorf("want globs_declared=1, got %+v ok=%v", s, ok)
	}
	if s, ok := findSample(samples, "tg_self_dependency_hosts_resolved", map[string]string{"capability": "journal-evidence"}); !ok || s.Value != 1 {
		t.Errorf("want hosts_resolved=1, got %+v ok=%v", s, ok)
	}
	if _, ok := findSample(samples, "tg_self_dependency_concentration", nil); ok {
		t.Error("a single resolved host must produce NO concentration series (absent = 'no concentration', not 'unmeasured')")
	}
}

func TestHostsResolvedCountsLivePlacementOnly(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))
	// dep-a has a FRESH placement; dep-b's runs_on edge has EXPIRED (an ingest source going quiet — the exact
	// TG-394 failure class). Both still EXIST as graph nodes (Upsert never removes an endpoint), so a node-
	// existence count would report 2 resolved. Only dep-a has a LIVE placement, so hosts_resolved must be 1 —
	// otherwise the coverage number stays high while the concentration silently loses members.
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "dep-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve03"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "dep-b"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve04"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE, ValidUntil: now.Add(-time.Hour)})

	samples := selfDepConcentrationSamples(g, "journal-evidence", []string{"dep-*"})
	if s, ok := findSample(samples, "tg_self_dependency_hosts_resolved", map[string]string{"capability": "journal-evidence"}); !ok || s.Value != 1 {
		t.Errorf("dep-b's placement edge is EXPIRED, so only dep-a has a live placement; want hosts_resolved=1, "+
			"got %+v ok=%v — a stale-but-existing node must NOT count as resolved (the false coverage claim finding 1 caught)", s, ok)
	}
}

func TestSelfDepConcentrationSamples_NilGraphEmitsNothing(t *testing.T) {
	if s := selfDepConcentrationSamples(nil, "journal-evidence", []string{"dep-*"}); len(s) != 0 {
		t.Errorf("a nil graph (no holder) must emit nothing, got %v", s)
	}
	// The wired reader (the MULTI-capability job, the only one a composition root builds) must be
	// nil-safe the same way: a nil holder emits nothing rather than dereferencing.
	if r := startSelfDepConcentrationMultiJob(nil, []selfDepCapability{{Name: "journal-evidence", Globs: []string{"dep-*"}}})(); len(r) != 0 {
		t.Errorf("a nil holder job must emit nothing, got %v", r)
	}
}

func TestResolveDepHosts_MatchesHostEntitiesOnly(t *testing.T) {
	now := time.Now()
	g := estate.NewGraph(estate.WithClock(func() time.Time { return now }))
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeLXC, Name: "dep-a"}, To: estate.Entity{Type: estate.TypePVENode, Name: "pve03"}, Rel: estate.RelRunsOn, Confidence: 0.95, Source: estate.SourcePVE})
	// A SERVICE whose name also matches the glob — it is NOT a host and must not resolve as a dependency host.
	g.Upsert(estate.Edge{From: estate.Entity{Type: estate.TypeService, Name: "dep-service"}, To: estate.Entity{Type: estate.TypeSite, Name: "nl"}, Rel: estate.RelMemberOf, Confidence: 0.90, Source: estate.SourceNetbox})

	hosts := resolveDepHosts(g, []string{"dep-*"})
	if len(hosts) != 1 || hosts[0] != "dep-a" {
		t.Errorf("glob dep-* must resolve ONLY the host dep-a, not the same-named service; got %v", hosts)
	}
}

func TestJournalDepGlobs_DedupsAndDropsEmpty(t *testing.T) {
	// ParseAccess yields Access{Site, HostGlob}; the exporter only needs the (de-duplicated) globs.
	got := journalDepGlobs([]journal.Access{
		{Site: "nl", HostGlob: "dep-b"},
		{Site: "nl", HostGlob: "dep-a"},
		{Site: "gr", HostGlob: "dep-a"}, // same glob, different site → one glob
		{Site: "nl", HostGlob: ""},      // dropped
	})
	if len(got) != 2 || got[0] != "dep-a" || got[1] != "dep-b" {
		t.Errorf("want sorted, de-duplicated, non-empty globs [dep-a dep-b], got %v", got)
	}
}
