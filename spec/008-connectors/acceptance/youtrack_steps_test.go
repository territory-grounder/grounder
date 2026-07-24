package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cucumber/godog"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/tracker/youtrack"
)

// The YouTrack tracker module (REQ-804) — the tracker family lead — binds its scenario here, proving a
// SECOND surface works end to end through the registry + a real adapter against a fake backend.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerYoutrackSteps)
}

// ytFakeDoer is a fake YouTrack backend: it returns an In-Progress issue for GET and records POSTs.
type ytFakeDoer struct {
	posts []string
}

func (f *ytFakeDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost {
		f.posts = append(f.posts, req.URL.Path)
	}
	resp := ""
	if req.Method == http.MethodGet {
		resp = `{"idReadable":"TG-5","summary":"Device Down","customFields":[{"name":"State","value":{"name":"In Progress"}}]}`
	}
	if req.Body != nil {
		_, _ = io.ReadAll(req.Body)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

type youtrackWorld struct {
	reg   *modules.Registry
	doer  *ytFakeDoer
	issue tracker.Issue
	err   error
}

func registerYoutrackSteps(sc *godog.ScenarioContext) {
	w := &youtrackWorld{}

	sc.Step(`^the YouTrack tracker module is registered and enabled$`, func() error {
		_ = os.Setenv("TG_YT_ACCEPT_TOKEN", "tok")
		w.reg = modules.NewRegistry()
		w.doer = &ytFakeDoer{}
		mod := youtrack.New("https://yt.example", config.SecretRef("env:TG_YT_ACCEPT_TOKEN"), youtrack.WithHTTPClient(w.doer))
		return w.reg.Register(modules.Registration{
			Surface:    modules.SurfaceTracker,
			SourceType: youtrack.SourceType,
			Capability: "tracker.youtrack",
			Enabled:    true,
			Adapter:    mod,
		})
	})

	sc.Step(`^an issue transitions to In Progress$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceTracker, youtrack.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		trk, ok := reg.Adapter.(tracker.Tracker)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/tracker.Tracker")
		}
		if w.issue, err = trk.Open(context.Background(), "TG-5"); err != nil { // trigger: open the entry issue
			w.err = err
			return nil
		}
		if err := trk.TransitionState(context.Background(), "TG-5", tracker.StateInProgress); err != nil {
			w.err = err
			return nil
		}
		if err := trk.Comment(context.Background(), "TG-5", "session opened; auto-triage in progress"); err != nil { // sink
			w.err = err
			return nil
		}
		return nil
	})

	sc.Step(`^a session is correlated by the issue id and the terminal audit comment is posted as the sink$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the tracker verbs must succeed: %w", w.err)
		}
		if w.issue.ID != "TG-5" {
			return fmt.Errorf("the session must be correlated by the issue id, got %q", w.issue.ID)
		}
		posted := false
		for _, p := range w.doer.posts {
			if strings.HasSuffix(p, "/api/issues/TG-5/comments") {
				posted = true
			}
		}
		if !posted {
			return fmt.Errorf("the terminal audit comment must be posted as the sink; posts=%v", w.doer.posts)
		}
		return nil
	})
}
