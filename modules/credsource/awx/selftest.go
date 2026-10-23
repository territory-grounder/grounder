package awx

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

// probeMePath identifies the ACCOUNT behind the saved token: AWX's /api/v2/me/ returns the one user the
// request authenticated as.
//
// WHY IT IS WORTH A SECOND REQUEST. A host count alone cannot tell an operator WHICH credential is in use,
// and the commonest AWX misconfiguration in this estate is not a broken token but the wrong one — the job
// launcher's write-capable token pasted into the read-only inventory dialog, or a token minted against the
// staging AWX. Naming the account puts that in front of the operator in the Summary. It is also the
// cleanest 401 in the API: /me/ needs no object permission at all, so a failure here is the TOKEN, never a
// permission, which is what lets the hosts read below be classified as a permission fault with confidence.
const probeMePath = "/api/v2/me/"

// SelfTest reads the account behind the token and then ONE bounded page of the configured inventory's hosts,
// over the module's REAL path: the same client, the same Bearer token resolved from the same SecretRef
// (awx.go token()), the same base URL, the same off-host guard.
//
// WHY THE HOSTS LIST. It is the first request Sync makes on every refresh, and it needs exactly the
// permission the sync needs; the paginated envelope's `count` reports the inventory's TOTAL size without
// pulling it, which is what makes a green Test able to reveal a module pointed at the wrong AWX. Ping() —
// the client's other cheap read — is deliberately NOT used: it is unauthenticated, so it passes with a
// revoked token, with a permission never granted, and with a token belonging to a different AWX entirely.
//
// WHY page_size=1 AND NOT paginate(). Sync walks the whole .next chain, which on this estate is thousands of
// hosts; a settings dialog holds an operator on a spinner with a 30-second bound and one attempt. The count
// comes from the envelope rather than from the results, so one row answers the question the whole walk
// would.
//
// WHAT A GREEN RESULT PROVES: AWX was reachable over verified TLS, the token resolved from the secret
// backend and was accepted, and the account behind it may read the inventory this source syncs. WHAT IT DOES
// NOT PROVE: that the job-template mode will work (that reads /credentials/ and /job_templates/, separate
// permissions this probe deliberately does not exercise), nor that the hosts carry usable connection vars —
// a host with no ansible_user is skipped at sync time and no read can predict that here.
//
// operator is ignored: this probe has no outward side effect, so there is no event in anyone's console that
// would need a named author.
func (s *Source) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if s == nil || s.client == nil {
		return selftest.Result{
				Summary: "no AWX client is wired",
				Detail:  "the module resolved to nothing — no request was made. This is a TG wiring fault, not an AWX one.",
			},
			fmt.Errorf("awx: selftest: nil client")
	}

	// 1. WHO the token is. A failure here is the credential itself.
	var me page[struct {
		Username string `json:"username"`
	}]
	if err := s.client.getJSON(ctx, probeMePath, &me); err != nil {
		return selftest.Result{
			Summary: "could not authenticate to AWX at " + s.instanceLabel(),
			Detail:  classifySelfTestFailure(err, "identify the account"),
		}, err
	}
	account := "an account AWX did not name"
	if len(me.Results) > 0 && strings.TrimSpace(me.Results[0].Username) != "" {
		account = strings.TrimSpace(me.Results[0].Username)
	}

	// 2. WHAT it can see. A failure here is a permission, because step 1 already proved the token.
	var hosts page[awxHost]
	if err := s.client.getJSON(ctx, s.probeHostsPath(), &hosts); err != nil {
		return selftest.Result{
			Summary: "reached AWX at " + s.instanceLabel() + " as " + account + ", but could not list hosts",
			Detail:  classifySelfTestFailure(err, "list inventory hosts"),
		}, err
	}

	scope := "across every inventory this token can see"
	if s.inventoryID > 0 {
		scope = "in inventory " + strconv.Itoa(s.inventoryID)
	}
	summary := fmt.Sprintf("read AWX at %s as %s: %s %s",
		s.instanceLabel(), account, plural(hosts.Count, "host"), scope)

	detail := ""
	if hosts.Count == 0 {
		detail = s.emptyInventoryDetail(ctx)
	}
	return selftest.Result{Summary: summary, Detail: detail}, nil
}

