package oidctoken_test

// ORACLE tests for the OIDC token minter's console TEST probe (core/selftest.Tester). CI has no IdP, so the
// RFC 6749 §4.4 token endpoint is faked with a httptest.Server and the tests drive the REAL Minter — the
// real client credentials resolved from their real SecretRefs, the real form encoding, the real auth style —
// through it. They prove: the Summary reports what the PROVIDER advertised and never the token; the probe
// bypasses the cache (a cached token would let it pass against a revoked secret) and does not fill it; a
// refused grant, an unreachable host and a 200 that is not a token response are classified as three
// different faults; and — the killing oracle — a fully-configured minter whose secret the provider rejects
// FAILS.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/selftest"
)

func TestSelfTestMintsAndReportsWhatWasIssued(t *testing.T) {
	f := newFakeIDP() // expires_in = 3600
	srv := f.server(t)
	m := minterFor(t, srv.URL, nil)

	res, err := m.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	// The endpoint and the lifetime come from the configuration and the SERVED payload; the scope is what
	// the module actually asked for. Together they let an operator see which IdP answered.
	for _, want := range []string{srv.URL, `scope "openid"`, "1h0m0s", "discarded"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary %q does not report %q", res.Summary, want)
		}
	}
	if res.Detail != "" {
		t.Fatalf("a healthy mint must not warn: %q", res.Detail)
	}
	// Rule 5, and here the probe is holding a live Bearer token when it writes this line.
	if strings.Contains(res.Summary+res.Detail, "tok-") {
		t.Fatalf("the probe leaked the minted token: %q", res.Summary)
	}
	if f.lastGrant != "client_credentials" {
		t.Fatalf("the probe did not use the real grant: %q", f.lastGrant)
	}
	if f.lastScope != "openid" {
		t.Fatalf("the probe did not request the configured scope: %q", f.lastScope)
	}
}

// TestSelfTestBypassesTheCacheAndDoesNotFillIt is the oracle that separates this probe from a mock in the
// one way a status-code test cannot.
//
// Mint() answers from an unexpired cached token WITHOUT touching the network. A probe built on it would
// report success from a token minted hours earlier — passing against a client_secret revoked five minutes
// ago, which is precisely what an operator presses TEST to rule out. And a probe that STORED what it minted
// would change the process's live credential as a side effect of pressing a button in a settings dialog,
// while the verb promises the token is discarded.
func TestSelfTestBypassesTheCacheAndDoesNotFillIt(t *testing.T) {
	f := newFakeIDP()
	m := minterFor(t, f.server(t).URL, nil)

	seeded, err := m.Mint(context.Background(), "") // seeds the cache
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	afterSeed := f.reqCount()

	if _, err := m.SelfTest(context.Background(), "alice"); err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if got := f.reqCount(); got != afterSeed+1 {
		t.Fatalf("the probe answered from the cache: requests went %d → %d, want one more", afterSeed, got)
	}

	// And the cache must be UNTOUCHED. The fake issues an incrementing token per request, so a cache the
	// probe had overwritten would hand the probe's token back here — no extra request either way, which is
	// why this compares the VALUE rather than the request count.
	after, err := m.Mint(context.Background(), "")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if after != seeded {
		t.Fatal("the probe replaced the process's cached credential with the token it was supposed to discard")
	}
	if got := f.reqCount(); got != afterSeed+1 {
		t.Fatalf("the cached token was evicted: requests went to %d", got)
	}
}

func TestSelfTestFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		fake       *fakeIDP
		closed     bool
		wantDetail []string
	}{
		{
			// invalid_client. The Detail must name BOTH candidate causes: providers answer identically for a
			// wrong secret and for the wrong client-authentication style, and pretending to know which is
			// which would send an operator to rotate a secret that was never wrong.
			name:       "refused grant names the secret and the auth style",
			fake:       func() *fakeIDP { f := newFakeIDP(); f.deny = true; return f }(),
			wantDetail: []string{"REFUSED THE GRANT", "auth style"},
		},
		{
			// A 200 with no access_token: the URL is answering, but it is not a token endpoint.
			name:       "a 200 without a token is not a token endpoint",
			fake:       func() *fakeIDP { f := newFakeIDP(); f.noToken = true; return f }(),
			wantDetail: []string{"no access_token"},
		},
		{
			name:       "closed server is unreachable and is an error",
			fake:       newFakeIDP(),
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
			m := minterFor(t, addr, nil)

			res, err := m.SelfTest(context.Background(), "alice")
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
			if strings.Contains(res.Summary+res.Detail+err.Error(), "s3cr3t-value") {
				t.Fatal("the probe leaked the client secret into its result or error")
			}
		})
	}
}

func TestSelfTestNamesAMissingExpiresIn(t *testing.T) {
	// A provider that advertises no lifetime is not a failure, but it changes runtime behaviour: TG cannot
	// cache, so every oidc: reference mints afresh. That is invisible anywhere else in the console.
	f := newFakeIDP()
	f.expiresIn = 0
	m := minterFor(t, f.server(t).URL, nil)

	res, err := m.SelfTest(context.Background(), "alice")
	if err != nil {
		t.Fatalf("SelfTest: %v", err)
	}
	if !strings.Contains(res.Detail, "NO expires_in") {
		t.Fatalf("a missing lifetime must be reported: %q", res.Detail)
	}
}

func TestSelfTestNamesAnEndpointThatRefusesPost(t *testing.T) {
	// The commonest URL mistake: the issuer root or a login page pasted in place of the token path. It
	// answers, so "unreachable" would be wrong; it refuses POST, which names the real fault.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)
	m := minterFor(t, srv.URL, nil)

	res, err := m.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a non-token endpoint must fail: %q", res.Summary)
	}
	if !strings.Contains(res.Detail, "not an OAuth2 token endpoint") {
		t.Fatalf("detail %q does not name the wrong-URL fault", res.Detail)
	}
}

// TestSelfTestFailsWithEveryValueConfigured is THE KILLING ORACLE.
//
// Every configured value is present and non-empty: a real token URL, a scope, and client_id/client_secret
// references that RESOLVE to real values. Only the provider disagrees — it refuses the grant the way a
// rotated secret, a disabled client, or the wrong auth style does. A SelfTest implemented as "the configured
// values are all set" passes this test; the real one must fail it. This is what makes the probe more than a
// mock.
func TestSelfTestFailsWithEveryValueConfigured(t *testing.T) {
	f := newFakeIDP()
	f.deny = true
	m := minterFor(t, f.server(t).URL, nil)

	res, err := m.SelfTest(context.Background(), "alice")
	if err == nil {
		t.Fatalf("a fully-configured minter whose grant is refused MUST fail: %q", res.Summary)
	}
	if res.Detail == "" {
		t.Fatal("a failed probe must carry an actionable Detail")
	}
}

// TestMinterImplementsTester pins the capability the console detects by assertion. Without it the dialog
// would report "no test is implemented" while promising a mint.
func TestMinterImplementsTester(t *testing.T) {
	f := newFakeIDP()
	if _, ok := selftest.Of(minterFor(t, f.server(t).URL, nil)); !ok {
		t.Fatal("the oidc token minter must be detected as a selftest.Tester")
	}
}
