package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFreezeWindows proves the freeze-window parser: valid rows parse, malformed/inverted rows are skipped
// (never a half/bad window), and an empty path yields none — fail toward investigating.
func TestFreezeWindows(t *testing.T) {
	if got := freezeWindows(""); got != nil {
		t.Errorf("empty path must yield no windows, got %v", got)
	}
	p := writeTemp(t, `[
		{"scope":"web01","start":"2026-07-16T02:00:00Z","end":"2026-07-16T04:00:00Z","reason":"reboot"},
		{"scope":"bad","start":"not-a-time","end":"2026-07-16T04:00:00Z","reason":"x"},
		{"scope":"inverted","start":"2026-07-16T04:00:00Z","end":"2026-07-16T02:00:00Z","reason":"x"}
	]`)
	got := freezeWindows(p)
	if len(got) != 1 || got[0].Scope != "web01" || got[0].Reason != "reboot" {
		t.Fatalf("want 1 valid window (malformed + inverted skipped), got %+v", got)
	}
	if freezeWindows(filepath.Join(t.TempDir(), "missing.json")) != nil {
		t.Error("an unreadable file must yield no windows (fail toward investigating)")
	}
}

// TestSuppressRules proves the operator-rule parser: valid rules parse, a CATCH-ALL (host=* rule=*) is
// refused (it would silence the whole estate), and an empty path yields none.
func TestSuppressRules(t *testing.T) {
	if got := suppressRules(""); got != nil {
		t.Errorf("empty path must yield no rules, got %v", got)
	}
	p := writeTemp(t, `[
		{"host":"noisy-*","rule":"FlapDetected","reason":"known flapper"},
		{"host":"*","rule":"*","reason":"oops-catch-all"}
	]`)
	got := suppressRules(p)
	if len(got) != 1 || got[0].HostPattern != "noisy-*" || got[0].RulePattern != "FlapDetected" {
		t.Fatalf("want 1 rule with the catch-all refused, got %+v", got)
	}
}
