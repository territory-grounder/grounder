package gitopsmr

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

const (
	testRepoURL  = "https://gitlab.example.net/infrastructure/prod"
	testMergeSHA = "1fdd1b30c99e72f24e7bf97d0843e3c9e3bf2f39"
)

// convFakeDoer returns a canned response for the single GET a reader/poller issues, recording the request.
type convFakeDoer struct {
	status  int
	body    string
	err     error
	gotURL  string
	gotAuth string
}

func (f *convFakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotURL, f.gotAuth = req.URL.String(), req.Header.Get("Authorization")
	if f.err != nil {
		return nil, f.err
	}
	st := f.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(f.body)), Header: make(http.Header)}, nil
}

func newConvReader(t *testing.T, d *convFakeDoer) *ConvergenceReader {
	t.Helper()
	os.Setenv("TG_TEST_CONV_API", "https://api.example.net:6443")
	os.Setenv("TG_TEST_CONV_TOKEN", "sa-token-xyz")
	r, err := NewConvergenceReader(ConvergenceConfig{
		APIRef:    config.SecretRef("env:TG_TEST_CONV_API"),
		TokenRef:  config.SecretRef("env:TG_TEST_CONV_TOKEN"),
		Namespace: "argocd",
	}, WithConvergenceDoer(d))
	if err != nil {
		t.Fatalf("NewConvergenceReader: %v", err)
	}
	return r
}

// appJSON builds an Argo Application list body for a single application.
func appJSON(repo, sync, health, rev string, history ...string) string {
	var hist strings.Builder
	for i, h := range history {
		if i > 0 {
			hist.WriteString(",")
		}
		hist.WriteString(`{"revision":"` + h + `"}`)
	}
	return `{"items":[{"spec":{"source":{"repoURL":"` + repo + `"}},"status":{"sync":{"status":"` + sync +
		`","revision":"` + rev + `"},"health":{"status":"` + health + `"},"history":[` + hist.String() + `]}}]}`
}

func TestConvergedWhenSyncedHealthyAtMergeRevision(t *testing.T) {
	d := &convFakeDoer{body: appJSON(testRepoURL+".git", "Synced", "Healthy", testMergeSHA)}
	ok, reason, err := newConvReader(t, d).Converged(context.Background(), testRepoURL, testMergeSHA)
	if err != nil || !ok {
		t.Fatalf("want converged, got ok=%v reason=%q err=%v", ok, reason, err)
	}
	if !strings.Contains(d.gotURL, "/apis/argoproj.io/v1alpha1/namespaces/argocd/applications") {
		t.Errorf("wrong API path: %s", d.gotURL)
	}
	if d.gotAuth != "Bearer sa-token-xyz" {
		t.Errorf("wrong auth header: %q", d.gotAuth)
	}
}

func TestConvergedViaDeployHistory(t *testing.T) {
	// Argo synced a later commit but the merge SHA is in its deploy history — the merge still landed.
	d := &convFakeDoer{body: appJSON(testRepoURL, "Synced", "Healthy", "abcabc0laterlaterlaterlaterlaterlater00", testMergeSHA)}
	if ok, _, err := newConvReader(t, d).Converged(context.Background(), testRepoURL, testMergeSHA); err != nil || !ok {
		t.Fatalf("want converged via history, got ok=%v err=%v", ok, err)
	}
}

func TestNotConvergedWhenOutOfSyncOrUnhealthy(t *testing.T) {
	for _, tc := range []struct{ sync, health string }{
		{"OutOfSync", "Healthy"}, {"Synced", "Progressing"}, {"Synced", "Degraded"},
	} {
		d := &convFakeDoer{body: appJSON(testRepoURL, tc.sync, tc.health, testMergeSHA)}
		if ok, _, err := newConvReader(t, d).Converged(context.Background(), testRepoURL, testMergeSHA); err != nil || ok {
			t.Errorf("sync=%s health=%s: want not-converged nil-err, got ok=%v err=%v", tc.sync, tc.health, ok, err)
		}
	}
}

