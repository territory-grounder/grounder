package servicenow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/selftest"
)

// probeIncidentQuery is the probe's incident read: the newest ONE row, three fields.
//
// It is deliberately the narrowest useful query. sysparm_limit=1 keeps a 30-second dialog off a table
// that at a real site holds hundreds of thousands of rows, and sysparm_fields omits short_description
// entirely — the probe needs to know the table is READABLE, not what the incident says, and an incident
// title is exactly the kind of text an operator should not have pasted into a ticket by a config dialog.
func probeIncidentQuery() string {
	q := url.Values{}
	// ORDERBYDESC makes the single row deterministic. Without it the "one" incident is whatever the
	// instance felt like returning, and a probe whose observation changes between presses teaches an
	// operator to ignore it.
	q.Set("sysparm_query", "ORDERBYDESCopened_at")
	q.Set("sysparm_fields", "number,state,opened_at")
	q.Set("sysparm_limit", "1")
	return "/api/now/table/incident?" + q.Encode()
}

// probeJournalQuery checks the OTHER table this module reads. It asks for one row and does NOT ask for
// `value`: the question is whether the account may read the journal at all, and the answer does not
// require any incident's discussion to leave the instance.
func probeJournalQuery() string {
	q := url.Values{}
	q.Set("sysparm_fields", "element,sys_created_on")
	q.Set("sysparm_limit", "1")
	return "/api/now/table/sys_journal_field?" + q.Encode()
}

// SelfTest implements the OPTIONAL core/selftest.Tester capability: it proves this module can reach the
// ServiceNow instance it is configured for, with the credential it is configured with.
//
// WHAT IT READS, AND WHY THOSE TWO TABLES.
//
//	GET /api/now/table/incident?sysparm_limit=1        — the table every verb of this module works
//	                                                     against. It is query-only, so it needs no
//	                                                     pre-existing sys_id: a probe that read a
//	                                                     hardcoded incident could not tell "that incident
//	                                                     does not exist" from "this credential is pointed
//	                                                     at a completely different instance".
//	GET /api/now/table/sys_journal_field?sysparm_limit=1 — the incident DISCUSSION, which ServiceNow keeps
//	                                                     on a separate table with its own ACLs. Incident
//	                                                     history reads both (history.go), and an account
//	                                                     that can read incidents but not the journal
//	                                                     returns history with the resolution missing —
//	                                                     which reads as "nobody wrote anything down"
//	                                                     rather than as a permissions gap.
//
// The Basic username is under test alongside the password: Table API auth is base64(username:password), a
// wrong username 401s exactly like a wrong password, and a 200 here is the instance itself confirming it
// accepted that account.
//
// WHAT A GREEN RESULT PROVES: the instance URL answers, the password was readable from the secret backend
// and is accepted with the configured user, the incident table is readable under that user's ACLs, and
// the state code that came back is shown folded through THIS deployment's configured mapping — so an
// operator can see at a glance that a "resolved" incident is being read back as resolved rather than as
// open. WHAT IT DOES NOT PROVE: that writes would be permitted (write ACLs are separate and are never
// exercised here) or that the state codes used for transitions exist in this instance's choice list.
//
// IT IS READ-ONLY BY CONSTRUCTION: two GETs on the module's own `do`, so no PATCH path is reachable.
//
// The operator parameter is ignored: nothing this probe does is visible to anyone but the operator who
// pressed the button, so there is no third party to attribute it to.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	host := m.instanceHost()
	who := m.probeUser()

	body, err := m.do(ctx, http.MethodGet, probeIncidentQuery(), nil)
	if err != nil {
		return selftest.Result{
			Summary: "could not read the incident table on " + host,
			Detail:  classifyProbeFailure(err, host),
		}, err
	}
	var env incidentListEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		werr := fmt.Errorf("servicenow selftest: malformed incident list response: %w", err)
		return selftest.Result{
			Summary: "could not read the incident table on " + host,
			Detail:  classifyProbeFailure(werr, host),
		}, werr
	}

	res := selftest.Result{}
	switch {
	case len(env.Result) == 0:
		// INCONCLUSIVE, NOT NEGATIVE. A 200 with no rows means the query was accepted; the instance may
		// genuinely have no incidents, or every row may be filtered away by this account's ACLs, and the
		// Table API does not distinguish the two. Both are worth saying out loud rather than hiding
		// behind a green tick.
		res.Summary = fmt.Sprintf("read the incident table on %s as %s — it accepted the query and "+
			"returned no incidents", host, who)
		res.Detail = "the URL and the credential are good. The empty result means either that this " +
			"instance has no incidents at all, or that this user's ACLs filter every one of them away — " +
			"the Table API answers both the same way, so TG cannot tell them apart. If you expect " +
			"incidents here, check the user's read ACL on the incident table."
	default:
		newest := env.Result[0]
		res.Summary = fmt.Sprintf("read the incident table on %s as %s — newest incident %s, %s",
			host, who, probeIncidentName(newest), m.describeState(newest.State))
	}

	// Best effort, and the failure is REPORTED rather than swallowed: history still works without the
	// journal, it just comes back without the part that holds the answer.
	if _, jerr := m.do(ctx, http.MethodGet, probeJournalQuery(), nil); jerr != nil {
		if probeAborted(ctx, jerr) {
			// A CANCELLED PROBE IS NOT A BEST-EFFORT MISS. See probeAborted: "the caller stopped waiting"
			// arrives through the same door as "this account may not read that table", and only the second
			// is a finding. Reporting a test that never finished as a pass — with a green tick over a
			// Detail that says the read was cancelled — is exactly the dishonest pass this file argues
			// against everywhere else.
			res.Summary = "the test ended before " + host + " answered — the journal table was never read"
			res.Detail = classifyProbeFailure(jerr, host) + " The incident read had already succeeded, but " +
				"the test did not finish, so this is not a pass: run it again."
			return res, jerr
		}
		res.Detail += " The journal table (sys_journal_field) could NOT be read: " +
			classifyProbeFailure(jerr, host) + " Incidents will still be found, but their comments and " +
			"work notes — where an engineer actually writes the resolution — will be missing from " +
			"incident history, which reads as an estate where nobody wrote anything down."
		res.Detail = strings.TrimSpace(res.Detail)
	}
	return res, nil
}

