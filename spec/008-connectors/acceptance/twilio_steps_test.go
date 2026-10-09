package acceptance

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cucumber/godog"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/modules"
	twiliosms "github.com/territory-grounder/grounder/modules/notifier/twilio-sms"
)

// Twilio SMS (REQ-807): a send-only pager, deduplicated by decision id, carrying no command-executing content.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerTwilioSteps)
}

type twilioWorld struct {
	reg  *modules.Registry
	fake *notifierFakeDoer
	err  error
}

func registerTwilioSteps(sc *godog.ScenarioContext) {
	w := &twilioWorld{}

	sc.Step(`^the Twilio SMS notifier module is registered and enabled$`, func() error {
		_ = os.Setenv("TG_TWILIO_ACCEPT_TOKEN", "tok")
		w.reg = modules.NewRegistry()
		w.fake = &notifierFakeDoer{}
		mod := twiliosms.New("https://api.twilio.test", "AC123", "+100", "+200", "env:TG_TWILIO_ACCEPT_TOKEN", twiliosms.WithHTTPClient(w.fake))
		return w.reg.Register(modules.Registration{
			Surface: modules.SurfaceNotifier, SourceType: twiliosms.SourceType, Capability: "notifier.twilio-sms", Enabled: true, Adapter: mod,
		})
	})

	sc.Step(`^a POLL_PAUSE decision requires an out-of-band page$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceNotifier, twiliosms.SourceType)
		if err != nil {
			return fmt.Errorf("the enabled module must resolve: %w", err)
		}
		n, ok := reg.Adapter.(notifier.Notifier)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/notifier.Notifier")
		}
		notice := notifier.Notice{DecisionID: "TG-9#restart", Body: "POLL_PAUSE: restart web01; password=hunter2"}
		// page the SAME decision twice — the second is a dedup no-op.
		if err := n.Notify(context.Background(), notice); err != nil {
			w.err = err
			return nil
		}
		if err := n.Notify(context.Background(), notice); err != nil {
			w.err = err
		}
		return nil
	})

	sc.Step(`^a page deduplicated by the decision id is delivered and it carries no command-executing content$`, func() error {
		if w.err != nil {
			return fmt.Errorf("paging must succeed: %w", w.err)
		}
		if len(w.fake.bodies) != 1 {
			return fmt.Errorf("the decision must be paged exactly once (dedup), got %d sends", len(w.fake.bodies))
		}
		body := w.fake.bodies[0]
		if strings.Contains(body, "hunter2") || !strings.Contains(body, "REDACTED") {
			return fmt.Errorf("the page must carry only redacted plain text, got %q", body)
		}
		// a form-encoded SMS body carries no shell/command payload — reject any obvious command injection marker.
		if strings.ContainsAny(body, "`;") || strings.Contains(body, "$(") {
			return fmt.Errorf("the page must carry no command-executing content, got %q", body)
		}
		return nil
	})
}
