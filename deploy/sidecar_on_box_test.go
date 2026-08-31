package deploy

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-413 — TG'S PRIMARY BRAIN RAN ON THE CODING AGENT'S DEV WORKSTATION.
//
// `dc1claude01` is the machine the agent works from. TG's production config pointed at it in eleven
// places, and not only for the head-to-head arm — the `primary` and `fast` aliases resolved there too, so
// the whole model path depended on a box outside TG's deploy, its monitoring and its supply chain. The
// same host carries a pile of scratch fixtures (tg-local-*pg, tg287-*, tg289-*), which is what a
// workstation looks like and is exactly why a production dependency must not live on one.
//
// These oracles pin the properties that keep it from drifting back.

func sidecarComposeDoc(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var d map[string]any
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	return d
}

func sidecarOnBoxService(t *testing.T) map[string]any {
	t.Helper()
	svcs, _ := sidecarComposeDoc(t)["services"].(map[string]any)
	sc, ok := svcs["sidecar"].(map[string]any)
	if !ok {
		t.Fatal("no `sidecar` service in docker-compose.yml — the model sidecar would still have to run " +
			"off-box, on the agent's dev workstation")
	}
	return sc
}

// THE LOAD-BEARING ORACLE: no estate host may be hardcoded as a model endpoint.
//
// The five api_base entries resolved to a literal host. Routing them through TG_SIDECAR_BASE is what
// makes the migration revertible WITHOUT a code change — which matters because a sidecar outage fails TG
// triage loudly by design ("No fallbacks", litellm-config.yaml), so the cutover must be reversible in one
// deploy-config edit.
func TestNoModelEndpointIsAHardcodedEstateHost(t *testing.T) {
	b, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	var cfg struct {
		ModelList []struct {
			ModelName  string         `yaml:"model_name"`
			LiteLLMPar map[string]any `yaml:"litellm_params"`
		} `yaml:"model_list"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse litellm-config.yaml: %v", err)
	}
	if len(cfg.ModelList) == 0 {
		t.Fatal("parsed no model_list entries — every assertion below would be vacuous")
	}
	var checked int
	for _, m := range cfg.ModelList {
		base, _ := m.LiteLLMPar["api_base"].(string)
		if base == "" {
			continue
		}
		checked++
		if strings.Contains(base, "dc1claude01") {
			t.Errorf("model %q dials %q — that host is the coding agent's DEV WORKSTATION, not a "+
				"deployment target", m.ModelName, base)
		}
	}
	if checked == 0 {
		t.Fatal("no api_base found on any model — the loop asserted nothing")
	}
}

// The sidecar must NOT publish a host port. On-box it is reached over the compose network; a published
// port puts the model channel back on the management LAN, which is the whole reason TG-287 had to build a
// TLS terminator and a CA-trust chain in the first place.
func TestTheSidecarPublishesNoHostPort(t *testing.T) {
	sc := sidecarOnBoxService(t)
	if p, ok := sc["ports"]; ok {
		t.Errorf("the sidecar publishes ports %v — on-box it is reached over tg-backplane by service "+
			"name, and exposing it re-creates the cleartext LAN hop TG-287 existed to close", p)
	}
}

// It must reach litellm AND the provider, and nothing else must reach it.
func TestTheSidecarJoinsTheBackplaneAndEgressButNotTheFrontDoor(t *testing.T) {
	sc := sidecarOnBoxService(t)
	var nets []string
	for _, n := range sc["networks"].([]any) {
		nets = append(nets, n.(string))
	}
	joined := strings.Join(nets, ",")
	for _, want := range []string{"tg-backplane", "tg-egress"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the sidecar is not on %s (networks=%v) — it needs the backplane so litellm can reach "+
				"it, and egress to dial the provider", want, nets)
		}
	}
	if strings.Contains(joined, "tg-frontdoor") {
		t.Errorf("the sidecar joins tg-frontdoor (networks=%v) — nothing outside the stack should reach "+
			"the model sidecar", nets)
	}
}

// ANTHROPIC_API_KEY must be ABSENT. If present, the CLI prefers it and bills per token instead of using
// the operator's subscription — a silent cost change with no functional symptom.
func TestTheSidecarCarriesNoAnthropicAPIKey(t *testing.T) {
	env, _ := sidecarOnBoxService(t)["environment"].(map[string]any)
	if _, present := env["ANTHROPIC_API_KEY"]; present {
		t.Error("ANTHROPIC_API_KEY is set on the sidecar — the CLI prefers it over the subscription and " +
			"bills per token, which has no functional symptom and so would not be noticed")
	}
}

// THE TOKEN MUST NOT BE A COMPOSE ENVIRONMENT VALUE AT ALL (INV-13).
//
// An env value here — even a `${...}` indirection — lands in `docker inspect` and in the deploying
// process's own environment. The token is instead resolved from OpenBao by sidecar-secrets and SOURCED
// from a 0600 drop at start, the same path litellm's provider keys take. This guard pins the absence,
// because "we moved it to the vault" is exactly the kind of claim that silently regresses to a
// passthrough when someone debugs a startup failure.
func TestTheSidecarTokenIsNotAComposeEnvironmentValue(t *testing.T) {
	sc := sidecarOnBoxService(t)
	env, _ := sc["environment"].(map[string]any)
	if v, present := env["CLAUDE_CODE_OAUTH_TOKEN"]; present {
		t.Errorf("CLAUDE_CODE_OAUTH_TOKEN is a compose environment value (%v) — it would appear in "+
			"`docker inspect` and in the deploy's own environment. It must be resolved from OpenBao and "+
			"sourced from the secrets drop instead (INV-13)", v)
	}
	// ...and the drop must actually be wired, or the absence above is just a broken sidecar.
	dep, _ := sc["depends_on"].(map[string]any)
	if _, ok := dep["sidecar-secrets"]; !ok {
		t.Error("the sidecar does not depend on sidecar-secrets — with no token source it would start " +
			"unauthenticated, which is the failure this arrangement exists to make impossible")
	}
	ep, _ := sc["entrypoint"].([]any)
	var joined string
	for _, x := range ep {
		joined += x.(string) + " "
	}
	if !strings.Contains(joined, "/run/sidecar-secrets/env") {
		t.Errorf("the sidecar entrypoint does not source the secrets drop (%q) — the init would write a "+
			"token nothing reads", joined)
	}
}

// THE SIDECAR IMAGE MUST BE NEITHER ${TG_TAG} NOR A MUTABLE TAG. Both were tried; both were refused, and
// by different mechanisms, which is why this guard pins both halves.
//
//   - ${TG_TAG} FAILS TO EXIST. CI builds this image only when deploy/claude-proxy/** changes
//     (.gitlab-ci.yml `image-sidecar` rules), so a per-commit sha tag exists for proxy commits ONLY. The
//     first real pull failed `manifest unknown` (TG_TAG=99080090 on the box, no matching claude-proxy
//     tag) and would have done so on nearly every deploy that did not touch the proxy.
//   - `latest` EXISTS AND MOVES, which is worse. It is re-pointed by every proxy build, so the deployed
//     brain can change underneath a stack whose own commit never changed. `lint-image-pins` refused it
//     in CI; that gate's exemption is structurally `${TG_*_IMAGE}:${TG_TAG}` — this pipeline's
//     per-commit output — and `latest` is not that, whoever built it.
//
// A digest satisfies both. The cost is real and is stated in the compose comment: a proxy rebuild does
// not reach production until the digest is bumped.
func TestTheSidecarImageIsDigestPinnedAndNotTheStackTag(t *testing.T) {
	img, _ := sidecarOnBoxService(t)["image"].(string)
	if img == "" {
		t.Fatal("the sidecar declares no image")
	}
	if strings.Contains(img, "${TG_TAG") {
		t.Errorf("the sidecar image is pinned to the stack tag (%s). CI does not build this image on every "+
			"commit, so that tag usually does not exist and the pull fails `manifest unknown`.", img)
	}
	if !strings.Contains(img, "@sha256:") {
		t.Errorf("the sidecar image (%s) is not digest-pinned. A moving tag lets the deployed BRAIN change "+
			"with no commit anywhere in this repo — and lint-image-pins refuses it in CI, so this would go "+
			"red later and further from the cause.", img)
	}
	// The override must survive, or an urgent roll-forward becomes a code change and a full deploy.
	if !strings.Contains(img, "${TG_SIDECAR_IMAGE") {
		t.Errorf("the sidecar image (%s) is not overridable via TG_SIDECAR_IMAGE — with a digest pin and no "+
			"override, rolling forward in an incident requires editing and shipping this repo", img)
	}
}

// THE SHIPPED DEFAULT ENDPOINT MUST NOT BE AN ESTATE LITERAL.
//
// This is the guard for the mistake that produced this whole arrangement. To make the migration inert,
// the first draft defaulted TG_SIDECAR_BASE to the OLD off-box address — moving a site literal INTO the
// shipped artifact, which is exactly what `lint-forbidden` 6/7 (STONITH) exists to refuse. Its remedy is
// the design: site config is a deploy-time override, never baked into the image.
//
// The gate already catches the specific estate-hostname shape. This catches the general one: any default
// that names a host outside this compose file.
func TestTheSidecarBaseDefaultIsComposeInternalNotASiteAddress(t *testing.T) {
	svcs, _ := sidecarComposeDoc(t)["services"].(map[string]any)
	litellm, ok := svcs["litellm"].(map[string]any)
	if !ok {
		t.Fatal("no `litellm` service — this guard's subject is absent and it would assert nothing")
	}
	env, _ := litellm["environment"].(map[string]any)
	base, present := env["TG_SIDECAR_BASE"]
	if !present {
		t.Fatal("litellm declares no TG_SIDECAR_BASE — litellm-config.yaml resolves the model endpoint " +
			"through os.environ/TG_SIDECAR_BASE, so an absent variable leaves every model alias with an " +
			"unresolvable api_base")
	}
	s, _ := base.(string)
	if !strings.Contains(s, "sidecar:8094") {
		t.Errorf("TG_SIDECAR_BASE defaults to %q — production runs the sidecar as a compose service on "+
			"this box, so the shipped default must be the compose-internal address. A default naming any "+
			"other host is site config baked into the image (lint-forbidden 6/7).", s)
	}
	if strings.Contains(s, "dc1") || strings.Contains(s, "dc2") {
		t.Errorf("TG_SIDECAR_BASE defaults to an estate host (%q) — that is a STONITH literal in a shipped "+
			"artifact. Put site addresses in the deploy's .env, which is what makes the cutover revertible "+
			"without a code change in the first place.", s)
	}
}
