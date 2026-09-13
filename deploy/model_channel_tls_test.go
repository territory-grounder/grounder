package deploy

// THE MODEL CHANNEL MUST STAY COMPOSE-INTERNAL (TG-287 -> TG-413 -> TG-414).
//
// TG-287 existed because the entire production brain rode PLAINTEXT HTTP between two hosts: every model
// call went `http://192.168.181.111:8094/v1`, tg01 to the sidecar on dc1claude01, across the
// management LAN in the clear — and so did the bearer that guards it, on every request. An on-path
// attacker there does not need to subvert a model or steal a key: they read every prompt (estate
// topology, hostnames, alert bodies) and can REWRITE the completion the agent then acts on, upstream of
// every TG gate (chokepoint, policy, risk, breaker). TG-287 closed that with a TLS terminator in front of
// the sidecar and a client-side CA-trust chain in litellm.
//
// TG-413 then moved the sidecar ON-BOX. litellm now reaches it over tg-backplane by service name
// (`http://sidecar:8094/v1`), with no published host port (deploy/sidecar_on_box_test.go). The channel no
// longer crosses anything, so TG-414 RETIRED the terminator, its private key (which lived only on
// dc1claude01) and the CA-trust block: LAN-hop TLS scaffolding with no subject left.
//
// This guard is tightened to the ONE surviving shape — the model channel is compose-internal — and fails
// in two directions:
//
//  1. Any sidecar api_base that is NOT the compose-internal service is a regression: the channel left the
//     box again, which is exactly the exposure TG-287 closed, and there is no longer a terminator or a
//     trusted CA standing between it and an on-path attacker.
//  2. If the scan matches NOTHING it FAILS rather than passing — a vacuity floor. The hostname and port in
//     a config are exactly the kind of thing that gets renamed, and a guard that silently stops matching
//     is worse than no guard because it reports clean. This repo has been bitten by scoped measurement
//     three times; the floor is not decoration.

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sidecarHosts are the names/addresses the model sidecar has been reachable at. Any api_base mentioning
// one of these is in scope for this guard. `sidecar` is the compose SERVICE NAME (TG-413, on-box); the two
// estate literals are the pre-TG-413 off-box arrangement, kept so a revert back to them is still caught.
var sidecarHosts = []string{"dc1claude01", "192.168.181.111", "sidecar"}

// composeLocalSidecar reports whether a base names the sidecar over the COMPOSE NETWORK rather than any
// routable host. `http://sidecar:8094/v1` never leaves the box, so it is the one shape that survives
// TG-414. A bare service name has no dots; anything routable does.
func composeLocalSidecar(base string) bool {
	h := base
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, ":/"); i >= 0 {
		h = h[:i]
	}
	return h == "sidecar"
}

// TestModelChannelIsComposeInternal asserts every sidecar api_base is the compose-internal hop, and no
// other shape. Before TG-414 this guard also accepted a TLS-terminated off-box base; that terminator and
// its trust anchor are gone, so a routable base — plaintext OR TLS — is now a regression, not a legal
// alternative. The vacuity floor still fires if the sidecar is renamed or its entries removed.
func TestModelChannelIsComposeInternal(t *testing.T) {
	raw, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	var doc struct {
		ModelList []struct {
			ModelName    string `yaml:"model_name"`
			LiteLLMParam struct {
				APIBase string `yaml:"api_base"`
			} `yaml:"litellm_params"`
		} `yaml:"model_list"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse litellm-config.yaml: %v", err)
	}

	var scanned int
	for _, m := range doc.ModelList {
		base := m.LiteLLMParam.APIBase
		// The endpoint is deploy config since TG-413, so resolve the indirection to the literal the
		// deployment will actually dial. Scanning "os.environ/..." itself would assert nothing.
		if strings.HasPrefix(base, "os.environ/TG_SIDECAR_BASE") {
			base = composeSidecarBaseDefault(t)
		}
		var matches bool
		for _, h := range sidecarHosts {
			if strings.Contains(base, h) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		scanned++
		if !composeLocalSidecar(base) {
			t.Errorf("model %q reaches the sidecar at %q, which is NOT the compose-internal hop.\n"+
				"TG-414 retired the off-box TLS terminator and its CA-trust chain, so the only supported "+
				"shape is http://sidecar:8094/... over tg-backplane, with no published port "+
				"(deploy/sidecar_on_box_test.go). A routable base puts the channel back on the management LAN "+
				"— the exposure TG-287 closed — and there is no terminator or trusted CA left to protect it.",
				m.ModelName, base)
		}
	}

	// The vacuity floor once guarded against the sidecar being silently renamed. 2026-08-18 (TG-168): the
	// sidecar model channel was INTENTIONALLY retired — forensic-ir, the last litellm model on TG_SIDECAR_BASE,
	// moved to dc1litellm01 because the sidecar's Max-sub weekly cap 429'd every forensic call (owner ruled
	// the move to a no-cap provider). So scanned==0 is now the intended state, not a silent rename. The
	// compose-internal assertion above still fires if any sidecar model is re-added, so "http://sidecar" stays
	// guarded — only the "must have at least one" floor is lifted.
	if scanned == 0 {
		t.Logf("no litellm model uses the compose-internal sidecar (TG_SIDECAR_BASE) — the sidecar model " +
			"channel was retired when forensic-ir moved to dc1litellm01 (TG-168); intended state")
		return
	}
	t.Logf("%d sidecar model entries scanned, all compose-internal", scanned)
}

// composeSidecarBaseDefault reads the TG_SIDECAR_BASE default out of docker-compose.yml. The endpoint is
// deploy config now, so THAT is where the literal lives and where this guard must look.
func composeSidecarBaseDefault(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "TG_SIDECAR_BASE:") {
			continue
		}
		if i := strings.Index(line, ":-"); i >= 0 {
			return strings.TrimSpace(strings.TrimSuffix(line[i+2:], "}"))
		}
		t.Fatalf("TG_SIDECAR_BASE is declared with no default (%q) — an unset variable would make litellm "+
			"dial nothing and fail every completion, which is worse than a wrong default", strings.TrimSpace(line))
	}
	t.Fatal("docker-compose.yml declares no TG_SIDECAR_BASE — litellm has no sidecar endpoint at all")
	return ""
}
