package notifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// THE NOTIFIER SELF-TEST — the one probe in TG that deliberately emits.
//
// Every other module's probe is read-only: it authenticates, lists, and reads. A notifier cannot be
// proved that way. The three things an operator presses TEST to rule out are a revoked token, a room the
// bot was removed from, and a server that has been offline for a week, and a read-only credential check
// passes all three. Delivery is the only evidence that delivery works, so the probe sends a real message
// down the real Notify path.
//
// WHAT MAKES THAT ACCEPTABLE, and each clause is load-bearing:
//
//   - The body carries selftest.BodyMarker, so no human reading the room can mistake it for a governance
//     decision. This is not tidiness: an operator acting on a test message is acting on a decision TG
//     never made.
//   - It names WHO pressed TEST. A marked message with no author is an unexplained event in a room people
//     watch during incidents.
//   - Approval is false, always. A probe that posted a ballot would put a poll in the room that nobody
//     should answer — and worse, one that could be answered.
//   - The DecisionID is a fixed test id, never a live decision ref, so a probe can never be mistaken for,
//     or attached to, a real governed decision.
//   - The descriptor's Test.Verb for every notifier says plainly that a message will be posted. The
//     operator consents before the press.
//
// WHY IT IS SHARED. Six notifier modules (matrix, slack, teams, email, mattermost, twilio-sms) implement
// one interface and are proved the same way. Six copies would be six chances for one of them to drift
// into a credential-non-empty check, which is the failure mode this whole capability exists to remove.
// Each module's SelfTest is a one-line delegation to this function.

// ProbeDelivery sends one marked test notice through sink and reports what happened.
//
// destination is what the operator is TOLD the message went to, so a pass over a mis-configured room is
// still legible: "delivered to !wrong-room:example" is a finding, while a bare "ok" hides it. Pass the
// module's own configured destination; an empty string degrades to a truthful generic phrase rather than
// claiming a destination the caller could not name.
func ProbeDelivery(ctx context.Context, sink Notifier, operator, destination string) (selftest.Result, error) {
	if sink == nil {
		return selftest.Result{
			Summary: "no notifier is wired",
			Detail:  "the module resolved to nothing — nothing was sent",
		}, fmt.Errorf("notifier: nil sink")
	}
	notice := Notice{
		DecisionID: "tg-config-test",
		Body:       selftest.ProbeBody(operator),
		Approval:   false,
	}
	if err := sink.Notify(ctx, notice); err != nil {
		return selftest.Result{
			Summary: "the message could not be delivered",
			Detail:  ClassifyDeliveryFailure(err),
		}, err
	}
	dest := strings.TrimSpace(destination)
	if dest == "" {
		dest = "the configured destination"
	}
	return selftest.Result{
		Summary: "test message delivered to " + dest,
		Detail: "delivery is proved; whether the RIGHT people see it is not — check that the destination " +
			"above is the room you expect.",
	}, nil
}

// ClassifyDeliveryFailure turns a transport error into something an operator can act on.
//
// "error" tells them nothing; "the bot is not a member of that room" tells them exactly what to fix. It
// classifies on the SHAPE of the failure rather than parsing vendor prose, and falls through to the raw
// error rather than inventing a diagnosis it cannot support — a confident wrong explanation sends someone
// to fix the wrong thing, which costs more than no explanation at all.
func ClassifyDeliveryFailure(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "401") || strings.Contains(s, "unauthor") || strings.Contains(s, "m_unknown_token"):
		return "the access token was rejected — it is wrong, expired, or has been revoked. Save a new " +
			"token and test again."
	case strings.Contains(s, "403") || strings.Contains(s, "forbidden") || strings.Contains(s, "m_forbidden"):
		return "the server accepted the credential but refused the post — the bot is most likely not a " +
			"member of that room, or lacks permission to post in it."
	case strings.Contains(s, "404") || strings.Contains(s, "m_not_found"):
		return "the destination does not exist — check the room id, channel, or address."
	case strings.Contains(s, "resolve token") || strings.Contains(s, "secret"):
		return "the credential could not be READ from the secret backend — the reference is wrong, or the " +
			"backend is unreachable. This is a TG-side problem, not a problem with the chat service."
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "no such host") ||
		strings.Contains(s, "connection refused"):
		return "the server could not be reached — check the URL and that it is up."
	default:
		return err.Error()
	}
}
