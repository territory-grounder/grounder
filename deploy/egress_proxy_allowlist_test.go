package deploy

// TG-420 slice 1 — THE EGRESS-PROXY FENCE GUARD. Per OWNER RULING TG-488 B11, CORRECTED 2026-08-14 on
// TG-420 (pending owner veto, TG-488 A1 decide-do-record).
//
// THE RULING + THE CORRECTION. B11 ruled a three-endpoint fence (sidecar, ollama, api.z.ai) — but that
// set was framed on an api_base-only grep that missed providers on DEFAULT endpoints: DeepSeek is the
// JUDGE (deliberately independent-vendor; ladder rung 2) and Mistral is the CURRENT primary/fast brain
// (since the 08-08 opus-cc sub-cap; rung 3). An enforce flip on the uncorrected set would have cut the
// live brain and the judge — the exact class of outage "log first" exists to prevent, caught at the
// stranger report before any block. The corrected fence is FIVE:
//   1. sidecar            — the on-box Anthropic lane (HTTP, stays direct; permitted by being off the proxy)
//   2. ${TG_OLLAMA_HOST}  — the LAN embed lane (HTTP, stays direct; the real host is an estate coordinate,
//                           so it is an env-ref here — mirror abort-on-survivor forbids committing it)
//   3. api.z.ai           — public HTTPS, ruled in B11 (owner funds the account)
//   4. api.deepseek.com   — the judge + rung 2 (the correction)
//   5. api.mistral.ai     — the current primary/fast brain + rung 3 (the correction)
// "Log first, then block strangers" still governs: observe mode LOGS every CONNECT; slice-2 enforcement
// blocks whatever is then outside the fence. A fresh owner ruling supersedes the correction.
//
// SO THIS GUARD HAS TWO JOBS (the derivation's role, INVERTED from a plain parity check):
//   A. ASSERT deploy/egress-proxy/provider-allowlist.txt == the ruled set. The fence tracks the owner's
//      ruling, never litellm-config.yaml — a config change must NOT silently widen the fence.
//   B. REPORT (never fail on) which litellm-config provider domains fall OUTSIDE the fence. That delta is
//      the stranger list the observe drill measures; it is derived from litellm-config.yaml so it stays
//      honest as the ladder changes (TG-293 swaps primary between Mistral and the sidecar).
//
// It is pure-stdlib + yaml.v3 over file reads, so it is CI-runnable and deterministic.

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ruledFence is the owner-ruled fence (TG-488 B11 + the 2026-08-14 TG-420 correction), verbatim. The
// committed allowlist must equal this set. Change it ONLY on a new owner ruling (or an owner veto of the
// correction), and cite it here and in the allowlist file's header.
var ruledFence = []string{"sidecar", "${TG_OLLAMA_HOST}", "api.z.ai", "api.deepseek.com", "api.mistral.ai"}

// providerAPIDomains maps a litellm provider prefix to its DEFAULT public API domain (litellm's documented
// provider API bases). Used to DERIVE the stranger list from litellm-config.yaml — NOT to build the fence.
// A prefix absent here, used without an api_base, fails the derivation closed (job B stays honest).
var providerAPIDomains = map[string]string{
	"mistral":   "api.mistral.ai",
	"deepseek":  "api.deepseek.com",
	"anthropic": "api.anthropic.com",
	"openai":    "api.openai.com",
	"xai":       "api.x.ai",
	"moonshot":  "api.moonshot.ai", // Kimi K-series (moonshot/ provider)
	"zai":       "api.z.ai",
}

type litellmModelEntry struct {
	ModelName string `yaml:"model_name"`
	Params    struct {
		Model   string `yaml:"model"`
		APIBase string `yaml:"api_base"`
	} `yaml:"litellm_params"`
}

type litellmConfigDoc struct {
	ModelList []litellmModelEntry `yaml:"model_list"`
}

