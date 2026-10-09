package semaphore

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof the module can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the module would silently degrade to "no
// test is implemented" — honest, but a dialog that promises a read and performs none.
var _ selftest.Tester = (*Source)(nil)

// SelfTest reads the project this source is scoped to and lists that project's inventories, over the
// module's REAL path: the same client, the same Bearer token resolved from the same SecretRef
// (semaphore.go token()), the same base URL, the same off-host guard.
//
// WHY THESE TWO READS AND NOT Ping(). Ping() hits /api/ping unauthenticated. It proves a host is up and
// nothing else: it passes with a revoked token, with a token for a different Semaphore, and with an API user
// removed from every project — the three faults an operator presses TEST to rule out. The project read is
// the FIRST request Sync makes (listProjects), and the inventory list is the request whose permission the
// sync actually depends on; together they exercise the token, the project scope, and the read the estate's
// host identities come from.
//
// WHY IT NAMES A PROJECT rather than counting everything. Sync walks every project's keys AND inventories;
// on a shared Semaphore that is a request per project per refresh, which is far too much for a settings
// dialog holding an operator on a spinner with a 30-second bound and no retry. Reading ONE project's
// inventories exercises the identical permission on the identical path, bounded to two requests.
//
// WHAT A GREEN RESULT PROVES: Semaphore was reachable over verified TLS, the token resolved from the secret
// backend and was accepted, and the API user can see this project and list its inventories. WHAT IT DOES NOT
// PROVE: that the Key Store read (/keys) is permitted — that is a separate request Sync makes and this probe
// deliberately does not, because a probe must not certify a permission it never exercised — nor that the
// inventory text parses into usable host identities.
//
// operator is ignored: this probe has no outward side effect, so there is no event in anyone's console that
// would need a named author.
func (s *Source) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if s == nil || s.client == nil {
		return selftest.Result{
				Summary: "no Semaphore client is wired",
				Detail: "the module resolved to nothing — no request was made. This is a TG wiring fault, not a " +
					"Semaphore one.",
			},
			fmt.Errorf("semaphore: selftest: nil client")
	}

	// listProjects is the source's OWN scoping rule: a configured project id reads that one project object,
	// an empty one lists every project the token can see. Reusing it is what keeps the probe testing the
	// scope the sync uses rather than a wider read the sync never performs.
	projects, err := s.listProjects(ctx)
	if err != nil {
		return selftest.Result{
			Summary: "could not read the project list from Semaphore at " + s.instanceLabel(),
			Detail:  classifySelfTestFailure(err, "read that project"),
		}, err
	}

	if len(projects) == 0 {
		// A 200 with an empty array: the token is valid but sees nothing. Semaphore enforces project access
		// by FILTERING the list, so "no projects" and "no permission" are the same response — and either way
		// this source will import nothing. That is a pass (the credential is proven) that must never read as
		// an unqualified success.
		return selftest.Result{
			Summary: "reached Semaphore at " + s.instanceLabel() + ": the token was accepted, but it can see " +
				"no projects at all",
			Detail: "Semaphore enforces project access by filtering the list, so a token with no project " +
				"membership looks exactly like an empty Semaphore here. This source will contribute no host " +
				"identities until the API user is added to the project that holds the inventories.",
		}, nil
	}

	target := projects[0]
	inventories, err := s.listInventories(ctx, target.ID)
	if err != nil {
		return selftest.Result{
			Summary: fmt.Sprintf("reached Semaphore at %s and read project %d, but could not list its inventories",
				s.instanceLabel(), target.ID),
			Detail: classifySelfTestFailure(err, "list that project's inventories"),
		}, err
	}

	// Sync can only map an inventory whose host text is served INLINE ("static"/"static-yaml"); "file" and
	// "terraform-workspace" inventories live outside the API and are skipped-with-record. Counting the two
	// separately is what distinguishes "12 inventories, all of them usable" from "12 inventories, none of
	// which this connector can read a host out of" — the latter looks identical in a plain count.
	inline := 0
	for _, inv := range inventories {
		if inv.Type == "static" || inv.Type == "static-yaml" {
			inline++
		}
	}

	summary := fmt.Sprintf("read Semaphore at %s: project %q (id %d) has %s",
		s.instanceLabel(), target.Name, target.ID, plural(len(inventories), "inventory", "inventories"))
	if s.projectID <= 0 {
		summary += fmt.Sprintf(", of %s this token can see", plural(len(projects), "project", "projects"))
	}
	if inline != len(inventories) {
		summary += fmt.Sprintf(" (%d with inline host text)", inline)
	}

	detail := ""
	switch {
	case len(inventories) == 0:
		detail = "the token was accepted and the project is readable, but it holds no inventories, so this " +
			"source will contribute no host identities from it."
	case inline == 0:
		detail = "none of this project's inventories carries inline host text: \"file\" and " +
			"\"terraform-workspace\" inventories live in the linked repository or in Terraform state, which " +
			"this read-only connector cannot see. It will skip every one of them and contribute no host " +
			"identities."
	}
	return selftest.Result{Summary: summary, Detail: detail}, nil
}

