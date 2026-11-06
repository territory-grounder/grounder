package deploy_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/wiring"
)

// TestNotifierWiringDeclaresInEveryBranch mechanically encodes "an if/else-if with no else is a dark
// path", which is the exact shape of the defect this whole seam exists to prevent: with zero notifiers
// configured NOTHING ran, deps.Notify stayed nil, and a judge-death page that fired on 2026-08-01
// degraded to log.Printf on stdout — reaching no operator, and reported by nothing.
//
// The rule: in the conditional that switches on notifierCount, EVERY branch — including the default —
// must record at the gov.notify seam, via wiring.Bind or wiring.Absent. A branch that records nothing is
// a branch that leaves the sink in whatever state it was, silently.
//
// It lives in deploy/ rather than cmd/worker/ for the same reason envparity_test.go does:
// cmd/worker/main.go is lockstep-governed by spec/012, so a guard sited under a governed path would force
// a spec re-stamp merely to add a test.
//
// KILLING MUTATION: delete the `default:` arm from the switch in cmd/worker/main.go. Two of three
// branches then declare, and this fails.
//
// CONTROL MUTATION (why the vacuity floor below is mandatory): restrict the call-target walk to
// *ast.Ident. `wiring.Bind` is a SelectorExpr, so zero sites would match and the test would pass having
// checked nothing. The repo has paid for this exact mistake before — cmd/worker/axis_wiring_test.go:28
// records a first draft that "bailed out on every non-Ident call… It failed loudly rather than passing
// vacuously, which is the only reason the mistake was cheap." Hence: zero recorded sites is a FAILURE.
func TestNotifierWiringDeclaresInEveryBranch(t *testing.T) {
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var (
		found        bool
		branches     int
		declaring    int
		hasDefault   bool
		totalRecords int
	)

	// records reports whether a statement list mentions a wiring record for the gov.notify seam. The walk
	// descends into nested statements, because the record may sit inside an if/else within the branch (the
	// resolve-error arm does exactly that).
	records := func(stmts []ast.Stmt) bool {
		hit := false
		for _, st := range stmts {
			ast.Inspect(st, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calleeName(call.Fun)
				// Count every wiring record anywhere in the file for the vacuity floor.
				if name == "wiring.Bind" || name == "wiring.Absent" {
					totalRecords++
					hit = true
				}
				// The notifier branches route their Absent through a local closure for readability; a call
				// to it is equally a declaration.
				if name == "darkNotify" {
					hit = true
				}
				return true
			})
		}
		return hit
	}

	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil {
			return true
		}
		// Locate the switch whose cases test notifierCount.
		mentions := false
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				if strings.Contains(exprText(e), "notifierCount") {
					mentions = true
				}
			}
		}
		if !mentions {
			return true
		}
		found = true
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			branches++
			if cc.List == nil { // `default:`
				hasDefault = true
			}
			if records(cc.Body) {
				declaring++
			}
		}
		return false
	})

	if !found {
		t.Fatal("could not find the notifier conditional in cmd/worker/main.go — if it was refactored, " +
			"this guard must be updated deliberately, not deleted")
	}
	// VACUITY FLOOR: if the walk matched no wiring calls at all, the test proved nothing and must fail
	// loudly rather than pass clean. This is the control-mutation defense described above.
	if totalRecords == 0 {
		t.Fatal("vacuity floor: the AST walk found ZERO wiring.Bind/wiring.Absent call sites — the " +
			"matcher is broken, and a passing run here would certify nothing")
	}
	if !hasDefault {
		t.Fatal("the notifier switch has no default arm: the un-enumerated case is precisely how " +
			"deps.Notify stayed nil and silent")
	}
	if declaring != branches {
		t.Fatalf("every branch of the notifier switch must record at the gov.notify seam: %d of %d do. "+
			"A branch that records nothing leaves the sink in whatever state it was, silently.",
			declaring, branches)
	}
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			return x.Name + "." + f.Sel.Name
		}
		return f.Sel.Name
	case *ast.IndexExpr: // generic instantiation: wiring.Absent[T](...)
		return calleeName(f.X)
	case *ast.IndexListExpr:
		return calleeName(f.X)
	}
	return ""
}

