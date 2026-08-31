package librenms

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
)

// THE LIBRENMS TEST BUTTON (core/selftest.Tester).
//
// WHAT THE DIALOG PROMISES. descriptor.go declares Test.Verb "read the LibreNMS device list from every
// configured deployment", and this file is the whole of that sentence: for EVERY row of
// TG_LIBRENMS_DEPLOYMENTS it resolves THAT ROW's own token reference through core/config.SecretRef, issues
// one authenticated GET /api/v0/devices against THAT ROW's own base URL through the module's own injected
// transport (the TLS posture TG_LIBRENMS_INSECURE selected), and reports what came back per row.
//
// WHY THE DEVICE LIST AND NOT SOMETHING CHEAPER. LibreNMS has no "who am I" endpoint, and /api/v0/devices
// is the endpoint this module's live readers already depend on: the estate topology reader (topology.go)
// builds the dependency graph from it, the alert puller joins hostnames from it (alerts.go), and the
// agent's investigation tools resolve devices through it (tools.go). Probing it therefore exercises the
// same URL, the same X-Auth-Token header and the same certificate handling the module uses in anger. A
// probe against an endpoint nothing else reads would prove the wrong thing.
//
// WHY EVERY DEPLOYMENT, ALWAYS, AND WHY ONE BAD ROW IS RED. LibreNMS is configured as a LIST — one row per
// site — and the rows are independent all the way down: separate base URL, separate token reference,
// separate estate. A revoked token in the GR row costs GR every alert while NL stays perfectly green, and
// the only symptom is a site that stopped raising incidents, which is indistinguishable from a quiet
// estate. So the probe never stops at the first failure: it reads every row, names each one, and returns an
// error if ANY row failed. A green here means every configured site answered — not that one did.
//
// WHAT A GREEN RESULT PROVES, AND WHAT IT DOES NOT. It proves that for each row the token reference
// resolved, the base URL was reachable under the configured TLS posture, LibreNMS accepted that credential,
// and the account behind it can read devices. It does NOT prove that the PUSH lane is authenticated (that
// is a different credential — the ingest bearer token, which LibreNMS presents to TG, not TG to LibreNMS),
// that the alert poller is switched on, or that the cascade alert name matches a rule that ever fires.

// probeDeviceLimit bounds the device page the probe transfers. The real readers pull 500 rows (alerts.go)
// or the whole list (topology.go); a probe runs on an operator's spinner with ONE attempt inside a
// 30-second budget, so it asks for a small page and takes the estate SIZE from LibreNMS's own envelope
// `count` rather than by dragging every row across the wire.
const probeDeviceLimit = 25

// probeBodyLimit caps what the probe will read from a response. The endpoint is the same one whose body
// alerts.go refuses to log because a LibreNMS device row carries SNMP community strings; the probe reads a
// bounded prefix, decodes it into apiDevice (which declares only the identity fields, so json.Unmarshal
// drops the secrets), and never quotes the body anywhere.
//
// IT MATCHES THE LIVE READER'S CAP (alerts.go get(), 16 MiB) ON PURPOSE. `?limit=` is NOT part of LibreNMS's
// list_devices contract — the module already sends `?limit=500` there and LibreNMS is free to ignore it and
// answer with the whole estate. A tighter cap here would therefore truncate a HEALTHY large estate's answer
// mid-JSON, and a truncated body decodes exactly like a login page: the probe would tell an operator with a
// perfectly good LibreNMS that they are pointed at a reverse proxy. A probe may report an inconclusive read;
// it may not invent a diagnosis. faultTooLarge below is the backstop for the estate that still exceeds this.
const probeBodyLimit = 16 << 20

// probeSummarySites bounds how many per-site fragments go in the one-line Summary. A fleet with twenty
// LibreNMS rows would otherwise render a Summary no operator can read; the remainder moves to Detail, which
// is the field meant to carry the rest.
const probeSummarySites = 4

// probeDevicePath is the exact read the probe performs. Declared next to the limit so the bound and the
// path cannot drift apart.
var probeDevicePath = "/api/v0/devices?limit=" + strconv.Itoa(probeDeviceLimit)

