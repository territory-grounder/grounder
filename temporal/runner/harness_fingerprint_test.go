package runner

import (
	"regexp"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/harnessid"
)

// TG-575 (Harness² learnings H2-1): every investigate result carries the harness identity, and it is a
// CONFIGURATION identity, not a session one — the same worker configuration fingerprints identically across
// execution classes (whose seeds differ), and carries no incident data. Harness² stamps every rollout with
// the fingerprint of the harness that produced it so evidence is never pooled across configurations; this
// pins the TG stamp at the activity boundary, where the seed hash is stamped too.
// harnessFingerprint is the test-side derivation of the persisted value: nil agent ⇒ unrecorded.
func harnessFingerprint(ag *agent.Agent) string {
	if ag == nil {
		return ""
	}
	return harnessid.Fingerprint(ag.HarnessParts(), promptPreambleVersion)
}

func TestHarnessFingerprintIsAConfigurationIdentity(t *testing.T) {
	hexSHA := regexp.MustCompile(`^[0-9a-f]{64}$`)
	_, deep, _, _ := seedFixture(t, string(execclass.DeepInvestigation))
	_, fast, _, _ := seedFixture(t, string(execclass.FastAgent))
	if !hexSHA.MatchString(deep.HarnessFP) {
		t.Fatalf("HarnessFP must be a hex SHA-256, got %q", deep.HarnessFP)
	}
	if deep.SeedHash == fast.SeedHash {
		t.Fatal("sanity: the deep and lean seeds differ, so their seed hashes must differ")
	}
	if deep.HarnessFP != fast.HarnessFP {
		t.Fatalf("the harness fingerprint must not vary with the execution class (deep %s vs fast %s) — it names the configuration, not the session", deep.HarnessFP[:12], fast.HarnessFP[:12])
	}
	if deep.HarnessFP == deep.SeedHash {
		t.Fatal("the harness fingerprint must not collapse onto the seed hash")
	}
	if harnessFingerprint(nil) != "" {
		t.Fatal("a nil agent has no harness; the fingerprint must read unrecorded, never a hash of nothing")
	}
}

// The fingerprint MOVES with what it claims to identify (the review of TG-575 asked for the positive half:
// DEEP == FAST holds by construction, so the property that matters is sensitivity). A different guidance
// half — a base-prompt trial arm's candidate body, or a graduated guidance row — is a different
// configuration; so is a different tool catalog (a pack-filtered subset). Both must change the value, and
// the tiers must not (they ride the row as model_tier / decision_model_tier).
func TestHarnessFingerprintMovesWithGuidanceAndToolsNotTiers(t *testing.T) {
	deps := testDeps()
	base := &agent.Agent{Tools: deps.Tools, Guidance: "## Guidance A", ModelName: "fast", DecisionModelName: "primary"}
	same := &agent.Agent{Tools: deps.Tools, Guidance: "## Guidance A", ModelName: "primary", DecisionModelName: "fast"}
	otherGuidance := &agent.Agent{Tools: deps.Tools, Guidance: "## Guidance B (trial arm)", ModelName: "fast", DecisionModelName: "primary"}
	otherTools := &agent.Agent{Tools: agent.NewReadOnlyToolSet(), Guidance: "## Guidance A", ModelName: "fast", DecisionModelName: "primary"}
	if harnessFingerprint(base) != harnessFingerprint(same) {
		t.Fatal("the model tiers must not move the fingerprint — they are recorded as their own columns")
	}
	if harnessFingerprint(base) == harnessFingerprint(otherGuidance) {
		t.Fatal("a different guidance half (a trial arm) is a different configuration and must change the fingerprint")
	}
	if harnessFingerprint(base) == harnessFingerprint(otherTools) {
		t.Fatal("a different tool catalog (a pack-filtered subset) is a different configuration and must change the fingerprint")
	}
}
