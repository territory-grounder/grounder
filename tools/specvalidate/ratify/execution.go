package ratify

// This file exists because `ratify --check` printed the word RATIFIED, and "specs ratified: 28/28", over a
// lattice in which 89 of 531 acceptance scenarios executed nothing and 18 more named an oracle that had been
// DELETED from the tree. Neither number appeared anywhere in the output. The check underneath was honest about
// its own scope — it answers requirement->task->scenario-NAME traceability and nothing else — but the word it
// printed is the word spec/00-INDEX.md reserves for the terminal delivery state ("Status vocabulary (fixed):
// Draft -> Approved -> Ratified"), and no spec has ever reached it. A reader, a reviewer and CI all read
// "RATIFIED 28/28" as delivery. .gitlab-ci.yml already carries a comment warning humans not to fall into that
// exact trap; a comment is not a control, and the tool went on printing the word.
//
// So this file adds the denominator the word was hiding, and three failures that nothing in the lattice could
// see. All three are the same shape as every other reachability defect here — complete on one side of a
// boundary, unreferenced on the other, where the boundary is exactly what nobody checks:
//
//  1. AN IMAGINARY ORACLE. `_test_mapping.json` marks a scenario `present` and names the test that drives it.
//     Nothing lattice-wide ever checked that the named test EXISTS. Measured when this shipped: 18 scenarios
//     in spec/010 name vitest suites (contract.test.ts, controls.test.tsx, approval.test.tsx, ...) in the
//     React `frontend/` that was REMOVED on 2026-07-30 under ADR-0015. Those 18 read as executing coverage,
//     execute nothing, and had done so for as long as the removal. spec/023's own acceptance runner catches
//     this for spec/023 alone ("THE LOAD-BEARING CHECK") — the other 27 specs had no such runner.
//
//  2. A SCENARIO ITS OWN RUNNER SKIPS, MARKED `present`. Every godog runner in the lattice is built with
//     Tags: "~@pending", so a scenario tagged @pending in the .feature is SKIPPED — the suite stays green
//     without ever attempting it. A mapping row that marks such a scenario `present` while naming only its
//     own spec's runner claims execution from a test that is configured to walk past it. (Legitimate and
//     common today: @pending on the Gherkin because no step definitions were written, with `present` naming a
//     REAL oracle elsewhere — 40 rows across specs 023/026/028, each carried by a Go test or a Playwright
//     .mjs outside the acceptance package. Those execute; they are counted separately, never silently.)
//
//  3. A STATUS CLAIM THE EVIDENCE DOES NOT SUPPORT. spec/00-INDEX.md's Status column is prose no gate read.
//     A spec could be marked `Ratified` while half its scenarios executed nothing, and the only thing that
//     would have contradicted it was the validator printing RATIFIED at the same time.
//
// The counts below are printed on EVERY run, passing or failing, with their denominators. That is the point:
// the defect was never that debt exists — declared debt is a reviewable engineering position (INV-22 forbids
// an UNDECLARED gap, not an acknowledged one) — the defect was a confident word with no number beside it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// knownSpecStatuses is the CLOSED status vocabulary spec/00-INDEX.md fixes. Anything else — including an
// ABSENT row for a spec directory that exists on disk — is reported rather than skipped, because an
// unreadable status is indistinguishable from `Draft` to every check that consumes it, and the whole reason
// this file exists is that a claim nobody parses is a claim nobody can contradict.
var knownSpecStatuses = map[string]bool{"Draft": true, "Approved": true, "Ratified": true}

// knownMappingStatuses is the CLOSED set of `_test_mapping.json` row statuses. `present` asserts a runnable
// oracle drives real code; `pending` is declared, visible debt; `retrospective_gap` is behaviour that shipped
// with no driving test at all (INV-22 — it must be zero, and today it is).
var knownMappingStatuses = map[string]bool{"present": true, "pending": true, "retrospective_gap": true}

