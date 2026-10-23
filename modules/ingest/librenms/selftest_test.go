package librenms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The probe is exercised end-to-end over a real TCP listener (httptest) rather than through a fake Doer,
// because two of the four things it must get right — a genuinely unreachable endpoint and the real
// X-Auth-Token header on the wire — cannot be observed through a hand-written transport that returns
// canned structs.

// probeDevicesOK is a realistic LibreNMS /api/v0/devices answer. It carries SNMP secrets exactly as the
// real endpoint does (apiDevice declares no such fields, so they are dropped at unmarshal); no test here
// may find them in anything the operator is shown.
const probeDevicesOK = `{"status":"ok","count":37,"devices":[
	{"device_id":42,"hostname":"web01.nl.example","sysName":"web01","community":"` + secretCommunity + `","authpass":"` + secretAuthpass + `"}
]}`

// probeToken is the API token value the probe must actually present. It must never appear in a Result.
const probeToken = "librenms-api-token-VALUE-must-not-leak"

// probeSrv records what the probe really sent, so a test can prove the read is a GET to the device list
// carrying the resolved credential — not a check that configuration is non-empty.
type probeSrv struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	rawQry  []string
	tokens  []string
	hits    int
}

func (r *probeSrv) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	r.methods = append(r.methods, req.Method)
	r.paths = append(r.paths, req.URL.Path)
	r.rawQry = append(r.rawQry, req.URL.RawQuery)
	r.tokens = append(r.tokens, req.Header.Get("X-Auth-Token"))
}

// newProbeServer starts a LibreNMS stand-in that records every request and answers with h.
func newProbeServer(t *testing.T, rec *probeSrv, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if rec != nil {
			rec.record(req)
		}
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveJSON answers with a fixed status and body.
func serveJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// newProbeEstate builds the reader the console would probe, pointed at base, with the token reference set
// and resolvable — every configured value present and non-empty.
func newProbeEstate(t *testing.T, site, base string) *EstateSource {
	t.Helper()
	t.Setenv("TG_TEST_PROBE_LN_TOKEN", probeToken)
	return NewEstateSource(
		[]Deployment{{Site: site, BaseURL: base, TokenRef: "env:TG_TEST_PROBE_LN_TOKEN"}},
		WithTopologyHTTPClient(http.DefaultClient),
	)
}

func TestSelfTestSingleDeployment(t *testing.T) {
	cases := []struct {
		name        string
		handler     http.HandlerFunc
		closeFirst  bool // take the URL, then close the listener: a real unreachable endpoint
		wantErr     bool
		wantSummary []string
		wantDetail  []string
	}{
		{
			// The observation must come from the SERVED payload: the count and the device name below appear
			// nowhere in the module's configuration, so a Summary carrying them can only have been read off
			// the wire.
			name:        "success reports what it observed",
			handler:     serveJSON(http.StatusOK, probeDevicesOK),
			wantSummary: []string{"1 of 1 deployment(s)", "nl", "37 device(s)", "web01.nl.example"},
		},
		{
			name:       "401 names the credential, not a vague failure",
			handler:    serveJSON(http.StatusUnauthorized, `{"status":"error","message":"Unauthenticated."}`),
			wantErr:    true,
			wantDetail: []string{"401", "token", "env:TG_TEST_PROBE_LN_TOKEN"},
		},
		{
			name:       "403 names the permission, not the token",
			handler:    serveJSON(http.StatusForbidden, `{"status":"error"}`),
			wantErr:    true,
			wantDetail: []string{"403", "not permitted"},
		},
		{
			name:       "404 points at the base URL",
			handler:    serveJSON(http.StatusNotFound, `not found`),
			wantErr:    true,
			wantDetail: []string{"404", "base URL"},
		},
		{
			name:       "5xx says the server is unhealthy, not the credential",
			handler:    serveJSON(http.StatusBadGateway, `<html>bad gateway</html>`),
			wantErr:    true,
			wantDetail: []string{"502", "unhealthy"},
		},
		{
			// 200 + the vendor envelope's own status=error: some LibreNMS builds refuse a request this way
			// instead of with a 4xx, and a probe that only checked the status code would call it a pass.
			name:       "200 with an API-error envelope is a failure",
			handler:    serveJSON(http.StatusOK, `{"status":"error","message":"Unauthenticated."}`),
			wantErr:    true,
			wantDetail: []string{"API error"},
		},
		{
			// A login page at the base URL — the classic "pointed at the wrong thing" misconfiguration.
			name:       "a non-LibreNMS answer is a failure and the body is not quoted",
			handler:    serveJSON(http.StatusOK, `<html><body>Please log in `+secretCommunity+`</body></html>`),
			wantErr:    true,
			wantDetail: []string{"not with a LibreNMS device list", "base URL"},
		},
		{
			name:       "a closed port is unreachable and an error, never a pass",
			handler:    serveJSON(http.StatusOK, probeDevicesOK),
			closeFirst: true,
			wantErr:    true,
			wantDetail: []string{"could not be reached"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &probeSrv{}
			srv := newProbeServer(t, rec, c.handler)
			if c.closeFirst {
				srv.Close()
			}
			src := newProbeEstate(t, "nl", srv.URL)

			res, err := src.SelfTest(context.Background(), "alice@example")
			if c.wantErr && err == nil {
				t.Fatalf("expected an error, got a pass: %+v", res)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected a pass, got error %v (%+v)", err, res)
			}
			if strings.TrimSpace(res.Summary) == "" {
				t.Error("Summary is empty — the operator is shown nothing")
			}
			for _, want := range c.wantSummary {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary %q does not name %q", res.Summary, want)
				}
			}
			if c.wantErr && strings.TrimSpace(res.Detail) == "" {
				t.Error("a failure must carry an actionable Detail")
			}
			for _, want := range c.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Errorf("Detail %q does not name %q", res.Detail, want)
				}
			}
			// INV-13 / selftest contract clause 5: nothing the operator can paste into a ticket may carry
			// credential material — neither TG's API token nor the SNMP secrets the device rows carry.
			shown := res.Summary + " " + res.Detail
			if err != nil {
				shown += " " + err.Error()
			}
			for _, secret := range []string{probeToken, secretCommunity, secretAuthpass} {
				if strings.Contains(shown, secret) {
					t.Fatalf("credential material leaked into the operator-visible result: %q", shown)
				}
			}
		})
	}
}