// The module satisfies the optional capability. A signature drift breaks the build here rather than
// silently falling out of the composition root's type assertion, which would leave the dialog promising
// an action nothing performs.
var _ selftest.Tester = (*Module)(nil)

// probeAborted reports whether the probe was CUT SHORT rather than answered.
//
// It closes the hole the pass-vs-fail doctrine would otherwise leave open. A best-effort read that fails
// is reported on a pass — but "the operator's context was cancelled" and "the instance timed out" reach
// that branch through exactly the same door as "the ACLs refuse this table", and a probe that never
// finished proved nothing at all. The console gives this 30 seconds with ONE attempt, so a slow instance
// would otherwise be certified green by a test that was still waiting when the clock ran out.
//
// Both halves are checked: the error carries the cause when the transport noticed it, and ctx carries it
// when the failure surfaced some other way (a truncated body decoding as malformed JSON, for instance).
func probeAborted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// probeIncidentName prefers the human-readable number (INC0010023) — the id an engineer at this site
// recognises, and the one that makes a wrong-instance mistake obvious. The sys_id is deliberately NOT a
// fallback: it is an opaque 32-character string that tells a reader nothing.
func probeIncidentName(rec incidentListRecord) string {
	if n := strings.TrimSpace(rec.Number); n != "" {
		return n
	}
	return "(an incident the instance returned without a number)"
}

