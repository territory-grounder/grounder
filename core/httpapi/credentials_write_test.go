package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TG-109 "Sync now": POST /v1/credentials/sources/{source_id}/sync — admin-session, the shared write guard,
// the worker lane behind a seam. The outcome type has no field that could carry a secret (INV-13 by
// construction — the same discipline as every credential DTO).

// Happy path: the operator identity comes from the authenticated principal, never the body; the outcome is
// the lane's non-secret sync facts.
func TestCredentialSyncUsesPrincipalIdentity(t *testing.T) {
	m := &MemCredentialSyncer{Outcome: CredentialSyncOutcome{
		SourceID: "openbao", OK: true, Summary: "synced OK — 7 entries (+2 ~0 -0)", Added: 2, Entries: 7, ElapsedMS: 120,
	}}
	rt, c := adminSurfaceRig(t, Deps{CredentialSync: m}, true)

	rec := adminPostJSON(rt, c, "/v1/credentials/sources/openbao/sync", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out CredentialSyncOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if !out.OK || out.SourceID != "openbao" || out.Entries != 7 {
		t.Fatalf("outcome = %+v", out)
	}
	if m.LastSourceID != "openbao" {
		t.Fatalf("source forwarded = %q", m.LastSourceID)
	}
	if m.LastOperator != "kyriakos" {
		t.Fatalf("operator must be the authenticated session principal, got %q", m.LastOperator)
	}
}

// A starved outcome (synced OK, zero bindings) passes through verbatim — the anti-quiet-zero fact must
// reach the operator, not be rounded to a success.
func TestCredentialSyncSurfacesStarvation(t *testing.T) {
	m := &MemCredentialSyncer{Outcome: CredentialSyncOutcome{SourceID: "openbao", OK: true, Starved: true,
		Summary: "synced OK and produced ZERO host bindings"}}
	rt, c := adminSurfaceRig(t, Deps{CredentialSync: m}, true)
	rec := adminPostJSON(rt, c, "/v1/credentials/sources/openbao/sync", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var out CredentialSyncOutcome
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Starved {
		t.Fatal("starvation flag dropped — the quiet-zero defect this field exists to surface")
	}
}

// The lane failing to RUN (worker unreachable) is 502 — distinct from a sync that ran and failed, which is
// a 200 with ok=false. "Could not run" must never read as a verdict on the source.
func TestCredentialSyncLaneFailureIs502(t *testing.T) {
	m := &MemCredentialSyncer{Err: errors.New("temporal unreachable")}
	rt, c := adminSurfaceRig(t, Deps{CredentialSync: m}, true)
	rec := adminPostJSON(rt, c, "/v1/credentials/sources/openbao/sync", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("lane failure: got %d, want 502", rec.Code)
	}
}

// Fail-closed wiring (the empty-input mutation): nil backend ⇒ 503, never a silent 200.
func TestCredentialSyncNilBackendIs503(t *testing.T) {
	rt, c := adminSurfaceRig(t, Deps{}, true)
	rec := adminPostJSON(rt, c, "/v1/credentials/sources/openbao/sync", `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
}

// The source id is bounded + charset-checked: it keys an engine lookup and a log line.
func TestCredentialSyncRejectsBadSourceID(t *testing.T) {
	m := &MemCredentialSyncer{}
	rt, c := adminSurfaceRig(t, Deps{CredentialSync: m}, true)
	rec := adminPostJSON(rt, c, "/v1/credentials/sources/Bad%2FId!/sync", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: got %d, want 400", rec.Code)
	}
	if m.LastSourceID != "" {
		t.Fatalf("a rejected id must never reach the lane, got %q", m.LastSourceID)
	}
}
