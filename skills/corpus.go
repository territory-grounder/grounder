// Package skillcorpus embeds the repo's runbook corpus (skills/runbook/*/SKILL.md) so the WORKER can seed
// it into the skill store at boot (TG-529). Before this package the tree below was reachable NOWHERE at
// runtime — the README's "THIS TREE IS INERT" was the whole story, and every merged runbook pack (TG-85
// Cisco, TG-78 K8s) silently stopped at the repo boundary: store rows existed only when an operator
// remembered the two manual tools (seedskills --execute, then promoterunbooks --execute).
//
// SCOPE: runbook class ONLY. Runbooks never compose into the agent seed (REQ-1316, TestRunbookNeverComposes)
// — their single venue is the console wiki, which lists production rows — so boot-seeding them straight to
// production (owner-ruled 2026-08-22: auto-promote; the operator-promotion gate is dead under
// owner-attended=never) changes display-surface content and nothing in the agent's behavior. The skill/
// half of the tree stays with the manual draft→trial→graduate lane: those rows DO shape agent competence
// and keep their governance.
package skillcorpus

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed runbook/*/SKILL.md
var runbookFS embed.FS

// PackageFiles names every HAND-WRITTEN file this package keeps inside the otherwise fully-generated
// skills/ tree. Go's embed cannot cross package boundaries, so the embed package MUST live beside the
// data it embeds — and the tree's two drift guards (tools/prosedistill verifyTree, tools/seedskills'
// corpus walker) exempt exactly these paths and nothing else. Adding a file here without listing it reds
// both gates: the guards stay strict about generated-content drift while admitting the one resident that
// makes the corpus reachable at runtime (TG-529).
var PackageFiles = []string{"skills/corpus.go", "skills/corpus_test.go"}

// Runbook is one embedded corpus entry, parsed from its SKILL.md frontmatter.
type Runbook struct {
	Name        string // frontmatter `name`, must equal the directory name
	Version     string // frontmatter `version` (e.g. 0.1.0-distilled)
	Description string // frontmatter `description`
	Body        string // the document below the frontmatter
}

// Runbooks returns every embedded runbook, sorted by name, or an error naming the FIRST malformed entry —
// a corpus that cannot be fully parsed refuses whole rather than seeding a silent subset (the no-silent-
// sampling rule: a partial seed would read as "the corpus is delivered" while dropping packs invisibly).
func Runbooks() ([]Runbook, error) {
	dirs, err := fs.ReadDir(runbookFS, "runbook")
	if err != nil {
		return nil, fmt.Errorf("skillcorpus: read embedded corpus: %w", err)
	}
	out := make([]Runbook, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(runbookFS, "runbook/"+d.Name()+"/SKILL.md")
		if err != nil {
			return nil, fmt.Errorf("skillcorpus: %s: %w", d.Name(), err)
		}
		rb, err := parse(d.Name(), string(raw))
		if err != nil {
			return nil, err
		}
		out = append(out, rb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parse splits the `---` frontmatter block and validates the fields the seed depends on. Strict on the
// invariants (name matches the directory, class is runbook, version present, body non-empty) and
// indifferent to everything else — the authoring-side validator (tools/seedskills) owns the full contract.
func parse(dir, raw string) (Runbook, error) {
	rest, ok := strings.CutPrefix(raw, "---\n")
	if !ok {
		return Runbook{}, fmt.Errorf("skillcorpus: %s: SKILL.md has no frontmatter", dir)
	}
	fmBlock, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Runbook{}, fmt.Errorf("skillcorpus: %s: unterminated frontmatter", dir)
	}
	fm := map[string]string{}
	for _, line := range strings.Split(fmBlock, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	rb := Runbook{Name: fm["name"], Version: fm["version"], Description: fm["description"], Body: strings.TrimSpace(body)}
	switch {
	case rb.Name != dir:
		return Runbook{}, fmt.Errorf("skillcorpus: %s: frontmatter name %q does not match the directory", dir, rb.Name)
	case fm["class"] != "runbook":
		return Runbook{}, fmt.Errorf("skillcorpus: %s: class %q — this package embeds ONLY the runbook corpus", dir, fm["class"])
	case rb.Version == "":
		return Runbook{}, fmt.Errorf("skillcorpus: %s: frontmatter carries no version", dir)
	case rb.Body == "":
		return Runbook{}, fmt.Errorf("skillcorpus: %s: empty body", dir)
	}
	return rb, nil
}
