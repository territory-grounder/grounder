// This file is the native-Go AWX (Ansible AWX / automation-controller) REST client for the AWX-job effect
// lane (spec/017 T-017-3, REQ-1704..1709, TG-110). It speaks the AWX / AWX-Tower REST API v2 over
// net/http + crypto/tls — NO subprocess, no awx-cli, no ansible binary (INV-02, distroless-safe) — with a
// long-lived OAuth2 Bearer token supplied as a core/config.SecretRef and NEVER logged (INV-13).
//
// It exposes exactly two capabilities the governed launch path needs:
//   - Launch  — POST /api/v2/job_templates/{id}/launch/ : the MUTATING effect (gated OFF until the flip; the
//     actuator only reaches it under the mode chokepoint). Returns the async job handle (job id).
//   - GetJob  — GET  /api/v2/jobs/{id}/ : the poll the GLOBAL deferred-verify channel (T-017-4) drives a
//     launched job to a terminal AWX status through. Read-only.
//
// The LAUNCH token is DISTINCT from the read-only sensor token used by the awxplaybooks knowledge lane
// (REQ-1708): a launch-capable credential, declared separately from any read-only sensor credential.
//
// Grounded in the AWX REST API v2 (docs.ansible.com/automation-controller + the AWX source, consistent with
// the verified-live modules/credsource/awx and modules/knowledge/awxplaybooks connectors, both confirmed
// against https://awx.example.net v24.6.1):
//   - POST /api/v2/job_templates/{id}/launch/ with body { "extra_vars": {…typed…}, "limit": "<host>" }.
//     A successful launch returns the JOB resource { id, job, url, status, job_template, ignored_fields }.
//     `id` (legacy alias `job`) is the async job handle. `ignored_fields` lists any prompt-on-launch field
//     (extra_vars/limit/…) the client SENT that the template did not enable (ask_variables_on_launch /
//     ask_limit_on_launch false) — AWX drops it silently. This client treats an ignored field it SENT as a
//     LAUNCH REFUSAL, never a silent no-op: a dropped extra_var or a dropped host-limit is a launch whose
//     effect no longer matches the prediction (REQ-1705 / the async-gap discipline).
//   - GET /api/v2/jobs/{id}/ → { id, status, failed, started, finished, … }. `status` transitions
//     pending → waiting → running → { successful | failed | error | canceled }. The four terminal statuses
//     (confirmed from awx/main/models/unified_jobs.py: the set that stamps `finished`) drive the spec/002
//     mechanical verdict in the deferred-verify channel (REQ-1709/1710).
//
// Provenance: [O] INV-02/INV-06/INV-13/INV-21, spec/017 (REQ-1704..1709), TG-110.
package awxjob

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this lane serves; it matches the awx-job regime slug.
const SourceType = "awx-job"

// defaultTimeout bounds a single API round-trip so a hung AWX fails closed promptly.
const defaultTimeout = 20 * time.Second

// Doer is the minimal HTTP contract; *http.Client satisfies it and tests inject a fake AWX.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is the AWX REST API client for the AWX-job lane. It holds the LAUNCH-capable Bearer OAuth2 token
// (resolved from a SecretRef, cached after first use, never logged) — distinct from the read-only sensor
// token (REQ-1708). It exposes the mutating Launch (POST) and the read-only GetJob (GET) the governed launch
// path and the deferred-verify channel need, and nothing else.
type Client struct {
	baseURL  string // e.g. "https://awx.example.net" (no trailing slash, no /api)
	tokenRef config.SecretRef
	http     Doer

	mu     sync.Mutex
	cached string // cached resolved LAUNCH token; never logged
}

// ClientConfig constructs a Client. BaseURL is required. TokenRef (the LAUNCH-capable Bearer token, a
// core/config.SecretRef — never a literal, and distinct from the read-only sensor token, REQ-1708) is
// required. CACertPath, if set, is trusted for TLS (AWX behind a private CA). HTTPClient overrides transport
// entirely (tests inject a fake AWX).
type ClientConfig struct {
	BaseURL    string
	TokenRef   config.SecretRef
	CACertPath string
	HTTPClient Doer
}