func TestNotConvergedOnRevisionMismatch(t *testing.T) {
	d := &convFakeDoer{body: appJSON(testRepoURL, "Synced", "Healthy", "0000000different000000000000000000000000")}
	if ok, _, err := newConvReader(t, d).Converged(context.Background(), testRepoURL, testMergeSHA); err != nil || ok {
		t.Errorf("want not-converged (rev mismatch), got ok=%v err=%v", ok, err)
	}
}

func TestRepoMismatchIsNotConverged(t *testing.T) {
	d := &convFakeDoer{body: appJSON("https://gitlab.example.net/other/repo", "Synced", "Healthy", testMergeSHA)}
	ok, reason, err := newConvReader(t, d).Converged(context.Background(), testRepoURL, testMergeSHA)
	if err != nil || ok {
		t.Errorf("want not-converged (repo mismatch), got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(reason, "no Argo Application syncs") {
		t.Errorf("reason should note no matching app, got %q", reason)
	}
}

func TestReadErrorIsNonTerminal(t *testing.T) {
	if ok, _, err := newConvReader(t, &convFakeDoer{status: 500, body: "boom"}).Converged(context.Background(), testRepoURL, testMergeSHA); ok || err == nil {
		t.Errorf("a 500 must be a non-terminal error (leaves the record pending), got ok=%v err=%v", ok, err)
	}
}

func TestCanonicalRepoNormalises(t *testing.T) {
	base := canonicalRepo("https://h/g/p")
	for _, v := range []string{"https://h/g/p.git", "https://h/g/p/", "HTTPS://H/g/p", "https://h/g/p.git/"} {
		if canonicalRepo(v) != base {
			t.Errorf("canonicalRepo(%q)=%q, want %q", v, canonicalRepo(v), base)
		}
	}
}

// --- poller integration: merge is authorized to success ONLY through the convergence reader ---

func convAllowlist() RepoAllowlist {
	os.Setenv("TG_TEST_MR_TOKEN", "mr-pat")
	return RepoAllowlist{"7": {
		BaseURL: "https://gitlab.example.net", ProjectPath: "infrastructure/prod",
		OpClass: "k8s-set-replicas", TokenRef: config.SecretRef("env:TG_TEST_MR_TOKEN"),
	}}
}

func TestPollerMergedConvergedIsSuccessful(t *testing.T) {
	mrDoer := &convFakeDoer{body: `{"state":"merged","merge_commit_sha":"` + testMergeSHA + `"}`}
	appDoer := &convFakeDoer{body: appJSON(testRepoURL, "Synced", "Healthy", testMergeSHA)}
	p := NewPoller(convAllowlist(), WithPollerHTTPClient(mrDoer), WithConvergenceReader(newConvReader(t, appDoer)))
	if got, err := p.PollJob(context.Background(), "7!533"); err != nil || got != "successful" {
		t.Fatalf("merged+converged: want successful, got %q err=%v", got, err)
	}
}

func TestPollerMergedNoReaderStaysRunning(t *testing.T) {
	mrDoer := &convFakeDoer{body: `{"state":"merged","merge_commit_sha":"` + testMergeSHA + `"}`}
	p := NewPoller(convAllowlist(), WithPollerHTTPClient(mrDoer))
	if got, err := p.PollJob(context.Background(), "7!533"); err != nil || got != "running" {
		t.Fatalf("merged, no reader: want running (rides to bound), got %q err=%v", got, err)
	}
}

func TestPollerMergedNotConvergedStaysRunning(t *testing.T) {
	mrDoer := &convFakeDoer{body: `{"state":"merged","merge_commit_sha":"` + testMergeSHA + `"}`}
	appDoer := &convFakeDoer{body: appJSON(testRepoURL, "OutOfSync", "Healthy", testMergeSHA)}
	p := NewPoller(convAllowlist(), WithPollerHTTPClient(mrDoer), WithConvergenceReader(newConvReader(t, appDoer)))
	if got, err := p.PollJob(context.Background(), "7!533"); err != nil || got != "running" {
		t.Fatalf("merged, not converged: want running, got %q err=%v", got, err)
	}
}
