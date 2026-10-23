package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIngestPushBearerAuth proves the AuthIngestPush route admits a valid per-source static bearer token and
// fail-closes on every wrong/absent credential. The bearer is keyed on the {source_type} URL slug.
func TestIngestPushBearerAuth(t *testing.T) {
	v := &Verifier{Sources: memSources{
		"prometheus-alertmanager": {SourceID: "prometheus-alertmanager", IngestToken: []byte("s3cr3t-token")},
	}}
	rt := NewRouter(v)
	var served bool
	var got Principal
	rt.Handle("/v1/ingest/{source_type}", AuthIngestPush, func(w http.ResponseWriter, r *http.Request, p Principal) {
		served, got = true, p
		w.WriteHeader(http.StatusAccepted)
	})
	do := func(authz string) int {
		served = false
		r := httptest.NewRequest(http.MethodPost, "/v1/ingest/prometheus-alertmanager", strings.NewReader("{}"))
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		rt.Mux().ServeHTTP(w, r)
		return w.Code
	}

	if code := do("Bearer s3cr3t-token"); code != http.StatusAccepted {
		t.Fatalf("a valid bearer token must be admitted; got %d", code)
	}
	if !served || got.Method != AuthIngestPush || got.SourceID != "prometheus-alertmanager" {
		t.Fatalf("principal not set: served=%v method=%v source=%q", served, got.Method, got.SourceID)
	}
	for _, tc := range []struct {
		name, authz string
	}{
		{"wrong token", "Bearer wrong-token"},
		{"no credential", ""},
		{"empty bearer", "Bearer "},
		{"not a bearer", "Basic abc"},
	} {
		if code := do(tc.authz); code != http.StatusUnauthorized {
			t.Fatalf("%s must be rejected 401; got %d", tc.name, code)
		}
	}
}

// TestIngestPushFailsClosedWithoutToken: a KNOWN source with no IngestToken provisioned, and an UNKNOWN
// source, both fail closed even with a bearer header — the bearer path is opt-in per source.
func TestIngestPushFailsClosedWithoutToken(t *testing.T) {
	v := &Verifier{Sources: memSources{
		"hmac-only": {SourceID: "hmac-only", HMACSecret: []byte("k")}, // registered, but no ingest token
	}}
	rt := NewRouter(v)
	rt.Handle("/v1/ingest/{source_type}", AuthIngestPush, func(w http.ResponseWriter, _ *http.Request, _ Principal) {
		w.WriteHeader(http.StatusAccepted)
	})
	do := func(path string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer anything")
		w := httptest.NewRecorder()
		rt.Mux().ServeHTTP(w, r)
		return w.Code
	}
	if code := do("/v1/ingest/hmac-only"); code != http.StatusUnauthorized {
		t.Fatalf("a source without an ingest token must fail closed; got %d", code)
	}
	if code := do("/v1/ingest/nonexistent"); code != http.StatusUnauthorized {
		t.Fatalf("an unknown source must fail closed; got %d", code)
	}
}

// TestTokenOnlySourceNotForgeableViaEmptyKeyHMAC: with hmac_secret_ref nullable (0008), a bearer-only
// source has NO HMAC secret. hmac.New with an empty key still computes, so WITHOUT the empty-secret guard
// an attacker who knows the (guessable) source id could sign the X-TG-* headers with the EMPTY key and
// authenticate via the HMAC path. The guard must reject it before any MAC comparison.
func TestTokenOnlySourceNotForgeableViaEmptyKeyHMAC(t *testing.T) {
	v, err := NewVerifier(memSources{
		"prometheus-alertmanager": {SourceID: "prometheus-alertmanager", IngestToken: []byte("s3cr3t")},
	}, newMemNonces(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRouter(v)
	rt.Handle("/v1/ingest/{source_type}", AuthIngestPush, func(w http.ResponseWriter, _ *http.Request, _ Principal) {
		w.WriteHeader(http.StatusAccepted)
	})
	body := `{"forged":"payload"}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/prometheus-alertmanager", strings.NewReader(body))
	r.Header.Set("X-TG-Source", "prometheus-alertmanager")
	r.Header.Set("X-TG-Timestamp", ts)
	r.Header.Set("X-TG-Nonce", "forged-nonce-1")
	r.Header.Set("X-TG-Signature", sign(nil, ts, "forged-nonce-1", body)) // signed with the EMPTY key
	w := httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an empty-key HMAC forgery against a token-only source must be rejected; got %d", w.Code)
	}
}

// TestPresentButInvalidHMACDoesNotFallThroughToBearer pins REQ-519's ordering: the bearer is considered
// ONLY when no machine credential is PRESENT. A request carrying garbage HMAC headers alongside a VALID
// bearer token is an active credential failure and must reject — never silently downgrade to the bearer.
func TestPresentButInvalidHMACDoesNotFallThroughToBearer(t *testing.T) {
	v, err := NewVerifier(memSources{
		"prometheus-alertmanager": {SourceID: "prometheus-alertmanager", HMACSecret: []byte("k"), IngestToken: []byte("s3cr3t")},
	}, newMemNonces(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRouter(v)
	rt.Handle("/v1/ingest/{source_type}", AuthIngestPush, func(w http.ResponseWriter, _ *http.Request, _ Principal) {
		w.WriteHeader(http.StatusAccepted)
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest/prometheus-alertmanager", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer s3cr3t") // valid bearer...
	r.Header.Set("X-TG-Source", "prometheus-alertmanager")
	r.Header.Set("X-TG-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-TG-Nonce", "n1")
	r.Header.Set("X-TG-Signature", "deadbeef") // ...but a PRESENT, INVALID HMAC credential
	w := httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a present-but-invalid machine credential must reject, not downgrade to bearer; got %d", w.Code)
	}
}
