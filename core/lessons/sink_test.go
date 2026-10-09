package lessons

import (
	"bytes"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// TestParseResolvedRoundTripAndRejectsNoRef proves the resolved-incident feed round-trips through the JSON
// form the operator export (and, later, the close-out path) uses, and that an identity-less record is
// rejected loudly rather than silently persisted as anonymous precedent.
func TestParseResolvedRoundTripAndRejectsNoRef(t *testing.T) {
	src := `[{"external_ref":"TG-9","host":"db01","alert_rule":"DiskFull","action":"prune wal","verdict":"match","confirmed_clear":true}]`
	got, err := ParseResolved(strings.NewReader(src))
	if err != nil {
		t.Fatalf("well-formed feed must parse: %v", err)
	}
	if len(got) != 1 || got[0].ExternalRef != "TG-9" || got[0].Verdict != safety.VerdictMatch || !got[0].ConfirmedClear {
		t.Fatalf("round-trip lost fields: %+v", got)
	}
	if _, err := ParseResolved(strings.NewReader(`[{"host":"db01","confirmed_clear":true}]`)); err == nil {
		t.Fatal("a resolved incident with no external_ref must be rejected, not silently kept")
	}
	// An unknown field is rejected (fail-loud on a malformed feed, never a silent partial import).
	if _, err := ParseResolved(strings.NewReader(`[{"external_ref":"TG-1","bogus":1}]`)); err == nil {
		t.Fatal("an unknown field must be rejected")
	}
}

// TestMergePersistsOnlyConfirmedCleanAndIsIdempotent is the persistence-hop oracle (INV-22): it drives the
// real Distill + knowledge.MergeCorpus and proves (a) only a confirmed-clean lesson is contributed to the
// corpus — a deviation and an unconfirmed match are NOT — (b) the net-new count reflects exactly the fresh
// external_refs, and (c) re-merging the same feed is a no-op (added=0), so an idempotent re-import never
// churns the corpus or double-counts.
func TestMergePersistsOnlyConfirmedCleanAndIsIdempotent(t *testing.T) {
	existing := []knowledge.Incident{{ExternalRef: "TG-0", Host: "old01", AlertRule: "OldAlert", Resolution: "did a thing"}}

	cleanNew := clean() // TG-1, match + confirmed clear → a real lesson
	dev := clean()
	dev.ExternalRef, dev.Verdict = "TG-2", safety.VerdictDeviation // never a lesson
	unconf := clean()
	unconf.ExternalRef, unconf.ConfirmedClear = "TG-3", false // asserted, not verified → never a lesson

	merged, added := Merge(existing, []ResolvedIncident{cleanNew, dev, unconf})
	if added != 1 {
		t.Fatalf("only the one confirmed-clean incident is net-new, got added=%d", added)
	}
	if len(merged) != 2 {
		t.Fatalf("corpus must hold the prior TG-0 plus the one new lesson, got %d: %+v", len(merged), merged)
	}
	// The poisoned outcomes are absent; the clean one is present and retrievable.
	refs := map[string]bool{}
	for _, inc := range merged {
		refs[inc.ExternalRef] = true
	}
	if !refs["TG-0"] || !refs["TG-1"] {
		t.Fatalf("corpus must contain TG-0 and TG-1, got %v", refs)
	}
	if refs["TG-2"] || refs["TG-3"] {
		t.Fatalf("a deviation/unconfirmed outcome must never enter the corpus, got %v", refs)
	}

	// Re-merging the same feed into the now-updated corpus adds nothing (idempotent).
	merged2, added2 := Merge(merged, []ResolvedIncident{cleanNew, dev, unconf})
	if added2 != 0 {
		t.Fatalf("re-importing the same feed must be a no-op, got added=%d", added2)
	}
	if len(merged2) != len(merged) {
		t.Fatalf("idempotent re-import must not grow the corpus: %d → %d", len(merged), len(merged2))
	}

	// The merged corpus round-trips through the corpus writer/parser the retriever reloads.
	var buf bytes.Buffer
	if err := knowledge.WriteCorpus(&buf, merged); err != nil {
		t.Fatalf("merged corpus must serialize: %v", err)
	}
	back, err := knowledge.ParseCorpus(&buf)
	if err != nil || len(back) != len(merged) {
		t.Fatalf("merged corpus must round-trip through the corpus file the retriever reloads: %v (%d)", err, len(back))
	}
}
