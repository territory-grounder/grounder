// Command prosedistill composes the distilled prose-artifact drafts (TG-477 batch 1; TG-478 + TG-479
// batches 2-3; epic TG-114 C-8): the predecessor's prose corpus — skill library, specialist agent
// definitions, and runbooks — re-authored per ADR-0012, landed as an inert content tree under
// skills/<class>/<target_name>/SKILL.md.
//
// WHAT THE PIPELINE IS — AND DELIBERATELY IS NOT. ADR-0012 rejected the verbatim port: predecessor prose
// carries estate constants and tool idioms that must not survive into a product, and a mechanical
// regex-rewrite of that prose would be the rejected port wearing placeholders. So the RE-AUTHORING is an
// authored act, reviewed like code: the distilled bodies live as this tool's committed input corpus
// (tools/prosedistill/distilled/<target_name>.md), and the manifest (tools/prosedistill/manifest.json)
// is the honest inventory — every predecessor source appears with a disposition (distilled / merged /
// skipped) and a reason. What the MACHINE then enforces is everything a machine can prove:
//
//   - the SANITIZER FLOOR — no estate token (the public-mirror denylist github-sync/denylist.txt, plus a
//     built-in minimal pattern set) survives into any input or composed artifact; a hit is a refusal,
//     never a silent rewrite, the same abort-on-survivor shape as the mirror gate itself;
//   - the ADR-0012 STRUCTURE — frontmatter (name, class, version, source, description) plus the
//     Goal / Required evidence / Decision rules / Verification body sections, present and in order;
//   - PROVENANCE — every artifact's frontmatter carries source: distill:<relative-source-path>, and with
//     --source-root the tool asserts each manifest source actually exists in the predecessor checkout;
//   - IDEMPOTENCY — the committed skills/ tree is exactly what the transform produces, nothing more
//     (strays are refused) and nothing less; --verify re-runs the transform in memory and diffs, with no
//     writes, no sources, no network, no subprocess (INV-02) — so CI can hold the proof.
//
// THE TREE IS INERT BY CONSTRUCTION, AND SEEDING IS DELIBERATELY NOT HERE. Nothing at runtime reads
// skills/ — the agent's live competence is the compiled seed library (agent/skills/, INV-08 selector),
// and the skill store's rows are written only through its governed write path. This batch lands as FILES
// ONLY so the distilled CONTENT gets owner review first; the idempotent DB seeding of draft rows through
// the store write path is a LATER wire, after that review passes. No draft rows exist, so no eval or
// waiver question arises from the content itself. Prose becomes behavior only through that seeding/
// graduation rail, and the eval gate binds THERE — which is why skills/ was removed from the merge-time
// eval behavior_re on 2026-08-14 (owner ruling: a red pipeline on inert files is not a control).
//
// Usage:
//
//	go run ./tools/prosedistill                      # compose and write skills/ (build mode)
//	go run ./tools/prosedistill --verify             # CI idempotency + floor proof; writes nothing
//	go run ./tools/prosedistill --source-root <dir>  # additionally assert every manifest source exists
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	skillcorpus "github.com/territory-grounder/grounder/skills"
)

// artifactVersion is the fixed version stamped on every distilled draft. Deterministic on purpose:
// --verify diffs bytes, so nothing composed may depend on when the tool ran.
const artifactVersion = "0.1.0-distilled"

// Manifest is the committed inventory of every predecessor source and its disposition.
type Manifest struct {
	Readme  []string `json:"readme"`
	Entries []Entry  `json:"entries"`
}

