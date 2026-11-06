package gate

// TG-194: one rubric per comparison — the gate refuses arms judged under different rubric wordings.

import (
	"strings"
	"testing"
)

// KILLING MUTATION: drop the version check from VerifyComparable. RED — a rubric edit between the base
// and candidate runs would move the verdict with no code change, silently.
func TestVerifyComparableRefusesMixedRubricVersions(t *testing.T) {
	base := []Scorecard{{N: 10, RubricVersion: "2026-08-03.1"}}
	cand := []Scorecard{{N: 10, RubricVersion: "2026-09-01.1"}}
	probs := VerifyComparable(base, cand)
	if len(probs) == 0 {
		t.Fatal("arms judged under two different rubrics compared without objection")
	}
	if !strings.Contains(strings.Join(probs, " "), "rubric") {
		t.Fatalf("the refusal does not name the rubric: %v", probs)
	}
	// control: same version compares clean, so the refusal is discriminating
	if probs := VerifyComparable(base, []Scorecard{{N: 10, RubricVersion: "2026-08-03.1"}}); len(probs) != 0 {
		t.Fatalf("same-version arms refused: %v", probs)
	}
}

// KILLING MUTATION: Pool manufactures a version for a mixed input. RED.
func TestPoolNeverManufacturesASingleVersionClaim(t *testing.T) {
	mixed := Pool([]Scorecard{{N: 5, RubricVersion: "a"}, {N: 5, RubricVersion: "b"}})
	if mixed.RubricVersion != "" {
		t.Fatalf("mixed pool claims version %q", mixed.RubricVersion)
	}
	uniform := Pool([]Scorecard{{N: 5, RubricVersion: "a"}, {N: 5, RubricVersion: "a"}})
	if uniform.RubricVersion != "a" {
		t.Fatalf("uniform pool lost its version: %q", uniform.RubricVersion)
	}
}
