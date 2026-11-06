// This file is the AWX-job lane's answer to the console's TEST button (core/selftest.Tester).
//
// WHY IT EXISTS AND WHY IT IS SHAPED LIKE THIS. The Actuator's only surface method is Exec, and Exec's whole
// job is to POST /api/v2/job_templates/{id}/launch/. A probe built from the actuation surface interface would
// therefore START AN AWX JOB — a real, unreviewed play against real hosts, triggered because somebody opened
// a settings dialog. That is the exact thing temporal/moduletest forbids and the reason core/selftest is a
// separate capability living next to the module instead of being derived from the surface.
//
// So the probe never touches /launch/. It performs two GETs and nothing else:
//
//	GET /api/v2/me/                      — who does this launch token authenticate as?
//	GET /api/v2/job_templates/{id}/      — for each SANCTIONED id: does it exist, can this token see it,
//	                                       and is it the template the operator meant?
//
// WHY THE SECOND READ IS THE POINT. A bare auth check (a green /me/ and stop) proves the token is live and the
// host is up, and would report success for an actuator whose allowlist names template 7 on an AWX where 7 is a
// completely different play — or where 7 does not exist at all. For a lane whose entire safety argument is
// "only these template ids, bound to these op-classes" (REQ-1704), a wrong id is the failure that matters
// most, and it is invisible until something reads the NAME back. The allowlist is a list of bare numbers; this
// is the only place those numbers are ever checked against reality before the owner-present flip arms them.
//
// WHAT A GREEN RESULT DOES NOT PROVE, stated here and repeated in the operator-facing Detail because a silent
// limit on a test is worse than no test:
//
//   - It does not prove the token may EXECUTE these templates. AWX settles execute permission at launch, and
//     the probe must not launch. Read access and execute access are separate AWX role grants; a token with
//     only the former passes this probe and refuses at the flip.
//   - It does not prove the launch will SUCCEED. The play itself, its inventory, its credentials and the hosts
//     it targets are untouched here.
//   - It says nothing about the mode chokepoint. The lane stays inert at Shadow regardless of this result;
//     configuring is not arming (see Descriptor's Summary).
//
// The probe travels the REAL path deliberately: a.client is the same *Client the launch uses, so the base URL,
// the CA pool, the SecretRef resolution and the cached-token behaviour under test are the ones that will be in
// force at launch. A probe with its own HTTP client would prove that second client works.
package awxjob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// maxProbedTemplates bounds the number of sanctioned templates re-read in one press.
//
// moduletest gives the whole activity 30 seconds with NO retry, and each read is a separate round trip against
// an AWX that is often behind a slow reverse proxy. An operator with twenty sanctioned templates would
// otherwise get a timeout — which reads as "AWX is down" and is the least useful answer available. Bounded, the
// probe always returns, and Summary states plainly how many of how many it actually looked at, so a partial
// read can never be mistaken for a full one.
const maxProbedTemplates = 5

// probeTarget names which of the probe's two reads produced a failure, so a diagnosis can be specific to it.
//
// It exists because the same HTTP status means different things on the two endpoints, and the WRONG advice is
// worse than none: a 404 on /api/v2/me/ says the base URL is not an AWX API, while a 404 on a job template
// says the base URL is fine and an allowlist entry is not. An operator sent to audit template ids because
// their base URL has a stray path prefix will not find anything, and will conclude the test is unreliable.
type probeTarget int

const (
	targetIdentity probeTarget = iota // GET /api/v2/me/
	targetTemplate                    // GET /api/v2/job_templates/{id}/
)

// identityEndpoint is how the identity read is named to the operator in a diagnosis.
const identityEndpoint = "the AWX identity endpoint /api/v2/me/"

// compile-time proof this module can honour the TEST button its descriptor advertises. Without it a rename or
// a signature drift in core/selftest would silently turn this back into an unreachable method, which is the
// same class of defect (a capability that exists and is never called) the whole exercise is closing.
var _ selftest.Tester = (*Actuator)(nil)

