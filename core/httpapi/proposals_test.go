package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

type fakeProposals struct {
	rows  []ShadowProposal
	total int // 0 = derive from rows (the common case); >0 = the store holds more than the page
	err   error
}

func (f fakeProposals) ShadowProposals(context.Context, auth.Principal, int) ([]ShadowProposal, int, error) {
	total := f.total
	if total == 0 {
		total = len(f.rows)
	}
	return f.rows, total, f.err
}

func getProposals(t *testing.T, d Deps) (*httptest.ResponseRecorder, ProposalsView) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/proposals", nil)
	w := httptest.NewRecorder()
	d.proposalsHandler(w, r, auth.Principal{SourceID: "operator:test"})
	var v ProposalsView
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w, v
}

// TestProposalsFailsClosedWithANilReader — spec/026 T-026-5's acceptance scenario verbatim: the surface
// 503s when nothing is wired, so the console renders "unavailable" honestly instead of an
// empty-but-plausible list (INV-15).
//
// RED mutation control (executed 2026-07-31): with the nil-reader guard removed the handler panics on
// the nil interface (caught as a test failure); restored green.
func TestProposalsFailsClosedWithANilReader(t *testing.T) {
	w, _ := getProposals(t, Deps{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader must 503 fail-closed, got %d", w.Code)
	}
}

// TestProposalsRendersRowsAndHonestTotalAndOffersNoVerb — the read surface serves the screened records
// with the REAL count, and its response contains no actuation-shaped affordance (this handler exposes GET
// only; the assertion pins the envelope's shape so a mutating field cannot ride in unnoticed).
func TestProposalsRendersRowsAndHonestTotalAndOffersNoVerb(t *testing.T) {
	rows := []ShadowProposal{{
		ExternalRef: "TG-shadow-1", Host: "svc01", AlertRule: "FluxDrift",
		Op: "rotate", OpClass: "rotate-flux-capacitor",
		Rationale: "observed drift", UndoSketch: "rotate back one notch",
		Confidence: 0.8, Attribution: "attributed-authorized", CreatedAt: time.Unix(2000, 0),
	}}
	w, v := getProposals(t, Deps{Proposals: fakeProposals{rows: rows}})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if v.Total != 1 || len(v.Proposals) != 1 || v.Proposals[0].OpClass != "rotate-flux-capacitor" {
		t.Fatalf("rows/total must render honestly: %+v", v)
	}
	// No mutating verb: POST is refused with Allow: GET.
	r := httptest.NewRequest(http.MethodPost, "/v1/proposals", strings.NewReader("{}"))
	wr := httptest.NewRecorder()
	Deps{Proposals: fakeProposals{}}.proposalsHandler(wr, r, auth.Principal{SourceID: "operator:test"})
	if wr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be method-not-allowed on a read-only surface, got %d", wr.Code)
	}
	// An empty spine is an empty list — never nil, never fabricated (INV-15).
	w2, v2 := getProposals(t, Deps{Proposals: fakeProposals{rows: nil}})
	if w2.Code != http.StatusOK || v2.Proposals == nil || len(v2.Proposals) != 0 || v2.Total != 0 {
		t.Fatalf("empty spine must render an empty list with total 0: code=%d %+v", w2.Code, v2)
	}
}

// TestProposalsTotalStaysHonestPastThePageLimit — the badge count is the STORE total, not the page size:
// with more rows in the store than the page returns, Total must exceed len(proposals). Guards the exact
// silent degradation INV-15 bans (a "real count" that quietly becomes "limit" at scale).
//
// RED mutation control (executed 2026-07-31): with the handler's Total reverted to len(rows), this fails
// "total must be the store count (7) past the page limit, got 2"; restored green.
func TestProposalsTotalStaysHonestPastThePageLimit(t *testing.T) {
	page := []ShadowProposal{{ExternalRef: "TG-a"}, {ExternalRef: "TG-b"}}
	_, v := getProposals(t, Deps{Proposals: fakeProposals{rows: page, total: 7}})
	if len(v.Proposals) != 2 || v.Total != 7 {
		t.Fatalf("total must be the store count (7) past the page limit, got %d (rows %d)", v.Total, len(v.Proposals))
	}
}

// fakeCounterfactual records the window it was asked for, so a test can prove the handler passes the
// real reporting period rather than a zero duration the reader would silently interpret as "all time".
type fakeCounterfactual struct {
	incidents, addressed, executed int
	err                  error
	gotWindow            *time.Duration
}

