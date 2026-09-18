package openbao_test

// ORACLE tests for the OpenBao module's console TEST probe (core/selftest.Tester). CI has no OpenBao, so a
// httptest.Server fakes the auth login and the KV v2 metadata LIST, and the tests drive the REAL Module —
// the real client, the real SecretRef resolution, the real authed() path — through it. They prove: the
// Summary reports what the SERVED payload contained (path + key count + a bounded name sample); a denied
// LIST and a refused LOGIN are classified differently and name the fault an operator must fix; an
// unreachable address is an error, not a pass; a 404 is the legitimately-empty case vault.Sync also accepts;
// and — the killing oracle — a fully-configured module pointed at a server that denies everything FAILS.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/modules/credsource/openbao"
)

// probeFake is a KV v2 backend for the probe oracles: it answers the auth login, then serves (or refuses)
// the metadata LIST the probe issues.
type probeFake struct {
	loginStatus int      // non-zero → the login is refused with this status (nothing is ever listed)
	listStatus  int      // non-zero → the LIST is refused with this status
	keys        []string // served as .data.keys on a successful LIST
	seenPaths   []string
}

func (f *probeFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(path, "/login") {
			if f.loginStatus != 0 {
				w.WriteHeader(f.loginStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"invalid role or secret id"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-1", "lease_duration": 3600}})
			return
		}
		f.seenPaths = append(f.seenPaths, r.Method+" "+path)
		if r.Header.Get("X-Vault-Token") != "tok-1" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": f.keys}})
	}))
	t.Cleanup(s.Close)
	return s
}

// probeModule builds the REAL module against url with a REAL SecretRef for the token — every configured
// value present and non-empty, which is the precondition the killing oracle depends on.
func probeModule(t *testing.T, url, prefix string) *openbao.Module {
	t.Helper()
	t.Setenv("TG_TEST_TOKEN", "tok-1")
	return probeModuleAuth(t, prefix, openbao.Config{
		BaseURL:    url,
		Auth:       openbao.Token{TokenRef: config.SecretRef("env:TG_TEST_TOKEN")},
		HTTPClient: http.DefaultClient,
	})
}

// probeModuleAppRole is the same module authenticating by AppRole, which — unlike a static token — performs
// a real login ROUND TRIP. It is what lets an oracle distinguish a refused login from a refused read.
func probeModuleAppRole(t *testing.T, url, prefix string) *openbao.Module {
	t.Helper()
	t.Setenv("TG_TEST_ROLE_ID", "role-1")
	t.Setenv("TG_TEST_SECRET_ID", "secret-id-1")
	return probeModuleAuth(t, prefix, openbao.Config{
		BaseURL: url,
		Auth: openbao.AppRole{
			RoleIDRef:   config.SecretRef("env:TG_TEST_ROLE_ID"),
			SecretIDRef: config.SecretRef("env:TG_TEST_SECRET_ID"),
		},
		HTTPClient: http.DefaultClient,
	})
}

func probeModuleAuth(t *testing.T, prefix string, cfg openbao.Config) *openbao.Module {
	t.Helper()
	c, err := openbao.New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	m, err := openbao.NewSource(openbao.SourceConfig{ID: "bao01", Client: c, Mount: "secret", Prefix: prefix})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	return m
}

