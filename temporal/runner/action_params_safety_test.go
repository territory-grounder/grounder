package runner

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-146 A3. The stateful/destructive floor was derived from Target/Op/OpClass TEXT only. A database named
// solely in Params was invisible to the classifier, so the band was RECORDED as auto.
//
// Measured 2026-08-06: of five actuation lanes (awxjob, kubernetes, mcp, proxmox, ssh) only ssh carries a
// stateful check of its own. For the other four the classifier is the ONLY line of defence, and it could
// not see the workload it was defending.

// TestADatabaseNamedOnlyInParamsIsSeen is the defect.
func TestADatabaseNamedOnlyInParamsIsSeen(t *testing.T) {
	a := manifest.Action{
		Target:  "dc1app01", // no stateful token
		Op:      "restart-service",
		OpClass: "restart-service",
		Params:  map[string]string{"unit": "mariadb.service"}, // the ONLY mention
	}

	if safety.IsStatefulWorkload(a.Target, a.Op, a.OpClass) {
		t.Fatal("precondition failed: the target/op/opclass triple already looks stateful, so this fixture " +
			"cannot demonstrate the params-blind gap — pick a target with no stateful token")
	}
	if !safety.IsStatefulWorkload(actionSafetyParts(a)...) {
		t.Error("a restart of mariadb.service on a neutrally-named host is NOT seen as a stateful " +
			"workload. The classifier's stateful-workload-mutation clamp never fires, the band is recorded " +
			"as auto, and four of the five actuation lanes have no check of their own to catch it.")
	}
}

// TestADestructiveCommandHiddenInParamsIsSeen — the same shape for the other predicate. "A plan cannot
// hide a mutation" is the stated principle; a param is a place to hide one.
func TestADestructiveCommandHiddenInParamsIsSeen(t *testing.T) {
	a := manifest.Action{
		Target:  "dc1app01",
		Op:      "run",
		OpClass: "restart-service", // under-declared
		Params:  map[string]string{"command": "dropdb prod"},
	}

	if safety.IsDestructiveOp(a.Op, a.OpClass) {
		t.Fatal("precondition failed: op/opclass alone already read as destructive")
	}
	if !safety.IsDestructiveOp(actionSafetyParts(a)...) {
		t.Error("`dropdb prod` carried in Params is not seen as destructive while the op_class claims " +
			"restart-service — exactly the under-declared op the server-derived override exists to catch")
	}
}

// TestTheOriginalTripleIsStillIncluded. The params must be ADDED to the existing signal, not replace it —
// dropping Target would stop a stateful HOSTNAME from clamping, which is what the predicate caught before.
func TestTheOriginalTripleIsStillIncluded(t *testing.T) {
	a := manifest.Action{
		Target:  "dc1cl01mariadb01", // stateful by hostname, per core/safety's substring rule
		Op:      "restart-service",
		OpClass: "restart-service",
	}
	if !safety.IsStatefulWorkload(actionSafetyParts(a)...) {
		t.Error("a stateful HOSTNAME stopped clamping once params were added — the params are additional " +
			"evidence, never a replacement for the triple")
	}

	got := actionSafetyParts(a)
	for _, want := range []string{"dc1cl01mariadb01", "restart-service"} {
		var found bool
		for _, p := range got {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is missing from the safety parts %v", want, got)
		}
	}
}

// TestParamKeysAreIncludedToo — a param NAMED statefulset names the workload class even when its value is
// an innocuous identifier.
func TestParamKeysAreIncludedToo(t *testing.T) {
	a := manifest.Action{
		Target:  "dc1app01",
		Op:      "scale",
		OpClass: "scale-workload",
		Params:  map[string]string{"statefulset": "abc123"},
	}
	if !safety.IsStatefulWorkload(actionSafetyParts(a)...) {
		t.Error("a param KEY of `statefulset` did not clamp — the key names the workload class as surely " +
			"as the value would")
	}
}

