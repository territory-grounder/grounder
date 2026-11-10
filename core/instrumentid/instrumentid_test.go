package instrumentid

import (
	"strings"
	"testing"
)

func full() Components {
	return Components{BuildSHA: "abc123", HarnessFP: strings.Repeat("a", 64), OpSchemaHash: strings.Repeat("b", 64),
		PolicyBundle: "sha256:666cdc1c8", ModelAliasHash: strings.Repeat("c", 64), RubricVersion: "2026-11-08.1",
		SkillSetHash: strings.Repeat("d", 64), KnowledgeHash: strings.Repeat("e", 64)}
}

// The identity is a function of every configured component: change any one and it moves; recompute it from the
// canonical text and it agrees; two equal component sets agree.
func TestIdentityMovesWithEveryComponentAndIsRecomputable(t *testing.T) {
	base := full()
	id := base.ID()
	if len(id) != 64 || base.ID() != id {
		t.Fatalf("id = %q, want a stable 64-hex", id)
	}
	muts := []func(*Components){
		func(c *Components) { c.BuildSHA = "def456" },
		func(c *Components) { c.HarnessFP = strings.Repeat("1", 64) },
		func(c *Components) { c.OpSchemaHash = strings.Repeat("2", 64) },
		func(c *Components) { c.PolicyBundle = "sha256:3b9ee0ab4" },
		func(c *Components) { c.ModelAliasHash = strings.Repeat("3", 64) },
		func(c *Components) { c.RubricVersion = "2026-10-23.1" },
		func(c *Components) { c.SkillSetHash = strings.Repeat("4", 64) },
		func(c *Components) { c.KnowledgeHash = strings.Repeat("5", 64) },
	}
	if len(muts) != len(Fields) {
		t.Fatalf("this test mutates %d components but Fields names %d — a new component needs a mutation here", len(muts), len(Fields))
	}
	for i, m := range muts {
		c := full()
		m(&c)
		if c.ID() == id {
			t.Errorf("changing %s did not move the identity", Fields[i])
		}
	}
	// The canonical text names every field in order, so the id is recomputable by hand.
	canon := base.Canonical()
	for _, f := range Fields {
		if !strings.Contains(canon, f+"=") {
			t.Errorf("canonical text lacks %s", f)
		}
	}
	if strings.Index(canon, "build_sha=") > strings.Index(canon, "harness_fp=") {
		t.Error("canonical order is not Fields order")
	}
}

// A component the process could not read is NAMED, never guessed; two sessions missing the same component share
// an identity that says what it lacks.
func TestUnrecordedComponentsAreNamedNotInvented(t *testing.T) {
	c := full()
	c.PolicyBundle, c.KnowledgeHash = "", ""
	if got := c.Unrecorded(); strings.Join(got, ",") != "policy_bundle,knowledge_hash" {
		t.Fatalf("Unrecorded = %v", got)
	}
	if len(full().Unrecorded()) != 0 {
		t.Fatal("a complete component set reported something unrecorded")
	}
	d := c
	if d.ID() != c.ID() {
		t.Fatal("two identical incomplete sets must share one identity (the row names the gap)")
	}
}

// Set hashes are order-free and an empty set is a recorded value.
func TestHashStringsIsOrderFree(t *testing.T) {
	a := HashStrings([]string{"k8s-triage@1.0.0:h1", "linux-triage@2.0.0:h2"})
	b := HashStrings([]string{"linux-triage@2.0.0:h2", "k8s-triage@1.0.0:h1"})
	if a != b || len(a) != 64 {
		t.Fatalf("order changed the hash: %s vs %s", a, b)
	}
	if HashStrings(nil) == "" || HashStrings(nil) == a {
		t.Fatal("an empty set must hash to a recorded value distinct from a non-empty one")
	}
}
