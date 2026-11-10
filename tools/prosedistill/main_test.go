package main

// Deterministic unit oracles for the transform itself, over throwaway fixture roots — each one exists
// to prove a refusal CAN fire (a floor that cannot go red is decoration, the lesson this repo keeps
// re-learning). Estate-shaped fixture tokens are assembled from split literals so no committed source
// line itself matches what the floor (or the public-mirror gate) scans for.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodBody = `## Goal
Say the one thing this fixture is for.

## Required evidence
- A named read.

## Decision rules
- One rule.

## Verification
- One named post-fix observation.
`

func manifestWith(entries string) string {
	return `{"readme":[],"entries":[` + entries + `]}`
}

const goodEntry = `{"source_path":"src/a/SKILL.md","disposition":"distilled","target_name":"sample","class":"skill","description":"A fixture artifact for the transform oracles"}`

func fixtureRoot(t *testing.T, manifest string, bodies map[string]string) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "github-sync"), 0o755))
	deny := "# fixture denylist — one comment, one blank, one pattern\n\nfixturetoken[0-9]{2}\n"
	must(os.WriteFile(filepath.Join(root, "github-sync", "denylist.txt"), []byte(deny), 0o644))
	must(os.MkdirAll(filepath.Join(root, "tools", "prosedistill", "distilled"), 0o755))
	must(os.WriteFile(filepath.Join(root, "tools", "prosedistill", "manifest.json"), []byte(manifest), 0o644))
	for name, body := range bodies {
		must(os.WriteFile(filepath.Join(root, "tools", "prosedistill", "distilled", name+".md"), []byte(body), 0o644))
	}
	return root
}

func TestManifestValidationRefusesIllegalShapes(t *testing.T) {
	cases := []struct {
		name    string
		entries string
		want    string
	}{
		{"unknown disposition",
			`{"source_path":"s/SKILL.md","disposition":"ported","notes":"x"}`,
			"closed set {distilled, merged, skipped}"},
		{"merged without existing target",
			`{"source_path":"s/SKILL.md","disposition":"merged","target_name":"ghost","notes":"x"}`,
			"must name an existing distilled target"},
		{"skipped without reason",
			`{"source_path":"s/SKILL.md","disposition":"skipped"}`,
			"must carry the reason"},
		{"skipped carrying class",
			`{"source_path":"s/SKILL.md","disposition":"skipped","class":"skill","notes":"x"}`,
			"a skip produces nothing"},
		{"class outside the vocabulary",
			`{"source_path":"s/SKILL.md","disposition":"distilled","target_name":"x","class":"prompt","description":"d"}`,
			"closed set {skill, runbook}"},
		{"description with a colon",
			`{"source_path":"s/SKILL.md","disposition":"distilled","target_name":"x","class":"skill","description":"a: b"}`,
			"may not contain"},
		{"absolute source path",
			`{"source_path":"/etc/SKILL.md","disposition":"distilled","target_name":"x","class":"skill","description":"d"}`,
			"must be RELATIVE"},
		{"duplicate target name",
			goodEntry + `,{"source_path":"src/b/SKILL.md","disposition":"distilled","target_name":"sample","class":"skill","description":"d"}`,
			"one production artifact per name"},
		{"duplicate source path",
			goodEntry + `,` + goodEntry,
			"duplicate source_path"},
		{"unknown field",
			`{"source_path":"s/SKILL.md","disposition":"skipped","notes":"x","extra":"y"}`,
			"unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t, manifestWith(tc.entries), map[string]string{"sample": goodBody, "x": goodBody})
			_, err := loadManifest(root)
			if err == nil {
				t.Fatalf("validation accepted an illegal manifest shape")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the refusal %q", err, tc.want)
			}
		})
	}
}

func TestFloorRefusesEstateTokens(t *testing.T) {
	// Assembled at runtime; the source literals here never themselves form an estate token.
	hostToken := "nll" + "ei01pve"
	cases := []struct {
		name  string
		plant string
		want  string
	}{
		{"builtin host prefix", "Check " + hostToken + " for load.", "builtin:"},
		{"builtin private subnet", "The address 192.168." + "44.7 answers.", "builtin:"},
		{"denylist pattern", "See fixturetoken" + "42 for details.", "denylist:fixturetoken[0-9]{2}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(goodBody, "- One rule.", "- One rule. "+tc.plant, 1)
			root := fixtureRoot(t, manifestWith(goodEntry), map[string]string{"sample": body})
			m, err := loadManifest(root)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			_, err = composeArtifacts(root, m)
			if err == nil {
				t.Fatalf("floor passed a planted estate token — the refusal cannot fire")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "sample") {
				t.Fatalf("refusal %q does not name the pattern class %q and the offending file", err, tc.want)
			}
		})
	}
}

func TestStructureEnforced(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing Verification", strings.Replace(goodBody, "## Verification", "## Postscript", 1), "exactly once"},
		{"does not open with Goal", "Intro line first.\n\n" + goodBody, "must OPEN with"},
		{"section out of order", strings.NewReplacer("## Required evidence", "## Decision rules", "## Decision rules\n- One rule.", "## Required evidence\n- A named read.").Replace(goodBody), "out of order"},
		{"own frontmatter", "---\nname: sneaky\n---\n" + goodBody, "carries its own frontmatter"},
		{"empty body", "   \n", "body is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t, manifestWith(goodEntry), map[string]string{"sample": tc.body})
			m, err := loadManifest(root)
			if err != nil {
				t.Fatalf("manifest: %v", err)
			}
			if _, err = composeArtifacts(root, m); err == nil {
				t.Fatalf("structure gate accepted a malformed body")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the defect %q", err, tc.want)
			}
		})
	}
}

func TestVerifyCatchesDriftStraysAndLoss(t *testing.T) {
	root := fixtureRoot(t, manifestWith(goodEntry), map[string]string{"sample": goodBody})
	m, err := loadManifest(root)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	artifacts, err := composeArtifacts(root, m)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	again, err := composeArtifacts(root, m)
	if err != nil {
		t.Fatalf("recompose: %v", err)
	}
	for rel := range artifacts {
		if string(artifacts[rel]) != string(again[rel]) {
			t.Fatalf("transform is not deterministic for %s", rel)
		}
	}
	if err := writeTree(root, artifacts); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectProblems := func(want string) {
		t.Helper()
		problems, err := verifyTree(root, artifacts)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if want == "" {
			if len(problems) != 0 {
				t.Fatalf("verify reported problems on a clean tree: %v", problems)
			}
			return
		}
		joined := strings.Join(problems, "\n")
		if !strings.Contains(joined, want) {
			t.Fatalf("verify problems %q do not name %q", joined, want)
		}
	}
	expectProblems("")

	target := filepath.Join(root, "skills", "skill", "sample", "SKILL.md")
	if err := os.WriteFile(target, append(artifacts["skills/skill/sample/SKILL.md"], '!'), 0o644); err != nil {
		t.Fatal(err)
	}
	expectProblems("DRIFT")
	if err := os.WriteFile(target, artifacts["skills/skill/sample/SKILL.md"], 0o644); err != nil {
		t.Fatal(err)
	}
	expectProblems("")

	stray := filepath.Join(root, "skills", "skill", "rogue.md")
	if err := os.WriteFile(stray, []byte("uninvited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectProblems("STRAY")
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	expectProblems("MISSING")
}
