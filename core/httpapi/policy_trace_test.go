package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

// TestPolicyTraceHandlerServesTheWorkerVerdict proves the happy path: a well-formed candidate is decoded,
// forwarded to the tracer verbatim, and the tracer's Result is encoded onto the wire — the packet-tracer
// answer reaches the operator unaltered.
func TestPolicyTraceHandlerServesTheWorkerVerdict(t *testing.T) {
	tr := &MemPolicyTracer{Result: PolicyTraceResult{
		Verdict: "deny", MatchedRuleID: "deny-cisco-write", ComposedBand: "POLL_PAUSE", Mode: "Shadow",
		Reason: "deny short-circuits: rule denied | NOTE: rate-governor not simulated", NeverAutoFloor: true,
		BundleVersion: "sha256:abc123", RateGovernorSimulated: false,
		MatchedRules: []PolicyTraceMatchedRule{{RuleID: "deny-cisco-write", Verdict: "deny"}},
	}}
	d := Deps{Tracer: tr}

	body := `{"op_class":"network.acl.write","host":"dc1fw01","band":"AUTO","confidence":0.9}`
	r := httptest.NewRequest(http.MethodPost, "/v1/policy/trace", strings.NewReader(body))
	w := httptest.NewRecorder()
	d.policyTraceHandler(w, r, auth.Principal{})

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// The candidate reached the tracer faithfully.
	if tr.Calls != 1 {
		t.Fatalf("tracer called %d times, want 1", tr.Calls)
	}
	if tr.LastReq.OpClass != "network.acl.write" || tr.LastReq.Host != "dc1fw01" || tr.LastReq.Band != "AUTO" || tr.LastReq.Confidence != 0.9 {
		t.Errorf("decoded request not forwarded faithfully: %+v", tr.LastReq)
	}
	// The tracer's answer reached the wire.
	var out PolicyTraceResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Verdict != "deny" || out.MatchedRuleID != "deny-cisco-write" || out.ComposedBand != "POLL_PAUSE" {
		t.Errorf("response did not carry the tracer verdict: %+v", out)
	}
	if len(out.MatchedRules) != 1 || out.MatchedRules[0].RuleID != "deny-cisco-write" {
		t.Errorf("matched-rule provenance dropped: %+v", out.MatchedRules)
	}
	// The honest rate-governor disclosure must be on the wire, by its JSON name.
	if !strings.Contains(w.Body.String(), `"rate_governor_simulated":false`) {
		t.Errorf("response must carry rate_governor_simulated=false honestly: %s", w.Body.String())
	}
}

// TestPolicyTraceHandlerNilTracerFailsClosed proves an unwired backend (no Temporal/worker) 503s rather than
// panicking or fabricating a verdict — the fail-closed 503 the router documents.
func TestPolicyTraceHandlerNilTracerFailsClosed(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/policy/trace", strings.NewReader(`{"op_class":"x"}`))
	w := httptest.NewRecorder()
	Deps{}.policyTraceHandler(w, r, auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil tracer must 503, got %d", w.Code)
	}
}

// TestPolicyTraceHandlerRejectsNonPost proves the read-surface path is POST-only (a trace carries a request
// body); a GET is 405.
func TestPolicyTraceHandlerRejectsNonPost(t *testing.T) {
	tr := &MemPolicyTracer{}
	r := httptest.NewRequest(http.MethodGet, "/v1/policy/trace", nil)
	w := httptest.NewRecorder()
	Deps{Tracer: tr}.policyTraceHandler(w, r, auth.Principal{})
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET must be 405, got %d", w.Code)
	}
	if tr.Calls != 0 {
		t.Error("a rejected method must not reach the tracer")
	}
}

// TestPolicyTraceHandlerValidation proves malformed JSON and an empty candidate are refused BEFORE the
// backend is called — never a fabricated evaluation over nothing.
func TestPolicyTraceHandlerValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"malformed", `{not json`, http.StatusBadRequest},
		{"empty-candidate", `{}`, http.StatusBadRequest},
		{"only-confidence", `{"confidence":0.9}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &MemPolicyTracer{}
			r := httptest.NewRequest(http.MethodPost, "/v1/policy/trace", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			Deps{Tracer: tr}.policyTraceHandler(w, r, auth.Principal{})
			if w.Code != tc.want {
				t.Fatalf("%s: got %d, want %d", tc.name, w.Code, tc.want)
			}
			if tr.Calls != 0 {
				t.Errorf("%s: a refused request must not reach the tracer", tc.name)
			}
		})
	}
	// A host-only candidate (no op-class) is a legitimate question and MUST be accepted.
	tr := &MemPolicyTracer{}
	r := httptest.NewRequest(http.MethodPost, "/v1/policy/trace", strings.NewReader(`{"host":"dc1fw01"}`))
	w := httptest.NewRecorder()
	Deps{Tracer: tr}.policyTraceHandler(w, r, auth.Principal{})
	if w.Code != http.StatusOK || tr.Calls != 1 {
		t.Fatalf("a host-only candidate must be accepted, got %d calls=%d", w.Code, tr.Calls)
	}
}

// TestPolicyTraceHandlerBackendErrorIs503 proves a trace that failed to reach the worker engine is retryable
// (503), never disguised as a real verdict.
func TestPolicyTraceHandlerBackendErrorIs503(t *testing.T) {
	tr := &MemPolicyTracer{Err: errors.New("temporal unavailable")}
	r := httptest.NewRequest(http.MethodPost, "/v1/policy/trace", strings.NewReader(`{"op_class":"x"}`))
	w := httptest.NewRecorder()
	Deps{Tracer: tr}.policyTraceHandler(w, r, auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("backend error must 503, got %d (%s)", w.Code, w.Body.String())
	}
}
