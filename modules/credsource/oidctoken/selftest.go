package oidctoken

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof the module can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the module would silently degrade to "no
// test is implemented" — honest, but a dialog that promises a mint and performs none.
var _ selftest.Tester = (*Minter)(nil)

// SelfTest performs ONE real client-credentials mint against the configured token endpoint and throws the
// token away.
//
// WHY A MINT IS THE ONLY HONEST PROBE HERE, and why it is acceptable. This module has exactly one behaviour:
// POST grant_type=client_credentials and hand back an access_token. There is nothing else to read. A
// discovery document (/.well-known/openid-configuration) would prove the host is up while carrying NO
// credential — it passes with a rotated client_secret, with a client the provider has disabled, and with the
// wrong auth style, which are the three faults this button exists to rule out. The mint is not estate-
// mutating: it creates no object, changes no state, and the token is discarded unused. It is not invisible
// either — the provider records the issuance in its own audit log and may rate-limit it — which is why the
// descriptor's verb says so in the operator's own words BEFORE they press it. That disclosure is the reason
// this is allowed to be the probe.
//
// WHY IT DELIBERATELY BYPASSES THE CACHE, AND WHY IT DOES NOT FILL IT. Mint() returns an unexpired cached
// token without touching the network; a probe built on it would report success from a token minted hours
// ago, which is a mock wearing a test's name — it would pass against a secret revoked five minutes earlier.
// So the probe calls the mint path directly. It equally does NOT store what it minted: the verb promises the
// token is discarded, and a probe that seeded the cache would change the process's live credential as a side
// effect of an operator pressing a button in a settings dialog.
//
// THE TOKEN NEVER LEAVES THIS FUNCTION. It is assigned to the blank identifier at the call site — not
// logged, not cached, not measured, not returned. Only the ADVERTISED LIFETIME and the endpoint are
// reported, because Result is rendered in a dialog and pasted into tickets.
//
// WHAT A GREEN RESULT PROVES: the token endpoint was reachable over verified TLS, the client_id and
// client_secret resolved from their references, the provider accepted them in the configured auth style, and
// it issued a token for the configured scope. WHAT IT DOES NOT PROVE: that any TARGET accepts that token —
// a wrong audience mints happily and is rejected downstream, which no mint can detect.
//
// The lock is held across the request because Mint() holds it across its own, so mints stay serialised
// exactly as they already were; the probe introduces no new concurrency shape.
//
// operator is ignored: the provider's audit entry names the OAuth2 CLIENT, which is not something this probe
// can attribute to a person, and inventing an attribution the IdP does not record would be worse than none.
func (m *Minter) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil {
		return selftest.Result{
				Summary: "no OIDC minter is wired",
				Detail:  "the module resolved to nothing — no request was made. This is a TG wiring fault.",
			},
			fmt.Errorf("oidctoken: selftest: nil minter")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// The blank identifier IS the discard the verb promises. Do not bind this to a name.
	_, ttl, err := m.mintLocked(ctx, m.scope)
	if err != nil {
		return selftest.Result{
			Summary: "could not mint a token from " + redactURL(m.tokenURL),
			Detail:  classifySelfTestFailure(err),
		}, err
	}

	scope := "the client's default scope"
	if s := strings.TrimSpace(m.scope); s != "" {
		scope = "scope " + strconv.Quote(s)
	}
	summary := fmt.Sprintf("minted a client-credentials token from %s (%s) and discarded it",
		redactURL(m.tokenURL), scope)
	if ttl > 0 {
		summary += fmt.Sprintf("; the provider advertised a %s lifetime", ttl)
	}

	detail := ""
	if ttl <= 0 {
		// Not a failure — the grant worked — but it changes how the module behaves at runtime, and an
		// operator cannot see it anywhere else.
		detail = "the provider returned NO expires_in, so TG cannot cache the token: every oidc: reference " +
			"will mint a fresh one at use time. The grant works, but the provider's audit log and any mint " +
			"rate limit will see one request per resolve rather than one per lifetime."
	}
	return selftest.Result{Summary: summary, Detail: detail}, nil
}

