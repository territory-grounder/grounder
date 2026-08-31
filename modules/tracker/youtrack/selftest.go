package youtrack

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// SelfTest implements the OPTIONAL core/selftest.Tester capability: it proves this module can actually
// talk to the YouTrack instance it is configured for, with the credential it is configured with.
//
// WHAT IT READS, AND WHY THOSE TWO ENDPOINTS.
//
//	GET /api/users/me           — the cheapest authenticated call YouTrack offers. It answers the only
//	                              question a token can answer on its own: WHO does TG act as here. A
//	                              revoked or mistyped token cannot get past it.
//	GET /api/admin/projects     — the read-scope half. An account can authenticate and still see nothing:
//	                              YouTrack permissions are per-project, so a token that passes /me and
//	                              sees zero projects reads every issue as a 404 and returns empty
//	                              incident history forever, silently.
//
// The descriptor used to promise "read one issue back from YouTrack". It cannot honour that: no field in
// this module's dialog names an issue, so the probe would have to hardcode an id — and a hardcoded id is
// the worst possible probe, because a 404 from "the issue does not exist here" is indistinguishable from
// a 404 from "this token is pointed at an entirely different YouTrack". The verb now says what these two
// reads actually do.
//
// WHAT A GREEN RESULT PROVES: the instance URL resolves and answers, the token in the secret backend was
// readable and is accepted, the account it belongs to is named, and at least one project is visible to
// it. WHAT IT DOES NOT PROVE: that the visible projects are the RIGHT ones (the Summary names them so a
// human can see that for themselves), that the State field name and value mapping match this project's
// bundle, or anything at all about writes — which are refused by configuration in the default posture and
// are never exercised here.
//
// IT IS READ-ONLY BY CONSTRUCTION. Both calls are GETs on the module's own `do`, so none of them passes
// through guardWrite and none of them can. That matters more here than on any other tracker: this
// instance is the shared corpus the predecessor comparison is measured against, and a probe that wrote
// anything — even a marker comment — would contaminate the inputs of a running experiment. The probe
// therefore behaves identically whether or not writes are armed, and the oracle asserts exactly that.
//
// The operator parameter is ignored. It exists in the interface for the one probe with an outward side
// effect (a notifier must name who caused the message that appears in an operations room); nothing here
// is visible to anyone but the operator who pressed the button, so there is nothing to attribute.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	host := m.instanceHost()

	me, err := m.Me(ctx)
	if err != nil {
		return selftest.Result{
			Summary: "could not authenticate to YouTrack at " + host,
			Detail:  classifyProbeFailure(err, host),
		}, err
	}
	who := describeAccount(me)

	projects, perr := m.Projects(ctx)
	if perr != nil {
		if probeAborted(ctx, perr) {
			// A CANCELLED PROBE IS NOT AN INCONCLUSIVE ONE. See probeAborted: "the caller stopped waiting"
			// arrives through the same door as "this endpoint refused", and only the second is a finding.
			return selftest.Result{
				Summary: fmt.Sprintf("the test ended before YouTrack answered — reached %s as %s, but the "+
					"project list was never read", host, who),
				Detail: classifyProbeFailure(perr, host) + " Nothing about read scope was proven, so this " +
					"is not a pass: run the test again.",
			}, perr
		}
		// INCONCLUSIVE IS NOT FAILURE. The credential and the instance are already proven by /me; what is
		// missing is the answer to "and can it read anything". Reporting that as a red TEST would send an
		// operator hunting a token that is fine, so it passes — but the Detail states plainly which half
		// went unproven, because a pass whose limits are unstated is the dishonest kind.
		return selftest.Result{
			Summary: fmt.Sprintf("reached YouTrack at %s as %s — the project list could not be read", host, who),
			Detail: "the token authenticated, so the URL and the credential are good, but the project " +
				"listing did not answer: " + classifyProbeFailure(perr, host) + " This test therefore did " +
				"NOT prove that TG can read issues or incident history from this instance.",
		}, nil
	}
	if len(projects) == 0 {
		// CONCLUSIVE NEGATIVE. Not "we could not tell" — YouTrack answered, and the answer is that this
		// account can see nothing. Every issue read will 404 and every history search will come back
		// empty, which downstream looks exactly like "this host has no incident history".
		return selftest.Result{
			Summary: fmt.Sprintf("reached YouTrack at %s as %s — but that account can see NO projects", host, who),
			Detail: "the instance is reachable and the token is valid, so this is a permissions problem, " +
				"not a connectivity one: grant this account read access to the projects TG must see. " +
				"Until then every issue read returns not-found and incident history comes back empty, " +
				"which is indistinguishable from an estate that has never had an incident.",
		}, fmt.Errorf("youtrack selftest: authenticated as %s at %s but no projects are visible", who, host)
	}

	res := selftest.Result{
		Summary: fmt.Sprintf("reached YouTrack at %s as %s — %s visible: %s",
			host, who, countPhrase(len(projects), "project"), sampleProjects(projects)),
	}
	// Projects() asks for $top=500. Saying so when the ceiling is hit keeps "500 projects" from being read
	// as a count when it is really a truncation.
	if len(projects) >= 500 {
		res.Detail = "the project listing was capped at 500 entries, so the count above is a floor, not a total."
	}
	return res, nil
}

