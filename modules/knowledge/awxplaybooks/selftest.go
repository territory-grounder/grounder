package awxplaybooks

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
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/selftest"
)

// THE AWX-PLAYBOOKS TEST BUTTON (core/selftest.Tester).
//
// WHAT THE DIALOG PROMISES. descriptor.go declares Test.Verb "list the AWX job templates read-only", and
// this file is that sentence and nothing more: it resolves the read-only sensor token through the client's
// own SecretRef (the same cached resolution every tick uses), issues ONE authenticated GET against the same
// /api/v2/job_templates/ list endpoint an ingest starts with, and reports how many templates AWX says the
// sensor account can see.
//
// WHY THE LIST AND NOT Ingest.Run. Run is READ-ONLY with respect to AWX but it WRITES the knowledge corpus
// file, and a probe that rewrote the retrieval corpus from a settings dialog would be a side effect the
// operator never consented to — the dialog says "list", not "re-ingest". The list is also the first thing a
// real Run does, so a green Test is the real first step of the real lane rather than a simulation of it.
//
// WHY ONLY THE FIRST PAGE. ListJobTemplateIDs walks the whole .next chain (bounded at maxPages=10,000)
// because the ingest needs every id. A probe runs on an operator's spinner with ONE attempt inside a
// 30-second budget, and AWX's list envelope already states the server-side TOTAL in `count` — so walking
// the chain would issue thousands of requests to learn a number the first response contains. The path comes
// from listPath, the module's own path builder, so the probe cannot drift away from the endpoint the lane
// depends on.
//
// WHY ZERO TEMPLATES IS RED. AWX filters list endpoints by RBAC rather than answering 403: a sensor token
// with no permission on any job template gets 200 and an empty page, indistinguishable at the HTTP layer
// from a healthy read of an empty AWX. Both mean the same thing for this lane — it will ingest nothing, for
// ever, silently, and the agent will keep triaging without ever citing a sanctioned runbook. That is a
// misconfiguration, and it must be red.
//
// WHAT A GREEN RESULT DOES NOT PROVE. That the corpus path points at the retriever's own corpus file
// (TG_KNOWLEDGE_FILE) — a wrong path yields runbooks that are ingested and findable by nothing, which this
// probe cannot see because it deliberately writes nothing. Nor that the token is read-only: this lane has
// no launch path, but a launch-capable credential configured here would test exactly as green, which is why
// the descriptor tells the operator to keep it distinct from the AWX launch token.

// probeBodyLimit bounds what the probe reads from the response. One page of templates is small; the cap
// exists so a misbehaving endpoint on the other end of an operator's TEST cannot stream indefinitely.
const probeBodyLimit = 8 << 20

// SelfTest implements core/selftest.Tester for the read-only AWX client.
//
// The operator argument is ignored: the probe has no outward side effect. Nothing is launched, nothing is
// created, and no AWX job appears in anyone's activity stream that would need an author's name against it.
func (c *Client) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	obs, err := c.probeTemplates(ctx)
	if err != nil {
		return selftest.Result{
			Summary: "could not list the AWX job templates at " + hostOf(c.baseURL),
			Detail:  classifyProbeFailure(c.tokenRef, err),
		}, err
	}

	summary := fmt.Sprintf("listed the AWX job templates at %s — %d visible to the sensor token",
		hostOf(c.baseURL), obs.count)
	if obs.witness != "" {
		// The witness is the point of naming anything at all: a count cannot tell an operator that this
		// module is pointed at the staging AWX, and a module pointed at the wrong instance is the failure a
		// green Test is most likely to hide. It is displayed only — REQ-1713's re-read-by-id discipline
		// governs what enters the CORPUS, and this probe ingests nothing.
		summary += ` (e.g. "` + obs.witness + `")`
	}

	if obs.count == 0 {
		return selftest.Result{
				Summary: summary,
				Detail: "The token was accepted, but AWX returned NO job templates, so this lane would " +
					"ingest nothing and the agent could never cite a sanctioned runbook. AWX filters list " +
					"endpoints by RBAC instead of refusing them, so the usual cause is that the sensor " +
					"account has no permission on any job template — grant it read access to the templates " +
					"(an organisation Auditor role is enough). The other cause is a base URL pointing at an " +
					"AWX that genuinely has none.",
			},
			errors.New("awxplaybooks: the sensor token can see no job templates")
	}
	return selftest.Result{Summary: summary}, nil
}