// describeState renders the raw state code AND what this deployment's configured mapping turns it into.
//
// Both halves matter. The code is what the instance said; the fold is what TG will believe. A deployment
// that customized its choice list and left the state codes at the out-of-box 2/6/1 reads resolved
// incidents as open — a silent, config-only fault that no other screen in the console would show, and
// which this line makes visible on a green test.
func (m *Module) describeState(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "with no state value"
	}
	folded := m.fromServiceNowState(code)
	label := string(folded)
	if folded == tracker.StateOpen && code != m.states.open {
		// The default arm of the fold is a catch-all, so an unrecognised code lands on Open. Saying it is
		// "read as open" without saying the code was unrecognised would hide the misconfiguration.
		return fmt.Sprintf("state %s — a code this deployment does not map, read as %s", code, label)
	}
	return fmt.Sprintf("state %s, which this deployment reads as %s", code, label)
}

// probeUser names the account the instance accepted. It is the configured Basic username rather than
// anything the response carried, and that is honest: a 200 means the instance authenticated THIS user,
// and reading sys_user to confirm it would demand an ACL many integration accounts are not given —
// turning a working configuration into a red test.
func (m *Module) probeUser() string {
	if u := strings.TrimSpace(m.username); u != "" {
		return u
	}
	return "an unnamed user"
}

// instanceHost renders WHERE the probe went, for both the pass and the failure text: a test that says
// "read the incident table" without saying on which instance cannot expose the mistake it exists to
// expose — and pointing at a clone of production is exactly that mistake.
//
// Only the HOST is rendered. url.Parse drops any userinfo, so a URL mistakenly saved as
// https://svc:hunter2@example.service-now.com cannot leak its password into a Result that ends up pasted
// into a ticket — and an unparseable URL is described rather than echoed, for the same reason.
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
// nothing; "the instance is hibernating" tells them exactly what to do, and on a ServiceNow developer
// instance that is the most common cause of a test that fails for a week and then starts working.
//
// It classifies on the SHAPE of the failure — the HTTP status this package itself formatted, the
// transport class from net/http — never on vendor prose, which is free text that changes between
// ServiceNow releases. An unrecognised shape falls through to the raw error rather than inventing a
// diagnosis, because a confident wrong answer costs an operator more than no answer.
func classifyProbeFailure(err error, host string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// Checked first: nothing left the process, so this is a TG-side fault and pointing the operator at
	// ServiceNow would waste their time.
	if strings.Contains(s, "resolve token") {
		return "the password could not be READ from the secret backend — the password reference is wrong " +
			"or the backend is unreachable. This is a TG-side problem, not a ServiceNow one."
	}
	switch code := httpStatusOf(err); {
	case code == 401:
		return "the instance rejected the credential. Table API auth is base64(username:password), so " +
			"BOTH halves are suspect: the password may be wrong or expired, or the Instance user may not " +
			"be the account it belongs to. A wrong username 401s exactly like a wrong password."
	case code == 403:
		return "the user authenticated but the instance refused the read — this account's ACLs do not " +
			"grant read on that table. Grant it the itil (or an equivalent read) role and test again."
	case code == 400:
		return "the instance rejected the query itself — the table named in the URL does not exist on " +
			"this instance, or the Instance URL points at something other than a ServiceNow API root."
	case code == 404:
		return "the instance answered but that API path does not exist there. Check the Instance URL: it " +
			"must be the instance base (https://<instance>.service-now.com) with no path suffix."
	case code == 429:
		return "the instance is rate-limiting this account. Wait and test again; nothing is wrong with " +
			"the configuration."
	case code >= 500:
		return fmt.Sprintf("the instance answered %d — it is reachable but unhealthy. This is a "+
			"ServiceNow-side fault, not a credential problem.", code)
	case code != 0:
		return fmt.Sprintf("the instance refused the read with status %d: %s", code, s)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the probe ran out of time before the instance answered — it is reachable but very slow, " +
			"or the URL points at something that accepts connections and never replies."
	case errors.Is(err, context.Canceled):
		return "the test was cancelled before the instance answered."
	case strings.Contains(s, "malformed"):
		return "something answered, but not with the Table API's JSON. A developer instance that has " +
			"HIBERNATED serves an HTML wake-up page with a 200 status and looks exactly like this; " +
			"otherwise the Instance URL is pointing at a proxy or a login page."
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return "the instance could not be reached at " + host + " — check the Instance URL and that the " +
			"instance is up. (transport: " + uerr.Err.Error() + ")"
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
