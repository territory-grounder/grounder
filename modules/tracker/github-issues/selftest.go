package githubissues

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

// probeRepo is the subset of GET /repos/{owner}/{repo} the probe reads.
//
// The three booleans are POINTERS on purpose: absent must mean "the server did not say", never "false".
// GitHub Enterprise Server omits fields github.com sends, and a probe that read a missing has_issues as
// "Issues are disabled" would fail a perfectly good configuration on an older Enterprise host.
type probeRepo struct {
	FullName   string `json:"full_name"`
	Private    *bool  `json:"private"`
	Archived   *bool  `json:"archived"`
	HasIssues  *bool  `json:"has_issues"`
	OpenIssues int    `json:"open_issues_count"`
}

// probeIssue is the subset of one row of the issues list the probe reads.
type probeIssue struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// probeUser is the subset of GET /user the probe reads. Nothing else about the account is any of TG's
// business, and a settings dialog is not the place to render somebody's profile.
type probeUser struct {
	Login string `json:"login"`
}

// SelfTest implements the OPTIONAL core/selftest.Tester capability: it proves this module can reach the
// repository it is configured for, with the token it is configured with.
//
// WHAT IT READS, AND WHY THOSE THREE ENDPOINTS.
//
//	GET /repos/{owner}/{repo}                — the module's ENTIRE blast radius, in one object. It
//	                                           confirms the repository exists, that this token can see it,
//	                                           and — via full_name — which repository GitHub actually
//	                                           served, which is how a rename or a transfer becomes visible.
//	GET /repos/{owner}/{repo}/issues?per_page=1 — the permission that matters. On a fine-grained token,
//	                                           repository METADATA and ISSUES are separate grants: the
//	                                           call above succeeds with Issues left unchecked, and then
//	                                           every read TG makes 403s in production. This is the
//	                                           "permission that was never granted" case TEST exists for.
//	GET /user                                — who TG acts as. Best effort only: a GitHub App installation
//	                                           token has no user and answers 403 here while being a
//	                                           perfectly valid credential, so a failure is reported and
//	                                           never fatal.
//
// The descriptor promised "read one issue back from the configured repository" and this honours it
// literally — but through the LIST endpoint, bounded to one row, rather than by reading a hardcoded issue
// number. That distinction is the whole design: an id baked into the probe turns a 404 from "that issue
// was deleted" into something indistinguishable from a 404 from "this token is pointed at a different
// repository", which is the exact mistake the descriptor warns about (a wrong repo name that EXISTS does
// not fail at all).
//
// WHAT A GREEN RESULT PROVES: the API base URL answers, the token was readable from the secret backend
// and is accepted, the configured repository resolves and is readable, issues on it are readable, and the
// account is named. WHAT IT DOES NOT PROVE: that TG may WRITE — commenting and closing need push access,
// which cannot be tested without writing and is therefore never claimed here.
//
// IT IS READ-ONLY BY CONSTRUCTION: three GETs on the module's own `do`. The mutating verbs of this module
// (TransitionState issues a PATCH, Comment a POST) are not reachable from here.
//
// The operator parameter is ignored: nothing this probe does is visible to anyone but the operator who
// pressed the button, so there is no third party to attribute it to.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	host := m.apiHost()
	slug := m.owner + "/" + m.repo

	body, err := m.do(ctx, http.MethodGet, "/repos/"+m.owner+"/"+m.repo, nil)
	if err != nil {
		return selftest.Result{
			Summary: "could not read the repository " + slug + " at " + host,
			Detail:  classifyProbeFailure(err, host),
		}, err
	}
	var repo probeRepo
	if err := json.Unmarshal(body, &repo); err != nil {
		werr := fmt.Errorf("github-issues selftest: malformed repository response: %w", err)
		return selftest.Result{
			Summary: "could not read the repository " + slug + " at " + host,
			Detail:  classifyProbeFailure(werr, host),
		}, werr
	}

	var notes []string
	// A repository GitHub serves under a DIFFERENT name is a rename or a transfer that the API followed
	// silently. It works today and is the reason a repository "moves" out from under a deployment.
	served := strings.TrimSpace(repo.FullName)
	if served != "" && !strings.EqualFold(served, slug) {
		notes = append(notes, "GitHub answered for "+served+", not the configured "+slug+
			" — the repository has been renamed or transferred and the API followed the redirect. Update "+
			"Repository owner/name before the redirect is retired.")
	}
	if repo.Archived != nil && *repo.Archived {
		notes = append(notes, "the repository is ARCHIVED, which makes it read-only on GitHub: TG can "+
			"read issues from it, but every comment and close-out will be refused.")
	}
	if repo.HasIssues != nil && !*repo.HasIssues {
		// CONCLUSIVE NEGATIVE. The repository is readable and Issues are switched off on it, so this
		// module has nothing to work with at all — no session can be anchored here.
		return selftest.Result{
			Summary: "reached GitHub at " + host + " — but " + repoName(served, slug) + " has Issues DISABLED",
			Detail: "the token and the repository are both fine; the repository simply has its Issues " +
				"feature turned off, so there is nothing for TG to read, comment on, or close. Enable " +
				"Issues in the repository settings, or point this module at the repository that actually " +
				"holds the incidents.",
		}, fmt.Errorf("github-issues selftest: %s has issues disabled", repoName(served, slug))
	}

	issues, ierr := m.probeIssues(ctx)
	if ierr != nil {
		return selftest.Result{
			Summary: "reached GitHub at " + host + " — but the issues of " + repoName(served, slug) + " could not be read",
			Detail: "the repository itself is readable, so this is not a wrong repository name: the token " +
				"is missing the ISSUES permission specifically (on a fine-grained token, repository " +
				"metadata and issues are separate grants). " + classifyProbeFailure(ierr, host),
		}, ierr
	}

	// Best effort and never fatal: a GitHub App installation token is a valid credential with no user
	// behind it, and 403s here. Losing the account name is worth strictly less than a false red.
	who := "an unnamed credential"
	if u, uerr := m.probeAccount(ctx); uerr != nil {
		if probeAborted(ctx, uerr) {
			// A CANCELLED PROBE IS NOT A BEST-EFFORT MISS. See probeAborted: "the caller stopped waiting"
			// arrives through the same door as "this credential has no user", and only the second is
			// benign. Worse, passing here would attach the installation-token explanation below to a
			// cancellation — a confident wrong diagnosis, which the classification rule forbids.
			return selftest.Result{
				Summary: "the test ended before GitHub answered — " + repoName(served, slug) +
					" was read, but the account behind the token was not",
				Detail: classifyProbeFailure(uerr, host) + " The test did not finish, so this is not a " +
					"pass: run it again.",
			}, uerr
		}
		notes = append(notes, "the account behind the token could not be read ("+
			classifyProbeFailure(uerr, host)+"). For a GitHub App installation token that is normal and "+
			"expected; otherwise it means only that this test cannot say WHO TG acts as here.")
	} else if login := strings.TrimSpace(u.Login); login != "" {
		who = login
	}

	res := selftest.Result{
		Summary: fmt.Sprintf("reached GitHub at %s as %s — %s readable (%s, %s), %s",
			host, who, repoName(served, slug), visibilityOf(repo),
			countPhrase(repo.OpenIssues, "open issue"), describeNewest(issues)),
		Detail: strings.Join(notes, " "),
	}
	return res, nil
}

