package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/opclasscat"
	"github.com/territory-grounder/grounder/core/safety"
)

// recordingOpClassWriter stands in for the Temporal-backed backend so the COMPOSITION is exercised without
// a worker. What is under test is whether an operator's ratification reaches a writer AT ALL through the
// router main() actually builds.
type recordingOpClassWriter struct {
	gotKey       string
	gotSpec      opschema.OpClassSpec
	gotApprover  string
	gotRationale string
	ratifyCalls  int
}

func (w *recordingOpClassWriter) Ratify(_ context.Context, key string, spec opschema.OpClassSpec, _ int, rationale, approver string) (httpapi.OpClassOutcome, error) {
	w.ratifyCalls++
	w.gotKey, w.gotSpec, w.gotRationale, w.gotApprover = key, spec, rationale, approver
	return httpapi.OpClassOutcome{CandidateKey: key, OpClass: spec.OpClass, Status: "ratified", LedgerSeq: 77}, nil
}

func (w *recordingOpClassWriter) Dismiss(context.Context, string, string, string) (httpapi.OpClassOutcome, error) {
	return httpapi.OpClassOutcome{}, nil
}
func (w *recordingOpClassWriter) Demote(context.Context, string, string, string) (httpapi.OpClassOutcome, error) {
	return httpapi.OpClassOutcome{}, nil
}
func (w *recordingOpClassWriter) Revoke(context.Context, string, string, string) (httpapi.OpClassOutcome, error) {
	return httpapi.OpClassOutcome{}, nil
}
func (w *recordingOpClassWriter) ExportEmbed(context.Context, string, string, string) (httpapi.OpClassOutcome, error) {
	return httpapi.OpClassOutcome{}, nil
}

// oneRatifyReadyCandidate is the dossier the lane reads: a candidate at the completeness gate, with model
// text the tripwire must be able to compare an operator's template against.
type oneRatifyReadyCandidate struct{}

func (oneRatifyReadyCandidate) OpClassCandidates(context.Context, int) (httpapi.OpClassCandidatePage, error) {
	return httpapi.OpClassCandidatePage{
		Candidates: []opclasscat.Candidate{{CandidateKey: "k9", OpClass: "reload-proxy", Op: "reload", Status: opclasscat.StatusRatifyReady}},
		Tallies:    map[string]opclasscat.Tally{"k9": {Occurrences: 6, Hosts: 3, MeanConfidence: 0.9}},
		Total:      1,
	}, nil
}

func (oneRatifyReadyCandidate) OpClassDossier(_ context.Context, key string) (opclasscat.Candidate, []opclasscat.Occurrence, error) {
	if key != "k9" {
		return opclasscat.Candidate{}, nil, httpapi.ErrOpClassCandidateNotFound
	}
	return opclasscat.Candidate{
			CandidateKey: "k9", OpClass: "reload-proxy", Op: "reload", Status: opclasscat.StatusRatifyReady,
			Family: opschema.FamilyServiceLifecycle, Tier: opschema.TierLowReversible,
		}, []opclasscat.Occurrence{{
			CandidateKey: "k9", Host: "nlweb01", Op: "reload", OpClass: "reload-proxy",
			// The MODEL's own words. An operator template that byte-matches any of these is a paste.
			Rationale:  "haproxy config drifted; reload it",
			UndoSketch: "systemctl reload haproxy",
			ObservedAt: time.Now().Add(-time.Hour),
		}}, nil
}

func ratifyRequest(t *testing.T, cookie *http.Cookie, key, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/opclass/candidates/"+key+"/ratify", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	return r
}

