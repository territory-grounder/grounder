package attribution

import (
	"encoding/json"
	"strings"
	"testing"
)

// AN OPERATOR RULESET THAT OMITS A TAXONOMY MUST BE REJECTED, NOT QUIETLY OBEYED.
//
// ParseConfig only ever visited the rows PRESENT in the document, and DispositionFor sends an unmapped
// taxonomy to DispositionEscalate. So a config that simply left a taxonomy out silently converted it into a
// forced human poll — no error, no log line, and nothing in the diff a reviewer would read as a behaviour
// change. Omitting `authorized-test` alone reinstates the single largest autonomy drag on this estate: it is
// the biggest taxonomy live (88 of 200 consecutive sessions on 2026-07-29; 848 rows over 7 days when !687
// removed the equivalent hardcoded demotion).
//
// ★ WHY THE EXISTING COMPLETENESS ORACLE COULD NOT CATCH THIS. config_test.go asserts five-taxonomy coverage
// against DefaultConfigDocument() — the EMBEDDED default, which is complete by construction and always will
// be. It therefore tests that a correct constant is correct. The entire failure mode lives in the LOADED
// OVERRIDE path, which no test exercised. That is the same shape as the console defect found the same day:
// a test that builds its own input and its own expectation proves only that the code agrees with itself.
func TestParseConfigRejectsARulesetMissingATaxonomy(t *testing.T) {
	// Build a COMPLETE override from the closed enumeration, then remove one row at a time. Deriving the
	// document from AllTaxonomies() rather than hand-listing rows means a taxonomy added to the enum is
	// covered here automatically — a hand-written list would be maintained by the same person who forgot to
	// map it.
	full := func() []map[string]string {
		rows := []map[string]string{}
		for _, tax := range AllTaxonomies() {
			rows = append(rows, map[string]string{
				"id": "rule-" + tax.String(), "taxonomy": tax.String(), "disposition": "escalate-to-human",
			})
		}
		return rows
	}

	docOf := func(rows []map[string]string) []byte {
		b, err := json.Marshal(map[string]any{
			"actor_attribution": map[string]any{"mapping": rows},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	// SANITY FIRST: the complete document must parse. Without this, a ParseConfig that rejected EVERYTHING
	// would satisfy every assertion below — the vacuous-control failure mode.
	if _, _, err := ParseConfig(docOf(full())); err != nil {
		t.Fatalf("a COMPLETE override must parse, got: %v — the check must reject incomplete rulesets, not all of them", err)
	}

	for i, omitted := range AllTaxonomies() {
		rows := full()
		rows = append(rows[:i], rows[i+1:]...)

		_, _, err := ParseConfig(docOf(rows))
		if err == nil {
			t.Errorf("a ruleset omitting %q PARSED CLEANLY. An unmapped taxonomy escalates to a human, so "+
				"this config silently withdraws autonomy for every session of that kind with no error and no "+
				"log line", omitted.String())
			continue
		}
		// The error must NAME the omission. "invalid config" sends an operator to read the whole file; the
		// point of failing here rather than at runtime is that the cause is stated.
		if !strings.Contains(err.Error(), omitted.String()) {
			t.Errorf("rejecting a ruleset that omits %q, the error does not name it: %v", omitted.String(), err)
		}
	}
}

// The rejection must be reachable through the SAME entry point production uses, with the taxonomy names the
// operator actually writes in the file. A check that only fires for a Mapping built in Go would not protect
// the config path at all.
func TestARealisticPartialOverrideIsRejectedByName(t *testing.T) {
	// A plausible operator edit: they wanted to tighten `attributed-authorized` and rewrote the block,
	// dropping the rows they were not thinking about.
	doc := []byte(`{"actor_attribution":{"mapping":[
	  {"id":"tighten-authorized","taxonomy":"attributed-authorized","disposition":"escalate-to-human"},
	  {"id":"keep-self","taxonomy":"attributed-self","disposition":"self-noop"}
	]}}`)

	_, _, err := ParseConfig(doc)
	if err == nil {
		t.Fatal("a partial operator override parsed cleanly — unattributable, attributed-suspicious and " +
			"authorized-test are all unmapped and would each escalate silently")
	}
	for _, want := range []string{"unattributable", "attributed-suspicious", "authorized-test"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection does not name the missing taxonomy %q: %v", want, err)
		}
	}
	// It must NOT name the ones that were present, or the operator goes looking for a problem that is not there.
	for _, present := range []string{"attributed-authorized", "attributed-self"} {
		if strings.Contains(err.Error(), present) {
			t.Errorf("the rejection names %q, which the ruleset DID map: %v", present, err)
		}
	}
}

// The completeness list must not drift from the enum it claims to cover. AllTaxonomies() is the single list
// ParseConfig consults; if a taxonomy is added to the const block and not to it, an omitted mapping for the
// new value becomes silently-escalating again — the exact defect this file exists to prevent.
func TestAllTaxonomiesCoversEveryDistinctEnumValue(t *testing.T) {
	seen := map[string]bool{}
	for _, tax := range AllTaxonomies() {
		if seen[tax.String()] {
			t.Errorf("AllTaxonomies() lists %q twice", tax.String())
		}
		seen[tax.String()] = true
	}
	// Walk the enum by VALUE, independently of the list under test, until String() stops yielding new names.
	// Taxonomy's default case renders "unattributable", so an unlisted new value shows up as a value whose
	// name is already claimed by the zero value while not BEING the zero value.
	for v := Taxonomy(0); v <= AuthorizedTest+3; v++ {
		name := v.String()
		if v != Unattributable && name == "unattributable" && v <= AuthorizedTest {
			t.Errorf("taxonomy value %d renders as %q — it has no String() case, so it is invisible to any "+
				"name-based check including the completeness check in ParseConfig", int(v), name)
		}
		if v <= AuthorizedTest && !seen[name] {
			t.Errorf("taxonomy value %d (%q) is in the enum but NOT in AllTaxonomies() — a ruleset omitting "+
				"it would parse cleanly and then escalate every session of that kind", int(v), name)
		}
	}
}
