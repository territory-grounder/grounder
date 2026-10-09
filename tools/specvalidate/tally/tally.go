// Package tally mechanizes the "lattice state" block in spec/00-INDEX.md.
//
// The block it replaces was HAND-WRITTEN, dated 2026-07-31, and wrong on every count by 2026-08-10: it said
// "No spec has reached Ratified" under a table marking 18 Ratified, and carried task totals (250/61/9) the
// tree had long since left behind (275/48/8). Nothing failed, because nothing compared the prose to the tree
// — a hand-written number is an assertion, and the release-gate rule here is generated numbers, never
// hand-written. So this package computes the block from the tree and pins it with a drift gate:
//
//	specvalidate tally --write   # recompute and rewrite the block between the markers
//	specvalidate tally --check   # recompute and byte-compare; any hand edit or stale number is non-zero
//
// The tally is deterministic and carries NO timestamp: a timestamp would make --check drift on every run,
// and a gate that always fails is a gate that gets deleted. Every number is rendered with its denominator.
//
// Deliberately NOT here: whether a `Ratified` row is EARNED (evidence vs claim). That cross-check lives in
// `ratify --check`, which adjudicates the Status column against the execution census; this package only
// counts, so the two cannot disagree about whose verdict is whose.
package tally

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The marker pair the generated block lives between in spec/00-INDEX.md. Exact strings: --check finds the
// block by them, and a missing pair is its own distinct failure (not "drift" — there is nothing to drift).
const (
	BeginMarker = "<!-- BEGIN GENERATED: specvalidate tally -->"
	EndMarker   = "<!-- END GENERATED: specvalidate tally -->"
)

var (
	// specDirPattern matches a spec directory name (001-risk-classification) — same shape main.go enforces.
	specDirPattern = regexp.MustCompile(`^\d{3}-[a-z0-9]+(?:-[a-z0-9]+)*$`)
	// indexRowPattern pulls the spec directory out of an index table row's link cell, e.g. `(015-policy-engine/)`.
	// The generated block itself never matches: it names specs bare, without the `(dir/)` link form.
	indexRowPattern = regexp.MustCompile(`\((\d{3}-[a-z0-9]+(?:-[a-z0-9]+)*)/\)`)
)

// taskStates is the CLOSED task-status vocabulary, in render order. A status outside it is a HARD ERROR
// naming file and task — an unknown status silently miscounted would put a wrong number in the index with
// this tool's authority behind it, which is strictly worse than the hand-written number it replaced.
var taskStates = []string{"completed", "pending", "blocked"}

// mappingStates is the closed _test_mapping.json status vocabulary, in render order (same set the
// spec-lattice validator enforces).
var mappingStates = []string{"present", "pending", "retrospective_gap"}

// indexStates is spec/00-INDEX.md's own status vocabulary, rendered most-earned first.
var indexStates = []string{"Ratified", "Approved", "Draft"}

type tasksFile struct {
	Tasks []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"tasks"`
}

type mappingFile struct {
	Scenarios []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"scenarios"`
}

// specPending is one spec's pending-task count, kept per spec so the block states WHERE the open work
// concentrates instead of hiding it in a total.
type specPending struct {
	Spec  string
	Count int
}

// counts is everything the block renders, collected from the tree in one pass.
type counts struct {
	specDirs     int
	tasksFiles   int            // tasks.json files actually read — the "across N specs" denominator
	tasks        map[string]int // by status
	tasksTotal   int
	pending      []specPending // specs with >0 pending tasks, in directory order
	mappingFiles int
	scen         map[string]int // by status
	scenTotal    int
	indexRows    int            // spec rows parsed from the index table
	index        map[string]int // by Status column value
}

