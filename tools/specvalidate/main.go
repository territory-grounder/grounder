// Command specvalidate is Territory Grounder's spec-lattice gate.
//
// It enforces that the executable spec/ tree stays well-formed, uniquely-identified, EARS-shaped,
// weasel-word-free, dependency-acyclic, and traceable — requirement -> task -> acceptance scenario ->
// runnable test — and that governed safety-critical source files stay hash-bound to their owning spec
// (the spec<->code lockstep, BEH-7 / REQ-701..704). It is pure-stdlib Go so it runs in the same
// golang CI image as the build, adding no runtime dependency. [F] spec/007 · [O] INV-22.
//
// Usage:
//
//	go run ./tools/specvalidate                 # validate every spec/NNN-* dir + the index (default)
//	go run ./tools/specvalidate lockstep --check    # recompute governed-file hashes, fail on drift
//	go run ./tools/specvalidate lockstep --restamp  # rewrite .lockstep.lock (authorized re-stamp)
//	go run ./tools/specvalidate spec-index <path>   # print which spec/REQ own a source file
//
// Exit code is non-zero on the first category of failure; a summary of PASS/FAIL checks is printed.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	lshash "github.com/territory-grounder/grounder/tools/specvalidate/lockstep"
)

// ---- lattice conventions (the single source of the shape the validator enforces) ----

var (
	specDirRe   = regexp.MustCompile(`^(\d{3})-[a-z0-9]+(?:-[a-z0-9]+)*$`)   // 001-risk-classification
	reqHeaderRe = regexp.MustCompile(`(?m)^-\s+\*\*REQ-(\d{3,4}[a-z]?)\*\*`) // requirement block header; 3 digits, or 4 for spec/011+ (blocks 0xx..9xx are used); incl. overlay-added REQ-102b
	reqRefRe    = regexp.MustCompile(`REQ-\d{3,4}[a-z]?`)
	scenReRe    = regexp.MustCompile(`(?m)^\s*Scenario(?: Outline)?:\s*(.+?)\s*$`)
)

// requiredFiles are the fixed 5-file shape every spec/NNN-* dir must carry.
var requiredFiles = []string{
	"requirements.md",
	"design.md",
	"tasks.json",
	"acceptance/_test_mapping.json",
	"security/threat-model.md",
}

// weaselWords are banned from requirements.md — vague words defeat machine-verifiable acceptance.
// (EARS + a runnable oracle leave no room for "should be robust".) Matched case-insensitively as
// whole words / phrases.
var weaselWords = []string{
	"TODO", "TBD", "FIXME", "XXX",
	"might", "maybe", "probably", "should be", "as appropriate", "as needed",
	"robust", "scalable", "simple", "user-friendly", "seamless", "flexible",
	"and/or", "etc.", "and so on", "some", "several", "a few",
}

var validMappingStatus = map[string]bool{"present": true, "pending": true, "retrospective_gap": true}

// ---- typed shapes of the machine-readable lattice files ----

type tasksFile struct {
	Spec  string `json:"spec"`
	Tasks []task `json:"tasks"`
}

type task struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	FilesOwned []string   `json:"files_owned"`
	Deps       []string   `json:"deps"`
	ReqIDs     []string   `json:"req_ids"`
	Acceptance acceptance `json:"acceptance"`
	Budget     budget     `json:"budget"`
	Status     string     `json:"status"`
}

type acceptance struct {
	Feature   string   `json:"feature"`
	Scenarios []string `json:"scenarios"`
}

type budget struct {
	MaxLOCDelta        int `json:"max_loc_delta"`
	MaxWallClockMinute int `json:"max_wall_clock_minutes"`
}

type mappingFile struct {
	Feature   string        `json:"feature"`
	Scenarios []mappingScen `json:"scenarios"`
}

type mappingScen struct {
	Name   string `json:"name"`
	Req    string `json:"req"`
	Status string `json:"status"`
	Test   string `json:"test"`
}

type lockFile struct {
	Note  string      `json:"note"`
	Files []lockEntry `json:"files"`
}

type lockEntry struct {
	Path   string `json:"path"`
	Spec   string `json:"spec"`
	SHA256 string `json:"sha256"`
}

// ---- check accumulator ----

type checker struct {
	pass int
	fail []string
}

func (c *checker) ok()                    { c.pass++ }
func (c *checker) bad(f string, a ...any) { c.fail = append(c.fail, fmt.Sprintf(f, a...)) }

// check runs cond as one named assertion.
func (c *checker) check(cond bool, f string, a ...any) {
	if cond {
		c.ok()
	} else {
		c.bad(f, a...)
	}
}

