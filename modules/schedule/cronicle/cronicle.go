// Package cronicle is the native-Go, READ-ONLY Scheduling connector for Cronicle (github.com/jhuckaby/
// Cronicle), Territory Grounder's estate scheduler-awareness source (spec/019, REQ-1900..1907). It gives TG
// maintenance-window + already-scheduled-job awareness by reading Cronicle's schedule over its REST API and
// deriving the vendor-neutral core/schedule model. It actuates NOTHING (INV-09 stays intact) and it is the
// integration of the estate's OWN scheduler, not a second scheduler.
//
// Grounded in the Cronicle REST API reference (docs/APIReference.md), verified live against a Cronicle
// 0.9.x instance:
//   - All commands are POST (or GET) to /api/app/<command>/v1; the response envelope carries `code` — 0
//     (a JSON number) on success, or a STRING code ("api"/"session") with a `description` on error, so
//     `code` is decoded as a raw token and only the literal `0` is success.
//   - Auth is an API Key sent as the `X-API-Key` header (also accepted as ?api_key= / body api_key; we use
//     the header so the key never lands in a URL/log). A dedicated read-only key needs NO action privilege.
//   - get_schedule/v1  -> { code, rows:[event], list:{ length } }  (paginate offset/limit until offset>=length)
//   - get_event/v1     -> { code, event }                           (re-read one event by id — INV-05)
//   - get_active_jobs/v1 -> { code, jobs:{ <jobid>: {...} } }       (a MAP keyed by job id)
//     Event fields used: id, title, enabled (int 0/1), target, timezone, notes, timing (an OBJECT of
//     years/months/days/weekdays/hours/minutes arrays, or ABSENT/null for an on-demand event).
//
// Distroless-safe: net/http + crypto/tls with an optional private-CA cert; NO subprocess, no cronicle CLI
// (INV-02). The API key is a core/config.SecretRef (env:/file:/store:), resolved at request time, cached,
// and NEVER logged (INV-13).
//
// Provenance: [F] spec/019 · [O] INV-02/INV-05/INV-09/INV-13.
package cronicle

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "cronicle"

// defaultTimeout bounds a single API round-trip so a hung scheduler fails closed promptly.
const defaultTimeout = 15 * time.Second

// schedulePageSize is the page size requested when paginating get_schedule.
const schedulePageSize = 100

// maxSchedulePages bounds pagination so a misbehaving length/offset can never loop forever (fail closed).
const maxSchedulePages = 1000

// defaultWindowDuration is the window length applied to a tagged event whose directive omits tg-duration.
const defaultWindowDuration = time.Hour

// maxWindowDuration rejects an absurd window length as a misconfiguration (skip-with-record): a window
// longer than this is treated as no window rather than an always-open one.
const maxWindowDuration = 24 * time.Hour

// Doer is the minimal HTTP contract; *http.Client satisfies it and the oracle injects a fake Cronicle.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a read-only Cronicle REST client. It holds the API key (resolved from a SecretRef, cached after
// first use, never logged).
type Client struct {
	baseURL string // e.g. "http://127.0.0.1:3012" (no trailing slash, no /api)
	keyRef  config.SecretRef
	http    Doer

	mu     sync.Mutex
	cached string // cached resolved API key; never logged
}

// Config constructs a Client. BaseURL and KeyRef are required. CACertPath, if set, is trusted for TLS
// (Cronicle behind a private CA). HTTPClient overrides transport entirely (the oracle injects a fake).
type Config struct {
	BaseURL    string
	KeyRef     config.SecretRef
	CACertPath string
	HTTPClient Doer
}

// New builds a Client. It fails closed if the base URL or key reference is missing, or a configured CA cert
// cannot be loaded.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("cronicle: base URL is required")
	}
	if strings.TrimSpace(string(cfg.KeyRef)) == "" {
		return nil, fmt.Errorf("cronicle: API key reference is required (fail closed)")
	}
	c := &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), keyRef: cfg.KeyRef, http: cfg.HTTPClient}
	if c.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("cronicle: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("cronicle: CA cert %q contains no valid certificate", cfg.CACertPath)
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		c.http = hc
	}
	return c, nil
}