// NewClient builds an AWX launch client. It fails closed if the base URL is missing, the launch token
// reference is empty (this lane authenticates — an anonymous launch is refused), or a configured CA cert
// cannot be loaded.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("awxjob: base URL is required")
	}
	if strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, fmt.Errorf("awxjob: launch token reference is required (fail closed)")
	}
	c := &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), tokenRef: cfg.TokenRef, http: cfg.HTTPClient}
	if c.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("awxjob: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("awxjob: CA cert %q contains no valid certificate", cfg.CACertPath)
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		c.http = hc
	}
	return c, nil
}

// ErrTokenUnresolved is returned when the launch token's SecretRef could not be READ from its backend. It is
// a TG-side fault (a wrong reference, or an unreachable secret backend), NOT an AWX one, and the self-test
// says so — an operator told "AWX rejected the credential" would go and mint a new AWX token for a problem
// that lives entirely on this side of the wire.
var ErrTokenUnresolved = errors.New("awxjob: resolve launch token")

// ErrTokenEmpty is returned when the reference resolved but yielded nothing. Distinct from ErrTokenUnresolved
// because the fix is different: the backend answered, the value at that path is blank.
var ErrTokenEmpty = errors.New("awxjob: launch token is empty (fail closed)")

// token resolves the launch Bearer token once and caches it. An empty/unset token fails closed.
//
// THE CACHE IS WHY THE SELF-TEST'S 401 ADVICE SAYS "restart": c.cached lives for the process's lifetime and
// the client is built once at boot, so a token rotated in the console is not picked up by a running worker.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	tok, err := c.tokenRef.Resolve()
	if err != nil {
		// Wrapping BOTH keeps the historical message ("awxjob: resolve launch token: <cause>") byte-identical
		// while making the class matchable with errors.Is instead of by parsing the string.
		return "", fmt.Errorf("%w: %w", ErrTokenUnresolved, err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", ErrTokenEmpty
	}
	c.cached = tok
	return tok, nil
}

// ---- AWX object shapes (only the fields this client reads) ------------------------------------------

// Launched is the async job HANDLE a successful launch returns — the effect leaf hands it out (via the
// actuator's Result) so the GLOBAL deferred-verify channel (T-017-4) can poll the job to terminal. JobID is
// the AWX job id; Status is its initial (non-terminal) status; URL is the job's API URL.
type Launched struct {
	JobID      int    `json:"job_id"`
	Status     string `json:"status"`
	TemplateID int    `json:"template_id"`
	URL        string `json:"url,omitempty"`
}

// launchResponse is the subset of the AWX launch response this client reads: the job id (id, or the legacy
// `job` alias), its url + status + job_template, and the ignored_fields map AWX returns for any
// prompt-on-launch field the template did not enable.
type launchResponse struct {
	ID            int            `json:"id"`
	Job           int            `json:"job"` // legacy alias for the launched job's id
	URL           string         `json:"url"`
	Status        string         `json:"status"`
	JobTemplate   int            `json:"job_template"`
	IgnoredFields map[string]any `json:"ignored_fields"`
}

// Job is the subset of a /jobs/{id}/ result the deferred-verify channel reads: identity + the status the
// verdict keys off + the failed flag + the started/finished timestamps.
type Job struct {
	ID       int    `json:"id"`
	Status   string `json:"status"`
	Failed   bool   `json:"failed"`
	Started  string `json:"started"`
	Finished string `json:"finished"`
}

// terminalStatuses is the closed set of AWX terminal job statuses (confirmed from AWX
// awx/main/models/unified_jobs.py — the statuses that stamp `finished`). A job in one of these has reached a
// deferred-verifiable outcome (REQ-1709).
var terminalStatuses = map[string]bool{
	"successful": true,
	"failed":     true,
	"error":      true,
	"canceled":   true,
}

// IsTerminalStatus reports whether an AWX job status is terminal (the deferred-verify channel stops polling
// and computes the mechanical verdict once true).
func IsTerminalStatus(status string) bool { return terminalStatuses[strings.TrimSpace(status)] }

// ---- HTTP plumbing -----------------------------------------------------------------------------------

