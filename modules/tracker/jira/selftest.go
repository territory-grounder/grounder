package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// probeMyselfPath is the cheapest authenticated read Jira offers: it answers "who is this credential",
// needs no issue and no project, and cannot be confused with a write.
const probeMyselfPath = "/rest/api/2/myself"

// probeProjectsPath is the read-SCOPE half, bounded to a handful of rows. `/project/search` is the
// paginated form and reports a `total`; the older `/rest/api/2/project` returns EVERY project in one
// unbounded response, which on a large site is a multi-megabyte answer behind a 30-second dialog.
const probeProjectsPath = "/rest/api/2/project/search?maxResults=5"

// probeMyself is the subset of GET /rest/api/2/myself the probe reads. Deliberately narrow: the response
// also carries avatars, locale and time zone, none of which tell an operator anything about whether this
// module works.
type probeMyself struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`   // Jira Server/DC returns a username here where Cloud returns accountId
	Active      *bool  `json:"active"` // POINTER: absent must mean "unknown", never "inactive"
}

// probeProjectPage is the subset of the paginated project search the probe reads.
type probeProjectPage struct {
	Total  int `json:"total"`
	Values []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"values"`
}

// SelfTest implements the OPTIONAL core/selftest.Tester capability: it proves this module can reach the
// Jira site it is configured for, with the credential it is configured with.
//
// WHAT IT READS, AND WHY THOSE TWO ENDPOINTS.
//
//	GET /rest/api/2/myself                    — identity. Jira Cloud auth is HTTP Basic
//	                                            base64(email:api_token), so BOTH halves are under test
//	                                            here: a right token with a wrong or blank account email
//	                                            401s exactly like a revoked token, and this is the only
//	                                            call that distinguishes them from a settings dialog.
//	GET /rest/api/2/project/search?maxResults=5 — scope. An account can authenticate and browse nothing;
//	                                            with no browsable project, every issue read returns 404
//	                                            and the module is silently inert.
//
// The descriptor used to promise "read one issue back from Jira". Nothing in this module's dialog names
// an issue, so honouring that would have meant hardcoding a key — and a 404 on a hardcoded key cannot be
// told apart from a token pointed at an entirely different Jira site, which is the exact failure TEST
// exists to expose. The verb now states what these two reads do.
//
// WHAT A GREEN RESULT PROVES: the site URL answers, the API token was readable from the secret backend
// and is accepted together with the configured account email, Jira names the account, and at least one
// project is browsable. WHAT IT DOES NOT PROVE: that the browsable projects are the intended ones (the
// Summary names them so a human can judge that), and nothing whatsoever about the workflow transition ids
// — those are write-path configuration, a probe may not exercise them, and a green TEST here says
// nothing about whether a close-out will land.
//
// IT IS READ-ONLY BY CONSTRUCTION: two GETs on the module's own `do`. The mutating verbs of this module
// (TransitionState, Comment) are not reachable from here.
//
// The operator parameter is ignored: nothing this probe does is visible to anyone but the operator who
// pressed the button, so there is no third party to attribute it to.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	site := m.siteHost()

	body, err := m.do(ctx, http.MethodGet, probeMyselfPath, nil)
	if err != nil {
		return selftest.Result{
			Summary: "could not authenticate to Jira at " + site,
			Detail:  classifyProbeFailure(err, site),
		}, err
	}
	var me probeMyself
	if err := json.Unmarshal(body, &me); err != nil {
		werr := fmt.Errorf("jira selftest: malformed myself response: %w", err)
		return selftest.Result{
			Summary: "could not read the Jira account at " + site,
			Detail:  classifyProbeFailure(werr, site),
		}, werr
	}
	who := describeAccount(me)

	res := selftest.Result{}
	if me.Active != nil && !*me.Active {
		// Reported, not failed: Jira let the call through, so TG works today. It is exactly the kind of
		// thing that stops working on a Monday with no configuration change to blame.
		res.Detail = "Jira reports this account as INACTIVE — it authenticates today but a deactivated " +
			"account can lose API access without any change on the TG side. "
	}

	page, perr := m.probeProjects(ctx)
	if perr != nil {
		if probeAborted(ctx, perr) {
			// A CANCELLED PROBE IS NOT AN INCONCLUSIVE ONE. See probeAborted: "the caller stopped waiting"
			// arrives through the same door as "this endpoint does not exist", and only the second is a
			// finding about the configuration.
			res.Summary = fmt.Sprintf("the test ended before Jira answered — reached %s as %s, but the "+
				"project list was never read", site, who)
			res.Detail += classifyProbeFailure(perr, site) + " Nothing about read scope was proven, so " +
				"this is not a pass: run the test again."
			return res, perr
		}
		// INCONCLUSIVE IS NOT FAILURE. /myself already proved the site and the credential; what is unknown
		// is the read scope. `/project/search` also simply does not exist on older Jira Server/DC, so a
		// red TEST here would send an operator hunting a credential that is fine.
		res.Summary = fmt.Sprintf("reached Jira at %s as %s — the project list could not be read", site, who)
		res.Detail += "the credential authenticated, so the site URL, the account email and the API token " +
			"are all good, but the project search did not answer: " + classifyProbeFailure(perr, site) +
			" This test therefore did NOT prove that TG can read any issue on this site."
		return res, nil
	}
	total := page.Total
	if total < len(page.Values) {
		// Older Jira omits `total`; the rows that came back are still a floor.
		total = len(page.Values)
	}
	if total == 0 {
		// CONCLUSIVE NEGATIVE: Jira answered, and the answer is that this account can browse nothing.
		res.Summary = fmt.Sprintf("reached Jira at %s as %s — but that account can browse NO projects", site, who)
		res.Detail += "the credential is valid and the site is reachable, so this is a permissions " +
			"problem: grant this account Browse Projects on the project holding the incidents. Until " +
			"then every issue read returns not-found, which looks exactly like a wrong issue key."
		return res, fmt.Errorf("jira selftest: authenticated as %s at %s but no projects are browsable", who, site)
	}
	res.Summary = fmt.Sprintf("reached Jira at %s as %s — %s browsable: %s",
		site, who, countPhrase(total, "project"), sampleProjects(page))
	return res, nil
}