func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		return exprText(v.X) + v.Op.String() + exprText(v.Y)
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.CallExpr:
		return calleeName(v.Fun)
	}
	return ""
}

// TestWiringReportIsTakenAfterEveryBind pins an ordering that is invisible in review and produced a
// FALSE POSITIVE in production on the feature's very first run.
//
// The manifest is written by Bind/Absent as the composition root executes, and read once by Report. If
// Report is called before a seam's Bind, that seam reports "dark-unrecorded" while being perfectly
// wired. That is exactly what happened: escalation.page was reported dark at 02:06:24 in the live
// worker while its Bind sat 23 lines further down the function.
//
// A false positive is the worst failure mode a detector can have. A missed defect is a gap; a detector
// that cries wolf gets ignored, and then it cannot report the real one either.
//
// KILLING MUTATION: move the wiringManifest.Report(...) call above any wiring.Bind site. RED.
func TestWiringReportIsTakenAfterEveryBind(t *testing.T) {
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var recordLines []int
	reportLine := -1
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch calleeName(call.Fun) {
		case "wiring.Bind", "wiring.Absent":
			recordLines = append(recordLines, fset.Position(call.Pos()).Line)
		case "wiringManifest.Report":
			reportLine = fset.Position(call.Pos()).Line
		}
		return true
	})

	// VACUITY FLOOR — the same defense the branch guard carries, and for the same reason: a matcher that
	// silently stops matching would otherwise pass having certified nothing.
	if len(recordLines) == 0 {
		t.Fatal("vacuity floor: no wiring.Bind/wiring.Absent sites found — the matcher is broken")
	}
	if reportLine < 0 {
		t.Fatal("no wiringManifest.Report call found: the manifest is written and never read, which is " +
			"a dark detector — the disease, applied to the cure")
	}
	for _, ln := range recordLines {
		if ln > reportLine {
			t.Errorf("a wiring record at line %d runs AFTER Report at line %d: that seam will be reported "+
				"dark-unrecorded while it is in fact bound — a false positive, which is how a detector "+
				"gets ignored", ln, reportLine)
		}
	}
}

// TestEveryWiringConditionalDeclaresInAllBranches generalises the notifier-specific guard above into the
// invariant it was always an instance of:
//
//	IF a conditional records at a wiring seam in ANY branch, it must record in EVERY branch —
//	including the implicit else.
//
// A branch that records nothing leaves the seam in whatever state it was, silently. That is not a
// hypothetical: it is how BOTH live dark components were shipped. The notifier's `if/else-if` had no
// else, so a zero-notifier deployment recorded nothing and a judge-death page reached a log file. The
// lessons feed's `if src != ""` had no else, so with the feed unset the precedent corpus silently froze
// while the boot log still reported "corpus loaded — 670 prior incidents".
//
// This guard exists because the specific one did NOT catch the second case: deleting the lessons `else`
// passed the entire suite. A rule written for one instance is not a rule.
//
// KILLING MUTATION: delete any `else` branch that pairs with a wiring record. RED, naming the line.
func TestEveryWiringConditionalDeclaresInAllBranches(t *testing.T) {
	for _, root := range compositionRoots() {
		t.Run(filepath.Base(filepath.Dir(root)), func(t *testing.T) { checkWiringBranches(t, root) })
	}
}

