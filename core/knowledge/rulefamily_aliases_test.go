package knowledge

import (
	"testing"

	"github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

// THE RECOVERY BELT COULD NOT CONFIRM A WHOLE CLASS OF INCIDENT, BECAUSE TWO VOCABULARIES NEVER INTERSECTED.
//
// core/db.TransitionLogStore.RecoveredSince matched alert_rule with string EQUALITY. pveliveness raises under
// TG's OWN label "Device-Down"; every captured recovery transition carries a LibreNMS name. Measured live
// 2026-07-30: 429 "Devices-up/down", 429 "Device-Down-Due-to-no-ICMP-response.", 428
// "Device-Down-SNMP-unreachable" recovery rows — and ZERO under "Device-Down", against 374 sessions raised
// with it. So the belt answered "not recovered" forever, the poll's `obsoleted` branch never fired for a
// liveness-sourced incident, and 16 decisions sat open whose target guests were ALL verified already running.
//
// RuleFamilyAliases is what closes that gap, and the ONLY thing standing between it and the fail-OPEN the rule
// predicate exists to prevent is family NARROWNESS. So this oracle pins both directions: siblings expand, and
// the deliberately-excluded rules do not.
func TestRuleFamilyAliasesExpandSiblingsAndNothingElse(t *testing.T) {
	// 1. TG's own label expands to the LibreNMS spellings the recovery log actually carries. Without this the
	//    belt has nothing to match and the poll can never self-close.
	got := RuleFamilyAliases(pveliveness.DeviceDownRule)
	if len(got) < 2 {
		t.Fatalf("RuleFamilyAliases(%q) = %v — a family member must expand to its siblings, or the recovery "+
			"belt still has no LibreNMS spelling to match", pveliveness.DeviceDownRule, got)
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	for _, need := range []string{
		"Devices-up/down",
		"Device-Down-SNMP-unreachable",
		"Device-Down-Due-to-no-ICMP-response.",
	} {
		if !has(got, need) {
			t.Errorf("expansion of %q is missing %q — that is a spelling the live ingest_transition log "+
				"actually carries, so omitting it leaves the incident unconfirmable: got %v",
				pveliveness.DeviceDownRule, need, got)
		}
	}
	// The caller's own spelling must always participate, or an exact-match recovery would stop matching.
	if !has(got, pveliveness.DeviceDownRule) {
		t.Errorf("expansion dropped the caller's own rule %q: %v", pveliveness.DeviceDownRule, got)
	}

	// 2. THE NARROWNESS. These denote DIFFERENT conditions and must never be pulled in — an unrelated rule
	//    confirming a recovery is the fail-OPEN that counted a heal TG never achieved into the A3 numerator.
	for _, excluded := range []string{"TargetDown", "Device-rebooted"} {
		if has(got, excluded) {
			t.Errorf("expansion of %q wrongly includes %q — that rule denotes a different condition and its "+
				"recovery must NOT confirm a host-down incident", pveliveness.DeviceDownRule, excluded)
		}
		// And the reverse direction: expanding the excluded rule must not reach the device-down family.
		for _, sib := range RuleFamilyAliases(excluded) {
			if sib == "Devices-up/down" {
				t.Errorf("RuleFamilyAliases(%q) reached into the device-down family: %v", excluded, RuleFamilyAliases(excluded))
			}
		}
	}

	// 3. AN UNMAPPED RULE KEEPS EXACT MATCHING — byte for byte as before, so this change cannot widen anything
	//    outside a declared family.
	if solo := RuleFamilyAliases("Some-Unmapped-Rule"); len(solo) != 1 || solo[0] != "Some-Unmapped-Rule" {
		t.Errorf("RuleFamilyAliases on an unmapped rule = %v, want exactly [Some-Unmapped-Rule] — an unmapped "+
			"rule must not gain siblings", solo)
	}

	// 4. AN EMPTY RULE EXPANDS TO NOTHING, so the caller's fail-closed guard cannot degrade into a wildcard.
	//    A non-empty expansion here would make `alert_rule = ANY(...)` match every row on the host.
	if e := RuleFamilyAliases("   "); len(e) != 0 {
		t.Errorf("RuleFamilyAliases on a blank rule = %v, want empty — a blank rule must never expand into a "+
			"predicate that matches anything", e)
	}

	// 5. DETERMINISM. The expansion lands in a SQL parameter; map iteration order must not leak into it.
	first := RuleFamilyAliases("Devices-up/down")
	for i := 0; i < 8; i++ {
		again := RuleFamilyAliases("Devices-up/down")
		if len(again) != len(first) {
			t.Fatalf("expansion length varies across calls: %v vs %v", first, again)
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("expansion order varies across calls: %v vs %v", first, again)
			}
		}
	}

	// 6. EVERY SIBLING EXPANDS TO THE SAME SET, so which alias the incident happened to be raised under cannot
	//    change whether its recovery is findable.
	base := RuleFamilyAliases("Devices-up/down")
	for _, alias := range []string{"Device-Down-SNMP-unreachable", "HostDown", pveliveness.DeviceDownRule} {
		other := RuleFamilyAliases(alias)
		if len(other) != len(base) {
			t.Errorf("RuleFamilyAliases(%q) has %d entries but RuleFamilyAliases(\"Devices-up/down\") has %d — "+
				"the set must not depend on which alias the incident was raised under: %v vs %v",
				alias, len(other), len(base), other, base)
		}
	}
}
