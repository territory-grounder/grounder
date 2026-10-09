package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/worldmodel"
)

// The spec/027 REQ-2703 oracles: the review-not-author surface. Every test drives the REAL handler.

type fakeManifestReader struct {
	entries       []worldmodel.Entry
	drafts, total int
	err           error
}

func (f fakeManifestReader) ManifestEntries(_ context.Context, _ auth.Principal, _ int) ([]worldmodel.Entry, int, int, error) {
	return f.entries, f.drafts, f.total, f.err
}

type fakeManifestWriter struct {
	calls []string // "id/status/rationale/approver" — proves what the handler actually forwarded
	out   ManifestTransitionOutcome
	err   error
}

func (f *fakeManifestWriter) Transition(_ context.Context, id int64, to worldmodel.Status, rationale, approver string) (ManifestTransitionOutcome, error) {
	f.calls = append(f.calls, strings.Join([]string{
		string(rune('0' + id)), string(to), rationale, approver}, "/"))
	return f.out, f.err
}

// withURLParams injects the chi path params for a direct-handler unit test (the session_detail_test.go
// route-context idiom) so the oracle drives the REAL handler, not a paraphrase of it.
func withURLParams(r *http.Request, kv map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range kv {
		rctx.URLParams.Add(k, v)
	}
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// operatorPrincipal mints a DISTINCT operator per test: the write lane rate-limits per operator, so
// tests sharing one identity would starve each other's budget (which is itself proof the limiter works —
// see TestManifestWriteRateLimitIsPerOperator).
func operatorPrincipal(name ...string) auth.Principal {
	who := "kp"
	if len(name) > 0 {
		who = name[0]
	}
	return auth.Principal{SourceID: "operator:" + who}
}
func machinePrincipal() auth.Principal  { return auth.Principal{SourceID: "module:librenms"} }

func draftEntry() worldmodel.Entry {
	return worldmodel.Entry{
		ID: 1, EntityType: estate.TypeService, Name: "mealie.service", Host: "dc1mealie01",
		Source: estate.SourceDeclared, Confidence: 0.85, Status: worldmodel.StatusDraft,
	}
}

// TestManifestReadFailsClosedWithANilReader — an unwired spine renders honestly unavailable, never an
// empty-but-plausible review queue that would read as "the estate has nothing to adopt".
func TestManifestReadFailsClosedWithANilReader(t *testing.T) {
	rec := httptest.NewRecorder()
	Deps{}.manifestHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil), operatorPrincipal())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader must fail closed with 503, got %d", rec.Code)
	}
}

// TestManifestReadServesHonestCountsAndMaterializationFacts pins the two things the console cannot compute
// for itself: the REAL counts (not page size) and whether adopting a row actually grants anything.
func TestManifestReadServesHonestCountsAndMaterializationFacts(t *testing.T) {
	rd := fakeManifestReader{
		entries: []worldmodel.Entry{
			draftEntry(),
			// A site materializes into NO leaf: adopting it grants nothing, and the surface must say so.
			{ID: 2, EntityType: estate.TypeSite, Name: "dc1", Source: estate.SourceNetbox,
				Confidence: 0.90, Status: worldmodel.StatusDraft},
		},
		drafts: 7, total: 41, // deliberately larger than the returned page
	}
	rec := httptest.NewRecorder()
	Deps{Manifest: rd}.manifestHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil), operatorPrincipal())
	if rec.Code != http.StatusOK {
		t.Fatalf("read: got %d", rec.Code)
	}
	var page ManifestPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Drafts != 7 || page.Total != 41 {
		t.Fatalf("counts must be the STORE's, never len(page): drafts=%d total=%d (page had %d rows)",
			page.Drafts, page.Total, len(page.Entries))
	}
	if !page.Entries[0].Materializes || page.Entries[0].AllowlistKind != string(worldmodel.KindUnit) {
		t.Fatalf("a .service entry must report it materializes into the unit allowlist, got %+v", page.Entries[0])
	}
	if page.Entries[1].Materializes || page.Entries[1].AllowlistKind != "" {
		t.Fatalf("a site materializes into NO leaf — adopting it grants nothing and the surface must not imply it does: %+v", page.Entries[1])
	}
}

// TestManifestCallerCanActIsServerComputed — the console keys its adopt buttons off this flag, so a
// machine principal (which can never reach the AuthSession write lane) must never be told it can act.
func TestManifestCallerCanActIsServerComputed(t *testing.T) {
	rd := fakeManifestReader{entries: []worldmodel.Entry{draftEntry()}, drafts: 1, total: 1}
	for _, tc := range []struct {
		name string
		p    auth.Principal
		want bool
	}{
		{"operator session", operatorPrincipal(), true},
		{"machine principal", machinePrincipal(), false},
	} {
		rec := httptest.NewRecorder()
		Deps{Manifest: rd}.manifestHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil), tc.p)
		var page ManifestPage
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		if page.CallerCanAct != tc.want || page.Entries[0].CallerCanAct != tc.want {
			t.Fatalf("%s: caller_can_act must be %v (page=%v row=%v)",
				tc.name, tc.want, page.CallerCanAct, page.Entries[0].CallerCanAct)
		}
	}
}

func postVerb(t *testing.T, d Deps, id, verb, body string, p auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/manifest/entries/"+id+"/"+verb, strings.NewReader(body))
	r = withURLParams(r, map[string]string{"id": id, "verb": verb})
	rec := httptest.NewRecorder()
	d.manifestTransitionHandler(rec, r, p)
	return rec
}

