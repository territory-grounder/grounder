package ldap

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	ldapv3 "github.com/go-ldap/ldap/v3"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof the module can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the module would silently degrade to "no
// test is implemented" — honest, but a dialog that promises a bind and performs none.
var _ selftest.Tester = (*Source)(nil)

const (
	// probeSizeLimit bounds each probe search at the SERVER. Sync's own limit is 2000 because it must import
	// the whole approver set; a probe only has to answer "does this bind see principals, and roughly how
	// many", and it answers it inside a 30-second activity with one attempt and no retry. A directory larger
	// than this reports "50+", which is the same operational answer.
	probeSizeLimit = 50
	// probeTimeLimit caps the per-search LDAP TimeLimit (seconds) so a slow replica fails over inside the
	// console's own bound rather than after it. Never RAISES the configured limit — a site that set a
	// stricter one keeps it.
	probeTimeLimit = 10
)

// probeCount is one bounded search's outcome: how many entries came back, how many of them carry the
// configured name attribute, and whether the server cut the result off at the size limit.
//
// found and named are separate on purpose. A search that returns entries none of which carries the
// configured id attribute (uid here, sAMAccountName on Active Directory) is not a working directory: Sync
// FAILS CLOSED on the first such entry rather than injecting a blank identity, so a probe that counted rows
// and stopped would report a healthy directory for a module that cannot sync a single principal.
type probeCount struct {
	found  int
	named  int
	capped bool
}

// SelfTest binds to the directory as the read-only service account and runs two bounded searches — the user
// base and the group base — over the module's REAL path: the same dialer, the same LDAPS/StartTLS transport
// with the same CA verification, and the same bind DN and password resolved from the same SecretRefs Sync
// resolves (ldap.go Sync). It walks the replicas in the SAME fixed order Sync does, and the first one that
// binds and searches answers — which is precisely the substitution risk the descriptor calls out, so the
// Summary names WHICH replica answered.
//
// WHY IT BINDS AND SEARCHES RATHER THAN JUST CONNECTING. A TCP/TLS connection proves a host is up. It passes
// with a rotated bind password, with a service account that has been locked out, and with a base DN that
// matches nothing — the three faults an operator presses TEST to rule out here. The bind password is the one
// field in this dialog that takes effect with no restart (Sync re-resolves it every time), so this button is
// also the only way to prove a password saved a moment ago actually works before the next sync depends on
// it.
//
// WHY BOTH BASES. Sync fails closed when users+groups is zero, and it maps group membership from the group
// base; a probe that searched only the user base would pass a directory whose group base DN is wrong, and
// approver eligibility comes from exactly those groups.
//
// WHAT A GREEN RESULT PROVES: a replica was reachable, its certificate verified, the service account bound,
// and the configured subtrees return principals carrying the configured name attribute. WHAT IT DOES NOT
// PROVE: that any particular person is in the directory, nor that memberOf carries the group CNs spec/015
// approve_by matches on — the probe reads names, not memberships.
//
// NOTHING IT RETURNS CARRIES THE CREDENTIAL. The bind DN is itself resolved from a SecretRef and is
// deliberately absent from the Summary; the password is never rendered, and go-ldap's errors carry the LDAP
// result code only.
//
// operator is ignored: this probe has no outward side effect, so there is no event in anyone's console that
// would need a named author.
func (s *Source) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if s == nil || len(s.servers) == 0 {
		return selftest.Result{
				Summary: "no directory server is configured",
				Detail:  "the module resolved to nothing — nothing was contacted. This is a TG wiring fault.",
			},
			fmt.Errorf("ldap: selftest: no servers")
	}

	// Resolved HERE, exactly as Sync resolves them, so a password saved in this dialog a second ago is the
	// one tested. A probe that reused a cached credential would report a rotation that had not happened.
	bindDN, err := s.bindDNRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the service bind DN could not be read",
			Detail: "the bind-DN reference could not be resolved, so NOTHING was sent to the directory. Fix " +
				"the reference (or the file/env it points at) — this is a TG-side fault, not a directory one.",
		}, fmt.Errorf("ldap: source %q selftest: resolve bind DN: %w", s.id, err)
	}
	bindPw, err := s.bindPwRef.Resolve()
	if err != nil {
		return selftest.Result{
			Summary: "the service bind password could not be read",
			Detail: "the bind-password reference could not be resolved from the secret backend, so NOTHING " +
				"was sent to the directory. Save the password in this dialog, or fix the reference the deploy " +
				"sets — this is a TG-side fault, not a directory one.",
		}, fmt.Errorf("ldap: source %q selftest: resolve bind password: %w", s.id, err)
	}
	if bindDN == "" || bindPw == "" {
		return selftest.Result{
			Summary: "the service bind DN or password is empty",
			Detail: "an empty credential is refused before it is sent: an LDAP simple bind with an empty " +
				"password is an ANONYMOUS bind, which many directories accept — and it would then read a " +
				"different, usually much smaller, view of the tree while looking like a success.",
		}, fmt.Errorf("ldap: source %q selftest: bind DN or password is empty (fail closed)", s.id)
	}

	var (
		failures []string // one per replica that did not answer, in the order they were tried
		lastErr  error
		tried    int
	)
	for _, ep := range s.servers {
		// ctx IS ONLY OBSERVABLE BETWEEN OPERATIONS HERE. go-ldap's Bind and Search take no context, and the
		// dialer's fallback timeout does not shrink once the deadline has already passed (timeoutFrom returns
		// the 15s default for a non-positive remainder). Without this check a two-replica estate answers a
		// cancelled 30-second activity by opening a FRESH 15-second dial to the next replica and then running
		// two more 20-second-capped operations against it — work nobody is waiting for, against a directory,
		// after the operator's spinner has gone.
		if err := ctx.Err(); err != nil {
			lastErr = err
			failures = append(failures, ep.url+": "+classifySelfTestFailure(err))
			break
		}
		tried++
		users, groups, err := s.probeOne(ctx, ep, bindDN, bindPw)
		if err != nil {
			lastErr = err
			failures = append(failures, ep.url+": "+classifySelfTestFailure(err))
			continue // failover, exactly as Sync fails over
		}
		return s.result(ep, users, groups, failures), nil
	}

	summary := fmt.Sprintf("no configured replica answered: all %d failed to bind or search", len(s.servers))
	if tried < len(s.servers) {
		// Saying "all N failed" when the run was cut short would report a whole-estate directory outage that
		// was never observed — the operator would go and wake somebody for the replicas nothing contacted.
		summary = fmt.Sprintf("the test ran out of time: %d of %d configured replicas were tried and none "+
			"bound and searched", tried, len(s.servers))
	}
	return selftest.Result{
			Summary: summary,
			Detail:  strings.Join(failures, " | "),
		},
		fmt.Errorf("ldap: source %q selftest: %d of %d replica(s) tried, all failed: %w",
			s.id, tried, len(s.servers), lastErr)
}

