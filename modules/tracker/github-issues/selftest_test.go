package githubissues

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
// They drive a real httptest server through the module's real `do`, so the parts a stub cannot judge —
// path construction from owner/repo, the row cap, token resolution from a secret reference, the Bearer
// header and the GitHub Accept type — are the parts under test. The probe's whole claim is that the
// network path works with the real credential; an oracle that faked the network path would assert
// nothing.

// probeSrv answers per PATH and records every request.
type probeSrv struct {
	byPath map[string]probeReply
	seen   []probeSeen
}

type probeReply struct {
	status int
	body   string
}

type probeSeen struct{ method, path, query, auth, accept string }

func (p *probeSrv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.seen = append(p.seen, probeSeen{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"),
	})
	rep, ok := p.byPath[r.URL.Path]
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
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
	probeOwner = "example"
	probeRepoN = "incidents"

	probeRepoPath   = "/repos/" + probeOwner + "/" + probeRepoN
	probeIssuesPath = probeRepoPath + "/issues"
	probeUserPath   = "/user"

	probeRepoJSON = `{"full_name":"` + probeOwner + `/` + probeRepoN + `","private":true,"archived":false,
	                  "has_issues":true,"open_issues_count":12}`
	probeIssuesJSON = `[{"number":1347,"title":"Login returns 500 on SSO callback","state":"open"}]`
	probeUserJSON   = `{"login":"tg-reader","id":42}`
)

const probeTokenEnv = "TG_TEST_GH_PROBE_TOKEN"

// newProbeFixture wires the module exactly as production does: API base URL, owner, repo, and a token
// RESOLVED FROM ITS REFERENCE at call time (INV-13) rather than injected pre-resolved.
func newProbeFixture(t *testing.T, srvURL string) *Module {
	t.Helper()
	t.Setenv(probeTokenEnv, "ghp-abc123")
	return New(srvURL, probeOwner, probeRepoN, config.SecretRef("env:"+probeTokenEnv))
}

func probeOKRoutes() map[string]probeReply {
	return map[string]probeReply{
		probeRepoPath:   {body: probeRepoJSON},
		probeIssuesPath: {body: probeIssuesJSON},
		probeUserPath:   {body: probeUserJSON},
	}
}

// A green TEST must name what it OBSERVED, not merely that it worked: "ok" cannot distinguish the
// incident repository from another repository that merely exists and that this token can also reach —
// which the descriptor calls out as the dangerous typo, because it does not fail at all. Every
// observation asserted here comes from the SERVED payload.
func TestSelfTestReportsTheRepositoryAndIssueItObserved(t *testing.T) {
	h := &probeSrv{byPath: probeOKRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "alice@example")
	if err != nil {
		t.Fatalf("SelfTest must pass against a healthy repository: %v", err)
	}
	for _, want := range []string{
		srv.Listener.Addr().String(),  // WHICH host — github.com and an Enterprise host share slugs
		"tg-reader",                   // WHO the token belongs to, from /user
		probeOwner + "/" + probeRepoN, // the repository GitHub actually served
		"private",                     // a one-word wrong-repository tell
		"12 open issues",              // from the served repository object
		"#1347",                       // the issue row that proves the issues read is permitted
	} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("Summary must report %q, got %q", want, res.Summary)
		}
	}
	if strings.Contains(res.Summary+res.Detail, "ghp-abc123") {
		t.Fatal("the access token leaked into the Result")
	}
	if len(h.seen) != 3 {
		t.Fatalf("want the three documented GETs, got %d: %+v", len(h.seen), h.seen)
	}
	for _, got := range h.seen {
		if got.method != http.MethodGet {
			t.Errorf("the probe must be read-only; it issued %s %s", got.method, got.path)
		}
		if got.auth != "Bearer ghp-abc123" {
			t.Errorf("request to %s did not carry the resolved token: %q", got.path, got.auth)
		}
		if got.accept != "application/vnd.github+json" {
			t.Errorf("request to %s lost the GitHub Accept type: %q", got.path, got.accept)
		}
	}
	// BOUNDED: a busy repository's default page is 30 full issue bodies, inside a 30-second dialog that
	// moduletest will not retry.
	if !strings.Contains(h.seen[1].query, "per_page=1") {
		t.Errorf("the issue read must cap its rows, got query %q", h.seen[1].query)
	}
	// state=all, or a repository whose incidents are all closed reports "no issues readable" while being
	// perfectly healthy.
	if !strings.Contains(h.seen[1].query, "state=all") {
		t.Errorf("the issue read must include closed issues, got query %q", h.seen[1].query)
	}
}

