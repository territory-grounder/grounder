package db

import (
	"testing"
	"time"
)

// LastAlertByHost is the fired-history side of the observation census (TG-180). Its two load-bearing
// properties are exactly the JOIN/aggregation semantics a fake cannot reproduce, so it runs against a real
// Postgres (TG_TEST_DSN): (1) MAX(received_at) per host — a host that fired twice reports its LATEST, or a
// recently-alarmed host would wrongly read healthy_quiet; (2) a host that never fired is ABSENT — present-with-
// zero would make the census read every silent host as observed. The killing direction is a host silently
// dropping out (→ mis-classified unobservable) or a stale timestamp winning (→ mis-classified quiet).
func TestLastAlertByHost_MaxPerHostAndNeverFiredAbsent(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()

	wipe := func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host LIKE 'census-%'`) }
	wipe()
	defer wipe()

	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	ins := func(host string, at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,'librenms','lnms','Device-Down','critical',$2,$3,$3)`,
			"census-"+host+"-"+at.Format("150405.000000"), host, at); err != nil {
			t.Fatalf("seed ingest_alert: %v", err)
		}
	}
	// census-a fires twice — the LATER time must win; census-b fires once; census-never is not seeded at all.
	ins("census-a", base)
	ins("census-a", base.Add(2*time.Hour))
	ins("census-b", base.Add(time.Hour))

	got, err := NewAxisReadStore(p).LastAlertByHost(ctx)
	if err != nil {
		t.Fatalf("LastAlertByHost: %v", err)
	}
	if la, ok := got["census-a"]; !ok || !la.Equal(base.Add(2*time.Hour)) {
		t.Errorf("census-a last-fired = %v (ok=%v), want the MAX %v (a stale timestamp winning would mis-read it as quiet)", la, ok, base.Add(2*time.Hour))
	}
	if _, ok := got["census-b"]; !ok {
		t.Error("census-b missing — a host that fired once must appear, or the census reads it unobservable")
	}
	if _, ok := got["census-never"]; ok {
		t.Error("a never-fired host appeared — the census would wrongly count it as observed")
	}
}
