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
	"strings"
	"time"

	"github.com/cucumber/godog"

	ingestadapter "github.com/territory-grounder/grounder/adapters/ingest"
	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
	contracts "github.com/territory-grounder/grounder/docs/contracts"
)

// REQ-508 oracles: the browser operator session drives the REAL router + REAL httpapi.Register — an
// end-to-end httptest server with the same fakes the REQ-501 oracles use. No step fabricates a result:
// every Then inspects an actual HTTP response (INV-22).

const sessionTestToken = "acceptance-operator-token-1234567890"

type sessionWorld struct {
	srv        *httptest.Server
	cookie     *http.Cookie
	resp       *http.Response
	body       string
	operator   string
	alerts     httpapi.AlertLog
	ingesters  httpapi.IngesterResolver
	governance httpapi.GovernanceReader
	secrets    httpapi.SecretsReader
	models     httpapi.ModelsReader
	contract   []byte
	estate     httpapi.EstateReader
	grounding  httpapi.GroundingReader
	votes      httpapi.VoteSignaler
}

// build (re)constructs the authenticated surface, optionally with the sessions read spine wired.
func (w *sessionWorld) build(spine httpapi.SessionsReader) error {
	w.close()
	ops := auth.MemOperators{w.operator: {Name: w.operator, TokenSHA256: sha256.Sum256([]byte(sessionTestToken))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("s", 32)), auth.NewMemSessionStore(), ops, time.Hour)
	if err != nil {
		return err
	}
	sa.Secure = false // httptest is plain http
	verifier, err := auth.NewVerifier(fakeSources{}, &fakeNonces{}, time.Hour)
	if err != nil {
		return err
	}
	verifier.EnableBrowserSessions(sa)
	rt := auth.NewRouter(verifier)
	httpapi.Register(rt, httpapi.Deps{
		Stats:        &fakeStats{},
		Ingesters:    w.ingesters, // nil unless a scenario wires the front door (reject-before-handler holds)
		Sessions:     sa,
		SessionsRead: spine,
		Alerts:       w.alerts,
		Governance:   w.governance,
		SecretsRead:  w.secrets,
		Models:       w.models,
		Contract:     w.contract,
		Estate:       w.estate,
		Grounding:    w.grounding,
		Votes:        w.votes,
	})
	w.srv = httptest.NewServer(rt.Mux())
	return nil
}

func (w *sessionWorld) close() {
	if w.srv != nil {
		w.srv.Close()
	}
}

func (w *sessionWorld) do(req *http.Request) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return err
	}
	w.resp, w.body = resp, buf.String()
	return nil
}

