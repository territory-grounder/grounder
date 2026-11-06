package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// A1'S TIME HALF: WHICH SOURCE FOUND THE FAULT FIRST, AND HOW LONG IT TOOK.
//
// A1 is a recall number — did an alert arrive inside the window, yes or no. Two detectors that both answer
// "yes" are indistinguishable in it, so TG's own pve-liveness detector (~39s) has been scored identically to
// the LibreNMS push path (~11min) it exists to beat. The AxisAgg doc said as much: "no surface reported time
// at all, not even the proven ~39s-vs-~11min A1 detection win". The evidence for that win lived in a note,
// not in anything the system computed.
//
// THE FIXTURE IS THE CASE THAT MATTERS: one host, one injected fault, and TWO sources that both eventually
// alert on it. A correct implementation credits the FIRST one and only the first one. A plain
// min(received_at) would produce the right latency and lose the identity of the detector, which is the entire
// point of the measurement; a naive join would credit both sources for one detection, inflating the slow
// path's numbers with faults it did not find.
//
// Gated on TG_TEST_POSTGRES_DSN (CI provides it).
func TestDetectionLatencyCreditsTheFirstSourceOnly(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the detection-latency test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("dl-%d", os.Getpid())
	hostA, hostB := uniq+"-hosta", uniq+"-hostb"
	since := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	inj := since.Add(time.Hour)

	defer func() {
		for _, h := range []string{hostA, hostB} {
			for _, q := range []string{
				`DELETE FROM ingest_alert WHERE host = $1`,
				`DELETE FROM injected_fault WHERE host = $1`,
			} {
				if _, err := p.Exec(ctx, q, h); err != nil {
					t.Errorf("cleanup %s %s: %v", h, q, err)
				}
			}
		}
	}()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	fault := func(host string, at time.Time) {
		mustExec(`INSERT INTO injected_fault (host, fault_type, injected_at) VALUES ($1,'device-down',$2)`, host, at)
	}
	alert := func(host, source string, at time.Time) {
		mustExec(`INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, received_at)
		          VALUES ($1,$2,$2,'Device-Down','critical',$3,'dc1','seeded',$4)`,
			fmt.Sprintf("%s-%s-%d", uniq, source, at.Unix()), source, host, at)
	}

	// hostA: BOTH sources alert. pve-liveness at +40s, librenms at +11min. Only pve-liveness detected it.
	fault(hostA, inj)
	alert(hostA, "pve-liveness", inj.Add(40*time.Second))
	alert(hostA, "librenms", inj.Add(11*time.Minute))

	// hostB: ONLY librenms alerts, at +10min. This is a genuine librenms first-detection.
	fault(hostB, inj)
	alert(hostB, "librenms", inj.Add(10*time.Minute))

	agg, err := NewAxisReadStore(p).Aggregate(ctx, since)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	got := map[string]SourceLatency{}
	for _, l := range agg.DetectionLatency {
		if l.Source == "pve-liveness" || l.Source == "librenms" {
			got[l.Source] = l
		}
	}

	pve, okPve := got["pve-liveness"]
	if !okPve {
		t.Fatalf("pve-liveness produced no latency row at all — it detected hostA 10 minutes before librenms " +
			"and the measurement cannot see it, which is the whole defect")
	}
	if pve.Detections != 1 {
		t.Errorf("pve-liveness credited %d first-detections, want 1", pve.Detections)
	}
	if pve.MedianSec != 40 {
		t.Errorf("pve-liveness median %ds, want 40 — the latency must be measured from INJECTION, not from "+
			"the window start or the other source's alert", pve.MedianSec)
	}

	lnms, okLnms := got["librenms"]
	if !okLnms {
		t.Fatalf("librenms produced no latency row — it genuinely detected hostB first and must be credited")
	}
	// THE LOAD-BEARING ASSERTION. librenms alerted on BOTH hosts, but was FIRST on only one. Crediting it
	// with 2 would mean a slow source is scored for faults another source had already found — which reads as
	// "librenms detects everything" and erases the reason the fast detector exists.
	if lnms.Detections != 1 {
		t.Errorf("librenms credited %d first-detections, want 1. It alerted on both hosts but was only FIRST "+
			"on hostB; counting the hostA alert too would score a source for a fault pve-liveness had already "+
			"detected 10 minutes earlier", lnms.Detections)
	}
	if lnms.MedianSec != 600 {
		t.Errorf("librenms median %ds, want 600 (its hostB detection only)", lnms.MedianSec)
	}

	// Ordering is part of the contract — the scorer prints this list as-is, fastest first.
	if len(agg.DetectionLatency) >= 2 {
		for i := 1; i < len(agg.DetectionLatency); i++ {
			if agg.DetectionLatency[i-1].MedianSec > agg.DetectionLatency[i].MedianSec {
				t.Errorf("latency rows are not sorted fastest-median-first: %v", agg.DetectionLatency)
				break
			}
		}
	}
}
