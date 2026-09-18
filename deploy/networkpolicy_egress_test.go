package deploy

// THE HELM EGRESS-ALLOWLIST GUARD (TG-160).
//
// WHY THIS EXISTS. Measured 2026-08-04: `deploy/helm/grounder/templates/` contained no NetworkPolicy of
// any kind, so a Kubernetes install of this chart put a worker running an LLM agent over untrusted alert
// content on a cluster network with unrestricted egress — and so did Postgres, and so did the Temporal UI.
// The compose stack got a three-tier network split in the same change; this file is what keeps the k8s
// half from silently regressing to nothing, which is what it was.
//
// WHY IT IS A SOURCE TEST AND NOT A RENDER TEST. There is no helm binary on the build box (`which helm`
// → not found), and CI's helm lint/template step runs elsewhere. Rather than skip — a skipped security
// test is indistinguishable from an absent one — this asserts the two properties that survive without a
// renderer and that actually matter: the VALUES contract (parseable YAML, safe default, declared shape)
// and the STRUCTURE of the template (a default-deny that cannot be separated from the allows).
//
// THE ONE PROPERTY WORTH MOST. An allow policy that renders WITHOUT the default-deny is not a weaker
// control, it is no control: Kubernetes unions NetworkPolicies, so "allow these three CIDRs" on its own
// permits everything it does not mention — because nothing denies. So the deny and the allows must be
// gated by the same single condition, with the deny first. That is asserted structurally below.

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const npTemplatePath = "helm/grounder/templates/networkpolicy.yaml"

func readNetworkPolicyTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(npTemplatePath)
	if err != nil {
		t.Fatalf("read %s: %v — the chart ships no NetworkPolicy, which is the pre-TG-160 state: every "+
			"pod in the release with unrestricted egress", npTemplatePath, err)
	}
	src := string(b)
	// VACUITY FLOOR: every assertion below is a substring search, and every substring search succeeds
	// against nothing when the haystack is empty. A truncated or emptied template must fail here rather
	// than sail through the checks that follow.
	if len(src) < 1500 || !strings.Contains(src, "kind: NetworkPolicy") {
		t.Fatalf("%s does not read as a NetworkPolicy template (%d bytes) — the checks below would pass "+
			"vacuously against it", npTemplatePath, len(src))
	}
	return src
}

func TestHelmChartShipsADefaultDenyEgressPolicyThatCannotRenderWithoutItsAllows(t *testing.T) {
	src := readNetworkPolicyTemplate(t)

	// The deny itself: a policy selecting every pod in the release with an EMPTY egress list.
	if !strings.Contains(src, `policyTypes: ["Egress"]`) {
		t.Error("no policy declares policyTypes Egress — a NetworkPolicy without it constrains ingress only")
	}
	if !strings.Contains(src, "egress: []") {
		t.Error("no DEFAULT-DENY rule (`egress: []`). Without it the allow policies below permit everything " +
			"they do not mention, because Kubernetes unions policies and nothing denies.")
	}

	// The deny must come FIRST and share the single enable gate with the allows. If the two could be
	// switched on independently, the reachable misconfiguration is "allows on, deny off" — which reads
	// like a tightened cluster and is in fact an unchanged one.
	if n := strings.Count(src, "if .Values.networkPolicy.enabled"); n != 1 {
		t.Errorf("the template has %d `networkPolicy.enabled` gates; it must have exactly ONE so the "+
			"default-deny and the allowlists can never be enabled separately", n)
	}
	denyAt := strings.Index(src, "egress: []")
	allowAt := strings.Index(src, "range $component, $rules := .Values.networkPolicy.egress")
	if allowAt < 0 {
		t.Fatal("the template never iterates networkPolicy.egress — the allowlist is not values-driven, " +
			"so an operator cannot declare a destination without editing the chart")
	}
	if denyAt > allowAt {
		t.Error("the default-deny is emitted AFTER the per-component allows; keep it first so the file " +
			"reads in the order the control works")
	}

	// A default-deny release that cannot resolve DNS fails at startup, not at the boundary, and the
	// operator's conclusion is "NetworkPolicy broke the stack" rather than "I have not declared my
	// destinations yet". The baseline rule is what makes the control adoptable.
	for _, want := range []string{"networkPolicy.dns.namespace", "networkPolicy.dns.podLabels", "protocol: UDP"} {
		if !strings.Contains(src, want) {
			t.Errorf("the baseline policy is missing %q — a default-deny release could not resolve even "+
				"its own service names", want)
		}
	}

	// Per-rule declarations are MANDATORY at render time, not by convention. `required` turns a rule with
	// no stated destination into a failed render instead of a policy nobody can review.
	for _, want := range []string{"$rule.why", "$rule.cidrs", "$rule.ports"} {
		if !strings.Contains(src, want) {
			t.Errorf("allow rules do not consume %q", want)
		}
	}
	if n := strings.Count(src, "required ("); n < 3 {
		t.Errorf("only %d `required` guards on allow-rule fields, want at least 3 (why/cidrs/ports). "+
			"An allow rule with no port is a rule to the whole host, which is not an allowlist.", n)
	}
	// The per-component selector is what stops the worker's estate reach from being handed to Postgres.
	if !strings.Contains(src, `"component" $component`) {
		t.Error("the allow policies do not select by component — one shared allowlist gives every pod in " +
			"the release the widest grant any one of them needed")
	}
}

