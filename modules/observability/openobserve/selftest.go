// This file is the OpenObserve module's answer to the console's TEST button (core/selftest.Tester).
//
// WHY IT IS A READ AND NOT AN INGEST. Every outbound call this module makes is a POST to an ingest route
// (a /{stream}/_json bulk-ingest route). Probing one would put a junk record in the operator's metrics store — an
// unreviewed change to the estate, made because somebody opened a settings dialog, and one that lands in
// the same stream a dashboard queries. Nothing about a write is more convincing here than a read, so a
// write would be a side effect bought for nothing.
//
// WHY THE STREAM LIST AND NOT /healthz. OpenObserve's health route is UNAUTHENTICATED: it answers 200 for
// an instance whose ingest credential was rotated an hour ago, which is one of the three things an operator
// presses TEST to rule out. GET {endpoint}/streams takes the SAME HTTP Basic credential ingest takes and
// answers with the streams that org actually holds. Two consequences make it the right read:
//
//   - The stream names are an OBSERVATION. They are the only thing in this dialog that can distinguish a
//     correctly configured exporter from one authenticated against the wrong OpenObserve, or against the
//     right one under the wrong org — a failure that otherwise shows up as a dashboard that stays empty
//     while every setting looks right.
//
//   - It shares the endpoint's org prefix with the ingest path. OpenObserve's ingest route is
//     {host}/api/{org}/{stream}/_json, so TG_OPENOBSERVE_URL must already carry /api/{org} for post() to work at
//     all; {endpoint}/streams therefore resolves to {host}/api/{org}/streams. A base URL missing that
//     prefix 404s here — and 404s identically on every export, silently, which is exactly the
//     misconfiguration nothing currently catches.
//
// WHAT A GREEN RESULT DOES NOT PROVE, repeated in the operator-facing Detail because a silent limit on a
// test is worse than no test: read access and ingest permission are separate grants in OpenObserve. A
// credential that can list streams may still be refused at ingest, and this probe must not launch a write to
// find out. It also says nothing about whether anything ships: the export loop is gated by the worker's
// platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, which is off by default.
package openobserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
)

// safeRef renders a secret REFERENCE for operator-facing text.
//
// A reference is safe to display — that is the point of INV-13, and naming it is what tells an operator
// WHICH lane to fix. The one value a reference field can hold that is NOT safe is the secret itself: an
// operator who pastes the ingest token into TG_OPENOBSERVE_TOKEN_REF instead of a pointer to it has put
// the credential in a field this probe prints. core/config already refuses to echo that value (its error
// reports only its LENGTH, precisely so the literal never reaches a log), and a probe that then printed
// the raw string into Summary, Detail and an error would undo that guard on the most commonly copied text
// in an incident.
func safeRef(ref config.SecretRef) string {
	switch config.SchemeOf(ref) {
	case "empty":
		return "an empty reference"
	case "literal":
		return fmt.Sprintf("the ingest-token reference field, which holds a bare %d-character value with no "+
			"env:/file:/store: prefix (that is a pasted token, not a reference — it is withheld here)",
			len(strings.TrimSpace(string(ref))))
	default:
		return string(ref)
	}
}

// streamsPath is the authenticated read the probe makes, relative to the configured endpoint.
//
// fetchSchema=false is not a nicety, it is the bound: with schemas the reply carries every field of every
// stream, which on a real estate is megabytes, and moduletest gives this activity 30 seconds with no retry.
// type=logs narrows it further to the stream kind this module writes.
const streamsPath = "/streams?fetchSchema=false&type=logs"

// probeBodyLimit is the second bound, in case a proxy or a wrong host answers with an unbounded stream.
const probeBodyLimit = 1 << 20

// maxNamedStreams caps how many stream names go into the one-line Summary. Enough to recognise the
// instance, not enough to turn Summary into a wall of text.
const maxNamedStreams = 5

// streamsResponse is the shape of GET /api/{org}/streams. Only the fields worth reporting are decoded.
type streamsResponse struct {
	List []struct {
		Name       string `json:"name"`
		StreamType string `json:"stream_type"`
	} `json:"list"`
}

// compile-time proof this module can honour the TEST button its descriptor advertises. Without it a rename
// in core/selftest would silently turn SelfTest back into an unreachable method — a capability that exists
// and is never called, which is the defect class this whole exercise is closing.
var _ selftest.Tester = (*Module)(nil)