// The module satisfies the optional capability. A signature drift breaks the build here rather than
// silently falling out of the composition root's type assertion, which would leave the dialog promising
// an action nothing performs.
var _ selftest.Tester = (*Module)(nil)

// probeProjects performs the bounded project read. It is separated so the URL — including the row cap —
// is stated in exactly one place: an unbounded project listing on a large site is a multi-megabyte
// response inside a 30-second dialog, and moduletest does not retry.
func (m *Module) probeProjects(ctx context.Context) (probeProjectPage, error) {
	body, err := m.do(ctx, http.MethodGet, probeProjectsPath, nil)
	if err != nil {
		return probeProjectPage{}, err
	}
	var page probeProjectPage
	if err := json.Unmarshal(body, &page); err != nil {
		return probeProjectPage{}, fmt.Errorf("jira selftest: malformed project search response: %w", err)
	}
	return page, nil
}

// probeAborted reports whether the probe was CUT SHORT rather than answered.
//
// It closes the hole the pass-vs-fail doctrine would otherwise leave open. An inconclusive read passes
// with a Detail naming what went unproven — but "the operator's context was cancelled" and "the site timed
// out" reach that branch through exactly the same door as "/project/search does not exist on this
// version", and a probe that never finished proved nothing at all. The console gives this 30 seconds with
// ONE attempt, so a slow site would otherwise be certified green by a test that was still waiting when the
// clock ran out.
//
// Both halves are checked: the error carries the cause when the transport noticed it, and ctx carries it
// when the failure surfaced some other way (a truncated body decoding as malformed JSON, for instance).
func probeAborted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// describeAccount renders the account JIRA says the credential belongs to — the server's answer, not the
// email we sent, because the point is to reveal that TG is acting as somebody other than the operator
// assumes. It never renders any part of the API token.
func describeAccount(me probeMyself) string {
	name := strings.TrimSpace(me.DisplayName)
	login := strings.TrimSpace(me.Name)
	switch {
	case name != "" && login != "" && !strings.EqualFold(name, login):
		return name + " (" + login + ")"
	case name != "":
		return name
	case login != "":
		return login
	case strings.TrimSpace(me.AccountID) != "":
		return "account id " + strings.TrimSpace(me.AccountID)
	default:
		// A 200 that names nobody is itself a finding: something answered, but not Jira answering for an
		// account.
		return "an account the site would not name"
	}
}

