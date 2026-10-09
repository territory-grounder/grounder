// Package plan defines the transaction-plan recipe registry (spec/030, TG-58): the ONLY producer of
// multi-step repairs. A recipe is compiled, operator-authored DATA over closed vocabularies — the
// opschema/catalog/pack discipline — validated once at build. The model never shapes a plan: the
// agent's single proposed action can at most SELECT a recipe by op-class (a pure lookup, REQ-3001),
// and every step's params derive only from the recipe's own fixed literals or a declared renaming of
// the trigger action's already-screened params.
//
// Governance is owner-ruled (2026-08-22): ONE approval for the whole plan, presented whole; any step
// failing auto-compensates the completed steps (REQ-3002/3004). This package owns the recipe shape,
// its build-time validation, and the content-addressed plan identity the one vote binds; the workflow,
// poll and ledger halves live with their own seams (tasks T-030-2..5).
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
)

// maxRecipeSteps bounds a recipe (compiled, deliberately small): a plan is a short governed sequence a
// human can adjudicate in one poll, not a playbook. Raising it is a spec amendment, not a tweak.
const maxRecipeSteps = 5

// PlanStep is one step of a recipe. Params come from exactly two closed sources, both authored in the
// recipe: fixed literals, and declared RENAMINGS of the trigger action's own screened params — never a
// model-provided name or value (INV-08).
type PlanStep struct {
	// OpClass is the registered op-class slug this step executes (opschema.Lookup must resolve it).
	OpClass string
	// ParamsFixed are operator-authored literal params for this step.
	ParamsFixed map[string]string
	// ParamsFrom maps this step's param name → the TRIGGER action's param name whose screened value it
	// takes (e.g. "node" ← "host").
	ParamsFrom map[string]string
}

// PlanRecipe is one declared multi-step repair.
type PlanRecipe struct {
	// Name is the stable recipe identifier (ledger/audit key).
	Name string
	// TriggerOpClass selects this recipe: a session whose single proposed action carries this op-class
	// is OFFERED the plan (offered — the one human vote still decides).
	TriggerOpClass string
	// Steps execute in order; on a failure at step N, steps N-1..1 compensate in reverse (REQ-3004).
	Steps []PlanStep
}

var recipeNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)

// Validate refuses a malformed recipe at BUILD time — the modules/catalog discipline: a bad recipe
// fails the test suite once rather than composing a wrong plan forever. The compensatability rule
// (REQ-3004) is the load-bearing one: every step must be safely compensatable per the ONE static
// authority (opschema.SafelyCompensatable), because a plan whose step cannot be undone cannot keep the
// owner-ruled all-or-nothing promise.
func (r PlanRecipe) Validate() error {
	if !recipeNameRE.MatchString(r.Name) {
		return fmt.Errorf("plan: recipe name %q must be a lowercase slug", r.Name)
	}
	trig, ok := opschema.Lookup(r.TriggerOpClass)
	if !ok {
		return fmt.Errorf("plan %s: trigger op-class %q is not in the registry", r.Name, r.TriggerOpClass)
	}
	if len(r.Steps) < 2 {
		return fmt.Errorf("plan %s: %d step(s) — a plan below two steps is a single action wearing a costume; propose it plainly", r.Name, len(r.Steps))
	}
	if len(r.Steps) > maxRecipeSteps {
		return fmt.Errorf("plan %s: %d steps exceeds the ceiling of %d — a plan is a short governed sequence a human adjudicates in one poll", r.Name, len(r.Steps), maxRecipeSteps)
	}
	trigParams := map[string]bool{}
	for _, p := range trig.Params {
		trigParams[p.Name] = true
	}
	for i, st := range r.Steps {
		spec, ok := opschema.Lookup(st.OpClass)
		if !ok {
			return fmt.Errorf("plan %s step %d: op-class %q is not in the registry", r.Name, i+1, st.OpClass)
		}
		if !spec.SafelyCompensatable() {
			return fmt.Errorf("plan %s step %d: op-class %q is not safely compensatable (tier %q, op %q, rollback_template declared=%v) — an uncompensatable step cannot join an all-or-nothing plan (REQ-3004)",
				r.Name, i+1, st.OpClass, spec.SafetyTier, spec.Op, len(spec.RollbackTemplate) > 0)
		}
		stepParams := map[string]bool{}
		for _, p := range spec.Params {
			stepParams[p.Name] = true
		}
		for k := range st.ParamsFixed {
			if !stepParams[k] {
				return fmt.Errorf("plan %s step %d: fixed param %q is not a declared param of op-class %q", r.Name, i+1, k, st.OpClass)
			}
		}
		for k, from := range st.ParamsFrom {
			if !stepParams[k] {
				return fmt.Errorf("plan %s step %d: mapped param %q is not a declared param of op-class %q", r.Name, i+1, k, st.OpClass)
			}
			if !trigParams[from] {
				return fmt.Errorf("plan %s step %d: param %q maps from %q, which the trigger op-class %q does not declare — a renaming can only carry the trigger's own screened values", r.Name, i+1, k, from, r.TriggerOpClass)
			}
			if _, both := st.ParamsFixed[k]; both {
				return fmt.Errorf("plan %s step %d: param %q is both fixed and mapped — one source per param", r.Name, i+1, k)
			}
		}
		// Every REQUIRED step param must have a source now, at build — a plan that would fail param
		// rendering at execute time is a plan that half-runs, the exact failure this lane exists to end.
		for _, p := range spec.Params {
			if p.Required && !hasKey(st.ParamsFixed, p.Name) && !hasKey(st.ParamsFrom, p.Name) {
				return fmt.Errorf("plan %s step %d: required param %q of op-class %q has no source (neither fixed nor mapped)", r.Name, i+1, p.Name, st.OpClass)
			}
		}
	}
	return nil
}

func hasKey(m map[string]string, k string) bool { _, ok := m[k]; return ok }

// StepParams renders one step's concrete params from the trigger action's screened params — fixed
// literals first, then the declared renamings. Pure; refuses (rather than blanks) a mapped trigger
// param that is absent/blank, because a blank required value belongs to build-time refusal or an honest
// error, never a silently empty argv slot.
func (st PlanStep) StepParams(triggerParams map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(st.ParamsFixed)+len(st.ParamsFrom))
	maps.Copy(out, st.ParamsFixed)
	for k, from := range st.ParamsFrom {
		v, ok := triggerParams[from]
		if !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("plan step %s: mapped param %q needs trigger param %q, which is absent/blank", st.OpClass, k, from)
		}
		out[k] = v
	}
	return out, nil
}

// PlanID is the content-addressed identity the ONE approval binds (REQ-3002 — the INV-07 argument one
// level up): a hash over the recipe name and every step's (op-class, rendered params) tuple IN ORDER.
// Reordering steps, changing a param value, or swapping an op-class all change the id, so the thing
// approved is provably the thing executed. Params render against the trigger's screened params first —
// an id over unrendered mappings would let two different concrete plans share an identity.
func PlanID(r PlanRecipe, triggerParams map[string]string) (string, error) {
	var b strings.Builder
	b.WriteString("tg-transaction-plan/1\n")
	b.WriteString(r.Name)
	b.WriteString("\n")
	for _, st := range r.Steps {
		params, err := st.StepParams(triggerParams)
		if err != nil {
			return "", err
		}
		keys := make([]string, 0, len(params))
		for k := range params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString(st.OpClass)
		for _, k := range keys {
			b.WriteString("\x1f")
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(params[k])
		}
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}
