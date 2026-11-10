package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
)

func demoteReq(token, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/graduation/demote", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// TG-666 part 3: POST /graduation/demote drops a registered class to approve durably and ledgers it with the reason.
// KILLING MUTATION: drop the m.HandleFunc("/graduation/demote", …) registration — RED here (404).
func TestTheOperatorDemoteRouteDropsTheRungAndLedgersIt(t *testing.T) {
	a, ledger, _ := newTestAdmin(t, testHaltToken, false)
	store := policy.NewMemGraduationStore().Seed(policy.ClassState{OpClass: "start-service", Level: policy.LevelAuto})
	l := policy.NewLadder(5, store, nil)
	wireGraduationTransitions(l, ledger)
	h := a.withLadder(l).mux()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, demoteReq(testHaltToken, `{"op_class":"start-service","reason":"TG-666 audit"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated demote = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["demoted"] != true || resp["persisted"] != true || resp["to"] != policy.LevelApprove.String() {
		t.Fatalf("the response must confirm a persisted demotion to approve, got %v", resp)
	}
	if lv := policy.NewLadder(5, store, nil).LevelOf(context.Background(), "start-service"); lv != policy.LevelApprove {
		t.Fatalf("the durable row must read approve, got %v", lv)
	}
	entries := ledger.Entries()
	if len(entries) != 1 || entries[0].Decision != "graduation:demote:start-service" || !entries[0].Withheld ||
		!strings.Contains(entries[0].Reason, "by an operator") || !strings.Contains(entries[0].Reason, "TG-666 audit") {
		t.Fatalf("one withheld graduation:demote row naming the operator and the reason, got %+v", entries)
	}
}

// The route refuses everything that is not an authenticated, well-formed, registered, reasoned demotion, and
// changes nothing when it refuses. KILLING MUTATION: drop the opschema.Lookup check — RED (unknown class answers 200).
func TestTheOperatorDemoteRouteRefusesAndChangesNothing(t *testing.T) {
	a, ledger, _ := newTestAdmin(t, testHaltToken, false)
	store := policy.NewMemGraduationStore().Seed(policy.ClassState{OpClass: "start-service", Level: policy.LevelAuto})
	l := policy.NewLadder(5, store, nil)
	wireGraduationTransitions(l, ledger)
	h := a.withLadder(l).mux()

	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"no bearer", demoteReq("", `{"op_class":"start-service","reason":"x"}`), http.StatusUnauthorized},
		{"wrong bearer", demoteReq("nope", `{"op_class":"start-service","reason":"x"}`), http.StatusUnauthorized},
		{"GET", httptest.NewRequest(http.MethodGet, "/graduation/demote", nil), http.StatusMethodNotAllowed},
		{"not JSON", demoteReq(testHaltToken, `op_class=start-service`), http.StatusBadRequest},
		{"unknown class", demoteReq(testHaltToken, `{"op_class":"rm-rf-root","reason":"x"}`), http.StatusBadRequest},
		{"no reason", demoteReq(testHaltToken, `{"op_class":"start-service","reason":"  "}`), http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, c.req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	if lv := l.LevelOf(context.Background(), "start-service"); lv != policy.LevelAuto || ledger.Len() != 0 {
		t.Fatalf("a refused demote must change nothing: level %v, %d ledger rows", lv, ledger.Len())
	}

	// No ladder in this process ⇒ 503, never a silent 200.
	b, _, _ := newTestAdmin(t, testHaltToken, false)
	rec := httptest.NewRecorder()
	b.mux().ServeHTTP(rec, demoteReq(testHaltToken, `{"op_class":"start-service","reason":"x"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no ladder ⇒ 503, got %d", rec.Code)
	}
}

// ctxStore fails a Save whose context is already done — the way a real database driver does.
type ctxStore struct{ *policy.MemGraduationStore }

func (s ctxStore) Save(ctx context.Context, st policy.ClassState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemGraduationStore.Save(ctx, st)
}

// TG-666 review S2: the demotion write must outlive the HTTP request — a client that hangs up mid-Save must not
// cancel it. The class name is the canonical slug (any spelling the caller sends) and control characters in the
// reason never reach the ledger. KILLING MUTATION: pass r.Context() to Demote — RED here (503, row still auto).
func TestTheOperatorDemoteWriteOutlivesTheRequestAndKeysOnTheCanonicalClass(t *testing.T) {
	a, ledger, _ := newTestAdmin(t, testHaltToken, false)
	mem := policy.NewMemGraduationStore().Seed(policy.ClassState{OpClass: "start-service", Level: policy.LevelAuto})
	l := policy.NewLadder(5, ctxStore{mem}, nil)
	wireGraduationTransitions(l, ledger)
	h := a.withLadder(l).mux()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	req := demoteReq(testHaltToken, `{"op_class":"  Start-Service ","reason":"TG-666\naudit"}`).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the write must not die with the request: got %d (%s)", rec.Code, rec.Body.String())
	}
	if st, _ := mem.Load(context.Background(), "start-service"); st.Level != policy.LevelApprove {
		t.Fatalf("the durable row must read approve, got %+v", st)
	}
	e := ledger.Entries()
	if len(e) != 1 || e[0].Decision != "graduation:demote:start-service" || strings.ContainsAny(e[0].Reason, "\n\r") {
		t.Fatalf("one row keyed on the canonical slug, reason without control characters, got %+v", e)
	}
}

// TG-666 review S3: only an OPERATOR's demotion is a withheld row. An automatic run-outcome demotion keeps
// Withheld=false, as it was — the wiki roster counts withheld rows as "TG declined to act".
// KILLING MUTATION: Withheld = r.Demoted || r.StreakReset — RED here.
func TestOnlyAnOperatorDemotionIsAWithheldLedgerRow(t *testing.T) {
	var rows []audit.GovDecision
	rec := graduationTransitionRecorder(func(d audit.GovDecision) error { rows = append(rows, d); return nil })
	rec(policy.RecordResult{OpClass: "start-service", From: policy.LevelAuto, To: policy.LevelApprove, Demoted: true, Outcome: policy.OutcomeDeviated})
	rec(policy.RecordResult{OpClass: "start-service", From: policy.LevelAuto, To: policy.LevelApprove, Demoted: true, Operator: true, Reason: "audit"})
	if len(rows) != 2 || rows[0].Withheld || !rows[1].Withheld {
		t.Fatalf("automatic demotion withheld=false, operator demotion withheld=true; got %+v", rows)
	}
}
