package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pure-Go half of the seeding-rail proof (constraint D5: acceptance runs without Postgres).
// The corpus fixtures here are BOTH the real committed tree (the artifact the tool exists for —
// the test set is derived from the artifact, not from imagination) and synthetic minimal trees
// for each refusal oracle, every one of which is EXECUTED red, not asserted by inspection.
// The pgx path — including the TG-489 chain — is proven in realstore_test.go.

// memSeedStore adapts skillstore.MemStore to the seeder's Store surface and counts writes, so
// the dry-run-writes-nothing property is an executed assertion, not a reading of the code.
type memSeedStore struct {
	m      *skillstore.MemStore
	writes int
}

func newMemSeedStore() *memSeedStore { return &memSeedStore{m: skillstore.NewMemStore()} }

func (s *memSeedStore) State(ctx context.Context, name string) (StoreState, error) {
	sk, err := s.m.GetSkill(ctx, name)
	if errors.Is(err, skillstore.ErrNotFound) {
		return StoreState{}, nil
	}
	if err != nil {
		return StoreState{}, err
	}
	st := StoreState{Found: true, Identity: sk}
	for _, v := range s.m.VersionsOf(name) {
		st.Versions = append(st.Versions, StoredVersion{Version: v.Version, ContentHash: v.ContentHash, Source: v.Source})
	}
	return st, nil
}

func (s *memSeedStore) PutSkill(_ context.Context, sk skillstore.Skill) error {
	s.writes++
	s.m.PutSkill(sk)
	return nil
}

func (s *memSeedStore) CreateVersion(ctx context.Context, v skillstore.Version) (skillstore.Version, error) {
	s.writes++
	return s.m.CreateVersion(ctx, v)
}

// repoRoot finds the module root (the tests run from the package directory).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test working directory")
		}
		dir = parent
	}
}

// composeSkillMD mirrors prosedistill's generated shape byte-for-byte.
func composeSkillMD(name, class, version, source, description, body string) string {
	return fmt.Sprintf("---\nname: %s\nclass: %s\nversion: %s\nsource: %s\ndescription: %s\n---\n\n%s\n",
		name, class, version, source, description, body)
}

// writeCorpus builds a synthetic repo root carrying a manifest and a skills/ tree.
func writeCorpus(t *testing.T, entries []manifestEntry, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	m := manifest{Readme: []string{"synthetic test corpus"}, Entries: entries}
	raw, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tools", "prosedistill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "prosedistill", "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func entry(name, class, desc, sourcePath string) manifestEntry {
	return manifestEntry{SourcePath: sourcePath, Disposition: "distilled", TargetName: name, Class: class, Description: desc}
}

// oneArtifactCorpus is the minimal agreeing corpus most refusal oracles mutate from.
func oneArtifactCorpus(t *testing.T, body string) (string, manifestEntry) {
	e := entry("probe-artifact", "skill", "a synthetic probe artifact", "src/probe.md")
	root := writeCorpus(t, []manifestEntry{e}, map[string]string{
		"skills/skill/probe-artifact/SKILL.md": composeSkillMD(
			"probe-artifact", "skill", "0.1.0-test", "distill:src/probe.md", "a synthetic probe artifact", body),
	})
	return root, e
}

// ---------------------------------------------------------------------------------------------------
// The real committed corpus.
// ---------------------------------------------------------------------------------------------------

// TestRealCorpusLoads grounds the tool against the artifact it exists for: the committed tree
// must load as exactly the manifest's distilled set — 48 artifacts, 12 skill + 36 runbook (batch 5,
// TG-78, added 6 Kubernetes runbook entries on top of batch 4's 38) — every one unscoped (no
// manifest entry carries a predicate today), hash-bound, and provenanced.
func TestRealCorpusLoads(t *testing.T) {
	arts, err := loadCorpus(repoRoot(t))
	if err != nil {
		t.Fatalf("the committed corpus must load: %v", err)
	}
	per := map[skillstore.ArtifactClass]int{}
	for _, a := range arts {
		per[a.Class]++
		if !predicateEmpty(a.AppliesWhen) {
			t.Errorf("%s: seeded predicate must be EMPTY (unscoped draft policy) — got %+v", a.Name, a.AppliesWhen)
		}
		if !strings.HasPrefix(a.Source, "distill:") {
			t.Errorf("%s: source %q lacks distill: provenance", a.Name, a.Source)
		}
		if a.ContentHash != skillstore.ContentHash(a.Body, a.AppliesWhen) {
			t.Errorf("%s: content hash does not bind body+predicate", a.Name)
		}
		if a.Version == "" || strings.Contains(a.Body, "---\nname:") {
			t.Errorf("%s: frontmatter leaked into the body or version missing", a.Name)
		}
	}
	if len(arts) != 48 || per[skillstore.ClassSkill] != 12 || per[skillstore.ClassRunbook] != 36 {
		t.Fatalf("corpus shape drifted: got %d artifacts (%d skill, %d runbook), the manifest truth is 48 = 12+36",
			len(arts), per[skillstore.ClassSkill], per[skillstore.ClassRunbook])
	}
}