// Run executes one tally mode against the repo at root. mode is "write" or "check".
func Run(root, mode string) error {
	if mode != "write" && mode != "check" {
		return fmt.Errorf("unknown mode %q (want \"write\" or \"check\")", mode)
	}
	c, err := collect(root)
	if err != nil {
		return err
	}
	// THE BLINDNESS GUARD. Zero tasks.json means the walk found no lattice at all — a wrong root, a renamed
	// tree, a broken glob. Rendering (or worse, certifying) an all-zero tally there would be a confident
	// statement about nothing. Distinct message from drift: this is a failed measurement, not a stale block.
	if c.tasksFiles == 0 {
		return fmt.Errorf("TALLY BLIND: 0 tasks.json under spec/ — refusing to certify an empty lattice")
	}
	want := render(c)
	indexPath := filepath.Join(root, "spec", "00-INDEX.md")
	b, err := os.ReadFile(indexPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", indexPath, err)
	}
	current := string(b)
	start := strings.Index(current, BeginMarker)
	end := strings.Index(current, EndMarker)
	if start < 0 || end < 0 || end < start {
		return fmt.Errorf("TALLY MARKERS MISSING: %s does not carry the %q .. %q pair — the generated block "+
			"has nowhere to live; restore the markers (the block between them is owned by 'tally --write')",
			indexPath, BeginMarker, EndMarker)
	}
	got := current[start : end+len(EndMarker)]
	if mode == "write" {
		if got == want {
			fmt.Println("specvalidate tally: block already current — nothing to write")
			return nil
		}
		out := current[:start] + want + current[end+len(EndMarker):]
		if err := os.WriteFile(indexPath, []byte(out), 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", indexPath, err)
		}
		fmt.Printf("specvalidate tally: wrote generated block to %s\n", indexPath)
		return nil
	}
	if got != want {
		line, inFile, recomputed := firstDiff(got, want)
		return fmt.Errorf("TALLY DRIFT: the generated block in %s does not match the tree (first difference "+
			"at block line %d):\n  in file:    %s\n  recomputed: %s\nhand edits are rejected; run "+
			"'specvalidate tally --write' and commit the result", indexPath, line, inFile, recomputed)
	}
	fmt.Printf("specvalidate tally: OK — block in spec/00-INDEX.md matches the tree "+
		"(%d specs, %d tasks, %d scenarios)\n", c.specDirs, c.tasksTotal, c.scenTotal)
	return nil
}

// collect walks spec/ and tallies. It never invents a zero for a file it could not read: an unreadable or
// malformed lattice file is an error, because a tally that silently skips input under-reports with authority.
func collect(root string) (counts, error) {
	c := counts{tasks: map[string]int{}, scen: map[string]int{}, index: map[string]int{}}
	specRoot := filepath.Join(root, "spec")
	entries, err := os.ReadDir(specRoot)
	if err != nil {
		return c, fmt.Errorf("cannot read spec/ under %s: %w", root, err)
	}
	known := map[string]bool{}
	for _, s := range taskStates {
		known[s] = true
	}
	knownScen := map[string]bool{}
	for _, s := range mappingStates {
		knownScen[s] = true
	}
	for _, e := range entries { // os.ReadDir sorts by name: deterministic walk, deterministic block
		if !e.IsDir() || !specDirPattern.MatchString(e.Name()) {
			continue
		}
		c.specDirs++
		dir := filepath.Join(specRoot, e.Name())

		tp := filepath.Join(dir, "tasks.json")
		tb, terr := os.ReadFile(tp)
		if terr != nil && !os.IsNotExist(terr) {
			// Unreadable-but-present is NOT absence: silently skipping it would shrink the tally and
			// under-report with this tool's authority — the exact conflation the package refuses.
			return c, fmt.Errorf("cannot read %s: %w — an unreadable lattice file fails the tally, it does not shrink it", tp, terr)
		}
		if terr == nil {
			var tf tasksFile
			if err := json.Unmarshal(tb, &tf); err != nil {
				return c, fmt.Errorf("%s is not valid JSON: %w", tp, err)
			}
			c.tasksFiles++
			pendingHere := 0
			for _, t := range tf.Tasks {
				if !known[t.Status] {
					return c, fmt.Errorf("unknown task status %q on task %s in %s — the closed vocabulary is "+
						"completed/pending/blocked, and an unrecognised status silently miscounted would put a "+
						"wrong number in the index", t.Status, t.ID, tp)
				}
				c.tasks[t.Status]++
				c.tasksTotal++
				if t.Status == "pending" {
					pendingHere++
				}
			}
			if pendingHere > 0 {
				c.pending = append(c.pending, specPending{Spec: e.Name(), Count: pendingHere})
			}
		}

		mp := filepath.Join(dir, "acceptance", "_test_mapping.json")
		mb, merr := os.ReadFile(mp)
		if merr != nil && !os.IsNotExist(merr) {
			return c, fmt.Errorf("cannot read %s: %w — an unreadable lattice file fails the tally, it does not shrink it", mp, merr)
		}
		if merr == nil {
			var mf mappingFile
			if err := json.Unmarshal(mb, &mf); err != nil {
				return c, fmt.Errorf("%s is not valid JSON: %w", mp, err)
			}
			c.mappingFiles++
			for _, s := range mf.Scenarios {
				if !knownScen[s.Status] {
					return c, fmt.Errorf("unknown scenario status %q on %q in %s — the closed vocabulary is "+
						"present/pending/retrospective_gap", s.Status, s.Name, mp)
				}
				c.scen[s.Status]++
				c.scenTotal++
			}
		}
	}
	// The index's own Status column, tallied as CLAIMED. Whether a `Ratified` claim is EARNED is
	// deliberately not adjudicated here — that cross-check lives in `ratify --check`.
	ib, err := os.ReadFile(filepath.Join(specRoot, "00-INDEX.md"))
	if err != nil {
		return c, fmt.Errorf("cannot read spec/00-INDEX.md: %w", err)
	}
	for _, line := range strings.Split(string(ib), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") || !indexRowPattern.MatchString(line) {
			continue
		}
		cells := strings.Split(line, "|")
		status := ""
		for i := len(cells) - 1; i >= 0; i-- {
			if v := strings.TrimSpace(strings.Trim(strings.TrimSpace(cells[i]), "*")); v != "" {
				status = v
				break
			}
		}
		c.indexRows++
		c.index[status]++
	}
	return c, nil
}

