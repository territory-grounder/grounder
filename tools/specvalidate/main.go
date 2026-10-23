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
//	go run ./tools/specvalidate tally --check       # generated lattice-state block in 00-INDEX.md matches the tree
//	go run ./tools/specvalidate tally --write       # regenerate that block from the tree
//
// Exit code is non-zero on the first category of failure; a summary of PASS/FAIL checks is printed.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/territory-grounder/grounder/tools/specvalidate/ratify"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/tools/faultinjector"
	lshash "github.com/territory-grounder/grounder/tools/specvalidate/lockstep"
	"github.com/territory-grounder/grounder/tools/specvalidate/opcover"
	"github.com/territory-grounder/grounder/tools/specvalidate/tally"
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
	// warn carries findings that are REAL but must not fail the build yet. The distinction exists because a
	// gate that goes from silent to fail-closed over pre-existing debt takes the pipeline down on the commit
	// that adds the gate, and gets deleted rather than fixed. A warning is how debt becomes MEASURABLE
	// before it becomes enforced.
	warn []string
}

func (c *checker) ok()                      { c.pass++ }
func (c *checker) bad(f string, a ...any)   { c.fail = append(c.fail, fmt.Sprintf(f, a...)) }
func (c *checker) warnf(f string, a ...any) { c.warn = append(c.warn, fmt.Sprintf(f, a...)) }

// check runs cond as one named assertion.
func (c *checker) check(cond bool, f string, a ...any) {
	if cond {
		c.ok()
	} else {
		c.bad(f, a...)
	}
}

// phantomOwnedMarker is the distinctive tail of the "completed task owns a path that does not exist"
// warning. It is SHARED between the warning that emits one and the ratchet that counts them, so the two
// cannot drift: reword the warning without this and the count silently reads zero.
const phantomOwnedMarker = "traceability gap: nothing verifies this path"

// phantomOwnedCeiling is the TG-416 RATCHET. A `completed` task owning a path that does not exist is only a
// WARN (see the checker's `warn` doc: debt is made measurable before it is enforced), so the spec-task
// completion count could inflate forever — 38 such entries existed on 2026-08-07 (spec/010's frontend/**,
// where the console shipped as deploy/console/v2 in an architecture change nobody swept the spec for, plus a
// handful in 020/023/028) and nothing stopped a 39th. All 38 were cleared on 2026-08-08 (TG-416), TWO ways —
// because a phantom path has two honest causes, and conflating them is itself the trap (a WARN is noise, a
// false green is a lie). The 020/023/028 Go paths were REPOINTED at the differently-named file that carries
// the concept, content-verified per entry (e.g. the confidence/attribution persistence in triage_judgment.go,
// the runner wiring in activities.go, the security-escalation routing in risk/classifier.go) — a name-match
// would have been a trap (T-023-6 "claimed 0034" which is `ingest_transition`). But the spec/010 console tasks
// were NOT delivered as specified: ADR-0015 removed the React frontend and the served console is a partial
// preview, so per the repo's own acceptance audit (spec/010-ux-console/acceptance/_test_mapping.json) every
// T-010-1..8 defining REQ is `pending` — no oracle, or no feature at all (REQ-607 replay, REQ-610/611 kill
// writes). Repointing those `completed` at a real console file would trade a visible WARN for a SILENT false
// green; they were flipped to `pending` instead (their files_owned now trace to the real partial artifact for
// reference). Now zero-tolerance: a newly-`completed` task naming an absent file fails HERE. LOWER it as debt
// is paid; NEVER raise it. A ratchet, not a waiver — the same discipline as `ratify --max-findings`.
const phantomOwnedCeiling = 0

// countPhantomOwned counts the completed-task-owns-missing-path warnings among all accumulated warnings.
func countPhantomOwned(warn []string) int {
	n := 0
	for _, w := range warn {
		if strings.Contains(w, phantomOwnedMarker) {
			n++
		}
	}
	return n
}

