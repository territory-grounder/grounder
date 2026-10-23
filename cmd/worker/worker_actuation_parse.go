package main

// Operator actuation-config PARSERS, carved out of main()'s composition root (TG-501 LOC-debt paydown).
// They turn config-not-code JSON env vars (TG_REGIME_RULES, TG_AWXJOB_ALLOWLIST) into typed regime rules and
// the AWX template allowlist, plus the shared kind:pattern selector grammar and the op-class->template resolver
// the temporal runner keys off. Every parser FAILS CLOSED — a malformed/unknown/ambiguous entry yields an error
// (or ok=false) so no unauthorized lane, selector, or launch can be encoded; there is no command string anywhere
// (a template is not a shell). Behaviour is unchanged by the move; regime_wiring_test pins these directly.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/modules/actuation/awxjob"
	"github.com/territory-grounder/grounder/modules/actuation/gitopsmr"
)

// regimeRuleSpec is one operator-declared regime rule as config-not-code JSON (TG_REGIME_RULES). The selector
// is a "kind:pattern" token over the SHARED estate object-model, the same grammar policy + credential key off.
type regimeRuleSpec struct {
	ID       string `json:"id"`
	Selector string `json:"selector"`
	Regime   string `json:"regime"`
}

// parseRegimeRules parses TG_REGIME_RULES (a JSON array of {id,selector,regime}) into regime rules. Empty ⇒
// no rules (every target then takes the operator default lane). A malformed rule / unknown regime / unknown
// selector kind fails closed.
func parseRegimeRules(spec string) ([]regime.Rule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var raw []regimeRuleSpec
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("TG_REGIME_RULES is not a JSON array of {id,selector,regime}: %w", err)
	}
	out := make([]regime.Rule, 0, len(raw))
	for i, r := range raw {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			return nil, fmt.Errorf("TG_REGIME_RULES[%d]: rule id is required (audit provenance)", i)
		}
		sel, err := parseSharedSelector(r.Selector)
		if err != nil {
			return nil, fmt.Errorf("TG_REGIME_RULES[%d] (%q): %w", i, id, err)
		}
		reg := regime.Regime(strings.TrimSpace(r.Regime))
		if !reg.Valid() {
			return nil, fmt.Errorf("TG_REGIME_RULES[%d] (%q): unknown regime %q (want native-ssh/awx-job/gitops-mr/k8s-declarative/api)", i, id, r.Regime)
		}
		out = append(out, regime.Rule{ID: id, Selector: sel, Regime: reg})
	}
	return out, nil
}

// parseSharedSelector parses a "kind:pattern" token into a shared-object-model Selector (mirrors the
// credential resolver's grammar; a malformed/unknown kind fails closed by construction).
func parseSharedSelector(tok string) (credential.Selector, error) {
	tok = strings.TrimSpace(tok)
	i := strings.IndexByte(tok, ':')
	if i < 0 {
		return credential.Selector{}, fmt.Errorf("malformed selector %q: want kind:pattern", tok)
	}
	kind := credential.SelectorKind(strings.TrimSpace(tok[:i]))
	pattern := strings.TrimSpace(tok[i+1:])
	if pattern == "" {
		return credential.Selector{}, fmt.Errorf("selector %q has an empty pattern", tok)
	}
	switch kind {
	case credential.KindHost, credential.KindResource, credential.KindHostGlob, credential.KindGroup, credential.KindDeviceClass:
		return credential.Selector{Kind: kind, Pattern: pattern}, nil
	default:
		return credential.Selector{}, fmt.Errorf("selector %q has unknown kind %q (use host/host-glob/group/device-class/resource)", tok, kind)
	}
}

// targetForSelector builds a representative estate Target that a selector matches, for the boot self-check
// (does each declared rule resolve to a wired lane?). It maps the selector kind to the Target field it keys.
func targetForSelector(s credential.Selector) credential.Target {
	switch s.Kind {
	case credential.KindHost, credential.KindHostGlob:
		return credential.Target{Host: s.Pattern}
	case credential.KindResource:
		return credential.Target{Resource: s.Pattern}
	case credential.KindGroup:
		return credential.Target{Groups: []string{s.Pattern}}
	case credential.KindDeviceClass:
		return credential.Target{DeviceClass: s.Pattern}
	default:
		return credential.Target{}
	}
}

