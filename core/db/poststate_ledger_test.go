package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// TG-625: the actuation plane's post-state/clear reader built from TG's own ledger. Each oracle has a seed
// whose absence or inversion flips it.

func seedLedgerObserver(ctx context.Context, t *testing.T, p *Pool, asOf time.Time) func() {
	t.Helper()
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host LIKE 'plo-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert_occurrence WHERE host LIKE 'plo-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE host LIKE 'plo-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE external_ref = 'plo--OmoikaneClusterScoped'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert_occurrence WHERE external_ref = 'plo--OmoikaneClusterScoped'`)
	}
	cleanup()
	raise := func(host, rule string, at time.Time) {
		ref := "plo-" + host + "-" + rule
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,'prometheus-alertmanager','am',$2,'warning',$3,$4,$4) ON CONFLICT (external_ref) DO NOTHING`,
			ref, rule, host, at); err != nil {
			t.Fatalf("seed raise %s: %v", ref, err)
		}
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert_occurrence (external_ref, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,$2,'warning',$3,$4,$4)`, ref, rule, host, at); err != nil {
			t.Fatalf("seed occurrence %s: %v", ref, err)
		}
	}
	recovery := func(host, rule string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_transition (external_ref, kind, host, alert_rule, received_at)
			VALUES ($1,'recovery',$2,$3,$4)`, "plo-r-"+host+"-"+rule+at.Format("150405"), host, rule, at); err != nil {
			t.Fatalf("seed recovery %s: %v", host, err)
		}
	}
	raise("plo-open", "PVEMemoryPressureHigh", asOf.Add(-10*time.Minute))
	// The TG-569 shape: first raised weeks ago, still re-firing a minute ago → OPEN.
	raise("plo-refiring", "NodeTextfileExporterStale", asOf.Add(-20*24*time.Hour))
	raise("plo-refiring", "NodeTextfileExporterStale", asOf.Add(-time.Minute))
	raise("", "OmoikaneClusterScoped", asOf.Add(-2*time.Minute)) // a host-less Alertmanager rule (live shape)
	raise("plo-cleared", "Device-Down", asOf.Add(-30*time.Minute))
	recovery("plo-cleared", "Device-Down", asOf.Add(-5*time.Minute))
	return cleanup
}

func ledgerObserverAt(p *Pool, now time.Time) *LedgerObserver {
	o := NewLedgerObserver(p, ingest.MaxOpenIncident, time.Hour)
	o.now = func() time.Time { return now }
	return o
}

func TestLedgerObserverReportsTheOpenEstate(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	asOf := time.Now().UTC().Add(-time.Hour)
	defer seedLedgerObserver(ctx, t, p, asOf)()

	got, ok := ledgerObserverAt(p, asOf).Observe(ctx, "plo-open", "")
	if !ok {
		t.Fatal("a successful read with a live front door must report ok=true")
	}
	open := map[string]bool{}
	for _, a := range got {
		open[a.Host+"|"+a.Rule] = true
	}
	if !open["plo-open|PVEMemoryPressureHigh"] {
		t.Error("an Alertmanager incident raised before now and never recovered must be observed open — the LibreNMS reader never saw am-* at all")
	}
	if !open["plo-refiring|NodeTextfileExporterStale"] {
		t.Error("TG-569: an incident first raised weeks ago that is still re-firing must be observed open")
	}
	for _, a := range got {
		if a.Host == "" {
			t.Errorf("a host-less pair (%s) must never be observed — it names no host a verdict or necessity check could match", a.Rule)
		}
	}
	if open["plo-cleared|Device-Down"] {
		t.Error("a recovered incident must not be observed open — a stale 'still faulted' licenses an unnecessary mutation")
	}
}

// The clear/necessity reader answers at HOST granularity, from the same set the TG-454 fallback reads.
func TestLedgerObserverOpenHosts(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	asOf := time.Now().UTC().Add(-time.Hour)
	defer seedLedgerObserver(ctx, t, p, asOf)()
	o := ledgerObserverAt(p, asOf)
	got, ok := o.ObserveOpenHosts(ctx, "plo-open", "")
	if !ok {
		t.Fatal("a live, successful read must report ok=true")
	}
	hosts := map[string]bool{}
	for _, a := range got {
		hosts[a.Host] = true
		if a.Rule != "" {
			t.Errorf("the host reader carries no rule, got %q on %s", a.Rule, a.Host)
		}
	}
	if !hosts["plo-open"] || !hosts["plo-refiring"] || hosts["plo-cleared"] {
		t.Fatalf("open hosts must be plo-open + plo-refiring and never the recovered plo-cleared, got %v", hosts)
	}
	if _, ok := ledgerObserverAt(p, time.Now().UTC().Add(10*24*time.Hour)).ObserveOpenHosts(ctx, "plo-open", ""); ok {
		t.Fatal("a silent front door must make the host reader ok=false too")
	}
}

func TestLedgerObserverSilentFrontDoorIsUnobservable(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	asOf := time.Now().UTC().Add(-time.Hour)
	defer seedLedgerObserver(ctx, t, p, asOf)()

	// Ten days after the newest delivery any test (or the seed) wrote: the front door has been silent far past
	// the 1h bound, so an empty or partial open-set cannot be told apart from a blind one.
	got, ok := ledgerObserverAt(p, time.Now().UTC().Add(10*24*time.Hour)).Observe(ctx, "plo-open", "")
	if ok {
		t.Fatalf("a silent front door must report ok=false, never a quiet estate (got %d alerts, ok=true)", len(got))
	}
	if got != nil {
		t.Fatalf("an unobservable read contributes nothing, got %d alerts", len(got))
	}
}

func TestLedgerObserverReadErrorIsUnobservable(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, ok := ledgerObserverAt(p, time.Now().UTC()).Observe(canceled, "h", ""); ok || got != nil {
		t.Fatalf("a failed read must be (nil,false), never (empty,true): got %v ok=%v", got, ok)
	}
	var nilObs *LedgerObserver
	if _, ok := nilObs.Observe(ctx, "h", ""); ok {
		t.Fatal("a nil observer must report ok=false")
	}
}

// TG-337 must survive TG-625: the actuation plane never reads alert BODIES. Both SQL texts this reader runs are
// pinned to name no content column.
func TestLedgerObserverNeverReadsAlertBodies(t *testing.T) {
	for _, f := range []string{"poststate_ledger.go", "alert_history.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var code []string // comments may name a forbidden column to explain the ban; only code is pinned
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "//") {
				code = append(code, l)
			}
		}
		src := strings.Join(code, "\n")
		if f == "alert_history.go" {
			i := strings.Index(src, "func (s *AlertHistoryStore) OpenIncidentPairs(")
			if i < 0 {
				t.Fatal("OpenIncidentPairs not found — the pin lost its subject")
			}
			src = src[i:]
			if j := strings.Index(src[1:], "\nfunc "); j > 0 {
				src = src[:j+1]
			}
		}
		for _, banned := range []string{"summary", "labels_json", "delivery_peer", "delivery_host"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s: the actuation-plane reader names %q — TG-337 forbids alert content on that plane", f, banned)
			}
		}
	}
}