// The module satisfies the optional capability. A signature drift breaks the build here rather than
// silently falling out of the composition root's type assertion, which would leave the dialog promising
// an action nothing performs.
var _ selftest.Tester = (*Module)(nil)

// probeIssues reads ONE row of the issue list.
//
// state=all is deliberate: a repository whose incidents are all closed would otherwise return an empty
// list and the probe would report "no issues readable" for a perfectly healthy tracker. per_page=1 is the
// bound — the console gives this 30 seconds with no retry, and a busy repository's default page is 30
// full issue bodies TG has no use for.
func (m *Module) probeIssues(ctx context.Context) ([]probeIssue, error) {
	q := url.Values{}
	q.Set("per_page", "1")
	q.Set("state", "all")
	body, err := m.do(ctx, http.MethodGet, "/repos/"+m.owner+"/"+m.repo+"/issues?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var out []probeIssue
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("github-issues selftest: malformed issue list response: %w", err)
	}
	return out, nil
}

// probeAccount asks GitHub who the token belongs to.
func (m *Module) probeAccount(ctx context.Context) (probeUser, error) {
	body, err := m.do(ctx, http.MethodGet, "/user", nil)
	if err != nil {
		return probeUser{}, err
	}
	var u probeUser
	if err := json.Unmarshal(body, &u); err != nil {
		return probeUser{}, fmt.Errorf("github-issues selftest: malformed user response: %w", err)
	}
	return u, nil
}

// probeAborted reports whether the probe was CUT SHORT rather than answered.
//
// It closes the hole the pass-vs-fail doctrine would otherwise leave open. The /user read is best effort
// and its failure is reported on a pass — but "the operator's context was cancelled" and "the API timed
// out" reach that branch through exactly the same door as "an installation token has no user", and a probe
// that never finished proved nothing. The console gives this 30 seconds with ONE attempt, so a slow API
// would otherwise be certified green by a test that was still waiting when the clock ran out.
//
// Both halves are checked: the error carries the cause when the transport noticed it, and ctx carries it
// when the failure surfaced some other way (a truncated body decoding as malformed JSON, for instance).
func probeAborted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// describeNewest names the newest entry the issues list returned, because a bare "issues are readable"
// cannot reveal a module pointed at a repository that merely EXISTS and is not the intended one.
//
// It says "entry" rather than "issue" on purpose: GitHub's issues list includes pull requests, so the row
// that comes back may be a PR, and a probe that called it an issue would be stating something it did not
// check.
func describeNewest(issues []probeIssue) string {
	if len(issues) == 0 {
		// Not a fault: the 200 already proved the read is permitted. A repository can legitimately have
		// no issues at all, and saying so is more useful than omitting the clause.
		return "the issue list is readable and currently empty"
	}
	first := issues[0]
	state := strings.TrimSpace(first.State)
	if state == "" {
		state = "state unstated"
	}
	return fmt.Sprintf("newest entry #%d (%s)", first.Number, state)
}

