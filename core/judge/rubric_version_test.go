package judge

// THE BUMP ENFORCER (TG-194). A declared version only protects comparability if it actually moves when
// the content moves — the whole defect was a rubric edit (the hollow-proposal rule) that changed scoring
// with nothing recording it. This test pins version → content-hash: edit any calibration text without
// bumping Version and the build goes red with instructions, so an un-bumped edit cannot ship.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// knownRubricHashes is the append-only record: each declared version, and the sha256 of the rubric
// content (minus the version key itself) it names. NEVER edit an existing entry — append the new pair
// when you bump.
var knownRubricHashes = map[string]string{
	"2026-08-03.1": "5290aae8f3345da1633fe7931c33081bb17cf39a85786903d6857bd5a442f075",
	// TG-201: adds the deterministically-scored diagnosis_grounded axis + its calibration text. The LLM
	// prompt is byte-identical (TestPromptMatchesGolden still passes) — the bump records that a NEW axis
	// now grades sessions, so rows scored before and after are not pooled as if the same rubric produced them.
	"2026-08-04.1": "8d6f057314efd109a6e818b0bcc2efced94df75ed37b1a1849dfdd59b9d8e991",
	// TG-201 follow-up: the diagnosis_rule's SCALE text did not match the code it describes. It said
	// "3 = two or more" unmatched assertions, while ScoreDiagnosis floors at 2, so three-or-more uncited
	// assertions actually score 2, not 3. No scoring behaviour changed here — only the written rule, which is
	// what an operator reads to learn how their agent is graded, and a rubric that misstates its own scale is
	// not auditable. Appended rather than corrected in place: .1 shipped, and the entry records what it said.
	"2026-08-04.2": "c76e43a8244566286ccee3f1832d4579ab723e2fa1c93bfadd4540aa2b8d43f0",
	// TG-202: adds the second deterministically-scored axis, estate_grounded, and its calibration text — the
	// judge can now check a stated root cause against the causal estate graph instead of only against the
	// session's own prose. The LLM prompt is byte-identical again (the model is not shown the graph and does
	// not score this axis); the bump records that a NEW axis grades sessions from here on, so rows written
	// before and after are not pooled as if one rubric produced them.
	"2026-08-04.3": "3cbd96e5389900aa6f9f9d9e18d71ddbc55cab47404e52c0027c13a5bd60cb66",
}

func contentHashSansVersion(t *testing.T) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(RubricJSON(), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "version")
	b, err := json.Marshal(m) // Go map marshal sorts keys — canonical enough for equality
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// KILLING MUTATION: change any calibration text in rubric.json without bumping "version". RED.
func TestRubricVersionMovesWithContent(t *testing.T) {
	got := contentHashSansVersion(t)
	want, known := knownRubricHashes[RubricVersion()]
	if !known {
		t.Fatalf("rubric version %q has no recorded content hash — APPEND (%q: %q) to knownRubricHashes "+
			"in this file (never edit an existing entry)", RubricVersion(), RubricVersion(), got)
	}
	if got != want {
		t.Fatalf("rubric content changed under version %q (hash %s, recorded %s) — scoring changed with "+
			"nothing recording it, which is exactly TG-194's defect. Bump \"version\" in rubric.json and "+
			"append the new pair to knownRubricHashes", RubricVersion(), got, want)
	}
}