// TestTheOutputIsDeterministic. This runs in Temporal WORKFLOW code, where a replay must reproduce the
// same decision. Map iteration order is random in Go; the result is order-independent because the
// predicates OR over their parts, but the emitted slice is sorted so that is visible rather than argued.
func TestTheOutputIsDeterministic(t *testing.T) {
	a := manifest.Action{
		Target: "h", Op: "o", OpClass: "c",
		Params: map[string]string{"z": "1", "a": "2", "m": "3", "b": "4", "y": "5"},
	}
	first := strings.Join(actionSafetyParts(a), "|")
	for i := 0; i < 200; i++ {
		if got := strings.Join(actionSafetyParts(a), "|"); got != first {
			t.Fatalf("actionSafetyParts is not deterministic across calls:\n  %s\n  %s", first, got)
		}
	}
}

// TestNoParamsIsUnchanged — the overwhelming majority of actions carry no params, and their classification
// must be byte-identical to before this change.
func TestNoParamsIsUnchanged(t *testing.T) {
	a := manifest.Action{Target: "dc1app01", Op: "restart-service", OpClass: "restart-service"}
	got := actionSafetyParts(a)
	if len(got) != 3 || got[0] != a.Target || got[1] != a.Op || got[2] != a.OpClass {
		t.Errorf("an action with no params produced %v, want exactly the original triple", got)
	}
}

// TestBothSafetyInputsActuallyUseTheParts is the wiring guard, and it exists because its absence let a
// mutation through: reverting the workflow call site to the bare (Target, Op, OpClass) triple left every
// test above GREEN. actionSafetyParts stays perfectly correct about an input nothing feeds it — the
// resolver-guarded/wiring-unguarded shape this repo keeps producing.
func TestBothSafetyInputsActuallyUseTheParts(t *testing.T) {
	raw, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatalf("read workflow.go: %v", err)
	}
	src := stripRunnerComments(string(raw))
	if len(src) < 5000 {
		t.Fatalf("VACUITY FLOOR: workflow.go stripped to %d bytes — every assertion below would pass on a stub",
			len(src))
	}

	for _, c := range []struct{ field, fn string }{
		{"Stateful", "IsStatefulWorkload"},
		{"Destructive", "IsDestructiveOp"},
	} {
		i := strings.Index(src, "safety."+c.fn+"(")
		if i < 0 {
			t.Errorf("workflow.go never calls safety.%s — the %s safety input is not derived at all",
				c.fn, c.field)
			continue
		}
		// Scoped to the call, not the file: a file-wide search for actionSafetyParts would be satisfied by
		// the OTHER call site, which is how a guard of mine survived gutting exactly one of two before.
		end := i + 160
		if end > len(src) {
			end = len(src)
		}
		call := src[i:end]
		if !strings.Contains(call, "actionSafetyParts(") {
			t.Errorf("safety.%s is called WITHOUT actionSafetyParts, so the %s floor cannot see the "+
				"action's Params. A database named only in Params (unit: mariadb.service) classifies as "+
				"not-stateful and the band is recorded as auto — and four of the five actuation lanes have "+
				"no check of their own.\ncall site:\n%s", c.fn, c.field, call)
		}
	}
}

// stripRunnerComments removes Go line comments so the assertion above cannot be satisfied by prose. The
// block it reads is heavily commented and names both predicates in its comments.
func stripRunnerComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestStripRunnerCommentsActuallyStrips(t *testing.T) {
	got := stripRunnerComments("// safety.IsStatefulWorkload(actionSafetyParts(x))\nreal()\n")
	if strings.Contains(got, "IsStatefulWorkload") {
		t.Fatalf("the stripper left a comment in place, so the wiring assertion can be satisfied by prose: %q", got)
	}
	if !strings.Contains(got, "real()") {
		t.Fatalf("the stripper removed real code: %q", got)
	}
}