// sampleProjects names a few project keys, because a bare count cannot reveal a module pointed at the
// WRONG Jira: "7 projects browsable" reads identically against production and against a sandbox, while
// "TG, OPS, IFR" is recognisable at a glance.
func sampleProjects(page probeProjectPage) string {
	keys := make([]string, 0, len(page.Values))
	for _, v := range page.Values {
		k := strings.TrimSpace(v.Key)
		if k == "" {
			k = strings.TrimSpace(v.Name)
		}
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return "(the site returned a count but no project keys)"
	}
	out := strings.Join(keys, ", ")
	if page.Total > len(keys) {
		out += fmt.Sprintf(" and %d more", page.Total-len(keys))
	}
	return out
}

// countPhrase avoids "1 projects browsable", which reads as a bug in the thing being tested.
func countPhrase(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// siteHost renders WHERE the probe went, for both the pass and the failure text: a test that says
// "reached Jira" without saying which Jira cannot expose the mistake it exists to expose.
//
// Only the HOST is rendered. url.Parse drops any userinfo, so a base URL mistakenly saved as
// https://svc:token@example.atlassian.net cannot leak its password into a Result that ends up pasted into
// a ticket — and an unparseable URL is described rather than echoed, for the same reason.
func (m *Module) siteHost() string {
	raw := strings.TrimSpace(m.baseURL)
	if raw == "" {
		return "an unconfigured site URL"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "the configured site URL"
	}
	return u.Host
}

// classifyProbeFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "the email and token are a pair — a wrong email 401s exactly like a wrong token" tells them
// what to check, and on Jira Cloud that pairing is the most common misconfiguration by a wide margin.
//
// It classifies on the SHAPE of the failure — the HTTP status this package itself formatted, the
// transport class from net/http — never on vendor prose, which is free text that differs between Cloud
// and Data Center. An unrecognised shape falls through to the raw error rather than inventing a
// diagnosis, because a confident wrong answer costs an operator more than no answer.
func classifyProbeFailure(err error, site string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Checked first: nothing left the process, so this is a TG-side fault and pointing the operator at
	// Jira would waste their time.
	if strings.Contains(s, "resolve token") {
		return "the API token could not be READ from the secret backend — the token reference is wrong or " +
			"the backend is unreachable. This is a TG-side problem, not a Jira one."
	}
	switch code := httpStatusOf(err); {
	case code == 401:
		return "Jira rejected the credential. Jira Cloud authenticates as base64(account email:API " +
			"token), so BOTH halves are suspect: the token may be revoked or expired, or the Account " +
			"email may not be the one the token belongs to. A wrong email 401s exactly like a wrong token."
	case code == 403:
		return "the credential authenticated but Jira refused the read — the account lacks the permission " +
			"(for a project read that is Browse Projects), or the site requires a CAPTCHA after failed " +
			"logins, which also surfaces as 403."
	case code == 404:
		return "the site answered but that API path does not exist there. Check the Site URL: it must be " +
			"the Jira base (https://<site>.atlassian.net), with no /jira or project suffix."
	case code == 429:
		return "Jira is rate-limiting this credential. Wait and test again; nothing is wrong with the " +
			"configuration."
	case code >= 500:
		return fmt.Sprintf("the site answered %d — it is reachable but unhealthy. This is a Jira-side "+
			"fault, not a credential problem.", code)
	case code != 0:
		return fmt.Sprintf("the site refused the read with status %d: %s", code, s)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the probe ran out of time before Jira answered — the site is reachable but very slow, or " +
			"the URL points at something that accepts connections and never replies."
	case errors.Is(err, context.Canceled):
		return "the test was cancelled before Jira answered."
	case strings.Contains(s, "malformed"):
		return "something answered, but not with Jira's JSON — the Site URL is almost certainly pointing " +
			"at a proxy, an SSO login page, or another product entirely."
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return "Jira could not be reached at " + site + " — check the Site URL and that the site is up. " +
			"(transport: " + uerr.Err.Error() + ")"
	}
	return s
}

// httpStatusOf recovers the HTTP status from an error this package produced.
//
// It reads a number THIS PACKAGE formatted (`do` renders "...: status %d: ..."), not vendor text. That
// distinction is the point of the classification rule: the status code is structure TG controls, while
// the response body is free-form remote text that must never be pattern-matched for meaning. An
// unrecognised shape yields 0 and the caller falls through to the raw error.
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
