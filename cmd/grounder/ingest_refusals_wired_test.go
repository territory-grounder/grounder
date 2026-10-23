package main

import (
	"os"
	"strings"
	"testing"
)

// TG-371. The register existing and the front door FEEDING it are different facts, and so are the
// register existing and /metrics PUBLISHING it. This repo's signature defect is the first without the
// second — a counter that compiles, tallies nothing, and reads as a healthy zero forever.
//
// Source assertions because both seams are wired inside functions with no callable seam: the Deps
// literal in buildPublicAPI and the metrics closure in main. That limitation is the reason these are
// pinned at all rather than left to an integration test that does not exist.

func grounderSource(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	src := strings.Join(out, "\n")
	if len(src) < 3_000 {
		t.Fatalf("VACUITY FLOOR: %s stripped to %d bytes — every assertion below would pass on a stub",
			file, len(src))
	}
	return src
}

func TestTheRefusalCounterIsFedByTheFrontDoor(t *testing.T) {
	src := grounderSource(t, "deps.go")
	if !strings.Contains(src, "IngestRefused:") {
		t.Fatal("httpapi.Deps.IngestRefused is not set in buildPublicAPI. The counter would exist, the " +
			"front door would refuse deliveries exactly as before, and tg_ingest_refused_total would " +
			"never appear — which reads identically to a front door that refuses nothing.")
	}
	if !strings.Contains(src, "ingestRefusalCounter.record") {
		t.Error("IngestRefused is set to something other than the package register's record method — the " +
			"tally and the series would then come from different objects")
	}
}

// TG-371 item 4: the auth layer refuses an ingest push (wrong/absent bearer) BEFORE the handler, so it must
// feed the SAME counter or that refusal — the most likely cause of a silently-refused source — is uncounted
// while the handler counter reads as though the gap were closed.
func TestTheAuthLayerRefusalObserverIsWired(t *testing.T) {
	src := grounderSource(t, "deps.go")
	if !strings.Contains(src, "SetIngestRejectObserver(") {
		t.Fatal("the auth router's ingest-reject observer is not wired in buildPublicAPI. An auth-layer " +
			"refusal (wrong/absent bearer) would then go uncounted — the single most likely cause of a " +
			"silently-refused source — while the handler counter reads as though the gap were closed (TG-371 item 4).")
	}
	if !strings.Contains(src, "SetIngestRejectObserver(ingestRefusalCounter.record)") {
		t.Error("the auth observer is wired to something other than the register the handler feeds — auth and " +
			"handler refusals would split across two objects and neither would be the whole tg_ingest_refused_total")
	}
}

func TestTheRefusalCounterIsPublishedOnMetrics(t *testing.T) {
	src := grounderSource(t, "main.go")
	if !strings.Contains(src, "ingestRefusalCounter.samples()") {
		t.Fatal("the /metrics closure does not append ingestRefusalCounter.samples(). The front door " +
			"would tally refusals into a map nothing scrapes — strictly worse than not counting, because " +
			"the code reads as though the gap were closed.")
	}
}

// TestTheRegisterEmitsNothingUntilARefusalHappens. A counter family that materialises one zero series
// per (source × reason) would multiply the whole vocabulary against every source that ever registered,
// and — worse — make "no refusals" and "refusals being counted" look the same on a dashboard.
func TestTheRegisterEmitsNothingUntilARefusalHappens(t *testing.T) {
	c := newIngestRefusals()
	if got := c.samples(); len(got) != 0 {
		t.Fatalf("a register with no refusals emitted %d series, want 0 — the PRESENCE of a series is "+
			"the signal here", len(got))
	}
	c.record("librenms", "payload_rejected")
	c.record("librenms", "payload_rejected")
	c.record("prometheus-alertmanager", "unknown_source")
	got := c.samples()
	if len(got) != 2 {
		t.Fatalf("got %d series, want 2 (one per source×reason)", len(got))
	}
	// Sorted for a byte-stable scrape, and the repeated refusal must ACCUMULATE rather than overwrite.
	if got[0].Labels["source_type"] != "librenms" || got[0].Value != 2 {
		t.Errorf("first series = %v value %v, want librenms/payload_rejected at 2", got[0].Labels, got[0].Value)
	}
	if got[1].Labels["source_type"] != "prometheus-alertmanager" || got[1].Value != 1 {
		t.Errorf("second series = %v value %v", got[1].Labels, got[1].Value)
	}
}

// TestARefusalWithNoSourceIsStillCounted. The earliest rejection can fire before a source is resolvable;
// dropping it would reintroduce the silence at the least-understood point.
func TestARefusalWithNoSourceIsStillCounted(t *testing.T) {
	c := newIngestRefusals()
	c.record("", "unreadable_body")
	got := c.samples()
	if len(got) != 1 {
		t.Fatalf("a sourceless refusal produced %d series, want 1", len(got))
	}
	if got[0].Labels["source_type"] != "unknown" {
		t.Errorf("source_type = %q, want \"unknown\" — an empty label value is silently dropped by some "+
			"scrapers, which would hide exactly the refusals that happen before the source is known",
			got[0].Labels["source_type"])
	}
}