func TestNetworkPolicyValuesContractIsSafeByDefaultAndDeclaresItsShape(t *testing.T) {
	b, err := os.ReadFile("helm/grounder/values.yaml")
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var vals map[string]any
	if err := yaml.Unmarshal(b, &vals); err != nil {
		t.Fatalf("parse values.yaml: %v", err)
	}
	np, ok := vals["networkPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("values.yaml has no `networkPolicy` block (%T) — the template's allowlist would have no "+
			"authoritative source, and this chart's whole contract is that every manifest draws from "+
			"values.yaml (spec/009 REQ-907)", vals["networkPolicy"])
	}

	// SAFE DEFAULT (house rule 5). A chart that turned default-deny egress on by default would take the
	// worker off the estate on the next `helm upgrade` of an existing release, before anyone had declared
	// a single destination.
	if enabled, _ := np["enabled"].(bool); enabled {
		t.Error("networkPolicy.enabled defaults to TRUE. Default-deny egress on an existing release with " +
			"empty allow lists severs the worker from the estate, litellm from every model provider, and " +
			"the grounder from OpenBao — an outage delivered by a security default nobody opted into.")
	}

	dns, ok := np["dns"].(map[string]any)
	if !ok || dns["namespace"] == nil || dns["port"] == nil || dns["podLabels"] == nil {
		t.Errorf("networkPolicy.dns is incomplete (%v) — without a DNS allow a default-deny release cannot "+
			"resolve its own services", np["dns"])
	}
	eg, ok := np["egress"].(map[string]any)
	if !ok {
		t.Fatalf("networkPolicy.egress is not a component→rules mapping (%T)", np["egress"])
	}
	// VACUITY FLOOR on the contract itself: the keys must name the workloads that actually reach off
	// cluster, or an operator gets a block that looks configurable and configures nothing.
	for _, want := range []string{"grounder", "worker", "litellm"} {
		if _, present := eg[want]; !present {
			t.Errorf("networkPolicy.egress has no %q key. That is one of the three components with a real "+
				"off-cluster dependency (see deploy/egress_parity_test.go for the compose equivalent), so "+
				"an operator has nowhere to declare its destinations.", want)
		}
	}
	// Shipped rules must be EMPTY. A CIDR baked into the chart is a destination somebody else's estate
	// never asked for, and it would be permitted the moment they enabled the policy.
	for comp, rules := range eg {
		if list, isList := rules.([]any); isList && len(list) > 0 {
			t.Errorf("networkPolicy.egress.%s ships %d rule(s). The chart must not bake in destinations: "+
				"they would be silently permitted in every estate that enables this policy.", comp, len(list))
		}
	}
}
