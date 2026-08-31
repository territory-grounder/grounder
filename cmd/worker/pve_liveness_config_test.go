package main

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/credential"
)

// envGet builds a `get` in the shape resolvePVELivenessPair takes, over a fixed map.
func envGet(m map[string]string) func(k, def string) string {
	return func(k, def string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return def
	}
}

// THE ESTATE READ PAIR IS PREFERRED, AND THE WHOLE PAIR TRAVELS TOGETHER.
//
// KILLING MUTATION: return the TG_PROXMOX_* pair first (the pre-TG-350 behaviour). RED — that is the
// configuration in which a GET-only detector authenticates with the guest-lifecycle WRITE token, and on a
// split deployment cannot authenticate at all.
func TestPVELivenessPrefersTheEstateReadPair(t *testing.T) {
	p, ok := resolvePVELivenessPair(envGet(map[string]string{
		"TG_PVE_URL":              "https://pve.example:8006",
		"TG_PVE_TOKEN_REF":        "bao:secret/data/tg/pve#token",
		"TG_PVE_INSECURE":         "true",
		"TG_PROXMOX_BASE_URL":     "https://pve.example:8006",
		"TG_PROXMOX_TOKEN_REF":    "bao:secret/data/tg/proxmox#token",
		"TG_PROXMOX_INSECURE":     "",
		"TG_PROXMOX_ALLOWED_GUES": "ignored",
	}))
	if !ok {
		t.Fatal("a fully-configured read pair did not resolve")
	}
	if p.name != "estate-read" {
		t.Fatalf("resolved the %q pair with BOTH pairs configured — the detector is read-only and must not "+
			"authenticate with the actuation lane's write token", p.name)
	}
	if p.tokenRef != "bao:secret/data/tg/pve#token" {
		t.Errorf("token = %q, want the estate read reference", p.tokenRef)
	}
	if p.tokenKey != "TG_PVE_TOKEN_REF" {
		t.Errorf("tokenKey = %q — the boot log would send an operator to the wrong variable", p.tokenKey)
	}
	// The TLS flag must come from the SAME pair. Taking the token from the read pair and the flag from
	// TG_PROXMOX_INSECURE (unset here, i.e. enforcing) against a self-signed endpoint swaps a
	// missing-credential failure for a certificate failure — and a poller whose GET never completes reports
	// no down guests, which is indistinguishable from an estate where nothing is down.
	if !p.insecure {
		t.Errorf("resolved the read pair's endpoint+token but NOT its TLS flag (TG_PVE_INSECURE=true): "+
			"insecure=%v via %q", p.insecure, p.insecureKey)
	}
	if p.insecureKey != "TG_PVE_INSECURE" {
		t.Errorf("insecureKey = %q, want TG_PVE_INSECURE", p.insecureKey)
	}
}

// TG_PVE_RO_TOKEN_REF wins over TG_PVE_TOKEN_REF: an operator who declared a read-ONLY reference meant it for
// a read path.
func TestPVELivenessPrefersTheDeclaredReadOnlyReference(t *testing.T) {
	p, _ := resolvePVELivenessPair(envGet(map[string]string{
		"TG_PVE_URL":          "https://pve.example:8006",
		"TG_PVE_RO_TOKEN_REF": "bao:secret/data/tg/pve-ro#token",
		"TG_PVE_TOKEN_REF":    "bao:secret/data/tg/pve#token",
	}))
	if p.tokenRef != "bao:secret/data/tg/pve-ro#token" || p.tokenKey != "TG_PVE_RO_TOKEN_REF" {
		t.Fatalf("read-only reference not preferred: %s = %q", p.tokenKey, p.tokenRef)
	}
}

// EXISTING `both`-PLANE INSTALLS MUST NOT LOSE THE DETECTOR. They configure only TG_PROXMOX_*.
//
// KILLING MUTATION: drop the fallback and resolve the read pair only. RED — every deployment that has not
// split its planes silently loses TG's fastest detector on upgrade, which is the same outage this ticket
// is fixing, pointed the other way.
func TestPVELivenessFallsBackToTheProxmoxPairForBothPlaneInstalls(t *testing.T) {
	p, ok := resolvePVELivenessPair(envGet(map[string]string{
		"TG_PROXMOX_BASE_URL":  "https://pve.example:8006",
		"TG_PROXMOX_TOKEN_REF": "env:PROXMOX_TOKEN",
		"TG_PROXMOX_INSECURE":  "1",
	}))
	if !ok || p.name != "actuation-write" {
		t.Fatalf("a TG_PROXMOX_*-only deployment lost its detector: ok=%v pair=%q", ok, p.name)
	}
	if p.baseURL == "" || p.tokenRef != "env:PROXMOX_TOKEN" || !p.insecure {
		t.Fatalf("fallback pair is incomplete: %+v", p)
	}
}

