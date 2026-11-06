package db

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// OpenIncidentHosts is the actuation verifier's HOST-arm baseline (the 2026-07-28 false deviation): the hosts
// already holding an OPEN incident as of asOf. These oracles pin the properties the verdict rests on (anchor cut, recovery, re-raise, staleness bound, and
// since TG-569 re-fires recorded only in ingest_alert_occurrence);
// each has a seed whose absence or inversion flips the result, so none can pass vacuously.

func seedOpenIncidents(ctx context.Context, t *testing.T, p *Pool, asOf time.Time) func() {
	t.Helper()
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host LIKE 'oih-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE host LIKE 'oih-%'`)
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert_occurrence WHERE host LIKE 'oih-%'`)
	}
	cleanup()
	alert := func(host string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,'librenms','lnms','Device-Down','critical',$2,$3,$3)`,
			"oih-"+host+at.Format("150405.000"), host, at); err != nil {
			t.Fatalf("seed alert %s: %v", host, err)
		}
	}
	recovery := func(host string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_transition (external_ref, kind, host, alert_rule, received_at)
			VALUES ($1,'recovery',$2,'Device-Down',$3)`,
			"oih-r-"+host+at.Format("150405.000"), host, at); err != nil {
			t.Fatalf("seed recovery %s: %v", host, err)
		}
	}
	// refire: a re-delivery of an already-admitted key. Only ingest_alert_occurrence gets a row, because the
	// canonical ingest_alert row keeps the first delivery (ON CONFLICT DO NOTHING). TG-569.
	refire := func(host string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert_occurrence (external_ref, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,'Device-Down','critical',$2,$3,$3)`,
			"oih-o-"+host, host, at); err != nil {
			t.Fatalf("seed refire %s: %v", host, err)
		}
	}
	alert("oih-open", asOf.Add(-20*time.Minute))                // raised, never recovered → OPEN as of asOf
	alert("oih-closed", asOf.Add(-40*time.Minute))              // raised...
	recovery("oih-closed", asOf.Add(-10*time.Minute))           // ...and recovered BEFORE asOf → closed
	alert("oih-after", asOf.Add(2*time.Minute))                 // raised AFTER asOf — the action's own effect shape
	alert("oih-reopened", asOf.Add(-30*time.Minute))            // raised...
	recovery("oih-reopened", asOf.Add(-25*time.Minute))         // ...recovered...
	alert("oih-reopened", asOf.Add(-5*time.Minute))             // ...re-raised before asOf → OPEN again
	alert("oih-stale", asOf.Add(-ingest.MaxOpenIncident-time.Hour)) // ancient un-recovered raise → past the bound
	// oih-late-recovery: open at asOf, recovery arrives AFTER asOf — must still read OPEN as of asOf, or the
	// verifier's baseline depends on how long the verify was delayed.
	alert("oih-late-recovery", asOf.Add(-15*time.Minute))
	recovery("oih-late-recovery", asOf.Add(3*time.Minute))
	// TG-569, the live shape: first raised weeks ago (canonical row past the bound), still re-firing → OPEN.
	alert("oih-refiring", asOf.Add(-ingest.MaxOpenIncident-20*24*time.Hour))
	refire("oih-refiring", asOf.Add(-30*time.Second))
	// A re-fire after a recovery re-opens, exactly like a fresh raise.
	alert("oih-refire-reopened", asOf.Add(-50*time.Minute))
	recovery("oih-refire-reopened", asOf.Add(-40*time.Minute))
	refire("oih-refire-reopened", asOf.Add(-2*time.Minute))
	// A recovery after the last re-fire still closes it — re-fires must not outvote a later recovery.
	alert("oih-refire-closed", asOf.Add(-ingest.MaxOpenIncident-24*time.Hour))
	refire("oih-refire-closed", asOf.Add(-20*time.Minute))
	recovery("oih-refire-closed", asOf.Add(-5*time.Minute))
	// A re-fire AFTER asOf is on the action's side of the cut; one OLDER than the bound is a gap, not an incident.
	alert("oih-refire-after", asOf.Add(-ingest.MaxOpenIncident-24*time.Hour))
	refire("oih-refire-after", asOf.Add(time.Minute))
	alert("oih-refire-stale", asOf.Add(-ingest.MaxOpenIncident-48*time.Hour))
	refire("oih-refire-stale", asOf.Add(-ingest.MaxOpenIncident-time.Hour))
	return cleanup
}

