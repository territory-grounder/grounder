// Package ratify answers one question per spec: is every live requirement actually reachable by an oracle,
// or has its absence been DECLARED?
//
// The lattice already binds code to specs (lockstep) and validates requirement prose (spec-lattice). Neither
// notices the two failures this catches, and both are silent by construction:
//
//  1. A REQUIREMENT NO TASK REFERENCES. It is law with no route to an oracle. Nothing fails — the requirement
//     simply sits there being true-by-assertion, and a reader counting requirements sees governance that is
//     not exercised. Measured when this shipped: 43 of 370.
//
//  2. A TASK NAMING AN ACCEPTANCE SCENARIO THAT DOES NOT EXIST. The task points at a scenario in a .feature
//     file; the scenario was renamed or never written. The task still reads as covered, the godog suite still
//     passes (it never knew the scenario was expected), and the coverage is imaginary. Measured: 5.
//
// This is the same shape as every reachability defect in this repo — a thing that is complete on one side of a
// boundary and unreferenced on the other, where the boundary is exactly what nobody checks. INV-22 forbids an
// UNDECLARED test gap; a declared one is a legitimate engineering position, so `retrospective_gaps` in
// tasks.json makes the declaration explicit and auditable rather than letting silence stand in for it.
package ratify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// reqPattern matches a requirement id as it is DECLARED in requirements.md: bolded, at the head of its entry.
// Matching the bold form rather than any mention avoids counting cross-references in another requirement's
// rationale as declarations — which would invent requirements that do not exist.
var reqPattern = regexp.MustCompile(`\*\*(REQ-\d+[a-z]?)\*\*`)

// scenarioPattern matches a Gherkin scenario heading.
var scenarioPattern = regexp.MustCompile(`(?m)^\s*Scenario(?: Outline)?:\s*(.+?)\s*$`)

// Gap is an operator declaration that a requirement has no executing oracle, and why. A gap is not a failure:
// it is a stated position that can be reviewed. Silence is the failure.
type Gap struct {
	Req string `json:"req"`
	Why string `json:"why"`
}

// doneStates is the CLOSED set of task statuses meaning "this work is finished". A scenario a FINISHED task
// names must exist; a scenario a PENDING task names is planned work, not imaginary coverage, and flagging it
// would drown the real defect in forward-looking noise.
var doneStates = map[string]bool{"completed": true}

// knownStates is the CLOSED set of every status a task may carry. Anything outside it — including an ABSENT
// status — is reported rather than silently treated as one of them: a typo'd or missing status would
// otherwise decide, invisibly, whether that task's acceptance is checked at all.
// "done" is deliberately ABSENT: it and "completed" were the same state under two words (120 vs 37 across the
// lattice), and a split vocabulary means a reader — or a checker — must know both to count correctly. One
// canonical word, enforced, so the split cannot reappear.
var knownStates = map[string]bool{"completed": true, "pending": true, "blocked": true}

type task struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	ReqIDs     []string `json:"req_ids"`
	Acceptance *struct {
		Feature   string   `json:"feature"`
		Scenarios []string `json:"scenarios"`
	} `json:"acceptance"`
}

type tasksFile struct {
	Spec              string `json:"spec"`
	Tasks             []task `json:"tasks"`
	RetrospectiveGaps []Gap  `json:"retrospective_gaps"`
}

// Finding is one unratified thing, named precisely enough to act on.
type Finding struct {
	Spec string
	// Kind is the closed set of ways a claim in this lattice can be false:
	//   unreferenced-requirement        law with no route to an oracle and no declared gap
	//   missing-scenario                a finished task names a scenario the feature file does not contain
	//   empty-gap-reason                a declared gap with no WHY — silence with extra steps
	//   unknown-task-status             a task status outside the closed set (decides invisibly what is checked)
	//   unmapped-scenario               a feature scenario with no _test_mapping.json row at all
	//   imaginary-oracle                `present` naming an oracle that does not exist in the tree
	//   non-test-oracle                 `present` naming something that exists but adjudicates nothing
	//   skipped-scenario-claimed-present  `present` on a @pending scenario whose only oracle is the runner
	//                                   that skips it
	//   unknown-spec-status             a spec status outside spec/00-INDEX.md's own closed vocabulary
	//   unearned-ratified-status        `Ratified` claimed over scenarios that execute nothing, or over none
	//   unreadable-acceptance           the census could not be taken — a failed measurement, said out loud
	Kind   string
	Detail string
}

