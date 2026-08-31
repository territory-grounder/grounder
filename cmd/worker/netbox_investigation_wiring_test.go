package main

import "testing"

// TG-56: the NetBox read-only investigation tool is an AGENT tool — it carries NetBox record TEXT into the
// model loop — so main.go reads its endpoint through the TG_NETBOX_URL_AGENT_TOOLS sentinel, and the
// ACTUATION plane must construct nothing. This pins the plane-scoping that wiring depends on: drop the
// sentinel from triagePlaneEnvKeys (re-exposing the tool to the mutation-key plane) or break the alias
// (the tool silently reads nothing and never registers) and this guard goes RED.
func TestNetBoxInvestigationToolIsPlaneScopedToTriage(t *testing.T) {
	const sentinel = "TG_NETBOX_URL_AGENT_TOOLS"

	onList := false
	for _, k := range triagePlaneEnvKeys {
		if k == sentinel {
			onList = true
		}
		// The PLAIN key must stay OFF the list: the CMDB/topology read (main.go ~1441) legitimately runs on
		// BOTH planes, and denying it blinds the actuation plane's blast-radius gate (the TG-346/TG-331 lesson).
		if k == "TG_NETBOX_URL" {
			t.Errorf("TG_NETBOX_URL (the plain key) is on triagePlaneEnvKeys — the actuation plane would lose "+
				"the NetBox CMDB/topology read the blast-radius gate reasons over. Only the %s sentinel belongs here.", sentinel)
		}
	}
	if !onList {
		t.Errorf("%s is NOT on triagePlaneEnvKeys, so the actuation worker would construct the read-only "+
			"NetBox investigation tool — an untrusted-content reader beside the estate mutation keys.", sentinel)
	}

	if got := planeEnvAlias[sentinel]; got != "TG_NETBOX_URL" {
		t.Errorf("planeEnvAlias[%q] = %q, want %q — the sentinel must resolve to the real endpoint key on the "+
			"triage plane, or the tool reads nothing and silently never registers.", sentinel, got, "TG_NETBOX_URL")
	}
}
