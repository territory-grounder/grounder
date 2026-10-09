// Package gitopsmr is the GitOps merge-request actor-evidence reader (spec/023 T-023-11, REQ-2306/REQ-2307,
// the declarative-deploy domain). It attributes deploy-config changes: merged MRs to the deploy branch that
// touched the deployment manifests (a path prefix, e.g. "deploy/") within the window, keyed on the MR's
// merged_by — the human who approved the change into the deployed config (GitLab is the authoritative record
// of WHO, which k8s audit / managedFields cannot supply since the applier there is the GitOps service account).
//
// Target-agnostic BY DESIGN (grounded 2026-07-24): a manifest change redeploys the whole stack, so a GitOps
// change is deployment-wide, not per-host. The reader therefore echoes the caller's target and reports the
// recent declarative changes as candidate causes; Covered=true because GitLab authoritatively records repo
// changes. READ-ONLY and FAIL-CLOSED: an unresolvable token or a non-2xx aborts with an error (advisory
// upstream per REQ-2307). Grounded live against the estate GitLab: merged MRs expose merged_by / merged_at /
// merge_commit_sha, and /merge_requests/{iid}/diffs exposes the changed paths to filter on the manifest prefix.
//
// Provenance: [O] spec/023 REQ-2306/2307, T-023-11.
package gitopsmr

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

