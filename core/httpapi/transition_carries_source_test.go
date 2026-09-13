package httpapi

import (
	"testing"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// TG-393. The freshness union keys on ingest_transition.source_id, and that column is only ever
// populated by this projector. A DB-level oracle that INSERTs its own rows proves the read and says
// nothing about the write — which is exactly what happened: the mutation `SourceID: ""` survived the
// database test and would have silently returned freshness to raises-only in production.
//
// This is the write half. It is deliberately in core/httpapi, beside the projector, so the two cannot
// drift apart the way the read oracle and the write path did.
func TestTransitionProjectorCarriesTheSource(t *testing.T) {
	env := coreingest.IncidentEnvelope{
		ExternalRef: "tg393-ref",
		SourceID:    "librenms-dc2",
		Host:        "h1",
		Site:        "GR",
		AlertRule:   "port-status",
	}
	rec := transitionFromEnvelope(env)

	if rec.SourceID != "librenms-dc2" {
		t.Fatalf("the transition projector dropped SourceID (got %q). ingest_transition.source_id is "+
			"populated ONLY here, and the per-source freshness union filters on it — so an empty value "+
			"silently returns freshness to raises-only, which is the defect TG-393 measured: a source "+
			"delivering nothing but recoveries reads as SILENT and fires AlertSourceWentSilent while "+
			"healthy.", rec.SourceID)
	}
	// The envelope's own fields must still ride along — a projector that carried only the source would
	// break the clear-evidence the Runner reads (spec/012).
	if rec.ExternalRef != env.ExternalRef || rec.Host != env.Host || rec.AlertRule != env.AlertRule {
		t.Errorf("the projector corrupted the record while adding the source: %+v", rec)
	}
	if rec.Kind != coreingest.TransitionRecovery {
		t.Errorf("Kind = %q, want the recovery constant", rec.Kind)
	}
}

// TestTransitionProjectorPassesAnEmptySourceThrough. An envelope with no SourceID must yield an empty
// one rather than a fabricated default — the writer turns "" into SQL NULL, and the union skips NULLs.
// Inventing a source here would attribute a recovery to the wrong feed.
func TestTransitionProjectorPassesAnEmptySourceThrough(t *testing.T) {
	rec := transitionFromEnvelope(coreingest.IncidentEnvelope{ExternalRef: "tg393-nosrc", Host: "h"})
	if rec.SourceID != "" {
		t.Errorf("SourceID = %q for an envelope that carries none — a fabricated source attributes a "+
			"recovery to a feed that did not deliver it, which is worse than not counting it", rec.SourceID)
	}
}