func checkWiringBranches(t *testing.T, path string) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// recordsIn reports whether a subtree contains a wiring record, and counts them for the floor.
	total := 0
	recordsIn := func(n ast.Node) bool {
		if n == nil {
			return false
		}
		hit := false
		ast.Inspect(n, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			// A seam is recorded either directly, or through a local `darkXxx` closure that wraps
			// wiring.Absent (every lane declares one so its Because is written once). This USED to be a
			// hardcoded list — `"darkNotify", "darkLessons"` — which meant the guard silently stopped
			// covering the next lane to arrive: a third lane's `darkWiki` else-branch read as "records
			// nothing", and the only signal was this guard failing on correct code. A guard keyed to the
			// instances it was written for is the exact failure its own doc comment warns about, so the
			// convention is now matched by shape. darkClosuresAreHonest below stops the name being a lie.
			name := calleeName(call.Fun)
			switch {
			case name == "wiring.Bind", name == "wiring.Absent":
				total++
				hit = true
			case isDarkHelper(name):
				hit = true
			}
			return true
		})
		return hit
	}

	var problems []string
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if !recordsIn(ifs.Body) {
			return true // this conditional is not a wiring seam site
		}
		// The then-branch records, so the else must exist AND record.
		if ifs.Else == nil {
			problems = append(problems, fmt.Sprintf(
				"line %d: a conditional records at a wiring seam but has NO else — with the condition "+
					"false, nothing is recorded and the seam goes silently dark (this is exactly how the "+
					"notifier and the lessons feed both shipped)", fset.Position(ifs.Pos()).Line))
			return true
		}
		if !recordsIn(ifs.Else) {
			problems = append(problems, fmt.Sprintf(
				"line %d: the else branch records nothing at the seam its if-branch records at",
				fset.Position(ifs.Pos()).Line))
		}
		return true
	})

	// A root with NO seams yet is legitimate — cmd/grounder has none today. Say so rather than passing
	// silently, so "this file has no seams" and "the matcher stopped matching" stay distinguishable. The
	// cross-root floor below is what actually catches a broken matcher.
	if total == 0 {
		t.Logf("%s declares no wiring seams yet — nothing to check here (this is a coverage GAP, not a pass)", path)
		return
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// compositionRoots are the files where capabilities are bound to real values. The wiring guards walk ALL
// of them, because a rule that checks one file is a rule with a coverage hole — and this one had exactly
// that: every guard hardcoded cmd/worker/main.go while cmd/grounder/deps.go, the API composition root
// that once served a permanently-dead /v1/proposals route with every unit test green, was unchecked.
//
// Adding a root here is how a new binary gets covered; forgetting to is caught by the vacuity floor only
// if NO root has seams, so keep this list honest.
func compositionRoots() []string {
	return []string{
		filepath.Join("..", "cmd", "worker", "main.go"),
		filepath.Join("..", "cmd", "grounder", "deps.go"),
	}
}

// TestAtLeastOneCompositionRootDeclaresSeams is the CROSS-ROOT vacuity floor.
//
// The per-root check now returns early when a file declares no seams, which is legitimate today but would
// also be the symptom of a matcher that silently stopped matching everywhere. This asserts the whole set
// is not vacuous, so the guards cannot all pass having checked nothing.
//
// KILLING MUTATION: break calleeName (return "" for SelectorExpr). Every root then reports "no seams" and
// this fails, instead of the suite going green over a dead matcher.
func TestAtLeastOneCompositionRootDeclaresSeams(t *testing.T) {
	found := 0
	for _, root := range compositionRoots() {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, root, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", root, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch calleeName(call.Fun) {
				case "wiring.Bind", "wiring.Absent":
					found++
				}
			}
			return true
		})
	}
	if found == 0 {
		t.Fatalf("vacuity floor: NO composition root declares a wiring seam. Either the seams were all "+
			"deleted, or calleeName stopped matching — in both cases every wiring guard is now passing "+
			"over nothing. Roots checked: %v", compositionRoots())
	}
	t.Logf("wiring seams declared across %d composition root(s): %d call site(s)", len(compositionRoots()), found)
}

// isDarkHelper matches the `darkXxx` convention every wiring lane uses for its Absent wrapper.
func isDarkHelper(name string) bool {
	if !strings.HasPrefix(name, "dark") || len(name) < 5 {
		return false
	}
	c := name[4]
	return c >= 'A' && c <= 'Z'
}

// TestDarkHelpersActuallyRecord — the floor under isDarkHelper. Matching a NAME is only safe while the
// name cannot lie: without this, a lane could satisfy the branch guard with `darkAnything := func(string){}`
// that records nothing, and the else-branch rule would be enforced in spelling only.
//
// KILLING MUTATION: give a darkXxx closure an empty body. RED, naming the helper.
func TestDarkHelpersActuallyRecord(t *testing.T) {
	for _, root := range compositionRoots() {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, root, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", root, err)
		}
		found := 0
		ast.Inspect(file, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			id, ok := as.Lhs[0].(*ast.Ident)
			if !ok || !isDarkHelper(id.Name) {
				return true
			}
			lit, ok := as.Rhs[0].(*ast.FuncLit)
			if !ok {
				return true
			}
			found++
			records := false
			ast.Inspect(lit.Body, func(x ast.Node) bool {
				if call, ok := x.(*ast.CallExpr); ok && calleeName(call.Fun) == "wiring.Absent" {
					records = true
				}
				return true
			})
			if !records {
				t.Errorf("%s:%d: %s looks like a wiring dark-helper but never calls wiring.Absent — the "+
					"branch guard would accept it while nothing is recorded",
					filepath.Base(root), fset.Position(as.Pos()).Line, id.Name)
			}
			return true
		})
		if found == 0 && filepath.Base(filepath.Dir(root)) == "worker" {
			t.Errorf("VACUITY: no darkXxx helpers found in %s — isDarkHelper matched nothing, so the "+
				"branch guard is relying on a convention this tree no longer uses", root)
		}
	}
}

