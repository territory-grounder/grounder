package preflight

import (
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
)

// ★ THE SHAPE THE SECRET POLICY COULD NOT SEE (TG-284).
//
// CheckSecretPolicy classifies REFERENCES. Every reference it classifies is one a caller ENUMERATED. So a
// credential that is neither a reference nor named in that table was not merely unpoliced — it was
// INVISIBLE, and the gate's green result actively asserted its absence. On the live worker, 2026-08-04,
// with TG_SECRET_POLICY=enforce and a clean boot:
//
//	LIBRENMS_GR_TOKEN = 64 chars of raw API token
//	LIBRENMS_TOKEN    = 32 chars of raw API token
//
// Both sat in the same process env the gate had just declared compliant. This is the project's signature
// failure mode in its purest form: a control reporting success over the exact condition it exists to detect.
//
// The fix is a second, SHAPE-based eye that needs no enumeration at all: walk the REAL process environment
// and treat a variable whose NAME declares a credential (TOKEN/KEY/PASS/PASSWORD/SECRET) but whose VALUE is
// not a SecretRef as a raw plaintext credential. Nothing has to be declared for this to fire, which is the
// whole point — the previous gate could only see what someone remembered to list.
//
// NO LENGTH OR ENTROPY HEURISTIC. "Looks random enough to be a secret" is unfalsifiable, drifts silently
// with every new credential format, and would have to LOOK AT the value to decide. The rule here is
// structural and stated in two halves — the name declares a credential, the value is not a location.
//
// NEVER TOUCHES A VALUE. Values are read only through config.HasReferenceScheme (a bool; see its comment for
// why SchemeOf would hand back a fragment of a colon-bearing secret) and a trimmed-empty test. No violation,
// log line or error carries anything but the variable NAME.
//
// Provenance: [F] owner directive (no plaintext at rest) · [O] INV-13, INV-21 (a control that cannot work
// must fail LOUD), spec/024 REQ-2400/REQ-2402.

// secretNameTokens are the substrings that make an env var NAME a declaration that it carries a credential.
// Matching is case-insensitive substring, deliberately broad: a false positive costs one accounted-for entry
// with a written reason, a false negative costs a plaintext credential nobody can see. PASSWORD is listed
// even though PASS already matches it — this set is the policy's documentation as much as its code.
var secretNameTokens = []string{"TOKEN", "KEY", "PASS", "PASSWORD", "SECRET"}

// IsSecretShapedName reports whether an environment variable NAME declares that it carries a credential.
func IsSecretShapedName(name string) bool {
	u := strings.ToUpper(strings.TrimSpace(name))
	for _, tok := range secretNameTokens {
		if strings.Contains(u, tok) {
			return true
		}
	}
	return false
}

