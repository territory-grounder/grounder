package deploy_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryCIJobOptsIntoMergeRequestPipelines catches a CI job that silently never runs.
//
// THIS GUARD EXISTS BECAUSE IT ALREADY HAPPENED, TO THE DARK-COMPONENT DETECTOR ITSELF. `darkcheck` was
// added without `<<: *rules-mr`, so it had no rules; GitLab's default `only` is [branches, tags], which
// EXCLUDES merge_request_event. The job therefore did not run in MR pipeline 45046 — and the pipeline
// went GREEN, because a job that does not exist cannot fail. A gate that never runs is the exact defect
// class core/wiring was built to detect, reproduced one layer up in the CI config.
//
// KILLING MUTATION: remove `<<: *rules-mr` from any job. That job stops running on merge requests while
// every pipeline stays green, and this test fails by name.
func TestEveryCIJobOptsIntoMergeRequestPipelines(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".gitlab-ci.yml"))
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	// A top-level job key: column 0, ends in ':', not a YAML anchor/template (leading '.'), not a
	// reserved top-level key.
	jobKey := regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9_-]*):\s*$`)
	reserved := map[string]bool{
		"stages": true, "variables": true, "default": true, "include": true, "workflow": true,
		"image": true, "services": true, "before_script": true, "after_script": true, "cache": true,
	}

	var checked int
	for i, ln := range lines {
		m := jobKey.FindStringSubmatch(ln)
		if m == nil || reserved[m[1]] {
			continue
		}
		name := m[1]
		// Collect the job body: everything indented until the next column-0 line.
		var body strings.Builder
		for j := i + 1; j < len(lines); j++ {
			nxt := lines[j]
			if nxt != "" && !strings.HasPrefix(nxt, " ") && !strings.HasPrefix(nxt, "\t") {
				break
			}
			body.WriteString(nxt + "\n")
		}
		text := body.String()
		// A job satisfies the rule if it merges the shared anchor, declares its own rules/only, or
		// EXTENDS a template that carries rules. The extends case is not a loophole: cosign-attest and
		// cosign-verify extend .image-build, which is deliberately `if: $CI_COMMIT_BRANCH == "main"`
		// because signing and verifying artifacts is a main-only act. The first draft of this guard
		// flagged both — a false positive that would have had me "fix" two correct jobs.
		if strings.Contains(text, "*rules-mr") || strings.Contains(text, "rules:") ||
			strings.Contains(text, "only:") || strings.Contains(text, "trigger:") ||
			extendsATemplateWithRules(text, string(raw)) {
			checked++
			continue
		}
		t.Errorf("CI job %q has no rules and no *rules-mr: GitLab's default `only` is [branches, tags], "+
			"so it will NOT run on merge requests — and the pipeline will go green having never run it. "+
			"This is what happened to `darkcheck` in pipeline 45046.", name)
	}

	// VACUITY FLOOR: if the key matcher stopped matching (an indentation or formatting change), a passing
	// run here would certify nothing. Same defense as the wiring branch guard.
	if checked < 5 {
		t.Fatalf("vacuity floor: only %d CI jobs were inspected — the job-key matcher is broken and a "+
			"passing run would prove nothing", checked)
	}
}

// extendsATemplateWithRules reports whether a job body `extends:` a template that itself declares rules.
// One level of indirection is enough for this repo and keeps the guard readable; a template extending a
// template would need a real YAML walk, and the vacuity floor would catch the matcher going blind.
func extendsATemplateWithRules(body, whole string) bool {
	ext := regexp.MustCompile(`extends:\s*\.?([a-zA-Z][a-zA-Z0-9_-]*)`)
	m := ext.FindStringSubmatch(body)
	if m == nil {
		return false
	}
	tmpl := "." + strings.TrimPrefix(m[1], ".") + ":"
	i := strings.Index(whole, "\n"+tmpl)
	if i < 0 {
		return false
	}
	rest := whole[i+1:]
	if j := strings.Index(rest[1:], "\n"); j >= 0 {
		// Read the template body: indented lines only.
		var b strings.Builder
		for _, ln := range strings.Split(rest, "\n")[1:] {
			if ln != "" && !strings.HasPrefix(ln, " ") && !strings.HasPrefix(ln, "\t") {
				break
			}
			b.WriteString(ln + "\n")
		}
		return strings.Contains(b.String(), "rules:")
	}
	return false
}