// TestMultiNotifierBindsAFanoutRatherThanGoingDark pins the property that closed MECH-719.
//
// For as long as the notifier seam existed, "more than one notifier enabled" was a DARK branch: the
// worker declared the seam absent and delivered nothing at all. Its own comment conceded the shape —
// "strictly worse than one" — and an operator who configured matrix AND SMS AND email, buying
// redundancy on the page that wakes them, bought silence, with each added channel making the silence
// more certain.
//
// The branch test above cannot catch a regression here: darkNotify IS a declaration, so reverting the
// fan-out to an Absent would keep every branch "declaring" and pass clean. What must be pinned is that
// the multi-channel path BINDS A REAL SINK.
//
// KILLING MUTATION: replace the notifier.NewFanout(...) branch body with a darkNotify(...) call. RED.
//
// VACUITY FLOOR: the switch must be found and must contain at least one wiring.Bind; a walk that
// matched nothing would certify nothing.
func TestMultiNotifierBindsAFanoutRatherThanGoingDark(t *testing.T) {
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var (
		foundSwitch bool
		binds       int
		fanoutArm   bool
	)
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil {
			return true
		}
		mentions := false
		for _, c := range sw.Body.List {
			if cc, ok := c.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if strings.Contains(exprText(e), "notifierCount") {
						mentions = true
					}
				}
			}
		}
		if !mentions {
			return true
		}
		foundSwitch = true
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			// Does this arm construct a fan-out AND bind it at the seam? Both, in the same arm: a
			// constructed fan-out that is never bound delivers exactly as much as no fan-out at all,
			// which is the pathology this whole seam set exists to detect.
			built, bound := false, false
			for _, st := range cc.Body {
				ast.Inspect(st, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch calleeName(call.Fun) {
					case "notifier.NewFanout":
						built = true
					case "wiring.Bind":
						binds++
						bound = true
					}
					return true
				})
			}
			if built && bound {
				fanoutArm = true
			}
		}
		return false
	})

	if !foundSwitch {
		t.Fatal("could not find the notifier conditional in cmd/worker/main.go — if it was refactored, " +
			"this guard must be updated deliberately, not deleted")
	}
	if binds == 0 {
		t.Fatal("vacuity floor: the walk found ZERO wiring.Bind sites inside the notifier switch — the " +
			"matcher is broken and a passing run would certify nothing")
	}
	if !fanoutArm {
		t.Fatal("no arm of the notifier switch both constructs a notifier.NewFanout AND binds it at the " +
			"gov.notify seam. With several notifiers enabled the worker therefore delivers to at most " +
			"one of them — or, as it did until 2026-08-01, to none at all: an operator who configures " +
			"three channels for redundancy gets silence, and adding channels makes it worse.")
	}
}

