package ingest

import (
	"context"
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func goodRaw() RawEvent {
	r := NewRawEvent("prometheus-dc1", []byte(`{"provider":"payload"}`))
	r.ExternalRef = "TG-4617"
	r.AlertRule = "MeshBFDSessionDown"
	r.Severity = "warning"
	r.Host = "nl-frr01"
	r.IP = "192.168.192.3"
	r.Site = "dc1"
	r.Summary = "BFD session down"
	r.ObservedAt = testNow
	return r
}

func TestNormalizeAcceptsAValidEvent(t *testing.T) {
	e, err := Normalize(goodRaw(), testNow)
	if err != nil {
		t.Fatalf("valid event must normalize, got %v", err)
	}
	if e.ExternalRef != "TG-4617" || e.Severity != SeverityWarning || e.Host != "nl-frr01" {
		t.Fatalf("unexpected envelope: %+v", e)
	}
	if e.IP == nil || e.IP.String() != "192.168.192.3" {
		t.Fatalf("IP not parsed: %v", e.IP)
	}
}

func TestNormalizeRejectsMalformedFields(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*RawEvent)
		wants error
	}{
		{"missing external_ref", func(r *RawEvent) { r.ExternalRef = "" }, ErrMissingField},
		{"missing alert_rule", func(r *RawEvent) { r.AlertRule = "" }, ErrMissingField},
		{"non-enum severity", func(r *RawEvent) { r.Severity = "sev-9000" }, ErrBadSeverity},
		{"malformed hostname", func(r *RawEvent) { r.Host = "not a host!" }, ErrBadHostname},
		{"malformed IP", func(r *RawEvent) { r.IP = "999.1.1.1" }, ErrBadIP},
		{"metachar external_ref", func(r *RawEvent) { r.ExternalRef = "TG-1; DROP TABLE" }, ErrBadExternalRef},
		{"future-dated", func(r *RawEvent) { r.ObservedAt = testNow.Add(time.Hour) }, ErrBadTimestamp},
		{"ancient", func(r *RawEvent) { r.ObservedAt = testNow.Add(-365 * 24 * time.Hour) }, ErrBadTimestamp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := goodRaw()
			c.mut(&r)
			_, err := Normalize(r, testNow)
			if !errors.Is(err, c.wants) {
				t.Fatalf("want %v, got %v", c.wants, err)
			}
		})
	}
}

// mkEnvelope builds a normalized envelope for pipeline tests.
func mkEnvelope(t *testing.T, ref, rule, host, sev string, observed time.Time) IncidentEnvelope {
	t.Helper()
	r := NewRawEvent("src", nil)
	r.ExternalRef = ref
	r.AlertRule = rule
	r.Host = host
	r.Severity = sev
	r.ObservedAt = observed
	e, err := Normalize(r, testNow)
	if err != nil {
		t.Fatalf("mkEnvelope: %v", err)
	}
	return e
}

func TestPipelineDedupCollapsesRepeats(t *testing.T) {
	// three identical fires of one alert within the window
	e := mkEnvelope(t, "TG-1", "DiskFull", "web01", "warning", testNow)
	batch := []IncidentEnvelope{e, e, e}
	res := NewPipeline().Process(batch, testNow)

	dupes := 0
	for _, d := range res.Decisions {
		if d.Duplicate {
			dupes++
		}
	}
	if dupes != 2 {
		t.Fatalf("expected 2 of 3 identical alerts marked duplicate, got %d", dupes)
	}
	// ≥3 fires of the same key ⇒ flapping
	if !res.Decisions[0].Flapping {
		t.Fatalf("3 fires of one key should mark it flapping")
	}
}