// SelfTest proves the AWX-job lane against the real AWX, READ-ONLY, WITHOUT LAUNCHING ANYTHING.
//
// The operator argument is ignored, and deliberately so: this probe leaves no trace in AWX for anyone to
// attribute. Attribution exists in core/selftest for the notifier, whose probe posts a message into a room
// humans watch during incidents. Two GETs against an API produce nothing a human will encounter and nothing to
// sign, so naming the operator here would be decoration. (AWX's own request log records the token's account,
// which is what an AWX admin would look at, and which this probe reports back.)
//
// It returns an error whenever the estate cannot be shown to match the configuration: an unreachable AWX, a
// rejected or unreadable token, or ANY sanctioned template that could not be re-read. That last one is on
// purpose — an allowlist entry AWX will not show us is an op-class that can only fail at the flip, and a test
// that reported that as a pass would be certifying the one thing nobody checked.
func (a *Actuator) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if a == nil || a.client == nil {
		// Reachable only through a zero-value Actuator: New refuses a nil client. Still handled, because the
		// alternative to an honest "unconfigured" here is a nil-pointer panic inside the activity, which the
		// console renders as an infrastructure fault rather than as the configuration problem it is.
		return selftest.Result{Summary: "the AWX-job lane has no launch client"},
			errors.New("awxjob: self-test needs a launch client — the lane is not configured (base URL / launch-token reference unset)")
	}
	target := a.client.baseURL

	// ── 1. WHO AM I ────────────────────────────────────────────────────────────────────────────────────
	// One GET that settles endpoint reachability, secret resolution and credential validity together, and
	// returns a comparable fact (the account name) rather than a bare 200.
	who, err := a.client.WhoAmI(ctx)
	if err != nil {
		return selftest.Result{
			Summary: "could not authenticate against AWX at " + target + " — nothing was launched",
			Detail:  classifyProbeFailure(err, identityEndpoint, targetIdentity),
		}, err
	}
	identity := describeIdentity(who)

	var notes []string
	if who.IsSuperuser {
		// Not a failure — a posture finding the operator can only see from here. The lane needs execute on a
		// handful of templates; a superuser token turns any future compromise of this secret into the whole
		// controller. Reported, never enforced: TG does not get to refuse an operator's credential choice.
		notes = append(notes, "this launch token belongs to a SUPERUSER account — the lane needs only execute "+
			"on the sanctioned templates, so a narrower account would bound the blast radius of this secret")
	}

	// ── 2. THE SANCTIONED TEMPLATES ────────────────────────────────────────────────────────────────────
	ids := a.sanctionedTemplateIDs()
	if len(ids) == 0 {
		// A real pass with a real caveat: everything that could be proven was proven, and the lane still
		// cannot launch anything, because an empty allowlist means Exec refuses every template (REQ-1704).
		return selftest.Result{
			Summary: fmt.Sprintf("reached AWX at %s as %s; no sanctioned templates to check — nothing was launched", target, identity),
			Detail: joinNotes(append(notes,
				"the sanctioned-templates list is EMPTY, so this lane can only refuse: with no allowlist entry "+
					"there is no template the actuator will launch. The endpoint and the launch token are proven; "+
					"the allowlist is not, because there is nothing in it to prove."), ""),
		}, nil
	}

	probed := ids
	skipped := 0
	if len(probed) > maxProbedTemplates {
		skipped = len(probed) - maxProbedTemplates
		probed = probed[:maxProbedTemplates]
	}

	var (
		seen     []string
		problems []string
		firstErr error
	)
	for _, id := range probed {
		// Respect the console's budget explicitly rather than relying on each round trip to notice: a
		// partially-completed sweep reported as a pass is exactly the false green this probe exists to deny.
		if cerr := ctx.Err(); cerr != nil {
			// Only if nothing has failed yet: a 403 on template 3 is a far more actionable answer than the
			// deadline that happened to arrive afterwards, and overwriting it would hand the operator the least
			// useful of the two faults.
			if firstErr == nil {
				firstErr = cerr
			}
			problems = append(problems, fmt.Sprintf("template %d and after: not checked — the test's time budget ran out", id))
			break
		}
		jt, terr := a.client.GetJobTemplate(ctx, id)
		if terr != nil {
			if firstErr == nil {
				firstErr = terr
			}
			problems = append(problems, fmt.Sprintf("template %d: %s", id,
				classifyProbeFailure(terr, "job template "+strconv.Itoa(id), targetTemplate)))
			continue
		}
		// The id printed is the one the operator SANCTIONED, not one taken from the response body: reporting a
		// served id back would let an endpoint that answers every template read with the same object look like a
		// clean sweep. GetJobTemplate has already refused any answer whose id is not the one it asked for, so
		// these are the same number — this keeps them the same number if that check is ever loosened.
		seen = append(seen, fmt.Sprintf("%d=%q", id, jt.Name))
		notes = append(notes, launchShapeNotes(id, jt, a.allowlist[id])...)
	}

	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d further sanctioned template(s) were NOT checked — this test reads "+
			"at most %d so it returns inside the console's time budget; press Test again after narrowing the "+
			"list, or check the rest in AWX directly", skipped, maxProbedTemplates))
	}
	// The ceiling of the probe, always said out loud. An operator who reads "green" as "the launch will work"
	// has been misled by us, not by AWX.
	notes = append(notes, "this proves the token can READ these templates; it does not prove it may EXECUTE "+
		"them — AWX only settles that at launch, and this test never launches")

	if firstErr != nil {
		return selftest.Result{
			Summary: fmt.Sprintf("reached AWX at %s as %s, but %d of %d checked sanctioned template(s) could not be read — nothing was launched",
				target, identity, len(problems), len(probed)),
			Detail: joinNotes(notes, strings.Join(problems, "; ")),
		}, fmt.Errorf("awxjob: self-test could not re-read every sanctioned job template: %w", firstErr)
	}

	return selftest.Result{
		Summary: fmt.Sprintf("reached AWX at %s as %s; re-read %d of %d sanctioned template(s): %s — nothing was launched",
			target, identity, len(seen), len(ids), strings.Join(seen, ", ")),
		Detail: joinNotes(notes, ""),
	}, nil
}

