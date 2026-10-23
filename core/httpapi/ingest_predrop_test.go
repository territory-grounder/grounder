package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
)

// TG-380: an alert the front door ACCEPTS (2xx) but does not turn into a new triage session is a
// pre-admission drop — the "the upstream sent more than we triaged" signal the pve03 cascade could not
// measure. The recovery-transition path is the cleanly-exercisable one: a recovery alert returns 202 and
// mints no triage.
//
// KILLING MUTATION (executed 2026-08-11): delete the `d.IngestPredrop("recovery_transition")` call in the
// recovery branch — TestARecoveryTransitionIsCountedAsPredrop fails ("recovery accepted but NOT counted").
// Restore → green.

type predropSpy struct{ reasons []string }

func (s *predropSpy) record(reason string) { s.reasons = append(s.reasons, reason) }

func recoveryResolver() fakeResolver {
	return fakeResolver{byType: map[string]ingestadapter.Ingester{
		"crowdsec": fakeIngester{src: "crowdsec", env: coreingest.IncidentEnvelope{
			ExternalRef: "inc-recovered",
			Labels:      map[string]string{coreingest.LabelTransition: coreingest.TransitionRecovery},
		}},
	}}
}

func TestARecoveryTransitionIsCountedAsPredrop(t *testing.T) {
	spy := &predropSpy{}
	// A recovery envelope: accepted, captured as clear-evidence, but mints no triage. Triage is wired to
	// prove it is NOT called for a recovery (a triage would mean the drop did not happen).
	d := Deps{Ingesters: recoveryResolver(), Triage: fakeTriage{id: "tg/should-not-be-used"}, IngestPredrop: spy.record}

	w := httptest.NewRecorder()
	d.ingestHandler(w, ingestReq("crowdsec"), auth.Principal{SourceID: "crowdsec-nl"})

	if w.Code != http.StatusAccepted {
		t.Fatalf("a recovery transition must be accepted (202), got %d", w.Code)
	}
	if len(spy.reasons) != 1 || spy.reasons[0] != "recovery_transition" {
		t.Fatalf("the recovery drop was NOT counted as a predrop (got %v). An accepted alert that mints no "+
			"triage is invisible without this — the exact 'sent more than we triaged' gap TG-380 closes.", spy.reasons)
	}
}

func TestANilPredropStillAcceptsRecovery(t *testing.T) {
	// The seam is optional; a nil counter must not change the accept behaviour.
	d := Deps{Ingesters: recoveryResolver(), IngestPredrop: nil}
	w := httptest.NewRecorder()
	d.ingestHandler(w, ingestReq("crowdsec"), auth.Principal{SourceID: "crowdsec-nl"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("a nil predrop counter must not change accept behaviour: got %d", w.Code)
	}
}