var (
	// indexSpecDirRe pulls the spec directory out of an index table row's link cell, e.g.
	// `| [spec/010](010-ux-console/) | ... | **Approved** |`.
	indexSpecDirRe = regexp.MustCompile(`\((\d{3}-[a-z0-9]+(?:-[a-z0-9]+)*)/\)`)

	// goFuncDeclRe matches a Go function declaration, method or plain, so a named oracle can be checked for
	// EXISTENCE rather than assumed. Anchored at column 0 because that is where a declaration lives; a
	// mention inside a comment or a string must not satisfy a coverage claim.
	goFuncDeclRe = regexp.MustCompile(`(?m)^func(?:\s+\([^)]*\))?\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

	// The three shapes a mapping row uses to name its oracle. The `test` field is deliberately free text (it
	// carries the WHY alongside the WHAT), so the oracle is extracted rather than parsed:
	//   - a repo path with a test-bearing extension: core/httpapi/proposals_test.go, e2e/candidates-ratify.mjs
	//   - a Go test function, optionally with a trailing * to mean a family: TestSessionStream*
	//   - a godog step-registration function, the lattice's fixed registerXxxSteps convention
	// oracleStepRefRe is deliberately NOT `register\w*` — the surrounding prose says "registered", "registers"
	// and would have manufactured oracle names out of English.
	oracleFileRefRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]*\.(?:go|ts|tsx|mjs|js)\b`)
	oracleTestRefRe = regexp.MustCompile(`\bTest[A-Za-z0-9_]*\*?`)
	oracleStepRefRe = regexp.MustCompile(`\bregister[A-Za-z0-9_]*Steps\b`)
)

// oracleDirSkip are directories the oracle index must NOT walk. node_modules is the load-bearing one: a
// third-party package shipping its own `contract.test.ts` would have resolved spec/010's claim to a file
// nobody in this repo wrote, and the imaginary-oracle check would have passed on vendored noise.
var oracleDirSkip = map[string]bool{".git": true, "node_modules": true, ".claude": true, "vendor": true}

// oracleIndex is every place in the tree an acceptance oracle could actually live, built once per run so a
// `present` claim can be checked against reality instead of taken at its word.
type oracleIndex struct {
	funcs map[string][]string // Go function name -> repo-relative files declaring it
	paths map[string]bool     // repo-relative slash path of every oracle-bearing file
	bases map[string][]string // basename -> repo-relative paths (mapping rows name bare files: sse.test.ts)
	files int
}

func newOracleIndex(root string) (*oracleIndex, error) {
	oi := &oracleIndex{funcs: map[string][]string{}, paths: map[string]bool{}, bases: map[string][]string{}}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not an oracle; the emptiness guard below catches a broken walk
		}
		if fi.IsDir() {
			if oracleDirSkip[fi.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		switch ext {
		case ".go", ".ts", ".tsx", ".mjs", ".js":
		default:
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		oi.paths[rel] = true
		base := filepath.Base(rel)
		oi.bases[base] = append(oi.bases[base], rel)
		oi.files++
		if ext != ".go" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, m := range goFuncDeclRe.FindAllStringSubmatch(string(b), -1) {
			oi.funcs[m[1]] = append(oi.funcs[m[1]], rel)
		}
		return nil
	})
	return oi, err
}

// oracleRef is one thing a mapping row names as the oracle that drives its scenario, plus where (if anywhere)
// that thing was found. `Where` is what makes failure 2 detectable: an oracle that lives inside the owning
// spec's own acceptance package is reached only by that spec's godog runner, which skips @pending scenarios.
//
// `Found` and `Reject` exist because "a thing by that name exists" turned out NOT to mean "an oracle exists",
// and the difference has to survive to the caller so it can be reported as the distinct lie it is rather than
// collapsed into "missing".
type oracleRef struct {
	Raw      string
	Resolved bool
	Where    []string // the TEST artifacts that satisfy this claim — the only things that may resolve it
	Found    []string // everything the name matched, test artifact or not; empty means genuinely absent
	Reject   string   // why a name that DID match is still not an oracle; "" when not rejected
}