// repoName prefers the name GITHUB served over the one TG asked for: when they differ, the served name is
// the one that tells an operator what actually happened.
func repoName(served, configured string) string {
	if served != "" {
		return served
	}
	return configured
}

// visibilityOf reports whether GitHub called the repository private. It is a one-word wrong-repository
// tell: an operator who knows the incident repo is private and sees "public" is looking at the wrong one.
func visibilityOf(r probeRepo) string {
	if r.Private == nil {
		return "visibility unstated"
	}
	if *r.Private {
		return "private"
	}
	return "public"
}

// countPhrase avoids "1 open issues", which reads as a bug in the thing being tested.
func countPhrase(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// apiHost renders WHERE the probe went, for both the pass and the failure text. It matters more here than
// anywhere else in the tracker family: the same owner/repo can exist on github.com and on an Enterprise
// Server host, and only the host distinguishes them.
//
// Only the HOST is rendered. url.Parse drops any userinfo, so a base URL mistakenly saved as
// https://x:token@ghe.example/api/v3 cannot leak its password into a Result that ends up pasted into a
// ticket — and an unparseable URL is described rather than echoed, for the same reason.
func (m *Module) apiHost() string {
	raw := strings.TrimSpace(m.baseURL)
	if raw == "" {
		return "an unconfigured API base URL"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "the configured API base URL"
	}
	return u.Host
}

// classifyProbeFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "GitHub answers 404 rather than 403 for a private repository your token cannot see" tells them
// why the obvious diagnosis is wrong, and that one is the single most misleading response this API gives.
//
// It classifies on the SHAPE of the failure — the HTTP status this package itself formatted, the
// transport class from net/http — never on vendor prose. An unrecognised shape falls through to the raw
// error rather than inventing a diagnosis, because a confident wrong answer costs an operator more than
// no answer.
func classifyProbeFailure(err error, host string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Checked first: nothing left the process, so this is a TG-side fault and pointing the operator at
	// GitHub would waste their time.
	if strings.Contains(s, "resolve token") {
		return "the access token could not be READ from the secret backend — the token reference is wrong " +
			"or the backend is unreachable. This is a TG-side problem, not a GitHub one."
	}
	switch code := httpStatusOf(err); {
	case code == 401:
		return "GitHub rejected the token — it is wrong, expired, or has been revoked. Save a new token " +
			"and test again."
	case code == 403:
		return "the token authenticated but GitHub refused the read. Either the token's permissions do " +
			"not include this repository's issues, or an organisation policy (SSO authorisation, IP " +
			"allow-list) is blocking it — GitHub also uses 403 when a secondary rate limit is hit."
	case code == 404:
		return "GitHub returned not-found. That does NOT mean the repository is absent: for a PRIVATE " +
			"repository a token cannot see, GitHub answers 404 rather than 403 so it does not disclose " +
			"that it exists. Check Repository owner and name for a typo AND that this token is granted " +
			"access to it."
	case code == 410:
		return "GitHub answered 410 Gone — Issues are disabled on this repository, so there is nothing " +
			"here for TG to read or write."
	case code == 429:
		return "GitHub is rate-limiting this token. Wait and test again; nothing is wrong with the " +
			"configuration."
	case code >= 500:
		return fmt.Sprintf("GitHub answered %d — the API is reachable but unhealthy. This is a "+
			"GitHub-side fault, not a credential problem.", code)
	case code != 0:
		return fmt.Sprintf("GitHub refused the read with status %d: %s", code, s)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the probe ran out of time before GitHub answered — the API is reachable but very slow, " +
			"or the base URL points at something that accepts connections and never replies."
	case errors.Is(err, context.Canceled):
		return "the test was cancelled before GitHub answered."
	case strings.Contains(s, "malformed"):
		return "something answered, but not with GitHub's JSON — check the API base URL: an Enterprise " +
			"Server host needs the /api/v3 suffix, and without it a web page is served where the API " +
			"should be."
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return "GitHub could not be reached at " + host + " — check the API base URL and that the host " +
			"is up. (transport: " + uerr.Err.Error() + ")"
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
