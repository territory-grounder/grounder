package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// evidenceReader is a hand twin (no pgx) recording what it was asked for, so the authority test can prove the
// store was never REACHED — not merely that its answer was withheld.
type evidenceReader struct {
	rows  map[string]trace.AgentStepEvidence
	asked []string
}

func (e *evidenceReader) Evidence(_ context.Context, ref, id string) (trace.AgentStepEvidence, error) {
	e.asked = append(e.asked, ref+"/"+id)
	v, ok := e.rows[ref+"/"+id]
	if !ok {
		return trace.AgentStepEvidence{}, trace.ErrEvidenceNotFound
	}
	return v, nil
}

type detailReader struct {
	allow map[string]bool
}

func (d detailReader) SessionDetail(_ context.Context, _ auth.Principal, ref string) (trace.SessionTrace, error) {
	if !d.allow[ref] {
		return trace.SessionTrace{}, trace.ErrNotFound
	}
	return trace.SessionTrace{ExternalRef: ref}, nil
}

func evidenceRig(t *testing.T) (*evidenceReader, http.HandlerFunc) {
	t.Helper()
	er := &evidenceReader{rows: map[string]trace.AgentStepEvidence{
		"sess-1/ev-1": {
			ExternalRef: "sess-1", Cycle: 4, EvidenceID: "ev-1", Tool: "check-host-services",
			Payload: "● faultgen-restore-101101011.service loaded failed failed", FullBytes: 57,
		},
	}}
	d := Deps{SessionEvidenceRead: er, SessionDetailRead: detailReader{allow: map[string]bool{"sess-1": true}}}
	h := func(w http.ResponseWriter, r *http.Request) {
		d.sessionEvidenceHandler(w, r, auth.Principal{})
	}
	return er, h
}

func evidenceGet(t *testing.T, h http.HandlerFunc, ref, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+ref+"/evidence/"+id, nil)
	req = withEvidenceParams(req, ref, id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// KILLING MUTATION: serve the stored payload without resolving the session through the authority-bearing
// reader. RED — the evidence store is keyed by external_ref alone and knows nothing about principals, so
// skipping that resolve hands any caller who may read ONE session the observations of EVERY session.
func TestEvidenceForAnUnauthorizedSessionIsNotServedAndTheStoreIsNotEvenAsked(t *testing.T) {
	er, h := evidenceRig(t)
	er.rows["other-sess/ev-9"] = trace.AgentStepEvidence{ExternalRef: "other-sess", EvidenceID: "ev-9", Payload: "secret-ish host output"}

	rec := evidenceGet(t, h, "other-sess", "ev-9")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 for a session this principal may not read (body: %s)", rec.Code, rec.Body.String())
	}
	for _, a := range er.asked {
		if a == "other-sess/ev-9" {
			t.Fatal("the evidence store was QUERIED for an unauthorized session — authority must gate the read, " +
				"not merely filter its result")
		}
	}
}

// KILLING MUTATION: map a missing row to 503, or to a 200 with an empty payload. RED — every session recorded
// before migration 0053 has a walk and no evidence, and both of those answers are wrong about it: one says the
// platform is broken, the other says the tool returned nothing.
func TestAnUnrecordedObservationIs404NotAnErrorAndNotAnEmptyBody(t *testing.T) {
	_, h := evidenceRig(t)
	rec := evidenceGet(t, h, "sess-1", "ev-never-stored")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 — an unrecorded observation is an ordinary answer, not a fault", rec.Code)
	}
}

// The happy path, and the honesty fields with it.
func TestStoredGroundTruthIsServedWithItsSizeAndTool(t *testing.T) {
	_, h := evidenceRig(t)
	rec := evidenceGet(t, h, "sess-1", "ev-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var dto SessionEvidenceDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if dto.Payload == "" {
		t.Fatal("the served body carries no payload — the citation would open an empty panel")
	}
	if dto.Tool != "check-host-services" {
		t.Fatalf("tool=%q, want check-host-services — the panel names which tool produced this", dto.Tool)
	}
	if dto.Cycle != 4 {
		t.Fatalf("cycle=%d, want 4 — the panel states which cycle it belongs to", dto.Cycle)
	}
}

// KILLING MUTATION: fail closed to 200-with-nothing when no evidence store is wired. RED — a deployment
// running the tracer without the evidence store must SAY the surface is unavailable, not imply the agent
// never observed anything.
func TestNoEvidenceStoreIsAStatedOutageNotAnEmptyAnswer(t *testing.T) {
	d := Deps{SessionDetailRead: detailReader{allow: map[string]bool{"sess-1": true}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-1/evidence/ev-1", nil)
	req = withEvidenceParams(req, "sess-1", "ev-1")
	rec := httptest.NewRecorder()
	d.sessionEvidenceHandler(rec, req, auth.Principal{})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 with no evidence reader wired", rec.Code)
	}
}

// withEvidenceParams injects both chi path params for a direct-handler unit test — the same route-context
// pattern session_detail_test.go and ingest_test.go use.
func withEvidenceParams(r *http.Request, ref, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("external_ref", ref)
	rctx.URLParams.Add("evidence_id", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}
