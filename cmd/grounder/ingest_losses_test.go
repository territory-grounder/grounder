package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

func TestTheAlertLossRegisterCountsAlertsInAClosedVocabulary(t *testing.T) {
	c := newIngestAlertLosses()
	if got := c.samples(); len(got) != 0 {
		t.Fatalf("no series until an alert is lost, got %v", got)
	}
	c.add("prometheus-alertmanager", lossAlertRejected, 3)
	c.add("prometheus-alertmanager", lossSenderTruncated, 7)
	c.add("prometheus-alertmanager", lossAlertRejected, 0)
	c.add("prometheus-alertmanager", lossAlertRejected, -2)
	c.add("crowdsec", "malformed startsAt \"%Y\"", 1)
	got := map[string]float64{}
	for _, s := range c.samples() {
		if s.Name != "tg_ingest_alerts_lost_total" {
			t.Fatalf("unexpected family %q", s.Name)
		}
		got[s.Labels["source_type"]+"/"+s.Labels["reason"]] = s.Value
	}
	want := map[string]float64{"prometheus-alertmanager/alert_rejected": 3, "prometheus-alertmanager/sender_truncated": 7, "crowdsec/other": 1}
	if len(got) != len(want) {
		t.Fatalf("series %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

func TestTheAlertLossCounterIsFedAndPublished(t *testing.T) {
	if src := grounderSource(t, "deps.go"); !strings.Contains(src, "IngestAlertsLost:         ingestAlertLossCounter.add") {
		t.Fatal("httpapi.Deps.IngestAlertsLost is not wired to ingestAlertLossCounter.add in buildPublicAPI: the front " +
			"door would count lost alerts into nothing, and tg_ingest_alerts_lost_total would read as a quiet zero")
	}
	if src := grounderSource(t, "main.go"); !strings.Contains(src, "ingestAlertLossCounter.samples()") {
		t.Fatal("the /metrics closure does not publish ingestAlertLossCounter.samples(): the tally would exist and " +
			"never be scraped, so AlertsLostAtIngest could never fire")
	}
}

type lossSeedIngester struct{ src string }

func (f lossSeedIngester) SourceType() string { return f.src }
func (f lossSeedIngester) Normalize(context.Context, []byte) (coreingest.IncidentEnvelope, error) {
	return coreingest.IncidentEnvelope{}, nil
}

type lossSeedBatchIngester struct{ lossSeedIngester }

func (f lossSeedBatchIngester) NormalizeBatch(context.Context, []byte) (coreingest.Batch, error) {
	return coreingest.Batch{}, nil
}

type lossSeedResolver map[string]ingestadapter.Ingester

func (r lossSeedResolver) ResolveIngester(st string) (ingestadapter.Ingester, error) {
	if ing, ok := r[st]; ok {
		return ing, nil
	}
	return nil, errors.New("no execution path")
}

func TestLostAlertSeriesStartAtZeroForEachRegisteredBatchSource(t *testing.T) {
	c := newIngestAlertLosses()
	c.seedBatchSources(lossSeedResolver{
		"prometheus-alertmanager": lossSeedBatchIngester{lossSeedIngester{"prometheus-alertmanager"}},
		"librenms":                lossSeedIngester{"librenms"},
	}, "prometheus-alertmanager", "librenms", "crowdsec")
	c.add("prometheus-alertmanager", lossAlertRejected, 2)
	got := map[string]float64{}
	for _, s := range c.samples() {
		got[s.Labels["source_type"]+"/"+s.Labels["reason"]] = s.Value
	}
	want := map[string]float64{"prometheus-alertmanager/alert_rejected": 2, "prometheus-alertmanager/sender_truncated": 0}
	if len(got) != len(want) {
		t.Fatalf("only a registered batch source is seeded, got %v", got)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Fatalf("%s = %v (present %v), want %v; all %v", k, g, ok, v, got)
		}
	}
}

func TestTheLostAlertSeriesAreSeededAtBoot(t *testing.T) {
	if src := grounderSource(t, "deps.go"); !strings.Contains(src, "ingestAlertLossCounter.seedBatchSources(ingesters, alertmanager.SourceType, crowdsec.SourceType)") {
		t.Fatal("buildPublicAPI does not seed the lost-alert series: the first loss after a restart would appear at 1, " +
			"increase() would read 0 and AlertsLostAtIngest would not fire")
	}
}
