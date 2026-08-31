package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryEvalCISelfTestIsInvoked catches an eval/ci SELF-TEST that is present in the tree but wired into no
// .gitlab-ci.yml job — a correct control that nothing invokes, the exact defect the repo keeps finding, one
// level up from Go wiring: a shell test CI never runs. Replaying !1059 (TG-359) surfaced it — the surviving
// killing mutation was commenting out `- bash eval/ci/check-rubric-gate-wired_test.sh`: every test still
// passed and nothing went red, because a self-test CI does not run cannot fail. These self-tests each exist
// because a gate was once found mis-reporting, so their silence is precisely the state they were written to
// prevent. Nothing else reads .gitlab-ci.yml and asserts a given script is actually invoked (TG-406).
//
// The expected set is DERIVED FROM THE TREE, never a hand-list: a hand-list would be maintained by the same
// person who forgot the CI line, and would silently miss a NEWLY-added self-test (mutation (b) below).
//
// KILLING MUTATIONS: (a) comment out any `eval/ci/*_test.sh` invocation in .gitlab-ci.yml -> RED by name;
// (b) add a new eval/ci/<x>_test.sh that no job runs -> RED (the case a hand-list misses); (c) vacuity floor
// -> if the tree walk finds nothing, fail loudly rather than certify an empty set.
func TestEveryEvalCISelfTestIsInvoked(t *testing.T) {
	root := ".."
	scripts, err := filepath.Glob(filepath.Join(root, "eval", "ci", "*_test.sh"))
	if err != nil {
		t.Fatalf("glob eval/ci/*_test.sh: %v", err)
	}
	// VACUITY FLOOR: if the glob matched nothing (eval/ci moved, or the *_test.sh convention changed), a
	// passing run would certify nothing about self-test wiring. Fail loudly instead.
	if len(scripts) == 0 {
		t.Fatal("vacuity floor: found no eval/ci/*_test.sh scripts — the tree walk is blind and a passing run " +
			"would prove nothing")
	}

	raw, err := os.ReadFile(filepath.Join(root, ".gitlab-ci.yml"))
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	// Match only against NON-COMMENT lines, so a script name that survives only in a `#` comment does not read
	// as "wired" — a commented-out invocation is exactly the unwired state this guard exists to catch.
	var body strings.Builder
	for _, ln := range strings.Split(string(raw), "\n") {
		if s := strings.TrimSpace(ln); s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		body.WriteString(ln)
		body.WriteByte('\n')
	}
	ci := body.String()

	for _, s := range scripts {
		rel := "eval/ci/" + filepath.Base(s)
		if !strings.Contains(ci, rel) {
			t.Errorf("CI self-test %q exists in the tree but is invoked by NO .gitlab-ci.yml job (searched for %q "+
				"in non-comment lines): a self-test that CI never runs is the exact state it was written to "+
				"prevent (TG-406). Wire it into a job's script, or delete it if it is truly dead.",
				filepath.Base(s), rel)
		}
	}
}