// phantomRatchetVerdict is the pure ratchet decision: -1 OVER the ceiling (a new phantom completion — FAIL),
// +1 UNDER it (debt paid down — lower the ceiling), 0 exactly AT it (held).
func phantomRatchetVerdict(phantom, ceiling int) int {
	switch {
	case phantom > ceiling:
		return -1
	case phantom < ceiling:
		return 1
	default:
		return 0
	}
}

func main() {
	root := repoRoot()
	args := os.Args[1:]
	switch {
	case len(args) == 0:
		validateSpecs(root)
	case args[0] == "lockstep" && len(args) >= 2 && args[1] == "--check":
		lockstep(root, false, false)
	case args[0] == "lockstep" && len(args) >= 2 && args[1] == "--restamp":
		allowUnchanged := len(args) >= 3 && args[2] == "--allow-unchanged-spec"
		lockstep(root, true, allowUnchanged)
	case args[0] == "spec-index" && len(args) >= 2:
		specIndex(root, args[1])
	case args[0] == "ratify" && len(args) >= 2 && args[1] == "--check":
		ratifyCheck(root, args[2:])
	case args[0] == "opcover" && len(args) >= 2 && args[1] == "--check":
		opcoverCheck(root)
	case args[0] == "tally" && len(args) >= 2 && (args[1] == "--write" || args[1] == "--check"):
		tallyRun(root, strings.TrimPrefix(args[1], "--"))
	default:
		fmt.Fprintln(os.Stderr, "usage: specvalidate [ | lockstep --check | lockstep --restamp | spec-index <path> | ratify --check | opcover --check | tally --write | tally --check]")
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

	// TG-416 ratchet: a newly-`completed` task owning a path that does not exist pushes the phantom count
	// OVER the pinned ceiling and fails HERE, rather than adding one more silently-inflating WARN that the
	// spec-task completion metric then counts as done.
	switch phantom := countPhantomOwned(c.warn); phantomRatchetVerdict(phantom, phantomOwnedCeiling) {
	case -1:
		c.bad("phantom-owned RATCHET: %d completed task(s) own a path that does not exist — ABOVE the pinned "+
			"ceiling of %d (TG-416). A newly-completed task names a file that is not there: point its files_owned "+
			"at the file that exists (content-verified, NOT name-matched) or drop the task's `completed` status. "+
			"Never raise the ceiling.", phantom, phantomOwnedCeiling)
	case 1:
		c.warnf("phantom-owned RATCHET: %d completed task(s) own a missing path — BELOW the pinned ceiling of "+
			"%d (TG-416): debt was paid down. Lower phantomOwnedCeiling to %d in tools/specvalidate/main.go so "+
			"it cannot silently regrow.", phantom, phantomOwnedCeiling, phantom)
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
			// DOES THE FILE EXIST? Until now files_owned was checked ONLY for duplicate ownership, so an entry
			// pointing at a renamed, renumbered or deleted path was invisible — and files_owned is the
			// spec<->code traceability spine. Measured 2026-08-07: 114 of 598 entries named nothing, 68 of them
			// on tasks marked `completed`. That is how T-023-6 could claim migration 0034_actor_attribution
			// while 0034 is ingest_transition and the real file is 0035.
			//
			// ONLY `completed` tasks are reported. A pending or blocked task naming a file that does not exist
			// yet is the correct state, not a defect — reporting those would bury the real signal under 46
			// entries that are working as intended.
			if t.Status == "completed" {
				if _, err := os.Stat(f); err != nil {
					c.warnf("%s: task %s is completed but owns %s, which does not exist (spec<->code %s)",
						name, t.ID, f, phantomOwnedMarker)
				}
			}
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

func lockstep(root string, restamp, allowUnchangedSpec bool) {
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
	var specsTouched map[string]bool
	if restamp && !allowUnchangedSpec {
		specsTouched = changedSpecDirs(root)
	}
	changed := false
	var refused []string
	movedBySpec := map[string][]string{} // owning spec -> paths whose hash moves (TG-536: authorized + ledgered)
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
				if specsTouched != nil && !specsTouched[e.Spec] {
					refused = append(refused, fmt.Sprintf("%s (owning spec %s unchanged in this diff)", e.Path, e.Spec))
					continue
				}
				lf.Files[i].SHA256 = got
				movedBySpec[e.Spec] = append(movedBySpec[e.Spec], e.Path)
				changed = true
			}
			c.ok()
			continue
		}
		c.check(e.SHA256 == got,
			"lockstep: %s changed but its owning spec %s was not updated (spec drift) — expected %s got %s",
			e.Path, e.Spec, short(e.SHA256), short(got))
	}
	if len(refused) > 0 {
		fmt.Fprintln(os.Stderr, "lockstep: --restamp REFUSED — a re-stamp is only authorized in the same diff that")
		fmt.Fprintln(os.Stderr, "updates the owning spec (SDD-WORKFLOW §5). Silently overwriting the hash is what made")
		fmt.Fprintln(os.Stderr, "this gate theater. Update the owning spec first, or (exceptional, visible in your")
		fmt.Fprintln(os.Stderr, "command history and MR) pass --allow-unchanged-spec:")
		for _, r := range refused {
			fmt.Fprintf(os.Stderr, "  - %s\n", r)
		}
		os.Exit(1)
	}
	if restamp && changed {
		// TG-536 / REQ-703: a hash may move ONLY inside an authorized, RBAC-attributed, chained approval.
		// AuthorizeRestamp appends to spec/.restamp-ledger.jsonl (committed in the same MR as the moved
		// hashes); any denial refuses the whole restamp with the manifest untouched.
		n, err := authorizeRestamps(root, movedBySpec, specsTouched, allowUnchangedSpec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lockstep: --restamp REFUSED — %v\n", err)
			os.Exit(1)
		}
		out, err := json.MarshalIndent(lf, "", "  ")
		if err != nil {
			// The ledger already holds the authorization; a manifest that then fails to write must be LOUD,
			// or the record asserts a re-stamp that never landed (review finding 2026-08-25).
			fmt.Fprintf(os.Stderr, "lockstep: manifest marshal FAILED after the ledger append — %v; re-run --restamp\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(lockPath, append(out, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "lockstep: writing %s FAILED after the ledger append — %v; re-run --restamp\n", lockPath, err)
			os.Exit(1)
		}
		fmt.Printf("lockstep: .lockstep.lock re-stamped (%d authorized approval(s) appended to %s — commit it with this change)\n",
			n, restampLedgerRel)
	}
	// The in-repo restamp chain is re-walked on EVERY run — check and restamp alike — with its denominator
	// printed even at zero: a content-tampered historical entry reds CI here, not on some later restamp.
	if nLedger, err := verifyRestampLedger(root); err != nil {
		c.bad("lockstep: %v", err)
	} else {
		fmt.Printf("lockstep: restamp ledger %s — %d entr(ies), chain OK\n", restampLedgerRel, nLedger)
	}
	report(c, "spec<->code lockstep")
}

// changedSpecDirs returns the set of spec ids (e.g. "012-runner-workflow") whose files differ from the
// merge-base with origin/main (falling back to HEAD when no remote is available), unioned with any
// uncommitted or staged spec changes. Used by --restamp to enforce that a hash may only move in the same
// diff that updates the owning spec. Fails CLOSED: if git is unavailable, no spec counts as changed and
// the caller must use --allow-unchanged-spec explicitly.
func changedSpecDirs(root string) map[string]bool {
	touched := map[string]bool{}
	base := "HEAD"
	if out, err := exec.Command("git", "-C", root, "merge-base", "HEAD", "origin/main").Output(); err == nil {
		base = strings.TrimSpace(string(out))
	}
	collect := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			// status --porcelain lines carry a 2-char code + space prefix; diff lines are bare paths.
			if i := strings.Index(line, "spec/"); i >= 0 {
				rest := strings.TrimPrefix(line[i:], "spec/")
				if j := strings.IndexByte(rest, '/'); j > 0 {
					touched[rest[:j]] = true
				}
			}
		}
	}
	collect("diff", "--name-only", base)
	collect("diff", "--name-only", "--cached")
	collect("status", "--porcelain")
	return touched
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
	sort.Strings(c.warn)
	// WARNINGS PRINT WITH THEIR COUNT, ALWAYS — including when there are none. "0 warnings" and a warning
	// channel that has stopped working produce different output, which is the whole point of stating the
	// denominator rather than staying silent on the healthy case.
	fmt.Printf("specvalidate: %s — %d warning(s)\n", what, len(c.warn))
	for _, w := range c.warn {
		fmt.Printf("  [WARN] %s\n", w)
	}
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

