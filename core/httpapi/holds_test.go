package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/holds"
)

type fakeHolds struct {
	open   []holds.HoldDecision
	failed []holds.FailedInvestigation
	err    error
}

func (f fakeHolds) OpenHolds(context.Context, time.Time) ([]holds.HoldDecision, error) {
	return f.open, f.err
}
func (f fakeHolds) FailedInvestigationsUnretried(context.Context, time.Time) ([]holds.FailedInvestigation, error) {
	return f.failed, f.err
}

func getHolds(t *testing.T, d Deps) (*httptest.ResponseRecorder, HoldsPage) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/holds", nil)
	w := httptest.NewRecorder()
	d.holdsHandler(w, r, auth.Principal{SourceID: "operator:test"})
	var page HoldsPage
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, page
}

func TestHoldsHandlerRendersOpenHoldsAndFailedInvestigations(t *testing.T) {
	now := time.Now().UTC()
	d := Deps{Holds: fakeHolds{
		open: []holds.HoldDecision{{ExternalRef: "INC-1", Host: "app01", AlertRule: "Devices-up/down", OpClass: "start-guest",
			ReasonKey: "actor-attributed-authorized", Why: "Held: a person stopped it", DecidedBy: "tg:fallback-approver",
			DecidedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(22 * time.Hour)}},
		failed: []holds.FailedInvestigation{{ExternalRef: "INC-2", Host: "app02", AlertRule: "Service-up/down", StopReason: "gateway-dial", FailedAt: now.Add(-time.Hour)}},
	}}
	w, page := getHolds(t, d)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if len(page.Holds) != 1 || len(page.FailedInvestigations) != 1 {
		t.Fatalf("want 1 hold + 1 failed investigation, got %d + %d", len(page.Holds), len(page.FailedInvestigations))
	}
	h := page.Holds[0]
	if h.ExternalRef != "INC-1" || !h.Sticky || h.Reading == h.ReasonKey || h.AgeSeconds < 7000 {
		t.Fatalf("hold view lost its content: %+v", h)
	}
	if page.FailedInvestigations[0].StopReason != "gateway-dial" || page.FailedLookback != FailedInvestigationLookback.String() {
		t.Fatalf("failed-investigation view lost its content: %+v (lookback %q)", page.FailedInvestigations[0], page.FailedLookback)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a live read must not be cached")
	}
}

// Empty is a real answer with its own shape — both lists present and empty, never a 503 and never null.
func TestHoldsHandlerEmptyIsAnAnswer(t *testing.T) {
	w, _ := getHolds(t, Deps{Holds: fakeHolds{}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 on an empty queue, got %d", w.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["holds"]) != "[]" || string(raw["failed_investigations"]) != "[]" {
		t.Fatalf("empty lists must serialise as [] not null: %s", w.Body.String())
	}
}

// A missing or failing reader is 503, never an empty page: "nothing to show" and "cannot read" are different states.
func TestHoldsHandlerFailsClosedWithoutAReader(t *testing.T) {
	w, _ := getHolds(t, Deps{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader: want 503, got %d", w.Code)
	}
	w, _ = getHolds(t, Deps{Holds: fakeHolds{err: errors.New("db down")}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("read error: want 503, got %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/holds", nil)
	w = httptest.NewRecorder()
	Deps{Holds: fakeHolds{}}.holdsHandler(w, r, auth.Principal{})
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: want 405, got %d", w.Code)
	}
}
