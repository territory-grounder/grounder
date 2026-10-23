package policy

// spec/029 T-029-3 — the inverse_only match dimension: the grammar that lets the COMPENSATING
// direction of an op-class earn autonomy without widening the model-proposed path of the same
// class. The claim, executed: the default ruleset's stop-guest rule matches ONLY a declared
// inverse (EvalInput.InvertsForward, threaded from actuate.Request.InvertsActionID), and a
// model-proposed stop-guest falls through to the default `approve`.
//
// KILLING MUTATION (executed 2026-08-14): in Match.matches, invert the arm
// (`*m.InverseOnly != in.InvertsForward` → `==`) — the rule then authorizes exactly the wrong
// direction: model-proposed stop-guest reads `auto` and the fired inverse reads `approve`. Both
// halves of TestStopGuestRuleMatchesOnlyTheDeclaredInverse go red. Restored, green.

import (
	"encoding/json"
	"testing"
)

func t0293EvalInput(inverse bool) EvalInput {
	return EvalInput{
		OpClass:        "stop-guest",
		Host:           "dc1librespeed01",
		Reversible:     true,
		Confidence:     0.9,
		InvertsForward: inverse,
	}
}

func TestStopGuestRuleMatchesOnlyTheDeclaredInverse(t *testing.T) {
	var ruleset struct {
		Rules []ruleDoc `json:"rules"`
	}
	if err := json.Unmarshal(DefaultRuleSetDocument(), &ruleset); err != nil {
		t.Fatalf("parse default ruleset: %v", err)
	}
	var rule Rule
	found := false
	for _, rd := range ruleset.Rules {
		if rd.Match.OpClass == "stop-guest" {
			r, err := ruleFromDoc(rd)
			if err != nil {
				t.Fatalf("build stop-guest rule: %v", err)
			}
			rule, found = r, true
		}
	}
	if !found {
		t.Fatal("the default ruleset must carry the stop-guest inverse rule (the completeness guard demands a rule; this drill demands its SCOPE)")
	}
	if rule.Match.InverseOnly == nil || !*rule.Match.InverseOnly {
		t.Fatalf("the stop-guest rule must be inverse_only:true — an unscoped auto rule widens the model-proposed path, got %+v", rule.Match.InverseOnly)
	}
	if !rule.Match.matches(t0293EvalInput(true)) {
		t.Fatal("the DECLARED INVERSE must match the rule (the commit-confirmed fire path's autonomy)")
	}
	if rule.Match.matches(t0293EvalInput(false)) {
		t.Fatal("a MODEL-PROPOSED stop-guest must NOT match — it falls to the default `approve` (poll)")
	}
}

// The dimension itself, both polarities, independent of the shipped ruleset.
func TestInverseOnlyDimensionMatchesBothPolarities(t *testing.T) {
	tr, fa := true, false
	inverseOnly := Match{OpClass: "x", InverseOnly: &tr}
	forwardOnly := Match{OpClass: "x", InverseOnly: &fa}
	in := EvalInput{OpClass: "x"}

	in.InvertsForward = true
	if !inverseOnly.matches(in) || forwardOnly.matches(in) {
		t.Fatal("inverse input: inverse_only=true must match, =false must not")
	}
	in.InvertsForward = false
	if inverseOnly.matches(in) || !forwardOnly.matches(in) {
		t.Fatal("forward input: inverse_only=false must match, =true must not")
	}
}

// TG-497: inverse_only is a real dimension for the validity predicate too — a rule constraining ONLY it
// (a global inverse brake: "every revert polls") must parse, not be refused as dimension-less. Killing
// mutation EXECUTED 2026-08-15: the `m.InverseOnly != nil` term dropped from specifiesAny() → this went
// red with "match specifies no dimension"; restored, green.
func TestInverseOnlyAloneIsADimension(t *testing.T) {
	doc := []byte(`{"rules":[{"id":"inverse-brake","verdict":"approve","match":{"inverse_only":true}}]}`)
	rs, err := ParseRuleSet(doc)
	if err != nil {
		t.Fatalf("a rule constraining only inverse_only must be valid: %v", err)
	}
	if len(rs.Rules) != 1 || rs.Rules[0].Match.InverseOnly == nil || !*rs.Rules[0].Match.InverseOnly {
		t.Fatalf("the dimension must survive the parse: %+v", rs.Rules)
	}
}
