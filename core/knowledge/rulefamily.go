package knowledge

import (
	_ "embed"
	"encoding/json"
	"strings"
)

// rulefamily.json is the LOADABLE alert-rule family map (INV-02: git-reviewed data, never a Go literal). The
// SAME physical fault surfaces under several source rule names — a WHOLE-HOST-DOWN event reaches TG as LibreNMS
// "Device-Down-SNMP-unreachable", "Device-Down-Due-to-no-ICMP-response.", or "Devices-up/down", and as
// Prometheus blackbox "HostDown" — and the novelty gate keys on the rule, so without canonicalization a
// confirmed-clean resolution under ONE alias de-novels only ~1/N of the fault's occurrences and the rest keep
// POLL_PAUSE-ing. Collapsing aliases into one family so a de-novel under any alias covers them all.
//
// MEMBERSHIP IS DELIBERATELY NARROW: only rules denoting the SAME condition (the whole host/device is
// unreachable) AND warranting the same remediation. Excluded on purpose: Prometheus "TargetDown" (a single
// SCRAPE TARGET / exporter down while the host is UP — a different fault; grouping it would suppress the
// first-sight human poll for a genuine host-down and vice-versa) and LibreNMS "Device-rebooted" (a reboot /
// uptime-reset, not a persistent down). Changing this map changes ONLY the novelty MATCH — never what
// actuates, never the mode/graduation/floor/ACL gates.
//
//go:embed rulefamily.json
var ruleFamilyJSON []byte

type ruleFamilyDoc struct {
	Families map[string][]string `json:"families"`
}

// ruleCanon maps a lower-cased/trimmed alias → its canonical family name, built once from the embedded map.
var ruleCanon = mustBuildRuleCanon()

func mustBuildRuleCanon() map[string]string {
	var doc ruleFamilyDoc
	if err := json.Unmarshal(ruleFamilyJSON, &doc); err != nil {
		panic("knowledge: malformed rulefamily.json: " + err.Error())
	}
	m := make(map[string]string)
	for family, aliases := range doc.Families {
		fam := strings.ToLower(strings.TrimSpace(family))
		if fam == "" {
			continue
		}
		for _, a := range aliases {
			key := strings.ToLower(strings.TrimSpace(a))
			if key == "" {
				continue
			}
			// Reject a duplicate alias across families: it would make canonicalRule non-deterministic across
			// process starts (Go randomizes map iteration). Caught at package-init in the build/test gate.
			if prev, dup := m[key]; dup && prev != fam {
				panic("knowledge: rulefamily.json alias " + key + " listed under two families (" + prev + ", " + fam + ")")
			}
			m[key] = fam
		}
	}
	return m
}

// canonicalRule returns the NOVELTY signature for an alert rule: the family name if the rule is a known alias,
// else the trimmed/lower-cased rule itself (identity). Two aliases of one family therefore share a signature,
// so a de-novel under any alias de-novels the fault regardless of which source rule name fired; a rule that is
// in NO family keeps EXACT (case-insensitive) novelty matching, exactly as before. Pure and deterministic.
func canonicalRule(rule string) string {
	key := strings.ToLower(strings.TrimSpace(rule))
	if fam, ok := ruleCanon[key]; ok {
		return fam
	}
	return key
}
