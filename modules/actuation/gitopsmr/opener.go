package gitopsmr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// httpOpener is the concrete GitLab Opener (TG-122 slice 4). It performs the effect's EXACTLY TWO write REST
// calls — an atomic branch+commit via the Commits API, then create-merge-request — and STOPS. It never merges,
// never comments `atlantis apply`, never uses the Files API, never touches the cluster. The api-scoped PAT is
// resolved from the repo policy's TokenRef at write time (INV-13) — AFTER the actuator has already admitted,
// op-class-bound, secret-guarded, and (via the interceptor) policy-authorized and mode-permitted the action —
// and is sent as the PRIVATE-TOKEN header, never logged, never placed in an MR.
type httpOpener struct {
	http *http.Client
}

// NewOpener builds the GitLab Opener over a bounded-timeout client (the two write calls must not hang the
// deferred-verify launch). A nil client gets a 20s-timeout default. Returned as the Opener interface so the
// actuator wiring (Slice 4) depends only on the seam.
func NewOpener(client *http.Client) Opener {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &httpOpener{http: client}
}

// commitAction is one GitLab Commits-API action[]. The Opener only ever emits "update" for a rendered file —
// the Renderer returns the FULL new content of an EXISTING repo file (a single-field edit), so create/delete/
// move never arise; an unexpected shape would surface as a GitLab 400, refused, not silently retried.
type commitAction struct {
	Action   string `json:"action"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type commitRequest struct {
	Branch        string         `json:"branch"`
	StartBranch   string         `json:"start_branch"`
	CommitMessage string         `json:"commit_message"`
	Actions       []commitAction `json:"actions"`
}

type mrRequest struct {
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Title        string `json:"title"`
	Description  string `json:"description"`
}

type mrCreateResponse struct {
	IID    int    `json:"iid"`
	WebURL string `json:"web_url"`
}

// OpenMR performs the two write calls and returns the async handle the deferred-verify channel polls. It sets
// IID / Branch / URL; the Actuator stamps RepoID and Handle (it holds the allowlist key). It NEVER merges the
// MR — an open is a PREDICTION, verified later by reconcile-convergence, not a declared success.
func (o *httpOpener) OpenMR(ctx context.Context, pol RepoPolicy, branch, title, body string, files map[string][]byte) (OpenedMR, error) {
	if len(files) == 0 {
		return OpenedMR{}, ErrNoEdits
	}
	token, err := pol.TokenRef.Resolve()
	if err != nil {
		return OpenedMR{}, fmt.Errorf("gitopsmr open: token for %q unresolvable (INV-13): %w", pol.ProjectPath, err)
	}
	projBase := strings.TrimRight(pol.BaseURL, "/") + "/api/v4/projects/" + url.PathEscape(pol.ProjectPath)
	target := pol.TargetBranch
	if target == "" {
		target = "main"
	}

	// (1) atomic branch + commit off the target branch. Files are emitted in a deterministic order so the
	// request body (and its test) is stable and the commit is reproducible.
	actions := make([]commitAction, 0, len(files))
	for _, path := range sortedKeys(files) {
		actions = append(actions, commitAction{Action: "update", FilePath: path, Content: string(files[path])})
	}
	commitBody, err := json.Marshal(commitRequest{Branch: branch, StartBranch: target, CommitMessage: title, Actions: actions})
	if err != nil {
		return OpenedMR{}, err
	}
	if err := o.post(ctx, projBase+"/repository/commits", token, commitBody, nil); err != nil {
		return OpenedMR{}, fmt.Errorf("gitopsmr open: create branch+commit: %w", err)
	}

	// (2) create the merge request. STOP here — never merge, never `atlantis apply`.
	mrBody, err := json.Marshal(mrRequest{SourceBranch: branch, TargetBranch: target, Title: title, Description: body})
	if err != nil {
		return OpenedMR{}, err
	}
	var mr mrCreateResponse
	if err := o.post(ctx, projBase+"/merge_requests", token, mrBody, &mr); err != nil {
		return OpenedMR{}, fmt.Errorf("gitopsmr open: create merge request: %w", err)
	}
	if mr.IID == 0 {
		return OpenedMR{}, fmt.Errorf("gitopsmr open: merge request created but GitLab returned no iid")
	}
	return OpenedMR{IID: mr.IID, Branch: branch, URL: mr.WebURL}, nil
}

// post issues one authenticated JSON POST and, when out is non-nil, decodes the response into it. A non-2xx is
// an error carrying the (secret-free) GitLab message, bounded to 200 bytes — the same shape the poller uses.
func (o *httpOpener) post(ctx context.Context, endpoint, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("POST %s → %d: %s", endpoint, resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("decode %s response: %w", endpoint, err)
		}
	}
	return nil
}

// sortedKeys returns the file paths in deterministic order.
func sortedKeys(files map[string][]byte) []string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MakeHandle builds the async handle `<repoID>!<iid>` (the awx-job job-id shape), the inverse of SplitHandle.
func MakeHandle(repoID string, iid int) string {
	return fmt.Sprintf("%s!%d", repoID, iid)
}