// The module satisfies the optional capability. A signature drift breaks the build here rather than
// silently falling out of the composition root's type assertion, which would put the dialog back where it
// started: a TEST button promising an action nothing performs.
var _ selftest.Tester = (*Module)(nil)

// probeAborted reports whether the probe was CUT SHORT rather than answered.
//
// It closes the hole the pass-vs-fail doctrine would otherwise leave open. An inconclusive read passes
// with a Detail naming what went unproven — but "the operator's context was cancelled" and "the instance
// timed out" reach that branch through exactly the same door as "the endpoint refused", and a probe that
// never finished proved nothing at all. The console gives this 30 seconds with ONE attempt, so a slow
// instance would otherwise be certified green by a test that was still waiting when the clock ran out.
//
// Both halves are checked: the error carries the cause when the transport noticed it, and ctx carries it
// when the failure surfaced some other way (a truncated body decoding as malformed JSON, for instance).
func probeAborted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// describeAccount renders the account YouTrack says the token belongs to. The login is what an
// administrator searches for; the full name is what they recognise, so both appear when they differ.
//
// It never renders the token or any part of it: the point of naming the account is that an operator can
// spot "TG is acting as the wrong service user" or "this is my personal account" from the dialog.
func describeAccount(u User) string {
	login := strings.TrimSpace(u.Login)
	name := strings.TrimSpace(u.Name)
	switch {
	case login != "" && name != "" && !strings.EqualFold(login, name):
		return login + " (" + name + ")"
	case login != "":
		return login
	case name != "":
		return name
	case strings.TrimSpace(u.ID) != "":
		return "account id " + strings.TrimSpace(u.ID)
	default:
		// A 200 that names nobody is itself the finding: something answered, but it was not YouTrack
		// answering for an account.
		return "an account the instance would not name"
	}
}