func main() {
	root := repoRoot()
	args := os.Args[1:]
	switch {
	case len(args) == 0:
		validateSpecs(root)
	case args[0] == "lockstep" && len(args) >= 2 && args[1] == "--check":
		lockstep(root, false)
	case args[0] == "lockstep" && len(args) >= 2 && args[1] == "--restamp":
		lockstep(root, true)
	case args[0] == "spec-index" && len(args) >= 2:
		specIndex(root, args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: specvalidate [ | lockstep --check | lockstep --restamp | spec-index <path>]")
		os.Exit(2)
	}
}

// repoRoot walks up from the cwd to the dir holding go.mod so the tool works from anywhere.
func repoRoot() string {
	d, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			return "." // fall back to cwd
		}
		d = p
	}
}

func validateSpecs(root string) {
	c := &checker{}
	specRoot := filepath.Join(root, "spec")
	entries, err := os.ReadDir(specRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no spec/ dir at %s: %v\n", specRoot, err)
		os.Exit(1)
	}

	indexBytes, _ := os.ReadFile(filepath.Join(specRoot, "00-INDEX.md"))
	index := string(indexBytes)
	c.check(len(index) > 0, "spec/00-INDEX.md is missing or empty")

	var specDirs []string
	for _, e := range entries {
		if e.IsDir() && specDirRe.MatchString(e.Name()) {
			specDirs = append(specDirs, e.Name())
		}
	}
	c.check(len(specDirs) > 0, "spec/ contains no NNN-slug spec directories")

	for _, name := range specDirs {
		validateOneSpec(c, specRoot, name, index)
	}

	report(c, "spec-lattice")
}

func validateOneSpec(c *checker, specRoot, name, index string) {
	dir := filepath.Join(specRoot, name)
	id := name[:3]

	// 1) The index lists this spec.
	c.check(strings.Contains(index, name) || strings.Contains(index, "spec/"+id),
		"%s: not listed in spec/00-INDEX.md", name)

	// 2) The fixed 5-file shape is present (allow any *.feature under acceptance/).
	for _, rel := range requiredFiles {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err != nil {
			c.bad("%s: required file missing: %s", name, rel)
		} else {
			c.ok()
		}
	}
	features, _ := filepath.Glob(filepath.Join(dir, "acceptance", "*.feature"))
	c.check(len(features) > 0, "%s: no acceptance/*.feature file", name)

	// 3) requirements.md — EARS shape, REQ uniqueness, no weasel words.
	reqBytes, err := os.ReadFile(filepath.Join(dir, "requirements.md"))
	if err != nil {
		c.bad("%s: cannot read requirements.md: %v", name, err)
		return
	}
	req := string(reqBytes)
	reqIDs := map[string]bool{}
	// A requirement is a block: from its `- **REQ-NNN**` header to the next header or `## ` heading.
	// The provenance tag lives on the header line; SHALL lives in the body — so the whole block is
	// checked, not just the header line.
	locs := reqHeaderRe.FindAllStringSubmatchIndex(req, -1)
	c.check(len(locs) > 0, "%s: requirements.md has no `- **REQ-0NN**` requirement blocks", name)
	for k, loc := range locs {
		rid := "REQ-" + req[loc[2]:loc[3]]
		end := len(req)
		if k+1 < len(locs) {
			end = locs[k+1][0]
		}
		block := req[loc[0]:end]
		if hi := strings.Index(block, "\n## "); hi >= 0 {
			block = block[:hi]
		}
		if reqIDs[rid] {
			c.bad("%s: duplicate requirement id %s", name, rid)
		} else {
			reqIDs[rid] = true
			c.ok()
		}
		// EARS core keyword: every requirement is an obligation ("SHALL"), unless explicitly RETIRED.
		isRetired := strings.Contains(block, "RETIRED")
		c.check(strings.Contains(block, "SHALL") || isRetired,
			"%s: %s is not EARS-shaped (no SHALL in block)", name, rid)
	}
	weaselClean := true
	for _, w := range weaselWords {
		if idx := indexWord(req, w); idx >= 0 {
			c.bad("%s: requirements.md contains banned weasel word %q (near %q)", name, w, snippet(req, idx))
			weaselClean = false
		}
	}
	c.check(weaselClean, "%s: requirements.md weasel-word scan", name)

	// 4) tasks.json — schema, id uniqueness, DAG acyclicity, no file-ownership overlap, req back-links.
	validateTasks(c, name, dir, reqIDs)

	// 5) acceptance features tagged to real REQs + _test_mapping coverage.
	validateAcceptance(c, name, dir, features, reqIDs)
}