// SelfTest authenticates against the configured OpenObserve org with the real ingest credential and lists
// the log streams it can see. It ingests nothing.
//
// The operator argument is ignored: this probe leaves nothing behind for anyone to attribute. Attribution
// exists in core/selftest for the notifier, whose probe posts into a room humans watch.
//
// It returns an error when the token cannot be resolved, when the endpoint cannot be reached, when
// OpenObserve refuses the credential or the read, or when the answer is not an OpenObserve stream list —
// that last one because a 200 from something that is not OpenObserve is exactly the "pointed at the wrong
// thing" case this probe exists to catch.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil || strings.TrimSpace(m.endpoint) == "" {
		// Reachable only from a zero-value Module: with an empty TG_OPENOBSERVE_URL the composition root
		// never registers the exporter. Reported honestly rather than left to nil-deref inside the activity,
		// which the console would render as an infrastructure fault instead of a configuration one.
		return selftest.Result{Summary: "no OpenObserve base URL is configured, so no exporter was registered"},
			errors.New("openobserve: self-test needs a base URL — TG_OPENOBSERVE_URL is empty, so nothing ships and nothing reports an error")
	}
	if m.http == nil {
		// Only reachable through a hand-built Module (New always installs a transport). NOT quietly
		// substituted with a default client: a probe over a transport the exporter does not use would be
		// proving the wrong thing.
		return selftest.Result{Summary: "this OpenObserve module has no HTTP transport, so nothing can be sent"},
			errors.New("openobserve: self-test found no HTTP transport on the module — it was not built by New")
	}

	// ── 1. THE INGEST CREDENTIAL, resolved exactly as post() resolves it on every send ────────────────
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the OpenObserve ingest token could not be read from " + safeRef(m.tokenRef) + " — nothing was sent",
			Detail: "the token never resolved, so every export is failing the same way before a request leaves " +
				"the worker. This is a TG-side secret problem, not an OpenObserve one: either the reference points " +
				"somewhere the value is not, or the secret backend it points at is unreachable. Underlying fault: " +
				err.Error(),
		}, fmt.Errorf("openobserve: self-test could not resolve the ingest-token reference %s: %w", safeRef(m.tokenRef), err)
	}
	if strings.TrimSpace(token) == "" {
		// Worse than an unresolved reference: Resolve succeeded, so nothing complains, and every export goes
		// out with an empty Basic credential that OpenObserve refuses — visible only as a store that stays empty.
		return selftest.Result{
			Summary: "the reference " + safeRef(m.tokenRef) + " resolved, but the stored ingest token is EMPTY — nothing was sent",
			Detail: "an empty credential is refused on every export, so no sample has ever landed. Save the " +
				"OpenObserve ingest token — base64(user:password), NOT a raw API key — into this module's secret lane.",
		}, errors.New("openobserve: self-test found the ingest-token reference resolves to an empty value")
	}

	// ── 2. THE AUTHENTICATED READ ─────────────────────────────────────────────────────────────────────
	target := m.endpoint + streamsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return selftest.Result{
			Summary: "the configured OpenObserve base URL is not a usable URL — nothing was sent",
			Detail:  "TG_OPENOBSERVE_URL could not be turned into a request: " + err.Error(),
		}, fmt.Errorf("openobserve: self-test could not build a request for the stream list: %w", err)
	}
	// The SAME auth construction post() uses: HTTP Basic with the already-base64 ingest token presented
	// verbatim, NOT a Bearer token. A probe that authenticated differently would prove a second, unused code
	// path works — and would in particular pass with a raw API key pasted into the dialog, which is the one
	// credential mistake this module's help text exists to prevent.
	req.Header.Set("Authorization", "Basic "+token)

	resp, err := m.http.Do(req)
	if err != nil {
		return selftest.Result{
			Summary: "OpenObserve at " + m.endpoint + " could not be reached — nothing was sent",
			Detail:  classifyProbeTransport(err),
		}, fmt.Errorf("openobserve: self-test could not reach %s: %w", m.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return selftest.Result{
			Summary: fmt.Sprintf("OpenObserve at %s answered %d to an authenticated read — nothing was sent", m.endpoint, resp.StatusCode),
			Detail:  classifyProbeStatus(resp.StatusCode),
		}, fmt.Errorf("openobserve: self-test got status %d from the stream list", resp.StatusCode)
	}

	// ── 3. THE OBSERVATION ────────────────────────────────────────────────────────────────────────────
	var sr streamsResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		// A 200 that is not a stream list means we are talking to something else — an SSO portal that
		// answered with a login page, a proxy, or the wrong service on the right host. Failing here is the
		// point: it looks like success to anything that only checks the status code.
		return selftest.Result{
			Summary: fmt.Sprintf("%s answered 200 but not with an OpenObserve stream list — nothing was sent", m.endpoint),
			Detail: "the response could not be parsed as OpenObserve's stream payload, so this host is probably " +
				"not the OpenObserve API: check TG_OPENOBSERVE_URL, and check for a portal or proxy answering in " +
				"front of it. Parse fault: " + err.Error(),
		}, fmt.Errorf("openobserve: self-test could not decode the stream list: %w", err)
	}

	var notes []string
	// THE CEILING OF THE PROOF, stated on every pass. An operator who reads this green as "ingest works" has
	// been misled by us, not by OpenObserve.
	notes = append(notes, "this proves the endpoint answers and the ingest credential is ACCEPTED; it does not "+
		"prove ingest is PERMITTED — OpenObserve grants read and write separately, and the probe must not write "+
		"to find out")
	notes = append(notes, "shipping also needs the worker's platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, which "+
		"is off by default: with it unset this module is configured, authenticated and silent")

	if len(sr.List) == 0 {
		// A real pass with a real caveat: the credential and the org are proven, and there is no stream to
		// name — which is exactly what a correctly configured TG looks like before its first export.
		return selftest.Result{
			Summary: fmt.Sprintf("reached OpenObserve at %s and authenticated with the ingest credential; the org holds NO log streams yet — nothing was ingested", m.endpoint),
			Detail: joinProbeNotes(append([]string{"an empty stream list is expected if nothing has ever been " +
				"ingested into this org, which is the normal state while TG_OBSERVABILITY_EXPORT_INTERVAL is " +
				"unset — but it also means this test cannot confirm WHICH org's data you are looking at"}, notes...)),
		}, nil
	}

	named := make([]string, 0, maxNamedStreams)
	for _, s := range sr.List {
		if len(named) == maxNamedStreams {
			break
		}
		name := strings.TrimSpace(s.Name)
		if name == "" {
			name = "(unnamed)"
		}
		named = append(named, name)
	}
	if extra := len(sr.List) - len(named); extra > 0 {
		notes = append(notes, fmt.Sprintf("%d further stream(s) exist and are not listed here", extra))
	}

	return selftest.Result{
		Summary: fmt.Sprintf("reached OpenObserve at %s and authenticated with the ingest credential — %d log stream(s) visible: %s — nothing was ingested",
			m.endpoint, len(sr.List), strings.Join(named, ", ")),
		Detail: joinProbeNotes(notes),
	}, nil
}

