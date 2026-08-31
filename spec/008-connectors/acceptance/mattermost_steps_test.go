package acceptance

import (
	"context"
	"fmt"
	"os"

	"github.com/cucumber/godog"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/notifier/mattermost"
)

// Mattermost (REQ-808): binds an authenticated user's response to the specific pending decision.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerMattermostSteps)
}

type mattermostWorld struct {
	reg  *modules.Registry
	vote notifier.Vote
	err  error
}

func registerMattermostSteps(sc *godog.ScenarioContext) {
	w := &mattermostWorld{}

	sc.Step(`^the Mattermost notifier and approval module is registered and enabled$`, func() error {
		_ = os.Setenv("TG_MM_ACCEPT_TOKEN", "tok")
		w.reg = modules.NewRegistry()
		mod := mattermost.New("https://mm.test", "env:TG_MM_ACCEPT_TOKEN", []string{"oncall"}, map[string]string{"tg-approvals": "kwoybt1n1pn5jgh8qs9x4p3qzo"}, mattermost.WithHTTPClient(&notifierFakeDoer{}))
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceNotifier, SourceType: mattermost.SourceType, Capability: "notifier.mattermost", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^a user responds to an approval prompt from an authenticated identity$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceNotifier, mattermost.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		n, ok := reg.Adapter.(notifier.Notifier)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/notifier.Notifier")
		}
		w.vote, err = n.ResolveVote(context.Background(), []byte(`{"user_name":"oncall","message":"approve TG-9#restart"}`))
		if err != nil {
			w.err = err
		}
		return nil
	})

	sc.Step(`^the response is bound to the specific pending decision id it answers$`, func() error {
		if w.err != nil {
			return fmt.Errorf("an authenticated response must resolve: %w", w.err)
		}
		if w.vote.DecisionID != "TG-9#restart" {
			return fmt.Errorf("the response must bind to the pending decision id, got %q", w.vote.DecisionID)
		}
		if w.vote.Sender != "oncall" {
			return fmt.Errorf("the response must record its authenticated identity, got %q", w.vote.Sender)
		}
		return nil
	})
}