// TestSeedRealCorpusIntoMemStoreIsIdempotent executes the whole rail twice against the store
// fake: first run creates all 48 drafts through the store's own governed gate (ValidateDraft),
// second run is a byte-level no-op — zero writes, everything skip-identical.
func TestSeedRealCorpusIntoMemStoreIsIdempotent(t *testing.T) {
	ctx := context.Background()
	arts, err := loadCorpus(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	st := newMemSeedStore()

	p, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	c, err := apply(ctx, p, st)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if c.Created != 48 || c.Skipped != 0 {
		t.Fatalf("first run must create all 48: %+v", c)
	}

	// The rows carry the contract: draft status, tool author, distill provenance, the fixed
	// rationale citing the corpus tickets, and the class→kind mapping.
	for _, probe := range []struct {
		name  string
		class skillstore.ArtifactClass
		kind  string
	}{
		{"alert-queue-review", skillstore.ClassSkill, "behavioral"},
		{"incident-lifecycle", skillstore.ClassRunbook, "catalog"},
	} {
		sk, err := st.m.GetSkill(ctx, probe.name)
		if err != nil {
			t.Fatalf("%s identity: %v", probe.name, err)
		}
		if sk.Class != probe.class || sk.Kind != probe.kind || sk.Pinned || sk.Description == "" {
			t.Errorf("%s identity wrong: %+v", probe.name, sk)
		}
		vs := st.m.VersionsOf(probe.name)
		if len(vs) != 1 {
			t.Fatalf("%s: want 1 version, got %d", probe.name, len(vs))
		}
		v := vs[0]
		if v.Status != skillstore.StatusDraft || v.Author != seedAuthor || !strings.HasPrefix(v.Source, "distill:") {
			t.Errorf("%s row contract broken: status=%s author=%s source=%s", probe.name, v.Status, v.Author, v.Source)
		}
		for _, cite := range []string{"TG-36", "TG-477", "TG-478", "TG-479", "ADR-0012", v.Source} {
			if !strings.Contains(v.Rationale, cite) {
				t.Errorf("%s rationale misses %q: %s", probe.name, cite, v.Rationale)
			}
		}
	}

	// Second run: idempotent no-op, proven by the write counter, not by inspection.
	before := st.writes
	p2, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := apply(ctx, p2, st)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if c2.Created != 0 || c2.Skipped != 48 {
		t.Fatalf("re-run must skip all 48 identical: %+v", c2)
	}
	if st.writes != before {
		t.Fatalf("re-run wrote %d times — the rail is not idempotent", st.writes-before)
	}
}

// TestPlanIsReadOnly: planning — the whole dry-run — performs zero writes by executed count.
func TestPlanIsReadOnly(t *testing.T) {
	ctx := context.Background()
	arts, err := loadCorpus(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	st := newMemSeedStore()
	if _, err := buildPlan(ctx, arts, st); err != nil {
		t.Fatal(err)
	}
	if st.writes != 0 {
		t.Fatalf("buildPlan wrote %d times — a dry-run must write NOTHING", st.writes)
	}
}

// ---------------------------------------------------------------------------------------------------
// Refusal oracles — each executed red.
// ---------------------------------------------------------------------------------------------------

func mustRefuse(t *testing.T, root, fragment string) {
	t.Helper()
	arts, err := loadCorpus(root)
	if err == nil {
		t.Fatalf("corpus must refuse (want %q), but loaded %d artifacts", fragment, len(arts))
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("refusal must name the defect %q, got: %v", fragment, err)
	}
}

func TestStrayFileRefusesTheWholeRun(t *testing.T) {
	root, _ := oneArtifactCorpus(t, "probe body")
	stray := filepath.Join(root, "skills", "skill", "rogue", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte(composeSkillMD("rogue", "skill", "0.1.0", "distill:x", "rogue", "body")), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, root, "STRAY")
}

func TestMissingFileRefusesTheWholeRun(t *testing.T) {
	e := entry("ghost", "runbook", "a manifest entry with no file", "src/ghost.md")
	root := writeCorpus(t, []manifestEntry{e}, map[string]string{"skills/README.md": "# banner"})
	mustRefuse(t, root, "MISSING")
}

func TestFrontmatterClassDisagreementRefuses(t *testing.T) {
	e := entry("probe-artifact", "skill", "a synthetic probe artifact", "src/probe.md")
	root := writeCorpus(t, []manifestEntry{e}, map[string]string{
		// The file sits where the manifest expects it, but its frontmatter claims runbook.
		"skills/skill/probe-artifact/SKILL.md": composeSkillMD(
			"probe-artifact", "runbook", "0.1.0-test", "distill:src/probe.md", "a synthetic probe artifact", "body"),
	})
	mustRefuse(t, root, "disagrees with the manifest")
}

func TestUnknownClassRefuses(t *testing.T) {
	// prompt IS a real store class — and exactly the kind this corpus must never mint. The
	// seeder's vocabulary is the distillation batches' {skill, runbook}, closed.
	e := entry("sneaky", "prompt", "a class outside the distillation set", "src/sneaky.md")
	root := writeCorpus(t, []manifestEntry{e}, map[string]string{})
	mustRefuse(t, root, "outside the closed {skill, runbook} set")
}

func TestOversizeBodyRefusesWithTheStoresOwnCap(t *testing.T) {
	root, _ := oneArtifactCorpus(t, strings.Repeat("x", skillstore.MaxBodyBytes(skillstore.ClassSkill)+1))
	mustRefuse(t, root, fmt.Sprintf("1..%d", skillstore.MaxBodyBytes(skillstore.ClassSkill)))
}

// TestOversizeBodySurfacesTheStoreRefusalAtWrite proves the belt behind the suspenders: even an
// artifact that somehow bypassed loadCorpus is refused by the STORE's own gate at CreateVersion,
// and apply surfaces that named error (skillstore.ErrBodyBounds) instead of laundering it.
func TestOversizeBodySurfacesTheStoreRefusalAtWrite(t *testing.T) {
	ctx := context.Background()
	body := strings.Repeat("x", skillstore.MaxBodyBytes(skillstore.ClassSkill)+1)
	a := Artifact{
		Name: "oversize-bypass", Class: skillstore.ClassSkill, Version: "0.1.0-test",
		Source: "distill:src/oversize.md", Description: "bypass probe", Body: body,
		ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{}), Position: 100,
	}
	st := newMemSeedStore()
	_, err := apply(ctx, Plan{Items: []PlanItem{{Artifact: a, Action: ActionCreate}}, StoreAware: true}, st)
	if !errors.Is(err, skillstore.ErrBodyBounds) {
		t.Fatalf("the store's own cap must surface as ErrBodyBounds, got: %v", err)
	}
}

func TestPinnedIdentityRefusesBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	root, _ := oneArtifactCorpus(t, "probe body")
	arts, err := loadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemSeedStore()
	st.m.PutSkill(skillstore.Skill{Name: "probe-artifact", Kind: "behavioral", Pinned: true, Class: skillstore.ClassSkill, Position: 1})
	st.writes = 0

	p, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	if p.Items[0].Action != ActionRefuse || !strings.Contains(p.Items[0].Reason, "PINNED") {
		t.Fatalf("pinned identity must refuse in the plan: %+v", p.Items[0])
	}
	if _, err := apply(ctx, p, st); err == nil {
		t.Fatal("apply over a refusing plan must error")
	}
	if st.writes != 0 {
		t.Fatalf("a refused run wrote %d times — refusals must write NOTHING", st.writes)
	}
}

