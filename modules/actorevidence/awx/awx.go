// Package awx is the AWX job-history actor-evidence reader (spec/023 T-023-10, REQ-2306/REQ-2307, the
// SECOND machine-automation domain after pve). Given a target host it reads the AWX jobs that actually RAN
// against that host — /api/v2/hosts/?name=<target> → the host's /job_host_summaries/ → each summary's job —
// and emits ONE minimized Evidence per in-window job naming the target host, the job's LAUNCHER as the
// actor, and the job-template + status as the action. It is READ-ONLY and FAIL-CLOSED: a missing host,
// an unresolvable token, or a non-2xx aborts with an error (never a partial/blank identity).
//
// Actor faithfulness (REQ-2306): `launched_by` is WHO launched THIS run as AWX records it — `{name,type}`.
// type=="user" is a human; type in {schedule,workflow,webhook,""} is an automation/self-adjacent principal,
// so the actor is emitted as "<name>" for a user and "<type>:<name>" otherwise, letting the deterministic
// attributor's loadable Sanctioned/SelfActors config (spec/023) decide the taxonomy — this reader never
// classifies. Grounded live against the estate AWX API 2026-07-24 (job 31380 → launched_by {user,admin};
// its job_host_summaries → host dc1omktst01, changed=5).
//
// Provenance: [O] spec/023 REQ-2306/2307, T-023-10.
package awx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/config"
)

// Doer is the minimal HTTP surface (so a test drives a fake without a live AWX).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the read-only AWX job-history reader.
type Module struct {
	base     string
	tokenRef config.SecretRef
	http     Doer
	timeout  time.Duration
	// pageSize is the DRF page size for the job_host_summaries walk; maxPages is the hard safety bound on how
	// many pages the (window-margin-bounded) walk may follow before failing closed rather than truncating.
	pageSize int
	maxPages int
}

// Option configures a Module.
type Option func(*Module)

// WithHTTPClient injects the HTTP client (a fake in tests).
func WithHTTPClient(d Doer) Option { return func(m *Module) { m.http = d } }

// WithTimeout caps the per-Read deadline (bounded to a sane ceiling).
func WithTimeout(d time.Duration) Option {
	return func(m *Module) {
		if d > 0 && d <= 20*time.Second {
			m.timeout = d
		}
	}
}

// New builds an AWX reader against baseURL, authenticating with the bearer token at tokenRef (resolved at
// use time, never stored — INV-13).
func New(baseURL string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{base: strings.TrimRight(baseURL, "/"), tokenRef: tokenRef, http: http.DefaultClient, timeout: 12 * time.Second, pageSize: 200, maxPages: 25}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Domain implements actorevidence.Reader.
func (m *Module) Domain() string { return "awx" }

// ReadOnly implements actorevidence.Reader — this reader never mutates AWX.
func (m *Module) ReadOnly() bool { return true }

var _ actorevidence.Reader = (*Module)(nil)

// Read returns one Evidence per AWX job that ran against `target` and finished within [since, until].
func (m *Module) Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	hostID, err := m.resolveHost(ctx, target)
	if err != nil {
		return nil, err
	}
	// Walk the host's job_host_summaries newest-first, following DRF pagination, and STOP once a job was
	// created before the window start minus a generous max-run margin — no older job can still have finished
	// inside the window (jobs are id-ordered by creation). This bounds the walk to the window without ever
	// SILENTLY truncating in-window jobs; if a host is so busy that the walk exceeds maxPages while still
	// inside the margin, we fail closed with an explicit truncation error rather than under-report (advisory
	// upstream per REQ-2307), never a partial-but-plausible set.
	stopBefore := since.Add(-maxJobRun)
	seen := map[int]bool{}
	var out []attribution.Evidence
	next := fmt.Sprintf("/api/v2/hosts/%d/job_host_summaries/?order_by=-id&page_size=%d", hostID, m.pageSize)
	for pages := 0; next != ""; pages++ {
		if pages >= m.maxPages {
			return nil, fmt.Errorf("awx: host %q has more than %d pages of job history within the window margin — coverage would truncate (fail closed, not silent)", target, m.maxPages)
		}
		page, err := m.summaryPage(ctx, next)
		if err != nil {
			return nil, err
		}
		stop := false
		for _, s := range page.Results {
			jobID := s.SummaryFields.Job.ID
			if jobID == 0 || seen[jobID] {
				continue
			}
			seen[jobID] = true
			job, err := m.job(ctx, jobID)
			if err != nil {
				continue // advisory (REQ-2307): a single unreadable job is skipped, never hides the rest
			}
			if c := job.createdAt(); !c.IsZero() && c.Before(stopBefore) {
				stop = true // this and every older job predate the window margin → walk is done
				break
			}
			at := job.finishedAt()
			if at.IsZero() || at.Before(since) || at.After(until) {
				continue
			}
			actor := job.actor()
			if actor == "" {
				continue // an actor-evidence record with no principal is not admissible (REQ-2306/2307)
			}
			out = append(out, attribution.Evidence{
				Domain:     m.Domain(),
				Actor:      actor,
				ActionKind: job.actionKind(),
				Target:     target,
				ObservedAt: at,
				Ref:        strconv.Itoa(jobID),
				Covered:    true,
			})
		}
		if stop {
			break
		}
		next = m.pathOf(page.Next)
	}
	return out, nil
}

