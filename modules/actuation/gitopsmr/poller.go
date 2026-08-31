package gitopsmr

// The gitops-mr SENSOR half (TG-122 slice 2, spec/017 REQ-1720): a read-only poller that maps an opened MR's
// lifecycle onto the deferred-verify channel's job-status vocabulary. It reads the SAME per-repo allowlist the
// actuator writes through (RepoAllowlist — per-repo BaseURL + api-scoped TokenRef, NL vs GR instances), so a
// handle whose repo is not operator-declared cannot even be polled.
//
// THE HONEST STATUS MAP — and what is deliberately withheld. The design's bar for success is
// RECONCILE-CONVERGENCE observed live ("Apply complete ≠ the live cluster changed"): not MR-merged, not
// apply-returned. No convergence read surface exists in this codebase yet (no Argo/Atlantis/k8s client), so
// this poller NEVER emits JobSuccessful:
//
//	opened / locked           → JobRunning  (proposal pending human review)
//	merged                    → JobRunning  (apply/reconcile UNOBSERVED — convergence reader is a later slice)
//	closed (unmerged)         → JobFailed   (the proposal was rejected; a durable negative signal)
//
// A merged MR therefore stays pending until the operator bound elapses and the record resolves `unverified`
// (REQ-1711) — fail-safe and visible, never a fabricated success: an unverified launch counts as NO clean run
// and feeds no graduation. When a convergence reader lands, IT (not the merge state) authorizes JobSuccessful.
// A transient read error, an unknown state, an unparseable handle, or an un-allowlisted repo returns an error,
// which leaves the deferred verify pending for retry — the channel never treats a read failure as terminal.

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
)

// PollerDoer is the injected HTTP seam (the actorevidence module's Doer shape) so the oracles run without a
// GitLab instance.
type PollerDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Poller reads MR lifecycle state for handles the actuator minted ("<repoID>!<iid>", OpenedMR.Handle). It is
// READ-ONLY by construction: one GET per poll, no write method exists on the type.
type Poller struct {
	allowlist   RepoAllowlist
	http        PollerDoer
	timeout     time.Duration
	convergence *ConvergenceReader // TG-555: nil ⇒ a merged MR is not observed as success (rides to the bound)
}

// PollerOption configures a Poller.
type PollerOption func(*Poller)

// WithPollerHTTPClient injects the HTTP client (tests: a fake Doer).
func WithPollerHTTPClient(d PollerDoer) PollerOption {
	return func(p *Poller) {
		if d != nil {
			p.http = d
		}
	}
}

// WithConvergenceReader wires the TG-555 reconcile-convergence reader (convergence.go, spec/017 REQ-1720): with
// it, a MERGED MR is polled to `successful` ONLY once the target Argo Application is Synced+Healthy at the merge
// commit; without it, merged stays `running` (rides to the bound, `unverified`). The poller never fabricates
// success.
func WithConvergenceReader(r *ConvergenceReader) PollerOption {
	return func(p *Poller) {
		if r != nil {
			p.convergence = r
		}
	}
}

// NewPoller builds the sensor over the operator's repo allowlist. An EMPTY allowlist is permitted (the poller
// then errors on every poll — the record stays pending, fail-safe): the allowlist is config, and config
// absence must degrade to "cannot observe", never to a fabricated terminal.
func NewPoller(allowlist RepoAllowlist, opts ...PollerOption) *Poller {
	p := &Poller{
		allowlist: allowlist,
		http:      &http.Client{Timeout: 12 * time.Second},
		timeout:   12 * time.Second,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// mrState is the narrow read this sensor needs from GET /merge_requests/{iid}.
type mrState struct {
	State          string `json:"state"`            // opened | locked | merged | closed
	MergedAt       string `json:"merged_at"`        // informational
	MergeCommitSHA string `json:"merge_commit_sha"` // the resulting commit on the target branch (convergence key)
}

// PollJob implements regime.JobPoller for gitops-mr handles. The returned strings are regime.JobStatus slugs
// (running / failed — never successful, see the package doc); errors leave the deferred verify pending.
func (p *Poller) PollJob(ctx context.Context, handle string) (string, error) {
	repoID, iid, err := SplitHandle(handle)
	if err != nil {
		return "", err
	}
	policy, ok := p.allowlist[repoID]
	if !ok {
		return "", fmt.Errorf("gitops-mr poll: repo %q is not on the operator allowlist — cannot observe its MRs", repoID)
	}
	token, err := policy.TokenRef.Resolve()
	if err != nil {
		return "", fmt.Errorf("gitops-mr poll: token for repo %q unresolvable (INV-13): %w", repoID, err)
	}
	u := strings.TrimRight(policy.BaseURL, "/") + "/api/v4/projects/" + url.PathEscape(policy.ProjectPath) +
		"/merge_requests/" + strconv.Itoa(iid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", fmt.Errorf("gitops-mr poll: GET %s → %d: %s", u, resp.StatusCode, msg)
	}
	var mr mrState
	if err := json.Unmarshal(b, &mr); err != nil {
		return "", fmt.Errorf("gitops-mr poll: unparseable MR state: %w", err)
	}
	switch mr.State {
	case "opened", "locked":
		return "running", nil // proposal pending human review — non-terminal
	case "merged":
		// Merged is a PREDICTION, not success. Without a convergence reader the reconcile is unobserved, so the
		// record stays running and the bound resolves it `unverified` (fail-safe, REQ-1711). WITH the TG-555
		// reader (spec/017 REQ-1720), success is authorized ONLY once the target Argo Application is
		// Synced+Healthy at the merge commit — convergence observed live, never merge state alone.
		if p.convergence == nil {
			return "running", nil
		}
		repoURL := strings.TrimRight(policy.BaseURL, "/") + "/" + strings.TrimLeft(policy.ProjectPath, "/")
		converged, _, cerr := p.convergence.Converged(ctx, repoURL, mr.MergeCommitSHA)
		if cerr != nil {
			return "", fmt.Errorf("gitops-mr poll: convergence read for %s: %w", handle, cerr) // non-terminal: retry
		}
		if converged {
			return "successful", nil // reconcile-convergence observed — the verified outcome (REQ-1720)
		}
		return "running", nil // merged but not yet converged — stays pending until the bound (then unverified)
	case "closed":
		return "failed", nil // rejected unmerged — a durable negative signal, never a clean run
	default:
		return "", fmt.Errorf("gitops-mr poll: unknown MR state %q for %s — refusing to map it", mr.State, handle)
	}
}

// SplitHandle parses the actuator's launch handle "<repoID>!<iid>" (OpenedMR.Handle). The split is on the
// LAST '!' so a repo id containing '!' cannot smuggle a different iid.
func SplitHandle(handle string) (repoID string, iid int, err error) {
	i := strings.LastIndex(handle, "!")
	if i <= 0 || i == len(handle)-1 {
		return "", 0, fmt.Errorf("gitops-mr poll: malformed handle %q (want <repoID>!<iid>)", handle)
	}
	n, err := strconv.Atoi(handle[i+1:])
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("gitops-mr poll: malformed MR iid in handle %q", handle)
	}
	return handle[:i], n, nil
}