func validateTasks(c *checker, name, dir string, reqIDs map[string]bool) {
	b, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	if err != nil {
		c.bad("%s: cannot read tasks.json: %v", name, err)
		return
	}
	var tf tasksFile
	if err := json.Unmarshal(b, &tf); err != nil {
		c.bad("%s: tasks.json is not valid JSON: %v", name, err)
		return
	}
	ids := map[string]bool{}
	owners := map[string]string{} // file -> task that owns it
	for _, t := range tf.Tasks {
		c.check(t.ID != "", "%s: a task is missing an id", name)
		if ids[t.ID] {
			c.bad("%s: duplicate task id %s", name, t.ID)
		}
		ids[t.ID] = true
		c.check(len(t.ReqIDs) > 0, "%s: task %s references no requirement (req_ids empty)", name, t.ID)
		for _, r := range t.ReqIDs {
			c.check(reqIDs[r], "%s: task %s references unknown requirement %s", name, t.ID, r)
		}
		c.check(t.Budget.MaxLOCDelta > 0 && t.Budget.MaxWallClockMinute > 0,
			"%s: task %s has no positive budget (max_loc_delta / max_wall_clock_minutes)", name, t.ID)
		for _, f := range t.FilesOwned {
			if prev, dup := owners[f]; dup {
				c.bad("%s: file %s owned by both task %s and task %s (parallel collision)", name, f, prev, t.ID)
			}
			owners[f] = t.ID
		}
	}
	// deps reference existing tasks + DAG is acyclic.
	adj := map[string][]string{}
	for _, t := range tf.Tasks {
		for _, d := range t.Deps {
			c.check(ids[d], "%s: task %s depends on unknown task %s", name, t.ID, d)
			adj[t.ID] = append(adj[t.ID], d)
		}
	}
	c.check(acyclic(ids, adj), "%s: tasks.json dependency graph has a cycle", name)
}

func validateAcceptance(c *checker, name, dir string, features []string, reqIDs map[string]bool) {
	// gather scenario names from every .feature + assert each is REQ-tagged with a known REQ.
	featScenarios := map[string]bool{}
	for _, f := range features {
		fb, err := os.ReadFile(f)
		if err != nil {
			c.bad("%s: cannot read feature %s: %v", name, filepath.Base(f), err)
			continue
		}
		lines := strings.Split(string(fb), "\n")
		for i, ln := range lines {
			if sm := scenReRe.FindStringSubmatch(ln); sm != nil {
				scen := sm[1]
				featScenarios[scen] = true
				// a REQ tag (@REQ-0NN, native Gherkin tag) must appear just above the Scenario.
				tag := nearestTag(lines, i)
				c.check(tag != "", "%s: scenario %q has no @REQ-0NN tag", name, scen)
				for _, r := range reqRefRe.FindAllString(tag, -1) {
					c.check(reqIDs[r], "%s: scenario %q tagged unknown requirement %s", name, scen, r)
				}
			}
		}
	}

	// _test_mapping.json — every feature scenario is mapped, statuses valid, present -> named test.
	b, err := os.ReadFile(filepath.Join(dir, "acceptance", "_test_mapping.json"))
	if err != nil {
		c.bad("%s: cannot read acceptance/_test_mapping.json: %v", name, err)
		return
	}
	var mf mappingFile
	if err := json.Unmarshal(b, &mf); err != nil {
		c.bad("%s: _test_mapping.json is not valid JSON: %v", name, err)
		return
	}
	mapped := map[string]bool{}
	for _, s := range mf.Scenarios {
		mapped[s.Name] = true
		c.check(validMappingStatus[s.Status], "%s: scenario %q has invalid status %q", name, s.Name, s.Status)
		c.check(reqIDs[s.Req], "%s: mapping for %q references unknown requirement %q", name, s.Name, s.Req)
		if s.Status == "present" {
			c.check(s.Test != "", "%s: present scenario %q names no test", name, s.Name)
		}
		c.check(featScenarios[s.Name], "%s: mapping references scenario %q absent from the .feature files", name, s.Name)
	}
	for scen := range featScenarios {
		c.check(mapped[scen], "%s: feature scenario %q is not in _test_mapping.json (honest debt must be declared)", name, scen)
	}
}

// nearestTag returns the REQ-bearing Gherkin tag/comment lines directly above line i (the tag block
// of a Scenario). It stops at the first non-tag, non-comment, non-blank line above the Scenario.
func nearestTag(lines []string, i int) string {
	var tags []string
	for j := i - 1; j >= 0 && j >= i-6; j-- {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "@") || strings.HasPrefix(t, "#") {
			if reqRefRe.MatchString(t) {
				tags = append(tags, t)
			}
			continue
		}
		break
	}
	return strings.Join(tags, " ")
}

