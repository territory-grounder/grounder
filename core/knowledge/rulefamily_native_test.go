package knowledge

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

// A TG-NATIVE DETECTOR'S RULE LABEL MUST SHARE THE FAMILY OF THE PUSH-SOURCED ALIASES IT STANDS IN FOR.
//
// pveliveness mints alert_rule "Device-Down" for a guest-down edge, and its own doc states the intent: the
// label "intentionally matches the LibreNMS device-down precedent so the Runner classifies a liveness-sourced
// incident IDENTICALLY to a push-sourced one (no skill/prompt drift between the two intakes)."
//
// That intent is real, but until this oracle it was held together by a COINCIDENCE rather than a declaration.
// "Device-Down" was NOT an alias in rulefamily.json. canonicalRule lower-cases its input and falls back to
// IDENTITY for an unmapped rule, and the family happens to be NAMED "device-down" — so
// canonicalRule("Device-Down") returned the identity string "device-down", which equals the family name the
// LibreNMS aliases map to. Equal by accident. Rename the family, or change the constant's spacing or
// hyphenation, and the two intakes silently stop sharing a novelty signature: a confirmed-clean resolution
// under a LibreNMS alias would stop de-novelling the liveness-sourced occurrences of the SAME fault, and every
// guest-down this poller catches would keep re-opening a first-sight human poll.
//
// Asserting against the PRODUCTION constant (never a copied literal) is the same discipline
// core/httpapi/record_from_envelope_test.go already applies to this string — a copied literal would test the
// test rather than the wiring.
func TestTGNativeGuestDownSharesTheLibreNMSDeviceDownFamily(t *testing.T) {
	native := canonicalRule(pveliveness.DeviceDownRule)

	// Every push-sourced alias that denotes the same whole-host-down condition must agree with it.
	for _, alias := range []string{
		"Devices-up/down",
		"Device-Down-SNMP-unreachable",
		"Device-Down-Due-to-no-ICMP-response.",
		"HostDown",
	} {
		if got := canonicalRule(alias); got != native {
			t.Errorf("canonicalRule(%q) = %q but canonicalRule(pveliveness.DeviceDownRule) = %q — the "+
				"TG-native detector's label does not share the push-sourced family, so a de-novel under one "+
				"intake does not cover the other and every liveness-caught guest-down re-opens a first-sight poll",
				alias, got, native)
		}
	}

	// AND the agreement must come from a DECLARED alias, not from the identity fallback coinciding with the
	// family name. If the constant is not a known alias, canonicalRule returns identity — which is exactly the
	// fragile state this oracle exists to end.
	if _, declared := ruleCanon[strings.ToLower(strings.TrimSpace(pveliveness.DeviceDownRule))]; !declared {
		t.Errorf("pveliveness.DeviceDownRule (%q) is not a DECLARED alias in rulefamily.json — it currently "+
			"canonicalises to %q only because canonicalRule falls back to identity and the family happens to "+
			"carry that same name. Renaming the family would silently split the two intakes with no test failing.",
			pveliveness.DeviceDownRule, native)
	}

	// A rule denoting a DIFFERENT condition must still NOT join the family — membership is deliberately narrow
	// (TargetDown is a scrape target down while the host is UP; Device-rebooted is a reboot, not a persistent
	// down). Widening it would suppress the first-sight poll for a genuine host-down and vice-versa.
	for _, outside := range []string{"TargetDown", "Device-rebooted"} {
		if canonicalRule(outside) == native {
			t.Errorf("canonicalRule(%q) joined the device-down family — membership must stay narrow to rules "+
				"denoting the SAME condition AND warranting the same remediation", outside)
		}
	}
}