func TestOpenIncidentHostsAnchorsAtAsOf(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	asOf := time.Now().UTC().Add(-time.Hour) // an anchor in the past, so post-asOf rows exist in the seed
	defer seedOpenIncidents(ctx, t, p, asOf)()

	got, err := NewAlertHistoryStore(p).OpenIncidentHosts(ctx, asOf, ingest.MaxOpenIncident)
	if err != nil {
		t.Fatalf("OpenIncidentHosts: %v", err)
	}
	if !got["oih-open"] {
		t.Error("an un-recovered raise before asOf is an OPEN incident — its absence means the baseline sees a " +
			"broken host as healthy and the verifier manufactures a deviation from it")
	}
	if !got["oih-reopened"] {
		t.Error("a raise AFTER a recovery re-opens the incident — latest-event-wins must see the re-raise")
	}
	if !got["oih-late-recovery"] {
		t.Error("a recovery that arrived AFTER asOf must not close the incident AS OF asOf — the baseline would " +
			"otherwise depend on how long the verify was delayed")
	}
	if got["oih-closed"] {
		t.Error("a recovery before asOf closes the incident — counting it open would let the baseline swallow a " +
			"REAL cascade onto a genuinely healthy host")
	}
	if got["oih-after"] {
		t.Error("a raise AFTER asOf is on the action's own side of the cut — laundering it into the baseline " +
			"hides exactly the cascades the verifier exists to catch")
	}
	if !got["oih-refiring"] {
		t.Error("TG-569: a weeks-old incident that is STILL RE-FIRING is open — reading only the first delivery " +
			"made it look stale, and the necessity re-check refused every fix for a recurring alert")
	}
	if !got["oih-refire-reopened"] {
		t.Error("a re-fire after a recovery re-opens the incident, exactly as a fresh raise does")
	}
	if got["oih-refire-closed"] {
		t.Error("a recovery AFTER the last re-fire closes the incident — re-fires must not outvote it")
	}
	if got["oih-refire-after"] {
		t.Error("a re-fire AFTER asOf is on the action's own side of the cut and must not open the baseline")
	}
	if got["oih-refire-stale"] {
		t.Error("a re-fire older than the bound is a monitoring gap, not an open incident")
	}
	if got["oih-stale"] {
		t.Error("an un-recovered raise older than the bound is a monitoring gap, not an eternal incident — " +
			"without the bound a host that once alerted corroborates as broken forever")
	}
}

// TestOpenIncidentsBaselineFailsClosed — the M3 seam. The (set, ok) closure is the ONE place the error→ok
// mapping lives, and a read error must be (nil,false), never (empty,true): an empty map asserts "no host was
// anomalous", which on a failed read is the manufactured-deviation defect reproduced at a new seam.
func TestOpenIncidentsBaselineFailsClosed(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	canceled, cancel := context.WithCancel(ctx)
	cancel() // force the read to error without touching the pool's health
	set, ok := OpenIncidentsBaseline(NewAlertHistoryStore(p), ingest.MaxOpenIncident)(canceled, time.Now().UTC())
	if ok {
		t.Fatal("a failed open-incident read reported ok=true — (empty,true) on error asserts a healthy estate " +
			"the read never saw, and the verifier builds a deviation on it")
	}
	if len(set) != 0 {
		t.Fatalf("a failed read must contribute an empty set alongside ok=false, got %d hosts", len(set))
	}

	// And the healthy path really is (set, true) — otherwise the closure "fails closed" by failing always.
	if _, ok := OpenIncidentsBaseline(NewAlertHistoryStore(p), ingest.MaxOpenIncident)(ctx, time.Now().UTC()); !ok {
		t.Fatal("a successful read must report ok=true — a closure that always fails is not fail-closed, it is unwired")
	}
}

