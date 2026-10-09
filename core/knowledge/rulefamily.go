package knowledge

import (
	_ "embed"
	"encoding/json"
	"sort"
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

// CanonicalRule is the exported family signature — the ONE rule-family authority (rulefamily.json), shared
// by every consumer that must decide "do these two rule strings denote the same condition". Exported for the
// deterministic verdict author (core/verify, spec/002 REQ-108): a predicted host firing a FAMILY SIBLING of a
// predicted rule is the predicted failure mode under another source's spelling, not an unpredicted one, and
// the verdict must key that judgment on THIS map — never a second family table that could drift from the
// novelty/recovery-belt matching (the recovery-belt lesson: two vocabularies that never intersect answered
// "not recovered" forever). Identical semantics to the internal signature: a known alias maps to its family
// name; an unmapped rule maps to its own trimmed lower-cased identity, so unmapped rules keep EXACT
// (case-insensitive) matching.
func CanonicalRule(rule string) string { return canonicalRule(rule) }

// RuleFamilyAliases returns every source rule string that denotes the SAME condition as `rule` — the rule
// itself plus its family siblings, or just the rule when it belongs to no family. Sorted and deduplicated, so
// callers that put it in a SQL `= ANY(...)` predicate get a deterministic parameter.
//
// WHY THIS IS EXPORTED (2026-07-30): the recovery belt (core/db.TransitionLogStore.RecoveredSince) matched
// alert_rule with string EQUALITY. That made a whole class of incident impossible to confirm as recovered:
// modules/ingest/pveliveness raises under TG's own label "Device-Down", while every captured recovery
// transition carries a LibreNMS name ("Devices-up/down", "Device-Down-SNMP-unreachable",
// "Device-Down-Due-to-no-ICMP-response."). The two vocabularies never intersect, so the belt answered "not
// recovered" forever and the poll parked until its vote window expired.
//
// Family membership — not a substring or prefix match — is what keeps this from re-opening the fail-OPEN the
// rule predicate exists to close. RecoveredSince's own doc records the cost of matching too loosely: without a
// rule scope the belt answered "did ANYTHING on this host recover", which counted a heal TG never achieved into
// the A3 numerator and de-novelled the (host, rule) pair. This map is deliberately narrow — only rules denoting
// the same condition AND warranting the same remediation — and explicitly EXCLUDES "TargetDown" (a scrape
// target down while the host is up) and "Device-rebooted" (a reboot, not a persistent down). So a sibling
// alias's recovery IS this incident's recovery; an unrelated rule's is still not.
func RuleFamilyAliases(rule string) []string {
	key := strings.ToLower(strings.TrimSpace(rule))
	if key == "" {
		return nil // an empty rule scopes to nothing — the caller must keep failing closed
	}
	fam, ok := ruleCanon[key]
	if !ok {
		return []string{strings.TrimSpace(rule)} // unmapped ⇒ EXACT matching, exactly as before
	}
	var doc ruleFamilyDoc
	if err := json.Unmarshal(ruleFamilyJSON, &doc); err != nil {
		return []string{strings.TrimSpace(rule)} // unreachable (init already validated) — degrade to exact
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 8)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, dup := seen[strings.ToLower(s)]; dup {
			return
		}
		seen[strings.ToLower(s)] = struct{}{}
		out = append(out, s)
	}
	add(rule) // the caller's own spelling always participates
	for family, aliases := range doc.Families {
		if strings.ToLower(strings.TrimSpace(family)) != fam {
			continue
		}
		for _, a := range aliases {
			add(a)
		}
	}
	sort.Strings(out)
	return out
}

// RuleFamilyPairs returns the alias -> family map as two parallel, alias-sorted slices, so a SQL caller
// can materialize the family table with `unnest($1::text[], $2::text[])` and fold rule names inside the
// query.
//
// WHY A SQL-SHAPED EXPORT EXISTS AT ALL. RuleFamilyAliases answers "what else denotes THIS rule" and fits
// a `= ANY(...)` predicate, which is enough when one side of the comparison is a Go value. It is not
// enough when BOTH sides are columns — core/db.AxisAgg's A6b time-to-recovery correlates a triage row's
// alert_rule against a transition row's alert_rule, and neither is known in Go. That query matched with
// string equality and therefore silently excluded the single commonest case in this estate: modules/
// ingest/pveliveness raises under TG's own label "Device-Down", while captured recoveries carry LibreNMS
// spellings ("Devices-up/down", "Device-Down-SNMP-unreachable", "Device-Down-Due-to-no-ICMP-response.").
// The two vocabularies never intersect, so those incidents were dropped from TG's only wall-clock MTTR
// number — not counted as slow, simply absent from the denominator, which makes the metric look better
// the more of this class occurs.
//
// This is the SAME authority (rulefamily.json) the novelty signature, the recovery belt and the verdict
// author use. Exporting a second shape of one map is deliberate; authoring a second MAP would recreate
// the drift the recovery-belt lesson was about.
//
// Keys are lower-cased and trimmed, matching canonicalRule, so a caller must fold its columns the same
// way (`lower(btrim(alert_rule))`) for the join to land.
func RuleFamilyPairs() (aliases, canons []string) {
	aliases = make([]string, 0, len(ruleCanon))
	for a := range ruleCanon {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases) // deterministic: Go randomizes map iteration, and a query plan should not
	canons = make([]string, 0, len(aliases))
	for _, a := range aliases {
		canons = append(canons, ruleCanon[a])
	}
	return aliases, canons
}