// Launch POSTs /api/v2/job_templates/{id}/launch/ with ONLY the typed, schema-validated extra_vars and the
// host limit (no free-form command string, REQ-1705). It resolves the launch token (fail closed on an empty
// token), sends it as a Bearer header (never logged), and returns the async job handle. It FAILS CLOSED on
// any non-2xx status, a missing job id, OR an ignored_fields entry for a field it SENT — a dropped prompt
// field is a launch whose effect no longer matches the prediction, never a silent no-op.
func (c *Client) Launch(ctx context.Context, templateID int, extraVars map[string]any, limit string) (Launched, error) {
	if templateID <= 0 {
		return Launched{}, fmt.Errorf("awxjob: launch requires a positive job_template id (fail closed)")
	}
	// Build the launch body from ONLY the fields we intend to send, tracking them so an ignored_fields echo
	// can be checked against exactly what we sent.
	body := map[string]any{}
	sent := map[string]bool{}
	if len(extraVars) > 0 {
		body["extra_vars"] = extraVars
		sent["extra_vars"] = true
	}
	if l := strings.TrimSpace(limit); l != "" {
		body["limit"] = l
		sent["limit"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Launched{}, fmt.Errorf("awxjob: marshal launch body: %w", err)
	}
	raw, err := c.do(ctx, http.MethodPost, "/api/v2/job_templates/"+strconv.Itoa(templateID)+"/launch/", payload)
	if err != nil {
		return Launched{}, err
	}
	var lr launchResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return Launched{}, fmt.Errorf("awxjob: decode launch response: %w", err)
	}
	// An ignored prompt field we SENT means AWX dropped it (the template did not enable it): refuse — the
	// launch's effect no longer matches what we predicted (REQ-1705, the async-gap discipline).
	for field := range sent {
		if v, ok := lr.IgnoredFields[field]; ok && v != nil {
			return Launched{}, fmt.Errorf("%w: %q (the job template does not accept it on launch)", ErrLaunchFieldIgnored, field)
		}
	}
	jobID := lr.ID
	if jobID == 0 {
		jobID = lr.Job
	}
	if jobID <= 0 {
		return Launched{}, fmt.Errorf("awxjob: launch returned no job handle (fail closed)")
	}
	return Launched{JobID: jobID, Status: lr.Status, TemplateID: templateID, URL: lr.URL}, nil
}

// ---- READ-ONLY IDENTITY + TEMPLATE READS (the self-test's whole network surface) ---------------------
//
// These two GETs exist for ONE caller: SelfTest (selftest.go). They are here rather than in the probe because
// a probe that built its own HTTP client, its own URL and its own token resolution would prove that SECOND
// client works and say nothing about the one that launches. The whole value of pressing Test is that the
// bytes travel the same path the launch will: c.do, c.baseURL, c.token, the same CA pool.
//
// Neither of them can mutate anything: AWX's /api/v2/me/ and /api/v2/job_templates/{id}/ are GET-only reads,
// and c.do is called with http.MethodGet and a nil body. There is deliberately NO ListJobTemplates here — the
// probe re-reads only the ids the operator already sanctioned, so the test cannot become a way to enumerate an
// AWX the operator did not point us at.

// Identity is the subset of /api/v2/me/ the self-test reports: WHICH account the launch token belongs to.
//
// The username is the load-bearing field. A launch token is opaque and interchangeable at a glance; the
// account behind it is not. An operator who pastes the read-only SENSOR token into the launch-token box
// (REQ-1708 exists precisely because those two must stay distinct) gets a green auth check either way — but
// a DIFFERENT username, which is the only signal in the whole exchange that says so.
type Identity struct {
	ID              int    `json:"id"`
	Username        string `json:"username"`
	IsSuperuser     bool   `json:"is_superuser"`
	IsSystemAuditor bool   `json:"is_system_auditor"`
}

// meResponse is AWX's shape for /api/v2/me/: a paginated LIST that contains exactly the calling user.
type meResponse struct {
	Count   int        `json:"count"`
	Results []Identity `json:"results"`
}