// isTestArtifact reports whether a repo-relative path is a file that can actually ADJUDICATE a scenario.
//
// This exists because the existence check alone certifies nothing. A `present` row whose `test` field read
// `main.go` resolved cleanly and the whole lattice reported "TRACEABLE + HONEST — 0 findings", exit 0:
// main.go exists, so the claim "an oracle drives this scenario" was satisfied by a file that asserts nothing.
// That is the identical imaginary-oracle lie wearing a different symptom — the spec/010 rows were caught only
// because the deleted files happened not to exist, not because the check understood what an oracle IS.
//
// The three shapes below are every shape a real oracle takes in this lattice, confirmed against all 16 file
// tokens the mapping rows actually name today: Go tests (`_test.go`, which is the only file `go test` will
// even compile a test out of), vitest/jest suites (`.test.` / `.spec.`), and the Playwright journeys under
// `e2e/`, which are plain `.mjs` named for the journey rather than by a test-suffix convention.
func isTestArtifact(rel string) bool {
	base := filepath.Base(rel)
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
		if seg == "e2e" {
			return true
		}
	}
	return false
}

// testArtifactsOnly keeps just the matches that could run an assertion.
func testArtifactsOnly(in []string) []string {
	var out []string
	for _, p := range in {
		if isTestArtifact(p) {
			out = append(out, p)
		}
	}
	return out
}

// refs extracts every oracle a mapping row names and resolves each against the tree.
//
// Resolution is deliberately STRICTER than "something by that name exists", because two executed mutations
// proved the looser rule green-lights a lattice with no coverage at all:
//
//   - `"test": "main.go"` — resolves, asserts nothing. Closed by isTestArtifact.
//   - `"test": "Test*"` — the bare convention prefix every Go test in the repository shares, so it matched
//     every test function in the tree and satisfied every claim. It names the SUITE, which is to say it names
//     nothing in particular. A coverage claim has to name the one oracle that adjudicates the scenario;
//     a token that cannot fail to match is unfalsifiable, and an unfalsifiable claim is the defect this whole
//     tool exists to catch.
func (oi *oracleIndex) refs(test string) []oracleRef {
	var out []oracleRef
	seen := map[string]bool{}
	// add records one named oracle. `found` is everything the name matched; only the test artifacts among
	// them may resolve it, so a name that matches solely production code is REJECTED rather than accepted or
	// silently dropped — dropping it would let the row claim coverage by naming nothing checkable.
	add := func(raw string, found []string, reject string) {
		if seen[raw] {
			return
		}
		seen[raw] = true
		ref := oracleRef{Raw: raw, Found: found, Reject: reject}
		if reject == "" {
			ref.Where = testArtifactsOnly(found)
			if len(found) > 0 && len(ref.Where) == 0 {
				ref.Reject = fmt.Sprintf("%q exists in the tree but is not a test artifact, so it asserts nothing",
					found[0])
			}
		}
		ref.Resolved = ref.Reject == "" && len(ref.Where) > 0
		out = append(out, ref)
	}
	for _, f := range oracleFileRefRe.FindAllString(test, -1) {
		clean := strings.TrimPrefix(filepath.ToSlash(f), "./")
		switch {
		case oi.paths[clean]:
			add(clean, []string{clean}, "")
		case len(oi.bases[filepath.Base(clean)]) > 0 && !strings.Contains(clean, "/"):
			// A bare basename ("sse.test.ts") is how the console rows name their suites. Resolve it only
			// when the row gave no directory at all — a row that DID give a path and got it wrong is naming
			// a file that does not exist, which is exactly the claim under test.
			add(clean, oi.bases[filepath.Base(clean)], "")
		default:
			add(clean, nil, "")
		}
	}
	for _, fn := range append(oracleTestRefRe.FindAllString(test, -1), oracleStepRefRe.FindAllString(test, -1)...) {
		if strings.HasSuffix(fn, "*") {
			// `TestSessionStream*` names a family; the row is satisfied by any member of it. But `Test*`
			// carries NO discriminator beyond the prefix every Go test shares, so it selects the entire
			// suite. Naming everything is naming nothing, and it is rejected by name rather than by count:
			// there is no honest numeric threshold for "too broad", but there is an exact answer to "does
			// this token discriminate at all?".
			prefix := strings.TrimSuffix(fn, "*")
			if prefix == "Test" || prefix == "" {
				add(fn, nil, "`Test*` is the bare prefix EVERY Go test in this repository shares, so it "+
					"matches the entire suite and singles out no oracle at all")
				continue
			}
			var where []string
			for name, files := range oi.funcs {
				if strings.HasPrefix(name, prefix) {
					where = append(where, files...)
				}
			}
			sort.Strings(where)
			add(fn, where, "")
			continue
		}
		add(fn, oi.funcs[fn], "")
	}
	return out
}