func TestForeignOwnerIdentityRefuses(t *testing.T) {
	ctx := context.Background()
	root, _ := oneArtifactCorpus(t, "probe body")
	arts, err := loadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemSeedStore()
	// The name exists with compiled-import provenance — another writer's artifact.
	st.m.PutSkill(skillstore.Skill{Name: "probe-artifact", Kind: "behavioral", Class: skillstore.ClassSkill, Position: 1})
	if _, err := st.m.CreateVersion(ctx, skillstore.Version{
		SkillName: "probe-artifact", Version: "9.9.9", Body: "compiled body",
		ContentHash: skillstore.ContentHash("compiled body", skillstore.AppliesWhen{}),
		Author:      "compiled", Source: "compiled-import", Rationale: "[production] compiled registry boot import",
	}); err != nil {
		t.Fatal(err)
	}
	p, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	if p.Items[0].Action != ActionRefuse || !strings.Contains(p.Items[0].Reason, "another writer") {
		t.Fatalf("foreign-owned name must refuse: %+v", p.Items[0])
	}
}

func TestSameVersionDifferentHashRefuses(t *testing.T) {
	ctx := context.Background()
	rootV1, _ := oneArtifactCorpus(t, "the reviewed body")
	arts, err := loadCorpus(rootV1)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemSeedStore()
	p, err := buildPlan(ctx, arts, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, p, st); err != nil {
		t.Fatal(err)
	}
	// The tree changes but the version string does not — the drift this oracle exists to catch.
	rootV2, _ := oneArtifactCorpus(t, "an EDITED body under the same version stamp")
	arts2, err := loadCorpus(rootV2)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := buildPlan(ctx, arts2, st)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Items[0].Action != ActionRefuse || !strings.Contains(p2.Items[0].Reason, "DIFFERENT content hash") {
		t.Fatalf("same-version different-hash must refuse: %+v", p2.Items[0])
	}
}