// awxTemplateSpec is one allowlisted AWX job template as config-not-code JSON (TG_AWXJOB_ALLOWLIST): the AWX
// job_template id, the op-class the policy engine authorizes for it, and the CLOSED extra_vars schema its
// launch variables must conform to (REQ-1704/1705). No command string anywhere — a template is not a shell.
type awxTemplateSpec struct {
	TemplateID int               `json:"template_id"`
	OpClass    string            `json:"op_class"`
	ExtraVars  map[string]string `json:"extra_vars"` // key -> declared primitive type (string/number/bool)
}

// awxTemplateResolver builds the runner's op-class→AWX-template id resolver (temporal/runner Deps seam) from
// the SAME operator allowlist the awx-job actuator uses (TG_AWXJOB_ALLOWLIST), inverting its template_id→op-class
// mapping to op-class→template_id so the runner can stamp a LaunchSpec's template id at seal time. It is
// FAIL-CLOSED: an unparseable/empty allowlist, an op-class bound to MORE THAN ONE template (the runner cannot
// deterministically choose), or a non-positive id all yield ok=false for that op-class — so the runner cannot
// encode a launch and the awx op is refused. The awx-job effect leaf RE-validates the resolved template
// against its own allowlist + the op-class binding at Exec (authoritative), so this seam is a convenience,
// never the authority.
func awxTemplateResolver(spec string) func(opClass string) (int, bool) {
	rev := map[string]int{}
	ambiguous := map[string]bool{}
	if al, err := parseAWXJobAllowlist(spec); err == nil {
		for id, pol := range al {
			oc := strings.TrimSpace(pol.OpClass)
			if oc == "" || id <= 0 {
				continue
			}
			if _, seen := rev[oc]; seen {
				ambiguous[oc] = true // >1 template bound to this op-class ⇒ the runner cannot pick one ⇒ fail closed
				continue
			}
			rev[oc] = id
		}
	}
	return func(opClass string) (int, bool) {
		oc := strings.TrimSpace(opClass)
		if ambiguous[oc] {
			return 0, false
		}
		id, ok := rev[oc]
		return id, ok
	}
}

// parseAWXJobAllowlist parses TG_AWXJOB_ALLOWLIST (a JSON array of {template_id,op_class,extra_vars}) into the
// operator template allowlist. Empty ⇒ an empty allowlist (the actuator is then read-only — it can only
// refuse). A malformed entry / illegal var type fails closed.
func parseAWXJobAllowlist(spec string) (awxjob.TemplateAllowlist, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return awxjob.TemplateAllowlist{}, nil
	}
	var raw []awxTemplateSpec
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST is not a JSON array of {template_id,op_class,extra_vars}: %w", err)
	}
	out := make(awxjob.TemplateAllowlist, len(raw))
	for i, t := range raw {
		if t.TemplateID <= 0 {
			return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST[%d]: template_id must be positive", i)
		}
		op := strings.TrimSpace(t.OpClass)
		if op == "" {
			return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST[%d] (template %d): op_class is required (the policy + graduation bucket)", i, t.TemplateID)
		}
		schema := awxjob.ExtraVarsSchema{}
		for k, typ := range t.ExtraVars {
			vt := awxjob.VarType(strings.TrimSpace(typ))
			if !vt.Valid() {
				return nil, fmt.Errorf("TG_AWXJOB_ALLOWLIST[%d] (template %d): extra_var %q declares illegal type %q (want string/number/bool)", i, t.TemplateID, k, typ)
			}
			schema[k] = vt
		}
		out[t.TemplateID] = awxjob.TemplatePolicy{OpClass: op, ExtraVarsSchema: schema}
	}
	return out, nil
}

