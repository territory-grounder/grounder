package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TG-687 (live 2026-11-08): every READ-ONLY investigation path that resolves an SSH identity must ask for
// credential.PurposeDiagRead, or a per-host admin binding (OpenBao, precedence 10) out-ranks TG's guarded read-only
// key, the read bypasses tg-readonly-guard, and TG's own read lands in the host journal as the shared admin key.
// This reads the SHIPPED source of each read site, the same "read what ships" discipline as the CI-file tests: a new
// credential.Target in one of these files without the purpose fails here, not on a live host.
func TestEveryDiagnosticReadSiteAsksForTheReadOnlyIdentity(t *testing.T) {
	sites := []string{
		"../../modules/actorevidence/journal/journal.go",
		"../../modules/actorevidence/k8saudit/k8saudit.go",
		"../../modules/observability/hostdiag/hostdiag.go",
		"../../modules/observability/hostdiag/objectstate.go",
		"../../modules/observability/hostdiag/selftest.go",
		"discovery_runner.go",
	}
	for _, rel := range sites {
		b, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "credential.Target{") || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			n++
			if !strings.Contains(line, "Purpose: credential.PurposeDiagRead") {
				t.Errorf("%s: a read-only credential resolution without PurposeDiagRead: %s", rel, strings.TrimSpace(line))
			}
		}
		if n == 0 {
			t.Errorf("control: %s builds no credential.Target — the site list is stale", rel)
		}
	}
}
