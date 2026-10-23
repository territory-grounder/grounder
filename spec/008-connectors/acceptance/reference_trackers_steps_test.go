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
	githubissues "github.com/territory-grounder/grounder/modules/tracker/github-issues"
	"github.com/territory-grounder/grounder/modules/tracker/jira"
	"github.com/territory-grounder/grounder/modules/tracker/servicenow"
)

// The three REQ-805 reference trackers (Jira, GitHub Issues, ServiceNow) share identical When/Then step
// text, so their steps are registered ONCE here (godog rejects duplicate step definitions). Each Given
// wires its vendor module behind the SAME tracker interface against a fake backend; the shared When/Then
// prove each satisfies the four-verb contract vendor-agnostically.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerReferenceTrackerSteps)
}

// refTrackerDoer is a shared fake backend: a canned issue for GET, 200 for mutations, recording the
// non-GET (state-transition + comment) request lines so the oracle can prove they happened.
type refTrackerDoer struct {
	getRet    string
	mutations []string
}

func (f *refTrackerDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.ReadAll(req.Body)
	}
	resp := ""
	if req.Method == http.MethodGet {
		resp = f.getRet
	} else {
		f.mutations = append(f.mutations, req.Method+" "+req.URL.Path)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

type refTrackerWorld struct {
	reg    *modules.Registry
	fake   *refTrackerDoer
	src    string
	testID string
	issue  tracker.Issue
	err    error
}

func registerReferenceTrackerSteps(sc *godog.ScenarioContext) {
	w := &refTrackerWorld{}
	_ = os.Setenv("TG_REFTRK_TOKEN", "tok")
	tokenRef := config.SecretRef("env:TG_REFTRK_TOKEN")

	register := func(src string, adapter any) error {
		w.reg = modules.NewRegistry()
		w.src = src
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceTracker, SourceType: src, Capability: "tracker." + src, Enabled: true, Adapter: adapter,
		})
	}

	sc.Step(`^the Jira reference tracker module is registered$`, func() error {
		w.fake = &refTrackerDoer{getRet: `{"key":"PROJ-123","fields":{"summary":"Login returns 500","status":{"name":"In Progress"}}}`}
		w.testID = "PROJ-123"
		return register(jira.SourceType, jira.New("https://jira.example", "grounder-bot@example.com", tokenRef, jira.WithHTTPClient(w.fake)))
	})
	sc.Step(`^the GitHub Issues reference tracker module is registered$`, func() error {
		w.fake = &refTrackerDoer{getRet: `{"number":1347,"title":"Grounder rejects empty issue id","state":"open"}`}
		w.testID = "1347"
		return register(githubissues.SourceType, githubissues.New("https://api.github.com", "org", "repo", tokenRef, githubissues.WithHTTPClient(w.fake)))
	})
	sc.Step(`^the ServiceNow reference tracker module is registered$`, func() error {
		w.fake = &refTrackerDoer{getRet: `{"result":{"sys_id":"1c832d9f4f411200adf9f8e18110c7b1","short_description":"VPN gateway unreachable","state":"2"}}`}
		w.testID = "1c832d9f4f411200adf9f8e18110c7b1"
		return register(servicenow.SourceType, servicenow.New("https://sn.example", "grounder.bot", tokenRef, servicenow.WithHTTPClient(w.fake)))
	})

	sc.Step(`^it is selected as the active tracker by configuration$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceTracker, w.src)
		if err != nil {
			return fmt.Errorf("the selected module must resolve: %w", err)
		}
		trk, ok := reg.Adapter.(tracker.Tracker)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/tracker.Tracker")
		}
		if w.issue, err = trk.Open(context.Background(), w.testID); err != nil { // trigger
			w.err = err
			return nil
		}
		if err := trk.TransitionState(context.Background(), w.testID, tracker.StateInProgress); err != nil { // transition
			w.err = err
			return nil
		}
		if err := trk.Comment(context.Background(), w.testID, "audit: session resolved"); err != nil { // sink
			w.err = err
			return nil
		}
		return nil
	})

	sc.Step(`^it satisfies the same trigger, correlation-key, state-transition, and audit-sink contract as YouTrack$`, func() error {
		if w.err != nil {
			return fmt.Errorf("the four-verb contract must succeed: %w", w.err)
		}
		if w.issue.ID != w.testID { // correlation keys on the issue id (INV-05)
			return fmt.Errorf("correlation must key on the issue id, got %q want %q", w.issue.ID, w.testID)
		}
		if len(w.fake.mutations) < 2 { // one state transition + one audit comment
			return fmt.Errorf("state-transition and audit-sink must each issue a request, got %v", w.fake.mutations)
		}
		return nil
	})
}
