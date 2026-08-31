package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TG-249 item 1, the half that was still open.
//
// The contract generator publishes a security scheme per route from AuthMethod.SchemeName(). The original
// defect was that it hardcoded "tgHMAC" for all 49 routes, including the twelve that 401 a valid HMAC
// credential — and VerifyCoverage could not catch it, because it substring-searched a document the
// generator had just written from that same string.
//
// VerifyCoverage was repaired to require the scheme be DEFINED and be referenced in the route's own
// security block. That closes DANGLING schemes. It does NOT close the original defect: I mutated
// SchemeName() so every session route returns SchemeHMAC — the exact production bug — and the whole
// gencontracts suite stayed GREEN. The repaired verifier still only proves the document agrees with the
// model; nothing proves the MODEL agrees with the server.
//
// This is that missing oracle, and it is behavioural on purpose. A table pairing AuthMethod to scheme
// would be a second copy of the claim, drifting alongside the first. Instead: drive a real HMAC-signed
// request at a route registered with each AuthMethod, and require SchemeName() to agree with what the
// router actually did. The repo already asserts this for ONE route
// (TestMachinePrincipalCannotSatisfyAdminRoute); this generalises it to every auth class.

// machineCredentialAdmitted reports whether a route registered with method admits a VALID machine
// (HMAC) credential — the question "does tgHMAC describe this route?" reduces to.
func machineCredentialAdmitted(t *testing.T, method AuthMethod) bool {
	t.Helper()

	rt := NewRouter(newVerifier())
	const pattern = "/v1/schemeprobe"
	rt.Handle(pattern, method, func(w http.ResponseWriter, _ *http.Request, _ Principal) {
		w.WriteHeader(http.StatusOK)
	}, http.MethodPost)

	body := `{}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, pattern, strings.NewReader(body))
	req.Header.Set("X-TG-Source", "prom-nl")
	req.Header.Set("X-TG-Timestamp", ts)
	req.Header.Set("X-TG-Nonce", "nonce-"+ts+"-"+strconv.Itoa(int(method)))
	req.Header.Set("X-TG-Signature", sign([]byte("shhh-secret-key"), ts,
		"nonce-"+ts+"-"+strconv.Itoa(int(method)), body))

	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	// 401 means the credential was rejected or never inspected. Anything else means the machine lane
	// carried the request — including a 403, which is an AUTHENTICATED caller being refused for another
	// reason and therefore still "tgHMAC describes this route".
	return rec.Code != http.StatusUnauthorized
}

// TestSchemeNameMatchesWhatTheRouterActuallyDoes is the oracle the contract never had.
func TestSchemeNameMatchesWhatTheRouterActuallyDoes(t *testing.T) {
	// Every ROUTE-legal AuthMethod. AuthNone is excluded because registering it panics by design.
	methods := []struct {
		m    AuthMethod
		name string
	}{
		{AuthHMAC, "AuthHMAC"},
		{AuthMTLS, "AuthMTLS"},
		{AuthSession, "AuthSession"},
		{AuthOperatorLogin, "AuthOperatorLogin"},
		{AuthReadOnly, "AuthReadOnly"},
		{AuthIngestPush, "AuthIngestPush"},
		{AuthTraceRead, "AuthTraceRead"},
		{AuthAdminSession, "AuthAdminSession"},
		{AuthAdminElevate, "AuthAdminElevate"},
	}
	if len(methods) == 0 {
		t.Fatal("VACUITY FLOOR: no auth methods under test")
	}

	var checked int
	for _, tc := range methods {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked at route registration: %v — every route-legal method must be "+
						"registrable, and this list must be updated when one is added or removed", tc.name, r)
				}
			}()
			admitted := machineCredentialAdmitted(t, tc.m)
			declared := tc.m.SchemeName()
			checked++

			switch {
			case admitted && declared != SchemeHMAC:
				t.Errorf("%s ADMITS a valid machine credential but the contract publishes %q. An integrator "+
					"reading the contract would present the wrong credential for a route that would have "+
					"accepted HMAC.", tc.name, declared)
			case !admitted && declared == SchemeHMAC:
				t.Errorf("%s REJECTS a valid machine credential (401) but the contract publishes %q. This is "+
					"TG-249 item 1 exactly: a published contract telling an integrator to sign a request the "+
					"server will reject. A contract that is confidently wrong is worse than none, and the "+
					"drift gate keeps it that way.", tc.name, declared)
			}
		}()
	}
	if checked == 0 {
		t.Fatal("VACUITY FLOOR: no auth method was actually probed — every assertion above was skipped")
	}
}

// TestTheProbeCanDistinguishAdmissionFromRejection is the negative control. If machineCredentialAdmitted
// returned a constant, the test above would pass for every possible SchemeName mapping and prove nothing.
func TestTheProbeCanDistinguishAdmissionFromRejection(t *testing.T) {
	admits := machineCredentialAdmitted(t, AuthHMAC)
	rejects := machineCredentialAdmitted(t, AuthAdminSession)
	if !admits {
		t.Error("AuthHMAC did not admit a valid HMAC credential — the probe cannot sign a request " +
			"correctly, so every 'rejects' result below is meaningless")
	}
	if rejects {
		t.Error("AuthAdminSession admitted an HMAC credential — either the admin tier regressed, or the " +
			"probe is not really exercising the auth path")
	}
	if admits == rejects {
		t.Fatalf("VACUOUS PROBE: machineCredentialAdmitted returned %v for BOTH an HMAC route and an admin "+
			"route. It is not measuring anything and the oracle above is worthless.", admits)
	}
}

// TestEverySchemeNameIsAPublishedScheme catches the third failure mode: a scheme that no securityScheme
// block defines. VerifyCoverage checks this against the generated document; here it is checked against
// the constants, so a new AuthMethod returning an unpublished name fails in the auth package itself
// rather than in a downstream generator run nobody reads.
func TestEverySchemeNameIsAPublishedScheme(t *testing.T) {
	published := map[string]bool{SchemeHMAC: true, SchemeSession: true, SchemeOperatorLogin: true, SchemeMTLS: true}
	seen := map[string]bool{}
	for m := AuthMethod(0); m < AuthMethod(32); m++ {
		n := m.SchemeName()
		seen[n] = true
		if !published[n] {
			t.Errorf("AuthMethod(%d).SchemeName() = %q, which is not one of the published security schemes "+
				"%v — the contract's security refs would dangle", int(m), n, keysOf(published))
		}
	}
	if len(seen) < 2 {
		t.Fatalf("VACUITY FLOOR: SchemeName() returned only %d distinct value(s) across every method. If it "+
			"returns a constant, the oracle above cannot fail.", len(seen))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