// sampleProjects lists a few project short names so a green TEST can still reveal a module pointed at the
// WRONG YouTrack. A bare count cannot: "12 projects visible" reads identically against the production
// instance and against a sandbox clone of it, while "IFR, TG, OPS" is recognisable at a glance.
func sampleProjects(ps []Project) string {
	const show = 4
	names := make([]string, 0, show)
	for _, p := range ps {
		if len(names) == show {
			break
		}
		n := strings.TrimSpace(p.ShortName)
		if n == "" {
			n = strings.TrimSpace(p.Name)
		}
		if n == "" {
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return "(none of them carry a name)"
	}
	out := strings.Join(names, ", ")
	if len(ps) > len(names) {
		out += fmt.Sprintf(" and %d more", len(ps)-len(names))
	}
	return out
}

// countPhrase avoids "1 projects visible", which reads as a bug in the thing being tested.
func countPhrase(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// instanceHost renders WHERE the probe went, for both the pass and the failure text. A test that says
// "reached YouTrack" without saying which YouTrack cannot expose the mistake it exists to expose.
//
// It deliberately renders only the HOST. url.Parse drops any userinfo the base URL carries, so a URL
// mistakenly saved as https://user:token@yt.example cannot leak its password into a Result that gets
// pasted into a ticket — and an unparseable URL is described rather than echoed, for the same reason.
func (m *Module) instanceHost() string {
	raw := strings.TrimSpace(m.baseURL)
	if raw == "" {
		return "an unconfigured instance URL"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "the configured instance URL"
	}
	return u.Host
}

// classifyProbeFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "the endpoint 404s — a YouTrack base URL usually ends in /youtrack" tells them exactly what to
// fix, and is the single most common way this module is misconfigured.
//
// It classifies on the SHAPE of the failure — the HTTP status this package itself formatted, the
// transport class from net/http — and never on vendor prose, which is free text that changes between
// YouTrack versions. When the shape is not recognised it falls through to the raw error rather than
// inventing a diagnosis: a wrong diagnosis costs an operator more than no diagnosis.
func classifyProbeFailure(err error, host string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Checked first because it never carries an HTTP status: nothing left the process, so this is a
	// TG-side fault and pointing the operator at YouTrack would waste their time.
	if strings.Contains(s, "resolve token") {
		return "the API token could not be READ from the secret backend — the token reference is wrong or " +
			"the backend is unreachable. This is a TG-side problem, not a YouTrack one."
	}
	switch code := httpStatusOf(err); {
	case code == 401:
		return "the permanent token was rejected — it is wrong, expired, or has been revoked. Save a new " +
			"token and test again."
	case code == 403:
		return "the token authenticated but the account is not permitted to read this — grant it read " +
			"access to the projects TG must see."
	case code == 404:
		return "the instance answered but that API path does not exist there. Check the Instance URL: a " +
			"YouTrack base URL commonly ends in /youtrack, and a bare host 404s every API call while " +
			"still looking reachable."
	case code == 429:
		return "YouTrack is rate-limiting this token. Wait and test again; nothing is wrong with the " +
			"configuration."
	case code >= 500:
		return fmt.Sprintf("the instance answered %d — it is reachable but unhealthy. This is a "+
			"YouTrack-side fault, not a credential problem.", code)
	case code != 0:
		return fmt.Sprintf("the instance refused the read with status %d: %s", code, s)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the probe ran out of time before YouTrack answered — the instance is reachable but very " +
			"slow, or the URL points at something that accepts connections and never replies."
	case errors.Is(err, context.Canceled):
		return "the test was cancelled before YouTrack answered."
	case strings.Contains(s, "malformed"):
		return "something answered, but not with YouTrack's JSON — the Instance URL is almost certainly " +
			"pointing at a proxy, a login page, or another product entirely."
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return "YouTrack could not be reached at " + host + " — check the Instance URL and that the " +
			"instance is up. (transport: " + uerr.Err.Error() + ")"
	}
	return s
}

// httpStatusOf recovers the HTTP status from an error this package produced.
//
// It reads a number THIS PACKAGE formatted (`do` renders "...: status %d: ..."), not vendor text. That
// distinction is the whole point of the classification rule: the status code is structure TG controls and
// can rely on, while the response body is free-form remote text that must never be pattern-matched for
// meaning. A shape it does not recognise yields 0 and the caller falls through to the raw error.
func httpStatusOf(err error) int {
	const marker = ": status "
	s := err.Error()
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	rest := s[i+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	code, cerr := strconv.Atoi(rest[:end])
	if cerr != nil {
		return 0
	}
	return code
}
