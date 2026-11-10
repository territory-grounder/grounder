package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/persist"
)

// fakePending is an in-test persist.PendingReader.
type fakePending struct {
	rows []persist.PendingDecision
	err  error
}

func (f fakePending) OpenDecisions(context.Context) ([]persist.PendingDecision, error) {
	return f.rows, f.err
}
func (f fakePending) CountOpen(context.Context) (int, error) { return len(f.rows), f.err }

func getDecisions(t *testing.T, d Deps, sourceID string) (*httptest.ResponseRecorder, DecisionsPage) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	w := httptest.NewRecorder()
	d.decisionsHandler(w, r, auth.Principal{SourceID: sourceID})
	var page DecisionsPage
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, page
}

func TestDecisionsHandler(t *testing.T) {
	row := persist.PendingDecision{
		ExternalRef: "TG-1", ActionID: "a1b2", Band: "POLL_PAUSE",
		Approaches: []string{"restart-service restart on web01"}, Prediction: "web01 recovers; deps unaffected",
		Reversible: true, Site: "dc1", OpenedAt: time.Unix(1000, 0), Status: persist.DecisionOpen,
	}
	d := Deps{PendingDecisions: fakePending{rows: []persist.PendingDecision{row}}}

	// An operator session may act on the listed decision (it can reach /v1/vote).
	w, page := getDecisions(t, d, "operator:kyriakos")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if len(page.Decisions) != 1 {
		t.Fatalf("want 1 decision, got %d", len(page.Decisions))
	}
	got := page.Decisions[0]
	if got.ActionID != "a1b2" || got.ExternalRef != "TG-1" || got.DecisionID != "TG-1" {
		t.Fatalf("decision identity lost: %+v", got)
	}
	if !got.CallerCanAct {
		t.Fatalf("an operator session must be able to act on a pending decision")
	}
	if len(got.Plan.Approaches) != 1 || got.Plan.Approaches[0] != "restart-service restart on web01" {
		t.Fatalf("plan approaches lost: %+v", got.Plan)
	}
	if got.Band != "POLL_PAUSE" || !got.Reversible {
		t.Fatalf("decision fields lost: %+v", got)
	}

	// A machine principal sees the queue read-only — the control is never offered where it cannot be used.
	_, page2 := getDecisions(t, d, "hmac:prometheus-dc1")
	if len(page2.Decisions) != 1 || page2.Decisions[0].CallerCanAct {
		t.Fatalf("a machine principal must NOT be authorized to act: %+v", page2.Decisions)
	}

	// A nil reader fails closed to 503 — never a fabricated empty list.
	if w3, _ := getDecisions(t, Deps{}, "operator:kyriakos"); w3.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader must be 503, got %d", w3.Code)
	}

	// A store error fails closed to 503.
	if w4, _ := getDecisions(t, Deps{PendingDecisions: fakePending{err: context.DeadlineExceeded}}, "operator:kyriakos"); w4.Code != http.StatusServiceUnavailable {
		t.Fatalf("store error must be 503, got %d", w4.Code)
	}

	// A non-GET is rejected 405.
	wp := httptest.NewRecorder()
	d.decisionsHandler(wp, httptest.NewRequest(http.MethodPost, "/v1/decisions", nil), auth.Principal{SourceID: "operator:kyriakos"})
	if wp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be 405, got %d", wp.Code)
	}
}
