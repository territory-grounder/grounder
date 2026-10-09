package db

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
)

// ConnectDynamic round-trip against the live test fixture (TG-422 slice 2): the credential must arrive
// through the BeforeConnect seam, not the DSN — so the template carries NO userinfo and the hook is the
// only source. Gated like every live-DB test here; the hook-counting assert is what kills a mutation that
// quietly reverts to pgxpool.New(dsn) with an embedded credential.
func TestConnectDynamicLeasesPerConnection(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the dynamic-connect round-trip test")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		// A set-but-unusable fixture DSN is a broken harness, not an absent one — fail, never skip
		// (dsn_gate_test.go: a non-DSN-conditional skip reports "ok" in the harness job forever).
		t.Fatalf("TG_TEST_POSTGRES_DSN is set but not a userinfo-carrying URL (%v) — cannot derive a template", err)
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	tmpl := *u
	tmpl.User = nil

	calls := 0
	pool, err := ConnectDynamic(context.Background(), tmpl.String(), func(context.Context) (string, string, error) {
		calls++
		return user, pass, nil
	})
	if err != nil {
		t.Fatalf("ConnectDynamic: %v", err)
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 through the dynamic pool = (%d, %v)", one, err)
	}
	if calls < 1 {
		t.Fatal("the credential hook was never consulted — the connection carried an embedded credential, which is the exact thing ConnectDynamic exists to end")
	}

	// A credential source that errors must fail the connect CLOSED — no fallback, no empty-credential dial.
	if _, err := ConnectDynamic(context.Background(), tmpl.String(), func(context.Context) (string, string, error) {
		return "", "", errors.New("no live lease")
	}); err == nil || !strings.Contains(err.Error(), "dynamic") {
		t.Fatalf("a failing credential source must fail the connect closed with a named cause, got %v", err)
	}
	// And no source at all is a refusal, not a default.
	if _, err := ConnectDynamic(context.Background(), tmpl.String(), nil); err == nil {
		t.Fatal("a nil credential source must be refused")
	}
}