func (f fakeCounterfactual) Counterfactual(_ context.Context, _ auth.Principal, w time.Duration) (int, int, int, error) {
	if f.gotWindow != nil {
		*f.gotWindow = w
	}
	return f.incidents, f.addressed, f.executed, f.err
}

// TestCounterfactualIsAbsentRatherThanZeroWhenTheStoreCannotAnswer — the headline is BEST-EFFORT, and
// "best effort" must mean OMITTED, never zero-valued.
//
// This is the server half of a defect found in the console oracle on 2026-08-01: a counterfactual that
// defaults to {incidents: 0} on failure does not render as an error, it renders as "no incidents this
// week" — a clean bill of health asserted on a store that just failed. The console decides whether to
// draw the headline purely from the key's ABSENCE, so this handler omitting it is load-bearing, and the
// assertion below is on the encoded JSON (not the decoded struct) because `omitempty` is the mechanism
// the console actually reads.
//
// RED mutation control (executed 2026-08-01): populating the view on `cerr != nil` as well fails with
// `a failed counterfactual read must be OMITTED, but the response carries "counterfactual"`.
func TestCounterfactualIsAbsentRatherThanZeroWhenTheStoreCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"reader errors", Deps{Proposals: fakeProposals{}, Counterfactual: fakeCounterfactual{err: context.DeadlineExceeded}}},
		{"reader unwired", Deps{Proposals: fakeProposals{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/proposals", nil)
			w := httptest.NewRecorder()
			tc.deps.proposalsHandler(w, r, auth.Principal{SourceID: "operator:test"})
			if w.Code != http.StatusOK {
				t.Fatalf("a failed headline must not fail the whole surface, got %d", w.Code)
			}
			if strings.Contains(w.Body.String(), "counterfactual") {
				t.Fatalf("a failed counterfactual read must be OMITTED, but the response carries %q: %s",
					"counterfactual", w.Body.String())
			}
		})
	}
}

// TestCounterfactualReportsTheStoresNumbersAndTheRealWindow — on success the headline passes the store's
// two numbers through unaltered and declares the window it actually asked for. The window assertion is
// not ceremony: the reader turns it into `now() - window`, so a zero duration would return an empty
// window while the console captioned it "the last 0 days".
//
// RED mutation control (executed 2026-08-01): passing a literal 0 as the window fails with
// `handler must ask for the reporting window 168h0m0s, asked 0s`; restored green.
func TestCounterfactualReportsTheStoresNumbersAndTheRealWindow(t *testing.T) {
	var asked time.Duration
	_, v := getProposals(t, Deps{
		Proposals:      fakeProposals{},
		Counterfactual: fakeCounterfactual{incidents: 17, addressed: 14, gotWindow: &asked},
	})
	if v.Counterfactual == nil {
		t.Fatal("a successful read must produce a headline")
	}
	if v.Counterfactual.Incidents != 17 || v.Counterfactual.Addressed != 14 {
		t.Fatalf("the store's numbers must pass through verbatim, got %+v", *v.Counterfactual)
	}
	if asked != counterfactualWindow {
		t.Fatalf("handler must ask for the reporting window %v, asked %v", counterfactualWindow, asked)
	}
	if want := int(counterfactualWindow / (24 * time.Hour)); v.Counterfactual.WindowDays != want {
		t.Fatalf("window_days must describe the window queried (%d), got %d", want, v.Counterfactual.WindowDays)
	}
}

// TestCounterfactualNeverClaimsMoreAddressedThanIncidents — the headline reads "TG would have addressed
// A of I incidents"; A > I is not a rounding artifact, it is a sentence that cannot be true, and it would
// be the first sign the two counts had drifted onto different denominators (the numerator is a FILTER
// over the same scan as the denominator precisely so they cannot).
func TestCounterfactualNeverClaimsMoreAddressedThanIncidents(t *testing.T) {
	_, v := getProposals(t, Deps{
		Proposals:      fakeProposals{},
		Counterfactual: fakeCounterfactual{incidents: 3, addressed: 3},
	})
	if v.Counterfactual == nil || v.Counterfactual.Addressed > v.Counterfactual.Incidents {
		t.Fatalf("addressed must never exceed incidents, got %+v", v.Counterfactual)
	}
}
