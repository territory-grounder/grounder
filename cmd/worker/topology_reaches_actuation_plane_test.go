package main

// ESTATE TOPOLOGY MUST REACH THE ACTUATION PLANE, OR ITS GATES CANNOT REFUSE.
//
// credential_plane.go states this as a deliberate exception to the plane split, and states why:
//
//	"the actuation process still reads the estate TOPOLOGY (device inventory, NetBox/PVE object graph)
//	 because the interceptor's host-match and blast-radius gates are evaluated against it and a mutation
//	 gate reasoning over an empty graph is a gate that cannot refuse. Topology is a structured inventory
//	 read, not attacker-authored prose."
//
// The mechanism is the alias split: TG_LIBRENMS_DEPLOYMENTS is read at two sites for two purposes — the
// agent's investigation TOOLS (alert text, event-log lines: untrusted, withheld from the actuation plane)
// and the estate TOPOLOGY refresh (structured inventory the mutation gate needs). Only the first is
// plane-scoped, via the TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS sentinel.
//
// Adding the plain key to triagePlaneEnvKeys would read like tightening the split and would instead blind
// the mutation gate. Nothing else in the tree would fail: the worker boots, the pipelines pass, and the
// interceptor evaluates blast radius against a graph missing its LibreNMS topology.
//
// This is not hypothetical. Measured on dc1tg01 2026-08-05 — with the code correct and the OPENBAO
// POLICY missing the matching grant:
//
//	estate: source librenms failed to seed: librenms[dc1]: resolve token: 403
//	        (its edges are absent, not silently assumed true)
//
// The code had split the key by purpose; tg-actuate-ro had not, so the actuation worker was handed the
// configuration and denied the credential. The policy now grants secret/data/tg/librenms-{nl,gr}. This
// test pins the CODE half of that agreement; the policy half lives in OpenBao and is recorded in TG-331.

import (
	"os"
	"strings"
	"testing"
)

func TestTopologyKeyIsNotWithheldFromTheActuationPlane(t *testing.T) {
	if len(triagePlaneEnvKeys) == 0 {
		t.Fatal("vacuity floor: triagePlaneEnvKeys is empty, so this guard compared nothing and the " +
			"plane split is withholding nothing from anyone")
	}

	const topologyKey = "TG_LIBRENMS_DEPLOYMENTS"
	const toolsAlias = "TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS"

	var scoped, aliasScoped bool
	for _, k := range triagePlaneEnvKeys {
		if k == topologyKey {
			scoped = true
		}
		if k == toolsAlias {
			aliasScoped = true
		}
	}

	if scoped {
		t.Errorf("%s is on triagePlaneEnvKeys, so the actuation plane is denied estate TOPOLOGY.\n"+
			"This reads like tightening the credential split and instead blinds the mutation gate: the "+
			"interceptor's host-match and blast-radius checks are evaluated against that graph, and a gate "+
			"reasoning over an empty graph cannot refuse anything. Only the %s alias belongs on this list "+
			"— that site reads alert TEXT, which is the untrusted half.", topologyKey, toolsAlias)
	}
	if !aliasScoped {
		t.Errorf("%s is NOT on triagePlaneEnvKeys, so the actuation plane would register the agent's "+
			"LibreNMS investigation tools — attacker-authored alert text reaching the process that holds "+
			"the key to mutate the estate. That is the direction the split exists to close.", toolsAlias)
	}

	// The alias must resolve to the real key, or the tools site reads nothing at all and the split is
	// enforced by accident rather than by design.
	if got := planeEnvAlias[toolsAlias]; got != topologyKey {
		t.Errorf("planeEnvAlias[%q] = %q, want %q — the sentinel must map back to the real environment "+
			"key, otherwise the agent tools silently see no deployments on EVERY plane", toolsAlias, got, topologyKey)
	}
}

// The comment carrying this rationale is load-bearing: it is the only place the exception is justified, and
// a future reader tightening the split will look here first. If it goes, the next person deletes the
// exception as an oversight.
func TestTheTopologyExceptionKeepsItsStatedRationale(t *testing.T) {
	src := credentialPlaneSource(t)
	for _, phrase := range []string{"estate TOPOLOGY", "cannot refuse"} {
		if !strings.Contains(src, phrase) {
			t.Errorf("credential_plane.go no longer explains the topology exception (missing %q). The "+
				"exception is deliberate and counter-intuitive; without the reason written down beside it, "+
				"the next person to tighten the plane split removes it and blinds the mutation gate.", phrase)
		}
	}
}

// credentialPlaneSource reads the file the rule lives in. Split out so a rename fails loudly here rather
// than turning the guard above into a check against an empty string.
func credentialPlaneSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("credential_plane.go")
	if err != nil {
		t.Fatalf("read credential_plane.go: %v — this guard cannot assert anything about a file it "+
			"cannot open, and must fail rather than pass vacuously", err)
	}
	if len(raw) == 0 {
		t.Fatal("credential_plane.go is empty")
	}
	return string(raw)
}