func TestSelfTestReportsWhatTheListReturned(t *testing.T) {
	f := &probeFake{keys: []string{"hostA", "hostB", "hostC", "hostD", "nested/"}}
	m := probeModule(t, f.server(t).URL, "tg/hosts")

	res, err := m.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	// The observation must come from the SERVED payload, not from configuration: four leaf keys and one
	// sub-path, at the path the source is scoped to.
	for _, want := range []string{"secret/metadata/tg/hosts", "4 keys", "1 sub-path", "hostA"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
	// The sample is bounded: the fourth leaf must not appear, or the line grows with the mount.
	if strings.Contains(res.Summary, "hostD") {
		t.Fatalf("summary is not bounded to %d sample keys: %q", 3, res.Summary)
	}
	if res.Detail != "" {
		t.Fatalf("a healthy list must not warn: %q", res.Detail)
	}
	if len(f.seenPaths) == 0 || !strings.HasPrefix(f.seenPaths[0], "LIST secret/metadata/tg/hosts") {
		t.Fatalf("the probe did not LIST the configured path: %v", f.seenPaths)
	}
}

func TestSelfTestFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		fake       *probeFake
		approle    bool // authenticate by AppRole, so the login is a real round trip
		closed     bool // point the module at a closed port instead
		wantErr    bool
		wantDetail []string // substrings the Detail must carry
	}{
		{
			// The LIST is refused: the credential worked, the POLICY did not. Naming that difference is the
			// whole point — an operator who re-issues the token here fixes nothing.
			name:       "list denied 403 names the policy",
			fake:       &probeFake{listStatus: http.StatusForbidden},
			wantErr:    true,
			wantDetail: []string{"POLICY", "list"},
		},
		{
			// The LOGIN is refused: the credential itself is wrong. A different fault, a different fix.
			name:       "login refused names the credential",
			fake:       &probeFake{loginStatus: http.StatusBadRequest},
			approle:    true,
			wantErr:    true,
			wantDetail: []string{"REFUSED THE LOGIN"},
		},
		{
			name:       "401 in front of OpenBao names the proxy",
			fake:       &probeFake{listStatus: http.StatusUnauthorized},
			wantErr:    true,
			wantDetail: []string{"401", "proxy"},
		},
		{
			// Every node answering 503 (sealed / not-ready) is retried inside the client's standby budget and
			// then gives up. The oracle pins only that it FAILS with something to act on: whether the budget
			// or the console's own deadline expires first depends on timing, and a test that pinned one
			// wording would fail on a slow runner.
			name:    "sealed cluster fails with an actionable detail",
			fake:    &probeFake{listStatus: http.StatusServiceUnavailable},
			wantErr: true,
		},
		{
			name:       "server error is named as server-side",
			fake:       &probeFake{listStatus: http.StatusInternalServerError},
			wantErr:    true,
			wantDetail: []string{"server error"},
		},
		{
			// A closed port: no status at all. It must be an ERROR and it must say unreachable — the third
			// fault TEST exists to rule out.
			name:       "closed server is unreachable and is an error",
			closed:     true,
			wantErr:    true,
			wantDetail: []string{"could not be reached"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := ""
			if tc.closed {
				dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				url = dead.URL
				dead.Close() // nothing listens on this port any more
			} else {
				url = tc.fake.server(t).URL
			}
			m := probeModule(t, url, "tg/hosts")
			if tc.approle {
				m = probeModuleAppRole(t, url, "tg/hosts")
			}

			// A short budget: the 503 arm spends the client's standby retry budget, and the oracle must not
			// wait ~30 seconds to learn what it already knows.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			res, err := m.SelfTest(ctx, "alice")
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got summary=%q detail=%q", res.Summary, res.Detail)
			}
			if res.Detail == "" {
				t.Fatal("a failed probe must carry an actionable Detail, never a bare error")
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Fatalf("detail %q does not carry %q", res.Detail, want)
				}
			}
			// Rule 5: nothing an operator pastes into a ticket may carry credential material.
			pasted := res.Summary + res.Detail
			if err != nil {
				pasted += err.Error()
			}
			if strings.Contains(pasted, "tok-1") {
				t.Fatal("the probe leaked the token into its result or error")
			}
		})
	}
}

func TestSelfTestEmptyPathIsAPassWithAWarning(t *testing.T) {
	// KV v2 answers 404 for a path holding no keys, and Source.Sync treats exactly that as a
	// legitimately-empty source. The probe must agree with the sync it is testing — but must not report an
	// unqualified success, because a wrong mount looks identical on the wire.
	f := &probeFake{listStatus: http.StatusNotFound}
	m := probeModule(t, f.server(t).URL, "tg/hosts")

	res, err := m.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("an empty KV path must not fail the probe: %v", err)
	}
	if !strings.Contains(res.Summary, "no keys") || !strings.Contains(res.Detail, "wrong mount/prefix") {
		t.Fatalf("empty path must warn about the mount/prefix: %q / %q", res.Summary, res.Detail)
	}
}

func TestSelfTestSaysTheSourceIsDisabledWithNoPrefix(t *testing.T) {
	// vault.Sync returns zero entries WITHOUT listing anything when the prefix is empty. A probe that listed
	// the mount root and reported a cheerful key count would certify a source that imports nothing.
	f := &probeFake{keys: []string{"tg/", "hostA"}}
	m := probeModule(t, f.server(t).URL, "")

	res, err := m.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !strings.Contains(res.Detail, "DISABLED") {
		t.Fatalf("an unconfigured prefix must be reported as a disabled source: %q", res.Detail)
	}
}

// TestSelfTestFailsWithEveryValueConfigured is THE KILLING ORACLE.
//
// Every configured value is present and non-empty: a real base URL, a real auth method, a token reference
// that RESOLVES to a real value. Only the server disagrees — it denies the read the way a revoked
// credential, a policy that was never granted, or a host that has been re-pointed does. A SelfTest
// implemented as "the configured values are all set" passes this test; the real one must fail it. This is
// what makes the probe more than a mock.
func TestSelfTestFailsWithEveryValueConfigured(t *testing.T) {
	f := &probeFake{listStatus: http.StatusForbidden}
	m := probeModule(t, f.server(t).URL, "tg/hosts")

	res, err := m.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a fully-configured module whose backend denies the read MUST fail: %q", res.Summary)
	}
	if res.Detail == "" {
		t.Fatal("a failed probe must carry an actionable Detail")
	}
}

// TestModuleImplementsTester pins the capability the console detects by assertion. Without it the dialog
// would report "no test is implemented" while promising a LIST.
func TestModuleImplementsTester(t *testing.T) {
	f := &probeFake{}
	if _, ok := selftest.Of(probeModule(t, f.server(t).URL, "tg/hosts")); !ok {
		t.Fatal("the openbao module must be detected as a selftest.Tester")
	}
}
