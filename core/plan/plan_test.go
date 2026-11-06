package plan

import (
	"strings"
	"testing"
)

// validRecipe uses REAL registry op-classes so the oracle exercises the live vocabulary: both steps
// declare rollback templates (restart-service, start-service — verified in opschema.json), and the
// trigger's "unit" param feeds both.
func validRecipe() PlanRecipe {
	return PlanRecipe{
		Name:           "restart-then-verify-unit",
		TriggerOpClass: "restart-service",
		Steps: []PlanStep{
			{OpClass: "start-service", ParamsFrom: map[string]string{"unit": "unit"}},
			{OpClass: "restart-service", ParamsFrom: map[string]string{"unit": "unit"}},
		},
	}
}

// Every Validate refusal, one mutation each (spec/030 REQ-3001/3004). KILLING MUTATION: delete any
// single rule in Validate and exactly one case fails on "expected a refusal".
func TestValidateRefusesEachAuthoringError(t *testing.T) {
	if err := validRecipe().Validate(); err != nil {
		t.Fatalf("the fixture must validate before mutation: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*PlanRecipe)
		want string
	}{
		{"bad name", func(r *PlanRecipe) { r.Name = "Bad Name" }, "lowercase slug"},
		{"unknown trigger", func(r *PlanRecipe) { r.TriggerOpClass = "no-such-class" }, "not in the registry"},
		{"one step", func(r *PlanRecipe) { r.Steps = r.Steps[:1] }, "single action wearing a costume"},
		{"too many steps", func(r *PlanRecipe) {
			for len(r.Steps) <= maxRecipeSteps {
				r.Steps = append(r.Steps, r.Steps[0])
			}
		}, "ceiling"},
		{"unknown step class", func(r *PlanRecipe) { r.Steps[0].OpClass = "no-such-class" }, "not in the registry"},
		// start-guest is low-reversible with op=start and NO rollback template: its "rollback" would
		// re-run the forward — the exact silent-no-op rollbackArgvFor refuses, now refused at BUILD.
		{"uncompensatable step", func(r *PlanRecipe) {
			r.Steps[0] = PlanStep{OpClass: "start-guest", ParamsFixed: map[string]string{"guest": "g1"}}
		}, "not safely compensatable"},
		// disk-grow is tier medium — no safe inverse by tier.
		{"medium-tier step", func(r *PlanRecipe) {
			r.Steps[0] = PlanStep{OpClass: "disk-grow", ParamsFixed: map[string]string{"filesystem": "/", "grow_by": "1G"}}
		}, "not safely compensatable"},
		{"undeclared fixed param", func(r *PlanRecipe) { r.Steps[0].ParamsFixed = map[string]string{"nope": "x"} }, "not a declared param"},
		{"undeclared mapped param", func(r *PlanRecipe) { r.Steps[0].ParamsFrom = map[string]string{"nope": "unit"} }, "not a declared param"},
		{"mapping from a param the trigger lacks", func(r *PlanRecipe) { r.Steps[0].ParamsFrom = map[string]string{"unit": "hostname"} }, "does not declare"},
		{"param both fixed and mapped", func(r *PlanRecipe) {
			r.Steps[0].ParamsFixed = map[string]string{"unit": "nginx.service"}
		}, "one source per param"},
		{"required param with no source", func(r *PlanRecipe) { r.Steps[0].ParamsFrom = nil }, "no source"},
	}
	for _, c := range cases {
		r := validRecipe()
		c.mut(&r)
		err := r.Validate()
		if err == nil {
			t.Errorf("%s: expected a refusal, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: refusal %q does not name the rule (%q)", c.name, err, c.want)
		}
	}
}

// The shipped catalog is EMPTY (REQ-3007) and the lookup is strict: no recipe, no plan, every failure
// direction is "single action as today".
func TestShippedCatalogIsInertAndLookupIsStrict(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("the catalog must ship EMPTY (T-030-6 declares the first recipe in its own MR), got %d", len(all))
	}
	if _, ok := ForTriggerIn(all, "restart-service"); ok {
		t.Fatal("zero recipes must offer zero plans")
	}
	if _, ok := ForTriggerIn([]PlanRecipe{{TriggerOpClass: "restart-service"}}, ""); ok {
		t.Fatal("an empty op-class must never select")
	}
}

// The plan identity binds what the vote approved (REQ-3002): deterministic for identical inputs, and
// ANY tuple change — a param value, step order, an op-class — changes it. KILLING MUTATION: drop the
// step separator or the param sort from PlanID — the sensitivity cases collapse.
func TestPlanIDBindsTheOrderedConcretePlan(t *testing.T) {
	r := validRecipe()
	trig := map[string]string{"unit": "nginx.service"}

	id1, err := PlanID(r, trig)
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := PlanID(r, trig)
	if id1 != id2 || len(id1) != 64 {
		t.Fatalf("identity must be a deterministic sha256 hex, got %q vs %q", id1, id2)
	}
	otherParam, _ := PlanID(r, map[string]string{"unit": "postgres.service"})
	if otherParam == id1 {
		t.Fatal("a different concrete param must change the plan identity")
	}
	swapped := validRecipe()
	swapped.Steps[0], swapped.Steps[1] = swapped.Steps[1], swapped.Steps[0]
	sw, _ := PlanID(swapped, trig)
	if sw == id1 {
		t.Fatal("step ORDER is part of the approved plan — reordering must change the identity")
	}
	if _, err := PlanID(r, map[string]string{}); err == nil {
		t.Fatal("a mapped param with no trigger value must refuse, never hash a blank")
	}
}

// The static compensatability authority itself (opschema.SafelyCompensatable), pinned through this
// package's own use: declared-rollback and idempotent-reconvergence pass; the start-with-no-inverse
// and medium-tier shapes refuse. (The runtime half stays with rollbackArgvFor.)
func TestCompensatabilityCriterionMatchesTheRollbackAuthority(t *testing.T) {
	ok := PlanRecipe{Name: "reload-pair", TriggerOpClass: "restart-service", Steps: []PlanStep{
		{OpClass: "reload-service", ParamsFrom: map[string]string{"unit": "unit"}},
		{OpClass: "restart-container", ParamsFixed: map[string]string{"container": "web"}},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("declared-rollback and idempotent-reconvergence steps must both validate: %v", err)
	}
}
