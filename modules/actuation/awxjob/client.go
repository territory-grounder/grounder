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

// token resolves the launch Bearer token once and caches it. An empty/unset token fails closed.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	tok, err := c.tokenRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("awxjob: resolve launch token: %w", err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("awxjob: launch token is empty (fail closed)")
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
		return nil, fmt.Errorf("awxjob: %s %s: %w", method, scrub(full), err) // unreachable → fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("awxjob: %s %s: status %d: %s", method, scrub(full), resp.StatusCode, awxErr(out))
	}
	return out, nil
}

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