// ratifyCheck answers, per spec, whether every live requirement has a route to an oracle or a DECLARED gap.
// Neither lockstep (code<->spec binding) nor the prose validator notices a requirement no task references, or
// a task naming an acceptance scenario that does not exist — both are silent, and both leave governance that
// reads as exercised and is not.
func ratifyCheck(root string, rest []string) {
	// --max-findings is a RATCHET, not a waiver. The lattice carries real debt (36 requirements with no oracle
	// route when this shipped), and a gate that fails on all of it from day one gets disabled within a week —
	// at which point it protects nothing. Pinning the CURRENT count instead makes the debt a ceiling that can
	// only fall: a new unratified requirement reds the build immediately, and lowering the pin is a deliberate
	// edit that records the improvement.
	//
	// This is the one shape of "tolerated failure" that is not a fail-open, because it cannot admit anything
	// NEW. A plain --check with no ceiling stays available and is what the phase exits on.
	max := -1
	for i := 0; i < len(rest); i++ {
		if v, ok := strings.CutPrefix(rest[i], "--max-findings="); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "specvalidate: ratify: --max-findings needs an integer")
				os.Exit(2)
			}
			max = n
		}
	}
	rep, err := ratify.Check(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "specvalidate: ratify:", err)
		os.Exit(2)
	}
	fmt.Print(rep.Render())
	n := len(rep.Findings)
	if max < 0 {
		if n > 0 {
			os.Exit(1)
		}
		return
	}
	if n > max {
		fmt.Fprintf(os.Stderr, "\nspecvalidate: ratify: %d finding(s) exceeds the pinned ceiling of %d — "+
			"the lattice REGRESSED. Fix the new finding, or lower nothing: the ceiling only ever comes down.\n", n, max)
		os.Exit(1)
	}
	if n < max {
		fmt.Printf("\n  RATCHET: %d finding(s) is BELOW the pinned ceiling of %d — lower --max-findings to %d "+
			"in .gitlab-ci.yml so the improvement cannot be undone.\n", n, max, n)
	}
}