// specStatuses reads spec/00-INDEX.md's table and returns spec-directory -> declared Status. A directory with
// no row comes back absent, which the caller reports: the index is the only place the lattice states delivery
// status, so a spec missing from it is a spec whose claim cannot be contradicted.
func specStatuses(root string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "spec", "00-INDEX.md"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		m := indexSpecDirRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := strings.Split(line, "|")
		status := ""
		for i := len(cells) - 1; i >= 0; i-- {
			c := strings.TrimSpace(strings.Trim(strings.TrimSpace(cells[i]), "*"))
			if c != "" {
				status = c
				break
			}
		}
		out[m[1]] = status
	}
	return out, nil
}

// featureScenario is one scenario as the GODOG RUNNER sees it: its name, and whether the @pending tag that
// every runner in this lattice filters out (Tags: "~@pending") sits above it.
type featureScenario struct {
	Name       string
	PendingTag bool
	Feature    string
}

// featureScenarios reads every acceptance/*.feature in a spec directory.
func featureScenarios(dir string) ([]featureScenario, error) {
	files, err := filepath.Glob(filepath.Join(dir, "acceptance", "*.feature"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []featureScenario
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			return nil, rerr
		}
		lines := strings.Split(string(b), "\n")
		for i, ln := range lines {
			m := scenarioPattern.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			out = append(out, featureScenario{
				Name:       strings.TrimSpace(m[1]),
				PendingTag: hasPendingTag(lines, i),
				Feature:    filepath.Base(f),
			})
		}
	}
	return out, nil
}

// hasPendingTag reports whether the Gherkin tag block directly above line i carries @pending. It walks up
// through tag and comment lines only and stops at the first line of anything else, so a @pending on an
// unrelated scenario further up cannot be attributed to this one.
func hasPendingTag(lines []string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "@") && !strings.HasPrefix(t, "#") {
			return false
		}
		for _, tag := range strings.Fields(t) {
			if tag == "@pending" {
				return true
			}
		}
	}
	return false
}

type mappingRow struct {
	Name   string `json:"name"`
	Req    string `json:"req"`
	Status string `json:"status"`
	Test   string `json:"test"`
}

type mappingDoc struct {
	Feature   string       `json:"feature"`
	Scenarios []mappingRow `json:"scenarios"`
}

