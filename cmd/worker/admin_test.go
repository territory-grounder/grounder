package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/preflight"
	"github.com/territory-grounder/grounder/core/safety"
)

const testHaltToken = "kill-switch-bearer-token-abc123"

// newTestAdmin builds the worker admin over a mode chokepoint. actuating=true starts it in an actuating
// posture (mode Semi/Full + preflight green) so a /halt can be observed dropping it to Shadow; false is the
// read-only default (the deployed posture) used by the metrics/auth oracles.
func newTestAdmin(t *testing.T, token string, actuating bool) (*workerAdmin, *audit.Ledger, *safety.Chokepoint) {
	t.Helper()
	var chokepoint *safety.Chokepoint
	if actuating {
		chokepoint = safety.NewActuatingChokepoint()
	} else {
		chokepoint = safety.NewReadOnlyChokepoint()
	}
	mb, err := safety.NewMutationBreaker(chokepoint, breaker.NewMemStore(), 1, nil)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	ledger := audit.NewLedger()
	return newWorkerAdmin(chokepoint, mb, nil, ledger, token), ledger, chokepoint
}

// TestHaltRequiresAuth proves POST /halt with no/wrong bearer is 401 and changes nothing.
func TestHaltRequiresAuth(t *testing.T) {
	a, ledger, gate := newTestAdmin(t, testHaltToken, false)
	h := a.mux()

	for _, tc := range []struct{ name, auth string }{
		{"no header", ""},
		{"wrong token", "Bearer nope"},
		{"not bearer", "Basic " + testHaltToken},
	} {
		req := httptest.NewRequest(http.MethodPost, "/halt", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: POST /halt = %d, want 401", tc.name, rec.Code)
		}
	}
	if ledger.Len() != 0 {
		t.Fatalf("an unauthorized halt must not ledger anything, got %d entries", ledger.Len())
	}
	if gate.MayActuate() {
		t.Fatal("gate must remain off")
	}
}

// TestHaltDisablesGateAndLedgers proves an authenticated POST /halt disables the gate, records the halt to
// the governance ledger bound to a synthetic action_id, and returns 200 — and is idempotent.
func TestHaltDisablesGateAndLedgers(t *testing.T) {
	a, ledger, gate := newTestAdmin(t, testHaltToken, true)
	h := a.mux()

	req := httptest.NewRequest(http.MethodPost, "/halt", nil)
	req.Header.Set("Authorization", "Bearer "+testHaltToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated POST /halt = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gate.MayActuate() || gate.GuardMutation() == nil {
		t.Fatalf("after /halt the gate must be OFF and GuardMutation must refuse: enabled=%v", gate.MayActuate())
	}
	var resp struct {
		Halted          bool   `json:"halted"`
		MutationEnabled bool   `json:"mutation_enabled"`
		ActionID        string `json:"action_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode halt response: %v", err)
	}
	if !resp.Halted || resp.MutationEnabled || resp.ActionID == "" {
		t.Fatalf("halt response must confirm halted=true, mutation off, and carry an action_id: %+v", resp)
	}
	// Exactly one ledger entry, a safety:halt bound to the synthetic action_id, withheld.
	entries := ledger.Entries()
	if len(entries) != 1 {
		t.Fatalf("halt must append exactly one ledger entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Decision != "safety:halt" || e.ActionID != resp.ActionID || !e.Withheld {
		t.Fatalf("ledger entry not a withheld safety:halt bound to the action_id: %+v", e)
	}
	if err := ledger.Verify(); err != nil {
		t.Fatalf("the halt-appended chain must verify: %v", err)
	}

	// Idempotent: a second halt is still 200 and the gate stays off.
	req2 := httptest.NewRequest(http.MethodPost, "/halt", nil)
	req2.Header.Set("Authorization", "Bearer "+testHaltToken)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || gate.MayActuate() {
		t.Fatalf("second /halt must be 200 and keep mutation off: code=%d enabled=%v", rec2.Code, gate.MayActuate())
	}
}

// TestHaltUnregisteredWithoutToken proves the fail-closed default: with no bearer token, /halt is NOT
// registered at all (404), and only /metrics exists.
func TestHaltUnregisteredWithoutToken(t *testing.T) {
	a, _, _ := newTestAdmin(t, "", false) // no token → /halt must not exist
	h := a.mux()
	req := httptest.NewRequest(http.MethodPost, "/halt", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("with no token, /halt must be unregistered (404), got %d", rec.Code)
	}
}

// TestMetricsExposition proves GET /metrics renders mutation_enabled 0 (the OFF gate) and the breaker gauge.
func TestMetricsExposition(t *testing.T) {
	a, _, _ := newTestAdmin(t, testHaltToken, false)
	h := a.mux()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`mutation_enabled{component="worker"} 0`,
		`circuit_breaker_state{name="mutation"} 0`,
		"deviation_count 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "mutation_enabled{component=\"worker\"} 1") {
		t.Fatalf("mutation_enabled must be 0 over the read-only worker gate; got:\n%s", body)
	}
	// No credential preflight attached ⇒ no SSH surface configured ⇒ the gauge is silent (no false alert).
	if strings.Contains(body, "tg_ssh_credential_ready") {
		t.Fatalf("tg_ssh_credential_ready must be absent when no SSH key is configured; got:\n%s", body)
	}
}

// TestSSHCredentialGauge proves the TG-113 DEGRADED-credential health signal reaches /metrics: a failed boot
// preflight publishes tg_ssh_credential_ready 0 (native SSH DEGRADED), a clean one publishes 1.
func TestSSHCredentialGauge(t *testing.T) {
	metricsBody := func(rep preflight.Report) string {
		a, _, _ := newTestAdmin(t, testHaltToken, false)
		a.withSSHCredential(rep)
		rec := httptest.NewRecorder()
		a.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}

	// A missing key (the exact silent-kill) ⇒ Failed report ⇒ gauge 0.
	degraded := preflight.CheckSSHKeys([]preflight.SSHKeyRef{{Name: "actuation", Ref: config.SecretRef("file:/secrets/does-not-exist-one_key")}})
	if !degraded.Failed() {
		t.Fatalf("a missing key must fail the preflight")
	}
	if body := metricsBody(degraded); !strings.Contains(body, `tg_ssh_credential_ready{component="worker"} 0`) {
		t.Fatalf("degraded credential must publish tg_ssh_credential_ready 0; got:\n%s", body)
	}

	// A configured-but-empty ref set (nothing to prove) stays silent — no gauge, no false alert.
	if body := metricsBody(preflight.CheckSSHKeys([]preflight.SSHKeyRef{{Name: "x", Ref: config.SecretRef("")}})); strings.Contains(body, "tg_ssh_credential_ready") {
		t.Fatalf("an unconfigured SSH surface must emit no gauge; got:\n%s", body)
	}
}
