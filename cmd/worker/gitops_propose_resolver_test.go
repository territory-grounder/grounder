package main

// TG-122 slice 3 — the TG_GITOPSMR_PROPOSE_MAP resolver: op-class + params → a gitops-mr ProposeSpec, all
// fail-closed. It is a convenience seam; the gitops-mr leaf re-validates, so every ambiguity here resolves to
// "no propose" rather than a guess.

import (
	"testing"
)

func TestGitOpsProposeResolver(t *testing.T) {
	r := gitopsProposeResolver(`[
		{"op_class":"k8s-set-replicas","repo_id":"infra-nl","rationale":"scale up",
		 "param_field_rules":{"replicas":"replicas-rule"}}
	]`)
	spec, ok := r("k8s-set-replicas", map[string]string{"replicas": "3", "ignored": "x"})
	if !ok {
		t.Fatal("a mapped op-class with a supplied param must resolve")
	}
	if spec.RepoID != "infra-nl" || spec.OpClass != "k8s-set-replicas" || spec.Rationale != "scale up" {
		t.Fatalf("spec = %+v, want repo/op/rationale from the map", spec)
	}
	// Exactly one edit — the mapped param; the unmapped "ignored" param is NEVER an edit (no free-form edits).
	if len(spec.Edits) != 1 || spec.Edits[0].FieldRuleID != "replicas-rule" || spec.Edits[0].NewValue != "3" {
		t.Fatalf("edits = %+v, want only the mapped replicas edit", spec.Edits)
	}
}

func TestGitOpsProposeResolverFailsClosed(t *testing.T) {
	// Empty / malformed config ⇒ resolves nothing.
	for _, spec := range []string{"", "  ", "{not json"} {
		if _, ok := gitopsProposeResolver(spec)("any-op", map[string]string{"x": "1"}); ok {
			t.Errorf("config %q must resolve nothing", spec)
		}
	}
	// Incomplete entries are skipped (missing repo / missing field rules) ⇒ that op-class fails closed.
	for name, cfg := range map[string]string{
		"no repo":        `[{"op_class":"o","param_field_rules":{"p":"r"}}]`,
		"no field rules": `[{"op_class":"o","repo_id":"infra-nl"}]`,
		"empty op":       `[{"op_class":"","repo_id":"infra-nl","param_field_rules":{"p":"r"}}]`,
	} {
		if _, ok := gitopsProposeResolver(cfg)("o", map[string]string{"p": "1"}); ok {
			t.Errorf("%s: must fail closed", name)
		}
	}
	// AMBIGUOUS — one op-class bound twice ⇒ the runner cannot pick ⇒ fail closed.
	dup := `[{"op_class":"o","repo_id":"a","param_field_rules":{"p":"r1"}},{"op_class":"o","repo_id":"b","param_field_rules":{"p":"r2"}}]`
	if _, ok := gitopsProposeResolver(dup)("o", map[string]string{"p": "1"}); ok {
		t.Error("a doubly-bound op-class must fail closed (ambiguous)")
	}
	// A mapped op-class whose mapped param is ABSENT from the proposal ⇒ no edits ⇒ fail closed (never an empty MR).
	r := gitopsProposeResolver(`[{"op_class":"o","repo_id":"infra-nl","param_field_rules":{"replicas":"r"}}]`)
	if _, ok := r("o", map[string]string{"other": "1"}); ok {
		t.Error("no mapped param supplied ⇒ no edits ⇒ must fail closed (an empty MR is refused)")
	}
}

// Both gitops-mr config keys are actuation-plane-scoped: the allowlist carries the PAT refs, and the propose
// map resolves WRITE proposals against those repos — neither belongs on the triage plane. main.go reads the
// propose map through planeEnv, so this scoping is what actually withholds it.
func TestGitOpsMRConfigKeysArePlaneScoped(t *testing.T) {
	want := map[string]bool{"TG_GITOPSMR_ALLOWLIST": false, "TG_GITOPSMR_PROPOSE_MAP": false}
	for _, k := range actuationPlaneEnvKeys {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("%s must be actuation-plane-scoped (it targets actuation repos / carries token refs)", k)
		}
	}
}