// census joins one spec's .feature scenarios to its _test_mapping.json rows and decides, per scenario,
// whether anything executes. It appends a finding for every claim the tree does not support and returns the
// counts that must appear in the output whether or not it failed.
func (r *Report) census(dir, name string, oi *oracleIndex) specCensus {
	var cs specCensus
	scenarios, err := featureScenarios(dir)
	if err != nil {
		// NOT a silent return. An unreadable acceptance directory produces the same zero census as a spec
		// with genuinely no scenarios, and a zero census is indistinguishable from "nothing to complain
		// about" everywhere downstream — the exact no-op-without-saying-so shape this tool exists to catch.
		// Say the measurement failed, and fail on it.
		r.Findings = append(r.Findings, Finding{
			Spec: name, Kind: "unreadable-acceptance",
			Detail: fmt.Sprintf("acceptance/*.feature could not be read (%v) — this spec's execution census is a "+
				"FAILED MEASUREMENT, not a clean result, and it would otherwise be reported as zero scenarios", err),
		})
		return cs
	}
	cs.Total = len(scenarios)
	if cs.Total == 0 {
		return cs
	}

	rows := map[string]mappingRow{}
	mapPath := filepath.Join(dir, "acceptance", "_test_mapping.json")
	if b, rerr := os.ReadFile(mapPath); rerr == nil {
		var doc mappingDoc
		if json.Unmarshal(b, &doc) == nil {
			for _, row := range doc.Scenarios {
				rows[strings.TrimSpace(row.Name)] = row
			}
		}
	}

	// A spec's own acceptance package is reached ONLY by that spec's godog runner, and every runner in this
	// lattice is constructed with Tags: "~@pending". An oracle that lives there therefore does not execute a
	// @pending-tagged scenario, no matter what the mapping says.
	ownAcceptance := "spec/" + name + "/acceptance/"

	for _, sc := range scenarios {
		row, mapped := rows[sc.Name]
		if !mapped {
			cs.UnexecutedUndeclared++
			r.Findings = append(r.Findings, Finding{
				Spec: name, Kind: "unmapped-scenario",
				Detail: fmt.Sprintf("scenario %q (%s) has NO row in _test_mapping.json — its execution status is "+
					"unrecorded, which is how a feature file comes to read as coverage it does not have", sc.Name, sc.Feature),
			})
			continue
		}
		if row.Status != "present" {
			// pending / retrospective_gap: DECLARED debt. Not a finding — a stated position can be reviewed.
			// It is counted and printed on every run so the declaration can never pass for delivery.
			cs.UnexecutedDeclared++
			continue
		}
		cs.Claimed++

		refs := oi.refs(row.Test)
		var missing, rejected []string
		executes := false
		for _, ref := range refs {
			if !ref.Resolved {
				// Absent and present-but-not-an-oracle are DIFFERENT lies and are reported apart: the first
				// is a stale name, the second is a name chosen (or left) so that it cannot fail to match.
				if ref.Reject != "" {
					rejected = append(rejected, fmt.Sprintf("%s (%s)", ref.Raw, ref.Reject))
				} else {
					missing = append(missing, ref.Raw)
				}
				continue
			}
			if !sc.PendingTag {
				executes = true
				continue
			}
			// The scenario is skipped by its own runner's ~@pending filter, so only an oracle OUTSIDE that
			// runner can be executing it.
			for _, w := range ref.Where {
				if !strings.HasPrefix(w, ownAcceptance) {
					executes = true
					break
				}
			}
		}
		switch {
		case len(refs) == 0:
			cs.UnexecutedUndeclared++
			r.Findings = append(r.Findings, Finding{
				Spec: name, Kind: "imaginary-oracle",
				Detail: fmt.Sprintf("scenario %q is marked `present` but its test field names NO oracle this tool "+
					"can locate (%q) — `present` asserts a runnable test drives real code, so it must say which one",
					sc.Name, truncate(row.Test)),
			})
		case len(missing) > 0:
			cs.UnexecutedUndeclared++
			r.Findings = append(r.Findings, Finding{
				Spec: name, Kind: "imaginary-oracle",
				Detail: fmt.Sprintf("scenario %q (%s) is marked `present` naming oracle(s) %s, which do NOT EXIST "+
					"in the tree — the scenario reads as executing coverage and executes nothing",
					sc.Name, row.Req, strings.Join(missing, ", ")),
			})
		case len(rejected) > 0:
			cs.UnexecutedUndeclared++
			r.Findings = append(r.Findings, Finding{
				Spec: name, Kind: "non-test-oracle",
				Detail: fmt.Sprintf("scenario %q (%s) is marked `present`, but %s. `present` asserts a RUNNABLE test "+
					"drives real code; a name that resolves to production code, or one that matches the whole suite, "+
					"satisfies the existence check while adjudicating nothing — the same imaginary coverage as a "+
					"deleted oracle, wearing a name that cannot fail to match",
					sc.Name, row.Req, strings.Join(rejected, "; ")),
			})
		case !executes:
			cs.UnexecutedUndeclared++
			r.Findings = append(r.Findings, Finding{
				Spec: name, Kind: "skipped-scenario-claimed-present",
				Detail: fmt.Sprintf("scenario %q (%s) is tagged @pending in %s — every runner in this lattice is "+
					"built with Tags: \"~@pending\", so the suite SKIPS it — yet it is marked `present` naming only "+
					"%s, which lives in that same skipped runner. The named test runs, walks past this scenario, and "+
					"stays green", sc.Name, row.Req, sc.Feature, row.Test),
			})
		default:
			cs.Executing++
			if sc.PendingTag {
				// Executes, but NOT as Gherkin: the runner skips it and a named oracle elsewhere carries the
				// behaviour. Legitimate, and counted apart so "executing" is never read as "the scenario ran".
				cs.SkippedButCarried++
			}
		}
	}
	return cs
}

