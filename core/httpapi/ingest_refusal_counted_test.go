package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// withChiParam injects a chi URL param so the handler resolves the source exactly as the router would.
func withChiParam(r *http.Request, k, v string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(k, v)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// TG-371. The front door published FIFTEEN tg_ingest_* families and every one measured ACCEPTANCE or
// upstream reachability — none counted a refusal. So three very different situations produced one
// observable (`tg_ingest_source_last_seen_seconds` growing):
//
//	1. the source genuinely has nothing to send        (healthy)
//	2. its token rotated and every POST is turned away (broken auth)
//	3. its payload stopped satisfying the grammar      (broken producer)
//
// Only the first is fine, and it is the one that leaves no trace anywhere else either.
//
// These drive the REAL handler over httptest rather than asserting on source, because the property is
// "a refused delivery is counted", and a counter wired to a branch the handler does not take is the
// present-but-not-reaching defect this repo keeps producing.

type refusalTally struct{ hits []([2]string) }

func (c *refusalTally) record(sourceType, reason string) {
	c.hits = append(c.hits, [2]string{sourceType, reason})
}

// TestAnUnknownSourceIsCountedAsRefused is the finding: a source TG has no capability for is a 404, and
// until now that 404 left no trace anywhere.
func TestAnUnknownSourceIsCountedAsRefused(t *testing.T) {
	c := &refusalTally{}
	d := Deps{IngestRefused: c.record} // no Ingesters ⇒ the earliest refusal path

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/librenms", strings.NewReader("{}"))
	d.ingestHandler(w, r, auth.Principal{})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (no ingester registry)", w.Code)
	}
	if len(c.hits) != 1 {
		t.Fatalf("the refusal was NOT counted (%d tallies). A 503 that leaves no trace is exactly the "+
			"silence this closes: the source sees an error, TG's metrics see nothing, and "+
			"last_seen_seconds grows identically to a quiet estate.", len(c.hits))
	}
	if c.hits[0][1] != "no_ingester_registry" {
		t.Errorf("reason = %q, want no_ingester_registry — the reason is what distinguishes broken auth "+
			"from a broken payload from a TG-side outage, and those are three different people's work",
			c.hits[0][1])
	}
}

// TestTheRefusalNamesTheSource. "Something was refused" does not tell an operator which feed went dark.
// The source is resolved BEFORE the first refusal specifically so this holds on every path.
func TestTheRefusalNamesTheSource(t *testing.T) {
	c := &refusalTally{}
	d := Deps{IngestRefused: c.record}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/prometheus-alertmanager", strings.NewReader("{}"))
	r = withChiParam(r, "source_type", "prometheus-alertmanager")
	d.ingestHandler(w, r, auth.Principal{})

	if len(c.hits) != 1 {
		t.Fatalf("expected exactly one refusal, got %d", len(c.hits))
	}
	if c.hits[0][0] != "prometheus-alertmanager" {
		t.Errorf("source_type = %q, want prometheus-alertmanager. A refusal counter without the source "+
			"only says that SOMETHING was rejected — it cannot say which feed stopped landing, which is "+
			"the question this was built to answer.", c.hits[0][0])
	}
}

// TestANilCounterStillRefuses. The seam is optional (a deployment with no metrics sink), and an optional
// dependency that changes REFUSAL behaviour would be a safety regression dressed as instrumentation.
func TestANilCounterStillRefuses(t *testing.T) {
	d := Deps{} // IngestRefused nil
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/librenms", strings.NewReader("{}"))
	d.ingestHandler(w, r, auth.Principal{})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d with a nil counter, want 503 — instrumentation must never change whether "+
			"the door refuses", w.Code)
	}
}
