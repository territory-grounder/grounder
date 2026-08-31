package youtrack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// Oracles for the TEST button (core/selftest.Tester).
//
// They drive a real httptest server through the module's real `do` — real URL building, real token
// resolution from a secret reference, real Authorization header — because those are exactly the parts a
// stub cannot judge and a live YouTrack can. The probe's whole claim is "the network path works"; an
// oracle that mocked the network path would be testing nothing.

// probeYT answers the probe's two GETs per path and records every request, so the oracles can assert both
// what came back AND what was sent (verb, path, credential).
type probeYT struct {
	// byPath maps a request path to the status and body it answers with. A path with no entry gets 404,
	// which is what a wrong instance would really do.
	byPath map[string]probeReply
	seen   []probeSeen
}

type probeReply struct {
	status int
	body   string
}

type probeSeen struct{ method, path, auth string }

func (p *probeYT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.seen = append(p.seen, probeSeen{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
	rep, ok := p.byPath[r.URL.Path]
	if !ok {
		http.Error(w, `{"error":"Not Found"}`, http.StatusNotFound)
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
	probeMePath       = "/api/users/me"
	probeProjectsPath = "/api/admin/projects"

	probeMeJSON       = `{"id":"1-7","login":"tg-reader","fullName":"TG Reader","email":"tg@example"}`
	probeProjectsJSON = `[{"id":"0-1","shortName":"IFR","name":"Infrastructure"},
	                      {"id":"0-2","shortName":"TG","name":"Territory Grounder"},
	                      {"id":"0-3","shortName":"OPS","name":"Operations"}]`
)

// newProbeFixture wires the module exactly as production does: a base URL, and a token that is RESOLVED
// FROM ITS REFERENCE at call time (INV-13) rather than injected pre-resolved.
func newProbeFixture(t *testing.T, srvURL string, opts ...Option) *Module {
	t.Helper()
	t.Setenv("TG_TEST_YT_PROBE_TOKEN", "tok-abc123")
	return New(srvURL, config.SecretRef("env:TG_TEST_YT_PROBE_TOKEN"), opts...)
}

func okRoutes() map[string]probeReply {
	return map[string]probeReply{
		probeMePath:       {body: probeMeJSON},
		probeProjectsPath: {body: probeProjectsJSON},
	}
}

// A green TEST must name what it OBSERVED, not merely that it worked: "ok" cannot distinguish a correctly
// configured module from one pointed at a sandbox clone of the same product. Every observation asserted
// here comes from the SERVED payload, so a probe that reported hardcoded prose would fail.
func TestSelfTestReportsTheAccountAndProjectsItObserved(t *testing.T) {
	h := &probeYT{byPath: okRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	m := newProbeFixture(t, srv.URL)

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("SelfTest must pass against a healthy instance: %v", err)
	}
	for _, want := range []string{
		srv.Listener.Addr().String(), // WHICH instance — the wrong-instance tell
		"tg-reader",                  // WHO the token authenticates as, from /api/users/me
		"TG Reader",
		"3 projects", // read scope, counted from the served list
		"IFR",        // recognisable at a glance; a bare count is not
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary must report %q, got %q", want, res.Summary)
		}
	}
	// Credential material must never reach a Result: it is pasted into tickets.
	if strings.Contains(res.Summary+res.Detail, "tok-abc123") {
		t.Fatal("the token leaked into the Result")
	}
	// The probe must have used the REAL credential over the REAL path, or it proved nothing.
	if len(h.seen) != 2 {
		t.Fatalf("want the two documented GETs, got %d requests: %+v", len(h.seen), h.seen)
	}
	for _, got := range h.seen {
		if got.method != http.MethodGet {
			t.Errorf("the probe must be read-only; it issued %s %s", got.method, got.path)
		}
		if got.auth != "Bearer tok-abc123" {
			t.Errorf("request to %s did not carry the resolved token: %q", got.path, got.auth)
		}
	}
}

// The descriptor's verb is the consent contract, and it now names these two endpoints. If the probe stops
// calling them the dialog is lying again, so the paths are pinned here.
func TestSelfTestCallsTheEndpointsTheDescriptorPromises(t *testing.T) {
	h := &probeYT{byPath: okRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), ""); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if h.seen[0].path != probeMePath || h.seen[1].path != probeProjectsPath {
		t.Fatalf("probe called %q then %q, want %q then %q",
			h.seen[0].path, h.seen[1].path, probeMePath, probeProjectsPath)
	}
}

// THE KILLING ORACLE. Every configured value is present and non-empty — base URL, token reference, a
// token that resolves — and the instance rejects the credential. A probe implemented as a
// "configured-values-are-non-empty" check passes this case; this one must fail it.
//
// That is precisely what makes the probe more than a mock: a revoked token, a permission never granted,
// and a host that has been down for a week are the three things TEST exists to rule out, and all three
// have complete, non-empty configuration.
func TestSelfTestFailsWithFullConfigWhenTheInstanceRejectsTheCredential(t *testing.T) {
	h := &probeYT{byPath: map[string]probeReply{
		probeMePath: {status: http.StatusUnauthorized, body: `{"error":"Unauthorized"}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	m := newProbeFixture(t, srv.URL)

	// Config is complete: this is not a "missing value" case.
	if m.baseURL == "" || m.tokenRef == "" {
		t.Fatal("fixture is wrong: the killing oracle requires COMPLETE configuration")
	}
	if tok, err := m.tokenRef.Resolve(); err != nil || tok == "" {
		t.Fatalf("fixture is wrong: the token must resolve to a real value (%q, %v)", tok, err)
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatal("a rejected credential must FAIL the test; a pass here would certify a revoked token")
	}
	if !strings.Contains(res.Detail, "revoked") {
		t.Errorf("Detail must name the credential problem specifically, got %q", res.Detail)
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
			name:  "401 names the credential",
			reply: probeReply{status: http.StatusUnauthorized, body: `{"error":"Unauthorized"}`},
			want:  []string{"token", "revoked"},
		},
		{
			name:  "403 names the permission, not the token",
			reply: probeReply{status: http.StatusForbidden, body: `{"error":"Forbidden"}`},
			want:  []string{"authenticated", "permitted", "read access"},
			// A 403 is NOT a bad token; telling an operator to rotate a working credential sends them
			// away from the actual fix.
			reject: []string{"revoked"},
		},
		{
			name:  "404 points at the base URL, which is the usual cause",
			reply: probeReply{status: http.StatusNotFound, body: `{"error":"Not Found"}`},
			want:  []string{"Instance URL", "/youtrack"},
		},
		{
			name:  "5xx blames the instance, not the configuration",
			reply: probeReply{status: http.StatusBadGateway, body: `<html>bad gateway</html>`},
			want:  []string{"unhealthy", "not a credential problem"},
		},
		{
			name:  "a 200 that is not YouTrack JSON is a wrong-URL tell",
			reply: probeReply{body: `<html><body>please log in</body></html>`},
			want:  []string{"not with YouTrack's JSON", "pointing at"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &probeYT{byPath: map[string]probeReply{probeMePath: tc.reply}}
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

// A host that has been down for a week is one of the three things TEST exists to rule out. It must be an
// error with an unreachable Detail — never a pass, and never a bare "error".
func TestSelfTestReportsAnUnreachableInstance(t *testing.T) {
	srv := httptest.NewServer(&probeYT{byPath: okRoutes()})
	addr := srv.Listener.Addr().String()
	srv.Close() // the port is now closed: the transport class this must classify

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("an unreachable instance must FAIL the test")
	}
	if !strings.Contains(res.Detail, "could not be reached") || !strings.Contains(res.Detail, addr) {
		t.Errorf("Detail must say the instance is unreachable and name it, got %q", res.Detail)
	}
}

// A token that authenticates and can see nothing is a real, silent outage: every issue read 404s and
// every history search returns empty, which downstream is indistinguishable from an estate that has never
// had an incident. YouTrack answered here, so this is conclusive, and a conclusive negative fails.
func TestSelfTestFailsWhenNoProjectAreVisible(t *testing.T) {
	h := &probeYT{byPath: map[string]probeReply{
		probeMePath:       {body: probeMeJSON},
		probeProjectsPath: {body: `[]`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("zero visible projects must FAIL: the credential is valid and useless")
	}
	if !strings.Contains(res.Detail, "permissions problem") {
		t.Errorf("Detail must send the operator at permissions, not connectivity: %q", res.Detail)
	}
}

// An UNREADABLE project list is inconclusive, not negative: /me already proved the URL and the token. It
// passes — but the Detail must state which half went unproven, because a pass whose limits are unstated
// is the dishonest kind.
func TestSelfTestPassesButSaysWhatItCouldNotProve(t *testing.T) {
	h := &probeYT{byPath: map[string]probeReply{
		probeMePath:       {body: probeMeJSON},
		probeProjectsPath: {status: http.StatusForbidden, body: `{"error":"Forbidden"}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an authenticated token with an unreadable project list is not a failed credential: %v", err)
	}
	if !strings.Contains(res.Detail, "did NOT prove") {
		t.Errorf("a partial pass must state its own limits, got %q", res.Detail)
	}
}

// THE POSTURE THE WHOLE MODULE IS BUILT AROUND. This instance is the shared corpus the predecessor
// comparison is measured against, so the probe must behave identically whether or not writes are armed —
// and must never issue anything but a GET, in either posture. A probe that tripped guardWrite would also
// be a probe that could contaminate a running experiment.
func TestSelfTestIsReadOnlyInEveryPosture(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{name: "writes disarmed (the default)", opts: []Option{WithReadOnly()}},
		{name: "writes armed", opts: []Option{WithReadOnlyUnless(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &probeYT{byPath: okRoutes()}
			srv := httptest.NewServer(h)
			defer srv.Close()
			res, err := newProbeFixture(t, srv.URL, tc.opts...).SelfTest(context.Background(), "")
			if err != nil {
				t.Fatalf("the probe must work regardless of the write posture: %v", err)
			}
			if !strings.Contains(res.Summary, "tg-reader") {
				t.Errorf("Summary lost its observation: %q", res.Summary)
			}
			for _, got := range h.seen {
				if got.method != http.MethodGet {
					t.Fatalf("the probe issued %s %s — it must never write to this instance", got.method, got.path)
				}
			}
		})
	}
}

// The console holds an operator on a spinner and moduletest bounds the activity at 30s with ONE attempt,
// so a probe that ignored ctx would hang the dialog rather than fail it.
func TestSelfTestRespectsContext(t *testing.T) {
	srv := httptest.NewServer(&probeYT{byPath: okRoutes()})
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(ctx, ""); err == nil {
		t.Fatal("a cancelled context must abort the probe")
	}
}

// A base URL that carries userinfo must never render it: a Result is the most-pasted text in an incident.
func TestInstanceHostNeverRendersEmbeddedCredentials(t *testing.T) {
	m := New("https://svc:hunter2@yt.example/youtrack", config.SecretRef("env:NOPE"))
	if got := m.instanceHost(); strings.Contains(got, "hunter2") || strings.Contains(got, "svc") {
		t.Fatalf("instanceHost leaked userinfo: %q", got)
	} else if got != "yt.example" {
		t.Fatalf("instanceHost = %q, want yt.example", got)
	}
}

// A probe CUT SHORT is not a probe with an inconclusive answer. /me succeeds, the operator's context is
// then cancelled — the console's 30-second single attempt expiring looks the same from here — and the
// project read never returns. Reporting that as a pass would put a green tick on a test that never
// finished, which is the same dishonest pass this probe refuses everywhere else.
func TestSelfTestFailsWhenCancelledBetweenItsTwoReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == probeProjectsPath {
			cancel()             // the operator navigated away, or the activity deadline expired
			<-r.Context().Done() // and the instance never answers
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, probeMeJSON)
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
