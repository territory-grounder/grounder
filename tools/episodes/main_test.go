package main

import (
	"bytes"
	"strings"
	"testing"
)

// The CLI refuses an unusable invocation before it touches a database: a cohort is a fixed window, and the
// read-only guard is added to the DSN unless the operator supplied options of their own.
func TestUsageErrorsExitTwoBeforeAnyConnection(t *testing.T) {
	for _, args := range [][]string{
		{"-dsn", "postgres://x/y", "-since", "2026-11-06T11:04:00Z"},
		{"-dsn", "postgres://x/y", "-until", "2026-11-07"},
		{"-dsn", "postgres://x/y", "-since", "2026-11-07", "-until", "2026-11-06"},
		{"-dsn", "postgres://x/y", "-since", "not-a-time", "-until", "2026-11-07"},
		{"-dsn", "postgres://x/y", "-since", "2026-11-06", "-until", "2026-11-07", "stray"},
		{"-since", "2026-11-06", "-until", "2026-11-07", "-dsn", ""},
	} {
		var out, errb bytes.Buffer
		if rc := run(args, &out, &errb); rc != 2 {
			t.Errorf("args %v: exit %d, want 2 (usage): %s", args, rc, errb.String())
		}
	}
}

func TestReadOnlyGuardIsAddedOnce(t *testing.T) {
	ro, applied, err := readOnlyDSN("postgres://u:p@h/db?sslmode=disable")
	if err != nil || !applied || !strings.Contains(ro, "default_transaction_read_only%3Don") {
		t.Fatalf("url DSN: %q %v %v", ro, applied, err)
	}
	ro, applied, err = readOnlyDSN("host=/var/run/postgresql user=postgres dbname=grounder")
	if err != nil || !applied || !strings.Contains(ro, "options='-c default_transaction_read_only=on'") {
		t.Fatalf("keyword DSN: %q %v %v", ro, applied, err)
	}
	if _, applied, _ := readOnlyDSN("host=x options='-c search_path=public'"); applied {
		t.Fatal("an operator-supplied options must be left alone")
	}
	if ts, err := parseInstant("2026-11-06"); err != nil || ts.Hour() != 0 {
		t.Fatalf("bare date: %v %v", ts, err)
	}
}
