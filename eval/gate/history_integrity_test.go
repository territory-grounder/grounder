package gate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// EVERY ARCHIVED RECORD MUST KEEP VERIFYING (TG-678). eval/history is the committed, append-only quality trail;
// its pooled scorecards were written by a Pool that dropped the counters, so each one reads judged:0 beside a
// real overall. The stricter integrity check must tolerate them AS LEGACY — disclosed, not rewritten and not
// newly refused — while the comparators (the fresh-base-arm or committed-baseline cards archived beside them)
// get the same treatment.
//
// The count floor is the vacuity guard: a glob that matched nothing would otherwise pass this test.
func TestEveryArchivedRecordStillVerifiesAndIsDisclosedAsLegacy(t *testing.T) {
	root := filepath.Join("..", "history")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("eval/history must exist: %v", err)
	}
	var cards, comparators, notesSeen int
	var newlyFailing []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		// A record whose file EXISTS but does not load is a finding, never a silent skip — only records of other
		// kinds (tier-ab, judgecal, holdout) legitimately carry no scorecard/comparator.
		check := func(kind string, sc Scorecard) {
			label := e.Name() + " " + kind
			probs, notes := VerifyIntegrity(label, []Scorecard{sc}, 0), IntegrityDisclosures(label, []Scorecard{sc})
			if len(probs) != 0 {
				newlyFailing = append(newlyFailing, strings.Join(probs, "; "))
			}
			if sc.Judged == 0 && !sc.CountersRecorded && len(notes) == 0 {
				t.Errorf("%s: a judged:0 unstamped archived %s was tolerated WITHOUT a disclosure note", e.Name(), kind)
			}
			notesSeen += len(notes)
		}
		if scPath := filepath.Join(dir, "scorecard.json"); fileExists(scPath) {
			sc, err := LoadScorecard(scPath)
			if err != nil {
				t.Errorf("%s: scorecard.json exists but does not load: %v", e.Name(), err)
			} else {
				cards++
				check("scorecard", sc)
			}
		}
		if cmpPath := filepath.Join(dir, "comparator.json"); fileExists(cmpPath) {
			b, err := LoadBaseline(cmpPath)
			switch {
			case err != nil:
				t.Errorf("%s: comparator.json exists but does not load: %v", e.Name(), err)
			case b.Scorecard.N > 0:
				comparators++
				check("comparator", b.Scorecard)
			default:
				t.Errorf("%s: comparator.json carries an empty scorecard (n=%d)", e.Name(), b.Scorecard.N)
			}
		}
	}
	if cards < 100 || comparators < 100 {
		t.Fatalf("vacuity guard: read only %d scorecards and %d comparators from %s — the archive has >130 of each", cards, comparators, root)
	}
	if len(newlyFailing) != 0 {
		sort.Strings(newlyFailing)
		t.Fatalf("%d archived record(s) NEWLY fail integrity — legitimate history must keep verifying:\n  %s", len(newlyFailing), strings.Join(newlyFailing, "\n  "))
	}
	if notesSeen == 0 {
		t.Fatal("no archived card was disclosed as legacy — every pre-TG-678 pooled card is judged:0, so the disclosure path never ran")
	}
	t.Logf("verified %d archived scorecards and %d comparators; %d legacy disclosure note(s)", cards, comparators, notesSeen)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