// key resolves the API key once and caches it. An empty/unset key fails closed.
func (c *Client) key() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	k, err := c.keyRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("cronicle: resolve API key: %w", err)
	}
	if strings.TrimSpace(k) == "" {
		return "", fmt.Errorf("cronicle: API key is empty (fail closed)")
	}
	c.cached = k
	return k, nil
}

// envelope is the fields every Cronicle response shares. Code is a raw token: the literal `0` (a number) is
// success; any string code ("api"/"session"/...) is an error carrying Description.
type envelope struct {
	Code        json.RawMessage `json:"code"`
	Description string          `json:"description"`
}

func (e envelope) ok() bool { return strings.TrimSpace(string(e.Code)) == "0" }

func (e envelope) err(cmd string) error {
	desc := e.Description
	if desc == "" {
		desc = "code " + string(e.Code)
	}
	return fmt.Errorf("cronicle: %s: API error: %s", cmd, desc)
}

// cronTiming is Cronicle's recurrence object; an ABSENT/null timing (on-demand event) decodes to nil.
type cronTiming struct {
	Years    []int `json:"years"`
	Months   []int `json:"months"`
	Days     []int `json:"days"`
	Weekdays []int `json:"weekdays"`
	Hours    []int `json:"hours"`
	Minutes  []int `json:"minutes"`
}

// cronEvent is one schedule row / get_event event (only the fields TG reads).
type cronEvent struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Enabled  int         `json:"enabled"` // 0/1 over the wire
	Target   string      `json:"target"`
	Timezone string      `json:"timezone"`
	Notes    string      `json:"notes"`
	Category string      `json:"category"`
	Plugin   string      `json:"plugin"`
	Timing   *cronTiming `json:"timing"`
}

type scheduleResp struct {
	envelope
	Rows []cronEvent `json:"rows"`
	List struct {
		Length int `json:"length"`
	} `json:"list"`
}

type eventResp struct {
	envelope
	Event cronEvent `json:"event"`
}

// post issues one authenticated POST to /api/app/<command>/v1 with a JSON body and decodes into v. It fails
// closed on unreachable/denied/malformed/non-2xx and on a non-zero API envelope code. The API key is never
// placed in an error message.
func (c *Client) post(ctx context.Context, command string, body any, v interface {
	envErr() envelope
}) error {
	k, err := c.key()
	if err != nil {
		return err
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cronicle: encode %s request: %w", command, err)
	}
	url := c.baseURL + "/api/app/" + command + "/v1"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", k)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cronicle: POST %s: %w", command, err) // unreachable -> fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cronicle: POST %s: status %d", command, resp.StatusCode)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("cronicle: decode %s: %w", command, err)
	}
	if e := v.envErr(); !e.ok() {
		return e.err(command)
	}
	return nil
}

func (r *scheduleResp) envErr() envelope { return r.envelope }
func (r *eventResp) envErr() envelope    { return r.envelope }

// Schedule reads the full event schedule read-only, following offset/limit pagination to completion. It
// re-reads every event live on each call (no cached copy — INV-05).
func (c *Client) Schedule(ctx context.Context) ([]cronEvent, error) {
	var all []cronEvent
	offset := 0
	for page := 0; page < maxSchedulePages; page++ {
		var resp scheduleResp
		if err := c.post(ctx, "get_schedule", map[string]int{"offset": offset, "limit": schedulePageSize}, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Rows...)
		offset += len(resp.Rows)
		if len(resp.Rows) == 0 || offset >= resp.List.Length {
			return all, nil
		}
	}
	return nil, fmt.Errorf("cronicle: get_schedule exceeded %d pages (fail closed)", maxSchedulePages)
}

// Event re-reads a single event from its system-of-record by id (INV-05 — no cached mutation is trusted).
func (c *Client) Event(ctx context.Context, id string) (cronEvent, error) {
	if strings.TrimSpace(id) == "" {
		return cronEvent{}, fmt.Errorf("cronicle: get_event requires an event id")
	}
	var resp eventResp
	if err := c.post(ctx, "get_event", map[string]string{"id": id}, &resp); err != nil {
		return cronEvent{}, err
	}
	return resp.Event, nil
}