func registerSessionSteps(sc *godog.ScenarioContext) {
	w := &sessionWorld{}
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.close()
		*w = sessionWorld{}
		return ctx, nil
	})

	login := func(token, remoteHint string) error {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/session", nil)
		req.Header.Set("X-TG-Operator", "kyriakos")
		req.Header.Set("Authorization", "Bearer "+token)
		if remoteHint != "" {
			req.Header.Set("X-Acceptance-Hint", remoteHint)
		}
		if err := w.do(req); err != nil {
			return err
		}
		w.cookie = nil
		if w.resp.StatusCode == http.StatusOK {
			for _, c := range w.resp.Cookies() {
				if c.Name == auth.SessionCookieName {
					w.cookie = c
				}
			}
		}
		return nil
	}

	sc.Step(`^a session-enabled interface surface with operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		return w.build(nil)
	})

	sc.Step(`^the operator logs in with the valid token$`, func() error {
		if err := login(sessionTestToken, ""); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusOK || w.cookie == nil {
			return fmt.Errorf("login: status=%d cookie=%v body=%q", w.resp.StatusCode, w.cookie, w.body)
		}
		return nil
	})
	sc.Step(`^a session cookie is issued$`, func() error {
		if w.cookie == nil || !w.cookie.HttpOnly || !strings.Contains(w.cookie.Value, ".") {
			return fmt.Errorf("expected an HttpOnly signed session cookie, got %+v", w.cookie)
		}
		return nil
	})
	sc.Step(`^a session GET of the stats route succeeds as "([^"]*)"$`, func(want string) error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/whoami", nil)
		req.AddCookie(w.cookie)
		if err := w.do(req); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusOK || !strings.Contains(w.body, want) {
			return fmt.Errorf("whoami over session: status=%d body=%q want %q", w.resp.StatusCode, w.body, want)
		}
		return nil
	})
	sc.Step(`^the session cookie is presented to the ingest route$`, func() error {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/ingest/prometheus", strings.NewReader(`{}`))
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the request is rejected unauthenticated before the handler runs$`, func() error {
		if w.resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("expected 401, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^the session performs a POST against the stats route$`, func() error {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/stats", strings.NewReader(`{}`))
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the request is rejected as read-only$`, func() error {
		if w.resp.StatusCode != http.StatusForbidden {
			return fmt.Errorf("expected 403 read-only, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^the session cookie is tampered with$`, func() error {
		if w.cookie == nil {
			return fmt.Errorf("no session cookie to tamper with")
		}
		id, _, _ := strings.Cut(w.cookie.Value, ".")
		w.cookie = &http.Cookie{Name: auth.SessionCookieName, Value: id + "." + strings.Repeat("0", 64)}
		return nil
	})
	sc.Step(`^a session GET of the stats route is rejected unauthenticated$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/stats", nil)
		req.AddCookie(w.cookie)
		if err := w.do(req); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("expected 401, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^the operator logs out$`, func() error {
		// re-login first so we hold a VALID cookie again (the tamper step replaced it)
		if err := login(sessionTestToken, ""); err != nil {
			return err
		}
		valid := w.cookie
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/session/logout", nil)
		req.AddCookie(valid)
		if err := w.do(req); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("logout: expected 204, got %d (%q)", w.resp.StatusCode, w.body)
		}
		w.cookie = valid // the browser still HOLDS the cookie — but the server revoked it
		return nil
	})
	sc.Step(`^five logins fail with a wrong token$`, func() error {
		for i := 0; i < 5; i++ {
			if err := login("wrong-token", ""); err != nil {
				return err
			}
			if w.resp.StatusCode != http.StatusUnauthorized {
				return fmt.Errorf("failed login %d: expected 401, got %d", i, w.resp.StatusCode)
			}
		}
		return nil
	})
	sc.Step(`^a sixth login is rate-limited even with the valid token$`, func() error {
		if err := login(sessionTestToken, ""); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("expected 429, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})

	// REQ-509 — the sessions read surface, sharing this world's server + cookie.
	registerSessionsReadSteps(sc, w)
	// REQ-510 — the alerts read surface (accepted-envelope log at the front door).
	registerAlertsReadSteps(sc, w)
	// REQ-511/512 — governance posture + secret references.
	registerGovernanceSteps(sc, w)
	// REQ-513 — the liveness stream.
	registerEventsSteps(sc, w)
	// REQ-514 — the models passthrough.
	registerModelsSteps(sc, w)
	// REQ-515 — the contract surface.
	registerContractSteps(sc, w)
	// REQ-516 — the estate read surface.
	registerEstateSteps(sc, w)
	// REQ-517 — the grounding scorecard.
	registerGroundingSteps(sc, w)
	// REQ-518 — the vote intake.
	registerVoteSteps(sc, w)
}

// --- REQ-509: the sessions read surface ---

// fakeSessionsRead is the in-memory audit-spine fake: it returns exactly what the spine recorded.
type fakeSessionsRead struct{ rows []httpapi.SessionSummary }

func (f *fakeSessionsRead) RecentSessions(_ context.Context, _ auth.Principal, limit int) ([]httpapi.SessionSummary, error) {
	if limit < len(f.rows) {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}

func registerSessionsReadSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	spine := &fakeSessionsRead{}
	sc.Step(`^the audit spine holds a classified session with a deviation verdict$`, func() error {
		spine.rows = []httpapi.SessionSummary{{
			ExternalRef: "librenms:22101", Band: "POLL_PAUSE", RiskLevel: "high",
			ActionID: "e5c8a1229f6b7704", PlanHash: "ph-1", Verdict: "deviation",
			Signals: map[string]string{"rule": "service_down"},
		}}
		// rebuild the surface WITH the spine wired
		return w.build(spine)
	})
	sc.Step(`^the operator lists the recent sessions$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/sessions", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the session list carries the spine's band, action id, and verdict unchanged$`, func() error {
		if w.resp.StatusCode != http.StatusOK {
			return fmt.Errorf("sessions: expected 200, got %d (%q)", w.resp.StatusCode, w.body)
		}
		for _, want := range []string{`"band":"POLL_PAUSE"`, `"action_id":"e5c8a1229f6b7704"`, `"verdict":"deviation"`} {
			if !strings.Contains(w.body, want) {
				return fmt.Errorf("sessions body missing %s: %q", want, w.body)
			}
		}
		return nil
	})
	sc.Step(`^the operator lists the recent sessions without a wired spine$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/sessions", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the sessions request is rejected as unavailable$`, func() error {
		if w.resp.StatusCode != http.StatusServiceUnavailable {
			return fmt.Errorf("expected 503 fail-closed, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
}

// --- REQ-510: the alerts read surface ---

// fakeAcceptIngester accepts any payload not containing "bad" and returns a fixed validated envelope.
type fakeAcceptIngester struct{}

func (fakeAcceptIngester) SourceType() string { return "librenms" }
func (fakeAcceptIngester) Normalize(_ context.Context, raw []byte) (coreingest.IncidentEnvelope, error) {
	if strings.Contains(string(raw), "bad") {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("grammar violation")
	}
	return coreingest.IncidentEnvelope{
		ExternalRef: "librenms:9001", SourceID: testSource, AlertRule: "mem_util",
		Severity: coreingest.SeverityCritical, Host: "dc1k8s-w3", Site: "dc1",
		Summary: "mem_util 94%", ObservedAt: ingestNow, ReceivedAt: ingestNow,
	}, nil
}

type fakeIngesterResolver struct{}

func (fakeIngesterResolver) ResolveIngester(st string) (ingestadapter.Ingester, error) {
	if st != "librenms" {
		return nil, fmt.Errorf("unknown source type")
	}
	return fakeAcceptIngester{}, nil
}

// signedIngest builds an HMAC-signed ingest POST exactly as core/auth verifies it.
func (w *sessionWorld) signedIngest(nonce string, body []byte) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/ingest/librenms", bytes.NewReader(body))
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(tsStr))
	mac.Write([]byte("\n"))
	mac.Write([]byte(nonce))
	mac.Write([]byte("\n"))
	mac.Write(body)
	req.Header.Set("X-TG-Source", testSource)
	req.Header.Set("X-TG-Timestamp", tsStr)
	req.Header.Set("X-TG-Nonce", nonce)
	req.Header.Set("X-TG-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}

func registerAlertsReadSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^a session-enabled interface surface with an alert log and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.alerts = httpapi.NewMemAlertLog(100)
		w.ingesters = fakeIngesterResolver{}
		return w.build(nil)
	})
	sc.Step(`^an authenticated source ingests a valid alert payload$`, func() error {
		if err := w.do(w.signedIngest("nonce-ok-1", []byte(`{"rule":"mem_util"}`))); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("ingest: expected 202, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^an authenticated source ingests a grammar-violating payload$`, func() error {
		if err := w.do(w.signedIngest("nonce-bad-1", []byte(`{"rule":"bad"}`))); err != nil {
			return err
		}
		if w.resp.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("rejected ingest: expected 400, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^the operator lists the recent alerts$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/alerts", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the alert list carries the accepted envelope's rule and severity unchanged$`, func() error {
		if w.resp.StatusCode != http.StatusOK {
			return fmt.Errorf("alerts: expected 200, got %d (%q)", w.resp.StatusCode, w.body)
		}
		for _, want := range []string{`"alert_rule":"mem_util"`, `"severity":"critical"`, `"external_ref":"librenms:9001"`} {
			if !strings.Contains(w.body, want) {
				return fmt.Errorf("alerts body missing %s: %q", want, w.body)
			}
		}
		return nil
	})
	sc.Step(`^the alert list is empty$`, func() error {
		if w.resp.StatusCode != http.StatusOK || !strings.Contains(w.body, `"alerts":[]`) {
			return fmt.Errorf("expected an empty alert list, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
}

// --- REQ-511/512: governance + secret-reference surfaces ---

const sessionsSecretValue = "resolvable-secret-value-do-not-serve"

type fakeGovernance struct{}

func (fakeGovernance) Governance(_ context.Context, _ auth.Principal) (httpapi.GovernanceState, error) {
	return httpapi.GovernanceState{
		MutationEnabled: false, PreflightGreen: true,
		Bands: map[string]int{"POLL_PAUSE": 3, "AUTO": 1},
		Chain: httpapi.ChainHead{Seq: 42, Hash: "abcd1234"},
	}, nil
}

type fakeSecretsRead struct{}

func (fakeSecretsRead) SecretRefs(_ context.Context, _ auth.Principal) ([]httpapi.SecretRefStatus, error) {
	// The fake resolves a real value internally and DISCARDS it — exactly like the composition does.
	_ = sessionsSecretValue
	return []httpapi.SecretRefStatus{{Ref: "env:TG_SESSION_KEY", Purpose: "session cookie signing key", Resolved: true}}, nil
}

func registerGovernanceSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^a session-enabled interface surface with governance state and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.governance = fakeGovernance{}
		w.secrets = fakeSecretsRead{}
		return w.build(nil)
	})
	sc.Step(`^the operator reads the governance surface$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/governance", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the posture carries mutation off, the spine's band counts, and the chain head unchanged$`, func() error {
		if w.resp.StatusCode != http.StatusOK {
			return fmt.Errorf("governance: expected 200, got %d (%q)", w.resp.StatusCode, w.body)
		}
		for _, want := range []string{`"mutation_enabled":false`, `"POLL_PAUSE":3`, `"seq":42`, `"hash":"abcd1234"`} {
			if !strings.Contains(w.body, want) {
				return fmt.Errorf("governance body missing %s: %q", want, w.body)
			}
		}
		return nil
	})
	sc.Step(`^the operator reads the secrets surface$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/secrets", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the reference list carries the reference and resolution state$`, func() error {
		if w.resp.StatusCode != http.StatusOK || !strings.Contains(w.body, `"ref":"env:TG_SESSION_KEY"`) || !strings.Contains(w.body, `"resolved":true`) {
			return fmt.Errorf("secrets: %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^the response never contains the resolvable secret value$`, func() error {
		if strings.Contains(w.body, sessionsSecretValue) {
			return fmt.Errorf("SECRET VALUE LEAKED onto the reference surface")
		}
		return nil
	})
}

// --- REQ-513: the liveness stream ---

func registerEventsSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^the operator connects to the events stream$`, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.srv.URL+"/v1/events", nil)
		req.AddCookie(w.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			return fmt.Errorf("events: status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		// read just the first event block, then hang up (the stream is unbounded by design)
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		w.body = string(buf[:n])
		w.resp = resp
		return nil
	})
	sc.Step(`^the first event is a posture snapshot carrying mutation off and the chain head$`, func() error {
		for _, want := range []string{"event: posture", `"mutation_enabled":false`, `"seq":42`} {
			if !strings.Contains(w.body, want) {
				return fmt.Errorf("events first block missing %s: %q", want, w.body)
			}
		}
		return nil
	})
}

// --- REQ-514: the models passthrough surface ---

type fakeModelsRead struct{ body []byte }

func (f *fakeModelsRead) ModelsUsage(_ context.Context, _ auth.Principal) ([]byte, error) {
	return f.body, nil
}

func registerModelsSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^a session-enabled interface surface with a model gateway report and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.models = &fakeModelsRead{body: []byte(`{"data":[{"model_name":"claude-opus-4-8","litellm_params":{"custom_llm_provider":"anthropic"}}]}`)}
		return w.build(nil)
	})
	sc.Step(`^the operator reads the models surface$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/models", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the gateway report is relayed verbatim$`, func() error {
		if w.resp.StatusCode != http.StatusOK || !strings.Contains(w.body, `"model_name":"claude-opus-4-8"`) {
			return fmt.Errorf("models: %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
	sc.Step(`^the operator reads the models surface without a wired gateway$`, func() error {
		w.models = nil
		if err := w.build(nil); err != nil {
			return err
		}
		// a fresh surface means a fresh session store — log in again before reading
		req0, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/session", nil)
		req0.Header.Set("X-TG-Operator", w.operator)
		req0.Header.Set("Authorization", "Bearer "+sessionTestToken)
		if err := w.do(req0); err != nil {
			return err
		}
		for _, c := range w.resp.Cookies() {
			if c.Name == auth.SessionCookieName {
				w.cookie = c
			}
		}
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/models", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the models request is rejected as unavailable$`, func() error {
		if w.resp.StatusCode != http.StatusServiceUnavailable {
			return fmt.Errorf("expected 503 fail-closed, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
}

// --- REQ-515: the contract surface serves the REAL embedded artifact ---

func registerContractSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^a session-enabled interface surface with the embedded contract and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.contract = contracts.OpenAPI // the real generated artifact, not a fixture
		return w.build(nil)
	})
	sc.Step(`^the operator reads the contract surface$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/contract", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the generated OpenAPI document is served verbatim$`, func() error {
		if w.resp.StatusCode != http.StatusOK || !strings.HasPrefix(w.resp.Header.Get("Content-Type"), "application/yaml") {
			return fmt.Errorf("contract: status=%d ct=%q", w.resp.StatusCode, w.resp.Header.Get("Content-Type"))
		}
		if w.body != string(contracts.OpenAPI) {
			return fmt.Errorf("contract not verbatim: got %d bytes, want %d", len(w.body), len(contracts.OpenAPI))
		}
		if !strings.Contains(w.body, "/v1/contract:") {
			return fmt.Errorf("the served map must include the contract route itself (drift gate)")
		}
		return nil
	})
}

// --- REQ-516: the estate read surface ---

type fakeEstate struct{ snap httpapi.EstateSnapshot }

func (f *fakeEstate) LatestEstate(_ context.Context, _ auth.Principal) (httpapi.EstateSnapshot, error) {
	return f.snap, nil
}

func registerEstateSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^a session-enabled interface surface with a published estate snapshot and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.estate = &fakeEstate{snap: httpapi.EstateSnapshot{
			Available: true, CapturedAt: "2026-07-17T02:00:00Z", NodeCount: 2, EdgeCount: 1, SourceCount: 3,
			Nodes: []httpapi.EstateNode{{Name: "dc1k8s-w3", Type: "host"}, {Name: "dc1k8s-cp1", Type: "host"}},
			Edges: []httpapi.EstateEdge{{From: "dc1k8s-w3", To: "dc1k8s-cp1", Rel: "runs_on", Confidence: 0.95, Source: "pve"}},
		}}
		return w.build(nil)
	})
	sc.Step(`^a session-enabled interface surface with no estate snapshot and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.estate = &fakeEstate{snap: httpapi.EstateSnapshot{Available: false}}
		return w.build(nil)
	})
	sc.Step(`^the operator reads the estate surface$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/estate", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the estate carries the published nodes and confidence-weighted edges unchanged$`, func() error {
		if w.resp.StatusCode != http.StatusOK {
			return fmt.Errorf("estate: expected 200, got %d (%q)", w.resp.StatusCode, w.body)
		}
		for _, want := range []string{`"available":true`, `"dc1k8s-w3"`, `"rel":"runs_on"`, `"confidence":0.95`, `"source":"pve"`} {
			if !strings.Contains(w.body, want) {
				return fmt.Errorf("estate body missing %s: %q", want, w.body)
			}
		}
		return nil
	})
	sc.Step(`^the estate reports it is unavailable with no nodes$`, func() error {
		if w.resp.StatusCode != http.StatusOK || !strings.Contains(w.body, `"available":false`) || !strings.Contains(w.body, `"nodes":[]`) {
			return fmt.Errorf("estate empty state: %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
}

// --- REQ-517: the grounding scorecard ---

type fakeGrounding struct{ sc httpapi.GroundingScorecard }

func (f *fakeGrounding) Grounding(_ context.Context, _ auth.Principal) (httpapi.GroundingScorecard, error) {
	return f.sc, nil
}

func registerGroundingSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	sc.Step(`^a session-enabled interface surface with a scored grounding spine and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		// a real distribution: 7 match / 2 partial / 1 deviation, and a real prediction that beats its
		// shuffled-graph control (avg real tp 3 vs control tp 1 → signal ratio 3).
		w.grounding = &fakeGrounding{sc: httpapi.GroundingScorecard{
			Verdicts: map[string]int{"match": 7, "partial": 2, "deviation": 1}, VerdictTotal: 10, MatchRate: 0.7,
			Predictions: 4, AvgRealTP: 3, AvgControlTP: 1, SignalRatio: 3, Precision: 0.8, Recall: 0.75,
			Bands: map[string]int{"AUTO": 6, "AUTO_NOTICE": 2, "POLL_PAUSE": 2}, FloorHolds: 2,
		}}
		return w.build(nil)
	})
	sc.Step(`^a session-enabled interface surface with an empty grounding spine and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		w.grounding = &fakeGrounding{sc: httpapi.GroundingScorecard{Verdicts: map[string]int{}, Bands: map[string]int{}}}
		return w.build(nil)
	})
	sc.Step(`^the operator reads the grounding scorecard$`, func() error {
		req, _ := http.NewRequest(http.MethodGet, w.srv.URL+"/v1/grounding", nil)
		req.AddCookie(w.cookie)
		return w.do(req)
	})
	sc.Step(`^the scorecard carries the verdict distribution and the falsifiability signal$`, func() error {
		if w.resp.StatusCode != http.StatusOK {
			return fmt.Errorf("grounding: expected 200, got %d (%q)", w.resp.StatusCode, w.body)
		}
		for _, want := range []string{`"match":7`, `"match_rate":0.7`, `"signal_ratio":3`, `"floor_holds":2`} {
			if !strings.Contains(w.body, want) {
				return fmt.Errorf("grounding body missing %s: %q", want, w.body)
			}
		}
		return nil
	})
	sc.Step(`^the scorecard reports honest zeros rather than a fabricated rate$`, func() error {
		if w.resp.StatusCode != http.StatusOK || !strings.Contains(w.body, `"match_rate":0`) || !strings.Contains(w.body, `"verdict_total":0`) {
			return fmt.Errorf("grounding empty state: %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
}

// --- REQ-518: the vote intake ---

// fakeVotes accepts a vote only for the ref a session is "waiting" on — any other ref returns the
// closed-decision-window sentinel, like the Temporal adapter mapping a NotFound.
type fakeVotes struct {
	waiting                     string
	gotRef, gotAction, gotVoter string
	gotApprove                  bool
}

func (f *fakeVotes) SignalVote(_ context.Context, ref, actionID string, approve bool, voter string) error {
	if ref != f.waiting {
		return httpapi.ErrNoWaitingDecision
	}
	f.gotRef, f.gotAction, f.gotApprove, f.gotVoter = ref, actionID, approve, voter
	return nil
}

func registerVoteSteps(sc *godog.ScenarioContext, w *sessionWorld) {
	votes := &fakeVotes{}
	sc.Step(`^a session-enabled interface surface with a waiting decision and operator "([^"]*)"$`, func(name string) error {
		w.operator = name
		votes.waiting = "TG-3"
		w.votes = votes
		return w.build(nil)
	})
	post := func(ref string) error {
		req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/vote",
			strings.NewReader(fmt.Sprintf(`{"external_ref":%q,"action_id":"act-3","approve":true}`, ref)))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(w.cookie)
		return w.do(req)
	}
	sc.Step(`^the operator votes to approve the waiting decision$`, func() error { return post("TG-3") })
	sc.Step(`^the operator votes on a decision no session is waiting for$`, func() error { return post("TG-404") })
	sc.Step(`^the vote is delivered bound to that decision with the session identity as voter$`, func() error {
		if w.resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("vote: expected 202, got %d (%q)", w.resp.StatusCode, w.body)
		}
		if votes.gotRef != "TG-3" || votes.gotAction != "act-3" || !votes.gotApprove {
			return fmt.Errorf("vote not delivered bound to its decision+action: %+v", votes)
		}
		if votes.gotVoter != w.operator {
			return fmt.Errorf("voter %q must be the SESSION identity %q, never a client claim", votes.gotVoter, w.operator)
		}
		return nil
	})
	sc.Step(`^the vote is rejected as a closed decision window$`, func() error {
		if w.resp.StatusCode != http.StatusConflict {
			return fmt.Errorf("closed window: expected 409, got %d (%q)", w.resp.StatusCode, w.body)
		}
		return nil
	})
}
