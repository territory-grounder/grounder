// This file is the Langfuse module's answer to the console's TEST button (core/selftest.Tester).
//
// WHY IT IS A READ AND NOT AN INGEST. This module has exactly one write path — POST /api/public/ingestion,
// the batch route Export and Record both use — and it is the ONLY route that accepts a sample or a trace.
// Probing it would leave a synthetic event in the operator's Langfuse project, sitting in the same trace
// list somebody reads while reconstructing an incident. The notifier is allowed to post because delivery to
// a room IS the thing being proven and the message carries an unmistakable marker; here nothing about a
// write is more convincing than a read, so writing would be a side effect bought for nothing.
//
// WHY /api/public/projects AND NOT /api/public/health. The health route is UNAUTHENTICATED: it answers 200
// on an instance whose keys were rotated an hour ago, and would therefore certify exactly the three things
// an operator presses TEST to rule out — a revoked credential, a permission never granted, an endpoint
// nobody can reach — as fine when two of them are broken. /api/public/projects is the route the Langfuse
// SDKs' own auth-check uses: it takes the same HTTP Basic pair ingestion takes (public key as username,
// secret key as password), and it answers with the PROJECT that pair belongs to. That project name is the
// observation this probe exists to produce — it is the only thing that can tell an operator their worker is
// authenticated against the staging Langfuse rather than the production one, which is the failure a green
// Test is most likely to hide.
//
// WHAT A GREEN RESULT DOES NOT PROVE, repeated in the operator-facing Detail because a silent limit on a
// test is worse than no test: it does not prove ingestion is ACCEPTED. Langfuse settles per-event validity
// at ingestion time and can reject individual events inside a 207 while the credential is perfectly good
// (see ingest()). It also says nothing about whether anything is being shipped at all: the export loop is
// gated by the worker's platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, which is off by default.
package langfuse

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
// operator who pastes the Langfuse key into TG_LANGFUSE_PUBLIC/TG_LANGFUSE_SECRET instead of a pointer to
// it has put the credential in a field this probe prints. core/config already refuses to echo that value
// (its error reports only its LENGTH, precisely so the literal never reaches a log), and a probe that then
// printed the raw string into Summary, Detail and an error would undo that guard on the most commonly
// copied text in an incident.
func safeRef(ref config.SecretRef) string {
	switch config.SchemeOf(ref) {
	case "empty":
		return "an empty reference"
	case "literal":
		return fmt.Sprintf("a reference field holding a bare %d-character value with no env:/file:/store: "+
			"prefix (that is a pasted key, not a reference — it is withheld here)",
			len(strings.TrimSpace(string(ref))))
	default:
		return string(ref)
	}
}

// projectsPath is the authenticated read the probe makes. It is the Langfuse SDKs' own auth-check route, so
// a Langfuse that does not serve it is not a Langfuse this module's ingestion would reach either.
const projectsPath = "/api/public/projects"

// probeBodyLimit bounds the reply we read. The project list for one key pair is a few hundred bytes; a
// misconfigured base URL can point at something that answers with an unbounded stream, and the console is
// holding an operator on a spinner while we read it.
const probeBodyLimit = 1 << 20

// maxNamedProjects bounds how many project names go into a one-line Summary. A key pair normally maps to
// exactly one project; the cap exists so an unusual answer cannot turn Summary into a wall of text.
const maxNamedProjects = 3

// projectsResponse is the shape of GET /api/public/projects. Only the two fields worth reporting are
// decoded: the name an operator recognises, and the id they can match against the Langfuse UI.
type projectsResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
}

// compile-time proof this module can honour the TEST button its descriptor advertises. Without it a rename
// in core/selftest would silently turn SelfTest back into an unreachable method — a capability that exists
// and is never called, which is the defect class this whole exercise is closing.
var _ selftest.Tester = (*Module)(nil)