func TestPipelineChainRunsBeforePublish(t *testing.T) {
	e1 := mkEnvelope(t, "TG-1", "DiskFull", "web01", "warning", testNow)
	e2 := mkEnvelope(t, "TG-2", "MemHigh", "web02", "critical", testNow)
	res := NewPipeline().Process([]IncidentEnvelope{e1, e2, e1}, testNow)

	want := []string{StageDedup, StageFlap, StageBurst, StageCorrelate}
	if len(res.Order) != len(want) {
		t.Fatalf("stage order %v != %v", res.Order, want)
	}
	for i := range want {
		if res.Order[i] != want[i] {
			t.Fatalf("stage %d = %q, want %q", i, res.Order[i], want[i])
		}
	}

	pub := &RecordingPublisher{}
	n, err := PublishTriage(context.Background(), pub, res, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// two distinct correlation groups, the duplicate e1 collapsed
	if n != 2 || len(pub.Events) != 2 {
		t.Fatalf("expected 2 published events, got n=%d recorded=%d", n, len(pub.Events))
	}
	for _, ev := range pub.Events {
		if ev.ExternalRef == "" || ev.ExternalRef != ev.Envelope.ExternalRef {
			t.Fatalf("event not keyed by external_ref: %+v", ev)
		}
	}
}

func TestPipelineBurstDetection(t *testing.T) {
	var batch []IncidentEnvelope
	for i, host := range []string{"a", "b", "c", "d", "e", "f"} {
		batch = append(batch, mkEnvelope(t, "TG-"+host, "R", host+"01", "warning", testNow.Add(time.Duration(i)*time.Second)))
	}
	res := NewPipeline().Process(batch, testNow)
	if !res.Decisions[0].InBurst {
		t.Fatalf("6 distinct incidents should trip the burst threshold")
	}
}

// The burst threshold is 3 (predecessor BURST_THRESHOLD; ARCHITECTURE.md "3+ hosts"): a 3-host correlated
// burst (e.g. Service up/down on pve01/02/03) must trip, and a 2-host coincidence must not.
func TestPipelineBurstThresholdIsThree(t *testing.T) {
	three := []IncidentEnvelope{
		mkEnvelope(t, "TG-a", "ServiceDown", "dc1pve01", "warning", testNow),
		mkEnvelope(t, "TG-b", "ServiceDown", "dc1pve02", "warning", testNow.Add(time.Second)),
		mkEnvelope(t, "TG-c", "ServiceDown", "dc1pve03", "warning", testNow.Add(2*time.Second)),
	}
	if res := NewPipeline().Process(three, testNow); !res.Decisions[0].InBurst {
		t.Fatalf("3 distinct correlated incidents must trip the burst threshold")
	}
	two := three[:2]
	if res := NewPipeline().Process(two, testNow); res.Decisions[0].InBurst {
		t.Fatalf("2 distinct incidents must NOT trip the burst threshold")
	}
}

// Flap is windowed: 3 re-deliveries of ONE alert clustered within flapWindow flap, but the SAME 3 spread wider
// than the window (a poll/backfill spanning >15m) must NOT be flagged flapping — the confirmed false positive.
func TestPipelineFlapIsWindowScoped(t *testing.T) {
	// clustered within 15m → flapping
	e := mkEnvelope(t, "TG-1", "DiskFull", "web01", "warning", testNow)
	clustered := []IncidentEnvelope{e, e, e}
	if res := NewPipeline().Process(clustered, testNow); !res.Decisions[0].Flapping {
		t.Fatalf("3 fires clustered within flapWindow must be flapping")
	}
	// same key, re-delivered at T-80m, T-40m, T (an 80-min spread) → no 3 within any 15m window → NOT flapping
	spread := []IncidentEnvelope{
		mkEnvelope(t, "TG-1", "DiskFull", "web01", "warning", testNow.Add(-80*time.Minute)),
		mkEnvelope(t, "TG-1", "DiskFull", "web01", "warning", testNow.Add(-40*time.Minute)),
		mkEnvelope(t, "TG-1", "DiskFull", "web01", "warning", testNow),
	}
	res := NewPipeline().Process(spread, testNow)
	for i, d := range res.Decisions {
		if d.Flapping {
			t.Fatalf("re-deliveries spread beyond flapWindow must NOT be flapping (decision %d): %+v", i, d)
		}
	}
}
