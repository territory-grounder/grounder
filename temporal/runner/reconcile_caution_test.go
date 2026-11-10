package runner

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-579 (TG-52's live writer + the experience reflector): the caution/reflect seams fire on EXACTLY the
// complement of the precedent gate — an executed, terminal, poll-answered session that was NOT a confirmed-clean
// match — and never on the clean success (that is the precedent's) nor on a session that ran no action.
// KILLING MUTATION (executed, RED): drop the `!(... VerdictMatch && ConfirmedClear)` clause — the clean
// terminus then also writes a caution, and TestReconcileComplementNeverFiresOnCleanSuccess fails.
func TestReconcileComplementFiresOnNonClean(t *testing.T) {
	cases := map[string]func(in *ReconcileInput){
		"deviation":         func(in *ReconcileInput) { in.Verdict = safety.VerdictDeviation; in.ConfirmedClear = false },
		"partial":           func(in *ReconcileInput) { in.Verdict = safety.VerdictPartial; in.ConfirmedClear = false },
		"unconfirmed match": func(in *ReconcileInput) { in.ConfirmedClear = false },
	}
	for name, mutate := range cases {
		var cautions []lessons.ResolvedIncident
		var reflections []ReflectRequest
		acts := NewActivities(Deps{
			LearnCaution: func(_ context.Context, ri lessons.ResolvedIncident) error {
				cautions = append(cautions, ri)
				return nil
			},
			Reflect: func(_ context.Context, r ReflectRequest) error {
				reflections = append(reflections, r)
				return nil
			},
			LearnResolved: func(_ context.Context, _ lessons.ResolvedIncident) error {
				t.Fatalf("%s: a non-clean terminus must never write a precedent", name)
				return nil
			},
		})
		in := cleanWritebackInput()
		in.Prediction = "nginx active within 60s" // the committed prediction rides to the reflector (TG-579 review)
		mutate(&in)
		if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("%s: reconcile must not error: %v", name, err)
		}
		if len(cautions) != 1 || len(reflections) != 1 {
			t.Fatalf("%s: want exactly one caution and one reflection, got %d/%d", name, len(cautions), len(reflections))
		}
		if reflections[0].Prediction != "nginx active within 60s" || reflections[0].Incident.ExternalRef != "TG-124-1" {
			t.Fatalf("%s: the reflector must receive the committed prediction with the incident, got %+v", name, reflections[0])
		}
		ri := cautions[0]
		if ri.ExternalRef != "TG-124-1" || ri.Host != "librespeed01" || ri.AlertRule != "NginxDown" || ri.Action != "restart nginx" || ri.ResolvedAt.IsZero() {
			t.Fatalf("%s: caution signature drifted: %+v", name, ri)
		}
		// The emitted record is a CAUTION by lessons' own law, never a precedent — the partition holds.
		if _, isLesson := lessons.Lesson(ri); isLesson {
			t.Fatalf("%s: the complement must never distill to a precedent", name)
		}
		if _, isCaution := lessons.Caution(ri); !isCaution {
			t.Fatalf("%s: the complement must distill to a caution", name)
		}
	}
}

func TestReconcileComplementNeverFiresOnCleanSuccess(t *testing.T) {
	fired := 0
	acts := NewActivities(Deps{
		LearnCaution:  func(_ context.Context, _ lessons.ResolvedIncident) error { fired++; return nil },
		Reflect:       func(_ context.Context, _ ReflectRequest) error { fired++; return nil },
		LearnResolved: func(_ context.Context, _ lessons.ResolvedIncident) error { return nil },
	})
	if _, err := acts.ReconcileActivity(context.Background(), cleanWritebackInput()); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("a confirmed-clean match is the precedent's; the caution/reflect seams fired %d time(s)", fired)
	}
	// Nothing executed ⇒ nothing to be cautious about: a grounded stand-down fires neither seam.
	in := cleanWritebackInput()
	in.Executed, in.HasVerdict, in.Verdict, in.ConfirmedClear, in.Action = false, false, "", false, ""
	if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("a session that ran no action must fire neither seam, got %d", fired)
	}
	// Single-field perturbations (review): each precondition withholds the seams ON ITS OWN.
	for name, mutate := range map[string]func(in *ReconcileInput){
		"poll unanswered":    func(in *ReconcileInput) { in.PollUnanswered = true; in.ConfirmedClear = false },
		"no terminal result": func(in *ReconcileInput) { in.HasTerminalResult = false; in.ConfirmedClear = false },
		"not executed":       func(in *ReconcileInput) { in.Executed = false; in.ConfirmedClear = false },
	} {
		in := cleanWritebackInput()
		in.Verdict = safety.VerdictDeviation
		mutate(&in)
		if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fired != 0 {
			t.Fatalf("%s: a non-clean verdict must still be withheld when this precondition fails, fired %d", name, fired)
		}
	}
}

// A seam error never fails the terminus (best-effort, observe-only).
func TestReconcileComplementSeamErrorIsSwallowed(t *testing.T) {
	acts := NewActivities(Deps{
		LearnCaution: func(_ context.Context, _ lessons.ResolvedIncident) error { return context.DeadlineExceeded },
		Reflect:      func(_ context.Context, _ ReflectRequest) error { return context.DeadlineExceeded },
	})
	in := cleanWritebackInput()
	in.Verdict = safety.VerdictDeviation
	in.ConfirmedClear = false
	if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
		t.Fatalf("a caution/reflect seam error must never fail the terminus: %v", err)
	}
}