// SelfTest implements core/selftest.Tester for the topology reader.
//
// IT IS IMPLEMENTED ON BOTH LIVE READERS ON PURPOSE. cmd/worker constructs librenms several times — an
// offline Normalize-only module, an estate source, an alert source and a tool set — and offers each
// construction to the probe registry, which keeps the LAST one that can self-test (see
// cmd/worker/probe_registry.go). The two constructions that hold a real TLS-configured transport are the
// estate source and the alert source, and both read the same device list, so both carry the capability and
// delegate to one implementation. Putting it on only one of them would make the module probeable or not
// depending on which construction the composition root happened to offer last, which is precisely the
// invisible-filter failure the registry exists to remove.
//
// The operator argument is ignored: this probe has no outward side effect, so there is no third-party
// system in which an unattributed event would need explaining.
func (s *EstateSource) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	return probeDeployments(ctx, s.deployments, s.http)
}

// SelfTest implements core/selftest.Tester for the alert puller. Same read, same reason — see the note on
// (*EstateSource).SelfTest for why the capability lives on both readers.
func (s *AlertSource) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	return probeDeployments(ctx, s.deployments, s.http)
}

// compile-time proof both live readers satisfy the optional self-test capability.
var (
	_ selftest.Tester = (*EstateSource)(nil)
	_ selftest.Tester = (*AlertSource)(nil)
)

// probeDeployments performs the real read against every configured deployment and assembles the operator's
// answer. It is READ-ONLY: one HTTP GET per row, no state on the LibreNMS side is created or changed.
func probeDeployments(ctx context.Context, deployments []Deployment, doer Doer) (selftest.Result, error) {
	if len(deployments) == 0 {
		// NOT a pass. With no rows LibreNMS is not a capability in this deployment at all — the front door
		// rejects /v1/ingest/librenms, the estate graph has no topology source and the agent has no device
		// tools — so reporting success here would certify a module that cannot do anything.
		return selftest.Result{
				Summary: "no LibreNMS deployment is configured, so nothing was read",
				Detail: "TG_LIBRENMS_DEPLOYMENTS is empty. Add one row per server as " +
					"site|https://base-url|token-ref[|timezone] (rows separated by ';') and restart the " +
					"worker — the deployment list is read once at boot.",
			},
			errors.New("librenms: no deployments configured")
	}
	if doer == nil {
		// Only reachable if a caller constructed the reader by hand; the constructors default to
		// http.DefaultClient. Falling back keeps the probe a real read rather than a nil-pointer panic on an
		// operator's spinner.
		doer = http.DefaultClient
	}

	var (
		frags []string // one per readable deployment, for the Summary
		warns []string // reads that succeeded but observed something worth saying
		fails []string // one per unreadable deployment, for the Detail
	)
	for _, d := range deployments {
		site := strings.TrimSpace(d.Site)
		if site == "" {
			site = "(unnamed site)"
		}
		// Check the budget BEFORE each row so a deadline reached halfway through a long list reports the
		// remaining rows as unattempted rather than as a fleet of identical timeouts an operator would read
		// as "every site is down".
		if err := ctx.Err(); err != nil {
			fails = append(fails, site+": not attempted — the test's time budget ran out on an earlier deployment")
			continue
		}
		obs, err := probeDeviceList(ctx, d, doer)
		if err != nil {
			fails = append(fails, site+": "+classifyDeploymentFailure(d, err))
			continue
		}
		frags = append(frags, obs.describe(site, hostOf(d.BaseURL)))
		if obs.count == 0 {
			// The read worked, so this is not a failure of the promised action — but a LibreNMS that reports
			// no devices either monitors nothing or is answering to a user who can see nothing, and in both
			// cases every alert from that site arrives without a hostname to enrich it.
			warns = append(warns, site+": the credential works but LibreNMS reports NO devices — either this "+
				"server monitors nothing, or the API token's user cannot see any device. Alerts from this "+
				"site would arrive without a hostname.")
		}
	}

	summary := fmt.Sprintf("read the LibreNMS device list from %d of %d deployment(s)", len(frags), len(deployments))
	if len(frags) > 0 {
		summary += " — " + strings.Join(capFragments(frags), "; ")
	}

	if len(fails) > 0 {
		detail := "could not read " + strconv.Itoa(len(fails)) + " deployment(s) — " + strings.Join(fails, " | ")
		if len(warns) > 0 {
			detail += " || " + strings.Join(warns, " | ")
		}
		return selftest.Result{Summary: summary, Detail: detail},
			fmt.Errorf("librenms: %d of %d deployment(s) could not be read", len(fails), len(deployments))
	}

	res := selftest.Result{Summary: summary}
	var detail []string
	if len(frags) > probeSummarySites {
		// The Summary was truncated; the full per-site list belongs somewhere, and Detail is that somewhere.
		detail = append(detail, "all deployments: "+strings.Join(frags, "; "))
	}
	detail = append(detail, warns...)
	res.Detail = strings.Join(detail, " | ")
	return res, nil
}

