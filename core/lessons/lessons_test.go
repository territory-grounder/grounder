package lessons

import (
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

func clean() ResolvedIncident {
	return ResolvedIncident{
		ExternalRef: "TG-1", Host: "web01", AlertRule: "NginxDown", Site: "nl",
		Summary: "nginx crashed", Action: "restart nginx", Verdict: safety.VerdictMatch, ConfirmedClear: true,
		Tags: []string{"web"},
	}
}

// Only a confirmed-clean outcome becomes a lesson; a deviation, a partial, or an unconfirmed session does not.
func TestLessonOnlyFromConfirmedClean(t *testing.T) {
	l, ok := Lesson(clean())
	if !ok || l.Resolution != "restart nginx" || l.AlertRule != "NginxDown" {
		t.Fatalf("a confirmed-clean match must become a lesson, got %+v ok=%v", l, ok)
	}
	// a deviation is never a lesson (reality diverged from the model)
	dev := clean()
	dev.Verdict = safety.VerdictDeviation
	if _, ok := Lesson(dev); ok {
		t.Fatal("a deviation must NOT become a lesson")
	}
	// a partial is not a clean success
	part := clean()
	part.Verdict = safety.VerdictPartial
	if _, ok := Lesson(part); ok {
		t.Fatal("a partial must NOT become a lesson")
	}
	// a match that was not confirmed clear (asserted success, not verified) is not a lesson
	unconf := clean()
	unconf.ConfirmedClear = false
	if _, ok := Lesson(unconf); ok {
		t.Fatal("an unconfirmed match must NOT become a lesson")
	}
	// no action / no ref → not a citable lesson
	noact := clean()
	noact.Action = ""
	if _, ok := Lesson(noact); ok {
		t.Fatal("a lesson with no action is not citable")
	}
}

// Distill keeps exactly the confirmed-clean subset, and the survivors are directly retrievable.
func TestDistillFeedsRetriever(t *testing.T) {
	dev := clean()
	dev.ExternalRef, dev.Verdict = "TG-2", safety.VerdictDeviation
	corpus := Distill([]ResolvedIncident{clean(), dev})
	if len(corpus) != 1 || corpus[0].ExternalRef != "TG-1" {
		t.Fatalf("only the clean incident should distill to a lesson, got %+v", corpus)
	}
	// the distilled lesson is retrievable as precedent for a similar incident
	hits := knowledge.NewLexicalRetriever(corpus).Retrieve(knowledge.Query{Host: "web01", AlertRule: "NginxDown"}, 5)
	if len(hits) != 1 || hits[0].Incident.Resolution != "restart nginx" {
		t.Fatalf("the distilled lesson must be retrievable, got %+v", hits)
	}
}