// SelfTest authenticates against the configured Langfuse with the real key pair and reads back the project
// that pair belongs to. It ingests nothing.
//
// The operator argument is ignored: this probe leaves nothing in Langfuse for anyone to attribute.
// Attribution exists in core/selftest for the notifier, whose probe posts into a room humans watch.
//
// It returns an error when either key reference cannot be resolved, when the endpoint cannot be reached,
// when Langfuse refuses the pair, or when the answer is not a Langfuse project list at all — that last one
// because a 200 from something that is not Langfuse is exactly the "pointed at the wrong thing" case this
// probe exists to catch.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil || strings.TrimSpace(m.endpoint) == "" {
		// Reachable only from a zero-value Module: with an empty TG_LANGFUSE_URL the composition root never
		// registers the exporter at all. Reported honestly rather than left to nil-deref inside the activity,
		// which the console would render as an infrastructure fault instead of a configuration one.
		return selftest.Result{Summary: "no Langfuse base URL is configured, so no exporter was registered"},
			errors.New("langfuse: self-test needs a base URL — TG_LANGFUSE_URL is empty, so nothing is ingested and nothing reports an error")
	}
	if m.http == nil {
		// Only reachable through a hand-built Module (New always installs a transport). NOT quietly
		// substituted with a default client: a probe over a transport the exporter does not use would be
		// proving the wrong thing.
		return selftest.Result{Summary: "this Langfuse module has no HTTP transport, so nothing can be sent"},
			errors.New("langfuse: self-test found no HTTP transport on the module — it was not built by New")
	}

	// ── 1. THE KEY PAIR, resolved exactly as ingest() resolves it on every send ────────────────────────
	// Both halves, in the same order, through the module's own SecretRefs. Never echoed: an error string is
	// the most-pasted text in an incident.
	public, err := m.publicRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the Langfuse public key could not be read from " + safeRef(m.publicRef) + " — nothing was sent",
			Detail:  refFaultDetail("public key", safeRef(m.publicRef), err),
		}, fmt.Errorf("langfuse: self-test could not resolve the public-key reference %s: %w", safeRef(m.publicRef), err)
	}
	secret, err := m.secretRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the Langfuse secret key could not be read from " + safeRef(m.secretRef) + " — nothing was sent",
			Detail:  refFaultDetail("secret key", safeRef(m.secretRef), err),
		}, fmt.Errorf("langfuse: self-test could not resolve the secret-key reference %s: %w", safeRef(m.secretRef), err)
	}
	// An empty half is worse than a missing one: Resolve succeeded, so nothing complains, and every export
	// goes out with a Basic header Langfuse will refuse — visible only as a metrics project that stays empty.
	if strings.TrimSpace(public) == "" || strings.TrimSpace(secret) == "" {
		which := "public key (" + safeRef(m.publicRef) + ")"
		if strings.TrimSpace(secret) == "" {
			which = "secret key (" + safeRef(m.secretRef) + ")"
			if strings.TrimSpace(public) == "" {
				which = "public key (" + safeRef(m.publicRef) + ") and the secret key (" + safeRef(m.secretRef) + ")"
			}
		}
		return selftest.Result{
			Summary: "the " + which + " resolved to an EMPTY value — nothing was sent",
			Detail: "Langfuse ingestion is HTTP Basic and needs BOTH halves of the pair. An empty half is " +
				"rejected on every export, so no sample and no trace has ever landed. Save the key into the " +
				"module's secret lane (the public key is changed where its reference points, not from this dialog).",
		}, errors.New("langfuse: self-test found an empty half of the ingestion key pair")
	}

	// ── 2. THE AUTHENTICATED READ ─────────────────────────────────────────────────────────────────────
	target := m.endpoint + projectsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return selftest.Result{
			Summary: "the configured Langfuse base URL is not a usable URL — nothing was sent",
			Detail:  "TG_LANGFUSE_URL could not be turned into a request: " + err.Error(),
		}, fmt.Errorf("langfuse: self-test could not build a request for %s: %w", projectsPath, err)
	}
	// The SAME auth construction ingest() uses. A probe that authenticated differently would prove that a
	// second, unused code path works.
	req.SetBasicAuth(public, secret)

	resp, err := m.http.Do(req)
	if err != nil {
		return selftest.Result{
			Summary: "Langfuse at " + m.endpoint + " could not be reached — nothing was sent",
			Detail:  classifyProbeTransport(err),
		}, fmt.Errorf("langfuse: self-test could not reach %s: %w", m.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return selftest.Result{
			Summary: fmt.Sprintf("Langfuse at %s answered %d to an authenticated read — nothing was sent", m.endpoint, resp.StatusCode),
			Detail:  classifyProbeStatus(resp.StatusCode),
		}, fmt.Errorf("langfuse: self-test got status %d from %s", resp.StatusCode, projectsPath)
	}

	// ── 3. THE OBSERVATION ────────────────────────────────────────────────────────────────────────────
	var pr projectsResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		// A 200 that is not a Langfuse project list means we are talking to something else — an SSO portal
		// that answered with a login page, a proxy, or the wrong service on the right host. Failing here is
		// the point: this is the "pointed at the wrong instance" case, and it looks like success to anything
		// that only checks the status code.
		return selftest.Result{
			Summary: fmt.Sprintf("%s answered 200 but not with a Langfuse project list — nothing was sent", m.endpoint),
			Detail: "the response could not be parsed as Langfuse's project payload, so this host is probably " +
				"not the Langfuse API root: check TG_LANGFUSE_URL for an extra path prefix, and for a portal or " +
				"proxy answering in front of it. Parse fault: " + err.Error(),
		}, fmt.Errorf("langfuse: self-test could not decode the project list from %s: %w", projectsPath, err)
	}

	var notes []string
	// THE CEILING OF THE PROOF, stated on every pass.
	notes = append(notes, "this proves the endpoint answers and that the public/secret pair is ACCEPTED; it does "+
		"not prove ingestion succeeds — Langfuse settles per-event validity when a batch arrives and can reject "+
		"individual events inside an otherwise successful response")
	notes = append(notes, "shipping also needs the worker's platform-wide TG_OBSERVABILITY_EXPORT_INTERVAL, which "+
		"is off by default: with it unset this module is configured, authenticated and silent")

	if len(pr.Data) == 0 {
		// The credential is proven (Langfuse refuses a bad pair with 401 before it gets here), but there is
		// no project to name — so the one observation that would catch a wrong-instance configuration is
		// missing, and the operator is told exactly that rather than being handed a bare green.
		return selftest.Result{
			Summary: "authenticated to Langfuse at " + m.endpoint + ", which named NO project for this key pair — nothing was sent",
			Detail: joinProbeNotes(append([]string{"the pair was accepted but Langfuse returned an empty project " +
				"list, so this test cannot say WHICH project traces would land in — check in the Langfuse UI that " +
				"these keys still belong to a live project"}, notes...)),
		}, nil
	}

	named := make([]string, 0, maxNamedProjects)
	for _, p := range pr.Data {
		if len(named) == maxNamedProjects {
			break
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = "(unnamed)"
		}
		named = append(named, fmt.Sprintf("%q (id %s)", name, strings.TrimSpace(p.ID)))
	}
	if extra := len(pr.Data) - len(named); extra > 0 {
		notes = append(notes, fmt.Sprintf("%d further project(s) were returned and are not listed here", extra))
	}

	return selftest.Result{
		Summary: fmt.Sprintf("reached Langfuse at %s and authenticated with the ingestion key pair as project %s — no trace or sample was ingested",
			m.endpoint, strings.Join(named, ", ")),
		Detail: joinProbeNotes(notes),
	}, nil
}

