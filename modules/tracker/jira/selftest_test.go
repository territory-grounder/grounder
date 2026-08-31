package jira

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// Oracles for the TEST button (core/selftest.Tester).
//
// They drive a real httptest server through the module's real `do`, so the parts a stub cannot judge —
// URL building, token resolution from a secret reference, and above all the HTTP BASIC scheme Jira Cloud
// requires — are the parts under test. The probe's whole claim is that the network path works with the
// real credential; an oracle that faked the network path would be asserting nothing.

// probeSrv answers per PATH and records every request.
type probeSrv struct {
	byPath map[string]probeReply
	seen   []probeSeen
}

type probeReply struct {
	status int
	body   string
}

type probeSeen struct{ method, path, query, auth string }

func (p *probeSrv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.seen = append(p.seen, probeSeen{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
	})
	rep, ok := p.byPath[r.URL.Path]
	if !ok {
		http.Error(w, `{"errorMessages":["Not Found"]}`, http.StatusNotFound)
		return
	}
	if rep.status != 0 && rep.status != http.StatusOK {
		http.Error(w, rep.body, rep.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, rep.body)
}

const (
	probeMyselfURLPath   = "/rest/api/2/myself"
	probeProjectsURLPath = "/rest/api/2/project/search"

	probeMyselfJSON = `{"accountId":"5b10ac8d82e05b22cc7d4ef5","displayName":"TG Reader",
	                    "emailAddress":"grounder-bot@example.com","active":true}`
	probeProjectsJSON = `{"total":7,"maxResults":5,"values":[
	                      {"key":"TG","name":"Territory Grounder"},
	                      {"key":"OPS","name":"Operations"}]}`
)

const probeTokenEnv = "TG_TEST_JIRA_PROBE_TOKEN"

// newProbeFixture wires the module exactly as production does: base URL, account email, and a token
// RESOLVED FROM ITS REFERENCE at call time (INV-13) rather than injected pre-resolved.
func newProbeFixture(t *testing.T, srvURL string) *Module {
	t.Helper()
	t.Setenv(probeTokenEnv, "tok-abc123")
	return New(srvURL, testEmail, config.SecretRef("env:"+probeTokenEnv))
}

func probeOKRoutes() map[string]probeReply {
	return map[string]probeReply{
		probeMyselfURLPath:   {body: probeMyselfJSON},
		probeProjectsURLPath: {body: probeProjectsJSON},
	}
}

// A green TEST must name what it OBSERVED, not merely that it worked: "ok" cannot distinguish a correctly
// configured module from one pointed at a sandbox clone of the same product. Every observation asserted
// here comes from the SERVED payload, so a probe reporting hardcoded prose would fail.
func TestSelfTestReportsTheAccountAndProjectsItObserved(t *testing.T) {
	h := &probeSrv{byPath: probeOKRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("SelfTest must pass against a healthy site: %v", err)
	}
	for _, want := range []string{
		srv.Listener.Addr().String(), // WHICH site — the wrong-instance tell
		"TG Reader",                  // WHO the credential authenticates as, from /myself
		"7 projects",                 // the site's own total, not a locally counted page
		"TG",                         // recognisable at a glance; a bare count is not
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary must report %q, got %q", want, res.Summary)
		}
	}
	if strings.Contains(res.Summary+res.Detail, "tok-abc123") {
		t.Fatal("the API token leaked into the Result")
	}

	// REGRESSION SEAM: Jira Cloud authenticates as HTTP Basic base64(email:api_token). A probe that sent
	// a bare Bearer would 401 against every real site while passing against a lenient fake, which is
	// precisely the class of bug this capability exists to catch.
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(testEmail+":tok-abc123"))
	if len(h.seen) != 2 {
		t.Fatalf("want the two documented GETs, got %d: %+v", len(h.seen), h.seen)
	}
	for _, got := range h.seen {
		if got.method != http.MethodGet {
			t.Errorf("the probe must be read-only; it issued %s %s", got.method, got.path)
		}
		if got.auth != wantAuth {
			t.Errorf("request to %s did not carry Basic base64(email:token): %q", got.path, got.auth)
		}
	}
	// The project read must be BOUNDED: an unbounded listing on a large site is a multi-megabyte response
	// inside a 30-second dialog that moduletest will not retry.
	if !strings.Contains(h.seen[1].query, "maxResults=5") {
		t.Errorf("the project read must cap its rows, got query %q", h.seen[1].query)
	}
}

