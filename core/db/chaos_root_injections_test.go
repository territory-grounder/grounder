package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Gated on TG_TEST_POSTGRES_DSN (CI provides it): the disproof-decay denominator counts DISTINCT injections per
// root inside the lookback — not rows outside it, not blank hosts.
func TestChaosRootInjectionsCountsDistinctFaultsPerRoot(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the chaos root-injection count")
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
	now := time.Now().UTC()
	root := fmt.Sprintf("tg673-root-%d-%d", os.Getpid(), now.UnixNano())
	ins := func(host string, at time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO injected_fault (host, fault_type, injected_at, note) VALUES ($1, 'device-down', $2, 'tg673 test')`, host, at); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	ins(root, now.Add(-time.Hour))
	ins(root, now.Add(-2*time.Hour))
	ins(root, now.Add(-3*time.Hour))
	ins(root, now.Add(-40*24*time.Hour))
	got, err := NewAxisReadStore(p).ChaosRootInjections(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got[root] != 3 {
		t.Fatalf("want 3 injections of %s inside the lookback (the 40-day-old one excluded), got %d", root, got[root])
	}
}