// tallyRun mechanizes the lattice-state block in spec/00-INDEX.md: --write regenerates it from the tree,
// --check byte-compares and fails on any hand edit or stale number. The hand-written block this replaced was
// wrong on every count within ten days of being written; the whole design is in tools/specvalidate/tally.
// (Whether a `Ratified` index row is EARNED is deliberately NOT re-checked here — that cross-check lives in
// `ratify --check`; tally only counts.)
func tallyRun(root, mode string) {
	if err := tally.Run(root, mode); err != nil {
		fmt.Fprintln(os.Stderr, "specvalidate tally:", err)
		os.Exit(1)
	}
}

// opcoverCheck proves every actuatable op-class is provoked by at least one fault class, or that its absence
// is declared with a reason. It reads the LIVE registry and the LIVE fault-class enumeration rather than a
// hand-maintained list, so the check cannot drift from what actually ships.
func opcoverCheck(root string) {
	ops := map[string]bool{}
	for _, s := range opschema.Specs() {
		ops[strings.ToLower(strings.TrimSpace(s.OpClass))] = true
	}
	prov := map[string][]string{}
	for _, c := range faultinjector.AllClasses() {
		prov[string(c)] = c.Provokes()
	}
	rep, err := opcover.Check(root, ops, prov)
	if err != nil {
		fmt.Fprintf(os.Stderr, "specvalidate opcover: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(rep.Render())
	if !rep.Covered100() {
		os.Exit(1)
	}
}