// Report is the whole-lattice result.
type Report struct {
	SpecsChecked  int
	Requirements  int
	Referenced    int
	DeclaredGaps  int
	Findings      []Finding
	PerSpecPassed []string
	PerSpecFailed []string

	// Scenarios is the execution census: how many acceptance scenarios exist, and how many of them anything
	// actually runs. It is reported on every run, passing or failing — the original check printed neither
	// number and printed the word RATIFIED instead.
	Scenarios specCensus
	// StatusCounts is spec/00-INDEX.md's Status column tallied, so the tool's verdict and the index's claim
	// are always side by side rather than silently disagreeing.
	StatusCounts map[string]int
	// FullyExecuted names the specs with zero findings AND zero scenarios executing nothing — the only specs
	// whose evidence would support the index's `Ratified` row.
	FullyExecuted []string
	// SpecStatus is each spec's CURRENTLY CLAIMED status from spec/00-INDEX.md, kept per spec rather than
	// only tallied, so the report can print the ready set beside what the index says about each one. The
	// tally alone cannot answer "which of these is promotable", which is the only actionable question here.
	SpecStatus map[string]string
	// OracleFiles is how many oracle-bearing files the existence index walked. Printed because an index that
	// silently came back empty would make every `present` claim pass vacuously.
	OracleFiles int
}

// Clean reports whether the lattice has zero findings.
//
// It is deliberately NOT called Ratified. `Ratified` is spec/00-INDEX.md's terminal delivery status and no
// spec has reached it; this method answers a much narrower question — traceability plus honest execution
// claims. The old name is the whole reason this check needed fixing: it made a boolean about cross-references
// read as a statement about delivery, in a tool whose output a reviewer and CI both treat as proof.
func (r Report) Clean() bool { return len(r.Findings) == 0 }

