package slurpit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof this reader can answer the console's TEST button. The capability is OPTIONAL and detected
// by assertion (core/selftest.Of), so without this line the dialog would degrade to "no test is implemented" —
// honest, but it would leave the declared verb unperformable.
var _ selftest.Tester = (*EstateSource)(nil)

// selfTestPath is the read the probe performs: ONE bounded page of the device inventory. It is derived from
// devicesPath so the probe and the estate refresh (fetchDevices) walk the IDENTICAL endpoint with the
// identical token and permission — the probe just stops after five rows. limit bounds the page at the SERVER,
// so the probe cannot pull an inventory of forty thousand devices through a 30-second activity.
const selfTestPath = devicesPath + "?offset=0&limit=5"

// SelfTest reads one bounded page of the device inventory over the REAL path: do() resolves the token from its
// secret reference on this call (INV-13), sends it as Slurp'it's `Bearer <token>` scheme through the module's
// own injected transport, against its own base URL. Nothing here inspects configured values — a check that the
// URL and token reference are non-empty passes against a revoked token, a permission never granted, and a host
// down for a week, which are precisely the faults an operator presses TEST to rule out.
//
// WHAT A GREEN RESULT PROVES: the endpoint is reachable, the token resolved from the backend, Slurp'it accepted
// it, and the account may list devices. WHAT IT DOES NOT PROVE: that this is the RIGHT Slurp'it (the Summary
// names the host and the sample so a human can see that), nor that a device carries the site/parent this source
// turns into edges — so the Summary also reports how many of the sample would contribute one.
//
// operator is ignored: this probe has no outward side effect, so there is no event needing a named author.
func (s *EstateSource) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	body, err := s.do(ctx, selfTestPath)
	if err != nil {
		return selftest.Result{
			Summary: "could not read the device inventory from " + s.instanceLabel(),
			Detail:  classifySelfTestFailure(err),
		}, err
	}

	// Slurp'it's /api/devices returns a bare JSON ARRAY of device objects (the SDK's get_devices → list[Device]
	// shape). A body that is not one — a login page, a reverse-proxy JSON error, an SSO redirect, another
	// product — is NOT an empty inventory, and must never be reported as one: an empty inventory is a permission
	// diagnosis, a wrong base URL is a fix the operator makes in this very dialog. Decoding into a slice
	// discriminates them: an object or HTML fails the unmarshal, an empty array (`[]`) succeeds with zero rows.
	var page []slurpitDevice
	if err := json.Unmarshal(body, &page); err != nil {
		return selftest.Result{
				Summary: "the endpoint at " + s.instanceLabel() + " answered, but not as Slurp'it",
				Detail: "the request succeeded (2xx) yet the body is not a Slurp'it device list. The base URL " +
					"most likely points at a proxy, a login page, or another product rather than at the Slurp'it " +
					"API root — check it is the scheme+host Slurp'it is served on (plain http:// unless you front " +
					"it with TLS), with no /api suffix and no path prefix.",
			}, fmt.Errorf("slurpit: selftest: %s did not return a Slurp'it device list (%d bytes)",
				selfTestPath, len(body))
	}

	// Two tallies, and the second is the one that matters. `names` is the evidence a human recognises; `edged`
	// counts how many of the sampled devices carry a placement the estate reader can turn into an edge, by
	// edgesFrom's OWN rule (a site membership, or a parent that is not dead). A Slurp'it that answers 200 with
	// devices none of which has a site or a usable parent is bound, authorised, reachable — and contributing an
	// empty topology, invisible downstream except a vacuous blast radius nobody notices until an incident.
	names := make([]string, 0, len(page))
	edged := 0
	for _, d := range page {
		name := deviceName(d)
		if name == "" {
			continue
		}
		names = append(names, name)
		if strings.TrimSpace(d.Site) != "" || !isDeadParent(strings.TrimSpace(d.Parent), name) {
			edged++
		}
	}

	// Slurp'it's device list is not an envelope, so there is no server-reported TOTAL — the probe reports the
	// bounded SAMPLE honestly rather than inventing a total it cannot see. A full page (== the limit) means
	// there are more.
	summary := fmt.Sprintf("read Slurp'it at %s: %s in the sample", s.instanceLabel(), plural(len(names), "device"))
	if len(names) > 0 {
		summary += " (" + strings.Join(names, ", ")
		if len(page) >= 5 {
			summary += ", …"
		}
		summary += fmt.Sprintf("); %d of the %d carry a site or parent TG can turn into an edge", edged, len(names))
	}

	// An empty page with a 2xx is a PASS with a warning, not a failure: the credential and endpoint are proven,
	// and an inventory can legitimately be empty. But it is also what a token whose permissions filter every
	// device away looks like, so a module that will contribute nothing must not read as an unqualified success.
	detail := ""
	switch {
	case len(names) == 0:
		detail = "the token was accepted but Slurp'it returned no devices. Either this instance genuinely has " +
			"none, or the token cannot list them — check its scope includes read access to the device inventory."
	case edged == 0:
		detail = "the read works, but none of the sampled devices carries a site or a usable parent, so this " +
			"module would contribute NO edges from them — a blast radius over Slurp'it topology would come back " +
			"empty rather than wrong. Set the site (and, where known, the parent) on your devices in Slurp'it."
	}
	return selftest.Result{Summary: summary, Detail: detail}, nil
}