// NotASecretEnvVars is the CLOSED allowlist of secret-SHAPED names that hold NO CREDENTIAL AT ALL, each
// mapped to the reason it cannot. It is deliberately tiny, and it is NOT the mechanism that keeps the live
// deployment bootable — that is the structural accounting in CheckEnvShape (a raw value reached by a
// declared reference is reported through that reference, and one reached by a PERMANENTLY EXEMPT reference
// inherits the exemption). Membership here answers one question only: "what does this variable hold?" If
// the answer is a name, a path, a mode, or public material, it belongs. If the answer is "a credential", it
// does not — the fix for a credential is a backend reference, never an allowlist row.
//
// Every entry is one this deployment really sets (deploy/docker-compose.yml, worker + grounder services); an
// allowlist of hypothetical names would be unfalsifiable cover. Two tests hold that:
// TestAllowlistEntriesAreSecretShapedAndReasoned fails the build on an entry the shape matcher no longer
// matches (dead cover that looks like a considered decision), and
// TestTheLiveDeploymentsCredentialNamesAreSeenByTheShapeRule fails on an entry no service actually sets.
var NotASecretEnvVars = map[string]string{
	// A Transit KEY NAME (live value: "tg-seal") — the identifier of the key INSIDE OpenBao, used to select
	// which key wraps the DEK. The key material never leaves OpenBao (core/seal/fromenv.go reads this as a
	// name and hands it to the Transit API). Naming a key is not holding it.
	"TG_SEAL_TRANSIT_KEY": "an OpenBao Transit key NAME, not key material — the key never leaves OpenBao",
	// A filesystem PATH to the mTLS client key (live value: "/secrets/tg.key"). The private key is the FILE,
	// delivered out-of-band under the /secrets mount; the path is public (cmd/worker, cmd/grounder and
	// cmd/tg-secretenv all pass it straight to the TLS loader).
	"TG_OPENBAO_KEY": "a filesystem PATH to the mTLS client key, not the key material",
	// This gate's own MODE: off | warn | enforce. It matches "SECRET" by name only. If policy=enforce ever
	// counted itself as a plaintext secret, enabling the control would break the boot it protects.
	"TG_SECRET_POLICY": "the secret-policy MODE (off/warn/enforce), not a credential",
	// The OIDC token ENDPOINT (an https:// URL) — the address the client-credentials grant is POSTed to. The
	// credential is TG_OIDC_CLIENT_SECRET_REF, which is enumerated and policed as a business secret.
	"TG_OIDC_TOKEN_URL": "an OIDC token endpoint URL, not a credential",
	// The PUBLIC half of the Langfuse key pair — published to browsers by design. Already classified as
	// public material on the reference side (TG_LANGFUSE_PUBLIC_REF is in PermanentExemptions); this is the
	// same judgement applied to the raw value that reference points at.
	"LANGFUSE_PUBLIC_KEY": "the PUBLIC half of the Langfuse key pair — public material, not a secret",
}

// EnvShapeReport is the outcome of one process-env shape scan.
//
// Scanned and Shaped exist so the scan's REACH is observable at boot rather than inferred. A filter that has
// silently stopped matching is this repo's most-repeated bug, and this one filters the whole process
// environment: "0 violations" out of 0 scanned means the control is DEAD, while "0 violations" out of 300
// scanned with 61 secret-shaped names means it looked and found nothing. Those must never read alike in a
// log, so every disposition is counted and the excused ones are named.
type EnvShapeReport struct {
	Scanned int // process env vars examined (non-empty value)
	Shaped  int // of those, names that declare a credential
	// Accounted are shaped names the REFERENCE pass already speaks for: the declared entries themselves, and
	// the raw variables a declared reference points at with env:. Excusing them here is not leniency — it
	// stops one credential being counted twice under two names, and CheckSecretPolicy has already ruled on it.
	Accounted []string
	// InheritedExempt are raw variables reached by a PERMANENTLY EXEMPT reference (TG_SEAL_KEY_REF=env:
	// TG_SEAL_KEY and friends). The exemption is a statement about the CREDENTIAL — it cannot come from the
	// backend it unwraps or authenticates — so it must follow the credential to wherever the value actually
	// sits. Without this the seal key and the OpenBao bootstrap token would make `enforce` unsatisfiable by
	// construction, and an unsatisfiable control gets switched off, which is how this gate died the first time.
	InheritedExempt []string
	Allowlisted     []string // shaped names excused by NotASecretEnvVars (surfaced, never silent)
	// InlineURLCredential are variables whose NAME declares nothing but whose VALUE carries a password
	// inside a URL's userinfo (`scheme://user:password@host`). They are the shape rule's own blind spot:
	// TG_DB_DSN matches none of TOKEN/KEY/PASS/PASSWORD/SECRET, so a live database password in the process
	// environment was never in the scanned population and the gate reported zero violations over it.
	//
	// REPORTED, NOT YET A VIOLATION, AND THAT IS A DELIBERATE SEQUENCING CHOICE. TG_SECRET_POLICY=enforce is
	// LIVE on this deployment. Promoting these to violations today would refuse the boot on the next restart
	// — a stricter fail-closed gate landing before the configuration it judges has been migrated is how the
	// actuation plane went down twice on 2026-08-06. The DSN moves behind a reference first; the promotion
	// is its own reviewed step.
	InlineURLCredential []string
	Violations          []SecretViolation
}

