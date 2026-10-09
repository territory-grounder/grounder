package gitopsmr

// TG-122 slice 2 — the sensor's status map, with the load-bearing negative: MERGED IS NOT SUCCESS. A poller
// that mapped merged→successful would let an unreconciled (or Argo-reverted) change graduate an op-class —
// the exact graduation-theater the async refusal existed to prevent, reintroduced one layer down.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// fakeDoer serves scripted responses keyed by URL substring and records the requests.
type fakeDoer struct {
	status int
	body   string
	err    error
	gotURL string
	gotTok string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.gotURL = req.URL.String()
	f.gotTok = req.Header.Get("PRIVATE-TOKEN")
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func pollerFixture(t *testing.T, d *fakeDoer) *Poller {
	t.Helper()
	t.Setenv("TG_TEST_GITOPS_TOKEN", "tok-123")
	return NewPoller(RepoAllowlist{
		"infra-nl": {
			BaseURL:     "https://git.example-int.lan",
			ProjectPath: "infrastructure/site/production",
			TokenRef:    config.SecretRef("env:TG_TEST_GITOPS_TOKEN"),
			OpClass:     "gitops-mr-propose",
		},
	}, WithPollerHTTPClient(d))
}

func TestPollJobStatusMap(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"opened", "running"},
		{"locked", "running"},
		// THE LOAD-BEARING ROW: merged stays running — apply/reconcile is unobserved, so the record must
		// ride to its bound and resolve `unverified`, never a fabricated success.
		// KILLING MUTATION: map merged→"successful" and this reddens.
		{"merged", "running"},
		{"closed", "failed"},
	}
	for _, c := range cases {
		d := &fakeDoer{status: 200, body: fmt.Sprintf(`{"state":%q}`, c.state)}
		got, err := pollerFixture(t, d).PollJob(context.Background(), "infra-nl!41")
		if err != nil {
			t.Fatalf("state %q: %v", c.state, err)
		}
		if got != c.want {
			t.Errorf("state %q → %q, want %q", c.state, got, c.want)
		}
	}
	// The request shape: project path URL-escaped, iid appended, per-repo token presented.
	d := &fakeDoer{status: 200, body: `{"state":"opened"}`}
	if _, err := pollerFixture(t, d).PollJob(context.Background(), "infra-nl!41"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.gotURL, "/api/v4/projects/infrastructure%2Fsite%2Fproduction/merge_requests/41") {
		t.Errorf("poll URL = %q, want the escaped project path + iid", d.gotURL)
	}
	if d.gotTok != "tok-123" {
		t.Errorf("token = %q, want the repo policy's resolved token", d.gotTok)
	}
}

// Every unobservable shape is an ERROR (the deferred verify stays pending and retries) — never a status.
func TestPollJobFailsClosedOnEveryUnobservableShape(t *testing.T) {
	cases := []struct {
		name   string
		handle string
		doer   *fakeDoer
	}{
		{"un-allowlisted repo", "rogue!5", &fakeDoer{status: 200, body: `{"state":"opened"}`}},
		{"malformed handle", "no-iid", &fakeDoer{status: 200, body: `{"state":"opened"}`}},
		{"non-numeric iid", "infra-nl!abc", &fakeDoer{status: 200, body: `{"state":"opened"}`}},
		{"http error", "infra-nl!41", &fakeDoer{err: fmt.Errorf("conn refused")}},
		{"http 404", "infra-nl!41", &fakeDoer{status: 404, body: `{"message":"404 Not Found"}`}},
		{"unparseable body", "infra-nl!41", &fakeDoer{status: 200, body: `not-json`}},
		{"unknown state", "infra-nl!41", &fakeDoer{status: 200, body: `{"state":"weird"}`}},
	}
	for _, c := range cases {
		if got, err := pollerFixture(t, c.doer).PollJob(context.Background(), c.handle); err == nil {
			t.Errorf("%s: got status %q, want an error (pending, retried) — a fabricated status here becomes a terminal verdict", c.name, got)
		}
	}
	// Unresolvable token ref: same direction.
	p := NewPoller(RepoAllowlist{"r": {BaseURL: "https://x", ProjectPath: "a/b",
		TokenRef: config.SecretRef("env:TG_TEST_GITOPS_ABSENT"), OpClass: "c"}},
		WithPollerHTTPClient(&fakeDoer{status: 200, body: `{"state":"opened"}`}))
	if _, err := p.PollJob(context.Background(), "r!1"); err == nil {
		t.Error("an unresolvable token ref must error, not poll unauthenticated")
	}
}

func TestSplitHandle(t *testing.T) {
	if repo, iid, err := SplitHandle("infra-nl!41"); err != nil || repo != "infra-nl" || iid != 41 {
		t.Fatalf("SplitHandle = %q %d %v", repo, iid, err)
	}
	// Last-'!' split: a repo id containing '!' cannot displace the iid.
	if repo, iid, err := SplitHandle("we!rd!7"); err != nil || repo != "we!rd" || iid != 7 {
		t.Fatalf("SplitHandle last-split = %q %d %v", repo, iid, err)
	}
	for _, bad := range []string{"", "!", "x!", "!5", "x!0", "x!-3"} {
		if _, _, err := SplitHandle(bad); err == nil {
			t.Errorf("SplitHandle(%q) must refuse", bad)
		}
	}
}