// Entry is one predecessor source file and what became of it.
type Entry struct {
	SourcePath  string `json:"source_path"`
	Disposition string `json:"disposition"`
	TargetName  string `json:"target_name,omitempty"`
	Class       string `json:"class,omitempty"`
	Description string `json:"description,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

var (
	targetNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	classes      = map[string]bool{"skill": true, "runbook": true}
)

// floorPattern is one refusal pattern of the sanitizer floor, with the label reported on a hit.
type floorPattern struct {
	label string
	re    *regexp.Regexp
}

// builtinFloor returns the hard-coded minimal estate-token set. It backstops the committed denylist:
// even if github-sync/denylist.txt were emptied, these still refuse. Patterns are case-insensitive
// (the mirror denylist is deliberately case-exact per its own shapes; a draft body has no legitimate
// use for ANY casing of these). The domain literal is assembled at runtime so this source file never
// itself carries a token the mirror gate scans for — the same split-literal convention as the PEM and
// credential-shape fixtures (see scripts/lint-forbidden.sh).
func builtinFloor() []floorPattern {
	domain := "nuclear" + "lighters"
	specs := []string{
		`(?i)nllei`,
		`(?i)grskg`,
		`192\.168\.`,
		`10\.\d+\.\d+\.\d+`,
		`(?i)` + domain,
	}
	pats := make([]floorPattern, 0, len(specs))
	for _, s := range specs {
		pats = append(pats, floorPattern{label: "builtin:" + s, re: regexp.MustCompile(s)})
	}
	return pats
}

// loadFloorPatterns returns the full sanitizer floor: the committed mirror denylist plus the built-in
// set. A denylist line that does not compile is a HARD error — a floor with a silently dropped pattern
// is thinner than it claims, which is the fail-open shape this repo refuses.
func loadFloorPatterns(root string) ([]floorPattern, error) {
	path := filepath.Join(root, "github-sync", "denylist.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sanitizer floor: cannot read the mirror denylist: %w", err)
	}
	var pats []floorPattern
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("sanitizer floor: denylist line %d (%q) does not compile as RE2: %v — the floor must not run thinner than the mirror gate", i+1, line, err)
		}
		pats = append(pats, floorPattern{label: "denylist:" + line, re: re})
	}
	if len(pats) == 0 {
		return nil, fmt.Errorf("sanitizer floor: the mirror denylist %s yielded zero patterns — refusing to run with half a floor", path)
	}
	return append(pats, builtinFloor()...), nil
}

// scanBytes reports every floor violation in data as "rel:line: matched <label>".
func scanBytes(rel string, data []byte, pats []floorPattern) []string {
	var hits []string
	for i, line := range strings.Split(string(data), "\n") {
		for _, p := range pats {
			if p.re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("%s:%d: matched %s", rel, i+1, p.label))
			}
		}
	}
	return hits
}

// loadManifest reads and validates the committed manifest. Validation is closed-world: unknown fields,
// unknown dispositions, and field combinations outside the three legal shapes are refusals, so the
// manifest cannot quietly grow a fourth meaning.
func loadManifest(root string) (*Manifest, error) {
	path := filepath.Join(root, "tools", "prosedistill", "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %v", err)
	}
	if len(m.Entries) == 0 {
		return nil, fmt.Errorf("manifest: zero entries — an empty inventory is not an inventory")
	}

	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	distilledTargets := map[string]bool{}
	seenSource := map[string]bool{}
	for _, e := range m.Entries {
		if e.Disposition == "distilled" {
			distilledTargets[e.TargetName] = true
		}
	}
	for i, e := range m.Entries {
		at := fmt.Sprintf("entry %d (%s)", i+1, e.SourcePath)
		switch {
		case e.SourcePath == "":
			fail("entry %d: empty source_path", i+1)
		case strings.HasPrefix(e.SourcePath, "/") || strings.Contains(e.SourcePath, ".."):
			fail("%s: source_path must be RELATIVE to --source-root with no traversal — machine-local absolute paths never enter the repo", at)
		case seenSource[e.SourcePath]:
			fail("%s: duplicate source_path — each source has exactly one disposition", at)
		}
		seenSource[e.SourcePath] = true

		switch e.Disposition {
		case "distilled":
			if !targetNameRe.MatchString(e.TargetName) {
				fail("%s: target_name %q must match %s", at, e.TargetName, targetNameRe)
			}
			if !classes[e.Class] {
				fail("%s: class %q is not in the closed set {skill, runbook}", at, e.Class)
			}
			if err := checkDescription(e.Description); err != nil {
				fail("%s: %v", at, err)
			}
		case "merged":
			if e.TargetName == "" || !distilledTargets[e.TargetName] {
				fail("%s: merged entry must name an existing distilled target_name (got %q)", at, e.TargetName)
			}
			if e.Notes == "" {
				fail("%s: merged entry must say what judgment moved and where", at)
			}
			if e.Class != "" || e.Description != "" {
				fail("%s: merged entry carries class/description — those belong to its absorbing target", at)
			}
		case "skipped":
			if e.Notes == "" {
				fail("%s: skipped entry must carry the reason — an unexplained skip is a silent decision", at)
			}
			if e.TargetName != "" || e.Class != "" || e.Description != "" {
				fail("%s: skipped entry carries target/class/description — a skip produces nothing", at)
			}
		default:
			fail("%s: disposition %q is not in the closed set {distilled, merged, skipped}", at, e.Disposition)
		}
	}
	// One target, one artifact: two distilled entries may not claim the same name.
	counts := map[string]int{}
	for _, e := range m.Entries {
		if e.Disposition == "distilled" {
			counts[e.TargetName]++
		}
	}
	for name, n := range counts {
		if n > 1 {
			fail("target_name %q is claimed by %d distilled entries — one production artifact per name", name, n)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("manifest validation failed:\n  %s", strings.Join(errs, "\n  "))
	}
	return &m, nil
}

// checkDescription enforces the frontmatter-safe single-line shape: the description is emitted as a
// YAML plain scalar, so a colon or hash inside it would change what a frontmatter parser reads.
func checkDescription(d string) error {
	switch {
	case strings.TrimSpace(d) == "":
		return fmt.Errorf("description is empty")
	case strings.ContainsAny(d, ":#\n"):
		return fmt.Errorf("description %q may not contain ':', '#', or a newline (YAML plain-scalar safety)", d)
	case len(d) > 200:
		return fmt.Errorf("description exceeds 200 characters — it is a subtitle, not the body")
	}
	return nil
}

// requiredSections is the ADR-0012 body order every distilled artifact must carry.
var requiredSections = []string{"## Goal", "## Required evidence", "## Decision rules", "## Verification"}

// checkStructure enforces the ADR-0012 shape on a distilled body: it opens with Goal and carries all
// four sections, each exactly once, in order.
func checkStructure(name string, body []byte) error {
	text := string(body)
	first := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			first = strings.TrimSpace(line)
			break
		}
	}
	if first != requiredSections[0] {
		return fmt.Errorf("distilled/%s.md: body must OPEN with %q (found %q)", name, requiredSections[0], first)
	}
	last := -1
	for _, h := range requiredSections {
		marker := "\n" + h + "\n"
		padded := "\n" + text
		if n := strings.Count(padded, marker); n != 1 {
			return fmt.Errorf("distilled/%s.md: section %q must appear exactly once as its own line (found %d)", name, h, n)
		}
		at := strings.Index(padded, marker)
		if at <= last {
			return fmt.Errorf("distilled/%s.md: section %q is out of order — the ADR-0012 shape is Goal, Required evidence, Decision rules, Verification", name, h)
		}
		last = at
	}
	return nil
}

// compose renders one artifact: generated frontmatter over the authored body.
func compose(e Entry, body []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "name: %s\n", e.TargetName)
	fmt.Fprintf(&b, "class: %s\n", e.Class)
	fmt.Fprintf(&b, "version: %s\n", artifactVersion)
	fmt.Fprintf(&b, "source: distill:%s\n", e.SourcePath)
	fmt.Fprintf(&b, "description: %s\n", e.Description)
	fmt.Fprintf(&b, "---\n\n")
	b.Write(body)
	if !bytes.HasSuffix(body, []byte("\n")) {
		b.WriteString("\n")
	}
	return b.Bytes()
}

// composeReadme renders the tree's banner file. Generated with the tree so the banner can never say
// something the tree does not do.
func composeReadme(m *Manifest) []byte {
	perClass := map[string][]string{}
	for _, e := range m.Entries {
		if e.Disposition == "distilled" {
			perClass[e.Class] = append(perClass[e.Class], e.TargetName)
		}
	}
	var classNames []string
	for c := range perClass {
		classNames = append(classNames, c)
	}
	sort.Strings(classNames)

	var b bytes.Buffer
	b.WriteString("# skills/ — distilled prose-artifact drafts (TG-477, TG-478, TG-479, TG-85, TG-78)\n\n")
	b.WriteString("> Reference content, not a work queue — and GENERATED, not hand-edited. This tree is the\n")
	b.WriteString("> byte-exact output of `go run ./tools/prosedistill` from the committed manifest and the\n")
	b.WriteString("> re-authored bodies in `tools/prosedistill/distilled/`. Edit THOSE and re-run the tool;\n")
	b.WriteString("> `go run ./tools/prosedistill --verify` (and the package's tests) red on any drift or stray.\n\n")
	b.WriteString("Batches 1-3: the predecessor's prose corpus — skill library, specialist agents, runbooks —\n")
	b.WriteString("re-authored per ADR-0012 (format adopted, content re-authored, estate specifics stripped) into\n")
	b.WriteString("draft prose artifacts for the ADR-0017 store classes. Batches 4 (TG-85) and 5 (TG-78) depart\n")
	b.WriteString("from that: their entries are newly authored, grounded directly in vendor documentation rather\n")
	b.WriteString("than distilled from predecessor prose — see each entry's own manifest notes and the artifact's\n")
	b.WriteString("Doc basis section. The\n")
	b.WriteString("manifest — `tools/prosedistill/manifest.json` — is the honest inventory: every source, including\n")
	b.WriteString("every one NOT distilled, with its disposition and reason.\n\n")
	b.WriteString("THIS TREE IS INERT. Nothing at runtime reads it: the agent's live competence is the compiled\n")
	b.WriteString("seed library (`agent/skills/`), and skill-store rows are written only through the store's\n")
	b.WriteString("governed write path. Seeding these drafts as store rows is a separate, later wire — after the\n")
	b.WriteString("content itself passes owner review. Prose becomes behavior only through that store seeding/\n")
	b.WriteString("graduation rail, and the eval gate binds THERE (per-artifact admission + trial) — which is why\n")
	b.WriteString("`skills/` was removed from the merge-time eval `behavior_re` on 2026-08-14 (owner ruling): a red\n")
	b.WriteString("pipeline on inert files is not a control. The seeding wire, when built, carries the binding evals.\n\n")
	for _, c := range classNames {
		names := append([]string(nil), perClass[c]...)
		sort.Strings(names)
		fmt.Fprintf(&b, "- `%s/` (%d): %s\n", c, len(names), strings.Join(names, ", "))
	}
	return b.Bytes()
}

// composeArtifacts runs the whole transform in memory: manifest -> {relative path -> bytes}. It
// enforces the sanitizer floor over both the INPUTS (manifest, authored bodies) and the composed
// OUTPUTS, and the ADR-0012 structure over every distilled body. Any violation refuses the batch.
func composeArtifacts(root string, m *Manifest) (map[string][]byte, error) {
	pats, err := loadFloorPatterns(root)
	if err != nil {
		return nil, err
	}
	var problems []string

	manifestRaw, err := os.ReadFile(filepath.Join(root, "tools", "prosedistill", "manifest.json"))
	if err != nil {
		return nil, err
	}
	problems = append(problems, scanBytes("tools/prosedistill/manifest.json", manifestRaw, pats)...)

	out := map[string][]byte{}
	for _, e := range m.Entries {
		if e.Disposition != "distilled" {
			continue
		}
		bodyRel := filepath.Join("tools", "prosedistill", "distilled", e.TargetName+".md")
		body, err := os.ReadFile(filepath.Join(root, bodyRel))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: authored body missing for distilled target: %v", bodyRel, err))
			continue
		}
		if strings.TrimSpace(string(body)) == "" {
			problems = append(problems, fmt.Sprintf("%s: authored body is empty — an empty draft is not a draft", bodyRel))
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(body)), "---") {
			problems = append(problems, fmt.Sprintf("%s: authored body carries its own frontmatter — frontmatter is generated, once", bodyRel))
			continue
		}
		if err := checkStructure(e.TargetName, body); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		problems = append(problems, scanBytes(bodyRel, body, pats)...)

		rel := filepath.ToSlash(filepath.Join("skills", e.Class, e.TargetName, "SKILL.md"))
		composed := compose(e, body)
		problems = append(problems, scanBytes(rel, composed, pats)...)
		out[rel] = composed
	}
	readme := composeReadme(m)
	problems = append(problems, scanBytes("skills/README.md", readme, pats)...)
	out["skills/README.md"] = readme

	if len(problems) > 0 {
		return nil, fmt.Errorf("compose refused:\n  %s", strings.Join(problems, "\n  "))
	}
	return out, nil
}

// listTree returns every file currently under skills/, as slash-relative paths. A missing tree is an
// empty list (build mode creates it; verify mode then reports every artifact as missing).
func listTree(root string) ([]string, error) {
	base := filepath.Join(root, "skills")
	var files []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return files, err
}

// verifyTree diffs the committed tree against the in-memory transform: every composed artifact must
// exist byte-identically, and nothing else may live under skills/. Returns one problem per file.
func verifyTree(root string, artifacts map[string][]byte) ([]string, error) {
	var problems []string
	onDisk, err := listTree(root)
	if err != nil {
		return nil, err
	}
	// The ONE class of legitimate non-generated resident: the embed package's own files (TG-529 — Go's
	// embed cannot cross package boundaries, so skills/corpus.go must live in the tree it embeds). The
	// package DECLARES its residents; anything else is still drift.
	resident := map[string]bool{}
	for _, rel := range skillcorpus.PackageFiles {
		resident[rel] = true
	}
	disk := map[string]bool{}
	for _, rel := range onDisk {
		disk[rel] = true
		if resident[rel] {
			continue
		}
		if _, ok := artifacts[rel]; !ok {
			problems = append(problems, fmt.Sprintf("%s: STRAY — present in the tree but produced by no manifest entry", rel))
		}
	}
	var rels []string
	for rel := range artifacts {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if !disk[rel] {
			problems = append(problems, fmt.Sprintf("%s: MISSING — the manifest produces it but the tree does not carry it", rel))
			continue
		}
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, artifacts[rel]) {
			problems = append(problems, fmt.Sprintf("%s: DRIFT — committed bytes differ from the transform's output (edit tools/prosedistill/distilled/ and re-run, never the tree)", rel))
		}
	}
	return problems, nil
}

// writeTree writes every composed artifact under root.
func writeTree(root string, artifacts map[string][]byte) error {
	var rels []string
	for rel := range artifacts {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, artifacts[rel], 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote %s (%d bytes)\n", rel, len(artifacts[rel]))
	}
	return nil
}

// checkSources asserts every manifest source_path exists under sourceRoot — the provenance half of the
// pipeline, runnable only where the predecessor checkout is present (CI never needs it).
func checkSources(sourceRoot string, m *Manifest) []string {
	var problems []string
	for _, e := range m.Entries {
		p := filepath.Join(sourceRoot, filepath.FromSlash(e.SourcePath))
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			problems = append(problems, fmt.Sprintf("%s: source not found under --source-root (%v)", e.SourcePath, err))
		}
	}
	return problems
}

// findRepoRoot walks upward from the working directory to the module root, so the tool runs from any
// subdirectory of the repo and from `go test` package directories alike.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory — run from inside the repo")
		}
		dir = parent
	}
}

func run() error {
	verify := flag.Bool("verify", false, "re-run the transform in memory and diff against the committed skills/ tree; write nothing")
	sourceRoot := flag.String("source-root", "", "predecessor checkout root; when set, every manifest source_path must exist under it")
	flag.Parse()

	root, err := findRepoRoot()
	if err != nil {
		return err
	}
	m, err := loadManifest(root)
	if err != nil {
		return err
	}
	if *sourceRoot != "" {
		if problems := checkSources(*sourceRoot, m); len(problems) > 0 {
			return fmt.Errorf("provenance check failed:\n  %s", strings.Join(problems, "\n  "))
		}
		fmt.Printf("prosedistill: all %d manifest sources present under %s\n", len(m.Entries), *sourceRoot)
	}
	artifacts, err := composeArtifacts(root, m)
	if err != nil {
		return err
	}

	distilled, merged, skipped := 0, 0, 0
	for _, e := range m.Entries {
		switch e.Disposition {
		case "distilled":
			distilled++
		case "merged":
			merged++
		case "skipped":
			skipped++
		}
	}

	if *verify {
		problems, err := verifyTree(root, artifacts)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			return fmt.Errorf("verify failed:\n  %s", strings.Join(problems, "\n  "))
		}
		fmt.Printf("prosedistill --verify: OK — %d artifacts byte-identical, no strays (manifest: %d distilled, %d merged, %d skipped)\n",
			len(artifacts), distilled, merged, skipped)
		return nil
	}

	fmt.Println("prosedistill: composing skills/ from the manifest")
	if err := writeTree(root, artifacts); err != nil {
		return err
	}
	problems, err := verifyTree(root, artifacts)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("tree carries files the manifest does not produce:\n  %s", strings.Join(problems, "\n  "))
	}
	fmt.Printf("prosedistill: OK — %d artifacts written (manifest: %d distilled, %d merged, %d skipped)\n",
		len(artifacts), distilled, merged, skipped)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "prosedistill:", err)
		os.Exit(1)
	}
}