// TestTrackerHistoryKeysOnCapabilityNotVendor pins the fix for a gap that hid for as long as the feature
// existed.
//
// get-tracker-history gives the agent the incident record that PREDATES TG — how the engineers already
// working at a site solved this exact fault, in their words, on their machines. It is the richest source
// of estate-specific knowledge available on day one, and the only one that exists before TG has done
// anything at all.
//
// It was gated behind `regn.Adapter.(*youtrack.Module)`. Every other tracker backend — ServiceNow, Jira,
// GitHub Issues, all implementing the same four-verb contract — fell through to an else arm that logged
// "no tracker configured". That sentence was FALSE: a tracker WAS configured, it just was not that one.
// A ServiceNow site therefore ran TG on its own weeks of session history while a decade of its own
// incident record sat one API call away, and the log line said everything was as expected.
//
// The rule this encodes: the composition root may not name a concrete tracker VENDOR type when deciding
// whether history is available. It must ask for the capability.
//
// KILLING MUTATION: restore the `*youtrack.Module` type assertion. RED.
//
// VACUITY FLOOR: the file must contain the capability assertion; a walk that found neither form would
// certify nothing.
func TestTrackerHistoryKeysOnCapabilityNotVendor(t *testing.T) {
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Concrete tracker module types. Asserting on any of these to decide a capability is the defect.
	vendorTypes := map[string]bool{
		"youtrack.Module": true, "servicenow.Module": true,
		"jira.Module": true, "githubissues.Module": true,
	}
	var (
		vendorAsserts []string
		capabilityHit bool
	)
	ast.Inspect(file, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok || ta.Type == nil {
			return true
		}
		name := typeName(ta.Type)
		if vendorTypes[name] {
			vendorAsserts = append(vendorAsserts, fmt.Sprintf("%s at line %d", name, fset.Position(ta.Pos()).Line))
		}
		if name == "tracker.History" {
			capabilityHit = true
		}
		return true
	})

	if !capabilityHit {
		t.Fatal("vacuity floor: cmd/worker/main.go contains no assertion on tracker.History — either the " +
			"capability wiring was removed (tracker history is dark for every backend) or this matcher is " +
			"broken, and a passing run would certify nothing")
	}
	if len(vendorAsserts) > 0 {
		t.Fatalf("the composition root decides a tracker CAPABILITY by asserting a concrete vendor type "+
			"(%s). Every other configured tracker is then excluded by construction and told 'no tracker "+
			"configured' — which is false, and is exactly how a ServiceNow site ran blind on its own "+
			"incident history. Assert adapters/tracker.History instead.", strings.Join(vendorAsserts, ", "))
	}
}

// typeName renders an assertion's target type as pkg.Name, including through a pointer.
func typeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return typeName(v.X)
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// TestTrackerWiringDeclaresInEveryBranch is the notifier guard's twin, for the seam whose darkness is
// the more expensive of the two.
//
// The entry ticket feeds FOUR capabilities: the investigation reads it, the terminal reconcile close-out
// transitions it, the learned scheduled-reboot lane consults it, and the dedup stage asks it whether a
// parent incident is still open. All four were bound behind `if trackerCount == 1`, with no else arm and
// no seam. Two configured trackers — ServiceNow for ITSM and YouTrack for engineering work, the ordinary
// shape at an established site — took all four dark simultaneously and nothing recorded it.
//
// The fourth WAS a WRONG ANSWER until TG-354: with gate.OpenIssue nil, core/suppression/dedup.go returned
// OutcomeSuppressed for a re-fire whose parent ticket had already RESOLVED, giving the reason "duplicate of
// an open incident within window" and asserting an openness nothing checked. The dedup stage now fails
// toward surfacing (a suppression must be BACKED by a confirmed-open parent), so that case escalates; this
// guard still pins that every branch of the tracker switch DECLARES at the seam.
//
// KILLING MUTATION: delete the `default:` arm of the tracker switch in cmd/worker/main.go. RED.
func TestTrackerWiringDeclaresInEveryBranch(t *testing.T) {
	branches, declaring, hasDefault, records := switchSeamCoverage(t, "trackerCount",
		map[string]bool{"wiring.Bind": true, "wiring.Absent": true, "darkTracker": true})

	if records == 0 {
		t.Fatal("vacuity floor: the AST walk found ZERO wiring records in the tracker switch — the matcher " +
			"is broken and a passing run would certify nothing")
	}
	if !hasDefault {
		t.Fatal("the tracker switch has no default arm: the un-enumerated case is exactly how four " +
			"capabilities went dark at once with nothing recording it")
	}
	if declaring != branches {
		t.Fatalf("every branch of the tracker switch must record at the %s seam: %d of %d do",
			"tracker.entry", declaring, branches)
	}
}

