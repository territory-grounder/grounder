package prometheus

import (
	"context"
	"strings"
	"testing"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// fixedClock pins the wall clock so the staleness-window check is deterministic — the module's only
// injection seam (there is no outbound HTTP, Doer, or secret: Prometheus pulls the exposition, so the
// "request" a scrape sees is the Gather output, and that is what these tests drive and assert).
func fixedClock(t time.Time) Option { return WithClock(func() time.Time { return t }) }

// staleGauges indexes the "<name>_stale" gauges emitted by Gather by their per-series-identity labels, so a
// test can assert the exact gauge for a given (metric, connector) rather than the bare name.
func staleGauges(samples []observability.Sample) map[string]observability.Sample {
	out := make(map[string]observability.Sample)
	for _, s := range samples {
		if !strings.HasSuffix(s.Name, "_stale") {
			continue
		}
		key := s.Name
		if c, ok := s.Labels["connector"]; ok {
			key += "|connector=" + c
		}
		if c, ok := s.Labels["component"]; ok {
			key += "|component=" + c
		}
		out[key] = s
	}
	return out
}

// TestGatherStalenessIsPerSeriesIdentity is the regression lock for the fixed bug: staleness must be
// computed per distinct series identity (name + labels), NOT per bare metric name. Two "tg_connector_up"
// series share the metric name but carry different "connector" labels; one writer is fresh and one is dead
// (its newest sample predates the window). The dead writer MUST surface its own stale gauge == 1. Under the
// name-only bug the live sibling's fresh stamp masked the dead one, so a single tg_connector_up_stale read
// 0 (healthy) and the dead writer never paged.
func TestGatherStalenessIsPerSeriesIdentity(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC)
	m := New(fixedClock(now), WithStalenessWindow(2*time.Minute))

	// netbox connector: fresh (no Stamped -> Export stamps it at now, age 0).
	// snmp connector: dead — last sample is 10m old, well past the 2m window, though its value is still 1
	// (a dead writer's last value looks healthy; only freshness reveals it — INV-15).
	if err := m.Export(context.Background(), []observability.Sample{
		{Name: "tg_connector_up", Value: 1, Labels: map[string]string{"connector": "netbox"}},
		{Name: "tg_connector_up", Value: 1, Stamped: now.Add(-10 * time.Minute), Labels: map[string]string{"connector": "snmp"}},
		{Name: "tg_control_plane_up", Value: 1, Labels: map[string]string{"component": "grounder"}},
	}); err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}

	got := m.Gather()
	gauges := staleGauges(got)

	// There must be a DISTINCT stale gauge per connector identity — the fix's whole point.
	dead, ok := gauges["tg_connector_up_stale|connector=snmp"]
	if !ok {
		t.Fatalf("dead per-connector writer must get its own stale gauge; gauges=%v", keysOf(gauges))
	}
	if dead.Value != 1 {
		t.Errorf("dead snmp writer stale gauge must be 1, got %v (name-only grouping would mask it as 0)", dead.Value)
	}
	if dead.Labels["connector"] != "snmp" {
		t.Errorf("dead stale gauge must carry the distinguishing connector label, got %v", dead.Labels)
	}

	live, ok := gauges["tg_connector_up_stale|connector=netbox"]
	if !ok {
		t.Fatalf("live per-connector writer must get its own stale gauge; gauges=%v", keysOf(gauges))
	}
	if live.Value != 0 {
		t.Errorf("fresh netbox writer stale gauge must be 0, got %v", live.Value)
	}

	// The two tg_connector_up_stale gauges must be genuinely distinct series (name + labels), else the
	// exposition would carry a duplicate series and the dead one could not page independently.
	if dead.Labels["connector"] == live.Labels["connector"] {
		t.Fatal("the two tg_connector_up_stale gauges must differ by the connector label")
	}
}

// TestExportStampsFreshnessAndGatherPreservesIt locks INV-15: a sample exported without a Stamped value is
// stamped from the clock (never exposed unstamped), an already-stamped sample keeps its own freshness, and
// every non-stale series returned by Gather carries a freshness timestamp.
func TestExportStampsFreshnessAndGatherPreservesIt(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC)
	m := New(fixedClock(now))

	prior := now.Add(-30 * time.Second)
	if err := m.Export(context.Background(), []observability.Sample{
		{Name: "tg_control_plane_up", Value: 1, Labels: map[string]string{"component": "grounder"}}, // unstamped
		{Name: "tg_connector_up", Value: 1, Stamped: prior, Labels: map[string]string{"connector": "netbox"}},
	}); err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}

	var sawStale bool
	for _, s := range m.Gather() {
		if strings.HasSuffix(s.Name, "_stale") {
			sawStale = true
			if s.Stamped != now {
				t.Errorf("staleness gauge %q must be stamped at scrape time, got %v", s.Name, s.Stamped)
			}
			continue
		}
		if s.Stamped.IsZero() {
			t.Errorf("no series may be exposed without a freshness timestamp (INV-15): %q", s.Name)
		}
		if s.Name == "tg_control_plane_up" && s.Stamped != now {
			t.Errorf("an unstamped sample must be stamped from the clock, got %v want %v", s.Stamped, now)
		}
		if s.Name == "tg_connector_up" && s.Stamped != prior {
			t.Errorf("an already-stamped sample must keep its own freshness, got %v want %v", s.Stamped, prior)
		}
	}
	if !sawStale {
		t.Fatal("a <name>_stale gauge must exist so a dead writer pages")
	}
}

// TestSeriesIdentityDistinguishesByLabels verifies the identity key that the fix is built on: same name +
// different labels are distinct identities; same name + same labels collapse to one.
func TestSeriesIdentityDistinguishesByLabels(t *testing.T) {
	a := seriesIdentity("tg_connector_up", map[string]string{"connector": "netbox"})
	b := seriesIdentity("tg_connector_up", map[string]string{"connector": "snmp"})
	if a == b {
		t.Errorf("same name + different labels must be distinct identities: %q == %q", a, b)
	}
	c := seriesIdentity("tg_connector_up", map[string]string{"connector": "netbox"})
	if a != c {
		t.Errorf("same name + same labels must share identity: %q != %q", a, c)
	}
	if seriesIdentity("tg_up", nil) == seriesIdentity("tg_up", map[string]string{"x": "y"}) {
		t.Error("a labelled series must not share identity with the bare-name series")
	}
}

// TestSourceTypeSlug pins the vendor slug the registry keys on.
func TestSourceTypeSlug(t *testing.T) {
	if got := New().SourceType(); got != "prometheus" {
		t.Errorf("SourceType() = %q, want prometheus", got)
	}
	// compile-time interface satisfaction is enforced by the package's var _ observability.Exporter guard.
	var _ observability.Exporter = New()
}

func keysOf(m map[string]observability.Sample) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
