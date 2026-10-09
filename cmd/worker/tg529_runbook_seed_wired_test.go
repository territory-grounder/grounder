package main

import (
	"os"
	"strings"
	"testing"
)

// TG-529: the runbook-corpus boot seed must stay wired, or every merged runbook pack silently regresses to
// the repo-boundary dead end this fixed (corpus authored, reachable nowhere — no store row, no wiki page —
// until an operator remembers two manual tools). Source guard in the house pattern: pins anchor on CODE
// (comment lines stripped), each names the regression it would mean.
//
// KILLING MUTATION: delete the seedRunbookCorpus call (or its Runbooks() read) — the matching pin fails
// naming the gap. Restore → green.
func TestTG529RunbookCorpusSeedIsWired(t *testing.T) {
	b, err := os.ReadFile("worker_skill_import.go")
	if err != nil {
		t.Fatalf("read worker_skill_import.go: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	src := strings.Join(kept, "\n")
	if len(src) < 1000 {
		t.Fatal("worker_skill_import.go read came back implausibly small — the guard would be vacuous")
	}
	if !strings.Contains(src, "seedRunbookCorpus(ctx, st, lg)") {
		t.Error("seedRunbookCorpus is no longer called from the boot import — merged runbook packs are " +
			"back to reachable-nowhere until a manual two-tool seed (TG-529)")
	}
	if !strings.Contains(src, "skillcorpus.Runbooks()") {
		t.Error("the seed no longer reads the embedded corpus (skillcorpus.Runbooks) — it would seed " +
			"nothing while the log still prints a seeded line (TG-529)")
	}
	if !strings.Contains(src, "Class: skillstore.ClassRunbook") {
		t.Error("the seeded identity rows no longer carry ClassRunbook — the wiki lists runbook-class " +
			"production rows only, so the packs would seed yet stay invisible (TG-529)")
	}
}