// Check walks every spec directory under root and reports what is unratified.
func Check(root string) (Report, error) {
	var rep Report
	rep.StatusCounts = map[string]int{}
	dirs, err := filepath.Glob(filepath.Join(root, "spec", "0*"))
	if err != nil {
		return rep, err
	}
	sort.Strings(dirs)

	// Built ONCE for the whole run: every place an acceptance oracle could actually live. Without it, a
	// `present` mapping row naming a deleted test is indistinguishable from one naming a passing test.
	oi, err := newOracleIndex(root)
	if err != nil {
		return rep, fmt.Errorf("indexing oracles under %s: %w", root, err)
	}
	rep.OracleFiles = oi.files
	// spec/00-INDEX.md's Status column is the lattice's own delivery claim. A missing index is not a reason
	// to skip the comparison silently — statuses come back empty and every spec is reported as having no row.
	statuses, serr := specStatuses(root)
	if serr != nil {
		statuses = map[string]string{}
	}
	for _, dir := range dirs {
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		reqPath := filepath.Join(dir, "requirements.md")
		tasksPath := filepath.Join(dir, "tasks.json")
		if _, err := os.Stat(reqPath); err != nil {
			continue // not a requirement-bearing spec
		}
		name := filepath.Base(dir)
		rep.SpecsChecked++
		before := len(rep.Findings)

		reqBody, err := os.ReadFile(reqPath)
		if err != nil {
			return rep, err
		}
		declared := map[string]bool{}
		for _, m := range reqPattern.FindAllStringSubmatch(string(reqBody), -1) {
			declared[m[1]] = true
		}
		rep.Requirements += len(declared)

		var tf tasksFile
		if b, err := os.ReadFile(tasksPath); err == nil {
			if err := json.Unmarshal(b, &tf); err != nil {
				return rep, fmt.Errorf("%s: tasks.json: %w", name, err)
			}
		}

		referenced := map[string]bool{}
		for _, t := range tf.Tasks {
			for _, id := range t.ReqIDs {
				referenced[id] = true
			}
			if !knownStates[t.Status] {
				got := t.Status
				if got == "" {
					got = "(absent)"
				}
				rep.Findings = append(rep.Findings, Finding{
					Spec: name, Kind: "unknown-task-status",
					Detail: fmt.Sprintf("task %s has status %s, which is outside the closed set "+
						"[completed pending blocked] — an unrecognised status silently decides whether this "+
						"task's acceptance is checked (note: \"done\" is NOT accepted — \"completed\" is the one "+
						"canonical finished-state word)", t.ID, got),
				})
			}
			// A scenario is only OWED by a task that claims to be finished.
			if t.Acceptance == nil || t.Acceptance.Feature == "" || !doneStates[t.Status] {
				continue
			}
			featPath := filepath.Join(dir, "acceptance", t.Acceptance.Feature)
			featBody, ferr := os.ReadFile(featPath)
			if ferr != nil {
				rep.Findings = append(rep.Findings, Finding{
					Spec: name, Kind: "missing-scenario",
					Detail: fmt.Sprintf("task %s names feature %q, which does not exist", t.ID, t.Acceptance.Feature),
				})
				continue
			}
			present := map[string]bool{}
			for _, m := range scenarioPattern.FindAllStringSubmatch(string(featBody), -1) {
				present[strings.TrimSpace(m[1])] = true
			}
			for _, want := range t.Acceptance.Scenarios {
				if !present[strings.TrimSpace(want)] {
					rep.Findings = append(rep.Findings, Finding{
						Spec: name, Kind: "missing-scenario",
						Detail: fmt.Sprintf("task %s claims scenario %q in %s — the feature file has no such scenario, "+
							"so the task reads as covered and is not", t.ID, want, t.Acceptance.Feature),
					})
				}
			}
		}

		gaps := map[string]bool{}
		for _, g := range tf.RetrospectiveGaps {
			if strings.TrimSpace(g.Why) == "" {
				rep.Findings = append(rep.Findings, Finding{
					Spec: name, Kind: "empty-gap-reason",
					Detail: fmt.Sprintf("%s is declared a gap with no reason — a declaration without a WHY is "+
						"silence with extra steps", g.Req),
				})
				continue
			}
			gaps[g.Req] = true
		}
		rep.DeclaredGaps += len(gaps)

		var unref []string
		for id := range declared {
			if referenced[id] || gaps[id] {
				rep.Referenced++
				continue
			}
			unref = append(unref, id)
		}
		sort.Strings(unref)
		for _, id := range unref {
			rep.Findings = append(rep.Findings, Finding{
				Spec: name, Kind: "unreferenced-requirement",
				Detail: fmt.Sprintf("%s is declared law but no task references it and no retrospective_gap "+
					"declares its absence — it has no route to an oracle", id),
			})
		}

		// The execution census. Everything above answers "is this requirement REFERENCED?"; this answers the
		// question the word RATIFIED was standing in for — "does anything RUN?" — and it runs for every spec
		// on every invocation, so the number can never again be absent from the output.
		cs := rep.census(dir, name, oi)
		rep.Scenarios.add(cs)
		status := statuses[name]
		rep.StatusCounts[statusLabel(status)]++
		// Kept per spec as well as tallied (TG target table): the ready-set listing below prints each
		// spec beside what the index currently claims, and a tally cannot answer "which of these is
		// promotable".
		if rep.SpecStatus == nil {
			rep.SpecStatus = map[string]string{}
		}
		rep.SpecStatus[name] = statusLabel(status)
		rep.checkStatusClaim(name, status, cs, before)

		if len(rep.Findings) == before {
			rep.PerSpecPassed = append(rep.PerSpecPassed, name)
			// cs.Total > 0 is load-bearing, not defensive. A spec with no acceptance scenarios has
			// unexecuted() == 0 vacuously, so without this it was counted among the specs "whose evidence
			// would support Ratified" — the tool would have recommended the terminal delivery status for a
			// spec that has no evidence whatsoever. Zero of zero executing is not a clean bill of health.
			if cs.unexecuted() == 0 && cs.Total > 0 {
				rep.FullyExecuted = append(rep.FullyExecuted, name)
			}
		} else {
			rep.PerSpecFailed = append(rep.PerSpecFailed, name)
		}
	}
	// THE EMPTINESS GUARD. Every `present` claim above was adjudicated against this index. If the walk came
	// back with nothing while the lattice makes claims to adjudicate, the walk is broken — and a broken walk
	// does not report itself: it turns every claim into an "imaginary oracle" finding (a flood of confident,
	// wrong output) rather than saying the measurement failed. Absent is visible; skipped is not.
	if oi.files == 0 && rep.Scenarios.Claimed > 0 {
		return rep, fmt.Errorf("the oracle index walked 0 oracle-bearing files under %s while %d scenario(s) claim "+
			"a `present` oracle — the walk is broken, so every existence check below would be measuring nothing; "+
			"this is a failed measurement, not a lattice verdict", root, rep.Scenarios.Claimed)
	}
	return rep, nil
}

// statusLabel keeps an ABSENT index row visible in the tally instead of letting it vanish into a zero-length
// key that prints as blank beside three real statuses.
func statusLabel(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no index row)"
	}
	return s
}

