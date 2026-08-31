package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-436 — the async graduation feed is FAIL-CLOSED against promotion.
//
// regimeGradSink.RecordDeferredVerdict feeds a completed deferred verify to the policy graduation ladder,
// but it has no credits.Claim (the exactly-once key) and no external_ref to ground migration-0064's
// graduation_credit_grounded trigger against an action_execution row. So it must NOT credit a clean run:
// a promoting outcome recorded here would earn autonomy ungrounded and un-deduplicated. Demotions and
// streak-breaks must still flow — a safety outcome is never withheld.

// TestAsyncGraduationFeedRefusesPromotion is the killing oracle: a CLEAN, verified async verdict — the
// promoting outcome, OutcomeFromVerdict(VerdictMatch,true)=OutcomeVerifiedClean — must reach the ladder as
// NOTHING. With the fail-closed guard the class is never written; drop the guard and the ladder records the
// promote, the class is persisted, and this test goes red.
func TestAsyncGraduationFeedRefusesPromotion(t *testing.T) {
	const op = "restart-service"
	store := policy.NewMemGraduationStore()
	// threshold 1: a single recorded VerifiedClean WOULD promote (approve→auto-notice) and persist the class.
	ladder := policy.NewLadder(1, store, nil)
	sink := regimeGradSink{ladder: ladder}

	if err := sink.RecordDeferredVerdict(context.Background(), op, safety.VerdictMatch, true); err != nil {
		t.Fatalf("RecordDeferredVerdict(clean) returned error: %v", err)
	}

	// The op-class must be ABSENT: the async feed recorded nothing, so no autonomy was credited. A recorded
	// promote (or even a count increment) persists a ClassState and Load then succeeds.
	if _, err := store.Load(context.Background(), op); !errors.Is(err, policy.ErrClassAbsent) {
		t.Fatalf("async CLEAN feed credited the graduation ladder — expected the class ABSENT (no write), got a "+
			"persisted state (err=%v). The async feed promoted without a credits.Claim / action_execution "+
			"grounding — the TG-436 fail-open.", err)
	}
}

// TestAsyncGraduationFeedStillDemotes is the vacuity floor: the guard gates ONLY promotion. A verified
// DEVIATION is a safety outcome and must still demote the class. If the guard were a blanket "do nothing",
// this would fail — a dropped demotion is a fail-open in the opposite direction.
func TestAsyncGraduationFeedStillDemotes(t *testing.T) {
	const op = "restart-service"
	store := policy.NewMemGraduationStore().Seed(policy.ClassState{OpClass: op, Level: policy.LevelAuto})
	ladder := policy.NewLadder(1, store, nil)
	sink := regimeGradSink{ladder: ladder}

	if err := sink.RecordDeferredVerdict(context.Background(), op, safety.VerdictDeviation, true); err != nil {
		t.Fatalf("RecordDeferredVerdict(deviation) returned error: %v", err)
	}

	st, err := store.Load(context.Background(), op)
	if err != nil {
		t.Fatalf("op-class absent after a demotion — the async feed dropped a safety outcome: %v", err)
	}
	if st.Level != policy.LevelApprove {
		t.Fatalf("async verified deviation did not demote LevelAuto→LevelApprove (got level %v) — the fail-closed "+
			"guard must gate ONLY promotion, never withhold a safety demotion.", st.Level)
	}
}

// ladderRecordCall matches a production call recording a run onto the graduation ladder: `ladder.Record(`,
// including the `s.ladder.Record(` receiver form (the `\b` anchors on the `ladder` word).
var ladderRecordCall = regexp.MustCompile(`\bladder\.Record\(`)

// TestGraduationLadderWritersAreAnnotated is the CLOSED-ENUMERATION seam guard (the core/verify
// callsites_test.go pattern). Every production `ladder.Record(` call is a promote/demote decision on earned
// autonomy (REQ-1514). A promoting write that skips the exactly-once credits.Claim + the 0064 action_execution
// grounding is the fail-open TG-266/TG-435/TG-436 keep re-finding — and a behaviour test cannot catch a NEW
// writer added without that discipline, because each writer is green in its own fixture. So this oracle
// asserts over the closed set of call sites in the shipped tree: each MUST carry a `GRADUATION-WRITER:`
// annotation stating how it is protected (claimed, or fail-closed against promotion). A new, unannotated
// writer fails this test BY LINE until it is reviewed and annotated.
func TestGraduationLadderWritersAreAnnotated(t *testing.T) {
	root := repoRootForGraduationGuard(t)
	const marker = "GRADUATION-WRITER:"
	const lookback = 16 // the annotation must sit within this many lines above the call

	found := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !ladderRecordCall.MatchString(line) {
				continue
			}
			found++
			lo := i - lookback
			if lo < 0 {
				lo = 0
			}
			if !strings.Contains(strings.Join(lines[lo:i+1], "\n"), marker) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d — a graduation-ladder writer with no %q annotation within %d lines above it. "+
					"Every ladder.Record call must state how it is protected against an ungrounded/undeduplicated "+
					"promote (claimed via credits.Claim, or fail-closed against promotion). See TG-436.",
					rel, i+1, marker, lookback)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Guard the guard: a rename of ladder.Record would make the walk match nothing and pass vacuously.
	if found < 2 {
		t.Fatalf("expected at least the 2 known graduation-ladder writers (terminus + async), found %d — has "+
			"ladder.Record been renamed or moved? Update this seam guard so it cannot pass vacuously.", found)
	}
}

// repoRootForGraduationGuard walks up from the test working directory to the nearest go.mod.
func repoRootForGraduationGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test working directory — cannot locate the repo root")
		}
		dir = parent
	}
}
