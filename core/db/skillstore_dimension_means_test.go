package db

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// TG-590: ONE HARNESS PER POOL. A version's judged pool narrows to the harness fingerprint of its NEWEST
// non-trial-arm configuration once that configuration has accrued minPool JUDGED sessions; the judged sessions
// of other configurations are excluded AND counted; below the threshold, or with legacy ” fingerprints only,
// the window pools unfiltered; an unjudged session is neither evidence nor an anchor; a trial-arm session never
// anchors. DSN-gated (two-DB recipe). Rows are uniquely tagged and LEFT.
func TestDimensionMeansPoolsOneHarnessFingerprintPerVersion(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewSkillStore(p)
	if _, err := st.EnsureChain(ctx); err != nil {
		t.Fatalf("EnsureChain: %v", err)
	}
	tag := fmt.Sprintf("tg590-%d", time.Now().UnixNano())
	if err := st.PutSkill(ctx, skillstore.Skill{Name: tag, Kind: "behavioral", Class: skillstore.ClassSkill}); err != nil {
		t.Fatalf("PutSkill: %v", err)
	}
	aw := skillstore.AppliesWhen{}
	v, err := st.CreateVersion(ctx, skillstore.Version{SkillName: tag, Version: "1.0.0", Body: "body", Status: skillstore.StatusDraft,
		AppliesWhen: aw, ContentHash: skillstore.ContentHash("body", aw), Author: "test", Source: "test", Rationale: "[draft] TG-590 pool oracle fixture"})
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	load := fmt.Sprintf("%s@1.0.0#%d:store", tag, v.ID)
	trialLoad := load + ":trial7/arm1"
	ts := NewTriageStore(p)
	record := func(ref, fp string, loads []string) {
		t.Helper()
		row := judge.TriageRow{ExternalRef: ref, Host: "h", AlertRule: "R", Band: "ESCALATE", Outcome: "escalated", Proposed: true,
			SkillLoads: loads, HarnessFP: fp}
		if err := ts.RecordTriage(ctx, row); err != nil {
			t.Fatalf("RecordTriage %s: %v", ref, err)
		}
		time.Sleep(2 * time.Millisecond) // created_at strictly increasing: "newest" is well-defined
	}
	write := func(ref, fp string, loads []string, score float64) {
		t.Helper()
		record(ref, fp, loads)
		if err := ts.WriteJudgment(ctx, ref, "correct_diagnosis", score, "", judge.RubricVersion()); err != nil {
			t.Fatalf("WriteJudgment %s: %v", ref, err)
		}
	}
	means := func(minPool int) skillstore.DimensionStat {
		t.Helper()
		stats, err := st.DimensionMeans(ctx, v.ID, time.Hour, minPool)
		if err != nil {
			t.Fatalf("DimensionMeans: %v", err)
		}
		for _, s := range stats {
			if s.Dimension == "correct_diagnosis" {
				return s
			}
		}
		t.Fatalf("no correct_diagnosis row in %+v", stats)
		return skillstore.DimensionStat{}
	}
	want := func(name string, s skillstore.DimensionStat, samples int, mean float64, excluded int) {
		t.Helper()
		if s.Samples != samples || math.Abs(s.Mean-mean) > 1e-9 || s.Excluded != excluded {
			t.Fatalf("%s: want samples=%d mean=%.3f excluded=%d, got %+v", name, samples, mean, excluded, s)
		}
	}
	// 1. Legacy rows only (no fingerprint): the filter is a no-op — both sessions pool, nothing excluded.
	write(tag+"-l1", "", []string{load}, 2)
	write(tag+"-l2", "", []string{load}, 4)
	want("legacy-only pool is unfiltered", means(2), 2, 3, 0)
	// 2. Fingerprinted configurations arrive: fpA (older, low) then fpB (newest, high), two judged sessions each.
	//    At minPool=2 the pool is fpB's and the four other judged sessions are excluded and counted; at minPool=3
	//    no configuration has accrued enough judged traffic to be a pool of its own, so the window pools as before.
	write(tag+"-a1", "fpA", []string{load}, 1)
	write(tag+"-a2", "fpA", []string{load}, 1)
	write(tag+"-b1", "fpB", []string{load}, 5)
	write(tag+"-b2", "fpB", []string{load}, 5)
	want("newest configuration with enough judged traffic is the pool", means(2), 2, 5, 4)
	want("below the threshold the window pools every configuration", means(3), 6, 18.0/6, 0)
	// 3. TWO trial-arm sessions on another fingerprint arrive newest — enough to clear minPool=2 on their own:
	//    they must never anchor (the candidate arm of a base-prompt trial carries its own fingerprint by
	//    construction). The pool stays fpB's; the arm sessions are judged, so they are excluded AND counted.
	write(tag+"-c1", "fpC", []string{trialLoad}, 1)
	write(tag+"-c1b", "fpC", []string{trialLoad}, 1)
	want("trial-arm sessions never anchor", means(2), 2, 5, 6)
	// 4. An UNJUDGED non-trial session on fpD arrives newest: no judgment ⇒ neither evidence nor an anchor, and
	//    not counted as excluded (the denominator is judged sessions).
	record(tag+"-d1", "fpD", []string{load})
	want("an unjudged session neither anchors nor counts", means(2), 2, 5, 6)
	// 5. Two judged NON-trial sessions on fpC make it the newest configuration with >= 2 non-trial judged
	//    sessions: the anchor moves; the arm sessions share the fingerprint and pool too; the rest is excluded.
	write(tag+"-c2", "fpC", []string{load}, 3)
	write(tag+"-c3", "fpC", []string{load}, 3)
	want("a newer configuration that reaches the threshold moves the anchor", means(2), 4, 8.0/4, 6)
	// 6. minPool <= 0 falls back to the default (20): nothing qualifies here, so the window pools unfiltered.
	want("non-positive minPool uses the default threshold", means(0), 10, (2+4+1+1+5+5+1+1+3+3)/10.0, 0)
}