// JobTemplate is the subset of /api/v2/job_templates/{id}/ the self-test reports.
//
// Name is what makes a wrong-template misconfiguration visible: the allowlist is a list of NUMBERS, and a
// number that exists on the wrong AWX (or was renumbered by a re-import) is indistinguishable from a correct
// one until something reads the name back.
//
// AskVariablesOnLaunch / AskLimitOnLaunch are read because Launch REFUSES when AWX reports a field it sent
// under ignored_fields (ErrLaunchFieldIgnored). A template that does not prompt for variables will therefore
// refuse every launch that carries extra_vars — a defect that today is only discoverable by launching, i.e.
// after the owner-present flip, in the one situation where a surprise is least welcome. Reading the two flags
// costs no extra request and turns that into a warning an operator can act on while the lane is still inert.
type JobTemplate struct {
	ID                   int    `json:"id"`
	Name                 string `json:"name"`
	JobType              string `json:"job_type"`
	AskVariablesOnLaunch bool   `json:"ask_variables_on_launch"`
	AskLimitOnLaunch     bool   `json:"ask_limit_on_launch"`
}

// WhoAmI reads the account the launch token authenticates as (GET /api/v2/me/). Read-only.
//
// It is the cheapest honest proof that all three of {base URL reachable, token resolvable, token accepted}
// hold, and unlike a bare 200 it returns something an operator can compare against what they expected.
func (c *Client) WhoAmI(ctx context.Context) (Identity, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v2/me/", nil)
	if err != nil {
		return Identity{}, err
	}
	var mr meResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return Identity{}, fmt.Errorf("awxjob: decode /api/v2/me/: %w", err)
	}
	// A 200 with no user is not a pass. It means something answered on this URL that is not the AWX API —
	// a proxy error page, a captive portal, a different product — and reporting "authenticated" for that is
	// exactly the false green a self-test exists to prevent.
	if len(mr.Results) == 0 {
		return Identity{}, fmt.Errorf("awxjob: /api/v2/me/ returned no user (this URL answered, but not as AWX)")
	}
	return mr.Results[0], nil
}

// GetJobTemplate RE-READS one job template by id (GET /api/v2/job_templates/{id}/). Read-only.
//
// It proves three separate things a bare auth check cannot: the id EXISTS on this AWX, this token may SEE it,
// and it is the template the operator meant (the name comes back). It does not — and cannot — prove the token
// may EXECUTE it; AWX only settles that at launch, and the self-test must not launch. That limit is stated in
// the probe's Detail rather than hidden behind a green tick.
func (c *Client) GetJobTemplate(ctx context.Context, id int) (JobTemplate, error) {
	if id <= 0 {
		return JobTemplate{}, fmt.Errorf("awxjob: GetJobTemplate requires a positive job_template id")
	}
	raw, err := c.do(ctx, http.MethodGet, "/api/v2/job_templates/"+strconv.Itoa(id)+"/", nil)
	if err != nil {
		return JobTemplate{}, err
	}
	var jt JobTemplate
	if err := json.Unmarshal(raw, &jt); err != nil {
		return JobTemplate{}, fmt.Errorf("awxjob: decode job template %d: %w", id, err)
	}
	// A 200 carrying valid JSON is NOT proof this was a job template, for exactly the reason /api/v2/me/
	// checks for an empty user list: a reverse proxy, a captive portal or a different product answering on
	// this base URL will serve a 200 with a body that unmarshals cleanly into a struct of zero values. Left
	// unchecked the self-test reports `0=""` as a successfully re-read sanctioned template — a green tick for
	// an estate nobody actually reached, which is the precise false green this read exists to deny.
	//
	// The id equality is the second half and the load-bearing one. GetJobTemplate(7) that returns template 99
	// has not answered the question asked; a caller that reports back what it was HANDED rather than what it
	// ASKED FOR would print the served id and hide the substitution.
	if jt.ID == 0 && strings.TrimSpace(jt.Name) == "" {
		return JobTemplate{}, fmt.Errorf("awxjob: /api/v2/job_templates/%d/ answered 200 but carried no job "+
			"template (this URL answered, but not as AWX)", id)
	}
	if jt.ID != id {
		return JobTemplate{}, fmt.Errorf("awxjob: asked AWX for job template %d and it answered with template %d "+
			"(%q) — this endpoint is not serving the AWX job-template API faithfully", id, jt.ID, jt.Name)
	}
	return jt, nil
}