// refFaultDetail explains a secret-reference failure as the TG-side fault it is. An operator told "Langfuse
// rejected the credential" would go and rotate a Langfuse key for a problem that never left this process.
func refFaultDetail(which, ref string, err error) string {
	return "the " + which + " never resolved from " + ref + ", so every export is failing the same way before " +
		"a request leaves the worker. This is a TG-side secret problem, not a Langfuse one: either the reference " +
		"points somewhere the value is not, or the secret backend it points at is unreachable. Underlying fault: " +
		err.Error()
}

// classifyProbeStatus turns Langfuse's answer into something an operator can act on.
//
// It classifies on the STATUS CODE and never on the body: Langfuse's wording changes between releases, and a
// reverse proxy in front of it substitutes its own error page entirely, so a text matcher would quietly stop
// classifying and start guessing.
func classifyProbeStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Langfuse rejected the key pair (401) — the public key, the secret key, or both are wrong, " +
			"expired or revoked. The half-rotated pair is the common shape of this: the secret key can be " +
			"changed from this dialog while the PUBLIC key still points at the old project, and that combination " +
			"401s every export. Check the public-key reference shown above as well as the secret you just saved"
	case http.StatusForbidden:
		return "Langfuse accepted the credential but refused the read (403) — the pair is valid and is not " +
			"permitted to read its own project. Ingestion may still work; this test cannot confirm it, which is " +
			"why it is reported as a failure rather than a pass"
	case http.StatusNotFound:
		return "this host does not serve " + projectsPath + " (404) — the Langfuse SDKs' own auth check uses " +
			"that route, so a Langfuse deployment answers it. Check TG_LANGFUSE_URL for an extra path prefix or " +
			"a trailing route, and that it points at the Langfuse API rather than a proxy in front of it"
	case http.StatusTooManyRequests:
		return "Langfuse is rate-limiting this key pair (429) — the credential and the endpoint are fine; try again shortly"
	}
	if status >= 500 {
		return fmt.Sprintf("Langfuse answered %d — TG reached it and the credential was presented, but Langfuse "+
			"itself is failing. This is a vendor-side fault rather than a TG configuration one", status)
	}
	return fmt.Sprintf("Langfuse answered %d to an authenticated read of %s", status, projectsPath)
}

// classifyProbeTransport turns a transport failure into something an operator can act on. It classifies on
// the SHAPE of the failure (DNS, refused connection, TLS, deadline) rather than on message text, and falls
// through to the raw error rather than inventing a diagnosis it cannot support.
func classifyProbeTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "Langfuse did not answer inside the test's time budget — it is reachable but very slow, or " +
			"something in front of it is holding the connection open without replying"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the Langfuse host name did not resolve (" + dnsErr.Name + ") — the base URL is wrong, or the " +
			"worker's DNS cannot see it. No credential ever left this process"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "nothing accepted a connection at the Langfuse base URL — the instance is down, the port is " +
			"wrong, or a firewall between the worker and it is dropping the connection"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Safe to print: the credential travels in a header, never in the URL.
		return "Langfuse could not be reached at " + urlErr.URL + " — a network, DNS or TLS problem rather than " +
			"a credential one (" + urlErr.Err.Error() + ")"
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