// The descriptor's verb is the consent contract and it now names these three reads. If the probe stops
// making them the dialog is lying again, so the paths are pinned.
func TestSelfTestCallsTheEndpointsTheDescriptorPromises(t *testing.T) {
	h := &probeSrv{byPath: probeOKRoutes()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), ""); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	want := []string{probeRepoPath, probeIssuesPath, probeUserPath}
	for i, w := range want {
		if h.seen[i].path != w {
			t.Errorf("request %d went to %q, want %q", i, h.seen[i].path, w)
		}
	}
}

// THE KILLING ORACLE. Every configured value is present and non-empty — API base URL, owner, repo, token
// reference, and a token that resolves — and GitHub rejects the credential. A probe implemented as a
// "configured-values-are-non-empty" check passes this case; this one must fail it.
//
// That is what makes the probe more than a mock: a revoked token, a permission never granted, and a host
// unreachable for a week all have complete, non-empty configuration.
func TestSelfTestFailsWithFullConfigWhenGitHubRejectsTheToken(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeRepoPath: {status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	m := newProbeFixture(t, srv.URL)

	// Config is complete: this is not a "missing value" case.
	if m.baseURL == "" || m.owner == "" || m.repo == "" || m.tokenRef == "" {
		t.Fatal("fixture is wrong: the killing oracle requires COMPLETE configuration")
	}
	if tok, err := m.tokenRef.Resolve(); err != nil || tok == "" {
		t.Fatalf("fixture is wrong: the token must resolve to a real value (%q, %v)", tok, err)
	}

	res, err := m.SelfTest(context.Background(), "alice@example")
	if err == nil {
		t.Fatal("a rejected token must FAIL the test; a pass here would certify a revoked credential")
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
			name:  "401 names the token",
			reply: probeReply{status: http.StatusUnauthorized, body: `{"message":"Bad credentials"}`},
			want:  []string{"rejected the token", "revoked"},
		},
		{
			name:  "403 names permissions and org policy, not a bad token",
			reply: probeReply{status: http.StatusForbidden, body: `{"message":"Resource not accessible"}`},
			want:  []string{"authenticated", "permissions", "SSO"},
			reject: []string{
				// Telling an operator to rotate a working token sends them away from the actual fix.
				"revoked",
			},
		},
		{
			// The single most misleading response this API gives: a private repository a token cannot see
			// is 404, not 403, so "the repo does not exist" is the wrong first conclusion.
			name:  "404 explains that GitHub hides private repositories behind not-found",
			reply: probeReply{status: http.StatusNotFound, body: `{"message":"Not Found"}`},
			want:  []string{"PRIVATE", "404 rather than 403", "typo"},
		},
		{
			name:  "5xx blames GitHub, not the configuration",
			reply: probeReply{status: http.StatusBadGateway, body: `<html>bad gateway</html>`},
			want:  []string{"unhealthy", "not a credential problem"},
		},
		{
			name:  "a 200 that is not GitHub JSON points at the /api/v3 suffix",
			reply: probeReply{body: `<html><body>GitHub Enterprise</body></html>`},
			want:  []string{"/api/v3", "not with GitHub's JSON"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &probeSrv{byPath: map[string]probeReply{probeRepoPath: tc.reply}}
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
func TestSelfTestReportsAnUnreachableAPI(t *testing.T) {
	srv := httptest.NewServer(&probeSrv{byPath: probeOKRoutes()})
	addr := srv.Listener.Addr().String()
	srv.Close() // the port is now closed: the transport class this must classify

	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("an unreachable API must FAIL the test")
	}
	if !strings.Contains(res.Detail, "could not be reached") || !strings.Contains(res.Detail, addr) {
		t.Errorf("Detail must say GitHub is unreachable and name the host, got %q", res.Detail)
	}
}

// THE CASE A METADATA-ONLY CHECK WOULD MISS. On a fine-grained token, repository metadata and issues are
// SEPARATE grants: the repository read succeeds and every issue read 403s. A probe that stopped after the
// repository object would report green while the module could not do its one job.
func TestSelfTestFailsWhenTheRepoIsReadableButItsIssuesAreNot(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeRepoPath:   {body: probeRepoJSON},
		probeIssuesPath: {status: http.StatusForbidden, body: `{"message":"Resource not accessible by personal access token"}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("unreadable issues must FAIL: the module exists to read issues")
	}
	if !strings.Contains(res.Detail, "not a wrong repository name") || !strings.Contains(res.Detail, "ISSUES permission") {
		t.Errorf("Detail must send the operator at the issues grant, got %q", res.Detail)
	}
}

// A repository with Issues switched off is a conclusive negative: the repository is readable and there is
// nothing in it for this module to read, comment on, or close.
func TestSelfTestFailsWhenIssuesAreDisabledOnTheRepository(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeRepoPath: {body: `{"full_name":"` + probeOwner + `/` + probeRepoN + `","has_issues":false}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err == nil {
		t.Fatal("a repository with Issues disabled must FAIL: no session can be anchored there")
	}
	if !strings.Contains(res.Detail, "Enable Issues") {
		t.Errorf("Detail must name the fix, got %q", res.Detail)
	}
}

// An Enterprise Server that omits has_issues must NOT be read as "Issues are disabled". Absent means the
// server did not say, and a probe that failed a healthy configuration on a missing field would be worse
// than no probe at all.
func TestSelfTestTreatsAnAbsentHasIssuesAsUnknown(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeRepoPath:   {body: `{"full_name":"` + probeOwner + `/` + probeRepoN + `","open_issues_count":3}`},
		probeIssuesPath: {body: probeIssuesJSON},
		probeUserPath:   {body: probeUserJSON},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if _, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), ""); err != nil {
		t.Fatalf("an absent has_issues must not fail the probe: %v", err)
	}
}

// A repository GitHub serves under a DIFFERENT name was renamed or transferred and the API followed the
// redirect silently. It works today and stops when the redirect is retired, so it is a warning on a pass.
func TestSelfTestFlagsARenamedRepository(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeRepoPath: {body: `{"full_name":"example/incidents-archive","private":true,
		                        "has_issues":true,"open_issues_count":1}`},
		probeIssuesPath: {body: probeIssuesJSON},
		probeUserPath:   {body: probeUserJSON},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("a followed redirect still works today; it is a warning, not a failure: %v", err)
	}
	if !strings.Contains(res.Detail, "renamed or transferred") {
		t.Errorf("Detail must flag the rename, got %q", res.Detail)
	}
	if !strings.Contains(res.Summary, "incidents-archive") {
		t.Errorf("Summary must name the repository GitHub actually served, got %q", res.Summary)
	}
}

// A GitHub App installation token is a VALID credential with no user behind it, and /user 403s for it.
// Losing the account name is worth strictly less than a false red, so the probe passes and says so.
func TestSelfTestPassesWhenTheTokenHasNoUserAccount(t *testing.T) {
	h := &probeSrv{byPath: map[string]probeReply{
		probeRepoPath:   {body: probeRepoJSON},
		probeIssuesPath: {body: probeIssuesJSON},
		probeUserPath:   {status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`},
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := newProbeFixture(t, srv.URL).SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("an installation token with no user must still pass: %v", err)
	}
	if !strings.Contains(res.Detail, "installation token") {
		t.Errorf("Detail must explain the missing account, got %q", res.Detail)
	}
	if !strings.Contains(res.Summary, probeOwner+"/"+probeRepoN) {
		t.Errorf("Summary must still report the repository it observed, got %q", res.Summary)
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
func TestAPIHostNeverRendersEmbeddedCredentials(t *testing.T) {
	m := New("https://svc:hunter2@ghe.example/api/v3", "o", "r", config.SecretRef("env:NOPE"))
	if got := m.apiHost(); strings.Contains(got, "hunter2") || strings.Contains(got, "svc") {
		t.Fatalf("apiHost leaked userinfo: %q", got)
	} else if got != "ghe.example" {
		t.Fatalf("apiHost = %q, want ghe.example", got)
	}
}

// A probe CUT SHORT is not a best-effort miss. The repository and issue reads succeed, the operator's
// context is then cancelled — the console's 30-second single attempt expiring looks the same from here —
// and /user never returns. Passing here would do worse than certify an unfinished test: it would attach
// the "normal for a GitHub App installation token" explanation to a cancellation, which is a confident
// wrong diagnosis of the kind the classification rule exists to forbid.
func TestSelfTestFailsWhenCancelledBeforeTheAccountRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case probeRepoPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, probeRepoJSON)
		case probeIssuesPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, probeIssuesJSON)
		default:
			cancel()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	res, err := newProbeFixture(t, srv.URL).SelfTest(ctx, "")
	if err == nil {
		t.Fatalf("a cancelled probe must FAIL, not pass with an installation-token note: %+v", res)
	}
	if strings.Contains(res.Detail, "installation token") {
		t.Errorf("a cancellation must not be diagnosed as a missing user account, got %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "not a pass") {
		t.Errorf("Detail must say the test did not finish, got %q", res.Detail)
	}
}
