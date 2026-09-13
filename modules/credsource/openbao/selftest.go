package openbao

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/selftest"
)

// compile-time proof the module can answer the console's TEST button. The capability is OPTIONAL and
// detected by assertion (core/selftest.Of), so without this line the module would silently degrade to "no
// test is implemented" — honest, but a dialog that promises a LIST and performs none.
var _ selftest.Tester = (*Module)(nil)

// Module is what NewSource hands back: the shared OpenBao/Vault KV v2 CredentialSource, plus the two facts
// the TEST button needs and the embedded source does not expose — the authenticated client, and the
// mount+prefix that source is scoped to.
//
// WHY A WRAPPER TYPE EXISTS AT ALL. This package is a thin re-export of modules/credsource/vault (OpenBao is
// wire-identical to Vault), so its Source and Client are ALIASES for types declared in that package. Go does
// not allow a method on a type declared elsewhere, so `func (s *Source) SelfTest(...)` cannot be written
// here, and writing it in the vault package would attach a console capability to a shared library that has
// no descriptor and no dialog. Embedding is the honest alternative: ID/Plane/Sync remain the vault source's
// own implementations, unchanged and untouched, and only the probe is added.
//
// It also keeps the composition root free of a special case. modules/bootstrap.buildOpenBaoSource returns
// this value as a credential.CredentialSource; because the probe rides ON that value, the worker's probe
// registry finds it with the same selftest.Of() call it uses for every other module, rather than needing a
// second construction site that somebody has to remember to add.
type Module struct {
	// Source is the real KV v2 CredentialSource (embedded, so ID/Plane/Sync are its methods, not ours).
	*Source

	// client is the SAME authenticated client the Source syncs with — not a second one built for the probe.
	// A probe with its own client would prove that some credential works, not that THIS module's does.
	client *Client
	// mount/prefix are the normalised KV v2 coordinates (see NewSource), which is what makes the probe read
	// the path this module actually syncs rather than a hardcoded one.
	mount  string
	prefix string
}

// probeSampleKeys bounds how many key NAMES the Summary renders.
//
// The sample is what turns a pass into evidence: an operator who knows their estate recognises these names,
// and recognises immediately when they belong to the wrong OpenBao. It is a fixed cap rather than "all of
// them" so the line cannot grow with the mount, and it is names ONLY — the descriptor's verb promises "key
// names only, no secret value is read", and this probe never issues a KV read.
const probeSampleKeys = 3

