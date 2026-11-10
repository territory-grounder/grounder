// Package egress meters and (optionally) constrains the platform's OUTBOUND network traffic.
//
// WHY THIS PACKAGE EXISTS (TG-160). Measured 2026-08-04, before this package existed:
//
//	$ grep -rn -w -i egress --include=*.go .        # → ZERO hits, whole tree
//
// while docs/THREAT-MODEL.md advertised the interceptor chain as "admission → territory/egress/policy
// check → execute". There was no egress step, no allowlist, no destination meter and no zone concept
// anywhere in Go. On the deployed stack the compose file carried no `networks:` stanza at all, so every
// container sat on the default bridge with full outbound NAT; `iptables -S DOCKER-USER` was the stock
// `-A DOCKER-USER -j RETURN`; and the helm chart shipped no NetworkPolicy. All five model tiers egress
// off-host. An advertised control that does not exist is worse than an absent one, because it is budgeted
// for and then not built.
//
// THE THREAT. The model-call path carries unconstrained prompt/response content to a third-party gateway
// (modules/model/litellm/gateway.go), and the worker legitimately beacons to a dozen more destinations —
// estate APIs, trackers, notifiers, observability exporters. That is precisely the cover the July-2026
// HuggingFace intrusion used: a self-migrating C2 riding public SaaS, indistinguishable from normal
// traffic because nothing was counting normal traffic. SecretRef redaction keeps CREDENTIALS out of
// payloads; it does nothing about estate recon or attacker-authored content leaving in a prompt.
//
// WHAT THIS IS AND IS NOT.
//
//   - It is a DESTINATION AND VOLUME METER on the process's own HTTP egress. It answers "who did this
//     process talk to, how often, and how many bytes moved" — three facts the platform could not produce.
//   - By DEFAULT it BLOCKS NOTHING (ModeMeter). That is deliberate and it is the ticket's instruction: a
//     wrong allowlist takes production off the network, so meter first, then block. ModeEnforce exists,
//     is exercised by tests, and is opt-in per deployment.
//   - It is NOT the control. The control is the NETWORK layer (the compose network split and the helm
//     NetworkPolicy that land with this change), because that is the only layer that still holds when
//     the process is compromised — which is the entire threat. This package is defence in depth ON TOP.
//     A process that can execute code can bypass its own RoundTripper; it cannot bypass the bridge.
//
// Provenance: [O] TG-160, parent TG-155; TG-153 High#4; NIST SP 800-207 (PEP); MITRE ATLAS AML.T0025
// (exfiltration via inference API). Sibling of core/safety/recon_budget.go (TG-165), which bounds read
// VOLUME inside the estate; this bounds where bytes GO.
package egress

import (
	"net"
	"net/url"
	"sort"
	"strings"
)

// Allowlist is the set of destination hosts this process has DECLARED it talks to. It is a matcher, not
// a policy: it answers "is this host one we said we use", and the caller decides what that means.
//
// Matching is on the HOSTNAME only, never the port or path. Two reasons: the question being answered is
// "did bytes leave for somewhere we never named", which a port cannot change; and a port-sensitive
// allowlist silently mis-flags every legitimate redirect to :443 as off-allowlist, which is how a meter
// earns its way onto an ignore list in week one.
type Allowlist struct {
	exact  map[string]string // normalised host → the rule that admitted it (for the metric label)
	suffix []suffixRule      // "*.example.org" → any host ending ".example.org" (and example.org itself)
	rules  []string          // the declared entries, normalised + deduped + sorted (for exposition)
}

type suffixRule struct {
	suffix string // ".example.org"
	bare   string // "example.org"
	rule   string // the original entry, for the label
}