// ---------------------------------------------------------------------------------------------------
// The applies_when policy.
// ---------------------------------------------------------------------------------------------------

// TestManifestPredicateRidesTheDraft: the policy's escape hatch — a manifest entry MAY carry a
// predicate; it must validate against the store's closed vocabulary, bind into the content hash,
// and change the rationale's scoping sentence.
func TestManifestPredicateRidesTheDraft(t *testing.T) {
	e := entry("scoped-probe", "skill", "a predicate-scoped probe", "src/scoped.md")
	e.Predicate = &skillstore.AppliesWhen{Phases: []string{"investigate"}}
	root := writeCorpus(t, []manifestEntry{e}, map[string]string{
		"skills/skill/scoped-probe/SKILL.md": composeSkillMD(
			"scoped-probe", "skill", "0.1.0-test", "distill:src/scoped.md", "a predicate-scoped probe", "body"),
	})
	arts, err := loadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	a := arts[0]
	if len(a.AppliesWhen.Phases) != 1 || a.AppliesWhen.Phases[0] != "investigate" {
		t.Fatalf("predicate did not ride: %+v", a.AppliesWhen)
	}
	if a.ContentHash != skillstore.ContentHash(a.Body, a.AppliesWhen) ||
		a.ContentHash == skillstore.ContentHash(a.Body, skillstore.AppliesWhen{}) {
		t.Fatal("the content hash must bind the predicate")
	}
	if !strings.Contains(rationaleFor(a), "carried from the manifest") {
		t.Fatalf("rationale must state the predicate provenance: %s", rationaleFor(a))
	}
}

func TestInvalidManifestPredicateRefuses(t *testing.T) {
	e := entry("bad-scope", "skill", "an out-of-vocabulary predicate", "src/bad.md")
	e.Predicate = &skillstore.AppliesWhen{Phases: []string{"daydream"}}
	root := writeCorpus(t, []manifestEntry{e}, map[string]string{
		"skills/skill/bad-scope/SKILL.md": composeSkillMD(
			"bad-scope", "skill", "0.1.0-test", "distill:src/bad.md", "an out-of-vocabulary predicate", "body"),
	})
	mustRefuse(t, root, "closed vocabulary")
}

// TestSkipIsHashKeyedNotVersionKeyed: identical content already stored under ANY version string
// of the name skips — the idempotency key is the ContentHash, exactly as the rail spec states.
func TestSkipIsHashKeyedNotVersionKeyed(t *testing.T) {
	body := "stable body"
	a := Artifact{Name: "n", Class: skillstore.ClassSkill, Version: "0.2.0",
		Body: body, ContentHash: skillstore.ContentHash(body, skillstore.AppliesWhen{})}
	act, reason := decide(a, StoreState{Found: true,
		Identity: skillstore.Skill{Name: "n", Class: skillstore.ClassSkill},
		Versions: []StoredVersion{{Version: "0.1.0", ContentHash: a.ContentHash, Source: "distill:src/n.md"}}})
	if act != ActionSkip {
		t.Fatalf("identical hash at an older version string must skip, got %s (%s)", act, reason)
	}
}