// The descriptor's verb is the consent contract and it now names these two reads. If the probe stops
// making them the dialog is lying again, so the paths are pinned.
func TestSelfTestCallsTheEndpointsTheDescriptorPromises(t *testing.T) {
	h := &probeSrv{byPath: probeOKRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), ""); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if h.seen[0].path != probeMyselfURLPath || h.seen[1].path != probeProjectsURLPath {
		t.Fatalf("probe called %q then %q, want %q then %q",
			h.seen[0].path, h.seen[1].path, probeMyselfURLPath, probeProjectsURLPath)
	}
}

// THE KILLING ORACLE. Every configured value is present and non-empty — site URL, account email, token
// reference, and a token that resolves — and the site rejects the credential. A probe implemented as a
// "configured-values-are-non-empty" check passes this case; this one must fail it.
//
// That is what makes the probe more than a mock: a revoked token, a permission never granted, and a site
// that has been unreachable for a week all have complete, non-empty configuration.
func TestSelfTestFailsWithFullConfigWhenTheSiteRejectsTheCredential(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeMyselfURLPath: {status: http.StatusUnauthorized, body: `{"errorMessages":["Unauthorized"]}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	m := newProbeFixture(t, srv.URL)

	// Config is complete: this is not a "missing value" case.
	if m.baseURL == "" || m.email == "" || m.tokenRef == "" {
		t.Fatal("fixture is wrong: the killing oracle requires COMPLETE configuration")
	}
	if tok, err := m.tokenRef.Resolve(); err != nil || tok == "" {
		t.Fatalf("fixture is wrong: the token must resolve to a real value (%q, %v)", tok, err)
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatal("a rejected credential must FAIL the test; a pass here would certify a revoked token")
	}
	// Jira's 401 is ambiguous between the two halves of the Basic credential, and saying so is the
	// actionable part: an operator who only rotates the token can chase a wrong email for hours.
	if !strings.Contains(res.Detail, "Account email") || !strings.Contains(res.Detail, "token") {
		t.Errorf("Detail must name BOTH halves of the credential, got %q", res.Detail)
	}
}

// Failure classification, on the SHAPE of the failure rather than vendor prose. Each case is a distinct
// thing an operator has to do something different about.
func TestSelfTestClassifiesFailuresActionably(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  probeReply
		want   []string
		reject []string
	}{
		{
			name:  "401 names both halves of the Basic credential",
			reply: probeReply{status: http.StatusUnauthorized, body: `{"errorMessages":["Unauthorized"]}`},
			want:  []string{"Account email", "revoked"},
		},
		{
			name:  "403 names the permission, not the token",
			reply: probeReply{status: http.StatusForbidden, body: `{"errorMessages":["Forbidden"]}`},
			want:  []string{"authenticated", "permission"},
			// A 403 is not a bad token; telling an operator to rotate a working credential sends them
			// away from the actual fix.
			reject: []string{"revoked"},
		},
		{
			name:  "404 points at the Site URL",
			reply: probeReply{status: http.StatusNotFound, body: `{"errorMessages":["Not Found"]}`},
			want:  []string{"Site URL", "atlassian.net"},
		},
		{
			name:  "5xx blames the site, not the configuration",
			reply: probeReply{status: http.StatusBadGateway, body: `<html>bad gateway</html>`},
			want:  []string{"unhealthy", "not a credential problem"},
		},
		{
			name:  "a 200 that is not Jira JSON is a wrong-URL tell",
			reply: probeReply{body: `<html><body>SSO login</body></html>`},
			want:  []string{"not with Jira's JSON", "login page"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &probeSrv{byPath: map[string]probeReply{probeMyselfURLPath: tc.reply}}
			srv := httptest.NewServer(h)
			defer srv.Close()
			res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
			if err == nil {
				t.Fatal("want an error; a failed probe that returns nil certifies a module nobody checked")
			}
			if !strings.Contains(res.Summary, srv.Listener.Addr().String()) {
				t.Errorf("even a failure must say WHERE it went, got %q", res.Summary)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Detail, w) {
					t.Errorf("Detail must contain %q, got %q", w, res.Detail)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(res.Detail, r) {
					t.Errorf("Detail must NOT contain %q (wrong diagnosis), got %q", r, res.Detail)
				}
			}
		})
	}
}

// A site that has been down for a week is one of the three things TEST exists to rule out. It must be an
// error with an unreachable Detail — never a pass, and never a bare "error".
func TestSelfTestReportsAnUnreachableSite(t *testing.T) {
	srv := httptest.NewServer(&probeSrv{byPath: probeOKRoutes()})
	addr := srv.Listener.Addr().String()
	srv.Close() // the port is now closed: the transport class this must classify

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("an unreachable site must FAIL the test")
	}
	if !strings.Contains(res.Detail, "could not be reached") || !strings.Contains(res.Detail, addr) {
		t.Errorf("Detail must say the site is unreachable and name it, got %q", res.Detail)
	}
}

// A credential that authenticates and can browse nothing is a real, silent outage: every issue read 404s,
// which an operator reads as a wrong issue key rather than as a missing permission. Jira answered here,
// so the negative is conclusive and must fail.
func TestSelfTestFailsWhenNoProjectIsBrowsable(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeMyselfURLPath:   {body: probeMyselfJSON},
		probeProjectsURLPath: {body: `{"total":0,"values":[]}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("zero browsable projects must FAIL: the credential is valid and useless")
	}
	if !strings.Contains(res.Detail, "Browse Projects") {
		t.Errorf("Detail must name the permission to grant, got %q", res.Detail)
	}
}