// instanceLabel renders the configured endpoint for display, and it renders the HOST ONLY. Naming the instance
// is the point of the Summary — "reached Slurp'it" cannot distinguish production from a staging clone, "reached
// Slurp'it at slurpit.example" can. Printing the raw base URL would be wrong: a URL may legally carry userinfo
// (http://user:token@host), and Result is pasted into tickets, so this is the one string that must never carry
// credential material. url.Host drops userinfo by construction; a URL too malformed to parse degrades to a phrase.
func (s *EstateSource) instanceLabel() string {
	if u, err := url.Parse(s.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured Slurp'it URL"
}

// classifySelfTestFailure turns a failed read into something an operator can act on, classifying on the SHAPE
// of the failure — the HTTP status first, then the transport class — never on Slurp'it's prose. Anything it
// cannot place falls through to the raw error rather than to an invented diagnosis.
//
// ★ THE TLS ARM IS THE INVERSE OF NETBOX/PVE. Slurp'it is served over PLAIN HTTP, so a TLS/certificate error
// almost always means the URL was configured as https:// against a cleartext port — the fix is to use http://,
// NOT to install a CA. This is the exact opposite guidance the HTTPS modules give, and getting it backwards
// would send an operator chasing a certificate for a server that serves none.
func classifySelfTestFailure(err error) string {
	switch code := statusFromDoError(err); {
	case code == 401:
		return "the API token was rejected — it is wrong, expired, or revoked. Save a new read-only token and " +
			"test again."
	case code == 403:
		return "the token authenticated but the account behind it may not list devices. Grant it read access to " +
			"the device inventory (read-only — this credential sits on the triage plane and must never be able " +
			"to write Slurp'it)."
	case code == 404:
		return "there is no Slurp'it API at that URL: /api/devices does not exist there. The base URL must be " +
			"the scheme+host Slurp'it is served on (plain http:// unless fronted by TLS), with no /api suffix."
	case code == 429:
		return "Slurp'it is rate-limiting this token. The read was refused rather than failed — retry, and if it " +
			"persists check what else is using this credential."
	case code >= 500:
		return fmt.Sprintf("Slurp'it answered with a server error (status %d). The URL and token are reaching "+
			"it, so this is a Slurp'it-side fault rather than a TG configuration one.", code)
	case code != 0:
		return fmt.Sprintf("Slurp'it refused the read with status %d.", code)
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve token"):
		return "the API token could not be READ from the secret backend — the token reference is wrong, or the " +
			"backend is unreachable. This is a TG-side problem, not a Slurp'it one: nothing was sent."
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate") || strings.Contains(s, "tls"):
		return "a TLS error talking to Slurp'it — which is served over PLAIN HTTP. The base URL was almost " +
			"certainly configured as https:// against a cleartext port. Use http:// (Slurp'it serves no " +
			"certificate); only keep https:// if you have deliberately fronted it with a TLS proxy."
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") ||
		strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") || strings.Contains(s, "eof"):
		return "Slurp'it could not be reached — check the base URL resolves, that the host is up, and that the " +
			"worker is allowed to reach it on that port (80 by default for plain HTTP)."
	default:
		return err.Error()
	}
}

// statusFromDoError recovers the HTTP status from the error do() formats (`slurpit: GET <path>: status 403:
// <body>`). It reads OUR OWN frame — the first ": status " — rather than searching the whole error for a
// three-digit number, so a Slurp'it error body that happens to mention 403 is not misread as a permission
// fault when the real status was 500. A transport failure has no status and yields 0, routing to the transport arm.
func statusFromDoError(err error) int {
	const marker = ": status "
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

// plural renders a count with its noun so the Summary reads as a sentence rather than a log line.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