// CheckEnvShape walks a process environment (os.Environ() form, "NAME=VALUE") and reports every variable
// whose NAME declares a credential while its VALUE is a raw literal rather than a SecretRef.
//
// entries is the SAME declared set CheckSecretPolicy classifies. It is passed rather than a bare name set
// because the dispositions below need the reference's scheme, its target, and whether it holds a permanent
// exemption — see Accounted and InheritedExempt.
//
// Dispositions, in order: an empty value is skipped (an unset optional secret is not a plaintext violation —
// the same rule CheckSecretPolicy applies to an empty ref); a non-shaped name is skipped; then a shaped name
// is Accounted, InheritedExempt, Allowlisted, a reference, or a VIOLATION. Every excused name is recorded,
// none is dropped silently. Deterministic: violations and name lists sorted.
func CheckEnvShape(environ []string, entries []SecretEntry) EnvShapeReport {
	declared := make(map[string]bool, len(entries))
	// envTargets maps a raw variable NAME to the declared entry that reaches it with env:. Two references
	// pointing at one variable is a misconfiguration, not a security question; last writer wins and the
	// reference pass reports both.
	envTargets := make(map[string]SecretEntry, len(entries))
	for _, e := range entries {
		declared[e.Name] = true
		if config.SchemeOf(e.Ref) != "env" {
			continue
		}
		if target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(e.Ref)), "env:")); target != "" {
			envTargets[target] = e
		}
	}

	var rep EnvShapeReport
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			continue // unset/blank — nothing is stored here, so nothing is exposed
		}
		rep.Scanned++
		if !IsSecretShapedName(name) {
			// THE SHAPE RULE'S BLIND SPOT, looked at rather than assumed away. A name that declares nothing
			// can still hold a credential structurally — a connection string. Checked here, before the
			// name gate returns, because that gate is exactly what misses it.
			//
			// Only names the shape rule does NOT already match are reported this way, so one credential is
			// never counted twice under two dispositions.
			if config.HasInlineURLCredential(value) {
				rep.InlineURLCredential = append(rep.InlineURLCredential, name)
			}
			continue
		}
		rep.Shaped++
		if declared[name] {
			rep.Accounted = append(rep.Accounted, name) // classified by scheme in CheckSecretPolicy
			continue
		}
		if e, reached := envTargets[name]; reached {
			// The credential behind a declared reference. If that reference holds a REAL permanent exemption
			// the credential does too; otherwise CheckSecretPolicy has already reported it as a plaintext
			// env: violation under the reference's name and repeating it here only inflates the count the
			// operator has to work through. An UNCLAIMED exemption (Exempt set on a name not in the closed
			// set) inherits nothing — it is already a violation and the raw value stays visible.
			if e.Exempt && PermanentExemptions[e.Name] {
				rep.InheritedExempt = append(rep.InheritedExempt, name+" (via "+e.Name+")")
			} else {
				rep.Accounted = append(rep.Accounted, name+" (via "+e.Name+")")
			}
			continue
		}
		if _, excused := NotASecretEnvVars[name]; excused {
			rep.Allowlisted = append(rep.Allowlisted, name)
			continue
		}
		if config.HasReferenceScheme(value) {
			continue // a LOCATION (env:/file:/store:/bao:/…), not a credential
		}
		// A name that declares a credential, holding something that is not a reference: a raw secret sitting
		// in the process env — exactly the shape the enumerated gate could not see.
		rep.Violations = append(rep.Violations, SecretViolation{Name: name, Scheme: rawValueScheme, RawPlaintext: true})
	}
	sort.Slice(rep.Violations, func(i, j int) bool { return rep.Violations[i].Name < rep.Violations[j].Name })
	sort.Strings(rep.Accounted)
	sort.Strings(rep.InheritedExempt)
	sort.Strings(rep.Allowlisted)
	sort.Strings(rep.InlineURLCredential)
	return rep
}

// rawValueScheme is the pseudo-scheme recorded for a shape violation. A raw value HAS no scheme — that is
// the finding — and the real text before its first colon must never be reported (it would be a fragment of
// the credential). This constant is what the report and every log line carry instead.
const rawValueScheme = "raw-value"