// NewAllowlist compiles declared entries into a matcher. Entries may be bare hosts ("librenms.example"),
// host:port ("temporal:7233" — the port is dropped), full URLs ("https://netbox.example/api"), or a
// leading-wildcard suffix ("*.example.org"). Anything that yields no host is discarded rather than
// silently becoming a match-everything rule.
//
// AN EMPTY ALLOWLIST PERMITS NOTHING. It does not permit everything. That direction is load-bearing: a
// deployment that declared no destinations should read as "every outbound is unaccounted for", which is
// visible, rather than "everything is fine", which is a control that reports success for doing nothing —
// this repository's signature defect.
func NewAllowlist(entries []string) *Allowlist {
	a := &Allowlist{exact: map[string]string{}}
	seen := map[string]bool{}
	for _, raw := range entries {
		for _, tok := range splitTokens(raw) {
			host, ok := hostOf(tok)
			if !ok {
				continue
			}
			if strings.Contains(host, "*") && !strings.HasPrefix(host, "*.") {
				continue // a bare "*" or an interior wildcard is not a declaration, it is an abdication
			}
			if strings.HasPrefix(host, "*.") {
				bare := strings.TrimPrefix(host, "*.")
				if bare == "" || !strings.Contains(bare, ".") {
					continue // "*" or "*.local" — too broad to be a declaration
				}
				if !seen[host] {
					seen[host] = true
					a.suffix = append(a.suffix, suffixRule{suffix: "." + bare, bare: bare, rule: host})
					a.rules = append(a.rules, host)
				}
				continue
			}
			if !seen[host] {
				seen[host] = true
				a.exact[host] = host
				a.rules = append(a.rules, host)
			}
		}
	}
	sort.Strings(a.rules)
	return a
}

// Permits reports whether host is declared, and returns the RULE that admitted it. The rule — not the
// host — is what may become a metric label, so cardinality is bounded by the size of the declaration
// (finite, config-supplied) and never by the traffic.
//
// Loopback is always permitted under the rule "loopback": 127.0.0.0/8, ::1 and "localhost" never leave
// the network namespace, so counting them as egress would bury the real signal under the process's own
// health checks.
func (a *Allowlist) Permits(host string) (rule string, ok bool) {
	h := normaliseHost(host)
	if h == "" {
		return "", false
	}
	if isLoopback(h) {
		return "loopback", true
	}
	if r, hit := a.exact[h]; hit {
		return r, true
	}
	for _, s := range a.suffix {
		if h == s.bare || strings.HasSuffix(h, s.suffix) {
			return s.rule, true
		}
	}
	return "", false
}

// Size is the number of declared rules. Published as tg_egress_allowlist_rules so a flat ZERO on the
// live surface reads as "this meter is comparing traffic against nothing" — the vacuity floor, on the
// metric rather than only in a test.
func (a *Allowlist) Size() int { return len(a.rules) }

// Rules returns the declared entries, sorted. Hosts only; no credential ever reaches here (the scanner
// below takes endpoint keys, never *_TOKEN/*_KEY/*_REF values).
func (a *Allowlist) Rules() []string { return append([]string(nil), a.rules...) }

// endpointKeySuffixes are the environment-key suffixes whose VALUES name a network destination. The
// allowlist is derived from the deployment's OWN configuration (config-not-code): every outbound
// destination this stack reaches is already declared as an env endpoint in deploy/docker-compose.yml —
// TG_LITELLM_URL, TG_OPENBAO_ADDR, TG_NETBOX_URL, TG_PVE_URL, TG_YOUTRACK_URL, TG_MATRIX_HOMESERVER,
// TG_LDAP_URLS, TG_AWX_ADDR, TG_LIBRENMS_DEPLOYMENTS, … — so a connector that is configured is a
// connector that is declared, and the allowlist maintains itself. A hand-written list would be stale the
// first time somebody armed a new module, and a stale allowlist that nobody trusts is not a control.
var endpointKeySuffixes = []string{
	"_ADDR", "_ADDRS", "_BASE", "_BASE_URL", "_DEPLOYMENTS", "_ENDPOINT", "_ENDPOINTS",
	"_HOMESERVER", "_HOST", "_HOSTPORT", "_HOSTS", "_TOPOLOGY", "_URL", "_URLS",
}

// NOTE on _TOPOLOGY (added 2026-08-08, TG-381 review): the actuation plane resolves its LibreNMS endpoint
// from TG_LIBRENMS_DEPLOYMENTS_TOPOLOGY (a deployments-spec value, topology-scoped credentials per TG-337).
// It went un-scanned while it only under-counted the HTTP meter — harmless there. But TG-381's egress-LAN
// allowlist is DERIVED from this same scan and default-drops everything else RFC1918, so a topology-only
// deployment (base key unset) would have had its LibreNMS host severed. It is a destination-bearing key and
// belongs in the scan. It carries the deployments spec shape, so it is also a bare-host suffix below.

