package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/opclasscat"
)

// This file is the oracle GET /v1/opclass/candidates never had.
//
// Its absence is why the queue shipped serving occurrences=0 / hosts=0 for every row while the console
// faithfully rendered "0x / 0 host(s)" over shapes with real evidence behind them, and why Total was the
// page length rather than the queue length. The dossier handler computed both correctly, so every
// per-candidate test passed; nothing exercised the LIST. The console e2e did not catch it either, because
// it stubs the API with counts already populated — a test that asserts the renderer can display a number
// the server never sends.
//
// TG-236 oracles 2 (shape over chronology) and 3 (the journey made visible) both READ these fields, so
// they are load-bearing now, not cosmetic.

type queueReader struct {
	page OpClassCandidatePage
	err  error
}

func (q queueReader) OpClassCandidates(context.Context, int) (OpClassCandidatePage, error) {
	return q.page, q.err
}

func (q queueReader) OpClassDossier(context.Context, string) (opclasscat.Candidate, []opclasscat.Occurrence, error) {
	return opclasscat.Candidate{}, nil, ErrOpClassCandidateNotFound
}

func queueRequest(t *testing.T, r OpClassCandidateReader) OpClassPage {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/opclass/candidates", nil)
	Deps{OpClass: r}.opClassCandidatesHandler(rec, req, auth.Principal{SourceID: "operator:t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got OpClassPage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestQueueRowsCarryTheRealRecurrenceCounts is the direct regression oracle for the shipped defect.
func TestQueueRowsCarryTheRealRecurrenceCounts(t *testing.T) {
	got := queueRequest(t, queueReader{page: OpClassCandidatePage{
		Candidates: []opclasscat.Candidate{
			{CandidateKey: "k1", OpClass: "restart-service", Op: "restart", Status: opclasscat.StatusCandidate},
			{CandidateKey: "k2", OpClass: "start-guest", Op: "start", Status: opclasscat.StatusObserving},
		},
		Tallies: map[string]opclasscat.Tally{
			"k1": {Occurrences: 8, Hosts: 3, Span: 9 * 24 * time.Hour, MeanConfidence: 0.91},
			"k2": {Occurrences: 1, Hosts: 1, MeanConfidence: 0.88},
		},
		Total: 17,
	}})

	if len(got.Candidates) != 2 {
		t.Fatalf("rows: want 2, got %d", len(got.Candidates))
	}
	if got.Candidates[0].Occurrences != 8 || got.Candidates[0].Hosts != 3 {
		t.Fatalf("k1 must report its REAL tally 8x/3 hosts, got %dx/%d hosts",
			got.Candidates[0].Occurrences, got.Candidates[0].Hosts)
	}
	if got.Candidates[1].Occurrences != 1 || got.Candidates[1].Hosts != 1 {
		t.Fatalf("k2 must report its REAL tally 1x/1 host, got %dx/%d hosts",
			got.Candidates[1].Occurrences, got.Candidates[1].Hosts)
	}
}

// TestQueueTotalIsTheStoreCountNeverThePageLength pins the badge's honesty. Same law as the proposals
// surface: a count rendered to an operator is a claim about the estate, not about the response.
func TestQueueTotalIsTheStoreCountNeverThePageLength(t *testing.T) {
	got := queueRequest(t, queueReader{page: OpClassCandidatePage{
		Candidates: []opclasscat.Candidate{{CandidateKey: "k1", OpClass: "restart-service"}},
		Tallies:    map[string]opclasscat.Tally{"k1": {Occurrences: 3, Hosts: 2, MeanConfidence: 0.9}},
		Total:      17,
	}})
	if got.Total != 17 {
		t.Fatalf("Total must be the STORE count 17, not the page length %d — got %d", len(got.Candidates), got.Total)
	}
}

// TestQueueStatesTheDistanceToCandidacyAndOmitsItOnceArrived is TG-236 oracle 3.
//
// A shape that has NOT arrived must say what it still needs, computed from the same constants the cron
// promotes on; a shape that HAS arrived must omit the countdown entirely, because a queue behind an open
// door is not a queue.
func TestQueueStatesTheDistanceToCandidacyAndOmitsItOnceArrived(t *testing.T) {
	got := queueRequest(t, queueReader{page: OpClassCandidatePage{
		Candidates: []opclasscat.Candidate{
			{CandidateKey: "far", OpClass: "restart-service"},
			{CandidateKey: "arrived", OpClass: "start-guest"},
		},
		Tallies: map[string]opclasscat.Tally{
			// One distinct incident on one host: short on refs AND on the second leg.
			"far": {Occurrences: 1, Hosts: 1, MeanConfidence: 0.9},
			// Clears every leg: 3 refs, 2 hosts, confident.
			"arrived": {Occurrences: opclasscat.MinDistinctRefs, Hosts: opclasscat.MinDistinctHosts, MeanConfidence: 0.9},
		},
		Total: 2,
	}})

	far := got.Candidates[0]
	if far.ToCandidate == nil {
		t.Fatal("a shape short of candidacy must state its remaining distance")
	}
	if far.ToCandidate.RefsNeeded != opclasscat.MinDistinctRefs-1 {
		t.Fatalf("refs needed: want %d, got %d", opclasscat.MinDistinctRefs-1, far.ToCandidate.RefsNeeded)
	}
	// The OR leg must offer BOTH routes, never imply the operator must satisfy both.
	if far.ToCandidate.HostsNeeded != opclasscat.MinDistinctHosts-1 {
		t.Fatalf("hosts needed: want %d, got %d", opclasscat.MinDistinctHosts-1, far.ToCandidate.HostsNeeded)
	}
	if far.ToCandidate.SpanHoursNeeded != opclasscat.MinSpan.Hours() {
		t.Fatalf("span hours needed: want %v, got %v", opclasscat.MinSpan.Hours(), far.ToCandidate.SpanHoursNeeded)
	}

	if got.Candidates[1].ToCandidate != nil {
		t.Fatalf("an arrived shape must omit the countdown, got %+v", got.Candidates[1].ToCandidate)
	}
}

// TestQueueFailsClosedWhenTheReaderIsAbsentOrErroring keeps the surface's honesty on the unhappy path: an
// unavailable queue must say so, never render as an empty-but-plausible "nothing to review" — which on this
// surface would read as "TG has earned nothing", the opposite of the truth.
func TestQueueFailsClosedWhenTheReaderIsAbsentOrErroring(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/opclass/candidates", nil)
	Deps{}.opClassCandidatesHandler(rec, req, auth.Principal{SourceID: "operator:t"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader must 503, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	Deps{OpClass: queueReader{err: context.DeadlineExceeded}}.
		opClassCandidatesHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/opclass/candidates", nil),
			auth.Principal{SourceID: "operator:t"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reader error must 503, got %d", rec.Code)
	}
}