// classifySelfTestFailure turns a failed mint into something an operator can act on. "error" tells them
// nothing; "the provider rejected the client credentials — or the auth style is the wrong one" tells them
// the two things to check.
//
// It classifies on the SHAPE of the failure — the HTTP status first, then the transport class — rather than
// on the provider's prose. The RFC 6749 §5.2 error CODE would be tempting to switch on, but providers differ
// on which code they use for the same fault (authentik and Keycloak disagree about invalid_client vs
// invalid_request for a wrong auth style), so the status plus a named ambiguity is the honest answer.
// Anything it cannot place falls through to the raw error rather than to an invented diagnosis.
func classifySelfTestFailure(err error) string {
	switch code := statusFromOIDCError(err); {
	case code == 400 || code == 401:
		return "the provider REFUSED THE GRANT. Either the client_secret is wrong, expired or has been " +
			"rotated at the provider, or the client authentication STYLE is the wrong one — a client " +
			"configured for client_secret_basic answers exactly like this when TG sends client_secret_post, " +
			"and the two are indistinguishable from the outside. Check the secret first, then the auth style. " +
			"(The secret is re-read on every mint, so saving a new one takes effect on the next mint.)"
	case code == 403:
		return "the credentials were accepted but the provider will not issue this token — usually a client " +
			"that is disabled, not permitted the client-credentials grant, or refused the requested scope. " +
			"Check the client's grant types and scope mapping at the provider."
	case code == 404:
		return "there is no token endpoint at that URL. It must be the provider's TOKEN endpoint itself " +
			"(e.g. .../protocol/openid-connect/token or .../application/o/token/), not the issuer root and " +
			"not the discovery document."
	case code == 405:
		return "the URL answered but refuses a POST, so it is not an OAuth2 token endpoint — this is usually " +
			"the issuer URL or a login page rather than the token path."
	case code == 429:
		return "the provider is RATE-LIMITING token requests from this client. The mint was refused rather " +
			"than failed — wait and retry, and check what else is minting with this client."
	case code >= 500:
		return fmt.Sprintf("the provider answered with a server error (status %d). The URL and the "+
			"credentials are reaching it, so this is a provider-side fault rather than a TG configuration "+
			"one.", code)
	case code != 0:
		return fmt.Sprintf("the provider refused the mint with status %d.", code)
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve client_id"), strings.Contains(s, "resolve client_secret"),
		strings.Contains(s, "resolve extra field"), strings.Contains(s, "is empty (fail closed)"):
		return "a client credential could not be READ from its reference — the reference is wrong, or the " +
			"secret backend is unreachable. NOTHING was sent to the provider: this is a TG-side problem, not " +
			"an IdP one."
	case strings.Contains(s, "no access_token"):
		return "the endpoint answered 2xx but returned no access_token, so it is not an OAuth2 token endpoint " +
			"— check the URL points at the token path and not at a proxy or a login page that returns 200."
	case strings.Contains(s, "decode token response"):
		return "the endpoint answered 2xx with a body that is not an OAuth2 token response. The URL most " +
			"likely points at something other than the provider's token endpoint."
	case strings.Contains(s, "x509"), strings.Contains(s, "certificate"), strings.Contains(s, "tls"):
		return "the provider's TLS certificate could not be verified, so TG refused to send the client " +
			"secret. Point the CA certificate path at the PEM that issued it (the demo authentik presents a " +
			"self-signed certificate); verification is never skipped."
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"), strings.Contains(s, "no such host"),
		strings.Contains(s, "connection refused"), strings.Contains(s, "connection reset"), strings.Contains(s, "eof"):
		return "the provider could not be reached — check the token endpoint URL resolves, that the host is " +
			"up, and that the worker is allowed to reach it. Every oidc: reference fails closed while this is " +
			"true."
	default:
		return err.Error()
	}
}

// statusFromOIDCError recovers the HTTP status from the error mintLocked formats:
//
//	oidctoken: token endpoint https://idp.example/token returned status 401: invalid_client
//
// It reads the connector's OWN frame — " returned status " — rather than searching the whole error for a
// three-digit number. That distinction is what keeps a provider error body that happens to mention 403 from
// being reported as a policy fault when the real status was 500. A transport failure has no status and
// yields 0, which routes classification to the transport arm instead.
func statusFromOIDCError(err error) int {
	const marker = " returned status "
	s := err.Error()
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	digits := s[i+len(marker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	code, convErr := strconv.Atoi(digits[:end])
	if convErr != nil {
		return 0
	}
	return code
}
