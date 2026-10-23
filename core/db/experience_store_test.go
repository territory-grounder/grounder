package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/reflect"
)

// TG-579: the reflector's record round-trips through the REAL pgx INSERT/SELECT (first write wins), and the
// lesson-feed adapter admits only experiences at or above the configured confidence, rendered as the lesson the
// generator incorporates. DSN-gated (two-DB recipe); rows are uniquely named and left.
func TestExperienceStoreRoundTripAndLessonFeed(t *testing.T) {
	ctx, p, done := openFixture(t)
	defer done()
	st := NewExperienceStore(p)
	tag := fmt.Sprintf("tg579test-%d", time.Now().UnixNano())
	hi := reflect.Experience{ExternalRef: tag + "-hi", Host: "web01", AlertRule: "NginxDown", Verdict: "deviation",
		Cue: "a service-down alert where the unit reads inactive (dead)", Lesson: "confirm the unit was not stopped deliberately before proposing a restart",
		Evidence: "steps 1-2: inactive (dead) then a deviation after restart", Confidence: "high", Moments: "step: 1\nverdict: hurt", Model: "primary"}
	lo := hi
	lo.ExternalRef, lo.Confidence = tag+"-lo", "low"
	lo.Lesson = "a DISTINCT low-confidence lesson (the feed dedupes identical lesson text, so this one must differ)"
	for _, e := range []reflect.Experience{hi, lo} {
		if err := st.Write(ctx, e); err != nil {
			t.Fatalf("Write %s: %v", e.ExternalRef, err)
		}
	}
	// First write wins: a reconcile retry never rewrites a reflection.
	dup := hi
	dup.Lesson = "REWRITTEN"
	if err := st.Write(ctx, dup); err != nil {
		t.Fatalf("duplicate Write must be an idempotent no-op: %v", err)
	}
	var gotLesson, gotConf, gotMoments string
	var created time.Time
	if err := p.QueryRow(ctx, `SELECT lesson, confidence, moments, created_at FROM session_experience WHERE external_ref = $1`, hi.ExternalRef).
		Scan(&gotLesson, &gotConf, &gotMoments, &created); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotLesson != hi.Lesson || gotConf != "high" || gotMoments != hi.Moments || created.IsZero() {
		t.Fatalf("high-confidence row did not round-trip intact (first write must win): lesson=%q conf=%s moments=%q", gotLesson, gotConf, gotMoments)
	}
	// The lesson feed at the default minimum (medium) admits the high row and not the low one.
	src := NewExperienceLessonSource(p, "triage-protocol", "correct_diagnosis", "medium", 100)
	incs, err := src.NotableIncidents(ctx, time.Hour)
	if err != nil {
		t.Fatalf("NotableIncidents: %v", err)
	}
	seen := map[string]string{}
	for _, inc := range incs {
		seen[inc.ExternalRef] = inc.Lesson
		if inc.TargetSkill != "triage-protocol" || inc.TargetDimension != "correct_diagnosis" {
			t.Errorf("lesson target drifted: %+v", inc)
		}
	}
	if l, ok := seen[hi.ExternalRef]; !ok || !strings.Contains(l, hi.Lesson) || !strings.Contains(l, "confidence high") {
		t.Errorf("the high-confidence experience must feed the flywheel as a lesson carrying its text, got %q (present=%v)", l, ok)
	}
	if _, ok := seen[lo.ExternalRef]; ok {
		t.Errorf("a low-confidence experience must NOT be admitted at minimum confidence medium")
	}
	// Identical lessons from different sessions feed the flywheel ONCE (review finding 8).
	twin := hi
	twin.ExternalRef = tag + "-twin"
	if err := st.Write(ctx, twin); err != nil {
		t.Fatal(err)
	}
	if incs, err := NewExperienceLessonSource(p, "triage-protocol", "correct_diagnosis", "high", 100).NotableIncidents(ctx, time.Hour); err != nil {
		t.Fatal(err)
	} else {
		n := 0
		for _, inc := range incs {
			if strings.Contains(inc.Lesson, hi.Lesson) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("an identical lesson from two sessions must feed the flywheel once, got %d", n)
		}
	}
	// Lowering the floor admits it; an unconfigured target yields nothing (dormant).
	if incs, err := NewExperienceLessonSource(p, "triage-protocol", "correct_diagnosis", "low", 100).NotableIncidents(ctx, time.Hour); err != nil {
		t.Fatal(err)
	} else {
		found := false
		for _, inc := range incs {
			if inc.ExternalRef == lo.ExternalRef {
				found = true
			}
		}
		if !found {
			t.Errorf("minimum confidence low must admit the low-confidence experience")
		}
	}
	if incs, err := NewExperienceLessonSource(p, "", "", "low", 100).NotableIncidents(ctx, time.Hour); err != nil || len(incs) != 0 {
		t.Errorf("an unconfigured target must yield nothing (dormant), got %d err=%v", len(incs), err)
	}
}