// deviceObservation is what one deployment's device list actually told us: how many devices that LibreNMS
// reports, and the name of one of them.
//
// THE WITNESS IS THE POINT. A count alone cannot tell an operator that the row labelled "nl" is being
// answered by the Athens server — and a module pointed at the wrong instance is the failure a green Test is
// most likely to hide. A device hostname is safe to display: it is the same field the module already puts
// in every envelope, and apiDevice deliberately declares no SNMP credential fields.
type deviceObservation struct {
	count   int
	exact   bool   // count came from LibreNMS's own envelope count; otherwise it is what fitted in one page
	witness string // one device hostname, as a human check on WHICH estate answered
}

// describe renders one deployment's observation for the Summary.
func (o deviceObservation) describe(site, host string) string {
	var b strings.Builder
	b.WriteString(site)
	if host != "" {
		b.WriteString(" (" + host + ")")
	}
	switch {
	case o.count == 0:
		b.WriteString(": no devices")
	case o.exact:
		b.WriteString(": " + strconv.Itoa(o.count) + " device(s)")
	default:
		// The server did not state a total, so all we honestly know is what one bounded page held.
		b.WriteString(": at least " + strconv.Itoa(o.count) + " device(s)")
	}
	if o.witness != "" {
		b.WriteString(", e.g. " + o.witness)
	}
	return b.String()
}