// SelfTest implements core/selftest.Tester for the whole lane by delegating to its client.
//
// It exists because the composition root holds the *Ingest, not the *Client (cmd/worker builds the client,
// wraps it in an Ingest and keeps the Ingest), and the probe registry detects the capability by type
// assertion on whatever instance it is offered. Without this method the module's capability would depend on
// which of the two objects the wiring happened to hand over — an invisible filter of exactly the kind the
// registry exists to remove. It does NOT call Run: Run writes the corpus.
func (ing *Ingest) SelfTest(ctx context.Context, operator string) (selftest.Result, error) {
	if ing == nil || ing.Client == nil {
		return selftest.Result{
				Summary: "the AWX playbooks lane has no client",
				Detail: "The lane was constructed without an AWX client, so nothing could be read. This is a " +
					"wiring fault inside TG, not a configuration or credential problem.",
			},
			errors.New("awxplaybooks: ingest has no client")
	}
	return ing.Client.SelfTest(ctx, operator)
}

// compile-time proof both the client and the lane satisfy the optional self-test capability.
var (
	_ selftest.Tester = (*Client)(nil)
	_ selftest.Tester = (*Ingest)(nil)
)

// templateObservation is what the list endpoint told us: how many job templates the sensor account can see,
// and the name of one of them as a human check on WHICH AWX answered.
type templateObservation struct {
	count   int
	witness string
}