// probeHostsPath is the hosts list bounded to ONE row, scoped to the configured inventory exactly as
// Sync scopes it (listPath). Sharing the scoping rule is the point: a probe that ignored the configured
// inventory id would pass against an estate-wide read the sync never performs.
func (s *Source) probeHostsPath() string {
	q := url.Values{}
	q.Set("page_size", "1")
	if s.inventoryID > 0 {
		q.Set("inventory", strconv.Itoa(s.inventoryID))
	}
	return "/api/v2/hosts/?" + q.Encode()
}

// emptyInventoryDetail explains a zero host count, and it spends ONE extra request to do so only in that
// case.
//
// A scoped source that reports no hosts has two very different causes that look identical in the list
// response: the inventory id names something that does not exist (AWX FILTERS on it and returns an empty
// page rather than 404-ing), or the inventory exists and is empty. The first is a configuration fault an
// operator can fix in this dialog; the second is an estate fact. Reading the inventory object itself is what
// separates them, and it is worth the round trip precisely because the ambiguity is invisible otherwise.
func (s *Source) emptyInventoryDetail(ctx context.Context) string {
	if s.inventoryID <= 0 {
		return "the token was accepted but AWX reports no hosts in any inventory it can see. Either this AWX " +
			"genuinely has no hosts, or the token's user has no inventory permissions — AWX enforces those by " +
			"filtering list results, so a permission problem looks exactly like an empty estate here. This " +
			"source will contribute no host identities."
	}
	var inv awxInventory
	err := s.client.getJSON(ctx, "/api/v2/inventories/"+strconv.Itoa(s.inventoryID)+"/", &inv)
	if err != nil {
		if statusFromAWXError(err) == 404 {
			return fmt.Sprintf("the token was accepted, but inventory %d DOES NOT EXIST on this AWX. The "+
				"inventory id is wrong, or it belongs to a different AWX instance — either way this source "+
				"will contribute nothing. Correct the inventory id (leave it empty to sync every inventory "+
				"the token can see).", s.inventoryID)
		}
		return fmt.Sprintf("the token was accepted, but inventory %d returned no hosts and the inventory "+
			"itself could not be read to say why (%s). This source will contribute no host identities.",
			s.inventoryID, err.Error())
	}
	return fmt.Sprintf("the token was accepted and inventory %d exists (%q), but it contains no hosts this "+
		"token can see. Either the inventory is genuinely empty, or the user's object permissions filter its "+
		"hosts away — AWX enforces those by filtering list results. This source will contribute no host "+
		"identities until that is resolved.", s.inventoryID, inv.Name)
}

