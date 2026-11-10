package proxmox

// TG-663 (G4A F4): the Proxmox leaf classifies its Exec failures by WHERE they happen. A mutating POST that fails
// WITHOUT an HTTP status reply (a timeout, a reset) may have reached the API — INDETERMINATE. A status reply is
// the API refusing before any task started — a plain error. A task the API ACCEPTED (a UPID) whose status poll
// fails — INDETERMINATE: the mutation is in flight unobserved.
//
// KILLING MUTATION (executed 2026-11-07): in Exec, return the pollTask error unwrapped (drop the
// MarkIndeterminate on the accepted-task path). RED: TestProxmoxAcceptedTaskWhosePollFailsIsIndeterminate.
// Restored from a copy, green.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/safety"
)

// flakyDoer is oneLXC with injectable failures on the status POST (transport error or an HTTP status) and on
// the task-status GET (transport error).
type flakyDoer struct {
	fakeDoer
	postErr    error
	postStatus int
	postBody   string
	pollErr    error
}

func (f *flakyDoer) Do(req *http.Request) (*http.Response, error) {
	p := req.URL.Path
	isPost := strings.Contains(p, "/status/") && !strings.Contains(p, "/tasks/") && req.Method == http.MethodPost
	if isPost && f.postErr != nil {
		f.seenURLs = append(f.seenURLs, p)
		return nil, f.postErr
	}
	if isPost && f.postStatus != 0 {
		f.seenURLs = append(f.seenURLs, p)
		return canned(f.postStatus, f.postBody), nil
	}
	if strings.Contains(p, "/tasks/") && f.pollErr != nil {
		f.seenURLs = append(f.seenURLs, p)
		return nil, f.pollErr
	}
	return f.fakeDoer.Do(req)
}

func flaky() *flakyDoer { return &flakyDoer{fakeDoer: *oneLXC()} }

// flakyModule is enabledModule over the flaky transport (mutation ON, test-only chokepoint).
func flakyModule(t *testing.T, f *flakyDoer) *Module {
	t.Helper()
	return New(testBaseURL, tokenRef(t), WithHTTPClient(f), WithMutation(safety.NewActuatingChokepoint(), []string{"web"}))
}

func TestProxmoxPostWithoutAReplyIsIndeterminate(t *testing.T) {
	f := flaky()
	f.postErr = errors.New("Post \"https://pve/api2/json/...\": net/http: timeout awaiting response headers")
	_, err := flakyModule(t, f).Exec(context.Background(), []string{"start", "web"}, nil)
	if err == nil || !errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("a mutating POST that got no reply may have reached the API — must be INDETERMINATE, got %v", err)
	}
	if !f.sawStatusPost() {
		t.Fatal("precondition: the POST must have been attempted")
	}
}

func TestProxmoxStatusReplyIsARefusalNotIndeterminate(t *testing.T) {
	f := flaky()
	f.postStatus, f.postBody = 500, `{"errors":{},"data":null,"message":"storage not available"}`
	_, err := flakyModule(t, f).Exec(context.Background(), []string{"start", "web"}, nil)
	if err == nil || errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("an HTTP status reply means the API answered before any task started — a plain refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("the status must be named, got %v", err)
	}
}

func TestProxmoxAcceptedTaskWhosePollFailsIsIndeterminate(t *testing.T) {
	f := flaky()
	f.pollErr = errors.New("Get \"https://pve/api2/json/...\": read tcp: connection reset by peer")
	_, err := flakyModule(t, f).Exec(context.Background(), []string{"start", "web"}, nil)
	if err == nil || !errors.Is(err, actuation.ErrIndeterminate) {
		t.Fatalf("a task the API accepted (UPID) whose poll fails is in flight unobserved — INDETERMINATE, got %v", err)
	}
	if !strings.Contains(err.Error(), "accepted") {
		t.Fatalf("the error must say the task was accepted, got %v", err)
	}
}

// POSITIVE CONTROL: the happy path is unchanged — a task that finishes OK is an executed Result.
func TestProxmoxHappyPathIsUnchangedByTheClassification(t *testing.T) {
	res, err := enabledModule(t, oneLXC()).Exec(context.Background(), []string{"start", "web"}, nil)
	if err != nil || res.ExitCode != 0 || res.NoOp {
		t.Fatalf("want a clean executed result, got %+v err=%v", res, err)
	}
}
