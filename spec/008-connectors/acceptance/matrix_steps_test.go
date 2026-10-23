package acceptance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cucumber/godog"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/notifier/matrix"
)

// The Matrix notifier lead (REQ-806) binds its scenario here — the first notifier registrar.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerMatrixSteps)
}

type notifierFakeDoer struct{ bodies []string }

func (f *notifierFakeDoer) Do(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.bodies = append(f.bodies, string(b))
	}
	// A realistic success envelope: Slack's Web API returns {"ok":true} on success (and 200 + {"ok":false}
	// on app-level failure). Matrix and Teams ignore the body; only Slack inspects ok, so this keeps the
	// shared fake honest for all three.
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
}

type matrixWorld struct {
	reg      *modules.Registry
	fake     *notifierFakeDoer
	vote     notifier.Vote
	err      error
	postBody string
}

func registerMatrixSteps(sc *godog.ScenarioContext) {
	w := &matrixWorld{}

	sc.Step(`^the Matrix notifier and approval module is registered and enabled$`, func() error {
		_ = os.Setenv("TG_MATRIX_ACCEPT_TOKEN", "tok")
		w.reg = modules.NewRegistry()
		w.fake = &notifierFakeDoer{}
		mod := matrix.New("https://matrix.example", "env:TG_MATRIX_ACCEPT_TOKEN", []string{"@oncall:example"}, matrix.WithHTTPClient(w.fake))
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceNotifier, SourceType: matrix.SourceType, Capability: "notifier.matrix", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^an approver replies to a pending approval poll from an authenticated sender$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceNotifier, matrix.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		n, ok := reg.Adapter.(notifier.Notifier)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/notifier.Notifier")
		}
		// post a poll whose body contains a credential, to prove redaction on the way out.
		if err := n.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "restart svc; password=hunter2", Approval: true}); err != nil {
			w.err = err
			return nil
		}
		if len(w.fake.bodies) > 0 {
			w.postBody = w.fake.bodies[0]
		}
		// an approver replies, bound to the pending decision.
		w.vote, err = n.ResolveVote(context.Background(), []byte(`{"sender":"@oncall:example","content":{"body":"approve TG-9#restart"}}`))
		if err != nil {
			w.err = err
		}
		return nil
	})

	sc.Step(`^the vote is bound to the specific pending decision id and the posted body is credential and PII redacted$`, func() error {
		if w.err != nil {
			return fmt.Errorf("notify + resolve must succeed: %w", w.err)
		}
		if w.vote.DecisionID != "TG-9#restart" {
			return fmt.Errorf("the vote must bind to the pending decision id, got %q", w.vote.DecisionID)
		}
		if w.vote.Sender != "@oncall:example" {
			return fmt.Errorf("the vote must record its authenticated sender, got %q", w.vote.Sender)
		}
		if strings.Contains(w.postBody, "hunter2") || !strings.Contains(w.postBody, "[REDACTED]") {
			return fmt.Errorf("the posted body must be credential-redacted, got %q", w.postBody)
		}
		return nil
	})
}