// TestMultiTrackerIsBoundNotDark pins that several configured trackers ROUTE rather than go dark — the
// same regression class the notifier fan-out guard covers, and undetectable by the branch guard above
// because darkTracker() is itself a declaration and would keep every branch "declaring".
//
// KILLING MUTATION: replace the tracker.NewMultiTracker(...) arm body with a darkTracker(...) call. RED.
func TestMultiTrackerIsBoundNotDark(t *testing.T) {
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var built, bound bool
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil || !switchMentions(sw, "trackerCount") {
			return true
		}
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			armBuilt, armBound := false, false
			for _, st := range cc.Body {
				ast.Inspect(st, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch calleeName(call.Fun) {
					case "tracker.NewMultiTracker":
						armBuilt = true
					case "wiring.Bind":
						armBound = true
					}
					return true
				})
			}
			if armBuilt && armBound {
				built, bound = true, true
			}
		}
		return false
	})
	if !built || !bound {
		t.Fatal("no arm of the tracker switch both constructs a tracker.NewMultiTracker AND binds it at " +
			"the tracker.entry seam. With two trackers configured the entry ticket is therefore unbound: " +
			"the investigation cannot read it, no close-out is written, and the dedup stage suppresses a " +
			"re-fire whose parent incident has already resolved.")
	}
}

// switchMentions reports whether any case expression of a switch names the given identifier.
func switchMentions(sw *ast.SwitchStmt, ident string) bool {
	for _, c := range sw.Body.List {
		cc, ok := c.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, e := range cc.List {
			if strings.Contains(exprText(e), ident) {
				return true
			}
		}
	}
	return false
}

// switchSeamCoverage walks the switch selected by `ident` and reports how many of its branches record
// at a wiring seam, using the given set of accepted declaration call names.
func switchSeamCoverage(t *testing.T, ident string, declarers map[string]bool) (branches, declaring int, hasDefault bool, records int) {
	t.Helper()
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Body == nil || !switchMentions(sw, ident) {
			return true
		}
		found = true
		for _, c := range sw.Body.List {
			cc, ok := c.(*ast.CaseClause)
			if !ok {
				continue
			}
			branches++
			if cc.List == nil {
				hasDefault = true
			}
			hit := false
			for _, st := range cc.Body {
				ast.Inspect(st, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if declarers[calleeName(call.Fun)] {
						records++
						hit = true
					}
					return true
				})
			}
			if hit {
				declaring++
			}
		}
		return false
	})
	if !found {
		t.Fatalf("could not find the switch on %s in cmd/worker/main.go — if it was refactored, this "+
			"guard must be updated deliberately, not deleted", ident)
	}
	return branches, declaring, hasDefault, records
}

// TestSeamYieldRegisterIsObservedNotJustConstructed pins the register against the exact failure it exists
// to detect, reproduced one level up.
//
// A YieldRegister that is created, reported, and never OBSERVED reports every seam as "unobserved" — which
// is honest, and is also a report nobody acts on if it never shrinks. The register earns its keep only
// when composition-root code actually calls Observe, and a refactor that dropped those calls would leave
// every test in core/wiring green while the register measured nothing.
//
// This is the third guard of this shape in this file (notifier fan-out, tracker routing, discovery
// probes). The pattern is now explicit: a unit oracle proves a property the composition root is free to
// ignore, so the composition root gets its own assertion.
//
// KILLING MUTATION: delete the wiringYield.Observe calls from cmd/worker/main.go. RED.
func TestSeamYieldRegisterIsObservedNotJustConstructed(t *testing.T) {
	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	constructed, observes, totalCalls := false, 0, 0
	seamsObserved := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		totalCalls++
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case x.Name == "wiring" && sel.Sel.Name == "NewYieldRegister":
			constructed = true
		case x.Name == "wiringYield" && sel.Sel.Name == "Observe":
			observes++
			// Record WHICH seam each call names, so "instrumented" cannot be satisfied by one seam
			// observed several times.
			if len(call.Args) > 0 {
				if s, ok := call.Args[0].(*ast.SelectorExpr); ok {
					seamsObserved[s.Sel.Name] = true
				}
			}
		}
		return true
	})

	if totalCalls < 10 {
		t.Fatalf("vacuity floor: the AST walk found only %d call sites — the matcher is broken", totalCalls)
	}
	if !constructed {
		t.Fatal("cmd/worker/main.go never constructs a wiring.NewYieldRegister — there is no runtime " +
			"coverage of seam yield at all, so a bound-and-starved lane is invisible")
	}
	if observes == 0 {
		t.Fatal("the yield register is CONSTRUCTED and never OBSERVED. Every seam would report " +
			"'unobserved' forever: the register would be a permanent complaint rather than a detector, " +
			"which is the same defect class it was built to find")
	}
	// More than one seam, or the register is a single-lane counter wearing a register's name.
	if len(seamsObserved) < 2 {
		t.Fatalf("only %d distinct seam(s) are observed (%v) — a register covering one seam cannot report "+
			"on the closed set", len(seamsObserved), seamsObserved)
	}
	// And the report must actually be taken, or the counts accumulate into a void.
	src, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if !strings.Contains(string(src), "wiringYield.Report(") {
		t.Error("the register is observed but never REPORTED — the counts accumulate and nothing reads them")
	}
}