// probeDeviceList issues the one authenticated read for a single deployment.
//
// It deliberately does not call AlertSource.get / EstateSource.fetchDevices even though it reads the same
// endpoint with the same header: those return the failure already flattened into a formatted string, and
// classifying an operator-facing diagnosis by re-parsing "status 401" out of our own message text turns a
// harmless wording change into a silently wrong diagnosis. This issues the identical request — same
// transport, same base URL, same X-Auth-Token, same secret resolution — and keeps the STATUS CODE as a
// value so classification switches on the shape of the failure.
func probeDeviceList(ctx context.Context, d Deployment, doer Doer) (deviceObservation, error) {
	token, err := config.SecretRef(d.TokenRef).Resolve()
	if err != nil {
		return deviceObservation{}, &probeFault{stage: faultCredential, err: err}
	}
	base := strings.TrimRight(strings.TrimSpace(d.BaseURL), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+probeDevicePath, nil)
	if err != nil {
		return deviceObservation{}, &probeFault{stage: faultRequest, err: err}
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Accept", "application/json")

	resp, err := doer.Do(req)
	if err != nil {
		return deviceObservation{}, &probeFault{stage: faultTransport, err: err}
	}
	defer resp.Body.Close()
	// One byte MORE than the cap, so a body that hit the cap is DISTINGUISHABLE from one that merely ended
	// there. Without that byte a truncated device list is indistinguishable from malformed JSON, and the
	// operator is told their base URL points at the wrong application when it does not.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deviceObservation{}, &probeFault{stage: faultStatus, status: resp.StatusCode}
	}
	if len(body) > probeBodyLimit {
		return deviceObservation{}, &probeFault{stage: faultTooLarge}
	}

	var envelope struct {
		Status  string      `json:"status"`
		Count   int         `json:"count"`
		Devices []apiDevice `json:"devices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return deviceObservation{}, &probeFault{stage: faultDecode, err: err}
	}
	// LibreNMS wraps every API answer in {"status":"ok"|"error",...} and some builds answer a refused
	// request with 200 + status=error rather than a 4xx. Only an EXPLICIT "error" counts: an envelope that
	// omits the field must not be read as a failure, or a proxy that strips it would fail a healthy server.
	if strings.EqualFold(strings.TrimSpace(envelope.Status), "error") {
		return deviceObservation{}, &probeFault{stage: faultAPIError}
	}

	obs := deviceObservation{count: envelope.Count, exact: envelope.Count > 0}
	if !obs.exact {
		obs.count = len(envelope.Devices)
	}
	for _, dv := range envelope.Devices {
		h := strings.TrimSpace(dv.Hostname)
		if h == "" {
			h = strings.TrimSpace(dv.SysName)
		}
		if h != "" {
			obs.witness = h
			break
		}
	}
	return obs, nil
}

// probeStage names WHERE the read broke, so the diagnosis is chosen from a value rather than from prose.
type probeStage int

const (
	faultCredential probeStage = iota // the token reference did not resolve — a TG-side fault
	faultRequest                      // the base URL could not form a request
	faultTransport                    // DNS / connect / TLS / deadline — nothing was answered
	faultStatus                       // LibreNMS answered with a non-2xx
	faultAPIError                     // 200, but LibreNMS's own envelope says status=error
	faultDecode                       // 200, but the body is not a LibreNMS device list
	faultTooLarge                     // 200, but the answer exceeded what the probe reads in one attempt
)

// probeFault carries the SHAPE of a failure — the stage, and the HTTP status when there was one.
//
// Its Error() is what lands in a ticket when nothing catches it, so it names the endpoint and the status
// and NOTHING else: never the token, never the response body (this endpoint's rows carry SNMP community
// strings), never the raw base URL (which could carry userinfo credentials in a pathological config).
type probeFault struct {
	stage  probeStage
	status int
	err    error
}

func (f *probeFault) Error() string {
	switch f.stage {
	case faultStatus:
		return fmt.Sprintf("librenms: GET %s: status %d", probeDevicePath, f.status)
	case faultAPIError:
		return fmt.Sprintf("librenms: GET %s: LibreNMS reported an API error", probeDevicePath)
	case faultCredential:
		return fmt.Sprintf("librenms: resolve token: %v", f.err)
	case faultDecode:
		return fmt.Sprintf("librenms: GET %s: response is not a LibreNMS device list", probeDevicePath)
	case faultTooLarge:
		return fmt.Sprintf("librenms: GET %s: answer exceeded the probe's %d-byte read", probeDevicePath, probeBodyLimit)
	default:
		// urlFaultReason, not %v: this branch carries the request-build and transport faults, and a *url.Error
		// prints the URL it was given. net/http redacts the password on the transport path but net/url does not
		// on the parse path. probeDeployments happens to aggregate these away today; a per-row error that ever
		// surfaces must be safe to paste on its own.
		return fmt.Sprintf("librenms: GET %s: %s", probeDevicePath, urlFaultReason(f.err))
	}
}

func (f *probeFault) Unwrap() error { return f.err }

// classifyDeploymentFailure turns one deployment's failure into a sentence an operator can act on. It
// classifies on the SHAPE of the fault (stage, status code, transport class) and falls through to the raw
// error rather than inventing a diagnosis it cannot support — an explanation that is confidently wrong
// sends someone to fix the wrong system.
func classifyDeploymentFailure(d Deployment, err error) string {
	var f *probeFault
	if !errors.As(err, &f) {
		return trimErr(err)
	}
	switch f.stage {
	case faultCredential:
		return "the API token could not be READ from its reference " + refLabel(d.TokenRef) + " — the " +
			"reference is wrong, or the secret backend is unreachable. This is a TG-side fault, not a " +
			"LibreNMS one (" + trimErr(f.err) + ")."
	case faultRequest:
		return "the configured base URL is not usable as a URL — it must look like https://nms.example " +
			"(" + urlFaultReason(f.err) + ")."
	case faultTransport:
		return classifyTransport(f.err)
	case faultAPIError:
		return "LibreNMS answered but reported an API error instead of a device list — it does this for an " +
			"unauthenticated or malformed request. Re-check the API token and that the base URL is the " +
			"LibreNMS site root."
	case faultDecode:
		// The body is NOT quoted: a LibreNMS device row carries SNMP community strings and authpass values,
		// and an error string is the most commonly pasted text in an incident.
		return "that address answered, but not with a LibreNMS device list — it is usually a reverse proxy, " +
			"a login page or a different application at that URL. Check the base URL is the LibreNMS site " +
			"root, with no /api and no trailing path."
	case faultTooLarge:
		// This one is NOT a misconfiguration, and saying so matters: the credential worked and the endpoint is
		// the right one — the estate is simply bigger than one bounded read. Diagnosing it as "the wrong
		// application answered" (which a truncated body otherwise looks exactly like) would send an operator to
		// rewrite a base URL that is correct.
		return "this LibreNMS answered with more device data than the test reads in one attempt, so the list " +
			"could not be summarised. The token WORKED and the address is right — this is not a credential or " +
			"base-URL fault. LibreNMS ignored the bounded page the probe asked for; the alert poller, which " +
			"reads the same endpoint on its own schedule, is unaffected."
	case faultStatus:
		switch {
		case f.status == http.StatusUnauthorized:
			return "LibreNMS rejected the API token (401) — it is wrong, expired, or has been revoked. " +
				"Issue a new API token on that server and update " + refLabel(d.TokenRef) + "."
		case f.status == http.StatusForbidden:
			return "the token was accepted but the read was refused (403) — the LibreNMS user behind this " +
				"API token is not permitted to list devices."
		case f.status == http.StatusNotFound:
			return "there is no LibreNMS API at that address (404) — the base URL must be the site root " +
				"(https://nms.example), with no /api and no trailing path."
		case f.status == http.StatusTooManyRequests:
			return "LibreNMS rate-limited the read (429) — the server is up and the token works; retry shortly."
		case f.status >= 500:
			return fmt.Sprintf("LibreNMS is reachable but answered %d — the server itself is unhealthy; this "+
				"is not a TG credential problem.", f.status)
		default:
			return fmt.Sprintf("LibreNMS answered GET %s with an unexpected status %d.", probeDevicePath, f.status)
		}
	default:
		return trimErr(err)
	}
}

// classifyTransport names the transport class of a failure that never reached an HTTP status.
func classifyTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the read did not finish inside the test's time budget — the address may be blackholed, or " +
			"this LibreNMS is answering too slowly to poll."
	}
	var cve *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &cve) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) {
		return "the TLS certificate was refused — this server is not presenting a certificate TG trusts. If " +
			"it serves an internal self-signed certificate, turn on 'Skip TLS verification' for LibreNMS, " +
			"knowing that TG then hands its API token to whatever answers at that address."
	}
	return "the server could not be reached at all — DNS, routing, port or TLS. Check the base URL and that " +
		"the host is up (" + trimErr(err) + ")."
}

// refLabel renders a secret REFERENCE for display.
//
// A reference (env:X, file:/p, bao:…#key) is safe to show — core/config says so explicitly, that being the
// whole point of INV-13. A value with NO scheme prefix is not a reference at all: it may BE a pasted
// plaintext secret, and echoing it into an error an operator pastes into a ticket would publish it.
func refLabel(ref string) string {
	r := strings.TrimSpace(ref)
	if r == "" {
		return "(unset)"
	}
	if scheme, _, ok := strings.Cut(r, ":"); !ok || strings.TrimSpace(scheme) == "" {
		return "(a token reference with no scheme prefix — use env:, file:, store: or a registered backend " +
			"scheme; TG will not accept an inline literal)"
	}
	return r
}

// hostOf renders the host of a configured base URL for the Summary. It returns the HOST ONLY, never the
// whole URL: a base URL may carry userinfo (https://user:pass@host) and that must not reach the console or
// a ticket.
func hostOf(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// capFragments bounds the per-site fragments rendered into the one-line Summary.
func capFragments(frags []string) []string {
	if len(frags) <= probeSummarySites {
		return frags
	}
	out := append([]string(nil), frags[:probeSummarySites]...)
	return append(out, fmt.Sprintf("and %d more (see below)", len(frags)-probeSummarySites))
}

// urlFaultReason renders WHY a URL could not be used, WITHOUT the URL itself.
//
// net/url puts the raw string it failed to parse into *url.Error, and prints it verbatim: a base URL of
// https://user:hunter2@nms.example with a stray control character yields an error containing "hunter2". The
// transport path is safe (net/http replaces the password with *** before wrapping), but the PARSE path is
// not, and this file's whole contract is that nothing an operator pastes into a ticket carries credential
// material. So a *url.Error is rendered by its REASON only; anything else falls through unchanged.
func urlFaultReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return trimErr(ue.Err)
	}
	return trimErr(err)
}

// trimErr bounds an error string before it is shown. A transport error can carry a very long chain, and the
// console renders one line.
func trimErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