// Render writes the report. Every count carries its denominator, and the execution census is printed on
// EVERY run — passing or failing.
//
// The line this replaced read "specs ratified: 28/28" followed by "RATIFIED", over a lattice where 107 of 531
// acceptance scenarios executed nothing and no spec had ever reached the index status `Ratified`. Nothing in
// the old output was false in isolation; the word was simply doing work the numbers did not support, and the
// numbers were not printed. So: the word `Ratified` now appears here only as spec/00-INDEX.md's status being
// quoted back, the tool's own verdict is stated as the narrow thing it is, and the count of scenarios that
// execute NOTHING is unconditional.
func (r Report) Render() string {
	var b strings.Builder
	sc := r.Scenarios
	fmt.Fprintf(&b, "specvalidate ratify — %d spec(s), %d requirement(s), %d acceptance scenario(s)\n",
		r.SpecsChecked, r.Requirements, sc.Total)
	b.WriteString("  REQUIREMENT REACHABILITY (does law have a route to an oracle?)\n")
	fmt.Fprintf(&b, "    with an oracle route or a declared gap:      %d/%d   (declared gaps: %d)\n",
		r.Referenced, r.Requirements, r.DeclaredGaps)
	fmt.Fprintf(&b, "    specs with no traceability finding:          %d/%d\n", len(r.PerSpecPassed), r.SpecsChecked)
	b.WriteString("  ACCEPTANCE EXECUTION (does anything actually RUN?)\n")
	fmt.Fprintf(&b, "    scenarios driven by an oracle in the tree:   %d/%d   (oracle index: %d files)\n",
		sc.Executing, sc.Total, r.OracleFiles)
	fmt.Fprintf(&b, "    scenarios that EXECUTE NOTHING:              %d/%d\n", sc.unexecuted(), sc.Total)
	fmt.Fprintf(&b, "      declared debt (_test_mapping.json pending): %d\n", sc.UnexecutedDeclared)
	// The undeclared half is the one that is a lie rather than a position, so it says so on the line itself —
	// a reader scanning only this block must not have to infer which number reds the build.
	claimedNote := ""
	if sc.UnexecutedUndeclared > 0 {
		claimedNote = "   <- each is a finding below"
	}
	fmt.Fprintf(&b, "      claimed `present`, nothing runs it:        %d%s\n", sc.UnexecutedUndeclared, claimedNote)
	fmt.Fprintf(&b, "    of the executing: gherkin skipped by ~@pending, behaviour carried elsewhere: %d\n",
		sc.SkippedButCarried)
	b.WriteString("  DELIVERY STATUS (spec/00-INDEX.md's own column, quoted — not this tool's verdict)\n")
	fmt.Fprintf(&b, "    %s\n", r.statusLine())
	fmt.Fprintf(&b, "    specs whose evidence would support `Ratified` (0 findings, 0 unexecuted): %d/%d\n",
		len(r.FullyExecuted), r.SpecsChecked)
	// NAME THEM. This line reported a COUNT and nothing else, and a count is not actionable: an operator
	// reading "14/28 would support Ratified" cannot promote a single one without re-deriving the set by
	// hand — which is the same defect this tool exists to catch, one level up. The names are already in
	// hand (FullyExecuted); only the printing was missing.
	//
	// Listed with the status the index CURRENTLY claims, because the gap is the point: a spec already
	// marked Ratified needs nothing, and one still at Draft or Approved is a promotion someone can make
	// today on evidence that already exists.
	if len(r.FullyExecuted) > 0 {
		ready := append([]string(nil), r.FullyExecuted...)
		sort.Strings(ready)
		for _, name := range ready {
			cur := r.SpecStatus[name]
			if cur == "" {
				cur = "(no status)"
			}
			note := ""
			if cur != "Ratified" {
				note = "  <- promotable on evidence that exists today"
			}
			fmt.Fprintf(&b, "      %-34s index says %-10s%s\n", name, cur, note)
		}
	}
	if len(r.Findings) == 0 {
		fmt.Fprintf(&b, "  TRACEABLE + HONEST — 0 findings; every requirement is reachable or its absence is "+
			"declared, and every `present` scenario names an oracle that exists. NOT a claim of ratification: "+
			"%d/%d scenarios still execute nothing.\n", sc.unexecuted(), sc.Total)
		return b.String()
	}
	byKind := map[string]int{}
	for _, f := range r.Findings {
		byKind[f.Kind]++
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Fprintf(&b, "  FAILED — %d finding(s); %d/%d acceptance scenarios execute nothing:\n",
		len(r.Findings), sc.unexecuted(), sc.Total)
	for _, k := range kinds {
		fmt.Fprintf(&b, "    %-32s %d\n", k, byKind[k])
	}
	b.WriteString("\n")
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  [%s] %s: %s\n", f.Kind, f.Spec, f.Detail)
	}
	return b.String()
}

// statusLine tallies spec/00-INDEX.md's Status column in the lattice's declared order, so a reader sees at a
// glance that `Ratified` is 0/28 in the same breath as any verdict this tool prints.
func (r Report) statusLine() string {
	var parts []string
	seen := map[string]bool{}
	for _, s := range []string{"Draft", "Approved", "Ratified"} {
		seen[s] = true
		parts = append(parts, fmt.Sprintf("%s %d/%d", s, r.StatusCounts[s], r.SpecsChecked))
	}
	var extra []string
	for s := range r.StatusCounts {
		if !seen[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	for _, s := range extra {
		parts = append(parts, fmt.Sprintf("%s %d/%d", s, r.StatusCounts[s], r.SpecsChecked))
	}
	return strings.Join(parts, " · ")
}
