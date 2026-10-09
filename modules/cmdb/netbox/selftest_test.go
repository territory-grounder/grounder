package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// selfTestToken is the material the probe must send and must NEVER print. It is deliberately a value a
// substring search can find, so the leak assertions below are real assertions rather than gestures.
const selfTestToken = "nb-probe-token-ffff"

// probeFixture wires the module the way production does — over its own base URL, its own secret reference
// and a REAL *http.Client (New's default) — pointed at an httptest server. The transport is not faked here
// on purpose: the whole claim of this probe is that it exercises the network path, so a test that swapped
// the Doer would verify the one part of the path that is not in question.
func probeFixture(t *testing.T, h http.HandlerFunc) (*Module, *httptest.Server) {
	t.Helper()
	t.Setenv("TG_TEST_NETBOX_PROBE_TOKEN", selfTestToken)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, config.SecretRef("env:TG_TEST_NETBOX_PROBE_TOKEN")), srv
}

// vmListJSON renders the NetBox paginated envelope the probe parses: a total `count` that is independent
// of the page, and a `results` array bounded by the limit the probe sent. Each sampled VM carries a
// cluster, which is what topology.go turns into a placement edge.
func vmListJSON(count int, names ...string) string {
	results := make([]string, 0, len(names))
	for i, n := range names {
		results = append(results, fmt.Sprintf(`{"id":%d,"name":%q,"display":%q,"cluster":{"name":"dc1pve01"}}`, i+1, n, n))
	}
	return fmt.Sprintf(`{"count":%d,"next":null,"results":[%s]}`, count, strings.Join(results, ","))
}

// vmListJSONUnplaced is the same page with NO device and NO cluster: NetBox answers, the token is fine, and
// the estate reader would emit nothing from it.
func vmListJSONUnplaced(count int, names ...string) string {
	results := make([]string, 0, len(names))
	for i, n := range names {
		results = append(results, fmt.Sprintf(`{"id":%d,"name":%q,"display":%q,"device":null,"cluster":null}`, i+1, n, n))
	}
	return fmt.Sprintf(`{"count":%d,"next":null,"results":[%s]}`, count, strings.Join(results, ","))
}

// TestSelfTestAgainstNetBox is the table over the shapes an operator actually hits: a healthy instance, an
// instance that answers but shows nothing, the two credential faults, a URL that is not NetBox, and a
// NetBox that is broken. Each case asserts the DETAIL names the specific fault, because a Detail that says
// "error" sends the operator to re-issue a token that may not be the problem.
func TestSelfTestAgainstNetBox(t *testing.T) {
	cases := []struct {
		name            string
		handler         http.HandlerFunc
		wantErr         bool
		summaryContains []string
		detailContains  string
	}{
		{
			name: "healthy instance reports what it observed",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(vmListJSON(3, "nl-app-01", "nl-db-02", "nl-web-03")))
			},
			// The count and the names come from the SERVED payload, not from configuration: that is what
			// lets a green Test reveal a module pointed at the staging instance.
			summaryContains: []string{"3 virtual machines", "nl-app-01", "nl-web-03"},
		},
		{
			name: "an empty page passes but says so, because it is what a filtered token looks like",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(vmListJSON(0)))
			},
			summaryContains: []string{"0 virtual machines"},
			detailContains:  "object permissions",
		},
		{
			// BOUND, AUTHORISED, AND PRODUCING NOTHING — the failure that hides behind a green light. The
			// read works perfectly; the VMs simply carry no placement, so the estate reader would emit no
			// edges and every blast radius over NetBox placement would come back empty.
			name: "a page whose VMs have no placement passes but names the empty topology",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(vmListJSONUnplaced(2, "nl-app-01", "nl-db-02")))
			},
			summaryContains: []string{"2 virtual machines", "0 of the 2 read carry a placement"},
			detailContains:  "contribute NO placement edges",
		},
		{
			name: "401 names the credential, not a generic failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":"Invalid token."}`))
			},
			wantErr:        true,
			detailContains: "revoked",
		},
		{
			name: "403 names the permission that is missing",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"detail":"You do not have permission to perform this action."}`))
			},
			wantErr:        true,
			detailContains: "VIEW object permission",
		},
		{
			name: "404 says the URL is not a NetBox root",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr:        true,
			detailContains: "no NetBox API at that URL",
		},
		{
			name: "5xx is reported as a NetBox-side fault, not a credential one",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// The body mentions 403 on purpose: classification must read the STATUS LINE, not the
				// vendor's prose, or a broken NetBox is reported as a permission problem the operator
				// then spends an afternoon "fixing".
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`upstream said 403 somewhere in this prose`))
			},
			wantErr:        true,
			detailContains: "server error",
		},
		{
			name: "a 200 that is not NetBox is not a pass",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// An SSO portal or a reverse proxy answering 200 for everything: the probe must not read
				// this as "an instance with no virtual machines".
				_, _ = w.Write([]byte(`{"login":"required"}`))
			},
			wantErr:        true,
			detailContains: "not a NetBox list page",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, srv := probeFixture(t, tc.handler)
			res, err := m.SelfTest(context.Background(), "alice")
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
			// The Summary must always name the instance, or a pass cannot distinguish production from a
			// staging clone — the failure a green Test is most likely to hide.
			if host := strings.TrimPrefix(srv.URL, "http://"); !strings.Contains(res.Summary, host) {
				t.Errorf("Summary must name the instance %q, got %q", host, res.Summary)
			}
			assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
		})
	}
}

