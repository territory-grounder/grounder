package deploy

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// TG-392. `allowed_fails: 1` in router_settings is not a tuning knob — it SELECTS WHICH COOLDOWN
// ALGORITHM LiteLLM's router uses, and the two differ in whether they protect a single-endpoint model
// group.
//
// litellm/router_utils/cooldown_handlers.py:_should_cooldown_deployment takes the v2 (current) branch
// only when:
//
//	allowed_fails_policy is None  AND  _is_allowed_fails_set_on_router(...) is False
//
// and that helper returns True whenever `router.allowed_fails != litellm.allowed_fails`. Executed in the
// running container: litellm.allowed_fails defaults to 3, so `allowed_fails: 1` made 1 != 3 and the
// router took the LEGACY v1 branch.
//
// v2 gates BOTH cooldown rules on `not is_single_deployment_model_group`. v1 has no such guard: it is
// `updated_fails > allowed_fails` with the fail-counter TTL = cooldown_time = 5s. `primary` IS a
// single-deployment group, so the SECOND failure within 5s blanked the whole model group with a 429 for
// every caller in the process.
//
// Measured 2026-08-06 02:58 — two sidecar 503s, then four LiteLLM-origin 429s naming its own
// cooldown_list, then `circuit "model-primary" OPEN`, while the sidecar served 200s throughout.
//
// THIS GUARD NAMES BOTH KEYS ON PURPOSE. The branch condition has two doors: setting `allowed_fails` OR
// setting `allowed_fails_policy` sends the router down the legacy path. A rule that named only the key
// that happened to be set would leave the other one open — the phrase-list mistake this repo has made
// before. The property is "nothing in router_settings selects the legacy cooldown branch".
var legacyCooldownSelectors = []struct {
	key string
	why string
}{
	{"allowed_fails", "any value != litellm.allowed_fails (default 3) makes _is_allowed_fails_set_on_router " +
		"return True, which selects the v1 branch"},
	{"allowed_fails_policy", "a non-nil policy short-circuits the v2 condition directly — the first clause " +
		"of the branch test is `allowed_fails_policy is None`"},
}

func litellmRouterSettings(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("litellm-config.yaml")
	if err != nil {
		t.Fatalf("read litellm-config.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse litellm-config.yaml: %v", err)
	}
	// VACUITY FLOOR. An empty or mis-parsed document would make every assertion below pass by having
	// nothing to check — and this file's whole point is that a MISSING key is the correct state, so
	// "found nothing" and "correct" are otherwise indistinguishable here.
	if len(doc) < 2 {
		t.Fatalf("parsed only %d top-level key(s) from litellm-config.yaml — the file is not being read, "+
			"and an absent-key assertion over an unread file is vacuous", len(doc))
	}
	rs, ok := doc["router_settings"].(map[string]any)
	if !ok {
		t.Fatal("litellm-config.yaml has no router_settings block. This guard asserts a key is ABSENT from " +
			"it, so a missing block would satisfy that trivially while the file no longer configures the " +
			"router at all — re-anchor this test rather than deleting it.")
	}
	if len(rs) == 0 {
		t.Fatal("router_settings is empty — same vacuity problem as above")
	}
	return rs
}

// TestNothingSelectsTheLegacyCooldownBranch is the finding.
func TestNothingSelectsTheLegacyCooldownBranch(t *testing.T) {
	rs := litellmRouterSettings(t)

	for _, sel := range legacyCooldownSelectors {
		if v, present := rs[sel.key]; present {
			t.Errorf("router_settings sets %s: %v — this selects LiteLLM's LEGACY v1 cooldown branch, which "+
				"has NO single-deployment guard.\nwhy: %s\n"+
				"`primary` is a single-deployment model group, so on that branch two failures inside the 5s "+
				"fail-counter TTL blank the entire group with a 429 for every caller — which is how two "+
				"transient sidecar 503s became a model-primary breaker trip on 2026-08-06 while the sidecar "+
				"was serving 200s.", sel.key, v, sel.why)
		}
	}
}

// TestTheSettingsThatMustSurviveAreStillThere. The fix is a DELETION, and the cheapest way to make the
// test above pass is to empty router_settings entirely — which would silently drop the fallback ladder
// and num_retries: 0, both of which are load-bearing for different reasons.
func TestTheSettingsThatMustSurviveAreStillThere(t *testing.T) {
	rs := litellmRouterSettings(t)

	if _, ok := rs["num_retries"]; !ok {
		t.Error("num_retries is gone from router_settings. It is deliberately 0: an overloaded rung must " +
			"fail over immediately rather than burn seconds retrying a dead deployment, and its absence " +
			"restores LiteLLM's retry default.")
	}
	if _, ok := rs["fallbacks"]; !ok {
		t.Error("fallbacks is gone from router_settings — the judge-isolation ladder (the scorer's model is " +
			"removed from every agent-facing chain) lives there")
	}
}
