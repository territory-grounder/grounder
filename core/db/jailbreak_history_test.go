package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TG-80 P2-6: the repeat-offender count reads the durable audit spine — jailbreak-polled
// classifications joined to their session's HOST (the stable subject), scoped by the window, and
// blind to other hosts and other poll reasons. Real Postgres: the jsonb ->> predicate and the join
// are the behavior under test. Unique refs; nothing deleted.
func TestPriorJailbreaksCountsTheHostInsideTheWindow(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the prior-jailbreaks read")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)

	host := fmt.Sprintf("p26-web-%d", time.Now().UnixNano())
	seed := func(ref, h, reason string, at time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx,
			`INSERT INTO session_triage (external_ref, host, alert_rule) VALUES ($1, $2, 'NginxDown') ON CONFLICT DO NOTHING`,
			ref, h); err != nil {
			t.Fatalf("seed triage: %v", err)
		}
		if _, err := p.Exec(ctx, `
			INSERT INTO session_risk_audit (external_ref, risk_level, band, action_id, schema_version, signals_json, created_at)
			VALUES ($1, 'medium', 'POLL_PAUSE', $2, 1, $3, $4)`,
			ref, "act-"+ref, fmt.Sprintf(`{"poll_reason":%q}`, reason), at); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	now := time.Now().UTC()
	seed("p26-hit-1-"+host, host, "jailbreak-detected", now.Add(-time.Hour))
	seed("p26-hit-2-"+host, host, "jailbreak-detected", now.Add(-2*time.Hour))
	seed("p26-stale-"+host, host, "jailbreak-detected", now.Add(-30*24*time.Hour)) // outside the window
	seed("p26-other-reason-"+host, host, "ood-novel-incident", now.Add(-time.Hour)) // polled, not hostile
	seed("p26-other-host-"+host, host+"-b", "jailbreak-detected", now.Add(-time.Hour))

	n, err := NewSessionReadStore(p).PriorJailbreaks(ctx, host, now.Add(-hostileTestWindow))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 2 {
		t.Fatalf("want exactly the two in-window jailbreak hits for THIS host, got %d", n)
	}
	if n2, err := NewSessionReadStore(p).PriorJailbreaks(ctx, "no-such-host-"+host, now.Add(-hostileTestWindow)); err != nil || n2 != 0 {
		t.Fatalf("an unseen host must be zero: n=%d err=%v", n2, err)
	}
}

const hostileTestWindow = 7 * 24 * time.Hour