// TestEveryDeclaredSeamIsYieldInstrumented keeps the register's coverage COMPLETE by construction.
//
// The register reports an un-instrumented seam as UNOBSERVED rather than healthy, which is honest — and
// honesty alone decays. Left to prose, the ninth seam joins the closed set, nobody wires Observe for it,
// and the yield report grows a permanent complaint that everyone learns to scroll past. That is how the
// predecessor ended up tracking 387 components with 46 declared-dark and no mechanism that ever made
// adding the 47th visible.
//
// So: adding a seam to the closed set now REQUIRES instrumenting it, in the same merge request, exactly
// as adding one already requires a Consequence and a Unit.
//
// KILLING MUTATION: add a seam to core/wiring.All() without a matching wiringYield.Observe call. RED.
func TestEveryDeclaredSeamIsYieldInstrumented(t *testing.T) {
	// The declared set, read from the source of truth rather than re-listed here (a hand-maintained copy
	// is the drift this whole file exists to prevent).
	declared := map[string]bool{}
	for _, sp := range wiring.All() {
		declared[string(sp.ID)] = true
	}
	if len(declared) == 0 {
		t.Fatal("vacuity floor: the closed seam set is empty, so this guard certifies nothing")
	}

	path := filepath.Join("..", "cmd", "worker", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Map the Go CONSTANT name each Observe call names (SeamGovNotify) back to the seam VALUE
	// ("gov.notify"), so the comparison is against the declared set rather than against a spelling.
	constToValue := seamConstNames(t)

	instrumented := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Name != "wiringYield" {
			return true
		}
		if sel.Sel.Name != "Observe" && sel.Sel.Name != "ObserveTotals" {
			return true
		}
		if arg, ok := call.Args[0].(*ast.SelectorExpr); ok {
			if v, known := constToValue[arg.Sel.Name]; known {
				instrumented[v] = true
			}
		}
		return true
	})

	if len(instrumented) == 0 {
		t.Fatal("vacuity floor: no wiringYield.Observe call named a known seam — either the matcher is " +
			"broken or nothing is instrumented, and both make a pass here meaningless")
	}
	for seam := range declared {
		if !instrumented[seam] {
			t.Errorf("seam %q is declared in the closed set but nothing calls wiringYield.Observe for it. "+
				"It will report UNOBSERVED forever: a permanent complaint an operator learns to scroll "+
				"past, which is worse than no register at all. Instrument it in the same merge request "+
				"that declares it.", seam)
		}
	}
}

// seamConstNames reads the constant-name -> seam-value mapping out of core/wiring/seam.go itself.
//
// A first draft DERIVED the constant name from the value ("gov.notify" -> "SeamGovNotify") and this guard
// promptly failed on suppression.tier1, whose constant is SeamSuppression rather than the derivable
// SeamSuppressionTier1. The names are simply not a function of the values, and a heuristic that is right
// seven times out of eight produces a guard that cries wolf on the eighth — which is how a guard gets
// disabled. Reading the declarations is exact.
func seamConstNames(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("..", "core", "wiring", "seam.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if !strings.HasPrefix(vs.Names[0].Name, "Seam") {
			return true
		}
		out[vs.Names[0].Name] = strings.Trim(lit.Value, `"`)
		return true
	})
	if len(out) == 0 {
		t.Fatal("vacuity floor: no Seam* constants parsed out of core/wiring/seam.go")
	}
	return out
}
