// This file is the dead-man switch's answer to the console's TEST button (core/selftest.Tester).
//
// WHY THIS PROBE DELIBERATELY DOES NOT PING, which is the whole design. The module has exactly one outbound
// call — Ping — and that call RESETS the dead-man timer. A probe that "just pings to prove it works" would,
// pressed during an incident, clear a genuinely missed heartbeat: the alert Healthchecks.io was about to
// raise (or had already raised) would resolve itself because somebody opened a settings dialog. The whole
// point of this module is that a wedged control plane cannot silence its own external watchdog; a Test that
// pings hands that power straight back. The descriptor's verb has always said the check is NOT pinged, and
// this file is the runtime half of that promise.
//
// WHAT IT DOES INSTEAD, and why each half is worth a round trip:
//
//  1. It resolves the check id through the module's OWN SecretRef, the same way ping() resolves it on every
//     heartbeat. That settles a failure class the operator cannot otherwise see: a reference pointing at an
//     env key nobody set, a file: path that is not mounted in the worker, a store:/bao: backend that is
//     down. Today that failure is discoverable only as heartbeats that never left — i.e. as an alert about
//     TG being dead, raised for the wrong reason.
//
//  2. It issues ONE real GET to the ping host, over the module's own Doer, in the same URL SHAPE a heartbeat
//     uses (base + "/" + one segment) — but with a segment that can never address a check. That settles DNS,
//     TLS, the proxy in front of the ping host and the host being up at all.
//
// WHAT A GREEN RESULT DOES NOT PROVE, said here and again in the operator-facing Detail, because a silent
// limit on a test is worse than no test: it does not prove the check id is VALID. The only request that
// would prove that is a ping, and a ping is the mutation above. An operator who wants that proof gets it the
// honest way — from the check's own page in Healthchecks.io, which shows the last ping TG actually made.
package healthchecks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
)

// safeRef renders a secret REFERENCE for operator-facing text.
//
// A reference is safe to display — that is the whole point of INV-13, and naming it is what tells an
// operator WHICH lane to fix. The one value a reference field can hold that is NOT safe is the secret
// itself: an operator who pastes the check uuid into TG_HEALTHCHECKS_CHECK_REF instead of a pointer to it
// has put the credential in a field this probe prints. core/config already refuses to echo that value
// (its error says "no scheme prefix (36 chars)" precisely so the literal never reaches a log), and a probe
// that then printed the raw string into Summary, Detail and an error would undo that guard on the most
// commonly copied text in an incident.
func safeRef(ref config.SecretRef) string {
	switch config.SchemeOf(ref) {
	case "empty":
		return "an empty reference"
	case "literal":
		return fmt.Sprintf("the check-id reference field, which holds a bare %d-character value with no "+
			"env:/file:/store: prefix (that is a pasted secret, not a reference — it is withheld here)",
			len(strings.TrimSpace(string(ref))))
	default:
		return string(ref)
	}
}

// probeSegment replaces the check id in the probe's URL.
//
// THE LEADING DOT IS THE SAFETY PROPERTY. Healthchecks routes a ping as one path segment matching a uuid, a
// ping-key or a slug — none of which may contain a '.'. So no Healthchecks deployment can route this path to
// a check, which means the probe cannot register a heartbeat even if the operator has (wrongly) configured
// the ping host WITH the check id already baked into it. A bare "/" would not have that property: on such a
// misconfiguration the base URL IS the check URL, and GETting it is a ping.
//
// The expected healthy answer is therefore a 404. That is not a failure — it is a ping host correctly saying
// "no such check", which is exactly what it should say about a segment that is not one.
const probeSegment = ".tg-selftest"

// probeBodyLimit caps the response we read. A ping host answers with a few bytes ("OK", "not found"); a
// captive portal or a misrouted proxy can answer with megabytes of HTML, and the console holds an operator
// on a spinner while we read it.
const probeBodyLimit = 4 << 10

// uuidRe is the shape a Healthchecks check id normally has. It is used ONLY to report that the resolved
// value looks like a check id or does not — never to reject one, because self-hosted deployments legitimately
// address checks by ping-key/slug instead.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// compile-time proof this module can honour the TEST button its descriptor advertises. Without it, a rename
// in core/selftest would silently turn SelfTest back into an unreachable method — a capability that exists
// and is never called, which is the defect class this whole exercise is closing.
var _ selftest.Tester = (*Module)(nil)