// SelfTest LISTs the configured KV v2 mount+prefix with the module's own credential — the same first request
// Source.Sync makes on every refresh (vault.go: Sync → ListKV on the metadata path).
//
// WHY THE LIST AND NOT sys/health. The client also exposes Health(), which is one unauthenticated GET and
// would be a far cheaper probe — and a worthless one here. sys/health carries no token, so it passes with a
// revoked token, with an AppRole whose SecretID was consumed, and with a policy that grants nothing: three of
// the exact faults an operator presses TEST to rule out. It is also wrong in the other direction, because
// sys/health answers 429 on a STANDBY node and Health() does not spend the retry budget, so a healthy cluster
// behind the round-robin VIP would report a failure roughly two times in three. The authenticated LIST goes
// through authed() → tokenNow() → retrying(), which is the login, the token cache, the 403 re-login and the
// standby budget that production actually depends on.
//
// WHAT A GREEN RESULT PROVES: OpenBao was reachable over verified TLS, the configured auth method completed
// (AppRole/JWT/cert login or a static token), and the resulting policy permits LIST on this mount+prefix.
// WHAT IT DOES NOT PROVE: that any individual secret READS (a policy may grant list and deny read, so Sync
// can still fail on the first entry), nor that the entries under the prefix have the host-bundle shape Sync
// requires. A probe must not certify a permission it never exercised, and this one deliberately issues no
// read: the verb promises key names only.
//
// The address is deliberately absent from the Summary. vault.Client keeps its base URL unexported and this
// package must not fork the client to expose it, so the mount, prefix and key sample are what let a human
// see they are looking at the wrong instance. See the note in the module report.
//
// operator is ignored: this probe has no outward side effect, so there is no event in anyone's console that
// would need a named author.
func (m *Module) SelfTest(ctx context.Context, _ string) (selftest.Result, error) {
	if m == nil || m.client == nil {
		return selftest.Result{
				Summary: "no OpenBao client is wired",
				Detail: "the module resolved to nothing — no request was made. This is a TG wiring fault, not " +
					"an OpenBao one.",
			},
			fmt.Errorf("openbao: selftest: nil client")
	}

	path := m.metadataPath()
	keys, err := m.client.ListKV(ctx, path)
	if err != nil {
		// A 404 on a KV v2 metadata LIST is not a failure and must not be reported as one: KV v2 answers 404
		// for a path that simply holds no keys, and Source.Sync treats exactly this case as a
		// legitimately-empty source (0 entries, outcome=ok). The credential was still proven — a rejected one
		// answers 403, never 404 — so this is a PASS whose warning names the misconfiguration it is
		// indistinguishable from: a wrong mount or a wrong prefix looks identical to an empty one.
		if statusFromVaultError(err) == 404 {
			return selftest.Result{
				Summary: "OpenBao accepted the credential, but " + path + " holds no keys",
				Detail: "the LIST returned 404, which KV v2 also uses for an empty path — so this is either a " +
					"genuinely empty subtree or a wrong mount/prefix, and the two look the same on the wire. " +
					"Sync treats it as a source with zero entries, so no host binding is learned and every " +
					"lookup falls through to a lower-precedence source. If bindings are expected here, check " +
					"the KV v2 mount and path prefix.",
			}, nil
		}
		return selftest.Result{
			Summary: "could not list " + path,
			Detail:  classifySelfTestFailure(err),
		}, err
	}

	// Sync imports ONE flat level: a key ending in '/' is a sub-path it skips. Counting the two separately is
	// what distinguishes "this prefix has 40 host bundles" from "this prefix has 40 FOLDERS and imports
	// nothing", which is the shape of a prefix set one level too high.
	var leaves, folders []string
	for _, k := range keys {
		if strings.HasSuffix(k, "/") {
			folders = append(folders, k)
			continue
		}
		leaves = append(leaves, k)
	}

	summary := "listed " + path + " as this module's own credential: " + plural(len(leaves), "key") + " visible"
	if len(folders) > 0 {
		summary += " (plus " + plural(len(folders), "sub-path") + ", which Sync does not descend into)"
	}
	if n := len(leaves); n > 0 {
		sample := leaves
		if n > probeSampleKeys {
			sample = sample[:probeSampleKeys]
		}
		summary += " — " + strings.Join(sample, ", ")
		if n > len(sample) {
			summary += ", …"
		}
	}

	detail := ""
	switch {
	case m.prefix == "":
		// The source is OFF in this state and the operator cannot see that from the dialog: vault.go's Sync
		// returns 0 entries immediately when the prefix is empty, WITHOUT listing anything, precisely so a
		// shared mount root is never imported wholesale. The probe listed the root to prove the credential;
		// it must say plainly that Sync will not.
		detail = "no path prefix is configured, so this source is DISABLED: Sync returns zero entries without " +
			"reading anything, and the keys above were listed by this probe alone. Point the prefix at a " +
			"dedicated host-bundle subtree (e.g. \"tg/hosts\") to enable it."
	case len(leaves) == 0 && len(folders) > 0:
		detail = "every entry under this prefix is a sub-path, and Sync imports one flat level only — it will " +
			"learn no host bindings from here. The prefix is most likely one level above the bundles."
	case len(leaves) == 0:
		detail = "the credential and the path are proven, but the prefix is empty, so Sync will learn no host " +
			"bindings from this source."
	}
	return selftest.Result{Summary: summary, Detail: detail}, nil
}

// metadataPath rebuilds the KV v2 LIST path for the configured mount+prefix — the same path Source.Sync
// lists (vault.go kvPath("metadata", "")). It is reconstructed rather than borrowed because vault keeps that
// helper unexported; the inputs are the SAME normalised mount and prefix the source was built from, so the
// two cannot disagree about which subtree is being tested.
func (m *Module) metadataPath() string {
	p := m.mount + "/metadata"
	if m.prefix != "" {
		p += "/" + m.prefix
	}
	return p
}

