package runner

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/lessons"
	"github.com/territory-grounder/grounder/core/safety"
)

// cleanWritebackInput is a confirmed-clean terminus carrying the exact novelty (host, alert_rule) signature
// the classifier read (Host = the incident subject env.Host, AlertRule = env.AlertRule) plus the op that
// resolved it.
func cleanWritebackInput() ReconcileInput {
	return ReconcileInput{
		ExternalRef:       "TG-124-1",
		SessionID:         "sess-1",
		Band:              safety.BandPollPause, // the FIRST-occurrence de-novel case is a human-approved POLL_PAUSE
		HasVerdict:        true,
		Verdict:           safety.VerdictMatch,
		ConfirmedClear:    true,
		HasTerminalResult: true,
		Executed:          true,
		Host:              "librespeed01", // the incident SUBJECT (env.Host) — the stable key knowledge.Count reads
		AlertRule:         "NginxDown",    // env.AlertRule
		Site:              "nl",
		Summary:           "nginx worker crashed",
		Action:            "restart nginx",
	}
}

// TestReconcileWritebackEmitsOnConfirmedClean proves a CONFIRMED-CLEAN terminus emits exactly one correctly
// signed ResolvedIncident — keyed on the incident-subject host (env.Host) + env alert rule, carrying the resolving op — and
// that the emitted record distills to a real lesson (the writeback is band-INDEPENDENT: this drives a
// POLL_PAUSE-band resolution, which the reconciler routes To Verify, yet the precedent must still be written,
// or the first-occurrence incident would stay novel forever — the exact TG-124 bug).
func TestReconcileWritebackEmitsOnConfirmedClean(t *testing.T) {
	for _, band := range []safety.Band{safety.BandPollPause, safety.BandAuto, safety.BandAutoNotice} {
		var got []lessons.ResolvedIncident
		acts := NewActivities(Deps{
			LearnResolved: func(_ context.Context, ri lessons.ResolvedIncident) error {
				got = append(got, ri)
				return nil
			},
		})
		in := cleanWritebackInput()
		in.Band = band
		if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("band %v: reconcile must not error: %v", band, err)
		}
		if len(got) != 1 {
			t.Fatalf("band %v: a confirmed-clean terminus must emit exactly one resolved incident, got %d", band, len(got))
		}
		ri := got[0]
		if ri.ExternalRef != "TG-124-1" || ri.Host != "librespeed01" || ri.AlertRule != "NginxDown" || ri.Action != "restart nginx" {
			t.Fatalf("band %v: writeback signature drifted from the classifier read shape: %+v", band, ri)
		}
		if ri.Verdict != safety.VerdictMatch || !ri.ConfirmedClear {
			t.Fatalf("band %v: writeback must carry the clean verdict + confirmed clear, got %+v", band, ri)
		}
		// The emitted record must actually distill to a lesson (the persistence gate accepts it).
		if _, ok := lessons.Lesson(ri); !ok {
			t.Fatalf("band %v: the emitted ResolvedIncident must distill to a lesson, got %+v", band, ri)
		}
	}
}

// TestReconcileWritebackSuppressedOnNonClean proves the writeback fails CLOSED: a deviation, a partial, an
// unconfirmed match, a crash (no terminal result), an orphaned poll, and a missing verdict each emit NOTHING —
// a false precedent would silence novelty for an incident TG did not actually resolve.
func TestReconcileWritebackSuppressedOnNonClean(t *testing.T) {
	deviation := cleanWritebackInput()
	deviation.Verdict = safety.VerdictDeviation

	partial := cleanWritebackInput()
	partial.Verdict = safety.VerdictPartial

	unconfirmed := cleanWritebackInput()
	unconfirmed.ConfirmedClear = false

	noTerminal := cleanWritebackInput()
	noTerminal.HasTerminalResult = false

	orphanedPoll := cleanWritebackInput()
	orphanedPoll.PollUnanswered = true

	noVerdict := cleanWritebackInput()
	noVerdict.HasVerdict = false
	noVerdict.Verdict = "" // asserted clear with no mechanical verdict is not a verified resolution

	// C2 (flywheel-audit): a session that never EXECUTED a mutation must not de-novel, even if it reports a
	// clean match + confirmed-clear — the loop learns ONLY from a genuinely executed+verified resolution.
	unexecuted := cleanWritebackInput()
	unexecuted.Executed = false

	cases := map[string]ReconcileInput{
		"deviation":         deviation,
		"partial":           partial,
		"unconfirmed-clear": unconfirmed,
		"no-terminal-crash": noTerminal,
		"orphaned-poll":     orphanedPoll,
		"no-verdict":        noVerdict,
		"unexecuted":        unexecuted,
	}
	for name, in := range cases {
		var emitted int
		acts := NewActivities(Deps{
			LearnResolved: func(_ context.Context, _ lessons.ResolvedIncident) error {
				emitted++
				return nil
			},
		})
		if _, err := acts.ReconcileActivity(context.Background(), in); err != nil {
			t.Fatalf("%s: reconcile must not error: %v", name, err)
		}
		if emitted != 0 {
			t.Fatalf("%s: a non-confirmed-clean terminus must emit NO precedent, got %d", name, emitted)
		}
	}
}

// TestReconcileWritebackNilSeamIsSafe proves a nil LearnResolved seam (the no-corpus oracle path) never panics
// and the terminus still completes — the writeback is best-effort wiring, not a load-bearing dependency.
func TestReconcileWritebackNilSeamIsSafe(t *testing.T) {
	acts := NewActivities(Deps{}) // no LearnResolved wired
	if _, err := acts.ReconcileActivity(context.Background(), cleanWritebackInput()); err != nil {
		t.Fatalf("a nil writeback seam must be a safe no-op, got %v", err)
	}
}
