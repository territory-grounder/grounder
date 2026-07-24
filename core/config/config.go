// Package config resolves configuration where every secret is a *reference*, never a literal.
//
// Provenance: [O] INV-13 (secrets are references; no literal secret in any artifact), P0-4 ·
// [R] paradigm-rule 3 (identity/secret isolation). Because TG orchestration is compiled Go and not
// exportable JSON, there is no blob to embed a secret into; the only supported way to supply a
// secret is a reference resolved in memory at runtime.
package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
)

// SecretRef is an indirect reference to a secret, e.g. "env:ZAI_API_KEY", "file:/run/secrets/x", or
// "store:librenms.token" (a sealed secret in the control plane's encrypted store, task #27 Phase D).
// A SecretRef is safe to log, commit, and export; the secret value it points to is not.
type SecretRef string

// storeResolver resolves "store:<name>" references against the sealed-secret store (REQ-524). It is
// wired ONCE at composition (cmd/grounder) when the seal master key resolves; nil = the scheme fails
// closed — a store: reference in a deployment with no sealed store is an error, never an empty value.
var storeResolver func(name string) (string, error)

// RegisterStoreResolver wires the sealed-secret store into the store: scheme. Composition-time only.
func RegisterStoreResolver(f func(name string) (string, error)) { storeResolver = f }

// schemeResolvers holds resolvers for PLUGGABLE reference schemes beyond the built-in env:/file:/store:
// — e.g. the vault: / bao: OpenBao/HashiCorp Vault backend (REQ-1613). A connector registers its scheme at
// composition; nil/unregistered = the scheme fails closed (an error, never an empty value). Unlike the
// single sealed-store slot (RegisterStoreResolver), this is a keyed registry so several backend schemes can
// coexist without one clobbering another. The resolver receives the FULL reference (scheme included) so a
// connector serving several schemes (vault: and bao:) can share one implementation.
var (
	schemeResolversMu sync.RWMutex
	schemeResolvers   = map[string]func(ref string) (string, error){}
)

// RegisterSchemeResolver wires a resolver for a custom SecretRef scheme (e.g. "vault", "bao"). Passing nil
// unregisters the scheme (it then fails closed). Composition-time; safe for concurrent Resolve.
func RegisterSchemeResolver(scheme string, f func(ref string) (string, error)) {
	schemeResolversMu.Lock()
	defer schemeResolversMu.Unlock()
	if f == nil {
		delete(schemeResolvers, scheme)
		return
	}
	schemeResolvers[scheme] = f
}

// schemeResolver returns the registered resolver for a scheme, if any.
func schemeResolver(scheme string) func(ref string) (string, error) {
	schemeResolversMu.RLock()
	defer schemeResolversMu.RUnlock()
	return schemeResolvers[scheme]
}

// Resolve loads the referenced secret into memory. Supported schemes: env:, file:, store:, plus any
// registered custom scheme (e.g. vault:/bao:, REQ-1613).
func (r SecretRef) Resolve() (string, error) {
	s := string(r)
	switch {
	case strings.HasPrefix(s, "env:"):
		name := strings.TrimPrefix(s, "env:")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("config: env secret %q is not set", name)
		}
		return v, nil
	case strings.HasPrefix(s, "file:"):
		b, err := os.ReadFile(strings.TrimPrefix(s, "file:"))
		if err != nil {
			return "", fmt.Errorf("config: file secret: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case strings.HasPrefix(s, "store:"):
		name := strings.TrimPrefix(s, "store:")
		if name == "" {
			return "", fmt.Errorf("config: empty store secret name")
		}
		if storeResolver == nil {
			return "", fmt.Errorf("config: store secret %q: sealed-secret store not wired (fail closed)", name)
		}
		return storeResolver(name)
	case s == "":
		return "", fmt.Errorf("config: empty secret reference")
	default:
		// A pluggable scheme (vault:/bao:/…) registered by a connector at composition (REQ-1613). An
		// unregistered scheme fails closed — never an empty or default value.
		if scheme, _, ok := strings.Cut(s, ":"); ok && scheme != "" {
			if f := schemeResolver(scheme); f != nil {
				return f(s)
			}
		}
		return "", fmt.Errorf("config: unsupported secret reference scheme in %q (use env:, file:, store:, or a registered scheme)", s)
	}
}

// backendSchemes are the secret-reference schemes that resolve through a real secret BACKEND (a vault /
// sealed store), as opposed to a plaintext-bearing scheme (env: / an inline literal) or an on-disk file:.
// The secret-policy boot gate (spec/024 REQ-2400) treats these as compliant. Keep in sync with the
// registered connector schemes.
var backendSchemes = map[string]bool{"bao": true, "vault": true, "store": true, "vw": true, "passbolt": true}

// SchemeOf returns the scheme of a secret reference ("env", "file", "bao", …), "empty" for the empty ref,
// or "literal" for a bare value that carries no scheme prefix (a plaintext secret written inline).
func SchemeOf(r SecretRef) string {
	s := strings.TrimSpace(string(r))
	if s == "" {
		return "empty"
	}
	if scheme, _, ok := strings.Cut(s, ":"); ok && scheme != "" {
		return scheme
	}
	return "literal"
}

// IsBackendScheme reports whether a reference resolves through a real secret backend (vault/sealed store)
// rather than a plaintext-bearing scheme. The secret-policy gate uses this to decide compliance.
func IsBackendScheme(r SecretRef) bool { return backendSchemes[SchemeOf(r)] }

// Finding is one suspected literal-secret hit from the config linter.
type Finding struct {
	Line   int
	Token  string // masked
	Reason string
}

// high-entropy token candidate: a base64/hex-ish run long enough to be a real credential.
var tokenRe = regexp.MustCompile(`[A-Za-z0-9+/=_\-]{20,}`)

// allowlisted non-secret shapes: reference schemes, obvious placeholders, and known-safe words.
var allowRe = regexp.MustCompile(`(?i)^(env:|file:|store:)|change[_-]?me|your[_-]|example|placeholder|redacted|xxxx|<[a-z_]+>|sha256|[0-9a-f]{7,40}$`)

// shannonEntropy returns bits of entropy per character for s.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// LintForbiddenSecrets scans src for tokens that look like literal secrets (high-entropy long runs
// that are not reference schemes or placeholders). It is a defence-in-depth heuristic that runs at
// config load AND in CI (alongside gitleaks). [O] INV-13, P0-4, P0-8.
func LintForbiddenSecrets(src []byte) []Finding {
	var out []Finding
	for i, line := range strings.Split(string(src), "\n") {
		for _, tok := range tokenRe.FindAllString(line, -1) {
			if allowRe.MatchString(tok) {
				continue
			}
			// require both length and high entropy to reduce false positives on slugs/paths.
			if len(tok) >= 20 && shannonEntropy(tok) >= 3.5 {
				out = append(out, Finding{
					Line:   i + 1,
					Token:  mask(tok),
					Reason: "high-entropy literal — supply as a secret reference (env:/file:), never inline",
				})
			}
		}
	}
	return out
}

func mask(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-2:]
}
