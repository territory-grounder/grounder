package main

// TG-122 slice 2 — the TG_GITOPSMR_ALLOWLIST parser. Fail-closed like its awx sibling: a policy that cannot
// name its instance, project, token ref, or op-class is refused at boot, never half-loaded. All tokens
// synthetic.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/actuation/gitopsmr"
)

func TestParseGitOpsMRAllowlist(t *testing.T) {
	al, err := parseGitOpsMRAllowlist(`[
		{"repo_id":"infra-a","base_url":"https://git.example-int.lan","project_path":"grp/prod",
		 "token_ref":"env:TG_TEST_TOK","op_class":"gitops-mr-propose",
		 "field_rules":[{"rule_id":"reps","file":"argocd-apps/x.yaml","selector":"spec.replicas"}]}
	]`)
	if err != nil {
		t.Fatal(err)
	}
	p := al["infra-a"]
	if p.TargetBranch != "main" || p.BranchPrefix != "tg/" {
		t.Errorf("defaults not applied: %+v", p)
	}
	if p.ProjectPath != "grp/prod" || string(p.TokenRef) != "env:TG_TEST_TOK" {
		t.Errorf("parsed policy wrong: %+v", p)
	}
	// The actuator's write-side field_rules (TG-122 slice 4) parse into the policy.
	if len(p.FieldRules) != 1 || p.FieldRules[0].RuleID != "reps" ||
		p.FieldRules[0].File != "argocd-apps/x.yaml" || p.FieldRules[0].Selector != "spec.replicas" {
		t.Errorf("field_rules not parsed: %+v", p.FieldRules)
	}

	if got, err := parseGitOpsMRAllowlist("   "); err != nil || len(got) != 0 {
		t.Errorf("empty spec must yield an empty allowlist, got %v %v", got, err)
	}

	for name, spec := range map[string]string{
		"not json":           `{`,
		"missing repo":       `[{"base_url":"https://x","project_path":"a/b","token_ref":"env:T","op_class":"c"}]`,
		"missing base":       `[{"repo_id":"r","project_path":"a/b","token_ref":"env:T","op_class":"c"}]`,
		"missing token":      `[{"repo_id":"r","base_url":"https://x","project_path":"a/b","op_class":"c"}]`,
		"missing class":      `[{"repo_id":"r","base_url":"https://x","project_path":"a/b","token_ref":"env:T"}]`,
		"duplicate repo":     `[{"repo_id":"r","base_url":"https://x","project_path":"a/b","token_ref":"env:T","op_class":"c"},{"repo_id":"r","base_url":"https://y","project_path":"a/c","token_ref":"env:T2","op_class":"c"}]`,
		"partial field rule": `[{"repo_id":"r","base_url":"https://x","project_path":"a/b","token_ref":"env:T","op_class":"c","field_rules":[{"rule_id":"x","file":"f.yaml"}]}]`,
		"dup field rule":     `[{"repo_id":"r","base_url":"https://x","project_path":"a/b","token_ref":"env:T","op_class":"c","field_rules":[{"rule_id":"x","file":"a.yaml","selector":"s"},{"rule_id":"x","file":"b.yaml","selector":"s"}]}]`,
	} {
		if _, err := parseGitOpsMRAllowlist(spec); err == nil {
			t.Errorf("%s: must fail closed", name)
		}
	}
}

// The allowlist env key is PLANE-SCOPED: it carries per-repo api-scoped PAT refs, so the triage plane must
// never see it (the same withholding every actuation credential gets).
func TestGitOpsMRAllowlistIsActuationPlaneScoped(t *testing.T) {
	found := false
	for _, k := range actuationPlaneEnvKeys {
		if k == "TG_GITOPSMR_ALLOWLIST" {
			found = true
		}
	}
	if !found {
		t.Fatal("TG_GITOPSMR_ALLOWLIST must be on actuationPlaneEnvKeys — it carries write-capable token refs")
	}
}

// The adapter's contract: module strings pass through as regime slugs, and errors PROPAGATE (a poller error
// must leave the deferred verify pending, never fabricate a status). Driven through a REAL Poller over a
// failing transport — the reachable production shape, not a nil-field stub.
func TestGitOpsMRJobPollerAdapterPropagatesErrors(t *testing.T) {
	t.Setenv("TG_TEST_GITOPS_ADAPTER_TOK", "tok")
	failing := gitopsmr.NewPoller(gitopsmr.RepoAllowlist{
		"r": {BaseURL: "https://git.example-int.lan", ProjectPath: "a/b",
			TokenRef: config.SecretRef("env:TG_TEST_GITOPS_ADAPTER_TOK"), OpClass: "c"},
	}, gitopsmr.WithPollerHTTPClient(erroringDoer{}))
	p := gitOpsMRJobPoller{poller: failing}
	if st, err := p.PollJob(t.Context(), "r!1"); err == nil {
		t.Errorf("a transport error must propagate through the adapter (got status %q) — the deferred verify must stay pending", st)
	}
	if st, err := p.PollJob(t.Context(), "not-on-allowlist!2"); err == nil {
		t.Errorf("an un-allowlisted repo must propagate as an error (got %q)", st)
	}
}

type erroringDoer struct{}

func (erroringDoer) Do(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("transport down")
}

func TestRefusingJobPollerAlwaysErrors(t *testing.T) {
	if _, err := (refusingJobPoller{}).PollJob(t.Context(), "any"); err == nil || !strings.Contains(err.Error(), "unobservable") {
		t.Fatalf("the fallback poller must refuse loudly, got %v", err)
	}
}