// gitopsRepoSpec is one operator-declared gitops-mr repo policy (TG-122 slice 2): the JSON DTO for
// TG_GITOPSMR_ALLOWLIST. field_rules are the actuator's write-side concern and arrive with the arm-live
// slice; the sensor needs the repo/instance/token half only.
type gitopsRepoSpec struct {
	RepoID       string                `json:"repo_id"`
	BaseURL      string                `json:"base_url"`
	ProjectPath  string                `json:"project_path"`
	TargetBranch string                `json:"target_branch"`
	BranchPrefix string                `json:"branch_prefix"`
	TokenRef     string                `json:"token_ref"`
	OpClass      string                `json:"op_class"`
	FieldRules   []gitopsFieldRuleSpec `json:"field_rules"` // the actuator's write-side CLOSED locate rules (TG-122 slice 4); absent ⇒ sensor-only
}

// gitopsFieldRuleSpec is one closed field-locate rule (TG-122 slice 4): the ONLY fields the Renderer may edit
// on a repo. rule_id/file/selector are all required — a partial rule could not honestly resolve a single field.
type gitopsFieldRuleSpec struct {
	RuleID   string `json:"rule_id"`
	File     string `json:"file"`
	Selector string `json:"selector"`
}

// parseGitOpsMRAllowlist parses TG_GITOPSMR_ALLOWLIST (a JSON array of gitopsRepoSpec) into the per-repo
// gitops-mr policy map the sensor polls through and the actuator will write through (TG-122). Empty ⇒ an
// empty allowlist (nothing pollable, nothing writable). A malformed entry fails closed — a repo policy
// missing its instance, project, token ref, or op-class could neither be observed honestly nor governed.
func parseGitOpsMRAllowlist(spec string) (gitopsmr.RepoAllowlist, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return gitopsmr.RepoAllowlist{}, nil
	}
	var raw []gitopsRepoSpec
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, fmt.Errorf("TG_GITOPSMR_ALLOWLIST is not a JSON array of {repo_id,base_url,project_path,target_branch,branch_prefix,token_ref,op_class}: %w", err)
	}
	out := make(gitopsmr.RepoAllowlist, len(raw))
	for i, r := range raw {
		id := strings.TrimSpace(r.RepoID)
		if id == "" {
			return nil, fmt.Errorf("TG_GITOPSMR_ALLOWLIST[%d]: repo_id is required", i)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("TG_GITOPSMR_ALLOWLIST[%d]: duplicate repo_id %q", i, id)
		}
		base := strings.TrimSpace(r.BaseURL)
		proj := strings.TrimSpace(r.ProjectPath)
		tok := strings.TrimSpace(r.TokenRef)
		op := strings.TrimSpace(r.OpClass)
		if base == "" || proj == "" || tok == "" || op == "" {
			return nil, fmt.Errorf("TG_GITOPSMR_ALLOWLIST[%d] (%s): base_url, project_path, token_ref and op_class are all required", i, id)
		}
		branch := strings.TrimSpace(r.TargetBranch)
		if branch == "" {
			branch = "main"
		}
		prefix := strings.TrimSpace(r.BranchPrefix)
		if prefix == "" {
			prefix = "tg/"
		}
		var rules []gitopsmr.FieldRule
		seen := map[string]bool{}
		for j, fr := range r.FieldRules {
			rid, file, sel := strings.TrimSpace(fr.RuleID), strings.TrimSpace(fr.File), strings.TrimSpace(fr.Selector)
			if rid == "" || file == "" || sel == "" {
				return nil, fmt.Errorf("TG_GITOPSMR_ALLOWLIST[%d] (%s) field_rules[%d]: rule_id, file and selector are all required", i, id, j)
			}
			if seen[rid] {
				return nil, fmt.Errorf("TG_GITOPSMR_ALLOWLIST[%d] (%s): duplicate field rule_id %q", i, id, rid)
			}
			seen[rid] = true
			rules = append(rules, gitopsmr.FieldRule{RuleID: rid, File: file, Selector: sel})
		}
		out[id] = gitopsmr.RepoPolicy{
			BaseURL:      base,
			ProjectPath:  proj,
			TargetBranch: branch,
			BranchPrefix: prefix,
			TokenRef:     config.SecretRef(tok),
			OpClass:      op,
			FieldRules:   rules,
		}
	}
	return out, nil
}

