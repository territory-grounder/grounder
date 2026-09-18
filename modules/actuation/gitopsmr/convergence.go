package gitopsmr

// convergence.go — TG-555: the reconcile-convergence reader for the gitops-mr lane (spec/017 REQ-1720). Merge
// is a PREDICTION; convergence is the verified outcome. For an Argo CD target, "converged" = the Application
// that syncs the target repo is Synced + Healthy AND has deployed the merge commit (its current sync revision,
// or a revision in its deploy history, is the merge SHA). Only then does the deferred-verify channel authorize
// JobSuccessful — never merge state alone (see poller.go). The check is SOUND, not complete: if Argo synced a
// later commit that superseded the merge without recording the merge SHA, this stays "not converged" and the
// record resolves `unverified` at its bound — fail-safe and visible, never a fabricated success.
//
// DISTROLESS-SAFE (INV-02): net/http only — a Bearer-token GET of the Argo Application CRD over a CA-pinned
// client (the SAME custom-transport idiom core/seal, core/credential/dyndb, and sshca use for an internal,
// private-CA backend). The read-only ServiceAccount token, the cluster CA, and the API URL are resolved from
// config.SecretRefs at build/read time (INV-13) — never literals; the SA can get/list applications only and
// mutates nothing.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// ConvergenceConfig is the read-only k8s access the reader needs, entirely indirect (SecretRefs) so no
// endpoint, token, or CA is ever a literal (INV-13).
type ConvergenceConfig struct {
	APIRef    config.SecretRef // k8s API server base URL (https://host:6443)
	TokenRef  config.SecretRef // read-only ServiceAccount bearer token
	CARef     config.SecretRef // cluster CA (PEM)
	Namespace string           // namespace Argo Applications live in (default "argocd")
}

// ConvergenceReader reads Argo CD Application status to decide whether a merged MR reconciled onto the cluster.
// No write method exists on the type — it can only observe, mirroring the Poller's read-only posture.
type ConvergenceReader struct {
	cfg  ConvergenceConfig
	http PollerDoer // injectable Doer seam (tests supply a fake; production a CA-pinned client)
}

// NewConvergenceReader builds the reader. It resolves the CARef ONCE and builds a CA-pinned client (the
// core/seal · dyndb · sshca idiom for an internal private-CA backend); the token is resolved per-read (INV-13).
// A nil/empty APIRef or TokenRef, or a CA that does not parse, fails closed with an error — the caller then
// leaves the lane convergence-blind (a merged MR rides to the bound, `unverified`), never fabricating success.
func NewConvergenceReader(cfg ConvergenceConfig, opts ...ConvergenceOption) (*ConvergenceReader, error) {
	if strings.TrimSpace(string(cfg.APIRef)) == "" || strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, fmt.Errorf("gitopsmr convergence: APIRef and TokenRef are required")
	}
	if strings.TrimSpace(cfg.Namespace) == "" {
		cfg.Namespace = "argocd"
	}
	r := &ConvergenceReader{cfg: cfg}
	for _, o := range opts {
		o(r)
	}
	if r.http == nil {
		caPEM, err := cfg.CARef.Resolve()
		if err != nil {
			return nil, fmt.Errorf("gitopsmr convergence: CA ref unresolvable (INV-13): %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, fmt.Errorf("gitopsmr convergence: cluster CA PEM did not parse")
		}
		r.http = &http.Client{
			Timeout:   12 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		}
	}
	return r, nil
}

// ConvergenceOption configures a ConvergenceReader.
type ConvergenceOption func(*ConvergenceReader)

// WithConvergenceDoer injects the HTTP client seam (tests: a fake Doer; skips the CA-pinned client build).
func WithConvergenceDoer(d PollerDoer) ConvergenceOption {
	return func(r *ConvergenceReader) {
		if d != nil {
			r.http = d
		}
	}
}

// argoApp is the narrow read of one Argo CD Application the reader needs.
type argoApp struct {
	Spec struct {
		Source struct {
			RepoURL string `json:"repoURL"`
		} `json:"source"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		History []struct {
			Revision string `json:"revision"`
		} `json:"history"`
	} `json:"status"`
}

type argoAppList struct {
	Items []argoApp `json:"items"`
}

// Converged reports whether an Argo CD Application syncing repoURL has reconciled mergeSHA onto the cluster:
// Sync=Synced AND Health=Healthy AND mergeSHA is the current or a historical deployed revision. It returns
// (false, reason, nil) when no matching Application has converged yet (the record stays pending until its bound
// — fail-safe, never a fabricated success) and (false, "", err) on a read error (also non-terminal: the
// channel retries and, failing that, resolves `unverified`).
func (r *ConvergenceReader) Converged(ctx context.Context, repoURL, mergeSHA string) (bool, string, error) {
	if strings.TrimSpace(mergeSHA) == "" {
		return false, "", fmt.Errorf("gitopsmr convergence: empty merge revision")
	}
	apiBase, err := r.cfg.APIRef.Resolve()
	if err != nil {
		return false, "", fmt.Errorf("gitopsmr convergence: api ref (INV-13): %w", err)
	}
	token, err := r.cfg.TokenRef.Resolve()
	if err != nil {
		return false, "", fmt.Errorf("gitopsmr convergence: token ref (INV-13): %w", err)
	}
	endpoint := strings.TrimRight(apiBase, "/") + "/apis/argoproj.io/v1alpha1/namespaces/" +
		url.PathEscape(r.cfg.Namespace) + "/applications"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false, "", err
	}
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return false, "", fmt.Errorf("gitopsmr convergence: GET applications → %d: %s", resp.StatusCode, msg)
	}
	var list argoAppList
	if err := json.Unmarshal(b, &list); err != nil {
		return false, "", fmt.Errorf("gitopsmr convergence: decode applications: %w", err)
	}
	want := canonicalRepo(repoURL)
	sawRepo := false
	for _, a := range list.Items {
		if canonicalRepo(a.Spec.Source.RepoURL) != want {
			continue
		}
		sawRepo = true
		if !strings.EqualFold(a.Status.Sync.Status, "Synced") || !strings.EqualFold(a.Status.Health.Status, "Healthy") {
			continue
		}
		if revisionDeployed(a, mergeSHA) {
			return true, fmt.Sprintf("Argo Application Synced+Healthy at merge %s", shortSHA(mergeSHA)), nil
		}
	}
	if !sawRepo {
		return false, fmt.Sprintf("no Argo Application syncs %s in namespace %s", want, r.cfg.Namespace), nil
	}
	return false, fmt.Sprintf("Argo Application not yet Synced+Healthy at merge %s", shortSHA(mergeSHA)), nil
}

// revisionDeployed reports whether mergeSHA is the Application's current sync revision or appears in its deploy
// history (Argo may have synced a later commit that superseded the merge; the merge still landed if recorded).
func revisionDeployed(a argoApp, mergeSHA string) bool {
	if revEqual(a.Status.Sync.Revision, mergeSHA) {
		return true
	}
	for _, h := range a.Status.History {
		if revEqual(h.Revision, mergeSHA) {
			return true
		}
	}
	return false
}

// revEqual compares two git revisions, allowing an abbreviated one to prefix the full (Argo records full SHAs;
// a caller may hold an abbreviated merge SHA). Both must be non-empty and at least 7 hex chars to match, so a
// stray empty/short field never collides.
func revEqual(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// canonicalRepo normalises a git repo URL for comparison: lowercased, scheme/userinfo dropped, and a trailing
// ".git" / slash removed — so https://host/g/p.git and https://host/g/p compare equal.
func canonicalRepo(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git"), "/")
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
