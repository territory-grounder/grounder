package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// TG-405. safety.HighRiskCategory forces a POLL_PAUSE for {maintenance, security-incident, deployment},
// read from env.Labels["category"]. The Alertmanager module passes every label through raw, and the estate
// uses that same key for SUBSYSTEMS. Measured over all 3,165 ingest_alert rows: 39 carried a category and
// ZERO were high-risk — the driver has never been reachable, and nothing said so because "no alert was
// high-risk" and "the driver cannot see a high-risk value" are the same quiet zero.

type fakeCategoryReader struct {
	values []db.CategoryCount
	totals map[string]int64
	err    error
}

func (f *fakeCategoryReader) CountCategoryValues(context.Context) ([]db.CategoryCount, map[string]int64, error) {
	return f.values, f.totals, f.err
}

func catSample(ss []metrics.Sample, name, src string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name && (src == "" || s.Labels["source_id"] == src) {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// TestTheProductionCollisionIsVisible reproduces the live shape: categories present, none high-risk.
func TestTheProductionCollisionIsVisible(t *testing.T) {
	f := &fakeCategoryReader{
		values: []db.CategoryCount{
			{SourceID: "prometheus-alertmanager", Category: "mesh-bgp", Count: 9},
			{SourceID: "prometheus-alertmanager", Category: "storage-write-path", Count: 4},
		},
		totals: map[string]int64{"prometheus-alertmanager": 168, "librenms-dc1": 2787},
	}
	ss := startCategoryCoverageJob(context.Background(), f, time.Hour)()

	present, ok := catSample(ss, "tg_ingest_category_present", "prometheus-alertmanager")
	if !ok || present.Value != 13 {
		t.Fatalf("category_present = %v (found=%v), want 13", present.Value, ok)
	}
	hr, ok := catSample(ss, "tg_ingest_category_high_risk", "prometheus-alertmanager")
	if !ok || hr.Value != 0 {
		t.Fatalf("high_risk = %v (found=%v), want 0 — mesh-bgp and storage-write-path are not in the "+
			"closed set", hr.Value, ok)
	}
	un, ok := catSample(ss, "tg_ingest_category_unrecognised", "")
	if !ok {
		t.Fatal("tg_ingest_category_unrecognised is not published — the collision has no series at all")
	}
	if un.Value != 13 {
		t.Errorf("unrecognised = %v, want 13. This is the number that distinguishes \"nothing was "+
			"high-risk\" from \"the driver cannot see a high-risk value\".", un.Value)
	}
}

// TestASourceWithNoCategoryIsStillCounted. librenms sets no labels at all; its denominator must still be
// published, or a source that cannot reach the driver looks identical to one that has no alerts.
func TestASourceWithNoCategoryIsStillCounted(t *testing.T) {
	f := &fakeCategoryReader{
		values: nil,
		totals: map[string]int64{"librenms-dc1": 2787},
	}
	ss := startCategoryCoverageJob(context.Background(), f, time.Hour)()

	tot, ok := catSample(ss, "tg_ingest_alerts_total_by_source", "librenms-dc1")
	if !ok || tot.Value != 2787 {
		t.Fatalf("denominator = %v (found=%v), want 2787 — without it, 0 categories on 2787 alerts is "+
			"indistinguishable from 0 categories on 0 alerts", tot.Value, ok)
	}
	if p, ok := catSample(ss, "tg_ingest_category_present", "librenms-dc1"); !ok || p.Value != 0 {
		t.Errorf("category_present = %v (found=%v), want an explicit 0", p.Value, ok)
	}
}

// TestARecognisedCategoryCountsAsReachable is the positive control. Without it, a job that reported
// everything as unrecognised would pass every assertion above.
func TestARecognisedCategoryCountsAsReachable(t *testing.T) {
	f := &fakeCategoryReader{
		values: []db.CategoryCount{{SourceID: "crowdsec", Category: "security-incident", Count: 5}},
		totals: map[string]int64{"crowdsec": 5},
	}
	ss := startCategoryCoverageJob(context.Background(), f, time.Hour)()

	hr, ok := catSample(ss, "tg_ingest_category_high_risk", "crowdsec")
	if !ok || hr.Value != 5 {
		t.Fatalf("high_risk = %v (found=%v), want 5 — security-incident IS in the closed set, and a "+
			"register that cannot see a recognised value would report the collision everywhere", hr.Value, ok)
	}
	if un, _ := catSample(ss, "tg_ingest_category_unrecognised", ""); un.Value != 0 {
		t.Errorf("unrecognised = %v, want 0", un.Value)
	}
}

func TestATransientErrorKeepsTheReading(t *testing.T) {
	failing := &fakeCategoryReader{err: errors.New("connection refused")}
	if ss := startCategoryCoverageJob(context.Background(), failing, time.Hour)(); len(ss) != 0 {
		t.Errorf("a reader whose FIRST read fails published %d sample(s) — it has never seen the database, "+
			"so it must publish nothing rather than a fabricated clean reading", len(ss))
	}
}

func TestANilStorePublishesNothingHere(t *testing.T) {
	if ss := startCategoryCoverageJob(context.Background(), nil, time.Hour)(); len(ss) != 0 {
		t.Errorf("a nil store published %d sample(s)", len(ss))
	}
}

// TestTheCoverageRegisterIsWired — guarding the job is not guarding the wiring.
func TestTheCoverageRegisterIsWired(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))
	for _, want := range []string{"startCategoryCoverageJob(", "withCategoryCoverage(", "categoryCoverageStoreOrNil("} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go does not call %s — the register would be computed and published by nothing", want)
		}
	}
}