// SelfTest proves the dead-man switch's configuration WITHOUT resetting its timer.
//
// The operator argument is ignored: this probe leaves nothing behind for anyone to attribute. Attribution
// exists in core/selftest for the notifier, whose probe posts into a room humans watch.
//
// It returns an error when the check id cannot be read from the secret backend, when the ping host cannot be
// reached at all, or when the ping host answers in a way that means TG's heartbeats would not be accepted
// either (an authenticating proxy in front of it, or the service itself failing). Everything else — most
// importantly the expected 404 — is a pass, with the ceiling of the proof stated in Detail.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil || strings.TrimSpace(m.baseURL) == "" {
		// Reachable only from a zero-value Module. Handled anyway: the alternative is a nil-deref inside the
		// activity, which the console renders as an infrastructure fault rather than the "not configured"
		// this is. An empty ping host means the dead-man switch was never registered at all — TG can go
		// silent with nothing outside it watching.
		return selftest.Result{Summary: "no ping host is configured, so there is no dead-man switch to test"},
			errors.New("healthchecks: self-test needs a ping host — TG_HEALTHCHECKS_URL is empty, so the module was never registered and nothing watches TG from outside")
	}
	if m.http == nil {
		// Only reachable through a hand-built Module (New always installs a transport). Reported rather than
		// left to nil-deref inside the activity, and NOT quietly substituted with a default client: a probe
		// that used a transport the heartbeat does not use would be proving the wrong thing.
		return selftest.Result{Summary: "this dead-man switch has no HTTP transport, so nothing can be sent"},
			errors.New("healthchecks: self-test found no HTTP transport on the module — it was not built by New")
	}

	// ── 1. THE CHECK ID, resolved exactly as a heartbeat resolves it ──────────────────────────────────
	// Never store, echo or transmit the value: it IS the credential (whoever holds it can ping the check,
	// and a check pinged by anything but TG makes a dead control plane read as alive). Only its SHAPE and
	// the REFERENCE it came from are reported, and both are safe to paste into a ticket.
	check, err := m.checkRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the dead-man check id could not be read from " + safeRef(m.checkRef) + " — no ping was made",
			Detail: "the check id never resolved, so every heartbeat this worker makes is failing the same way, " +
				"before any request leaves the process. This is a TG-side secret problem, not a Healthchecks one: " +
				"either the reference points somewhere the value is not (check the reference shown in this dialog), " +
				"or the secret backend it points at is unreachable. Underlying fault: " + err.Error(),
		}, fmt.Errorf("healthchecks: self-test could not resolve the check-id reference %s: %w", safeRef(m.checkRef), err)
	}

	var notes []string
	switch {
	case strings.TrimSpace(check) == "":
		// An empty value is worse than an unresolved one: Resolve succeeded, so nothing complains, and every
		// heartbeat goes to base + "/" + "" — a URL that pings nothing while looking configured.
		return selftest.Result{
			Summary: "the reference " + safeRef(m.checkRef) + " resolved, but the stored check id is EMPTY — no ping was made",
			Detail: "an empty check id addresses no check: heartbeats are sent to the ping host's root and " +
				"register nowhere, so the dead-man switch would never fire no matter how dead TG is. Save the " +
				"check's id into this module's secret lane.",
		}, errors.New("healthchecks: self-test found the check-id reference resolves to an empty value")
	case !uuidRe.MatchString(strings.TrimSpace(check)):
		// NOT a failure: self-hosted Healthchecks addresses checks by ping-key/slug, which are not uuids. It
		// is worth saying out loud, because the other thing that is not a uuid is a placeholder somebody
		// pasted ("changeme", a whole URL, a trailing newline).
		notes = append(notes, "the resolved check id is NOT uuid-shaped — legitimate on a self-hosted instance "+
			"addressed by ping-key/slug, but also what a placeholder or an accidentally-pasted full URL looks like")
	}

	// ── 2. THE PING HOST, reached for real, in the shape a heartbeat uses ─────────────────────────────
	target := m.baseURL + "/" + probeSegment
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return selftest.Result{
			Summary: "the configured ping host is not a usable URL — no ping was made",
			Detail:  "TG_HEALTHCHECKS_URL could not be turned into a request: " + err.Error(),
		}, fmt.Errorf("healthchecks: self-test could not build a request for the ping host: %w", err)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return selftest.Result{
			Summary: "the ping host " + m.baseURL + " could not be reached — the dead-man switch is not receiving heartbeats",
			Detail:  classifyProbeTransport(err),
		}, fmt.Errorf("healthchecks: self-test could not reach the ping host: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Read and discard, bounded. We do not interpret the body — Healthchecks' wording is vendor prose and a
	// proxy substitutes its own — but the connection has to be drained to be reusable.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyLimit))

	if detail, failed := classifyProbeStatus(resp.StatusCode); failed {
		return selftest.Result{
			Summary: fmt.Sprintf("the ping host %s answered %d — heartbeats would be rejected the same way", m.baseURL, resp.StatusCode),
			Detail:  joinProbeNotes(append([]string{detail}, notes...)),
		}, fmt.Errorf("healthchecks: self-test got status %d from the ping host", resp.StatusCode)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// A ping host that answers 2xx to a segment that cannot be a check is answering 2xx to everything —
		// a captive portal, an SPA, or a proxy that never forwarded us. Reported, not failed: TG cannot know
		// what an operator has in front of their ping host, but it can say that the answer was suspicious.
		notes = append(notes, fmt.Sprintf("the host answered %d to a path that can never address a check — a real "+
			"Healthchecks ping host answers 404 there, so this may be a portal, an SPA or a proxy answering "+
			"instead of the ping endpoint", resp.StatusCode))
	}

	// THE CEILING OF THE PROOF, stated every time. An operator who reads this green as "the heartbeat works"
	// has been misled by us, not by Healthchecks.
	notes = append(notes, "this proves the ping host is reachable and the check id is readable; it does NOT prove "+
		"the check id is valid — the only request that would prove that IS a heartbeat, and sending one from this "+
		"dialog would reset the dead-man timer and silence a real missed-heartbeat alert. Confirm the id from the "+
		"check's own page, which shows the last ping TG actually made")
	notes = append(notes, "the heartbeat also needs the worker's platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, "+
		"which is off by default: with it unset this module is configured, reachable and silent")

	return selftest.Result{
		Summary: fmt.Sprintf("the ping host %s answered %d to a deliberate non-ping path (a healthy ping endpoint's "+
			"answer there) and the check id resolved from %s — the dead-man check itself was NOT pinged",
			m.baseURL, resp.StatusCode, safeRef(m.checkRef)),
		Detail: joinProbeNotes(notes),
	}, nil
}

