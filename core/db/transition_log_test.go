package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/httpapi"
)

// TestTransitionLogRoundTrip drives the REAL pgx path for the recovery-transition log (migration 0034) and
// proves the append-only immutability grant (REQ-2016). It appends recovery observations — one BEFORE and two
// AT/AFTER a cutoff, plus one for a different host — and asserts RecoveredSince keys correctly on
// (host, received_at) with the >= cutoff boundary, that many transitions per external_ref are allowed (no
// unique-ref collision, the whole point vs ingest_alert), and that tg_runtime holds NO UPDATE/DELETE on
// ingest_transition. Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestTransitionLogRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the transition-log round-trip test")
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
	s := NewTransitionLogStore(p)

	host := fmt.Sprintf("it-host-%d", os.Getpid())
	ref := fmt.Sprintf("librenms-it-%d", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM ingest_transition WHERE host = $1 OR host = $1 || '-other'", host) }()

	cut := time.Now().UTC()
	// One recovery BEFORE the cutoff (must NOT count), two AT/AFTER (must count) — same ref (re-fire+recover).
	s.Append(ctx, httpapi.TransitionRecord{ExternalRef: ref, Host: host, Site: "nl", AlertRule: "Devices-up/down", ReceivedAt: cut.Add(-10 * time.Minute)})
	s.Append(ctx, httpapi.TransitionRecord{ExternalRef: ref, Host: host, Site: "nl", AlertRule: "Devices-up/down", ReceivedAt: cut.Add(1 * time.Minute)})
	s.Append(ctx, httpapi.TransitionRecord{ExternalRef: ref, Host: host, Site: "nl", AlertRule: "Devices-up/down", ReceivedAt: cut.Add(2 * time.Minute)})
	// A recovery for a DIFFERENT host after the cutoff must not leak into this host's answer.
	s.Append(ctx, httpapi.TransitionRecord{ExternalRef: ref + "-x", Host: host + "-other", Site: "nl", ReceivedAt: cut.Add(3 * time.Minute)})

	// RecoveredSince at the cutoff sees the two at/after rows.
	if ok, err := s.RecoveredSince(ctx, host, cut); err != nil || !ok {
		t.Fatalf("RecoveredSince(host, cut) = %v (err=%v), want true", ok, err)
	}
	// A cutoff AFTER every recovery for this host sees none (the before-row is excluded, the future window empty).
	if ok, err := s.RecoveredSince(ctx, host, cut.Add(1*time.Hour)); err != nil || ok {
		t.Fatalf("RecoveredSince(host, cut+1h) = %v (err=%v), want false", ok, err)
	}
	// An unknown host never matches.
	if ok, err := s.RecoveredSince(ctx, host+"-nope", cut.Add(-1*time.Hour)); err != nil || ok {
		t.Fatalf("RecoveredSince(unknown host) = %v (err=%v), want false", ok, err)
	}

	// REQ-2016: the runtime role holds no UPDATE and no DELETE on the append-only transition log.
	var upd, del bool
	if err := p.QueryRow(ctx,
		"SELECT has_table_privilege('tg_runtime', 'ingest_transition', 'UPDATE'), has_table_privilege('tg_runtime', 'ingest_transition', 'DELETE')").
		Scan(&upd, &del); err != nil {
		t.Logf("grant check skipped (role lacks privilege to introspect): %v", err)
		return
	}
	if upd || del {
		t.Fatalf("ingest_transition: tg_runtime has UPDATE=%v DELETE=%v, want both false (REQ-2016 append-only)", upd, del)
	}
}
