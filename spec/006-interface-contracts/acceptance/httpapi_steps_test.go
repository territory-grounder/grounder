package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// --- in-memory fakes for the REQ-501/504 HTTP oracles (CI has no live DB/Temporal) ---

const (
	testSource = "prometheus-dc1"
	testSecret = "unit-test-hmac-secret-value-1234567890"
)

// fakeSources resolves exactly one known source; anything else is unauthenticated.
type fakeSources struct{}

func (fakeSources) LookupSource(_ context.Context, id string) (auth.Source, error) {
	if id != testSource {
		return auth.Source{}, fmt.Errorf("unknown source %q", id)
	}
	return auth.Source{SourceID: id, HMACSecret: []byte(testSecret)}, nil
}

// fakeNonces is an in-memory replay guard; a repeated (source,nonce) reports true.
type fakeNonces struct{ seen map[string]bool }

func (n *fakeNonces) SeenBefore(_ context.Context, source, nonce string, _ time.Time) (bool, error) {
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	k := source + "|" + nonce
	if n.seen[k] {
		return true, nil
	}
	n.seen[k] = true
	return false, nil
}

// fakeStats records whether the handler ran (proving auth ran first).
type fakeStats struct{ called bool }

func (s *fakeStats) Stats(_ context.Context, _ auth.Principal) (httpapi.Stats, error) {
	s.called = true
	return httpapi.Stats{MutationEnabled: false, OpenSessions: 0}, nil
}

// fakeSnaps owns a set of external_refs for the authenticated source; a ref it does not own — whether
// unknown or belonging to another role — returns found=false (REQ-504 indistinguishability).
type fakeSnaps struct{ owned map[string]bool }

func (s *fakeSnaps) Get(_ context.Context, ref string, _ auth.Principal) (httpapi.ContextSnapshot, bool, error) {
	if !s.owned[ref] {
		return httpapi.ContextSnapshot{}, false, nil
	}
	return httpapi.ContextSnapshot{ExternalRef: ref, CapturedAt: ingestNow}, true, nil
}

// fakeStarter records that a NEW workflow was minted from a snapshot.
type fakeStarter struct {
	called   bool
	fromSnap httpapi.ContextSnapshot
}

func (s *fakeStarter) StartFromSnapshot(_ context.Context, snap httpapi.ContextSnapshot) (string, error) {
	s.called = true
	s.fromSnap = snap
	return "tg/replay-" + snap.ExternalRef, nil
}

// httpHarness holds the live httptest server and the prepared request for a scenario.
type httpHarness struct {
	server     *httptest.Server
	nonces     *fakeNonces
	stats      *fakeStats
	snaps      *fakeSnaps
	starter    *fakeStarter
	prepared   *http.Request
	lastStatus int
	lastBody   string
	panicked   bool
}

// start builds the real auth router + httpapi surface over in-memory fakes and serves it.
func (h *httpHarness) start() error {
	h.nonces = &fakeNonces{}
	h.stats = &fakeStats{}
	h.snaps = &fakeSnaps{owned: map[string]bool{"TG-1": true}}
	h.starter = &fakeStarter{}
	v, err := auth.NewVerifier(fakeSources{}, h.nonces, 5*time.Minute)
	if err != nil {
		return err
	}
	rt := auth.NewRouter(v)
	httpapi.Register(rt, httpapi.Deps{Stats: h.stats, Snapshots: h.snaps, Starter: h.starter})
	h.server = httptest.NewServer(rt.Mux())
	return nil
}

