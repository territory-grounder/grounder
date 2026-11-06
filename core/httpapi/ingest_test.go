package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

type fakeIngester struct {
	src     string
	env     coreingest.IncidentEnvelope
	normErr error
}

func (f fakeIngester) SourceType() string { return f.src }
func (f fakeIngester) Normalize(context.Context, []byte) (coreingest.IncidentEnvelope, error) {
	return f.env, f.normErr
}

type fakeResolver struct {
	byType map[string]ingestadapter.Ingester
}

func (f fakeResolver) ResolveIngester(sourceType string) (ingestadapter.Ingester, error) {
	ing, ok := f.byType[sourceType]
	if !ok {
		return nil, errors.New("no execution path")
	}
	return ing, nil
}

type fakeTriage struct {
	id  string
	err error
}

func (f fakeTriage) StartTriage(context.Context, coreingest.IncidentEnvelope) (string, error) {
	return f.id, f.err
}

func ingestReq(sourceType string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/ingest/"+sourceType, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("source_type", sourceType)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestIngestHandler(t *testing.T) {
	resolver := fakeResolver{byType: map[string]ingestadapter.Ingester{
		"crowdsec": fakeIngester{src: "crowdsec", env: coreingest.IncidentEnvelope{ExternalRef: "inc-1"}},
		"bad":      fakeIngester{src: "bad", normErr: errors.New("grammar violation")},
	}}

	t.Run("known source + triage wired mints the workflow (202 + triggered + id)", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver, Triage: fakeTriage{id: "tg/inc-1"}}.ingestHandler(w, ingestReq("crowdsec"), auth.Principal{SourceID: "crowdsec-nl"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
		var got IngestAccepted
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.Accepted || got.SourceType != "crowdsec" || got.ExternalRef != "inc-1" || !got.Triggered || got.WorkflowID != "tg/inc-1" {
			t.Errorf("unexpected response: %+v", got)
		}
	})

	t.Run("no triage backend wired → accepted + normalized, not triggered (validate-only)", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver}.ingestHandler(w, ingestReq("crowdsec"), auth.Principal{})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
		var got IngestAccepted
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if !got.Accepted || got.Triggered || got.WorkflowID != "" {
			t.Errorf("expected accepted-but-not-triggered, got: %+v", got)
		}
	})

	t.Run("triage backend failure is 502 (not a silent drop)", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver, Triage: fakeTriage{err: errors.New("temporal down")}}.ingestHandler(w, ingestReq("crowdsec"), auth.Principal{})
		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", w.Code)
		}
	})

	t.Run("unregistered/disabled source is 404 (no execution path, INV-17)", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver}.ingestHandler(w, ingestReq("nope"), auth.Principal{})
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("grammar violation is 400 (rejected, never coerced, INV-04)", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver}.ingestHandler(w, ingestReq("bad"), auth.Principal{})
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("nil resolver fails closed to 503", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{}.ingestHandler(w, ingestReq("crowdsec"), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
	})
}

// countingTriage records whether StartTriage was called (a recovery must NOT mint a session).
type countingTriage struct{ calls int }

func (c *countingTriage) StartTriage(context.Context, coreingest.IncidentEnvelope) (string, error) {
	c.calls++
	return "tg/should-not-be-called", nil
}

// captureTransitions records the recovery transitions the front door captures.
type captureTransitions struct{ recs []TransitionRecord }

func (c *captureTransitions) Append(_ context.Context, rec TransitionRecord) { c.recs = append(c.recs, rec) }

// TestIngestRecoveryRouting: a RECOVERY-labelled envelope is captured to the transition recorder as
// clear-evidence and does NOT mint a triage session (it would only dedup into the finished fault workflow);
// a FAULT envelope (no recovery label) mints triage and is NOT captured as a transition. Both return 202.
func TestIngestRecoveryRouting(t *testing.T) {
	recoveryEnv := coreingest.IncidentEnvelope{
		ExternalRef: "librenms-nl-42", Host: "web01", Site: "nl", AlertRule: "Devices-up/down",
		Labels: map[string]string{coreingest.LabelTransition: coreingest.TransitionRecovery},
	}
	faultEnv := coreingest.IncidentEnvelope{ExternalRef: "librenms-nl-42", Host: "web01", Site: "nl", AlertRule: "Devices-up/down"}
	resolver := func(env coreingest.IncidentEnvelope) fakeResolver {
		return fakeResolver{byType: map[string]ingestadapter.Ingester{"librenms": fakeIngester{src: "librenms", env: env}}}
	}

	t.Run("recovery is captured, not triaged", func(t *testing.T) {
		tr := &countingTriage{}
		cap := &captureTransitions{}
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver(recoveryEnv), Triage: tr, Transitions: cap}.ingestHandler(w, ingestReq("librenms"), auth.Principal{})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
		if tr.calls != 0 {
			t.Errorf("a recovery must NOT mint a triage session, StartTriage called %d time(s)", tr.calls)
		}
		if len(cap.recs) != 1 || cap.recs[0].ExternalRef != "librenms-nl-42" || cap.recs[0].Kind != coreingest.TransitionRecovery || cap.recs[0].Host != "web01" {
			t.Errorf("recovery not captured as clear-evidence: %+v", cap.recs)
		}
		var got IngestAccepted
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got.Triggered {
			t.Errorf("a recovery must report Triggered=false, got %+v", got)
		}
	})

	t.Run("fault is triaged, not captured as a transition", func(t *testing.T) {
		tr := &countingTriage{}
		cap := &captureTransitions{}
		w := httptest.NewRecorder()
		Deps{Ingesters: resolver(faultEnv), Triage: tr, Transitions: cap}.ingestHandler(w, ingestReq("librenms"), auth.Principal{})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
		if tr.calls != 1 {
			t.Errorf("a fault must mint exactly one triage session, StartTriage called %d time(s)", tr.calls)
		}
		if len(cap.recs) != 0 {
			t.Errorf("a fault must NOT be captured as a recovery transition: %+v", cap.recs)
		}
	})
}