// Doer is the minimal HTTP surface (so a test drives a fake without a live GitLab).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Module is the read-only GitOps MR-history reader.
type Module struct {
	base           string // GitLab instance base, e.g. https://gitlab.example (REST API at /api/v4)
	project        string // project id or URL-encoded path (e.g. "48" or "group%2Fproject")
	tokenRef       config.SecretRef
	targetBranch   string // the deploy branch (default "main")
	manifestPrefix string // a changed path under this prefix marks a declarative-deploy change (default "deploy/")
	http           Doer
	timeout        time.Duration
	pageSize       int
	maxPages       int
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

// WithTargetBranch overrides the deploy branch (default "main").
func WithTargetBranch(b string) Option {
	return func(m *Module) {
		if strings.TrimSpace(b) != "" {
			m.targetBranch = b
		}
	}
}

// WithManifestPrefix overrides the manifest path prefix (default "deploy/").
func WithManifestPrefix(p string) Option {
	return func(m *Module) {
		if strings.TrimSpace(p) != "" {
			m.manifestPrefix = p
		}
	}
}

// New builds a GitOps MR reader against a GitLab instance baseURL + project, authenticating with the
// read-only token at tokenRef (sent as PRIVATE-TOKEN, resolved at use time — INV-13).
func New(baseURL, project string, tokenRef config.SecretRef, opts ...Option) *Module {
	m := &Module{
		base: strings.TrimRight(baseURL, "/"), project: project, tokenRef: tokenRef,
		targetBranch: "main", manifestPrefix: "deploy/", http: http.DefaultClient,
		timeout: 12 * time.Second, pageSize: 100, maxPages: 25,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Domain implements actorevidence.Reader.
func (m *Module) Domain() string { return "gitops-mr" }

// ReadOnly implements actorevidence.Reader — this reader never mutates GitLab.
func (m *Module) ReadOnly() bool { return true }

var _ actorevidence.Reader = (*Module)(nil)

// Read returns one Evidence per merged MR that landed a deploy-manifest change within [since, until].
func (m *Module) Read(ctx context.Context, target string, since, until time.Time) ([]attribution.Evidence, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	base := url.Values{}
	base.Set("state", "merged")
	base.Set("target_branch", m.targetBranch)
	base.Set("order_by", "updated_at")
	base.Set("sort", "desc")
	base.Set("per_page", strconv.Itoa(m.pageSize))
	if !since.IsZero() {
		// GitLab's MR list has no `merged_after` filter; `updated_after` is the tightest server-side bound (a
		// merge always updates the MR, so every merged-in-window MR is included). The exact merged_at ∈ [since,
		// until] check is applied client-side below, so no unmerged/out-of-window MR ever yields evidence.
		base.Set("updated_after", since.UTC().Format(time.RFC3339))
	}

	var out []attribution.Evidence
	page := 1
	for pages := 0; page > 0; pages++ {
		if pages >= m.maxPages {
			return nil, fmt.Errorf("gitops-mr: more than %d pages of merged MRs in the window — coverage would truncate (fail closed, not silent)", m.maxPages)
		}
		q := cloneValues(base)
		q.Set("page", strconv.Itoa(page))
		mrs, next, err := m.mergeRequests(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, mr := range mrs {
			at := parseTime(mr.MergedAt)
			if at.IsZero() || at.Before(since) || at.After(until) {
				continue
			}
			actor := strings.TrimSpace(mr.MergedBy.Username)
			if actor == "" {
				continue // an actor-evidence record with no principal is not admissible (REQ-2306/2307)
			}
			touched, err := m.touchesManifest(ctx, mr.IID)
			if err != nil {
				continue // advisory (REQ-2307): one MR whose manifest coverage we cannot determine is skipped,
				//           never aborting the whole Read (which would discard the MRs already collected)
			}
			if !touched {
				continue // not a declarative-deploy change — outside this reader's coverage
			}
			ref := mr.MergeCommitSHA
			if ref == "" {
				ref = strconv.Itoa(mr.IID)
			}
			out = append(out, attribution.Evidence{
				Domain:     m.Domain(),
				Actor:      actor,
				ActionKind: "MR-merged",
				Target:     target,
				ObservedAt: at,
				Ref:        ref,
				// Covered=FALSE: this reader is target-AGNOSTIC (a manifest change is deployment-wide, so it
				// returns the same MRs for any target). Unlike the per-target readers (pve/journal/netbox/awx
				// which resolve the specific object and read ITS trail), it cannot AFFIRM it covers a named
				// target's audit trail (REQ-2304 half 2). The evidence is still admissible as a candidate actor;
				// we just do not license the "covered trail with no entry ⇒ no cause" inference for this domain.
				Covered: false,
			})
		}
		page = next
	}
	return out, nil
}

// ---- GitLab API shapes (only the fields we read) ---------------------------------------------------

type mrRow struct {
	IID            int    `json:"iid"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	MergedBy       struct {
		Username string `json:"username"`
	} `json:"merged_by"`
}

// mergeRequests fetches one page of merged MRs and returns the rows plus the next page number (0 = last
// page), read from GitLab's X-Next-Page pagination header.
func (m *Module) mergeRequests(ctx context.Context, q url.Values) ([]mrRow, int, error) {
	body, hdr, err := m.get(ctx, "/api/v4/projects/"+m.project+"/merge_requests?"+q.Encode())
	if err != nil {
		return nil, 0, err
	}
	var rows []mrRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, 0, fmt.Errorf("gitops-mr: merge_requests unparseable: %w", err)
	}
	return rows, nextPage(hdr), nil
}

// nextPage reads GitLab's X-Next-Page pagination header; "" (last page) or a non-numeric value yields 0,
// which terminates a page loop.
func nextPage(hdr http.Header) int {
	if n := strings.TrimSpace(hdr.Get("X-Next-Page")); n != "" {
		if v, e := strconv.Atoi(n); e == nil && v > 0 {
			return v
		}
	}
	return 0
}

type diffRow struct {
	NewPath string `json:"new_path"`
	OldPath string `json:"old_path"`
}

// touchesManifest reports whether the MR changed any file under the manifest prefix. It reads a single
// (large) page of the diff; GitLab returns the full changed-file list here for typical MRs.
func (m *Module) touchesManifest(ctx context.Context, iid int) (bool, error) {
	page := 1
	for pages := 0; page > 0; pages++ {
		if pages >= m.maxPages {
			// A huge MR whose manifest file might live beyond maxPages of diff: fail closed rather than
			// silently answer "no manifest touched" (which would wrongly DROP a real declarative change).
			return false, fmt.Errorf("gitops-mr: MR %d has more than %d pages of changed files — manifest check would truncate (fail closed, not silent)", iid, m.maxPages)
		}
		body, hdr, err := m.get(ctx, fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/diffs?per_page=%d&page=%d", m.project, iid, m.pageSize, page))
		if err != nil {
			return false, err
		}
		var diffs []diffRow
		if err := json.Unmarshal(body, &diffs); err != nil {
			return false, fmt.Errorf("gitops-mr: MR %d diffs unparseable: %w", iid, err)
		}
		for _, d := range diffs {
			if strings.HasPrefix(d.NewPath, m.manifestPrefix) || strings.HasPrefix(d.OldPath, m.manifestPrefix) {
				return true, nil
			}
		}
		page = nextPage(hdr)
	}
	return false, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// ---- transport -------------------------------------------------------------------------------------

func (m *Module) get(ctx context.Context, path string) ([]byte, http.Header, error) {
	token, err := m.tokenRef.Resolve()
	if err != nil {
		return nil, nil, fmt.Errorf("gitops-mr: read-only token unresolvable (INV-13): %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, nil, fmt.Errorf("gitops-mr: GET %s → %d: %s", path, resp.StatusCode, msg)
	}
	return b, resp.Header, nil
}
