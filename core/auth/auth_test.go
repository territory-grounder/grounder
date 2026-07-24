package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type memSources map[string]Source

func (m memSources) LookupSource(_ context.Context, id string) (Source, error) {
	s, ok := m[id]
	if !ok {
		return Source{}, ErrUnauthenticated
	}
	return s, nil
}

type memNonces struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func newMemNonces() *memNonces { return &memNonces{seen: map[string]struct{}{}} }
func (n *memNonces) SeenBefore(_ context.Context, src, nonce string, _ time.Time) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := src + "|" + nonce
	if _, ok := n.seen[k]; ok {
		return true, nil
	}
	n.seen[k] = struct{}{}
	return false, nil
}

func sign(secret []byte, ts, nonce, body string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "\n" + nonce + "\n" + body))
	return hex.EncodeToString(mac.Sum(nil))
}

func newVerifier() *Verifier {
	return &Verifier{
		Sources: memSources{"prom-nl": {SourceID: "prom-nl", HMACSecret: []byte("shhh-secret-key")}},
		Nonces:  newMemNonces(),
		Window:  5 * time.Minute,
	}
}

// A route declared with auth=none must panic at registration — never become an open endpoint.
func TestAuthNoneRoutepanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a route with AuthNone must panic (INV-01)")
		}
	}()
	NewRouter(newVerifier()).Handle("/x", AuthNone, func(http.ResponseWriter, *http.Request, Principal) {})
}

func newSignedReq(secret []byte, source, body string, tOffset time.Duration, nonce string) *http.Request {
	ts := strconv.FormatInt(time.Now().Add(tOffset).Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(body))
	r.Header.Set("X-TG-Source", source)
	r.Header.Set("X-TG-Timestamp", ts)
	r.Header.Set("X-TG-Nonce", nonce)
	r.Header.Set("X-TG-Signature", sign(secret, ts, nonce, body))
	return r
}

func TestAuthFlow(t *testing.T) {
	v := newVerifier()
	secret := []byte("shhh-secret-key")
	rt := NewRouter(v)
	var served Principal
	rt.Handle("/ingest", AuthHMAC, func(w http.ResponseWriter, r *http.Request, p Principal) {
		served = p
		w.WriteHeader(http.StatusOK)
	})

	// 1) unauthenticated → 401, handler not run
	served = Principal{}
	w := httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader("{}")))
	if w.Code != http.StatusUnauthorized || served.SourceID != "" {
		t.Fatalf("unauth request must 401 and not run handler; code=%d served=%+v", w.Code, served)
	}

	// 2) valid HMAC → 200, principal populated
	w = httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, newSignedReq(secret, "prom-nl", `{"host":"web01"}`, 0, "n1"))
	if w.Code != http.StatusOK || served.SourceID != "prom-nl" {
		t.Fatalf("valid request should 200 with principal; code=%d served=%+v", w.Code, served)
	}

	// 3) replayed nonce → 401
	w = httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, newSignedReq(secret, "prom-nl", `{"host":"web01"}`, 0, "n1"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed nonce must 401, got %d", w.Code)
	}

	// 4) stale timestamp → 401
	w = httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, newSignedReq(secret, "prom-nl", "{}", -10*time.Minute, "n2"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stale timestamp must 401, got %d", w.Code)
	}

	// 5) tampered body (signature over different bytes) → 401
	r := newSignedReq(secret, "prom-nl", `{"host":"web01"}`, 0, "n3")
	r.Body = httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(`{"host":"ATTACKER"}`)).Body
	w = httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body must 401, got %d", w.Code)
	}
}

// Replay protection is mandatory: a verifier with no window AND no nonce store must fail closed,
// both at construction and at request time. (security-review HIGH fix)
func TestReplayProtectionMustBeConfigured(t *testing.T) {
	if _, err := NewVerifier(memSources{}, nil, 5*time.Minute); err != ErrReplayProtectionUnconfigured {
		t.Fatalf("NewVerifier with nil nonce store must fail, got %v", err)
	}
	if _, err := NewVerifier(memSources{}, newMemNonces(), 0); err != ErrReplayProtectionUnconfigured {
		t.Fatalf("NewVerifier with no window must fail, got %v", err)
	}
	// A struct-literal Verifier with both replay defenses off must still fail closed at request time.
	v := &Verifier{Sources: memSources{"s": {SourceID: "s", HMACSecret: []byte("k")}}} // Window 0, Nonces nil
	rt := NewRouter(v)
	served := false
	rt.Handle("/ingest", AuthHMAC, func(http.ResponseWriter, *http.Request, Principal) { served = true })
	w := httptest.NewRecorder()
	rt.Mux().ServeHTTP(w, newSignedReq([]byte("k"), "s", "{}", 0, "n"))
	if w.Code != http.StatusUnauthorized || served {
		t.Fatalf("misconfigured verifier must fail closed; code=%d served=%v", w.Code, served)
	}
}