// classifyProbeStatus decides whether the ping host's answer means TG's heartbeats would be accepted.
//
// It classifies on the STATUS CODE — never on the body, because the body is vendor prose on a good day and a
// proxy's error page on a bad one. The default is a PASS: a 4xx to a segment that is not a check is the
// correct answer, and treating "not found" as a fault would make this test red on every healthy install.
func classifyProbeStatus(status int) (detail string, failed bool) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Not the check id being wrong — the probe presented no credential at all. Something in front of the
		// ping host demands one, and heartbeats do not carry one either.
		return fmt.Sprintf("the ping host answered %d to an unauthenticated request, so something in front of it "+
			"(a reverse proxy, an SSO gateway, an IP allowlist) is refusing callers. TG's heartbeats carry no "+
			"credential beyond the check id in the URL, so they are being refused too — the dead-man switch is "+
			"receiving nothing and will fire for the wrong reason. Exempt the ping path, or point "+
			"TG_HEALTHCHECKS_URL at an endpoint that is reachable from the worker", status), true
	case status >= 500:
		return fmt.Sprintf("the ping host answered %d — TG reached it, but the service itself is failing. This is "+
			"a Healthchecks-side fault rather than a TG configuration one; heartbeats sent while it lasts do not "+
			"land", status), true
	default:
		return "", false
	}
}

// classifyProbeTransport turns a transport failure into something an operator can act on.
//
// It classifies on the SHAPE of the failure (DNS, refused connection, TLS, deadline) rather than on message
// text, and falls through to the raw error rather than inventing a diagnosis it cannot support — a confident
// wrong diagnosis costs an operator more than none, because they go and fix the thing we named.
func classifyProbeTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the ping host did not answer inside the test's time budget — it is reachable but very slow, or " +
			"something in front of it is holding the connection open without replying"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the ping host's name did not resolve (" + dnsErr.Name + ") — the URL is wrong, or the worker's " +
			"DNS cannot see it. No request ever left this process"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "nothing accepted a connection at the ping host — it is down, the port is wrong, or a firewall " +
			"between the worker and it is dropping the connection"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// The URL is safe to print: the probe's path segment is a constant and the check id is never in it.
		return "the ping host could not be reached at " + urlErr.URL + " — a network, DNS or TLS problem rather " +
			"than a configuration one (" + urlErr.Err.Error() + ")"
	}
	return err.Error()
}

// joinProbeNotes assembles the operator-facing Detail. Kept as one helper so the pass and fail paths cannot
// drift into two different layouts.
func joinProbeNotes(notes []string) string {
	kept := make([]string, 0, len(notes))
	for _, n := range notes {
		if strings.TrimSpace(n) != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, ". ")
}