// deriveProviderDomains returns the set of PUBLIC provider API domains the litellm model ladder can reach,
// each mapped to the model_name that introduced it. This is the raw material for the STRANGER REPORT (job
// B): a domain here that is not in the fence is a stranger.
func deriveProviderDomains(cfg []byte) (map[string]string, error) {
	var doc litellmConfigDoc
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		return nil, fmt.Errorf("parse litellm config: %w", err)
	}
	domains := map[string]string{}
	for _, e := range doc.ModelList {
		model := strings.TrimSpace(e.Params.Model)
		if model == "" {
			continue
		}
		if apiBase := strings.TrimSpace(e.Params.APIBase); apiBase != "" {
			host, public := publicHostOfAPIBase(apiBase)
			if !public {
				continue // deploy-set / internal / private endpoint — not a public provider hop
			}
			domains[host] = e.ModelName
			continue
		}
		provider, _, _ := strings.Cut(model, "/")
		provider = strings.ToLower(strings.TrimSpace(provider))
		domain, known := providerAPIDomains[provider]
		if !known {
			return nil, fmt.Errorf("model_name %q uses provider %q with no api_base and no known API domain — "+
				"map it in providerAPIDomains so the stranger report stays honest (fail-closed: an unmapped "+
				"provider must not vanish silently from the strangers-outside-the-fence delta)", e.ModelName, provider)
		}
		domains[domain] = e.ModelName
	}
	return domains, nil
}

// publicHostOfAPIBase classifies a litellm_params.api_base and returns (host, true) ONLY for a public
// endpoint. os.environ/… (the deploy-set sidecar + ollama lanes), a private/loopback IP literal, or a bare
// single-label compose service name are internal and return ("", false) — they are inside the fence's direct
// lanes, never strangers.
func publicHostOfAPIBase(apiBase string) (string, bool) {
	if strings.HasPrefix(apiBase, "os.environ/") {
		return "", false // resolved from the box .env (TG_SIDECAR_BASE / TG_OLLAMA_BASE), never a literal here
	}
	u, err := url.Parse(apiBase)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsPrivate() || ip.IsLoopback() {
			return "", false
		}
		return host, true // a public IP literal — unusual for a provider, but honour it
	}
	if !strings.Contains(host, ".") {
		return "", false // a bare compose service name (e.g. "sidecar")
	}
	return host, true
}

func readFenceLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("egress-proxy", "provider-allowlist.txt"))
	if err != nil {
		t.Fatalf("read deploy/egress-proxy/provider-allowlist.txt: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// TestProxyFenceMatchesOwnerRuling is JOB A: the committed fence == the owner-ruled set {sidecar,
// ${TG_OLLAMA_HOST}, api.z.ai}, exactly, in both directions. The fence tracks the RULING, never
// litellm-config.yaml — adding a provider to the ladder must NOT widen the fence (that is what makes
// deepseek/mistral strangers), and no entry may appear that the ruling did not name.
func TestProxyFenceMatchesOwnerRuling(t *testing.T) {
	want := map[string]bool{}
	for _, e := range ruledFence {
		want[e] = true
	}
	got := map[string]bool{}
	for _, l := range readFenceLines(t) {
		got[l] = true
	}

	if len(got) == 0 {
		t.Fatal("the fence file parsed to ZERO entries — a fence that permits nothing is not the ruled fence " +
			"(and would blackhole the brain under enforcement); the parse or the file is broken")
	}
	for e := range want {
		if !got[e] {
			t.Errorf("fence entry %q (owner ruling TG-488 B11) is MISSING from deploy/egress-proxy/provider-allowlist.txt", e)
		}
	}
	for e := range got {
		if !want[e] {
			t.Errorf("deploy/egress-proxy/provider-allowlist.txt contains %q, which the owner ruling (TG-488 B11) "+
				"does NOT name. The fence is EXACTLY {sidecar, ${TG_OLLAMA_HOST}, api.z.ai}; deepseek/mistral and "+
				"any other provider are deliberately STRANGERS, not fence entries. Change the fence only on a new "+
				"ruling.", e)
		}
	}
}

// TestProxyFenceStrangerReport is JOB B: it does not gate the fence, it MEASURES the delta. It derives the
// public provider domains litellm-config.yaml can reach and reports which fall OUTSIDE the fence — the
// stranger list the observe drill expects to see logged, and slice-2 enforcement will block. It asserts only
// the vacuity floor (the derivation ran over real data), never the strangers themselves.
func TestProxyFenceStrangerReport(t *testing.T) {
	cfg, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	domains, err := deriveProviderDomains(cfg)
	if err != nil {
		t.Fatalf("derive provider domains for the stranger report: %v", err)
	}
	if len(domains) < 2 {
		t.Fatalf("derived only %d provider domain(s) from litellm-config.yaml (%v) — the ladder reaches more "+
			"than that; the parse is broken, so the stranger report would be a false 'all clear'", len(domains), domains)
	}

	// The fence's PUBLIC domains (the only fence entries a public provider domain could match). sidecar and
	// the ollama env-ref are direct lanes, not public domains, so they never classify a provider as in-fence.
	fenceDomains := map[string]bool{}
	for _, e := range ruledFence {
		if strings.Contains(e, ".") && !strings.Contains(e, "$") {
			fenceDomains[e] = true
		}
	}

	var strangers, inFence []string
	for d, model := range domains {
		if fenceDomains[d] {
			inFence = append(inFence, fmt.Sprintf("%s (%s)", d, model))
		} else {
			strangers = append(strangers, fmt.Sprintf("%s (%s)", d, model))
		}
	}
	sort.Strings(strangers)
	sort.Strings(inFence)
	t.Logf("fence PUBLIC endpoint(s) reached by the ladder: %v", inFence)
	t.Logf("STRANGERS — litellm-config provider domains OUTSIDE the fence (logged in observe, blocked in slice 2): %v", strangers)
}

// TestProxyFenceStrangerDerivationReactsToANewProvider is the standing red-proof for JOB B: adding a
// provider to the ladder MUST enlarge the derived domain set, so a new provider cannot slip out of the
// stranger report unseen. (Manual proof: add `xai/grok-*` to litellm-config.yaml and the report gains
// api.x.ai as a stranger.)
func TestProxyFenceStrangerDerivationReactsToANewProvider(t *testing.T) {
	base := "model_list:\n" +
		"  - model_name: primary\n" +
		"    litellm_params:\n" +
		"      model: mistral/mistral-large-latest\n"
	added := base +
		"  - model_name: newarm\n" +
		"    litellm_params:\n" +
		"      model: xai/grok-4\n"

	d0, err := deriveProviderDomains([]byte(base))
	if err != nil {
		t.Fatalf("derive base: %v", err)
	}
	if _, ok := d0["api.x.ai"]; ok {
		t.Fatal("api.x.ai present before the xai provider was added — the fixture is wrong")
	}
	d1, err := deriveProviderDomains([]byte(added))
	if err != nil {
		t.Fatalf("derive added: %v", err)
	}
	if _, ok := d1["api.x.ai"]; !ok {
		t.Fatalf("adding an xai provider did NOT add api.x.ai to the derived set (%v) — the stranger report is "+
			"blind to new providers", d1)
	}
}

// TestProxyFenceStrangerDerivationFailsClosedOnUnknownProvider: a provider with no api_base and no known API
// domain must ERROR, never silently drop — otherwise it would vanish from the stranger delta unnoticed.
func TestProxyFenceStrangerDerivationFailsClosedOnUnknownProvider(t *testing.T) {
	cfg := "model_list:\n" +
		"  - model_name: primary\n" +
		"    litellm_params:\n" +
		"      model: cohere/command-r-plus\n"
	if _, err := deriveProviderDomains([]byte(cfg)); err == nil {
		t.Fatal("a provider with no known API domain and no api_base was derived WITHOUT error — the stranger " +
			"report must fail closed so an un-mappable provider cannot silently drop out of the delta")
	}
}

// TestProxyFenceStrangerDerivationExemptsDeploySetAndInternalApiBases: the sidecar (Anthropic) and ollama
// (embed) lanes resolve their api_base from the box .env (os.environ/…) and internal/private hosts are the
// fence's DIRECT lanes — none is a public provider hop, so none may appear as a stranger.
func TestProxyFenceStrangerDerivationExemptsDeploySetAndInternalApiBases(t *testing.T) {
	cfg := "model_list:\n" +
		"  - model_name: opus-cc\n" +
		"    litellm_params:\n" +
		"      model: openai/opus-cc\n" +
		"      api_base: os.environ/TG_SIDECAR_BASE\n" +
		"  - model_name: embed-nomic\n" +
		"    litellm_params:\n" +
		"      model: ollama/nomic-embed-text\n" +
		"      api_base: os.environ/TG_OLLAMA_BASE\n" +
		"  - model_name: internal\n" +
		"    litellm_params:\n" +
		"      model: openai/whatever\n" +
		"      api_base: http://sidecar:8094/v1\n" +
		"  - model_name: lanip\n" +
		"    litellm_params:\n" +
		"      model: openai/whatever\n" +
		"      api_base: http://192.168.1.9:11434\n"
	d, err := deriveProviderDomains([]byte(cfg))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(d) != 0 {
		t.Fatalf("deploy-set/internal/private api_bases produced provider domains %v — these are the fence's "+
			"direct lanes, never strangers", d)
	}
}

// TestLitellmEgressProxyShipsUnarmed is the parity guard for the new compose vars (the TG-384 lesson applied
// to the proxy arming knob): the DEFAULTS in the litellm environment block must keep today's DIRECT path, so
// merging this MR cannot change how the running brain reaches its providers. Arming is a deliberate .env
// override at deploy time, never a committed default.
func TestLitellmEgressProxyShipsUnarmed(t *testing.T) {
	block, ok := composeServiceBlock(composeFile(t), "litellm")
	if !ok {
		t.Fatal("no litellm service block in docker-compose.yml — this guard's subject is gone")
	}

	// HTTPS_PROXY must default EMPTY. A non-empty default (e.g. :-http://tg-egress-proxy:8888}) would arm the
	// proxy for EVERY deployment on merge — exactly what TG-420's RISK NOTE forbids on a single-brain path.
	for _, key := range []string{"HTTPS_PROXY", "https_proxy"} {
		want := key + ": ${TG_LITELLM_HTTPS_PROXY:-}"
		if !strings.Contains(block, want) {
			t.Errorf("litellm %s does not default EMPTY. Expected the line `%s` (empty default = today's "+
				"direct path). Anything else arms the egress proxy on merge, which must be a deliberate .env "+
				"override, not a committed default.", key, want)
		}
	}

	// NO_PROXY's default must keep the fence's direct lanes (sidecar + loopback + RFC1918) off the proxy.
	def := noProxyDefault(block)
	if def == "" {
		t.Fatal("litellm declares no NO_PROXY with a ${TG_LITELLM_NO_PROXY:-…} default — the sidecar/embed/" +
			"loopback direct lanes would be unset when the proxy is armed, breaking the Anthropic + embed lanes")
	}
	for _, exempt := range []string{"sidecar", "localhost", "127.0.0.1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !strings.Contains(def, exempt) {
			t.Errorf("litellm NO_PROXY default is missing the %q exemption (default=%q). Each keeps a fence "+
				"DIRECT lane off the proxy when armed; dropping one routes that lane through the proxy, which "+
				"for the sidecar (Anthropic) means losing the brain.", exempt, def)
		}
	}

	// HTTP_PROXY must NOT be set: the sidecar + ollama lanes are HTTP, and leaving HTTP_PROXY unset is what
	// keeps them direct. (Comments naming HTTP_PROXY are stripped first — this repo has passed a guard on its
	// own comment before.)
	stripped := stripYAMLCommentLines(block)
	for _, key := range []string{"HTTP_PROXY:", "http_proxy:"} {
		if strings.Contains(stripped, key) {
			t.Errorf("litellm declares %s — the sidecar (http://sidecar) and ollama (http://…) lanes are HTTP, "+
				"so setting HTTP_PROXY would route them through the egress proxy. Leave it unset so they stay "+
				"direct; api.z.ai is HTTPS and goes through HTTPS_PROXY alone.", key)
		}
	}
}

// noProxyDefault pulls the default value out of `NO_PROXY: ${TG_LITELLM_NO_PROXY:-<default>}` in a compose
// service block (the first occurrence). Returns "" if absent.
func noProxyDefault(block string) string {
	const marker = "NO_PROXY: ${TG_LITELLM_NO_PROXY:-"
	i := strings.Index(block, marker)
	if i < 0 {
		return ""
	}
	rest := block[i+len(marker):]
	j := strings.IndexByte(rest, '}')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// TestEgressProxyServiceIsProfileGated: the proxy must be behind the `egress-proxy` compose profile so a
// bare `up -d` does NOT start it — arming stays deliberate (TG-420 RISK NOTE: do not enable blind).
func TestEgressProxyServiceIsProfileGated(t *testing.T) {
	svcs := composeServices(t)
	proxy, ok := svcs["tg-egress-proxy"]
	if !ok {
		t.Fatal("no tg-egress-proxy service in docker-compose.yml — the egress proxy is the subject of TG-420")
	}
	found := false
	for _, p := range stringList(proxy["profiles"]) {
		if strings.TrimSpace(p) == "egress-proxy" {
			found = true
		}
	}
	if !found {
		t.Errorf("tg-egress-proxy is not behind the `egress-proxy` compose profile (profiles=%v). Without it a "+
			"bare `up -d` starts the proxy; it must be opt-in so arming is a deliberate act.", stringList(proxy["profiles"]))
	}
}
