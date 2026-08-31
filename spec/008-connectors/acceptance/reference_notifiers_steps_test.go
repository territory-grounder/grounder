package acceptance

import (
	"context"
	"fmt"
	"net/smtp"
	"os"
	"strings"

	"github.com/cucumber/godog"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/notifier/email"
	"github.com/territory-grounder/grounder/modules/notifier/slack"
	"github.com/territory-grounder/grounder/modules/notifier/teams"
)

// The three REQ-809 reference notifiers (Slack, Teams, SMTP email) share identical When/Then step text, so
// their steps are registered ONCE here. Each Given wires its vendor behind the SAME notifier interface; the
// shared When/Then prove each preserves sender authentication, decision-id binding, and redaction.
func init() {
	moduleStepRegistrars = append(moduleStepRegistrars, registerReferenceNotifierSteps)
}

type refNotifierWorld struct {
	reg      *modules.Registry
	src      string
	posted   func() string // the content the module sent to the channel (for the redaction check)
	sample   string        // an inbound approver vote
	expect   string        // the decision id it must bind to
	badVote  string        // an inbound vote from a NON-approver
	vote     notifier.Vote
	err      error
	rejected bool
}

func registerReferenceNotifierSteps(sc *godog.ScenarioContext) {
	w := &refNotifierWorld{}

	register := func(src string, adapter any) error {
		w.reg = modules.NewRegistry()
		w.src = src
		return w.reg.Register(modules.Registration{Surface: modules.SurfaceNotifier, SourceType: src, Capability: "notifier." + src, Enabled: true, Adapter: adapter})
	}

	sc.Step(`^the Slack reference notifier module is registered$`, func() error {
		_ = os.Setenv("TG_SLACK_ACCEPT", "tok")
		f := &notifierFakeDoer{}
		w.posted = func() string { return lastOf(f.bodies) }
		w.sample = `{"user":"U123","text":"approve TG-9#restart"}`
		w.expect = "TG-9#restart"
		w.badVote = `{"user":"intruder","text":"approve TG-9#restart"}`
		return register(slack.SourceType, slack.New("https://slack.test", "env:TG_SLACK_ACCEPT", []string{"U123"}, slack.WithHTTPClient(f)))
	})

	sc.Step(`^the Microsoft Teams reference notifier module is registered$`, func() error {
		_ = os.Setenv("TG_TEAMS_ACCEPT", "tok")
		f := &notifierFakeDoer{}
		w.posted = func() string { return lastOf(f.bodies) }
		w.sample = `{"from":{"id":"29:abc"},"text":"approve TG-9#restart"}`
		w.expect = "TG-9#restart"
		w.badVote = `{"from":{"id":"29:intruder"},"text":"approve TG-9#restart"}`
		return register(teams.SourceType, teams.New("https://teams.test", "19:abc123def456@thread.tacv2", "env:TG_TEAMS_ACCEPT", []string{"29:abc"}, teams.WithHTTPClient(f)))
	})

	sc.Step(`^the SMTP email reference notifier module is registered$`, func() error {
		var sent []string
		fake := email.SendFunc(func(_ smtp.Auth, from string, to []string, msg []byte) error {
			sent = append(sent, string(msg))
			return nil
		})
		w.posted = func() string { return lastOf(sent) }
		w.sample = `{"from":"oncall@example.com","body":"approve TG-9#restart"}`
		w.expect = "TG-9#restart"
		w.badVote = `{"from":"intruder@evil.com","body":"approve TG-9#restart"}`
		return register(email.SourceType, email.New("smtp.test:25", "grounder@example.com", []string{"ops@example.com"}, []string{"oncall@example.com"}, email.WithSender(fake)))
	})

	sc.Step(`^it is selected as the active human channel by configuration$`, func() error {
		reg, err := w.reg.Resolve(modules.SurfaceNotifier, w.src)
		if err != nil {
			return fmt.Errorf("the selected module must resolve: %w", err)
		}
		n, ok := reg.Adapter.(notifier.Notifier)
		if !ok {
			return fmt.Errorf("the registered adapter must satisfy adapters/notifier.Notifier")
		}
		if err := n.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "restart web01; password=hunter2"}); err != nil {
			w.err = err
			return nil
		}
		if w.vote, err = n.ResolveVote(context.Background(), []byte(w.sample)); err != nil {
			w.err = err
			return nil
		}
		_, badErr := n.ResolveVote(context.Background(), []byte(w.badVote))
		w.rejected = badErr != nil
		return nil
	})

	sc.Step(`^it preserves sender authentication, decision-id binding, and credential and PII redaction$`, func() error {
		if w.err != nil {
			return fmt.Errorf("notify + resolve must succeed: %w", w.err)
		}
		if body := w.posted(); strings.Contains(body, "hunter2") || !strings.Contains(body, "REDACTED") {
			return fmt.Errorf("the sent content must be credential-redacted, got %q", body)
		}
		if w.vote.DecisionID != w.expect {
			return fmt.Errorf("the vote must bind to the decision id, got %q want %q", w.vote.DecisionID, w.expect)
		}
		if !w.rejected {
			return fmt.Errorf("a vote from a non-approver must be rejected (sender authentication)")
		}
		return nil
	})
}

func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}