// instanceLabel renders the configured endpoint for display, and it renders the HOST ONLY.
//
// Naming the instance is the point of the Summary — "reached Semaphore" cannot distinguish production from a
// clone, "reached Semaphore at semaphore.example:3000" can. Printing the raw base URL would be simpler and
// wrong: a URL may legally carry userinfo (http://user:token@semaphore.example), and Result is rendered in a
// dialog and pasted into tickets, so the one string that must never carry credential material is exactly
// this one. url.Host drops userinfo by construction; a URL too malformed to parse degrades to a phrase
// rather than to its own raw text.
func (s *Source) instanceLabel() string {
	if u, err := url.Parse(s.client.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured Semaphore URL"
}

// classifySelfTestFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "the token authenticated but the API user is not a member of that project" tells them exactly
// what to fix. what names the step that failed, so the same classifier serves both reads without pretending
// a token fault is a membership one.
//
// It classifies on the SHAPE of the failure — the HTTP status first, then the transport class — and never on
// Semaphore's prose, which differs between versions. Anything it cannot place falls through to the raw error
// rather than to an invented diagnosis: a wrong diagnosis sends an operator to re-issue a token that was
// never the problem.
func classifySelfTestFailure(err error, what string) string {
	switch code := statusFromSemaphoreError(err); {
	case code == 401:
		// WHICH token this was matters, and getting it backwards costs an operator a whole diagnosis loop.
		// Client.token() caches the resolved token for the process lifetime — the descriptor's header note
		// calls that out at length and marks the token field EffectRestart because of it. So on a worker
		// that has already synced once, this button tested the token THIS PROCESS IS HOLDING, not one saved
		// in the dialog a moment ago; "save a new token and test again" would have an operator press a red
		// button twice and conclude the replacement is broken too.
		return "Semaphore REJECTED THE TOKEN — it is wrong, expired, or has been deleted from the API-token " +
			"list. Note WHICH token this tested: the client caches its API token at first use, so on a " +
			"worker that has already synced this is the token the running PROCESS holds, not one saved in " +
			"this dialog since it started (the token field is restart-effect for exactly that reason). Save " +
			"a new token, restart the worker, then test again — a green result then also means the sync is " +
			"using the new token."
	case code == 403:
		return "the token authenticated but the API user may not " + what + ". In Semaphore this is project " +
			"MEMBERSHIP, not a global role: add the user to the project this source syncs, as a viewer — " +
			"read is enough, and this credential must never be able to run a task."
	case code == 404:
		return "Semaphore answered 404. Either the configured project id does not exist on this instance " +
			"(check it, or leave it empty to sync every project the token can see), or the base URL is not a " +
			"Semaphore API root — it must be the scheme+host+port Semaphore is served on, with no /api suffix."
	case code >= 500:
		return fmt.Sprintf("Semaphore answered with a server error (status %d). The URL and the token are "+
			"reaching it, so this is a Semaphore-side fault rather than a TG configuration one.", code)
	case code != 0:
		return fmt.Sprintf("Semaphore refused the read with status %d.", code)
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve token"), strings.Contains(s, "token is empty"):
		return "the API token could not be READ from the secret backend — the token reference is wrong, or " +
			"the backend is unreachable. NOTHING was sent to Semaphore: this is a TG-side problem."
	case strings.Contains(s, "off-host"):
		return "the request URL resolved to a DIFFERENT host than the configured base, and TG refused to send " +
			"the Bearer token there. Check the base URL."
	case strings.Contains(s, "x509"), strings.Contains(s, "certificate"), strings.Contains(s, "tls"):
		return "the TLS certificate could not be verified — TG refuses to send its token to a host it cannot " +
			"authenticate. Point the CA certificate path at the private CA that issued the Semaphore " +
			"certificate; do not work around it by switching the URL to http."
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"), strings.Contains(s, "no such host"),
		strings.Contains(s, "connection refused"), strings.Contains(s, "connection reset"), strings.Contains(s, "eof"):
		return "Semaphore could not be reached — check the base URL resolves (Semaphore is commonly served on " +
			"port 3000, which is easy to omit), that the host is up, and that the worker is allowed to reach " +
			"it on that port."
	default:
		return err.Error()
	}
}

// statusFromSemaphoreError recovers the HTTP status from the error raw() formats:
//
//	semaphore: GET http://semaphore.example:3000/api/projects: status 401: unauthorized
//
// It reads the connector's OWN frame — the first ": status " in the string, written before Semaphore's body
// is appended — rather than searching the whole error for a three-digit number. That distinction is what
// keeps an error body that happens to mention 403 from being reported as a membership fault when the real
// status was 500. A transport failure has no status and yields 0, which routes classification to the
// transport arm instead.
func statusFromSemaphoreError(err error) int {
	const marker = ": status "
	s := err.Error()
	i := strings.Index(s, marker)
	if i < 0 {
		return 0
	}
	digits := s[i+len(marker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	code, convErr := strconv.Atoi(digits[:end])
	if convErr != nil {
		return 0
	}
	return code
}

// plural renders a count with its noun so the Summary reads as a sentence rather than as a log line: an
// operator reading "1 inventories" wonders whether the probe counted correctly. It takes both forms because
// "inventory"/"inventories" is not an -s plural.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
