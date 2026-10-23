package db

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ReadOnlyDSN appends the server-side read-only default in the DSN's own form and leaves an explicit choice alone.
func TestReadOnlyDSNShapes(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:p@h:5432/db":                 "postgres://u:p@h:5432/db?options=-c%20default_transaction_read_only%3Don",
		"postgres://u:p@h:5432/db?sslmode=disable": "postgres://u:p@h:5432/db?sslmode=disable&options=-c%20default_transaction_read_only%3Don",
		"host=h dbname=db":                         "host=h dbname=db options='-c default_transaction_read_only=on'",
		"postgres://u:p@h/db?options=-c%20default_transaction_read_only%3Doff": "postgres://u:p@h/db?options=-c%20default_transaction_read_only%3Doff",
	} {
		if got := ReadOnlyDSN(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

// Against a real database the option is ENFORCED: a write through a read-only pool is refused by the server. DSN-gated.
func TestReadOnlyDSNRefusesWrites(t *testing.T) {
	dsn := os.Getenv("TG_TEST_DSN")
	if dsn == "" {
		t.Skip("TG_TEST_DSN unset")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, ReadOnlyDSN(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("a read must still work: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS tg581_readonly_probe (id int)")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Fatalf("a write through the read-only pool must be refused by the server, got %v", err)
	}
}