// classifyProbeStatus turns OpenObserve's answer into something an operator can act on.
//
// It classifies on the STATUS CODE and never on the body: vendor wording changes between releases, and a
// reverse proxy in front of OpenObserve substitutes its own error page entirely, so a text matcher would
// quietly stop classifying and start guessing.
func classifyProbeStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "OpenObserve rejected the credential (401) — it is wrong, expired, or has been revoked. The " +
			"common shape of this is a RAW API key saved into the token field: OpenObserve expects the " +
			"base64(user:password) credential it issues, presented as HTTP Basic, and a raw key 401s every " +
			"export exactly like this"
	case http.StatusForbidden:
		return "the credential authenticated but OpenObserve refused the read (403) — the account it belongs to " +
			"is not permitted to list this org's streams. An ingest-only service account looks like this, and its " +
			"exports may well be working; this test cannot confirm that, which is why it reports a failure rather " +
			"than a pass. Grant the account read on streams, or verify ingest from the OpenObserve side"
	case http.StatusNotFound:
		return "this host has no stream list at that path (404) — the base URL is almost certainly missing the " +
			"/api/<org> prefix, or names an org that does not exist. That is not only a test problem: ingest " +
			"posts to <base URL>/v1/logs, so the same wrong prefix 404s every export, silently"
	case http.StatusTooManyRequests:
		return "OpenObserve is rate-limiting this credential (429) — the credential and the endpoint are fine; " +
			"try again shortly"
	}
	if status >= 500 {
		return fmt.Sprintf("OpenObserve answered %d — TG reached it and the credential was presented, but "+
			"OpenObserve itself is failing. This is a vendor-side fault rather than a TG configuration one", status)
	}
	return fmt.Sprintf("OpenObserve answered %d to an authenticated read of the stream list", status)
}

// classifyProbeTransport turns a transport failure into something an operator can act on. It classifies on
// the SHAPE of the failure (DNS, refused connection, TLS, deadline) rather than on message text, and falls
// through to the raw error rather than inventing a diagnosis it cannot support.
func classifyProbeTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "OpenObserve did not answer inside the test's time budget — it is reachable but very slow, or " +
			"something in front of it is holding the connection open without replying"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the OpenObserve host name did not resolve (" + dnsErr.Name + ") — the base URL is wrong, or the " +
			"worker's DNS cannot see it. No credential ever left this process"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "nothing accepted a connection at the OpenObserve base URL — the instance is down, the port is " +
			"wrong (the HTTP API is 5080 by default, not the 5081 gRPC port), or a firewall between the worker " +
			"and it is dropping the connection"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Safe to print: the credential travels in a header, never in the URL.
		return "OpenObserve could not be reached at " + urlErr.URL + " — a network, DNS or TLS problem rather " +
			"than a credential one (" + urlErr.Err.Error() + ")"
	}
	return err.Error()
}

// joinProbeNotes assembles the operator-facing Detail, so the pass and fail paths cannot drift into two
// different layouts.
func joinProbeNotes(notes []string) string {
	kept := make([]string, 0, len(notes))
	for _, n := range notes {
		if strings.TrimSpace(n) != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, ". ")
}