func acyclic(nodes map[string]bool, adj map[string][]string) bool {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var dfs func(string) bool
	dfs = func(n string) bool {
		color[n] = gray
		for _, m := range adj[n] {
			switch color[m] {
			case gray:
				return false
			case white:
				if !dfs(m) {
					return false
				}
			}
		}
		color[n] = black
		return true
	}
	for n := range nodes {
		if color[n] == white {
			if !dfs(n) {
				return false
			}
		}
	}
	return true
}

// ---- lockstep: governed source files hash-bound to their owning spec ----

func lockstep(root string, restamp bool) {
	c := &checker{}
	lockPath := filepath.Join(root, "spec", ".lockstep.lock")
	b, err := os.ReadFile(lockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", lockPath, err)
		os.Exit(1)
	}
	var lf lockFile
	if err := json.Unmarshal(b, &lf); err != nil {
		fmt.Fprintf(os.Stderr, "%s is not valid JSON: %v\n", lockPath, err)
		os.Exit(1)
	}
	changed := false
	for i, e := range lf.Files {
		src, err := os.ReadFile(filepath.Join(root, e.Path))
		if err != nil {
			c.bad("lockstep: governed file missing: %s", e.Path)
			continue
		}
		// its owning spec must exist.
		if _, err := os.Stat(filepath.Join(root, "spec", e.Spec)); err != nil {
			c.bad("lockstep: %s bound to non-existent spec %s", e.Path, e.Spec)
		}
		got := lshash.HashSemantic(e.Path, src)
		if restamp {
			if lf.Files[i].SHA256 != got {
				lf.Files[i].SHA256 = got
				changed = true
			}
			c.ok()
			continue
		}
		c.check(e.SHA256 == got,
			"lockstep: %s changed but its owning spec %s was not updated (spec drift) — expected %s got %s",
			e.Path, e.Spec, short(e.SHA256), short(got))
	}
	if restamp && changed {
		out, _ := json.MarshalIndent(lf, "", "  ")
		_ = os.WriteFile(lockPath, append(out, '\n'), 0o644)
		fmt.Println("lockstep: .lockstep.lock re-stamped")
	}
	report(c, "spec<->code lockstep")
}

func specIndex(root, target string) {
	lockPath := filepath.Join(root, "spec", ".lockstep.lock")
	b, err := os.ReadFile(lockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read %s: %v\n", lockPath, err)
		os.Exit(1)
	}
	var lf lockFile
	_ = json.Unmarshal(b, &lf)
	clean := filepath.ToSlash(strings.TrimPrefix(target, "./"))
	for _, e := range lf.Files {
		if e.Path == clean || strings.HasSuffix(clean, e.Path) {
			fmt.Printf("%s is governed by spec %s — read spec/%s/requirements.md before changing it; "+
				"changing it without updating that spec fails `specvalidate lockstep --check`.\n", e.Path, e.Spec, e.Spec)
			return
		}
	}
	fmt.Printf("%s is not in the lockstep manifest (not a governed safety-critical file). "+
		"If it should be, add it to spec/.lockstep.lock bound to its owning spec.\n", clean)
}

// ---- helpers ----

func indexWord(hay, word string) int {
	lh, lw := strings.ToLower(hay), strings.ToLower(word)
	from := 0
	for {
		i := strings.Index(lh[from:], lw)
		if i < 0 {
			return -1
		}
		abs := from + i
		if wordBoundary(lh, abs, len(lw)) {
			return abs
		}
		from = abs + len(lw)
	}
}

func wordBoundary(s string, start, n int) bool {
	isAlnum := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
	}
	// phrases containing spaces/periods (e.g. "should be", "etc.") match as-is.
	if strings.ContainsAny(s[start:start+n], " .") {
		return true
	}
	if start > 0 && isAlnum(s[start-1]) {
		return false
	}
	if start+n < len(s) && isAlnum(s[start+n]) {
		return false
	}
	return true
}

func snippet(s string, idx int) string {
	start := idx - 20
	if start < 0 {
		start = 0
	}
	end := idx + 20
	if end > len(s) {
		end = len(s)
	}
	return strings.ReplaceAll(s[start:end], "\n", " ")
}

func short(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

func report(c *checker, what string) {
	sort.Strings(c.fail)
	total := c.pass + len(c.fail)
	if len(c.fail) == 0 {
		fmt.Printf("specvalidate: %s OK — %d/%d checks PASS\n", what, c.pass, total)
		return
	}
	fmt.Printf("specvalidate: %s FAILED — %d/%d checks PASS, %d FAIL:\n", what, c.pass, total, len(c.fail))
	for _, f := range c.fail {
		fmt.Printf("  [FAIL] %s\n", f)
	}
	os.Exit(1)
}