// maxJobRun is the generous upper bound on how long a single AWX job may run; a job created before
// (since - maxJobRun) cannot have finished inside the window, so the paginated walk stops there.
const maxJobRun = 24 * time.Hour

// ---- AWX API shapes (only the fields we read) ------------------------------------------------------

type hostRow struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func (m *Module) resolveHost(ctx context.Context, name string) (int, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("page_size", "2")
	body, err := m.get(ctx, "/api/v2/hosts/?"+q.Encode())
	if err != nil {
		return 0, err
	}
	var env struct {
		Results []hostRow `json:"results"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, fmt.Errorf("awx: hosts response unparseable: %w", err)
	}
	match := 0
	for _, h := range env.Results {
		if h.Name == name {
			if match != 0 {
				return 0, fmt.Errorf("awx: host name %q is ambiguous in AWX (fail closed)", name)
			}
			match = h.ID
		}
	}
	if match == 0 {
		return 0, fmt.Errorf("awx: host %q not found in AWX (fail closed)", name)
	}
	return match, nil
}

type summaryRow struct {
	SummaryFields struct {
		Job struct {
			ID int `json:"id"`
		} `json:"job"`
	} `json:"summary_fields"`
}

type summaryPageT struct {
	Next    string       `json:"next"` // DRF absolute next-page URL, "" (JSON null) on the last page
	Results []summaryRow `json:"results"`
}

func (m *Module) summaryPage(ctx context.Context, path string) (summaryPageT, error) {
	body, err := m.get(ctx, path)
	if err != nil {
		return summaryPageT{}, err
	}
	var page summaryPageT
	if err := json.Unmarshal(body, &page); err != nil {
		return summaryPageT{}, fmt.Errorf("awx: job_host_summaries unparseable: %w", err)
	}
	return page, nil
}

// pathOf converts a DRF `next` URL (absolute, e.g. https://awx/api/v2/…?page=2) to the path+query this
// client's get() expects; a blank or unparseable next ends the walk.
func (m *Module) pathOf(next string) string {
	if next == "" {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

type jobRow struct {
	Finished   string `json:"finished"`
	Created    string `json:"created"`
	Status     string `json:"status"`
	LaunchedBy struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"launched_by"`
	SummaryFields struct {
		UnifiedJobTemplate struct {
			Name string `json:"name"`
		} `json:"unified_job_template"`
	} `json:"summary_fields"`
}

// finishedAt is the moment the job actually RAN (completed). A still-running job has no `finished` and
// yields the zero time → it is skipped, never attributed by its `created` time (a running job has not run).
func (j jobRow) finishedAt() time.Time { return parseTime(j.Finished) }

// createdAt drives the paginated walk's early-stop only (never attribution): jobs are id-ordered by
// creation, so once creation is older than the window start minus a generous max-run margin, no remaining
// (older) job can still have finished inside the window.
func (j jobRow) createdAt() time.Time { return parseTime(j.Created) }

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// actor renders launched_by faithfully: a bare username for a human (type=="user"), else "<type>:<name>"
// so the automation/self-adjacent nature survives into the deterministic attributor (which alone classifies).
func (j jobRow) actor() string {
	name := strings.TrimSpace(j.LaunchedBy.Name)
	if j.LaunchedBy.Type == "" || j.LaunchedBy.Type == "user" {
		return name
	}
	return j.LaunchedBy.Type + ":" + name
}

func (j jobRow) actionKind() string {
	tmpl := strings.TrimSpace(j.SummaryFields.UnifiedJobTemplate.Name)
	if tmpl == "" {
		tmpl = "job"
	}
	if j.Status != "" {
		return tmpl + "/" + j.Status
	}
	return tmpl
}

func (m *Module) job(ctx context.Context, id int) (jobRow, error) {
	body, err := m.get(ctx, fmt.Sprintf("/api/v2/jobs/%d/", id))
	if err != nil {
		return jobRow{}, err
	}
	var j jobRow
	if err := json.Unmarshal(body, &j); err != nil {
		return jobRow{}, fmt.Errorf("awx: job %d unparseable: %w", id, err)
	}
	return j, nil
}

// ---- transport -------------------------------------------------------------------------------------

func (m *Module) get(ctx context.Context, path string) ([]byte, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, fmt.Errorf("awx: read-only token unresolvable (INV-13): %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("awx: GET %s → %d: %s", path, resp.StatusCode, msg)
	}
	return b, nil
}