// bareHostKeySuffixes are the subset whose values may be BARE hostnames with no scheme and no port
// (SSH target lists, mostly). For every other suffix a token must carry a scheme or a port before it is
// believed to be a host — otherwise a value like "corpus.maintained.json" becomes an allowlist entry and
// the meter starts excusing traffic on the strength of a filename.
var bareHostKeySuffixes = []string{"_ADDR", "_ADDRS", "_DEPLOYMENTS", "_HOST", "_HOSTPORT", "_HOSTS", "_TOPOLOGY"}

// DeclaredDestinations scans an environment (os.Environ() form: "KEY=value") and returns the destination
// hosts the deployment has declared. Only keys ending in an endpointKeySuffix are read, so no secret
// value is ever inspected: *_TOKEN, *_KEY, *_REF, *_PASSWORD do not end in any of them.
func DeclaredDestinations(env []string) []string {
	var out []string
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		key, val := strings.ToUpper(kv[:i]), strings.TrimSpace(kv[i+1:])
		if val == "" || !hasAnySuffix(key, endpointKeySuffixes) {
			continue
		}
		bareOK := hasAnySuffix(key, bareHostKeySuffixes)
		for _, tok := range splitTokens(val) {
			host, ok := hostOfToken(tok, bareOK)
			if ok {
				out = append(out, host)
			}
		}
	}
	return out
}

func hasAnySuffix(key string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(key, s) {
			return true
		}
	}
	return false
}

// splitTokens breaks a configuration value into candidate destination tokens. Values in this stack are
// CSV, semicolon-separated, pipe-separated or whitespace-separated depending on which module owns them,
// and a deployment spec ("nl=https://ln.example,gr=https://ln2.example") mixes forms — so split on all
// of them and let hostOf reject what is not a destination.
func splitTokens(v string) []string {
	f := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(f))
	for _, t := range f {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		// "site=https://host/…" — the deployment-spec form; keep the right-hand side.
		if eq := strings.IndexByte(t, '='); eq >= 0 && strings.Contains(t[eq+1:], "://") {
			t = t[eq+1:]
		}
		out = append(out, t)
	}
	return out
}

// hostOf extracts a hostname from an allowlist ENTRY. Entries are hand-declared, so a bare host is
// believed here (that is what an operator writing TG_EGRESS_ALLOW means).
func hostOf(tok string) (string, bool) { return hostOfToken(tok, true) }

// hostOfToken extracts a hostname from tok. allowBare admits a scheme-less, port-less token as a host.
func hostOfToken(tok string, allowBare bool) (string, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", false
	}
	if strings.Contains(tok, "://") {
		u, err := url.Parse(tok)
		if err != nil {
			return "", false
		}
		return normaliseHost(u.Hostname()), normaliseHost(u.Hostname()) != ""
	}
	// host:port — "temporal:7233", "librenms.example:8080", "[::1]:443".
	if h, p, err := net.SplitHostPort(tok); err == nil && p != "" {
		return normaliseHost(h), normaliseHost(h) != ""
	}
	if !allowBare {
		return "", false
	}
	if strings.ContainsAny(tok, "/?#@ ") {
		return "", false
	}
	h := normaliseHost(tok)
	if h == "" {
		return "", false
	}
	// A bare token must look like a host: a wildcard, an IP, or something with a dot or a docker-compose
	// service name. Reject anything carrying a filename-ish extension so a path value cannot smuggle in.
	if strings.HasPrefix(h, "*.") || net.ParseIP(h) != nil {
		return h, true
	}
	if i := strings.LastIndexByte(h, '.'); i >= 0 {
		switch h[i+1:] {
		case "json", "yaml", "yml", "crt", "pem", "key", "txt", "sql", "log", "conf", "toml", "ini":
			return "", false
		}
	}
	return h, true
}

// normaliseHost lowercases, strips a trailing root dot and unwraps a bracketed IPv6 literal, so
// "API.Example.Org.", "api.example.org" and "[2001:db8::1]" compare as one thing.
func normaliseHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if strings.ContainsAny(h, "/\\ ") {
		return ""
	}
	return h
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