// sign builds a request signed exactly as core/auth verifies: HMAC-SHA256(secret, ts \n nonce \n body).
func (h *httpHarness) sign(method, path, nonce string, ts time.Time, signedBody, sentBody []byte) *http.Request {
	req, _ := http.NewRequest(method, h.server.URL+path, bytes.NewReader(sentBody))
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(tsStr))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	mac.Write([]byte("\n"))
	mac.Write(signedBody)
	req.Header.Set("X-TG-Source", testSource)
	req.Header.Set("X-TG-Timestamp", tsStr)
	req.Header.Set("X-TG-Nonce", nonce)
	req.Header.Set("X-TG-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}

func (h *httpHarness) send(req *http.Request) error {
	resp, err := h.server.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	h.lastStatus = resp.StatusCode
	h.lastBody = string(b)
	return nil
}

func (h *httpHarness) expectStatus(want int) error {
	if h.lastStatus != want {
		return fmt.Errorf("expected HTTP %d, got %d (body=%q)", want, h.lastStatus, h.lastBody)
	}
	return nil
}

// registerHTTPAPISteps binds the REQ-501/504 scenarios to the real auth + httpapi code path.
func registerHTTPAPISteps(sc *godog.ScenarioContext) {
	h := &httpHarness{}
	now := time.Now()

	// unauthenticated stats
	sc.Step(`^a stats route registered on the authenticated router$`, h.start)
	sc.Step(`^a request arrives with no valid credential$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/stats", nil)
		return h.send(req)
	})
	sc.Step(`^the response is 401 and the request body is never parsed$`, func() error {
		if err := h.expectStatus(http.StatusUnauthorized); err != nil {
			return err
		}
		if h.stats.called {
			return fmt.Errorf("stats handler ran despite failed auth — body would have been parsed")
		}
		return nil
	})

	// auth=none registration panics
	sc.Step(`^a route declared with auth method none$`, func() error { return h.start() })
	sc.Step(`^the router registers the route$`, func() error {
		v, _ := auth.NewVerifier(fakeSources{}, &fakeNonces{}, time.Minute)
		rt := auth.NewRouter(v)
		h.panicked = didPanic(func() {
			rt.Handle("/v1/open", auth.AuthNone, func(http.ResponseWriter, *http.Request, auth.Principal) {})
		})
		return nil
	})
	sc.Step(`^registration panics at boot and no open endpoint is created$`, func() error {
		if !h.panicked {
			return fmt.Errorf("auth=none route must panic at registration (INV-01)")
		}
		return nil
	})

	// replayed nonce
	sc.Step(`^a valid HMAC request whose nonce was already seen for its source$`, func() error {
		if err := h.start(); err != nil {
			return err
		}
		first := h.sign(http.MethodGet, "/v1/stats", "nonce-A", now, nil, nil)
		if err := h.send(first); err != nil { // consumes the nonce
			return err
		}
		if h.lastStatus != http.StatusOK {
			return fmt.Errorf("first request should authenticate (200), got %d", h.lastStatus)
		}
		h.prepared = h.sign(http.MethodGet, "/v1/stats", "nonce-A", now, nil, nil) // same nonce again
		return nil
	})
	// stale timestamp
	sc.Step(`^an HMAC request whose timestamp is outside the replay window$`, func() error {
		if err := h.start(); err != nil {
			return err
		}
		h.prepared = h.sign(http.MethodGet, "/v1/stats", "nonce-stale", now.Add(-10*time.Minute), nil, nil)
		return nil
	})
	// tampered body
	sc.Step(`^an HMAC request whose body was altered after signing$`, func() error {
		if err := h.start(); err != nil {
			return err
		}
		signed := []byte(`{"ref":"TG-1"}`)
		sent := []byte(`{"ref":"TG-EVIL"}`)
		h.prepared = h.sign(http.MethodPost, "/v1/sessions/TG-1/replay", "nonce-tamper", now, signed, sent)
		return nil
	})
	sc.Step(`^the router authenticates the request$`, func() error { return h.send(h.prepared) })
	sc.Step(`^the request is rejected as a nonce replay$`, func() error { return h.expectStatus(http.StatusUnauthorized) })
	sc.Step(`^the request is rejected as a stale timestamp$`, func() error { return h.expectStatus(http.StatusUnauthorized) })
	sc.Step(`^the signature does not verify and the request is rejected$`, func() error { return h.expectStatus(http.StatusUnauthorized) })

	// session-replay mints a new workflow
	sc.Step(`^an authenticated session-replay request for a known session$`, func() error {
		if err := h.start(); err != nil {
			return err
		}
		h.prepared = h.sign(http.MethodPost, "/v1/sessions/TG-1/replay", "nonce-replay", now, nil, nil)
		return nil
	})
	sc.Step(`^the replay handler processes the request$`, func() error { return h.send(h.prepared) })
	sc.Step(`^a new Temporal workflow is started from an immutable read-only ContextSnapshot$`, func() error {
		if err := h.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if !h.starter.called {
			return fmt.Errorf("replay must mint a new workflow via the starter")
		}
		if h.starter.fromSnap.ExternalRef != "TG-1" {
			return fmt.Errorf("workflow not seeded from the snapshot: %+v", h.starter.fromSnap)
		}
		return nil
	})
	sc.Step(`^no mutating session is resumed with caller-supplied input$`, func() error {
		// The only path is StartFromSnapshot(read-only snapshot); there is no resume primitive to invoke.
		if h.lastBody == "" || !bytes.Contains([]byte(h.lastBody), []byte("workflow_id")) {
			return fmt.Errorf("expected a new workflow_id in the response, got %q", h.lastBody)
		}
		return nil
	})

	// unknown id → 404
	sc.Step(`^an authenticated replay request for an id that does not exist$`, func() error {
		if err := h.start(); err != nil {
			return err
		}
		h.prepared = h.sign(http.MethodPost, "/v1/sessions/TG-UNKNOWN/replay", "nonce-unknown", now, nil, nil)
		return nil
	})
	// unauthorized id → 404 (indistinguishable)
	sc.Step(`^an authenticated replay request for an id owned by a different role$`, func() error {
		if err := h.start(); err != nil {
			return err
		}
		// The snapshot exists in the store but is NOT owned by the caller ⇒ Get returns found=false.
		h.snaps.owned["TG-FOREIGN"] = false
		h.prepared = h.sign(http.MethodPost, "/v1/sessions/TG-FOREIGN/replay", "nonce-foreign", now, nil, nil)
		return nil
	})
	sc.Step(`^the replay handler resolves the id under the caller's authority$`, func() error { return h.send(h.prepared) })
	sc.Step(`^the response is not-found$`, func() error { return h.expectStatus(http.StatusNotFound) })
	sc.Step(`^the response is not-found and reveals nothing about the foreign row$`, func() error {
		if err := h.expectStatus(http.StatusNotFound); err != nil {
			return err
		}
		if !bytes.Contains([]byte(h.lastBody), []byte("not found")) || bytes.Contains([]byte(h.lastBody), []byte("TG-FOREIGN")) {
			return fmt.Errorf("404 body must not reveal the row: %q", h.lastBody)
		}
		return nil
	})
}