// THE TWO PAIRS ARE NEVER MIXED. A half-configured read pair falls through to the write pair ENTIRELY —
// endpoint, token and TLS flag — rather than contributing its endpoint to a write token.
//
// KILLING MUTATION: resolve the URL and the token independently (`url := PVE_URL else PROXMOX_BASE_URL;
// token := PVE_TOKEN_REF else PROXMOX_TOKEN_REF`). GREEN on both single-pair fixtures above and RED here —
// which is exactly why this case is written out rather than assumed to be covered by them.
func TestPVELivenessNeverMixesTheTwoPairs(t *testing.T) {
	p, ok := resolvePVELivenessPair(envGet(map[string]string{
		"TG_PVE_URL":           "https://read.example:8006", // read pair: endpoint but NO token
		"TG_PVE_INSECURE":      "true",
		"TG_PROXMOX_BASE_URL":  "https://write.example:8006",
		"TG_PROXMOX_TOKEN_REF": "env:PROXMOX_TOKEN",
	}))
	if !ok {
		t.Fatal("a complete write pair did not resolve when the read pair was half-configured")
	}
	if p.baseURL != "https://write.example:8006" {
		t.Errorf("endpoint %q came from the read pair while the token came from the write pair — the two "+
			"halves are not required to describe the same conversation", p.baseURL)
	}
	if p.insecure {
		t.Errorf("TLS flag came from TG_PVE_INSECURE while the credential came from TG_PROXMOX_TOKEN_REF")
	}
	if p.name != "actuation-write" {
		t.Errorf("pair name %q does not describe where the credential came from", p.name)
	}
}

// NOTHING CONFIGURED ⇒ NO PAIR, and the caller's idle branch fires. A pair that resolved with an empty token
// would construct a poller that 401s every tick and reports no down guests.
func TestPVELivenessResolvesNoPairWhenNothingIsConfigured(t *testing.T) {
	if p, ok := resolvePVELivenessPair(envGet(map[string]string{})); ok {
		t.Fatalf("an empty configuration resolved a pair: %+v", p)
	}
	if p, ok := resolvePVELivenessPair(envGet(map[string]string{"TG_PVE_URL": "https://pve.example:8006"})); ok {
		t.Fatalf("an endpoint with no credential resolved a pair: %+v", p)
	}
}

// ON THE TRIAGE PLANE THE ACTUATION FALLBACK IS UNREACHABLE, BY ACQUISITION.
//
// This is the property that makes the fix a fix rather than a preference. planeEnv withholds
// TG_PROXMOX_TOKEN_REF from a triage-plane process, so even a `both`-shaped .env cannot arm the detector with
// the write token: it is never handed to this process at all.
//
// KILLING MUTATION: call the resolver with `getenv` instead of `planeEnv` (which is what the composition root
// did before TG-350). RED — the triage plane resolves the write pair.
func TestOnTheTriagePlaneOnlyTheReadPairCanArmTheDetector(t *testing.T) {
	prev := credentialPlane
	credentialPlane = credential.ProcessPlaneTriage
	t.Cleanup(func() { credentialPlane = prev })

	t.Setenv("TG_PVE_URL", "")
	t.Setenv("TG_PVE_RO_TOKEN_REF", "")
	t.Setenv("TG_PVE_TOKEN_REF", "")
	t.Setenv("TG_PROXMOX_BASE_URL", "https://pve.example:8006")
	t.Setenv("TG_PROXMOX_TOKEN_REF", "bao:secret/data/tg/proxmox#token")

	if p, ok := resolvePVELivenessPair(planeEnv); ok {
		t.Fatalf("the triage plane resolved the %q pair holding %q — planeEnv is meant to withhold the "+
			"guest-lifecycle WRITE token from this process entirely", p.name, p.tokenKey)
	}
	// …and the same configuration DOES arm on a `both` worker, so the refusal above is the plane split and
	// not a resolver that never works.
	credentialPlane = credential.ProcessPlaneBoth
	if _, ok := resolvePVELivenessPair(planeEnv); !ok {
		t.Fatal("the same configuration failed to resolve on a `both` worker — the refusal above proves " +
			"nothing about the plane split")
	}
}

// THE ALLOWLIST FAIL-SAFE SURVIVES THE WIDENING. Empty ⇒ watch nothing, on every key.
//
// KILLING MUTATION: return every guest the read token can see when no list is configured. RED — on this
// estate that is 195 guests across two sites including ~50 on an offline hypervisor, and a maintenance
// shutdown of an unrelated guest would mint a triage.
func TestAnEmptyGuestListStillWatchesNothing(t *testing.T) {
	g, key := resolvePVELivenessGuests(envGet(map[string]string{
		"TG_PVE_LIVENESS_GUESTS":    "",
		"TG_PROXMOX_ALLOWED_GUESTS": "",
	}))
	if len(g) != 0 {
		t.Fatalf("an empty allowlist produced %d guest(s) from %q — the detector would fire on guests the "+
			"operator never declared managed", len(g), key)
	}
	if key != "" {
		t.Errorf("no list was configured but the resolver reported source %q", key)
	}
}