// classifySelfTestFailure turns a failed LIST into something an operator can act on. "error" tells them
// nothing; "the credential authenticated but its policy does not allow list here" tells them which policy to
// fix.
//
// It classifies on the SHAPE of the failure — the HTTP status first, then the transport class — never on
// OpenBao's prose, which differs between versions and between OpenBao and HashiCorp Vault. Anything it
// cannot place falls through to the raw error rather than to an invented diagnosis: a wrong diagnosis sends
// an operator to re-issue a token that was never the problem.
func classifySelfTestFailure(err error) string {
	s := err.Error()
	// The auth-method login is a DIFFERENT fault from the read, and the two are worth separating before the
	// status is even considered: a 400 at auth/approle/login means the AppRole credentials were refused,
	// while a 400 on the LIST would mean something else entirely. The frame matched here is this connector's
	// OWN ("vault: POST auth/<mount>/login: …"), not the server's body.
	if strings.Contains(s, "vault: POST auth/") && strings.Contains(s, "/login") {
		switch code := statusFromVaultError(err); {
		case code == 400 || code == 403:
			return "OpenBao REFUSED THE LOGIN: the configured auth method reached the server and was rejected. " +
				"For approle the role_id/secret_id pair is wrong, revoked, or the SecretID has been used up; " +
				"for wrapped-approle the wrapping token is single-use and a restart needs a freshly delivered " +
				"one; for cert the client certificate is not trusted by (or not mapped to) any cert role. " +
				"Nothing was read."
		case code != 0:
			return fmt.Sprintf("the login to OpenBao failed with status %d — the address is reachable, so this "+
				"is an auth-method or auth-mount problem rather than a network one.", code)
		}
	}
	switch code := statusFromVaultError(err); {
	case code == 401:
		// OpenBao/Vault answers 403 for both "no token" and "denied", so a 401 here is unusual — it is what a
		// reverse proxy or an SSO gateway IN FRONT of OpenBao returns. Saying so is more useful than
		// describing a policy that was never consulted.
		return "the request was rejected with 401 before OpenBao judged it. OpenBao itself answers 403 for a " +
			"bad or missing token, so a 401 usually comes from a proxy or SSO gateway sitting in front of the " +
			"address — check that the address points at the OpenBao API and not at a gateway that expects its " +
			"own credential."
	case code == 403:
		return "OpenBao accepted the request but the credential's POLICY does not permit listing this path. " +
			"Grant the token's policy the \"list\" capability on <mount>/metadata/<prefix> (list only — this " +
			"source is read-only and must never be able to write the KV store). Note that a token that has " +
			"EXPIRED also answers 403; this probe already re-logged-in once, so a persistent 403 is a policy " +
			"fault, not a stale token."
	case code == 400:
		return "OpenBao rejected the request as malformed — the usual cause is a KV v1 mount configured as " +
			"KV v2 (only v2 has the metadata/ segment this path uses). Check the mount's version."
	case code == 429 || code == 503:
		return "every attempt was answered by a node that would not serve it: 429 is what a STANDBY returns " +
			"for a request it does not forward, 503 is sealed-or-not-ready. The retry budget was exhausted, " +
			"so this is a cluster-state problem — check that a leader is elected and that the cluster is " +
			"unsealed."
	case code == 404:
		// Handled as a pass by the caller; reachable only if a 404 arrives from a different request shape.
		return "the path does not exist under that mount — check the KV v2 mount name and the path prefix."
	case code >= 500:
		return fmt.Sprintf("OpenBao answered with a server error (status %d). The address and the credential "+
			"are reaching it, so this is a server-side fault rather than a TG configuration one.", code)
	case code != 0:
		return fmt.Sprintf("OpenBao refused the list with status %d.", code)
	}

	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "resolve token"), strings.Contains(l, "resolve approle"),
		strings.Contains(l, "resolve jwt"), strings.Contains(l, "resolve wrapping token"),
		strings.Contains(l, "is empty (fail closed)"):
		return "the auth credential could not be READ from its reference — the env:/file: reference is wrong, " +
			"or the file is missing. NOTHING was sent to OpenBao. This module's credential cannot live inside " +
			"OpenBao (it is what authenticates it), so it is set outside this dialog: fix the reference the " +
			"deploy sets, not the secret backend."
	case strings.Contains(l, "unwrap"):
		return "the response-wrapped SecretID could not be unwrapped. A wrapping token is SINGLE-USE and " +
			"short-lived: it has already been spent, has expired, or somebody else unwrapped it first — the " +
			"last of which is a tamper signal, not a retry. A fresh wrap must be delivered."
	case strings.Contains(l, "x509"), strings.Contains(l, "certificate"), strings.Contains(l, "tls"):
		return "the TLS certificate could not be verified — TG refuses to hand its credential to a host it " +
			"cannot authenticate. Point the CA certificate path at the private CA that issued the OpenBao " +
			"server certificate; do not work around it by switching the address to http."
	case strings.Contains(l, "timeout"), strings.Contains(l, "deadline"), strings.Contains(l, "no such host"),
		strings.Contains(l, "connection refused"), strings.Contains(l, "connection reset"),
		strings.Contains(l, "eof"):
		return "OpenBao could not be reached — check the address resolves, that the cluster is up, and that " +
			"the worker is allowed to reach it on that port. Every bao: reference in this process fails " +
			"closed while this is true."
	default:
		return s
	}
}

// statusFromVaultError recovers the HTTP status from the error the vault client formats:
//
//	vault: LIST secret/metadata/hosts: status 403: permission denied
//
// It reads the connector's OWN frame — the first ": status " in the string, written before the server's body
// is appended — rather than searching the whole error for a three-digit number. That distinction is what
// keeps an OpenBao error body that happens to mention 403 from being reported as a policy fault when the
// real status was 500. A transport failure has no status and yields 0, which routes classification to the
// transport arm instead.
func statusFromVaultError(err error) int {
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
// operator reading "1 keys visible" wonders whether the probe counted correctly.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