// TestSelfTestUsesTheRealCredentialOnTheRealReadOnlyPath pins the three properties the probe's honesty
// rests on: the resolved token really is sent, in NetBox's own scheme, and the request is a bounded GET of
// the list endpoint the descriptor's verb names. A probe that authenticated differently from the module, or
// that read the whole estate, would be a different program from the one under test.
func TestSelfTestUsesTheRealCredentialOnTheRealReadOnlyPath(t *testing.T) {
	var got *http.Request
	var calls int
	m, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		got = r.Clone(r.Context())
		_, _ = w.Write([]byte(vmListJSON(1, "nl-app-01")))
	})
	if _, err := m.SelfTest(context.Background(), "alice"); err != nil {
		t.Fatalf("SelfTest must succeed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the probe must issue exactly one request, got %d", calls)
	}
	if got.Method != http.MethodGet {
		t.Fatalf("the probe must be read-only; it used %s", got.Method)
	}
	if got.URL.Path != "/api/virtualization/virtual-machines/" {
		t.Fatalf("the probe must read the endpoint the verb names, got %s", got.URL.Path)
	}
	if got.URL.Query().Get("limit") == "" {
		t.Fatal("the read must be bounded server-side: no limit was sent, so a large estate would be pulled whole")
	}
	if want := "Token " + selfTestToken; got.Header.Get("Authorization") != want {
		t.Fatalf("the probe must send the token resolved from the secret reference in NetBox's own scheme, got %q",
			got.Header.Get("Authorization"))
	}
}

// TestSelfTestCountsPlacementTheWayTheEstateReaderDoes — the placement tally exists to answer "how many
// edges would this module actually contribute", so it must count by topology.go's rule, not by a looser one.
// topology.go skips an UNNAMED VM before it ever looks at the placement, so a nameless VM with a cluster
// yields no edge; counting it as placed reports a yield that will not arrive, and prints an arithmetic
// nonsense ("3 of the 1 read") on the way.
func TestSelfTestCountsPlacementTheWayTheEstateReaderDoes(t *testing.T) {
	const page = `{"count":3,"next":null,"results":[
		{"id":1,"name":"nl-app-01","cluster":{"name":"dc1pve01"}},
		{"id":2,"name":"","cluster":{"name":"dc1pve01"}},
		{"id":3,"name":"   ","device":{"name":"dc1dell01"}}
	]}`
	m, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	})
	res, err := m.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest must succeed: %v", err)
	}
	// The estate reader emits exactly ONE edge from this page, and only the named VM may be counted.
	if !strings.Contains(res.Summary, "1 of the 1 read carry a placement") {
		t.Errorf("the placement tally must match what topology.go would draft, got %q", res.Summary)
	}
	// Cross-check against the reader itself, so this stays true if either rule is ever changed.
	edges, err := NewEstateSource(m).Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges must succeed: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("fixture drift: the reader must draft exactly 1 edge from this page, got %d", len(edges))
	}
}

// TestSelfTestIsNotAConfigurationCheck IS THE KILLING ORACLE.
//
// Every configured value is present and non-empty here — a real base URL, a secret reference that resolves
// to a real token — and the instance rejects the credential. A SelfTest implemented as "the settings are
// filled in" passes this; the real probe cannot, because it has to hear NetBox say yes. This is the test
// that makes the difference between a probe and a mock wearing a test's name, and it is the reason a
// revoked token cannot be certified green from a settings dialog.
func TestSelfTestIsNotAConfigurationCheck(t *testing.T) {
	m, _ := probeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if m.baseURL == "" || m.tokenRef == "" {
		t.Fatal("the fixture must have COMPLETE configuration, or this oracle proves nothing")
	}
	if tok, err := m.tokenRef.Resolve(); err != nil || tok == "" {
		t.Fatalf("the token reference must resolve to real material: %v", err)
	}
	res, err := m.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a revoked token with perfect configuration must FAIL the test, got a pass: %+v", res)
	}
}

// TestSelfTestFailsWhenNetBoxIsUnreachable — the second half of the killing oracle: a host that has been
// down for a week. The server is closed before the probe runs, so nothing answers on that port; the probe
// must call that a failure with a reachability diagnosis, never a pass.
func TestSelfTestFailsWhenNetBoxIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Setenv("TG_TEST_NETBOX_PROBE_TOKEN", selfTestToken)
	m := New(srv.URL, config.SecretRef("env:TG_TEST_NETBOX_PROBE_TOKEN"))
	srv.Close()

	res, err := m.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("an unreachable NetBox must be an error, got %+v", res)
	}
	if !strings.Contains(res.Detail, "could not be reached") {
		t.Errorf("Detail must say the instance is unreachable, got %q", res.Detail)
	}
	assertNoTokenLeak(t, res.Summary, res.Detail, errText(err))
}

// TestSelfTestReportsTheSecretBackendSeparatelyFromNetBox — a token reference that does not resolve is a
// TG-side fault and must not be reported as a NetBox one. Nothing was sent, so telling the operator to
// re-issue their NetBox token would send them to fix the wrong system.
func TestSelfTestReportsTheSecretBackendSeparatelyFromNetBox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("nothing may be sent when the credential cannot be resolved")
	}))
	t.Cleanup(srv.Close)
	m := New(srv.URL, config.SecretRef("env:TG_TEST_NETBOX_ABSENT_TOKEN"))

	res, err := m.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatal("an unresolvable token reference must fail the test")
	}
	if !strings.Contains(res.Detail, "secret backend") {
		t.Errorf("Detail must name the secret backend as the fault, got %q", res.Detail)
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