// TestServedGrounderExecutesAnOperatorsRatification is the ratify lane's ALIVENESS oracle.
//
// It drives buildPublicAPI — the SAME construction main() calls, with the same argument positions — rather
// than a hand-assembled httpapi.Deps, because the Stage-1 defect was precisely a surface that was perfect
// in isolation and never reachable from the binary. Threading two new parameters through a 38-argument
// function is exactly the kind of edit that silently lands a reader in the wrong slot, and only the real
// call proves it did not.
//
// It matters more here than for any other lane: if this seam is dead, an operator clicks Ratify, gets a
// 503, and the fail-closed design makes the dead seam look deliberate. Nothing would ever be granted and
// nothing would ever complain.
func TestServedGrounderExecutesAnOperatorsRatification(t *testing.T) {
	v, sa, cookie := operatorSession(t, "zoe")
	rec := &recordingOpClassWriter{}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, sa, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, oneRatifyReadyCandidate{}, rec, nil, nil, nil, nil, 0, nil, nil)

	w := httptest.NewRecorder()
	// An OPERATOR-authored template. It expresses the same intent as the model's undo sketch and shares no
	// element with it byte-for-byte — which is the whole distinction ADR-0016 decision 3 draws.
	body := `{"rationale":"nlweb01 haproxy drifts weekly; a reload is safe and we do it by hand anyway",
	          "op":"reload","family":"service-lifecycle","safety_tier":"low-reversible",
	          "params":[{"name":"unit","required":true}],
	          "argv_template":["/usr/bin/systemctl","reload","{unit}"]}`
	api.Mux().ServeHTTP(w, ratifyRequest(t, cookie, "k9", body))

	if w.Code != http.StatusOK {
		t.Fatalf("the served ratify lane answered %d, not 200 — a 503 here is the dead-seam defect: an "+
			"operator clicks Ratify forever and no capability is ever granted; body: %s", w.Code, w.Body.String())
	}
	if rec.ratifyCalls != 1 {
		t.Fatalf("the writer ran %d times — the operator's decision never reached the ledger lane", rec.ratifyCalls)
	}
	if rec.gotKey != "k9" {
		t.Errorf("the writer received key %q, not the candidate the operator decided on", rec.gotKey)
	}
	if rec.gotApprover != "zoe" {
		t.Errorf("the approver must be SERVER-derived from the session, got %q — a client-named approver "+
			"would make the authorship of an argv template a client claim", rec.gotApprover)
	}
	if strings.Join(rec.gotSpec.ArgvTemplate, " ") != "/usr/bin/systemctl reload {unit}" {
		t.Errorf("the OPERATOR's template must reach the lane intact, got %q", rec.gotSpec.ArgvTemplate)
	}
	if !strings.Contains(rec.gotRationale, "drifts weekly") {
		t.Errorf("the rationale must reach the ledger writer intact, got %q", rec.gotRationale)
	}
}

// TestServedGrounderRefusesARatificationThatPastesTheModelsOwnWords is the laundering tripwire, driven
// through the SERVED router rather than against opschema directly.
//
// Testing the validator in isolation proves the function works. This proves the system CALLS it — on the
// real route, with the model text the server reads for itself, before any writer runs. Those are different
// claims, and only the second one is a safety property.
func TestServedGrounderRefusesARatificationThatPastesTheModelsOwnWords(t *testing.T) {
	v, sa, cookie := operatorSession(t, "zoe")
	rec := &recordingOpClassWriter{}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, sa, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, oneRatifyReadyCandidate{}, rec, nil, nil, nil, nil, 0, nil, nil)

	w := httptest.NewRecorder()
	// The operator pasted the model's undo sketch verbatim as an argv element.
	body := `{"rationale":"looks right to me","op":"reload","family":"service-lifecycle",
	          "safety_tier":"low-reversible","params":[{"name":"unit","required":true}],
	          "argv_template":["systemctl reload haproxy"]}`
	api.Mux().ServeHTTP(w, ratifyRequest(t, cookie, "k9", body))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a template that byte-matches the model's own text must be refused 422, got %d: %s",
			w.Code, w.Body.String())
	}
	if rec.ratifyCalls != 0 {
		t.Fatal("the writer RAN for a laundered template — the operator's name would now sit on a " +
			"capability the model authored, which is the one thing ADR-0016 decision 3 forbids")
	}
}

// TestServedGrounderRefusesAnUnauthenticatedRatification pins the other half of the gate. An alive-but-open
// ratify lane is worse than a dead one: it lets anyone who can reach the port author an argv template that
// runs as root.
func TestServedGrounderRefusesAnUnauthenticatedRatification(t *testing.T) {
	v, sa, _ := operatorSession(t, "zoe")
	rec := &recordingOpClassWriter{}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, sa, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, oneRatifyReadyCandidate{}, rec, nil, nil, nil, nil, 0, nil, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/opclass/candidates/k9/ratify",
		strings.NewReader(`{"rationale":"no session","op":"reload"}`))
	r.Header.Set("Content-Type", "application/json")
	api.Mux().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated ratify must be 401, got %d", w.Code)
	}
	if rec.ratifyCalls != 0 {
		t.Fatal("the writer ran for an unauthenticated caller — the grant lane is open")
	}
}