// render produces the whole block, markers included. Deterministic — same tree, same bytes; NO timestamp
// (a timestamp would make --check drift on every run, and "when" belongs to git history, not the block).
func render(c counts) string {
	var b strings.Builder
	b.WriteString(BeginMarker + "\n")
	b.WriteString("> **Lattice tally (generated).** " + fmt.Sprintf("%d specs in the lattice shape under `spec/`.\n", c.specDirs))
	fmt.Fprintf(&b, "> Index status column (as claimed): %s of %d rows.\n", tallyLine(c.index, indexStates), c.indexRows)
	fmt.Fprintf(&b, "> Tasks: %s of %d across %d specs.\n", tallyLine(c.tasks, taskStates), c.tasksTotal, c.tasksFiles)
	fmt.Fprintf(&b, "> Acceptance scenarios: %s of %d across %d mappings.\n",
		tallyLine(c.scen, mappingStates), c.scenTotal, c.mappingFiles)
	fmt.Fprintf(&b, "> Pending-task concentration: %s.\n", concentration(c.pending))
	b.WriteString("> Generated by `specvalidate tally --write`; hand edits are rejected by `tally --check`.\n")
	b.WriteString(EndMarker)
	return b.String()
}

// tallyLine renders "275 completed / 48 pending / 8 blocked" in the vocabulary's declared order, then any
// value OUTSIDE the vocabulary sorted after it — an off-vocabulary index status stays visible rather than
// silently dropping out of a total that still adds up.
func tallyLine(m map[string]int, order []string) string {
	parts := make([]string, 0, len(order))
	seen := map[string]bool{}
	for _, s := range order {
		seen[s] = true
		parts = append(parts, fmt.Sprintf("%d %s", m[s], s))
	}
	var extra []string
	for s := range m {
		if !seen[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	for _, s := range extra {
		parts = append(parts, fmt.Sprintf("%d %s", m[s], s))
	}
	return strings.Join(parts, " / ")
}

// concentration names each spec still carrying pending tasks, with its count — the total alone hides WHERE
// the open work sits. The healthy case is stated, not blank: "(none)" and a line that stopped rendering are
// different things.
func concentration(pending []specPending) string {
	if len(pending) == 0 {
		return "(none — no spec carries a pending task)"
	}
	parts := make([]string, 0, len(pending))
	for _, p := range pending {
		parts = append(parts, fmt.Sprintf("%s %d", p.Spec, p.Count))
	}
	return strings.Join(parts, " · ")
}

// firstDiff returns the 1-based line number and both versions of the first differing line between the block
// in the file and the recomputed block, so the failure message shows the exact stale claim.
func firstDiff(inFile, recomputed string) (int, string, string) {
	fl := strings.Split(inFile, "\n")
	rl := strings.Split(recomputed, "\n")
	n := len(fl)
	if len(rl) > n {
		n = len(rl)
	}
	for i := 0; i < n; i++ {
		var f, r string
		if i < len(fl) {
			f = fl[i]
		} else {
			f = "(line absent)"
		}
		if i < len(rl) {
			r = rl[i]
		} else {
			r = "(line absent)"
		}
		if f != r {
			return i + 1, f, r
		}
	}
	return 0, "(no differing line — lengths differ only in trailing bytes)", ""
}