// probeTemplateRow is the subset of a list row the probe displays. It is deliberately separate from
// templateListItem (which declares only the id, because the INGEST must re-read content by id and must not
// be able to take it from the list copy): a name read here is shown to an operator and never ingested.
type probeTemplateRow struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// probeTemplates issues the one authenticated read.
//
// It deliberately does not call ListJobTemplateIDs or getJSON, even though it sends the identical request
// with the identical credential through the identical transport: those flatten a failure into a formatted
// string ("status %d: %s"), and choosing an operator-facing diagnosis by re-parsing our own message text
// turns a harmless wording change into a silently wrong diagnosis. Keeping the STATUS CODE as a value lets
// the classifier switch on the SHAPE of the failure. Everything that makes the read real — c.baseURL,
// c.token() (the same cached SecretRef resolution the lane uses), c.http, listPath — is the client's own.
func (c *Client) probeTemplates(ctx context.Context) (templateObservation, error) {
	full, err := c.resolveURL(listPath("job_templates"))
	if err != nil {
		return templateObservation{}, &probeFault{stage: faultRequest, err: err}
	}
	tok, err := c.token()
	if err != nil {
		return templateObservation{}, &probeFault{stage: faultCredential, err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return templateObservation{}, &probeFault{stage: faultRequest, err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.http.Do(req)
	if err != nil {
		return templateObservation{}, &probeFault{stage: faultTransport, err: err}
	}
	defer resp.Body.Close()
	// One byte MORE than the cap, so a body that hit the cap is DISTINGUISHABLE from one that merely ended
	// there. Without that byte a truncated page decodes exactly like a login page, and the operator would be
	// told a healthy AWX "is not an AWX" — a diagnosis that sends them to rewrite a correct base URL.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return templateObservation{}, &probeFault{stage: faultStatus, status: resp.StatusCode, body: body}
	}
	if len(body) > probeBodyLimit {
		return templateObservation{}, &probeFault{stage: faultTooLarge}
	}

	var p page[probeTemplateRow]
	if err := json.Unmarshal(body, &p); err != nil {
		return templateObservation{}, &probeFault{stage: faultDecode, err: err}
	}
	obs := templateObservation{count: p.Count}
	if obs.count == 0 {
		// Not every AWX-compatible endpoint fills in count; never report fewer templates than we were shown.
		obs.count = len(p.Results)
	}
	for _, r := range p.Results {
		if n := strings.TrimSpace(r.Name); n != "" {
			obs.witness = n
			break
		}
	}
	return obs, nil
}

// probeStage names WHERE the read broke, so the diagnosis is chosen from a value rather than from prose.
type probeStage int

const (
	faultCredential probeStage = iota // the sensor token reference did not resolve — a TG-side fault
	faultRequest                      // the base URL could not form a request
	faultTransport                    // DNS / connect / TLS / deadline — nothing was answered
	faultStatus                       // AWX answered with a non-2xx
	faultDecode                       // 2xx, but the body is not an AWX list envelope
	faultTooLarge                     // 2xx, but the page exceeded what the probe reads in one attempt
)

// probeFault carries the SHAPE of a failure — the stage, the HTTP status when there was one, and the
// response body kept ONLY so an unrecognised status can quote AWX's own {"detail": …} through awxErr,
// which truncates it. Error() never contains the token: an error string is the most commonly pasted text in
// an incident.
type probeFault struct {
	stage  probeStage
	status int
	body   []byte
	err    error
}

func (f *probeFault) Error() string {
	switch f.stage {
	case faultStatus:
		return fmt.Sprintf("awxplaybooks: GET /api/v2/job_templates/: status %d", f.status)
	case faultCredential:
		return fmt.Sprintf("awxplaybooks: resolve read-only sensor token: %v", f.err)
	case faultDecode:
		return "awxplaybooks: GET /api/v2/job_templates/: response is not an AWX list"
	case faultTooLarge:
		return fmt.Sprintf("awxplaybooks: GET /api/v2/job_templates/: answer exceeded the probe's %d-byte read",
			probeBodyLimit)
	default:
		// urlFaultReason, not %v: this branch carries the request-build and transport faults, and a *url.Error
		// prints the URL it was given. net/http redacts the password on the transport path but net/url does not
		// on the parse path, and THIS string is the one that reaches a ticket when nothing catches it.
		return fmt.Sprintf("awxplaybooks: GET /api/v2/job_templates/: %s", urlFaultReason(f.err))
	}
}

func (f *probeFault) Unwrap() error { return f.err }

// classifyProbeFailure turns a failed read into a sentence an operator can act on, classifying on the SHAPE
// of the fault (stage, status code, transport class) and falling through to AWX's own short detail rather
// than inventing a diagnosis it cannot support.
func classifyProbeFailure(ref config.SecretRef, err error) string {
	refText := refLabel(ref)
	var f *probeFault
	if !errors.As(err, &f) {
		return trimErr(err)
	}
	switch f.stage {
	case faultCredential:
		return "the read-only sensor token could not be READ from its reference " + refText + " — the " +
			"reference is wrong, or the secret backend is unreachable. This is a TG-side fault, not an AWX " +
			"one (" + trimErr(f.err) + ")."
	case faultRequest:
		return "the configured base URL is not usable as a URL — it must look like https://awx.example, " +
			"with no trailing slash and no /api (" + urlFaultReason(f.err) + ")."
	case faultTransport:
		return classifyTransport(f.err)
	case faultDecode:
		return "that address answered, but not with an AWX list — it is usually a reverse proxy or a " +
			"different application at that URL. The base URL must be the AWX root, e.g. " +
			"https://awx.example, with no /api and no trailing path."
	case faultTooLarge:
		// NOT a misconfiguration: the credential worked and the endpoint is right — one page of templates was
		// simply larger than one bounded read. Diagnosing it as "that is not an AWX" (which a truncated body
		// otherwise looks exactly like) would send an operator to rewrite a base URL that is correct.
		return "AWX answered with a template page larger than the test reads in one attempt, so the list could " +
			"not be summarised. The token WORKED and the address is right — this is not a credential or " +
			"base-URL fault. The ingest, which reads the same pages on its own schedule, is unaffected."
	case faultStatus:
		switch {
		case f.status == http.StatusUnauthorized:
			return "AWX rejected the sensor token (401) — it is wrong, expired, or has been revoked. Issue a " +
				"new READ-ONLY OAuth2 token in AWX and save it in this dialog (it is read from " + refText +
				"); the client caches it at first use, so the change applies at the next worker restart, not " +
				"the next tick."
		case f.status == http.StatusForbidden:
			return "the token authenticated but AWX refused the read (403) — the sensor account is not " +
				"permitted to list job templates. Grant it read access (an organisation Auditor role is " +
				"enough); it needs nothing more, because this lane never launches anything."
		case f.status == http.StatusNotFound:
			return "there is no AWX API at that address (404) — the base URL must be the AWX root " +
				"(https://awx.example), with no trailing slash and no /api."
		case f.status >= 500:
			return fmt.Sprintf("AWX is reachable but answered %d — the controller itself is unhealthy; this "+
				"is not a TG credential fault.", f.status)
		default:
			// Unrecognised status: report the code and let AWX's own STRUCTURED detail speak rather than
			// guessing. Only the parsed {"detail": …} field, never the raw body — see probeErrDetail.
			if d := probeErrDetail(f.body); d != "" {
				return fmt.Sprintf("AWX answered the template list with an unexpected status %d: %s", f.status, d)
			}
			return fmt.Sprintf("AWX answered the template list with an unexpected status %d.", f.status)
		}
	default:
		return trimErr(err)
	}
}

// classifyTransport names the transport class of a failure that never reached an HTTP status.
func classifyTransport(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "the read did not finish inside the test's time budget — the address may be blackholed, or " +
			"this AWX is answering too slowly."
	}
	var cve *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &cve) || errors.As(err, &unknownCA) || errors.As(err, &hostErr) {
		return "the TLS certificate was refused — AWX is not presenting a certificate TG trusts. If it sits " +
			"behind a private CA, set the CA certificate path for this module; there is deliberately no " +
			"option to skip verification, because that would hand the sensor token to whatever answers."
	}
	return "AWX could not be reached at all — DNS, routing, port or TLS. Check the base URL and that the " +
		"controller is up (" + trimErr(err) + ")."
}