// instanceLabel renders the configured endpoint for display, and it renders the HOST ONLY.
//
// Naming the instance is the point of the Summary — "reached AWX" cannot distinguish production from the
// staging clone, "reached AWX at awx.example" can. Printing the raw base URL would be simpler and wrong: a
// URL may legally carry userinfo (https://user:token@awx.example), and Result is rendered in a dialog and
// pasted into tickets, so the one string that must never carry credential material is exactly this one.
// url.Host drops userinfo by construction; a URL too malformed to parse degrades to a phrase rather than to
// its own raw text.
func (s *Source) instanceLabel() string {
	if u, err := url.Parse(s.client.baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured AWX URL"
}

// classifySelfTestFailure turns a failed read into something an operator can act on. "error" tells them
// nothing; "the token authenticated but its user cannot read that inventory" tells them exactly which
// permission to grant. what names the step that failed, so the same classifier can serve both reads without
// pretending a token fault is a permission fault.
//
// It classifies on the SHAPE of the failure — the HTTP status first, then the transport class — and never on
// AWX's prose, which differs between versions. Anything it cannot place falls through to the raw error
// rather than to an invented diagnosis: a wrong diagnosis sends an operator to re-issue a token that was
// never the problem.
func classifySelfTestFailure(err error, what string) string {
	switch code := statusFromAWXError(err); {
	case code == 401:
		// WHICH token this was matters, and getting it backwards costs an operator a whole diagnosis loop.
		// Client.token() caches the resolved token for the process lifetime, which is why the descriptor
		// marks the token field EffectRestart. So on a worker that has already synced once, this button
		// tested the token THIS PROCESS IS HOLDING — not one saved in the dialog a moment ago. Telling an
		// operator to "save a new token and test again" would have them press a red button twice and
		// conclude the replacement is broken too.
		return "AWX REJECTED THE TOKEN — it is wrong, expired, or has been revoked. Note WHICH token this " +
			"tested: the client caches its API token at first use, so on a worker that has already synced " +
			"this is the token the running PROCESS holds, not one saved in this dialog since it started " +
			"(the token field is restart-effect for exactly that reason). Save a new read-only OAuth2 " +
			"token, restart the worker, then test again — a green result then also means the sync is using " +
			"the new token."
	case code == 403:
		return "the token authenticated but the account behind it may not " + what + ". Grant its user or " +
			"team the READ role on the inventory this source syncs — read is enough, and this credential must " +
			"never be able to launch anything."
	case code == 404:
		return "AWX answered 404 for a core API path. The base URL is most likely not an AWX API root — it " +
			"must be the scheme+host AWX is served on, with no /api suffix and no path prefix."
	case code == 429:
		return "AWX is rate-limiting this token. The read was refused rather than failed — retry, and if it " +
			"persists check what else is using this credential."
	case code >= 500:
		return fmt.Sprintf("AWX answered with a server error (status %d). The URL and the token are reaching "+
			"it, so this is an AWX-side fault rather than a TG configuration one.", code)
	case code != 0:
		return fmt.Sprintf("AWX refused the read with status %d.", code)
	}

	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "resolve token"), strings.Contains(s, "token is empty"):
		return "the API token could not be READ from the secret backend — the token reference is wrong, or " +
			"the backend is unreachable. NOTHING was sent to AWX: this is a TG-side problem, not an AWX one."
	case strings.Contains(s, "off-host"):
		return "AWX returned a pagination link pointing at a DIFFERENT host, and TG refused to follow it " +
			"rather than send the Bearer token somewhere else. Check the base URL matches the hostname AWX " +
			"advertises in its own API links (a reverse proxy rewriting Host is the usual cause)."
	case strings.Contains(s, "x509"), strings.Contains(s, "certificate"), strings.Contains(s, "tls"):
		return "the TLS certificate could not be verified — TG refuses to send its token to a host it cannot " +
			"authenticate. Point the CA certificate path at the private CA that issued the AWX certificate; " +
			"do not work around it by switching the URL to http."
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"), strings.Contains(s, "no such host"),
		strings.Contains(s, "connection refused"), strings.Contains(s, "connection reset"), strings.Contains(s, "eof"):
		return "AWX could not be reached — check the base URL resolves, that the host is up, and that the " +
			"worker is allowed to reach it on that port."
	default:
		return err.Error()
	}
}

// statusFromAWXError recovers the HTTP status from the error raw() formats:
//
//	awx: GET https://awx.example/api/v2/hosts/: status 403: You do not have permission…
//
// It reads the connector's OWN frame — the first ": status " in the string, written before AWX's body is
// appended — rather than searching the whole error for a three-digit number. That distinction is what keeps
// an AWX error body that happens to mention 403 from being reported as a permission fault when the real
// status was 500. A transport failure has no status and yields 0, which routes classification to the
// transport arm instead.
func statusFromAWXError(err error) int {
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
// operator reading "1 hosts visible" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