// probeOne binds against ONE replica and runs the two bounded searches. Any failure returns an error, which
// makes the caller fail over — the same shape as syncOne, so the probe cannot pass where the sync would move
// on, or fail where the sync would succeed.
func (s *Source) probeOne(ctx context.Context, ep serverEndpoint, bindDN, bindPw string) (probeCount, probeCount, error) {
	c, err := s.dial(ctx, ep.url) // unreachable / TLS-verify failure surfaces here
	if err != nil {
		// The wrapper says "connect" and NOT "connect/TLS" (the wording syncOne uses) on purpose: the
		// classifier below matches on the shape of the error TEXT, and a wrapper that always mentioned TLS
		// would make every refused connection report itself as a certificate fault.
		return probeCount{}, probeCount{}, fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	// Each of the three operations below is capped by the CONNECTION timeout (realDial's SetTimeout), not by
	// ctx — go-ldap takes no context. Checking between them is what keeps a run that has already exceeded the
	// console's bound from spending two more capped operations on it. The error is returned unwrapped in kind
	// (%w preserved) so classifySelfTestFailure's errors.Is arm names the timeout rather than diagnosing a
	// perfectly reachable directory as unreachable.
	if err := ctx.Err(); err != nil {
		return probeCount{}, probeCount{}, err
	}
	if err := c.Bind(bindDN, bindPw); err != nil {
		// The go-ldap error carries only the LDAP result code, never the password; do not add it.
		return probeCount{}, probeCount{}, fmt.Errorf("bind failed: %w", err)
	}

	users, err := s.probeSearch(c, s.userBase, s.userFilter, s.userIDAttr)
	if err != nil {
		return probeCount{}, probeCount{}, fmt.Errorf("user search: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return probeCount{}, probeCount{}, err
	}
	groups, err := s.probeSearch(c, s.groupBase, s.groupFilter, s.groupNameAttr)
	if err != nil {
		return probeCount{}, probeCount{}, fmt.Errorf("group search: %w", err)
	}
	// Sync's own rule, applied identically: a replica that returns no principals at all is not a directory
	// this source can use, and syncOne fails over on exactly this condition. Reporting it as a pass would
	// certify an approver set that will never populate.
	if users.found+groups.found == 0 {
		return users, groups, fmt.Errorf("the bind succeeded but both base DNs returned zero entries")
	}
	// An entry that does not carry the configured name attribute is not a partial success: entryToApprover
	// FAILS the whole sync on the first one rather than injecting a blank identity, so a probe that reported
	// this as a warning would show green for a module that cannot import a single principal. The commonest
	// cause is a directory that is not FreeIPA — Active Directory names the login attribute sAMAccountName,
	// and the default here is uid.
	if n := users.found - users.named; n > 0 {
		return users, groups, fmt.Errorf("%d of %d user entries carry no %q attribute", n, users.found, s.userIDAttr)
	}
	if n := groups.found - groups.named; n > 0 {
		return users, groups, fmt.Errorf("%d of %d group entries carry no %q attribute", n, groups.found, s.groupNameAttr)
	}
	return users, groups, nil
}

// probeSearch runs ONE bounded subtree search and counts what came back.
//
// It requests only the name attribute — the probe reports how many principals are visible, not who they are,
// and a memberOf list on a large directory is a lot of bytes for a question nobody asked. A server that cuts
// the result off at the size limit is NOT an error here: that is the bound working as intended, and the
// count is reported as "at least N".
func (s *Source) probeSearch(c Conn, baseDN, filter, nameAttr string) (probeCount, error) {
	size := probeSizeLimit
	if s.sizeLimit > 0 && s.sizeLimit < size {
		size = s.sizeLimit
	}
	timeLimit := s.timeLimit
	if timeLimit <= 0 || timeLimit > probeTimeLimit {
		timeLimit = probeTimeLimit
	}
	req := ldapv3.NewSearchRequest(
		baseDN, ldapv3.ScopeWholeSubtree, ldapv3.NeverDerefAliases,
		size, timeLimit, false, filter, []string{nameAttr}, nil,
	)
	res, err := c.Search(req)
	if err != nil && !isSizeLimit(err) {
		return probeCount{}, err
	}
	out := probeCount{capped: isSizeLimit(err)}
	if res == nil {
		return out, nil
	}
	out.found = len(res.Entries)
	for _, e := range res.Entries {
		if strings.TrimSpace(e.GetAttributeValue(nameAttr)) != "" {
			out.named++
		}
	}
	if out.found >= size {
		out.capped = true
	}
	return out, nil
}

// result renders a successful probe. It names the replica that answered, the subtrees searched and the
// counts, because that is what lets a human see they are looking at the wrong directory — the descriptor
// calls the replica list an AUTHORITY field for exactly this reason: whichever server answers first supplies
// the approver set outright.
func (s *Source) result(ep serverEndpoint, users, groups probeCount, failures []string) selftest.Result {
	summary := fmt.Sprintf("bound to %s as the configured service account: %s under %q and %s under %q",
		ep.url,
		count(users, "user", "users"), s.userBase,
		count(groups, "group", "groups"), s.groupBase)

	var notes []string
	if len(failures) > 0 {
		// A replica that is DOWN while a later one answers is invisible in a plain pass, and it is exactly
		// the state in which the next failure takes the approver directory with it.
		notes = append(notes, fmt.Sprintf("%s did not answer and the directory was read from a later "+
			"replica: %s", plural(len(failures), "replica", "replicas"), strings.Join(failures, " | ")))
	}
	if users.found == 0 {
		notes = append(notes, "no user entry matched under the user base DN: the DN or the user filter does "+
			"not match this directory's layout, so no PERSON will ever be resolvable as an approver.")
	}
	if groups.found == 0 {
		notes = append(notes, "no group entry matched under the group base DN: group membership is what "+
			"carries approver eligibility, so approve_by rules naming a group will never resolve.")
	}
	return selftest.Result{Summary: summary, Detail: strings.Join(notes, " ")}
}

// count renders one bounded search's result for display, saying "at least" when the server cut it off — a
// bare "50" would read as an exact estate size and quietly mislead on a directory of thousands.
func count(c probeCount, one, many string) string {
	if c.capped {
		return "at least " + plural(c.found, one, many)
	}
	return plural(c.found, one, many)
}

// isSizeLimit reports whether err is the SIZE LIMIT being reached rather than a fault. It matches both
// shapes go-ldap can produce: the server's LDAPResultSizeLimitExceeded (result code 4, returned WITH the
// partial entries) and the client-side ErrSizeLimitExceeded. Treating it as a fault would turn "this
// directory is bigger than the probe's bound" — the normal case on a real estate — into a red TEST.
func isSizeLimit(err error) bool {
	if err == nil {
		return false
	}
	return ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultSizeLimitExceeded) ||
		errors.Is(err, ldapv3.ErrSizeLimitExceeded)
}

// classifySelfTestFailure turns one replica's failure into something an operator can act on. "error" tells
// them nothing; "the service account bound but may not read that subtree" tells them exactly what to fix.
//
// It classifies on the SHAPE of the failure — the LDAP RESULT CODE first, then the transport class — never
// on the directory's prose, which differs between FreeIPA, 389-DS, OpenLDAP and Active Directory. Anything
// it cannot place falls through to the raw error rather than to an invented diagnosis.
func classifySelfTestFailure(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// FIRST, and deliberately so. context.DeadlineExceeded's text contains "deadline", which the transport
		// arm below matches — so without this case a probe cut short by the console's own 30-second bound
		// would be reported as "the replica could not be reached", sending an operator to check a network path
		// and a host that were never shown to be at fault.
		return "the test was CUT SHORT before this replica answered — the console bounds a module test at 30 " +
			"seconds with no retry, and the directory did not complete a bind and two searches inside it. " +
			"That is a slow or half-open replica rather than a wrong credential: nothing here says the bind " +
			"DN, the password or the base DNs are incorrect. Check the replica's own load and latency, and " +
			"put a responsive replica first in the ordered list."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultInvalidCredentials):
		return "the directory REJECTED THE BIND: the service account's DN or password is wrong, or the " +
			"account is locked or expired. The password is re-read on every sync, so saving a correct one " +
			"here takes effect immediately — no restart."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultInsufficientAccessRights):
		return "the service account bound successfully but may not read that subtree. Grant it a read ACI on " +
			"the user and group base DNs (read is enough — this connector never writes to the directory)."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultNoSuchObject):
		return "the base DN DOES NOT EXIST on this directory. The commonest cause is a missing site suffix: " +
			"the defaults are the generic FreeIPA layout (cn=users,cn=accounts) with no dc= component, which " +
			"matches nothing on a real directory. Set the full DN, e.g. " +
			"cn=users,cn=accounts,dc=example,dc=net."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultInvalidDNSyntax):
		return "the configured base DN is not a valid DN. Check for a stray comma, quote or space."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultTimeLimitExceeded):
		return "the directory hit its own time limit answering the search. The filter is most likely too " +
			"broad for the subtree, or the replica is overloaded."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultStrongAuthRequired),
		ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultConfidentialityRequired):
		return "the directory refuses a simple bind on an unencrypted connection. Use an ldaps:// URL, or " +
			"keep ldap:// and turn StartTLS on — a plain bind would put the service password on the wire in " +
			"clear text, which is why TG will not do it silently."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultReferral):
		return "the directory answered with a REFERRAL rather than data — this base DN is served by another " +
			"server. Point the replica list at the server that actually holds this subtree."
	case ldapv3.IsErrorWithCode(err, ldapv3.LDAPResultUnwillingToPerform):
		return "the directory refused to perform the operation. On FreeIPA/389-DS this is usually an " +
			"anonymous-read restriction or a disabled account rather than a wrong password."
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "x509"), strings.Contains(s, "certificate"), strings.Contains(s, "tls"),
		strings.Contains(s, "starttls"):
		return "the replica's TLS certificate could not be verified, so TG refused to send the service bind " +
			"password to it. Point the CA certificate reference at the PEM that issued the directory's " +
			"certificate (FreeIPA's own CA on this estate); verification is never skipped."
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"), strings.Contains(s, "no such host"),
		strings.Contains(s, "connection refused"), strings.Contains(s, "connection reset"), strings.Contains(s, "eof"):
		return "the replica could not be reached — check the URL resolves, that the host is up, and that the " +
			"worker is allowed to reach it on that port (636 for ldaps, 389 for ldap)."
	case strings.Contains(s, "zero entries"):
		return "the bind succeeded but BOTH base DNs returned nothing. The credential is fine and the " +
			"subtrees are wrong (or empty): Sync treats this replica as unusable and fails over, so no " +
			"approver identity is learned from it."
	case strings.Contains(s, "carry no"):
		return "the bind and the search both worked, but entries came back WITHOUT the configured name " +
			"attribute. Sync fails closed on the first of those rather than injecting a blank identity, so " +
			"nothing is imported at all. Either the filter is matching objects that are not principals, or " +
			"the attribute is wrong for this directory — Active Directory uses sAMAccountName where FreeIPA " +
			"uses uid. Raw: " + err.Error()
	default:
		return err.Error()
	}
}

// plural renders a count with its noun so the Summary reads as a sentence rather than as a log line: an
// operator reading "1 users" wonders whether the probe counted correctly.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