// An UNREADABLE project list is inconclusive, not negative: /myself already proved the site and the
// credential, and /project/search does not even exist on older Jira Server. It passes — but the Detail
// must state which half went unproven, because a pass whose limits are unstated is the dishonest kind.
func TestSelfTestPassesButSaysWhatItCouldNotProve(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{probeMyselfURLPath: {body: probeMyselfJSON}}}
	srv := httptest.NewServer(h) // /project/search is absent -> 404, as on Jira Server 8.3 and older
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("a working credential with no project-search endpoint is not a failed credential: %v", err)
	}
	if !strings.Contains(res.Detail, "did NOT prove") {
		t.Errorf("a partial pass must state its own limits, got %q", res.Detail)
	}
}

// An account Jira has DEACTIVATED still authenticates today and stops without warning later. It is a
// warning on a pass, not a failure — but it must be said, because nothing else in the console would.
func TestSelfTestWarnsAboutAnInactiveAccount(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeMyselfURLPath:   {body: `{"accountId":"a1","displayName":"Old Bot","active":false}`},
		probeProjectsURLPath: {body: probeProjectsJSON},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an inactive account that still authenticates is not a failed probe: %v", err)
	}
	if !strings.Contains(res.Detail, "INACTIVE") {
		t.Errorf("Detail must warn about the deactivated account, got %q", res.Detail)
	}
}

// The console holds an operator on a spinner and moduletest bounds the activity at 30s with ONE attempt,
// so a probe that ignored ctx would hang the dialog rather than fail it.
func TestSelfTestRespectsContext(t *testing.T) {
	srv := httptest.NewServer(&probeSrv{byPath: probeOKRoutes()})
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(ctx, ""); err == nil {
		t.Fatal("a cancelled context must abort the probe")
	}
}

// A base URL that carries userinfo must never render it: a Result is the most-pasted text in an incident.
func TestSiteHostNeverRendersEmbeddedCredentials(t *testing.T) {
	m := New("https://svc:hunter2@acme.atlassian.net", "a@b", config.SecretRef("env:NOPE"))
	if got := m.siteHost(); strings.Contains(got, "hunter2") || strings.Contains(got, "svc") {
		t.Fatalf("siteHost leaked userinfo: %q", got)
	} else if got != "acme.atlassian.net" {
		t.Fatalf("siteHost = %q, want acme.atlassian.net", got)
	}
}

// A probe CUT SHORT is not a probe with an inconclusive answer. /myself succeeds, the operator's context
// is then cancelled — the console's 30-second single attempt expiring looks the same from here — and the
// project search never returns. It must not be reported as the "older Jira has no /project/search" pass:
// nothing was learned, and a green tick would certify a test that never finished.
func TestSelfTestFailsWhenCancelledBetweenItsTwoReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == probeProjectsURLPath {
			cancel()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, probeMyselfJSON)
	}))
	defer srv.Close()

	res, err := newProbeFixture(t, srv.URL).SelfTest(ctx, "")
	if err == nil {
		t.Fatalf("a cancelled probe must FAIL, not pass as inconclusive: %+v", res)
	}
	if !strings.Contains(res.Detail, "not a pass") {
		t.Errorf("Detail must say the test did not finish, got %q", res.Detail)
	}
}
