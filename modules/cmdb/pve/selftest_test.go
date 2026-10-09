package pve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// selfTestToken is the FULL `user@realm!tokenid=secret` value PVE expects — and the material the probe must
// send and must never print. It is a value a substring search can find, so the leak assertions below are
// real assertions rather than gestures.
const selfTestToken = "tg-reader@pve!probe=1f2e3d4c-secret"

// clusterResourcesJSON is the shape /api2/json/cluster/resources?type=vm returns: guests with the node they
// are placed on. The nameless and nodeless entries are the ones the reader skips (a missing edge is safer
// than a guessed one), and they are here so the probe's counts are proven to come from the SERVED payload
// rather than from anything configured.
const clusterResourcesJSON = `{"data":[
	{"type":"lxc","node":"dc1pve02","name":"grafana01","status":"running"},
	{"type":"lxc","node":"dc1pve01","name":"n8n01","status":"running"},
	{"type":"qemu","node":"dc1pve01","name":"win-vm","status":"running"},
	{"type":"lxc","node":"","name":"unplaced"},
	{"type":"qemu","node":"dc1pve01","name":""}
]}`

// probeFixture wires the reader the way production does — its own base URL, its own secret reference, and a
// REAL *http.Client (New's default) — pointed at an httptest server. The transport is deliberately NOT
// faked: the claim this probe makes is that it crosses the network with the real credential, so a test that
// swapped the Doer would verify the one part of the path that is not in question.
func probeFixture(t *testing.T, h http.HandlerFunc) (*EstateSource, *httptest.Server) {
	t.Helper()
	t.Setenv("TG_TEST_PVE_PROBE_TOKEN", selfTestToken)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, config.SecretRef("env:TG_TEST_PVE_PROBE_TOKEN")), srv
}