// OpenIncidentPairs is the falsifiability scorer's PAIR-arm commit-time baseline (Phase C4): the same
// latest-event-wins walk as OpenIncidentHosts, at (host, alert_rule) granularity, anchored at asOf. The
// anchoring properties are inherited from the same timeline; this oracle pins the PAIR-specific one — two
// rules on one host resolve independently — plus the shared anchor cut.
func TestOpenIncidentPairsAnchorsAtAsOfPerPair(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	asOf := time.Now().UTC().Add(-time.Hour)
	defer seedOpenIncidents(ctx, t, p, asOf)()
	// One host, TWO rules: Device-Down recovered before asOf, DiskFull still open — the pair walk must keep
	// them apart (the host arm would collapse them).
	if _, err := p.Exec(ctx, `
		INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
		VALUES ('oih-two-a','librenms','lnms','Device-Down','critical','oih-two',$1,$1),
		       ('oih-two-b','librenms','lnms','DiskFull','warning','oih-two',$2,$2)`,
		asOf.Add(-30*time.Minute), asOf.Add(-20*time.Minute)); err != nil {
		t.Fatalf("seed two-rule host: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, alert_rule, received_at)
		VALUES ('oih-two-r','recovery','oih-two','Device-Down',$1)`, asOf.Add(-10*time.Minute)); err != nil {
		t.Fatalf("seed two-rule recovery: %v", err)
	}

	pairs, err := NewAlertHistoryStore(p).OpenIncidentPairs(ctx, asOf, ingest.MaxOpenIncident)
	if err != nil {
		t.Fatalf("OpenIncidentPairs: %v", err)
	}
	open := map[string]bool{}
	for _, a := range pairs {
		open[a.Host+"|"+a.Rule] = true
	}
	if !open["oih-two|DiskFull"] {
		t.Error("the still-open pair on the two-rule host must be in the baseline")
	}
	if open["oih-two|Device-Down"] {
		t.Error("the recovered pair on the SAME host must not be — pair granularity is the point of this arm")
	}
	if !open["oih-open|Device-Down"] {
		t.Error("an un-recovered raise before asOf is an OPEN pair")
	}
	if open["oih-after|Device-Down"] {
		t.Error("a raise AFTER asOf must not launder into the commit-time baseline — it is on the outcome's side of the cut")
	}
	if !open["oih-refiring|Device-Down"] {
		t.Error("TG-569: a pair first raised weeks ago that is STILL RE-FIRING is open at commit time")
	}
	if open["oih-refire-closed|Device-Down"] || open["oih-refire-after|Device-Down"] {
		t.Error("a pair whose re-fire was followed by a recovery, or whose only recent re-fire is after asOf, is not open")
	}
	if open["oih-stale|Device-Down"] {
		t.Error("a pair past the staleness bound is a monitoring gap, not an eternal baseline entry")
	}
}

// FalsifyBaseline is the ONE error→ok mapping for the scoring lane's commit-time baseline. Same law as
// OpenIncidentsBaseline: a failed read is (nil, nil, false) — (empty, empty, true) on error would assert a
// healthy estate the read never saw, and the scorer would author forecast deviations on it.
func TestFalsifyBaselineFailsClosed(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	pairs, hosts, ok := FalsifyBaseline(NewAlertHistoryStore(p), ingest.MaxOpenIncident)(canceled, time.Now().UTC())
	if ok {
		t.Fatal("a failed baseline read reported ok=true — the scorer would adjudicate against a fabricated-empty baseline")
	}
	if len(pairs) != 0 || len(hosts) != 0 {
		t.Fatalf("a failed read must contribute nothing alongside ok=false, got %d pairs %d hosts", len(pairs), len(hosts))
	}
	if _, _, ok := FalsifyBaseline(NewAlertHistoryStore(p), ingest.MaxOpenIncident)(ctx, time.Now().UTC()); !ok {
		t.Fatal("a successful read must report ok=true — a seam that always fails would silently stop every forecast verdict")
	}
}