// GetJob reads one job by id (GET /api/v2/jobs/{id}/) — the read-only poll the deferred-verify channel drives
// to a terminal status (REQ-1709). It fails closed on unreachable/denied/not-found/malformed.
func (c *Client) GetJob(ctx context.Context, jobID int) (Job, error) {
	if jobID <= 0 {
		return Job{}, fmt.Errorf("awxjob: GetJob requires a positive job id")
	}
	raw, err := c.do(ctx, http.MethodGet, "/api/v2/jobs/"+strconv.Itoa(jobID)+"/", nil)
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return Job{}, fmt.Errorf("awxjob: decode job %d: %w", jobID, err)
	}
	return j, nil
}

// do issues one authenticated request (GET or POST) against a path joined onto the base, attaching the launch
// Bearer token and returning the body on 2xx or an error (never a body with a nil error). The token is NEVER
// placed in an error message; the path is scrubbed of any query string before it enters one.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	full, err := c.resolveURL(path)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, terr := c.token()
	if terr != nil {
		return nil, terr
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &TransportError{Method: method, URL: scrub(full), Err: err} // unreachable → fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &StatusError{Method: method, URL: scrub(full), Status: resp.StatusCode, Detail: awxErr(out)}
	}
	return out, nil
}

// StatusError is a non-2xx answer from AWX. TransportError is a request that never got an answer at all.
//
// They are TYPES rather than formatted strings because the self-test must classify a failure by its SHAPE —
// 401 vs 403 vs 404 vs "the host is down" are four different things for an operator to go and fix, and they
// are distinguished by the status code, not by the prose AWX chose to put in the body. Matching on prose
// silently stops working when a vendor rewords a message or a proxy substitutes its own error page.
//
// Both render EXACTLY the message the previous fmt.Errorf produced, so every existing caller and test that
// reads the text is unaffected; what is new is that errors.As can now reach the code.
type StatusError struct {
	Method string
	URL    string // already scrubbed of any query string
	Status int
	// Detail is AWX's own {"detail": ...} text, truncated. It is included for the operator's benefit but is
	// never CLASSIFIED on — see the type comment.
	Detail string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("awxjob: %s %s: status %d: %s", e.Method, e.URL, e.Status, e.Detail)
}

// TransportError is a request that produced no HTTP response: DNS failure, refused connection, TLS
// rejection, timeout, cancelled context. The distinction from StatusError matters because there is no
// credential problem to fix here — nothing ever read the credential.
type TransportError struct {
	Method string
	URL    string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("awxjob: %s %s: %v", e.Method, e.URL, e.Err)
}

// Unwrap keeps errors.Is(err, context.DeadlineExceeded) and friends working through the wrapper.
func (e *TransportError) Unwrap() error { return e.Err }

// resolveURL joins a root-relative API path onto the client base and refuses any off-host absolute link so a
// compromised or MITM'd AWX cannot redirect a Bearer-token-carrying request to another host (INV-13).
func (c *Client) resolveURL(u string) (string, error) {
	switch {
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		nu, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("awxjob: unparseable url: %w", err)
		}
		bu, err := url.Parse(c.baseURL)
		if err != nil {
			return "", fmt.Errorf("awxjob: unparseable base url: %w", err)
		}
		if !strings.EqualFold(nu.Host, bu.Host) || !strings.EqualFold(nu.Scheme, bu.Scheme) {
			return "", fmt.Errorf("awxjob: refusing off-host url (got host %q scheme %q, expected %q %q)",
				nu.Host, nu.Scheme, bu.Host, bu.Scheme)
		}
		return u, nil
	case strings.HasPrefix(u, "/"):
		return c.baseURL + u, nil
	default:
		return c.baseURL + "/" + u, nil
	}
}

// awxErr extracts AWX's {"detail": "..."} error body into a short, secret-free message.
func awxErr(body []byte) string {
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &e) == nil && e.Detail != "" {
		return e.Detail
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// scrub strips any query string from a URL before it enters an error message (defensive: a token must never
// travel in a query param, but never echo one if a caller ever puts it there).
func scrub(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