// TestSelfTestAgainstPVE is the table over the shapes an operator actually hits: a healthy cluster, a
// cluster that answers but shows nothing, the two credential faults, a URL that is not the PVE API, and a
// cluster that is broken. Each case asserts the DETAIL names the specific fault, because "error" sends an
// operator to re-issue a token that may not be the problem.
func TestSelfTestAgainstPVE(t *testing.T) {
	cases := []struct {
		name            string
		handler         http.HandlerFunc
		wantErr         bool
		summaryContains []string
		detailContains  string
	}{
		{
			name: "healthy cluster reports the guests and where they run",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(clusterResourcesJSON))
			},
			// Every number here is derived from the payload above — 3 placed guests (2 lxc, 1 qemu) across
			// 2 nodes — which is what lets a green Test reveal a module pointed at the lab cluster.
			summaryContains: []string{
				"3 guests", "2 lxc, 1 qemu", "2 nodes",
				"dc1pve01 (2)", "dc1pve02 (1)",
			},
		},
		{
			name: "an empty cluster passes but says so, because it is what an unprivileged token looks like",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":[]}`))
			},
			summaryContains: []string{"NO guests visible to this token"},
			detailContains:  "PVEAuditor",
		},
		{
			name: "401 points at the token value, not at a generic failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"data":null}`))
			},
			wantErr:        true,
			detailContains: "user@realm!tokenid=secret",
		},
		{
			name: "403 names the role that is missing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			},
			wantErr:        true,
			detailContains: "PVEAuditor role on /",
		},
		{
			name: "404 says the URL is not the PVE API",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr:        true,
			detailContains: "no PVE API at that URL",
		},
		{
			name: "5xx is a cluster-side fault, not a credential one",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// The body mentions 401 on purpose: classification must read the STATUS the transport
				// reported, not the vendor's prose, or a cluster with no quorum is reported as a bad token
				// and the operator rotates a credential that was never broken.
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`proxy detected 401 upstream`))
			},
			wantErr:        true,
			detailContains: "server error",
		},
		{
			name: "a 200 that is not the PVE API is not a pass",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`<html><body>Proxmox Virtual Environment</body></html>`))
			},
			wantErr:        true,
			detailContains: "not a PVE cluster-resources page",
		},
		{
			// THE WRONG-INSTANCE FAILURE A GREEN TEST HIDES. A gateway, an SSO portal or another product
			// answering 2xx with JSON produces no guests — and so does a real cluster whose guests this
			// token may not see. Reporting them identically would tell the operator to grant PVEAuditor on
			// a machine that is not Proxmox, while the base URL (the actual fault, fixable in this very
			// dialog) went unmentioned.
			name: "a JSON 200 with no PVE envelope is not read as an empty cluster",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"error":"unauthorized","login":"required"}`))
			},
			wantErr:        true,
			detailContains: "not a PVE cluster-resources page",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, srv := probeFixture(t, tc.handler)
			res, err := s.SelfTest(context.Background(), "alice")
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v (result %+v)", tc.wantErr, err, res)
			}
			for _, want := range tc.summaryContains {
				if !strings.Contains(res.Summary, want) {
					t.Errorf("Summary must report what was observed; %q missing from %q", want, res.Summary)
				}
			}
			if tc.detailContains != "" && !strings.Contains(res.Detail, tc.detailContains) {
				t.Errorf("Detail must be actionable; %q missing from %q", tc.detailContains, res.Detail)
			}
			// The Summary must always name the endpoint, or a pass cannot tell production from the lab
			// cluster — the failure a green Test is most likely to hide.
			if host := strings.TrimPrefix(srv.URL, "http://"); !strings.Contains(res.Summary, host) {
				t.Errorf("Summary must name the cluster endpoint %q, got %q", host, res.Summary)
			}
			assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
		})
	}
}

// TestSelfTestUsesTheRealCredentialOnTheRealReadPath pins the properties the probe's honesty rests on: the
// resolved token is sent in PVE's own scheme, and the request is the single GET of the cluster-resources
// endpoint the descriptor's verb names. A probe that authenticated differently from the reader, or that hit
// a different endpoint, would be a different program from the one under test.
func TestSelfTestUsesTheRealCredentialOnTheRealReadPath(t *testing.T) {
	var got *http.Request
	var calls int
	s, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		got = r.Clone(r.Context())
		_, _ = w.Write([]byte(clusterResourcesJSON))
	})
	if _, err := s.SelfTest(context.Background(), "alice"); err != nil {
		t.Fatalf("SelfTest must succeed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the probe must issue exactly one request, got %d", calls)
	}
	if got.Method != http.MethodGet {
		t.Fatalf("the probe must be read-only; it used %s", got.Method)
	}
	if got.URL.Path != "/api2/json/cluster/resources" || got.URL.Query().Get("type") != "vm" {
		t.Fatalf("the probe must read the endpoint the verb names, got %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if want := "PVEAPIToken=" + selfTestToken; got.Header.Get("Authorization") != want {
		t.Fatalf("the probe must send the token resolved from the secret reference in PVE's own scheme, got %q",
			got.Header.Get("Authorization"))
	}
}

// TestSelfTestIsNotAConfigurationCheck IS THE KILLING ORACLE.
//
// Every configured value is present and non-empty — a real base URL, a secret reference that resolves to a
// real token — and the cluster rejects the credential. A SelfTest implemented as "the settings are filled
// in" passes this; the real probe cannot, because it has to hear PVE say yes. This is what makes the
// difference between a probe and a mock wearing a test's name, and it is the reason a revoked token cannot
// be certified green from a settings dialog.
func TestSelfTestIsNotAConfigurationCheck(t *testing.T) {
	s, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if s.baseURL == "" || s.tokenRef == "" {
		t.Fatal("the fixture must have COMPLETE configuration, or this oracle proves nothing")
	}
	if tok, err := s.tokenRef.Resolve(); err != nil || tok == "" {
		t.Fatalf("the token reference must resolve to real material: %v", err)
	}
	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a revoked token with perfect configuration must FAIL the test, got a pass: %+v", res)
	}
}

// TestSelfTestFailsWhenTheClusterIsUnreachable — the other half of the killing oracle: a hypervisor that has
// been off for a week. The server is closed before the probe runs, so nothing answers on that port; the
// probe must call that a failure with a reachability diagnosis, never a pass.
func TestSelfTestFailsWhenTheClusterIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Setenv("TG_TEST_PVE_PROBE_TOKEN", selfTestToken)
	s := New(srv.URL, config.SecretRef("env:TG_TEST_PVE_PROBE_TOKEN"))
	srv.Close()

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("an unreachable cluster must be an error, got %+v", res)
	}
	if !strings.Contains(res.Detail, "could not be reached") {
		t.Errorf("Detail must say the cluster is unreachable, got %q", res.Detail)
	}
	assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
}

// TestSelfTestReportsTheSecretBackendSeparatelyFromPVE — a token reference that does not resolve is a
// TG-side fault. Nothing was sent, so telling the operator their PVE token is bad would send them to fix
// the wrong system.
func TestSelfTestReportsTheSecretBackendSeparatelyFromPVE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("nothing may be sent when the credential cannot be resolved")
	}))
	t.Cleanup(srv.Close)
	s := New(srv.URL, config.SecretRef("env:TG_TEST_PVE_ABSENT_TOKEN"))

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatal("an unresolvable token reference must fail the test")
	}
	if !strings.Contains(res.Detail, "secret backend") {
		t.Errorf("Detail must name the secret backend as the fault, got %q", res.Detail)
	}
}

// TestSelfTestBoundsWhatItReportsFromALargeCluster — /cluster/resources answers for the whole cluster and
// has no server-side page size, so the bound has to be on the reporting side. A Summary that grew with the
// estate would be unreadable in the dialog and unusable in a ticket.
func TestSelfTestBoundsWhatItReportsFromALargeCluster(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < 40; i++ { // 40 nodes, one guest each
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"lxc","node":"dc1pve` + string(rune('a'+i%26)) + string(rune('a'+i/26)) +
			`","name":"guest` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `"}`)
	}
	b.WriteString(`]}`)
	body := b.String()

	s, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest must succeed: %v", err)
	}
	if !strings.Contains(res.Summary, "40 guests") {
		t.Errorf("the totals must still be complete, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "more") {
		t.Errorf("the node list must be truncated with a count of the remainder, got %q", res.Summary)
	}
	if n := strings.Count(res.Summary, "dc1pve"); n > selfTestNodeSample {
		t.Errorf("at most %d nodes may be named, %d were: %q", selfTestNodeSample, n, res.Summary)
	}
}

func assertNoTokenLeak(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, selfTestToken) {
			t.Fatalf("credential material leaked into operator-visible text: %q", s)
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