// TestRejectWithoutRationaleIsRefused is the spec's named scenario: an unexplained decision never reaches
// the state machine, and the entry is untouched (the writer is never called).
func TestRejectWithoutRationaleIsRefused(t *testing.T) {
	wr := &fakeManifestWriter{}
	rec := postVerb(t, Deps{ManifestWrite: wr}, "1", "reject", `{"rationale":"   "}`, operatorPrincipal("rej"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty rationale must be refused with 400, got %d", rec.Code)
	}
	if len(wr.calls) != 0 {
		t.Fatalf("a refused request must never reach the state machine, got calls %v", wr.calls)
	}
}

// TestManifestVerbTableIsClosed — route text is never a status. A create verb in particular must not
// exist: an operator who could POST a draft could hand-author an actuation target (paradigm rule 9).
func TestManifestVerbTableIsClosed(t *testing.T) {
	for _, verb := range []string{"create", "draft", "approve", "delete", "stale"} {
		wr := &fakeManifestWriter{}
		rec := postVerb(t, Deps{ManifestWrite: wr}, "1", verb, `{"rationale":"x"}`, operatorPrincipal("verb"+verb))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("verb %q must not exist (404), got %d", verb, rec.Code)
		}
		if len(wr.calls) != 0 {
			t.Fatalf("verb %q reached the writer — the table is not closed", verb)
		}
	}
	if _, ok := manifestVerbs["create"]; ok {
		t.Fatal("a create verb exists: discovery must be the only author of manifest rows (paradigm rule 9)")
	}
}

// TestApproverIsServerDerivedNeverClientSupplied — the audit trail's subject comes from the authenticated
// principal. A body field claiming to be someone else must not reach the ledger.
func TestApproverIsServerDerivedNeverClientSupplied(t *testing.T) {
	wr := &fakeManifestWriter{out: ManifestTransitionOutcome{ID: 1, Status: "approved"}}
	rec := postVerb(t, Deps{ManifestWrite: wr}, "1",
		"adopt", `{"rationale":"reviewed the unit","approver":"root"}`, operatorPrincipal())
	if rec.Code != http.StatusOK {
		t.Fatalf("adopt: got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(wr.calls) != 1 || !strings.HasSuffix(wr.calls[0], "/kp") {
		t.Fatalf("approver must be the SERVER-derived session identity, got %v", wr.calls)
	}
	if strings.Contains(wr.calls[0], "root") {
		t.Fatal("a client-supplied approver reached the write lane — the audit trail became a client claim")
	}
}

// TestCrossOriginWriteIsRejected — the second CSRF layer over SameSite=Strict (the vote.go kit).
func TestCrossOriginWriteIsRejected(t *testing.T) {
	wr := &fakeManifestWriter{}
	r := httptest.NewRequest(http.MethodPost, "/v1/manifest/entries/1/adopt", strings.NewReader(`{"rationale":"x"}`))
	r.Host = "console.internal"
	r.Header.Set("Origin", "https://evil.example")
	r = withURLParams(r, map[string]string{"id": "1", "verb": "adopt"})
	rec := httptest.NewRecorder()
	Deps{ManifestWrite: wr}.manifestTransitionHandler(rec, r, operatorPrincipal())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin write must be refused with 403, got %d", rec.Code)
	}
	if len(wr.calls) != 0 {
		t.Fatalf("a cross-origin write reached the state machine: %v", wr.calls)
	}
}

// TestManifestWriteFailsClosedWithNoWriter — an unwired write path is 503, never a silent success.
func TestManifestWriteFailsClosedWithNoWriter(t *testing.T) {
	rec := postVerb(t, Deps{}, "1", "adopt", `{"rationale":"x"}`, operatorPrincipal("nowriter"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil writer must fail closed with 503, got %d", rec.Code)
	}
}

// TestManifestWriteErrorsAreHonest — a genuinely-illegal transition is a 409 (do not retry); a sick write
// path is a 503 (retry). Collapsing them teaches an operator the wrong lesson about a retryable failure.
func TestManifestWriteErrorsAreHonest(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"illegal transition", worldmodel.ErrBadTransition, http.StatusConflict},
		{"missing rationale at the authority", worldmodel.ErrRationaleRequired, http.StatusBadRequest},
		{"unknown entry", ErrManifestEntryNotFound, http.StatusNotFound},
		{"unknown entity type", worldmodel.ErrUnknownEntityType, http.StatusUnprocessableEntity},
	} {
		wr := &fakeManifestWriter{err: tc.err}
		rec := postVerb(t, Deps{ManifestWrite: wr}, "1", "adopt", `{"rationale":"x"}`, operatorPrincipal("err"+tc.name))
		if rec.Code != tc.want {
			t.Fatalf("%s: want %d, got %d", tc.name, tc.want, rec.Code)
		}
	}
}

// TestManifestWriteRateLimitIsPerOperator — a runaway client must not mass-adopt the estate faster than a
// human could have reviewed it, and one operator's burst must never spend another's budget.
func TestManifestWriteRateLimitIsPerOperator(t *testing.T) {
	wr := &fakeManifestWriter{out: ManifestTransitionOutcome{ID: 1, Status: "approved"}}
	d := Deps{ManifestWrite: wr}
	var limited bool
	for i := 0; i < 40; i++ {
		if postVerb(t, d, "1", "adopt", `{"rationale":"burst"}`, operatorPrincipal("burst")).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("a 40-write burst from one operator must hit the rate limit")
	}
	// A DIFFERENT operator still has their full budget — the window is per-caller, not global.
	if rec := postVerb(t, d, "1", "adopt", `{"rationale":"fresh"}`, operatorPrincipal("fresh")); rec.Code != http.StatusOK {
		t.Fatalf("a different operator must keep their own budget, got %d", rec.Code)
	}
}