// The probe must be the REAL read: one GET, to the device list, carrying the token resolved from the
// deployment's own SecretRef, bounded so it cannot drag a whole estate onto an operator's spinner.
func TestSelfTestIssuesTheRealReadOnlyRequest(t *testing.T) {
	rec := &probeSrv{}
	srv := newProbeServer(t, rec, serveJSON(http.StatusOK, probeDevicesOK))
	src := newProbeEstate(t, "nl", srv.URL)

	if _, err := src.SelfTest(context.Background(), "alice@example"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits != 1 {
		t.Fatalf("expected exactly 1 request (one attempt, no retry), got %d: %v", rec.hits, rec.paths)
	}
	if rec.methods[0] != http.MethodGet {
		t.Errorf("probe issued %s — a self-test may only read the estate, never write to it", rec.methods[0])
	}
	if rec.paths[0] != "/api/v0/devices" {
		t.Errorf("probe read %q, want the device list the module's own readers use", rec.paths[0])
	}
	if !strings.Contains(rec.rawQry[0], "limit=") {
		t.Errorf("probe query %q is unbounded — a natural read of this endpoint returns the whole estate", rec.rawQry[0])
	}
	if rec.tokens[0] != probeToken {
		t.Errorf("probe presented %q, want the token resolved from the deployment's SecretRef", rec.tokens[0])
	}
}

// Every configured deployment is probed, and ONE bad row is red. A per-site token can be revoked
// independently, and the symptom — that site silently raising no incidents — is indistinguishable from a
// quiet estate, so a pass may never mean "at least one site answered".
func TestSelfTestCoversEveryDeploymentAndFailsOnOneBadRow(t *testing.T) {
	good := newProbeServer(t, nil, serveJSON(http.StatusOK, probeDevicesOK))
	bad := newProbeServer(t, nil, serveJSON(http.StatusUnauthorized, `{"status":"error"}`))
	t.Setenv("TG_TEST_PROBE_LN_TOKEN", probeToken)
	src := NewEstateSource([]Deployment{
		{Site: "nl", BaseURL: good.URL, TokenRef: "env:TG_TEST_PROBE_LN_TOKEN"},
		{Site: "gr", BaseURL: bad.URL, TokenRef: "env:TG_TEST_PROBE_LN_TOKEN"},
	}, WithTopologyHTTPClient(http.DefaultClient))

	res, err := src.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("one unreadable deployment must fail the test, got a pass: %+v", res)
	}
	if !strings.Contains(res.Summary, "1 of 2 deployment(s)") {
		t.Errorf("Summary %q must say how many of how many were read", res.Summary)
	}
	if !strings.Contains(res.Summary, "nl") || !strings.Contains(res.Summary, "37 device(s)") {
		t.Errorf("Summary %q must still report the healthy site's observation", res.Summary)
	}
	if !strings.Contains(res.Detail, "gr:") {
		t.Errorf("Detail %q must name WHICH deployment failed", res.Detail)
	}
	if strings.Contains(res.Detail, "nl:") {
		t.Errorf("Detail %q must not blame the healthy deployment", res.Detail)
	}
}

