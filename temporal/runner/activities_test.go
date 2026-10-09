package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
)

// TestRecordTriageActivityScreensModelText — REQ-2606 on the ORDINARY terminal path.
//
// THE REGRESSION THIS PINS. ShadowProposalActivity screened; RecordTriageActivity did not. That gap was
// invisible while the console's proposals predicate was `outcome = 'proposed:shadow'`, because that
// predicate selects exactly the screened branch. Broadening it to `LIKE 'proposed%'` on 2026-08-01 — to
// fix a surface rendering 1 row over 1,484 — put the unscreened path's text on an operator's screen the
// same day. Nothing had leaked (production carried 0 credential-shaped rows, checked), but a latent
// exposure is still an exposure, and the invariant said "before persist".
//
// RED MUTATION CONTROL (executed 2026-08-01): removing the screen.Scrub block from RecordTriageActivity
// fails with the raw secret still present in the persisted row; restored green.
func TestRecordTriageActivityScreensModelText(t *testing.T) {
	var got judge.TriageRow
	a := &Activities{D: Deps{TriageRecord: func(_ context.Context, r judge.TriageRow) error {
		got = r
		return nil
	}}}

	const secret = "password=hunter2correcthorse"
	res, err := a.RecordTriageActivity(context.Background(), judge.TriageRow{
		ExternalRef: "librenms-1",
		Host:        "dc1mealie01", // identifier grammar — deliberately NOT screened
		AlertRule:   "Service up/down",
		Op:          "restart nginx " + secret,
		OpClass:     "restart-service",
		Conclusion:  "the unit died; " + secret,
		UndoSketch:  "stop the unit; " + secret,
		StopReason:  "gave up: " + secret,
		Prediction:  "will recur; " + secret,
	})
	if err != nil || !res.Recorded {
		t.Fatalf("record: err=%v res=%+v", err, res)
	}

	for _, f := range []struct{ name, val string }{
		{"Op", got.Op},
		{"Conclusion", got.Conclusion},
		{"UndoSketch", got.UndoSketch},
		{"StopReason", got.StopReason},
		{"Prediction", got.Prediction},
	} {
		if strings.Contains(f.val, "hunter2correcthorse") {
			t.Errorf("%s reached the persisted row UNSCREENED (%q). REQ-2606 requires screen.Scrub before "+
				"persist, ledger and console render — and since 2026-08-01 the console renders exactly "+
				"these rows.", f.name, f.val)
		}
	}
	// The converse: screening must not blank a field that carried no secret, or the console renders
	// redactions over ordinary text and operators learn to distrust the marker.
	if got.OpClass != "restart-service" {
		t.Errorf("a clean field must survive screening unchanged, got %q", got.OpClass)
	}
	if got.Host != "dc1mealie01" || got.AlertRule != "Service up/down" {
		t.Errorf("identifier-grammar fields are ingest-validated and must not be screened, got host=%q rule=%q",
			got.Host, got.AlertRule)
	}
}