// sanctionedTemplateIDs returns the allowlisted template ids in a STABLE order.
//
// Map iteration order in Go is randomised, and an unordered Summary would make two consecutive presses look
// like two different answers — which teaches an operator to distrust the one control that is supposed to
// settle an argument. It also makes the truncation at maxProbedTemplates deterministic: the same ids are
// checked every time, rather than a random five.
func (a *Actuator) sanctionedTemplateIDs() []int {
	ids := make([]int, 0, len(a.allowlist))
	for id := range a.allowlist {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// describeIdentity renders the account the launch token belongs to.
//
// The username is the whole reason /api/v2/me/ is worth a round trip. REQ-1708 requires the launch token to be
// DISTINCT from the read-only sensor token the playbooks knowledge lane uses; if the two get crossed in the
// config, every check in this probe still passes — except that the name reported back is the sensor account's.
// Printing it is the only way an operator can catch that from the dialog.
func describeIdentity(id Identity) string {
	name := strings.TrimSpace(id.Username)
	if name == "" {
		// AWX always sends a username; an empty one means we are talking to something that answers on
		// /api/v2/me/ without being AWX. Say that rather than printing empty quotes.
		return fmt.Sprintf("an account that reported no username (user id %d)", id.ID)
	}
	return fmt.Sprintf("%q (user id %d)", name, id.ID)
}

// launchShapeNotes reports the misconfigurations this read can see that WOULD make a future launch refuse.
//
// This is the part of the probe that earns its round trip twice. Client.Launch treats an `ignored_fields` echo
// for a field it SENT as a launch REFUSAL, never a silent no-op (ErrLaunchFieldIgnored, REQ-1705) — and AWX
// ignores exactly those prompt-on-launch fields the template did not enable. So a template with
// ask_variables_on_launch=false will refuse every launch that carries extra_vars, and one with
// ask_limit_on_launch=false will refuse every launch that carries a host limit — which the runner always sets
// from the incident's target host (temporal/runner sealEffect).
//
// Today that defect is discoverable ONLY by launching, i.e. after the owner-present flip to a live mode, in
// the one situation where an unexpected refusal is least welcome. Reading two booleans off a GET the probe was
// making anyway moves the discovery to a settings dialog with the lane still inert.
//
// These are NOTES, not failures: the template exists and is readable, the configuration is intact, and TG has
// no standing to declare an operator's AWX template wrong. The probe reports; the operator decides.
func launchShapeNotes(id int, jt JobTemplate, pol TemplatePolicy) []string {
	var out []string
	if len(pol.ExtraVarsSchema) > 0 && !jt.AskVariablesOnLaunch {
		out = append(out, fmt.Sprintf("template %d (%q) has %d launch variable(s) declared in the sanctioned list, "+
			"but the template does not prompt for variables on launch (ask_variables_on_launch is off) — AWX would "+
			"drop them and this lane refuses such a launch rather than running a play with missing input. Enable "+
			"'Prompt on launch' for Variables on that template in AWX",
			id, jt.Name, len(pol.ExtraVarsSchema)))
	}
	if !jt.AskLimitOnLaunch {
		out = append(out, fmt.Sprintf("template %d (%q) does not prompt for a host limit on launch "+
			"(ask_limit_on_launch is off) — TG sets the limit from the incident's target host, and a launch "+
			"carrying a limit the template will not accept is refused. Enable 'Prompt on launch' for Limit, or "+
			"expect this template to be launchable only for incidents with no target host",
			id, jt.Name))
	}
	return out
}

// joinNotes assembles the operator-facing Detail: the failures first (they are what has to be fixed), then the
// advisory notes. Kept as one helper so the success and failure paths cannot drift into two different layouts.
func joinNotes(notes []string, failures string) string {
	parts := make([]string, 0, len(notes)+1)
	if strings.TrimSpace(failures) != "" {
		parts = append(parts, failures)
	}
	parts = append(parts, notes...)
	return strings.Join(parts, ". ")
}

// classifyProbeFailure turns an AWX API failure into something an operator can act on.
//
// It classifies on the SHAPE of the failure — the HTTP status code, or the fact that no HTTP response arrived
// at all — and NOT by parsing AWX's prose. Vendor wording changes between releases, and a reverse proxy in
// front of AWX substitutes its own error page entirely; a classifier that matched on text would quietly stop
// classifying and start guessing. Anything unrecognised falls through to the raw error rather than inventing a
// diagnosis, because a confident wrong diagnosis costs an operator more than no diagnosis (they go and fix the
// thing we named).
//
// what names the thing that was being read ("job template 7"), so the same sentence serves the identity read
// and the per-template reads without either having to be vague.
//
// target says WHICH of the two reads failed, and it is not decoration. 403 and 404 have entirely different
// causes and entirely different fixes on the two endpoints: a 404 on /api/v2/me/ means the base URL does not
// point at an AWX API at all, while a 404 on a template means the base URL is fine and an allowlisted ID is
// not. Telling an operator whose base URL is wrong to go and check their template ids is the "confident wrong
// diagnosis" this function's own comment warns about — they go and fix the thing we named.
//
// It never includes credential material: StatusError carries a scrubbed URL and AWX's own detail text, and the
// token is only ever an Authorization header. This text is pasted into tickets.
func classifyProbeFailure(err error, what string, target probeTarget) string {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusUnauthorized:
			return "AWX rejected the launch token (401) — it is wrong, expired, or has been revoked. Save a new " +
				"launch token, then RESTART the worker: the client caches the token for its lifetime, so a " +
				"rotation alone does not reach a running process"
		case http.StatusForbidden:
			if target == targetIdentity {
				return "the launch token reached AWX but was refused its own identity read (403 on " + what +
					") — the token itself is valid, so this is a SCOPE or a policy restriction on the account " +
					"rather than a missing object permission. Check that the launch token was issued with a " +
					"scope that permits reading, and that no AWX policy blocks this account"
			}
			return "the launch token authenticated but AWX refused the read of " + what + " (403) — the account " +
				"it belongs to lacks permission on that object. Grant it at least Read on the job template (a " +
				"launch will additionally need Execute)"
		case http.StatusNotFound:
			if target == targetIdentity {
				return "there is no AWX API at this base URL (404 on " + what + ") — something answered, but it " +
					"is not AWX. The base URL is the thing to fix here: a wrong host, a missing or extra path " +
					"prefix, or a reverse-proxy route that does not forward /api/ all look exactly like this. " +
					"The sanctioned-template ids are NOT implicated — nothing got far enough to check them"
			}
			return what + " does not exist on this AWX (404) — either the id in the sanctioned-templates list is " +
				"wrong, or this base URL points at a DIFFERENT AWX than the one those ids came from. Both are " +
				"launch-time refusals waiting to happen"
		case http.StatusTooManyRequests:
			return "AWX is rate-limiting this token (429) — the credential and the endpoint are fine; try again " +
				"shortly"
		}
		switch {
		case se.Status >= 500:
			return fmt.Sprintf("AWX answered %d for %s — the credential reached it, but AWX itself is failing. "+
				"This is an AWX-side fault, not a TG configuration one", se.Status, what)
		default:
			return fmt.Sprintf("AWX answered %d for %s: %s", se.Status, what, se.Detail)
		}
	}
	var te *TransportError
	if errors.As(err, &te) {
		if errors.Is(te, context.DeadlineExceeded) || errors.Is(te, context.Canceled) {
			return "the read of " + what + " did not finish inside the test's time budget — AWX is reachable but " +
				"very slow, or a proxy in front of it is holding the connection"
		}
		return "AWX could not be reached at " + te.URL + " — nothing ever read the credential, so this is a " +
			"network, DNS, TLS or host-down problem rather than a token one (" + te.Err.Error() + ")"
	}
	// TG-side secret faults, kept distinct from an AWX rejection: an operator told "AWX rejected the
	// credential" would go and mint a new AWX token for a problem that never left this process.
	if errors.Is(err, ErrTokenUnresolved) {
		return "the launch token could not be READ from the secret backend — the launch-token reference is " +
			"wrong, or the backend is unreachable. This is a TG-side problem, not an AWX one"
	}
	if errors.Is(err, ErrTokenEmpty) {
		return "the launch-token reference resolved but the value stored there is EMPTY — save the launch token " +
			"into the module's secret lane, then restart the worker"
	}
	return err.Error()
}