// A LibreNMS that answers but monitors nothing is a pass with a warning, not a failure: the promised read
// succeeded. It is still worth saying, because alerts from that site then arrive with no hostname.
func TestSelfTestWarnsOnZeroDevices(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusOK, `{"status":"ok","count":0,"devices":[]}`))
	src := newProbeEstate(t, "nl", srv.URL)

	res, err := src.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("a successful read of an empty estate is not a failure: %v", err)
	}
	if !strings.Contains(res.Summary, "no devices") {
		t.Errorf("Summary %q must say the server reported no devices", res.Summary)
	}
	if !strings.Contains(res.Detail, "NO devices") {
		t.Errorf("Detail %q must warn that nothing is monitored", res.Detail)
	}
}

// With no deployments configured there is nothing to prove, and a pass would certify a module that cannot
// ingest, cannot build topology, and gives the agent no tools.
func TestSelfTestNoDeploymentsIsAnError(t *testing.T) {
	res, err := NewEstateSource(nil).SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("no configured deployment must not pass: %+v", res)
	}
	if !strings.Contains(res.Detail, "TG_LIBRENMS_DEPLOYMENTS") {
		t.Errorf("Detail %q must name the setting to fix", res.Detail)
	}
}

// The alert puller carries the same capability, because the composition root may offer either construction
// and the registry keeps the last one that can self-test.
func TestAlertSourceSelfTestReadsTheSameList(t *testing.T) {
	rec := &probeSrv{}
	srv := newProbeServer(t, rec, serveJSON(http.StatusOK, probeDevicesOK))
	t.Setenv("TG_TEST_PROBE_LN_TOKEN", probeToken)
	src := NewAlertSource(
		[]Deployment{{Site: "nl", BaseURL: srv.URL, TokenRef: "env:TG_TEST_PROBE_LN_TOKEN"}},
		WithAlertHTTPClient(http.DefaultClient),
	)

	res, err := src.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !strings.Contains(res.Summary, "37 device(s)") || !strings.Contains(res.Summary, "web01.nl.example") {
		t.Errorf("Summary %q must report what the alert puller's own transport observed", res.Summary)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.hits != 1 || rec.methods[0] != http.MethodGet {
		t.Fatalf("expected one GET, got %v %v", rec.methods, rec.paths)
	}
}

// A HEALTHY LibreNMS WITH A BIG ESTATE MUST NOT BE DIAGNOSED AS THE WRONG APPLICATION.
//
// `?limit=` is not part of LibreNMS's list_devices contract — the module's own alert poller asks for
// `?limit=500` and LibreNMS may answer with the whole estate anyway — so the probe must assume the answer can
// be large. A body cut off at the read cap decodes exactly like a login page, and telling an operator with a
// perfectly good LibreNMS to go and fix their base URL is the confidently-wrong diagnosis the contract's
// clause 3 forbids. Whatever the outcome, the message must NOT blame the address or the credential.
func TestSelfTestDoesNotMisdiagnoseALargeEstate(t *testing.T) {
	// A valid device list far larger than a page: well over 1 MiB, comfortably inside the reader's own cap.
	var b strings.Builder
	b.WriteString(`{"status":"ok","count":900,"devices":[`)
	for i := 0; i < 900; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"device_id":` + strconv.Itoa(i+1) + `,"hostname":"host` + strconv.Itoa(i) +
			`.nl.example","sysName":"h","sysDescr":"` + strings.Repeat("y", 1200) + `"}`)
	}
	b.WriteString(`]}`)
	if b.Len() < 1<<20 {
		t.Fatalf("the premise requires a body over 1 MiB, got %d bytes", b.Len())
	}

	srv := newProbeServer(t, nil, serveJSON(http.StatusOK, b.String()))
	res, err := newProbeEstate(t, "nl", srv.URL).SelfTest(context.Background(), "alice@example")
	if err != nil {
		// Permitted (the read was inconclusive) — but it may not blame a configuration that is correct.
		for _, forbidden := range []string{"reverse proxy", "login page", "different application"} {
			if strings.Contains(res.Detail, forbidden) {
				t.Fatalf("a healthy LibreNMS with a large estate was diagnosed as %q: %s", forbidden, res.Detail)
			}
		}
		if !strings.Contains(res.Detail, "WORKED") {
			t.Errorf("Detail %q must tell the operator the credential and address are not at fault", res.Detail)
		}
		return
	}
	if !strings.Contains(res.Summary, "900 device(s)") {
		t.Errorf("Summary %q must report the estate LibreNMS actually stated", res.Summary)
	}
}

