package deploy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// spec/020 REQ-2012's third clause: "the assemble.py build byte-reproduces the served index.html".
//
// That property already had a check — `python3 deploy/console/v2/assemble.py --check`, run by the
// console-drift CI job — and two gaps made it weaker than it reads:
//
//  1. THE JOB IS CHANGES-GATED on deploy/console/v2/**. index.html is a BUILD ARTIFACT of assemble.py
//     plus modules/*, so the only way it drifts is through those paths — but a changes-gated job is a
//     conditional check, and `make all` could not tell you the artifact was reproducible.
//  2. IT IS NOT AN INDEXABLE ORACLE. The acceptance lattice resolves a scenario's `test` field against
//     files with a test-bearing extension (.go/.ts/.tsx/.mjs/.js). A .py entry point and a CI job name
//     are both invisible to it, so the clause could not be mapped without either lying about what runs
//     or leaving a delivered property recorded as debt.
//
// This wraps the SAME command rather than reimplementing the comparison. Reimplementing it in Go would
// create a second definition of "reproduces", and the two could then disagree while both stayed green —
// the drift this check exists to catch, one level up.
func TestAssemblePyByteReproducesTheServedIndex(t *testing.T) {
	const script = "console/v2/assemble.py"
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("%s is missing — the served console has no declared build, so nothing can say whether "+
			"index.html was hand-edited: %v", script, err)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		// NOT a skip. A silently-skipped check reads identically to a passing one, which is how `make all`
		// came to be green while skipping 34 DB tests. python3 is present in the toolchain this repo
		// already requires (the same interpreter runs the spec-task tally in the Makefile).
		t.Fatalf("python3 not found, so this check cannot run — and a check that cannot run must say so "+
			"rather than pass: %v", err)
	}

	out, err := exec.Command(py, script, "--check").CombinedOutput()
	got := strings.TrimSpace(string(out))
	if err != nil {
		t.Fatalf("assemble.py --check FAILED — the committed index.html is not what assemble.py produces, so "+
			"the served console has drifted from its source (or was hand-edited ahead of it):\n%s", got)
	}

	// VACUITY FLOOR. A --check that printed nothing, or that stopped asserting reproduction and merely
	// exited 0, would make this test pass while proving nothing. Pin the claim the command makes.
	if !strings.Contains(got, "byte-for-byte") {
		t.Errorf("assemble.py --check exited 0 but did not claim byte-for-byte reproduction — this oracle is "+
			"asserting an exit code rather than the property. Output: %q", got)
	}
}