// The triage-side key is preferred, and the actuation-side list still works for `both` installs.
func TestGuestListPrefersTheTriageSideKeyAndKeepsTheFallback(t *testing.T) {
	g, key := resolvePVELivenessGuests(envGet(map[string]string{
		"TG_PVE_LIVENESS_GUESTS":    "a01, b01",
		"TG_PROXMOX_ALLOWED_GUESTS": "c01",
	}))
	if key != "TG_PVE_LIVENESS_GUESTS" || len(g) != 2 || g[0] != "a01" || g[1] != "b01" {
		t.Fatalf("triage-side list not preferred: %v from %q", g, key)
	}
	g, key = resolvePVELivenessGuests(envGet(map[string]string{"TG_PROXMOX_ALLOWED_GUESTS": "c01,d01"}))
	if key != "TG_PROXMOX_ALLOWED_GUESTS" || len(g) != 2 {
		t.Fatalf("`both`-plane deployments lost their allowlist: %v from %q", g, key)
	}
}

// THE COMPOSITION ROOT, NOT THE RESOLVER.
//
// Three times on 2026-08-06 a resolver was guarded correctly while main() went on calling the old thing, and
// every unit test stayed green. So this reads the liveness block out of main.go, STRIPS COMMENTS (the
// rationale above mentions TG_PROXMOX_TOKEN_REF by name, and a comment is not a call), and asserts on what
// the block executes.
//
// KILLING MUTATION: revert the block's first two lines to the getenv("TG_PROXMOX_*") reads. RED.
func TestTheLivenessBlockResolvesThePairAtTheCompositionRoot(t *testing.T) {
	block := livenessBlockCode(t)

	for _, want := range []string{
		"resolvePVELivenessPair(planeEnv)",   // planeEnv, not getenv — the withholding is the control
		"resolvePVELivenessGuests(planeEnv)", //
		"estateHTTPClient(pvePair.insecure)", // the TLS flag travels with the pair it came from
		"pvePair.baseURL",
		"pvePair.tokenRef",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the pve-liveness composition block does not contain %q — the resolver is guarded and "+
				"the wiring is not.\n%s", want, block)
		}
	}
	for _, forbidden := range []string{
		`getenv("TG_PROXMOX_TOKEN_REF"`,
		`getenv("TG_PROXMOX_BASE_URL"`,
		`truthyEnv("TG_PROXMOX_INSECURE")`,
	} {
		if strings.Contains(block, forbidden) {
			t.Errorf("the pve-liveness composition block still reads %s directly. That bypasses planeEnv, so "+
				"a triage-plane worker resolves the guest-lifecycle WRITE token for a GET (TG-350).\n%s",
				forbidden, block)
		}
	}
}

// NEGATIVE CONTROL for the extractor. If livenessBlockCode ever returns the whole file, or an empty string,
// the assertions above become vacuous — file-wide they would pass on the resolver's own source, and on ""
// every Contains fails but every forbidden check passes.
func TestTheLivenessBlockExtractorIsScoped(t *testing.T) {
	block := livenessBlockCode(t)
	if strings.TrimSpace(block) == "" {
		t.Fatal("extracted an empty block — every forbidden-substring assertion above would pass vacuously")
	}
	// The window must START at the liveness guard. Widening the anchor (say, to `package main`) does not
	// return the whole file — brace-counting finds the first balanced group instead — so "the block is not
	// huge" is NOT evidence that it is the right block. Pin the opening line itself.
	if !strings.HasPrefix(strings.TrimSpace(block), `if iv := planeEnv("TG_PVE_LIVENESS_POLL_INTERVAL"`) {
		t.Fatalf("the extracted window does not open on the pve-liveness guard, so the assertions above are "+
			"scanning some other block of main.go:\n%.200s", block)
	}
	if strings.Contains(block, "func main(") || strings.Contains(block, "librenms: alert intake is PUSH") {
		t.Fatal("the extractor returned more than the liveness block, so a match anywhere in main.go would " +
			"satisfy the assertions above")
	}
	// A guest allowlist read is IN this block; the actuation lane's own guest-pool read (elsewhere in main.go)
	// is not — proof the window has a right-hand edge.
	if strings.Contains(block, "no guest in TG_PROXMOX_ALLOWED_GUESTS") {
		t.Fatal("the window extends past the liveness block into the actuation guest-pool report")
	}
}

// livenessBlockCode returns the pve-liveness composition block from main.go with comment lines removed.
func livenessBlockCode(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	const open = `if iv := planeEnv("TG_PVE_LIVENESS_POLL_INTERVAL", ""); iv != "" {`
	i := strings.Index(string(src), open)
	if i < 0 {
		t.Fatalf("the pve-liveness block is no longer opened by %q — this guard is scanning for a shape "+
			"that no longer exists and would pass on any wiring", open)
	}
	rest := string(src)[i:]
	depth, end := 0, -1
	for off, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = off + 1
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatal("the pve-liveness block is unbalanced — cannot scope this guard")
	}
	var kept []string
	for _, ln := range strings.Split(rest[:end], "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}