// specCensus is one spec's execution arithmetic. Every field is a numerator whose denominator is Total.
type specCensus struct {
	Total                int
	Claimed              int // rows asserting `present` — the claims the oracle index has to adjudicate
	Executing            int
	UnexecutedDeclared   int // status pending/retrospective_gap — declared, reviewable debt
	UnexecutedUndeclared int // claimed `present`, nothing executes it — each one is also a Finding
	SkippedButCarried    int // Gherkin skipped by ~@pending, behaviour carried by a named oracle elsewhere
}

func (c specCensus) unexecuted() int { return c.UnexecutedDeclared + c.UnexecutedUndeclared }

func (c *specCensus) add(o specCensus) {
	c.Total += o.Total
	c.Claimed += o.Claimed
	c.Executing += o.Executing
	c.UnexecutedDeclared += o.UnexecutedDeclared
	c.UnexecutedUndeclared += o.UnexecutedUndeclared
	c.SkippedButCarried += o.SkippedButCarried
}

// checkStatusClaim compares spec/00-INDEX.md's Status column against what the lattice can actually show.
//
// `Ratified` is the index's own terminal state and it defines it: "every non-@pending requirement has a
// `present` acceptance oracle". A spec that claims it while any scenario executes nothing is asserting
// delivery the tree does not support — and until now the validator AGREED with it, printing RATIFIED beside
// the claim. `Draft` and `Approved` make no execution claim, so debt under them is not a finding: it is
// counted, printed, and left to the reader.
func (r *Report) checkStatusClaim(name, status string, cs specCensus, findingsBefore int) {
	if !knownSpecStatuses[status] {
		got := status
		if got == "" {
			got = "(no row in spec/00-INDEX.md)"
		}
		r.Findings = append(r.Findings, Finding{
			Spec: name, Kind: "unknown-spec-status",
			Detail: fmt.Sprintf("declares status %s, outside the closed vocabulary [Draft Approved Ratified] that "+
				"spec/00-INDEX.md fixes — an unparseable status is a delivery claim no gate can contradict", got),
		})
		return
	}
	if status != "Ratified" {
		return
	}
	// THE VACUOUS CLAIM. A spec with NO acceptance scenarios has unexecuted() == 0, because zero of zero
	// execute nothing — so the arithmetic below waved it through, and `FullyExecuted` went further and counted
	// it among the specs "whose evidence would support Ratified". Demonstrated: a spec directory with
	// requirements, no acceptance/ at all, and a **Ratified** row in spec/00-INDEX.md passed with 0 findings
	// and exit 0. That inverts the entire point of the gate — total absence of evidence scored BETTER than
	// partial evidence, which is this repository's signature defect (a control that no-ops without saying so)
	// reproduced inside the control built to catch it. Ratified means every non-@pending requirement HAS a
	// present acceptance oracle; zero oracles cannot satisfy "every".
	if cs.Total == 0 {
		r.Findings = append(r.Findings, Finding{
			Spec: name, Kind: "unearned-ratified-status",
			Detail: "spec/00-INDEX.md marks this spec **Ratified**, but it has NO acceptance scenarios at all — " +
				"there is nothing here that could execute, so the terminal delivery status rests on an absence of " +
				"evidence rather than on evidence. Ratified requires that every non-@pending requirement have a " +
				"present acceptance oracle; a spec with zero scenarios has none",
		})
		return
	}
	if cs.unexecuted() == 0 && len(r.Findings) == findingsBefore {
		return
	}
	r.Findings = append(r.Findings, Finding{
		Spec: name, Kind: "unearned-ratified-status",
		Detail: fmt.Sprintf("spec/00-INDEX.md marks this spec **Ratified**, but %d of its %d acceptance scenarios "+
			"execute nothing (%d declared pending, %d claimed `present` with no oracle in the tree) and it carries "+
			"%d finding(s) of its own. Ratified is the index's own terminal state — it means every non-@pending "+
			"requirement has a present acceptance oracle — so this row claims delivery the lattice cannot show",
			cs.unexecuted(), cs.Total, cs.UnexecutedDeclared, cs.UnexecutedUndeclared, len(r.Findings)-findingsBefore),
	})
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 70 {
		return s
	}
	return s[:70] + "…"
}