// A base URL that cannot be parsed must not republish it. net/http strips the password from a TRANSPORT
// error; net/url does not strip anything from a PARSE error, and a base URL may carry userinfo.
func TestSelfTestNeverEchoesAnUnparseableBaseURL(t *testing.T) {
	const pw = "hunter2-must-not-leak"
	t.Setenv("TG_TEST_PROBE_LN_TOKEN", probeToken)
	src := NewEstateSource([]Deployment{{
		Site:     "nl",
		BaseURL:  "https://tg:" + pw + "@nms.nl.example\x7f", // a stray control character: url.Parse refuses it
		TokenRef: "env:TG_TEST_PROBE_LN_TOKEN",
	}}, WithTopologyHTTPClient(http.DefaultClient))

	res, err := src.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("an unusable base URL must not pass: %+v", res)
	}
	shown := res.Summary + " " + res.Detail + " " + err.Error()
	if strings.Contains(shown, pw) {
		t.Fatalf("the base URL's password leaked into the operator-visible result: %q", shown)
	}
	if !strings.Contains(res.Detail, "base URL") {
		t.Errorf("Detail %q must still name what the operator has to fix", res.Detail)
	}
}

// THE KILLING ORACLE.
//
// Every configured value is present and non-empty — a site label, a base URL, and a token reference that
// resolves to a real non-empty token — and the endpoint answers 401. A SelfTest replaced by a
// configured-values-are-non-empty check passes here; this test fails it. That is the whole difference
// between a probe and a mock wearing a test's name: the three faults an operator presses TEST to rule out
// (a revoked credential, a permission never granted, a host down for a week) are all invisible to a
// configuration check, and all of them look exactly like this.
func TestSelfTestFailsWithCompleteConfigAgainstARejectingServer(t *testing.T) {
	srv := newProbeServer(t, nil, serveJSON(http.StatusUnauthorized, `{"status":"error","message":"Unauthenticated."}`))
	t.Setenv("TG_TEST_PROBE_LN_TOKEN", probeToken)
	dep := Deployment{Site: "nl", BaseURL: srv.URL, TokenRef: "env:TG_TEST_PROBE_LN_TOKEN", Timezone: "Europe/Amsterdam"}

	// Guard the premise: if any of these were empty the test would prove nothing.
	if dep.Site == "" || dep.BaseURL == "" || dep.TokenRef == "" {
		t.Fatal("the oracle requires a COMPLETE configuration")
	}
	if tok, err := config.SecretRef(dep.TokenRef).Resolve(); err != nil || tok == "" {
		t.Fatalf("the oracle requires a token reference that really resolves: %v", err)
	}

	res, err := NewEstateSource([]Deployment{dep}, WithTopologyHTTPClient(http.DefaultClient)).
		SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatalf("a complete configuration pointed at a rejecting server MUST fail: %+v", res)
	}
	if !strings.Contains(res.Detail, "401") {
		t.Errorf("Detail %q must name the rejection so the operator knows the token is the problem", res.Detail)
	}

	// The same completeness against a dead port must also fail — the "host has been down for a week" case.
	dead := newProbeServer(t, nil, serveJSON(http.StatusOK, probeDevicesOK))
	dead.Close()
	deadDep := Deployment{Site: "nl", BaseURL: dead.URL, TokenRef: "env:TG_TEST_PROBE_LN_TOKEN"}
	if res, err := NewEstateSource([]Deployment{deadDep}, WithTopologyHTTPClient(http.DefaultClient)).
		SelfTest(context.Background(), "alice@example"); err == nil {
		t.Fatalf("a complete configuration pointed at a dead port MUST fail: %+v", res)
	}
}