// gitopsHasFieldRules reports whether any repo in the allowlist declares write-side field rules — the
// precondition for arming the gitops-mr actuator (arming a lane that can render nothing is refused config).
func gitopsHasFieldRules(a gitopsmr.RepoAllowlist) bool {
	for _, p := range a {
		if len(p.FieldRules) > 0 {
			return true
		}
	}
	return false
}

// gitopsProposeSpec is one operator-declared k8s-declarative op-class → gitops-mr propose mapping (TG-122
// slice 3): the JSON DTO for TG_GITOPSMR_PROPOSE_MAP. It binds an op-class to a repo on the allowlist and a
// param→FieldRule mapping, so the runner can translate the op-class's typed params into a ProposeSpec's
// closed FieldEdits — the awx-launch template-binding analogue for the declarative lane.
type gitopsProposeSpec struct {
	OpClass         string            `json:"op_class"`
	RepoID          string            `json:"repo_id"`           // MUST be a key on the gitops-mr RepoAllowlist
	Rationale       string            `json:"rationale"`         // templated MR prose (the actuator's secret guard re-screens it)
	ParamFieldRules map[string]string `json:"param_field_rules"` // param name -> FieldRuleID on the repo policy
}

// gitopsProposeResolver builds the runner's op-class→ProposeSpec resolver (temporal/runner Deps seam) from
// TG_GITOPSMR_PROPOSE_MAP. FAIL-CLOSED: an unparseable/empty map, an op-class bound to more than one entry,
// an entry missing its repo id or field-rule mapping, all yield ok=false for that op-class — so the runner
// cannot encode a propose and the declarative op is refused. The gitops-mr effect leaf RE-validates the
// resolved ProposeSpec (repo on the allowlist, op-class cross-check, one field per edit, no decoded secret)
// at Exec (authoritative); this seam is a convenience, never the authority.
func gitopsProposeResolver(spec string) func(opClass string, params map[string]string) (gitopsmr.ProposeSpec, bool) {
	byOp := map[string]gitopsProposeSpec{}
	ambiguous := map[string]bool{}
	if trimmed := strings.TrimSpace(spec); trimmed != "" {
		var raw []gitopsProposeSpec
		if err := json.Unmarshal([]byte(trimmed), &raw); err == nil {
			for _, e := range raw {
				oc := strings.TrimSpace(e.OpClass)
				if oc == "" || strings.TrimSpace(e.RepoID) == "" || len(e.ParamFieldRules) == 0 {
					continue // an incomplete mapping is unusable ⇒ skip ⇒ that op-class fails closed
				}
				if _, seen := byOp[oc]; seen {
					ambiguous[oc] = true // >1 mapping for one op-class ⇒ the runner cannot pick ⇒ fail closed
					continue
				}
				byOp[oc] = e
			}
		}
	}
	return func(opClass string, params map[string]string) (gitopsmr.ProposeSpec, bool) {
		oc := strings.TrimSpace(opClass)
		if ambiguous[oc] {
			return gitopsmr.ProposeSpec{}, false
		}
		e, ok := byOp[oc]
		if !ok {
			return gitopsmr.ProposeSpec{}, false
		}
		// Build one FieldEdit per mapped param that the proposal actually supplies. A mapped param absent from
		// params is simply not edited; an unmapped param is IGNORED (never a free-form edit). Deterministic
		// order (sorted by field-rule id) so the encoded spec is stable across runs.
		edits := make([]gitopsmr.FieldEdit, 0, len(e.ParamFieldRules))
		for param, ruleID := range e.ParamFieldRules {
			if v, present := params[param]; present {
				edits = append(edits, gitopsmr.FieldEdit{FieldRuleID: ruleID, NewValue: v})
			}
		}
		if len(edits) == 0 {
			return gitopsmr.ProposeSpec{}, false // nothing to propose ⇒ fail closed (never an empty MR)
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].FieldRuleID < edits[j].FieldRuleID })
		return gitopsmr.ProposeSpec{RepoID: e.RepoID, OpClass: oc, Edits: edits, Rationale: e.Rationale}, true
	}
}
