package lessons

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// A lesson carries provenance (ResolvedAt round-trips through ParseResolved), and its age is DOWN-WEIGHTED on
// a half-life: one half-life old counts half, four half-lives count a sixteenth, and an undatable lesson (no
// provenance) is never down-weighted.
func TestHalfLifeWeightDownWeightsByAge(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	hl := 30 * 24 * time.Hour

	// provenance round-trips through the JSON feed (DisallowUnknownFields accepts the new field).
	feed := `[{"external_ref":"TG-1","action":"restart nginx","confirmed_clear":true,"verdict":"match","resolved_at":"2026-06-20T00:00:00Z"}]`
	resolved, err := ParseResolved(strings.NewReader(feed))
	if err != nil {
		t.Fatalf("feed with provenance must parse: %v", err)
	}
	if len(resolved) != 1 || resolved[0].ResolvedAt.IsZero() {
		t.Fatalf("provenance timestamp did not round-trip: %+v", resolved)
	}

	fresh := HalfLifeWeight(now, now, hl)
	if fresh != 1.0 {
		t.Fatalf("a lesson resolved now must weigh 1.0, got %.4f", fresh)
	}
	oneHL := HalfLifeWeight(now.Add(-hl), now, hl)
	if oneHL < 0.49 || oneHL > 0.51 {
		t.Fatalf("a lesson one half-life old must weigh ~0.5, got %.4f", oneHL)
	}
	fourHL := HalfLifeWeight(now.Add(-4*hl), now, hl)
	if fourHL < 0.06 || fourHL > 0.07 {
		t.Fatalf("a lesson four half-lives old must weigh ~1/16, got %.4f", fourHL)
	}
	// an aged lesson is strictly down-weighted vs a fresh one.
	if !(fourHL < oneHL && oneHL < fresh) {
		t.Fatalf("influence must decay monotonically with age: fresh=%.4f oneHL=%.4f fourHL=%.4f", fresh, oneHL, fourHL)
	}
	// no provenance ⇒ never down-weighted (fail toward retention).
	if w := HalfLifeWeight(time.Time{}, now, hl); w != 1.0 {
		t.Fatalf("an undatable lesson must weigh 1.0, got %.4f", w)
	}
}

// Reconcile prunes only the lessons older than the retention horizon; a fresh one and an undatable one are
// kept, and PruneStaleFromCorpus removes exactly the aged-out precedents from the corpus.
func TestReconcilePrunesStaleAndPrunesCorpus(t *testing.T) {
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	maxAge := 90 * 24 * time.Hour
	resolved := []ResolvedIncident{
		{ExternalRef: "OLD-1", Action: "a", ConfirmedClear: true, ResolvedAt: now.Add(-200 * 24 * time.Hour)}, // stale
		{ExternalRef: "NEW-1", Action: "b", ConfirmedClear: true, ResolvedAt: now.Add(-10 * 24 * time.Hour)},  // fresh
		{ExternalRef: "UNDATED", Action: "c", ConfirmedClear: true},                                           // no provenance
	}
	res := Reconcile(resolved, now, maxAge)
	if len(res.StaleRefs) != 1 || res.StaleRefs[0] != "OLD-1" {
		t.Fatalf("only OLD-1 must be stale, got %v", res.StaleRefs)
	}
	if len(res.Fresh) != 2 {
		t.Fatalf("the fresh + undatable lessons must be kept, got %d", len(res.Fresh))
	}

	corpus := []knowledge.Incident{{ExternalRef: "OLD-1"}, {ExternalRef: "NEW-1"}, {ExternalRef: "UNDATED"}}
	kept, removed := PruneStaleFromCorpus(corpus, res.StaleRefs)
	if removed != 1 || len(kept) != 2 {
		t.Fatalf("exactly OLD-1 must leave the corpus, got removed=%d kept=%d", removed, len(kept))
	}
	for _, inc := range kept {
		if inc.ExternalRef == "OLD-1" {
			t.Fatal("the stale precedent must be gone from the corpus")
		}
	}

	// maxAge <= 0 disables pruning (every lesson kept), and an empty stale set is a corpus no-op.
	if r := Reconcile(resolved, now, 0); len(r.StaleRefs) != 0 || len(r.Fresh) != 3 {
		t.Fatalf("maxAge<=0 must keep every lesson, got stale=%v fresh=%d", r.StaleRefs, len(r.Fresh))
	}
	if _, removed := PruneStaleFromCorpus(corpus, nil); removed != 0 {
		t.Fatalf("an empty stale set must prune nothing, removed=%d", removed)
	}
}
