package awx_test

// ORACLE tests for the AWX credential source's console TEST probe (core/selftest.Tester). CI has no AWX, so
// a httptest.Server fakes /api/v2/me/, the hosts list envelope and one inventory object, and the tests drive
// the REAL Source — the real client, the real Bearer token resolved from its real SecretRef — through it.
// They prove: the Summary reports what the SERVED payload contained (the instance host, the account name and
// the host count); a rejected token and a refused inventory read are classified as DIFFERENT faults; an
// unreachable host is an error and not a pass; a configured inventory that does not exist is named as such;
// and — the killing oracle — a fully-configured source pointed at a server that rejects the token FAILS.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
	"github.com/territory-grounder/grounder/modules/credsource/awx"
)

// probeAWX serves only what the probe reads, so an oracle can refuse ONE of the two requests and prove the
// classifier tells them apart.
type probeAWX struct {
	meStatus    int    // non-zero → /api/v2/me/ is refused with this status
	hostsStatus int    // non-zero → the hosts list is refused with this status
	invStatus   int    // non-zero → the inventory object is refused with this status
	username    string // the account /api/v2/me/ reports
	hostCount   int    // the count the hosts envelope advertises
	invName     string // the name the inventory object reports
	sawHostsQ   url.Values
}

func (f *probeAWX) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			writeJSON(w, 401, map[string]any{"detail": "Authentication credentials were not provided."})
			return
		}
		switch {
		case r.URL.Path == "/api/v2/me/":
			if f.meStatus != 0 {
				writeJSON(w, f.meStatus, map[string]any{"detail": "denied"})
				return
			}
			writeJSON(w, 200, map[string]any{
				"count":   1,
				"results": []map[string]any{{"id": 3, "username": f.username}},
			})
		case r.URL.Path == "/api/v2/hosts/":
			if f.hostsStatus != 0 {
				writeJSON(w, f.hostsStatus, map[string]any{"detail": "You do not have permission to perform this action."})
				return
			}
			f.sawHostsQ = r.URL.Query()
			results := []map[string]any{}
			if f.hostCount > 0 {
				results = append(results, map[string]any{"id": 1, "name": "host-1", "enabled": true})
			}
			writeJSON(w, 200, map[string]any{"count": f.hostCount, "next": "", "results": results})
		case strings.HasPrefix(r.URL.Path, "/api/v2/inventories/"):
			if f.invStatus != 0 {
				writeJSON(w, f.invStatus, map[string]any{"detail": "Not found."})
				return
			}
			writeJSON(w, 200, map[string]any{"id": 7, "name": f.invName})
		default:
			writeJSON(w, 404, map[string]any{"detail": "Not found."})
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// probeSource builds the REAL source against url with every configured value present and non-empty — the
// precondition the killing oracle depends on. tokenValue lets a case supply a token the server will refuse.
func probeSource(t *testing.T, url string, inventoryID int, tokenValue string) *awx.Source {
	t.Helper()
	t.Setenv("TG_PROBE_AWX_TOKEN", tokenValue)
	c, err := awx.New(awx.Config{
		BaseURL:    url,
		TokenRef:   config.SecretRef("env:TG_PROBE_AWX_TOKEN"),
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	s, err := awx.NewSource(awx.SourceConfig{ID: "awx01", Client: c, InventoryID: inventoryID})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	return s
}

func TestSelfTestReportsTheAccountAndTheHostCount(t *testing.T) {
	f := &probeAWX{username: "tg-reader", hostCount: 128}
	srv := f.server(t)
	s := probeSource(t, srv.URL, 4, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	// Every fact in the Summary must come from the SERVED payload or the configured scope — never from a
	// "the settings are filled in" check.
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, want := range []string{host, "tg-reader", "128 hosts", "inventory 4"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
	if res.Detail != "" {
		t.Fatalf("a healthy read must not warn: %q", res.Detail)
	}
	// The probe must be BOUNDED and must respect the configured scope, or it is not testing what syncs.
	if got := f.sawHostsQ.Get("page_size"); got != "1" {
		t.Fatalf("the probe must bound the page at the server, got page_size=%q", got)
	}
	if got := f.sawHostsQ.Get("inventory"); got != "4" {
		t.Fatalf("the probe must scope to the configured inventory, got inventory=%q", got)
	}
	if strings.Contains(res.Summary+res.Detail, goodToken) {
		t.Fatal("the probe leaked the Bearer token into its result")
	}
}

func TestSelfTestFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		fake       *probeAWX
		token      string
		closed     bool
		wantDetail []string
	}{
		{
			// The token itself is refused. /api/v2/me/ needs no object permission, so this can only be the
			// credential — and the Detail must say so rather than sending an operator to edit permissions.
			name:       "rejected token names the credential",
			fake:       &probeAWX{username: "tg-reader", hostCount: 1},
			token:      "a-token-that-was-revoked",
			wantDetail: []string{"REJECTED THE TOKEN"},
		},
		{
			// The token is good and the ACCOUNT is named, but the inventory read is refused: a permission,
			// not a credential. Confusing the two is what makes an operator rotate a working token.
			name:       "refused inventory read names the permission",
			fake:       &probeAWX{username: "tg-reader", hostsStatus: http.StatusForbidden},
			token:      goodToken,
			wantDetail: []string{"may not list inventory hosts", "READ role"},
		},
		{
			name:       "closed server is unreachable and is an error",
			fake:       &probeAWX{},
			token:      goodToken,
			closed:     true,
			wantDetail: []string{"could not be reached"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.fake.server(t)
			addr := srv.URL
			if tc.closed {
				srv.Close() // nothing listens on this port any more
			}
			s := probeSource(t, addr, 0, tc.token)

			res, err := s.SelfTest(context.Background(), "alice")
			if err == nil {
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
			if strings.Contains(res.Summary+res.Detail+err.Error(), tc.token) {
				t.Fatal("the probe leaked the Bearer token into its result or error")
			}
		})
	}
}

func TestRejectedTokenDetailAgreesWithTheTokenCache(t *testing.T) {
	// Client.token() caches the resolved token for the PROCESS LIFETIME, which is exactly why the descriptor
	// marks the token field EffectRestart ("a save here takes effect on the next restart"). A 401 Detail that
	// told the operator this button had just tested the token they saved a moment ago would contradict the
	// dialog next to it and cost them a diagnosis loop: save, press the same red button, conclude the
	// replacement token is broken too. The Detail must name the cache and the restart instead.
	f := &probeAWX{username: "tg-reader", hostCount: 1}
	s := probeSource(t, f.server(t).URL, 4, "a-token-that-was-revoked")

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a refused token must fail: %q", res.Summary)
	}
	for _, want := range []string{"caches", "restart the worker"} {
		if !strings.Contains(res.Detail, want) {
			t.Fatalf("the 401 detail must say the tested token is the cached one and that a save needs a "+
				"restart; %q does not carry %q", res.Detail, want)
		}
	}
}

func TestSelfTestNamesAnInventoryThatDoesNotExist(t *testing.T) {
	// AWX FILTERS on ?inventory=<id>: a wrong id returns an empty page with a 200, exactly like an inventory
	// that is genuinely empty. A probe that reported "0 hosts" and stopped would leave the operator unable to
	// tell a typo from an estate fact, so the empty case spends one more request to separate them.
	f := &probeAWX{username: "tg-reader", hostCount: 0, invStatus: http.StatusNotFound}
	s := probeSource(t, f.server(t).URL, 9, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("an empty inventory is a pass with a warning, not a failure: %v", err)
	}
	if !strings.Contains(res.Detail, "DOES NOT EXIST") {
		t.Fatalf("a missing inventory must be named: %q", res.Detail)
	}
}

func TestSelfTestWarnsOnAnEmptyButRealInventory(t *testing.T) {
	f := &probeAWX{username: "tg-reader", hostCount: 0, invName: "Lab"}
	s := probeSource(t, f.server(t).URL, 7, goodToken)

	res, err := s.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !strings.Contains(res.Detail, "Lab") || !strings.Contains(res.Detail, "no hosts") {
		t.Fatalf("an empty-but-real inventory must be reported as such: %q", res.Detail)
	}
}

// TestSelfTestFailsWithEveryValueConfigured is THE KILLING ORACLE.
//
// Every configured value is present and non-empty: a real base URL, an inventory id, and a token reference
// that RESOLVES to a real value. Only the server disagrees — it rejects the token the way a revoked one, a
// permission never granted, or an instance the token does not belong to does. A SelfTest implemented as "the
// configured values are all set" passes this test; the real one must fail it. This is what makes the probe
// more than a mock.
func TestSelfTestFailsWithEveryValueConfigured(t *testing.T) {
	f := &probeAWX{username: "tg-reader", hostCount: 128}
	s := probeSource(t, f.server(t).URL, 4, "revoked-but-present-token")

	res, err := s.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a fully-configured source whose token is refused MUST fail: %q", res.Summary)
	}
	if res.Detail == "" {
		t.Fatal("a failed probe must carry an actionable Detail")
	}
}

// TestSourceImplementsTester pins the capability the console detects by assertion. Without it the dialog
// would report "no test is implemented" while promising a read.
func TestSourceImplementsTester(t *testing.T) {
	f := &probeAWX{}
	if _, ok := selftest.Of(probeSource(t, f.server(t).URL, 0, goodToken)); !ok {
		t.Fatal("the awx credential source must be detected as a selftest.Tester")
	}
}