// probeErrDetail returns AWX's own {"detail": …} from an error body, bounded — and NOTHING else.
//
// It deliberately does not do what awxErr does and fall back to a prefix of the RAW body. awxErr's caller is
// a log line; this string is rendered in a settings dialog and pasted into tickets, and the body of a failed
// request came from whatever actually answered at that address. A debug/echo page from a proxy in front of
// AWX can render the request headers back — and this request carries the sensor Bearer token. An unstructured
// body from an unknown responder is not something a probe may quote (the librenms probe refuses the same
// thing for the same reason); a JSON `detail` field is AWX's own deliberate error text.
func probeErrDetail(body []byte) string {
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	d := strings.TrimSpace(e.Detail)
	if len(d) > 200 {
		return d[:200] + "…"
	}
	return d
}

// urlFaultReason renders WHY a URL could not be used, WITHOUT the URL itself.
//
// net/url puts the raw string it failed to parse into *url.Error and prints it verbatim: a base URL of
// https://user:hunter2@awx.example with a stray control character yields an error containing "hunter2". The
// transport path is safe (net/http replaces the password with *** before wrapping); the PARSE path is not.
// So a *url.Error is rendered by its REASON only; anything else falls through unchanged.
func urlFaultReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return trimErr(ue.Err)
	}
	return trimErr(err)
}

// refLabel renders a secret REFERENCE for display.
//
// A reference (env:X, file:/p, bao:…#key) is safe to show — core/config says so explicitly, that being the
// point of INV-13. A value with NO scheme prefix is not a reference at all: it may BE a pasted plaintext
// secret, and echoing it into an error an operator pastes into a ticket would publish it.
func refLabel(ref config.SecretRef) string {
	text := strings.TrimSpace(string(ref))
	if text == "" {
		return "(unset)"
	}
	if scheme, _, ok := strings.Cut(text, ":"); !ok || strings.TrimSpace(scheme) == "" {
		return "(a token reference with no scheme prefix — use env:, file:, store: or a registered backend " +
			"scheme; TG will not accept an inline literal)"
	}
	return text
}

// hostOf renders the host of the configured base URL. It returns the HOST ONLY, never the whole URL: a base
// URL may carry userinfo (https://user:pass@host) and that must not reach the console or a ticket.
func hostOf(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return "the configured AWX endpoint"
	}
	return u.Host
}

// trimErr bounds an error string before it is shown; the console renders one line.
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
