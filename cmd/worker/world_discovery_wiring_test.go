package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/wikicompile"
)

// THE ORACLE WHOSE ABSENCE LET A WHOLE LANE SHIP DEAD.
//
// temporal/worlddiscovery was built, documented, unit-tested and GREEN IN CI while being referenced by
// nothing outside its own package. Production ran with manifest_entry at 0 rows and the console said
// "Discovery has not drafted anything yet" — true, and indistinguishable from "it ran and found nothing".
// Its own cron_test.go passed the entire time, because those tests call Run directly and nothing else did.
//
// A unit test proves a function WORKS. It cannot prove anything CALLS it. This file asserts the second
// thing, at the only place that can: the composition root.
//
// KILLING MUTATION: delete the worlddiscovery block from main.go. RED here, naming what is missing.
func TestWorldDiscoveryIsReachableFromTheCompositionRoot(t *testing.T) {
	const root = "main.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, root, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", root, err)
	}

	imported := false
	for _, im := range file.Imports {
		if strings.Contains(im.Path.Value, "temporal/worlddiscovery") {
			imported = true
		}
	}
	if !imported {
		t.Fatal("main.go does not import temporal/worlddiscovery — the discovery pass is unreachable, " +
			"manifest_entry can never be written, and #manifest is permanently empty. This is the exact " +
			"state production shipped in.")
	}

	var buildsJob, runsPeriodically, bindsSeam bool
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "worlddiscovery" && sel.Sel.Name == "Job" {
					buildsJob = true
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					if id.Name == "worlddiscovery" && sel.Sel.Name == "RunPeriodically" {
						runsPeriodically = true
					}
					if id.Name == "wiring" && sel.Sel.Name == "Bind" {
						for _, a := range x.Args {
							if s, ok := a.(*ast.SelectorExpr); ok && s.Sel.Name == "SeamWorldDiscovery" {
								bindsSeam = true
							}
						}
					}
				}
			}
		}
		return true
	})

	if !buildsJob {
		t.Error("main.go never constructs a worlddiscovery.Job — importing the package is not calling it")
	}
	if !runsPeriodically {
		t.Error("main.go never calls worlddiscovery.RunPeriodically — a Job nothing runs drafts nothing")
	}
	if !bindsSeam {
		t.Error("main.go never Binds wiring.SeamWorldDiscovery — the lane would be live while the wiring " +
			"manifest reports it dark-unrecorded, which is the report lying in the safe direction")
	}
}

// TestWorldDiscoveryRefusesToRunWithoutSources — the guard that matters most, because the failure it
// prevents is SILENT AND DESTRUCTIVE-LOOKING.
//
// The pass computes disappearance by diffing what the sources observed against what the manifest holds.
// With NO sources it observes nothing, so a naive pass would conclude that every approved entry has
// disappeared and mark the entire world model stale — one missing config turning into estate-wide drift
// noise. worlddiscovery.Run refuses with ErrNoSources rather than doing that; this asserts the composition
// root ALSO refuses, up front and with a reason, instead of arming a job that logs that refusal forever.
//
// KILLING MUTATION: remove the `len(estateSources) == 0` arm from the switch. RED.
func TestWorldDiscoveryDeclaresDarkRatherThanArmingWithoutSources(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Inspect the CASE CONDITIONS, not the switch's text. The first version of this test scanned every
	// identifier inside the switch and asked whether "estateSources" and "darkDiscovery" both appeared —
	// which a mutation replacing the guard with `case false:` satisfied trivially, because both names were
	// still present elsewhere in the block. It passed with the guard gone. A test that a rewrite of the
	// very condition it guards cannot break is not testing that condition.
	var guarded bool
	ast.Inspect(src, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok || len(cc.List) == 0 {
			return true
		}
		condMentionsSources := false
		for _, cond := range cc.List {
			ast.Inspect(cond, func(x ast.Node) bool {
				if id, ok := x.(*ast.Ident); ok && id.Name == "estateSources" {
					condMentionsSources = true
				}
				return true
			})
		}
		if !condMentionsSources {
			return true
		}
		// The body of THAT case must declare the seam dark.
		ast.Inspect(cc, func(x ast.Node) bool {
			if call, ok := x.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "darkDiscovery" {
					guarded = true
				}
			}
			return true
		})
		return true
	})
	if !guarded {
		t.Error("no switch case CONDITION tests estateSources and declares the seam dark in its body. " +
			"Without that guard the lane arms with zero sources: the pass observes nothing, diffs that " +
			"empty observation against the manifest, and marks EVERY approved entry stale — one missing " +
			"config becoming estate-wide drift noise.")
	}
}

// TestWiringSamplesReachTheExporter — the dark-seam gauge must not be computed and dropped.
//
// core/wiring/report.go:76 emits a tg_wiring_seam_dark sample per seam on every boot. Until 2026-08-01
// cmd/worker/main.go discarded it:
//
//	_ = wiringSamples // exported alongside the other boot samples below when an exporter is configured
//
// There was no "below". The export loop sits ~2,600 lines ABOVE that line and shipped only capability and
// suppression samples. So the control built to make a silently-dark seam visible was itself silently dark
// in its ALERTING limb — while temporal/worlddiscovery ran unwired in production and #manifest stayed
// empty. The log and ledger limbs did work, which is why this went unnoticed: findings were readable, just
// never alertable.
//
// This asserts the hand-off structurally, at the composition root, because there is no unit seam to hold
// it: the producer and the consumer are 2,600 lines apart in one function.
//
// KILLING MUTATIONS (both executed 2026-08-01, restored green):
//   - restore `_ = wiringSamples`            -> "never stored ... the gauge is computed and dropped"
//   - drop the Load() from the export loop   -> "never read by the export loop"
func TestWiringSamplesReachTheExporter(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var stored, loaded, appended bool
	ast.Inspect(src, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != "wiringSampleSet" {
			return true
		}
		switch sel.Sel.Name {
		case "Store":
			stored = true
		case "Load":
			loaded = true
		}
		return true
	})
	// ...and the loaded value must actually be APPENDED to the exported slice, not merely read. A Load
	// whose result is discarded would satisfy a naive check while shipping nothing.
	//
	// This follows the DATAFLOW rather than the name: the value arrives through a local
	// (`ws := wiringSampleSet.Load()`) and is appended as `*ws...`, so searching the append arguments for
	// the identifier "wiringSampleSet" finds nothing even when the wiring is correct. The first version of
	// this check did exactly that and reported a false failure against working code — the same
	// name-matching mistake that made an earlier guard in this file miss a third lane's helper.
	loadedInto := map[string]bool{}
	ast.Inspect(src, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Load" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "wiringSampleSet" {
			return true
		}
		if lhs, ok := as.Lhs[0].(*ast.Ident); ok {
			loadedInto[lhs.Name] = true
		}
		return true
	})
	ast.Inspect(src, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "append" {
			return true
		}
		for _, arg := range call.Args {
			ast.Inspect(arg, func(x ast.Node) bool {
				if id, ok := x.(*ast.Ident); ok && (loadedInto[id.Name] || id.Name == "wiringSampleSet") {
					appended = true
				}
				return true
			})
		}
		return true
	})
	if len(loadedInto) == 0 && loaded {
		t.Error("VACUITY: wiringSampleSet.Load() is called but its result is bound to nothing this check " +
			"can follow — the dataflow assertion below proved nothing")
	}

	if !stored {
		t.Error("the wiring samples are never stored for the export loop — the gauge is computed on every " +
			"boot (core/wiring/report.go) and dropped. That is the alerting limb of the control that " +
			"exists to catch a silently-dark seam.")
	}
	if !loaded {
		t.Error("the wiring samples are stored but never read by the export loop")
	}
	if !appended {
		t.Error("the wiring samples are read but never appended to the exported sample slice — a Load whose " +
			"result is discarded ships nothing while looking wired")
	}
}

// TestPeriodicLanesHaveASafeGoDefault — a lane whose Go default is zero runs ONLY where a compose file
// says so, and silently does nothing everywhere else.
//
// That is not hypothetical: world discovery shipped with `envDuration(..., 0)` while its sibling
// wiki-compile lane shipped with 30m, so the lane wired in !826 was armed by deploy/docker-compose.yml
// alone. Any deployment not using that file — a bare binary, a different orchestration — reproduced the
// empty-manifest state !826 existed to fix, and reported itself dark while doing it.
//
// The default belongs in the binary. The compose value is then explicit agreement, not the only thing
// holding the lane up.
//
// KILLING MUTATION: set either lane's default back to 0. RED, naming the lane.
func TestPeriodicLanesHaveASafeGoDefault(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(src)
	for _, lane := range []struct{ env, why string }{
		{"TG_WORLD_DISCOVERY_INTERVAL",
			"nothing is ever drafted, manifest_entry stays empty and #manifest is permanently blank"},
		{"TG_WIKI_COMPILE_INTERVAL",
			"no per-host article is compiled and the console's host surface stays a client-side window"},
	} {
		zero := `envDuration("` + lane.env + `", 0)`
		if strings.Contains(text, zero) {
			t.Errorf("%s defaults to 0 in Go, so the lane runs ONLY where a compose file sets it — "+
				"everywhere else %s. Put the safe value in the binary; let compose agree with it.",
				lane.env, lane.why)
		}
		if !strings.Contains(text, `envDuration("`+lane.env+`"`) {
			t.Errorf("VACUITY: %s is not read via envDuration in main.go — this guard checked nothing "+
				"for that lane", lane.env)
		}
	}
}

// TestWikiCompileEmitsTheCoveragePage — the wiring half. CompileCoverage is a pure function with its own
// oracles; this asserts the LANE actually calls it, because a coverage page nothing emits is exactly the
// class of defect this whole file exists to catch.
//
// KILLING MUTATION: delete the CompileCoverage append from compileWikiArticles. RED.
func TestWikiCompileEmitsTheCoveragePage(t *testing.T) {
	src, err := os.ReadFile("wiki_compile.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "wikicompile.CompileCoverage(") {
		t.Error("the wiki lane never calls CompileCoverage — the coverage page would exist as a tested " +
			"pure function that nothing ever produces, which is the worlddiscovery failure in miniature")
	}
	// It must be appended to the SAME slice that goes into the envelope, not computed and dropped.
	if !strings.Contains(text, "articles = append(articles, wikicompile.CompileCoverage(") {
		t.Error("CompileCoverage is called but its result is not appended to the article set — a page " +
			"computed and discarded ships nothing while looking wired")
	}
}

// TestWikiCompileEmitsRulePages — the wiring half for the per-rule family, and the CANONICALISATION.
//
// Two things can go wrong independently: the lane can fail to call CompileRules at all (a tested pure
// function nothing produces), or it can call it with RAW rule strings — which would split production's
// device-down family four ways and hide the exact recurrence the page exists to show. Both are asserted
// at the composition root because neither is visible from inside the pure package.
//
// KILLING MUTATIONS: delete the CompileRules block; or pass r.AlertRule as Rule instead of the canonical
// form. Both RED.
func TestWikiCompileEmitsRulePages(t *testing.T) {
	src, err := os.ReadFile("wiki_compile.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "wikicompile.CompileRules(") {
		t.Fatal("the wiki lane never calls CompileRules — the per-rule family would be a tested pure " +
			"function nothing ever produces")
	}
	if !strings.Contains(text, "articles = append(articles, ruleArts...)") {
		t.Error("the rule pages are compiled but not appended to the article set")
	}
	if !strings.Contains(text, "knowledge.CanonicalRule(r.AlertRule)") {
		t.Error("rule sessions must be folded into FAMILIES before compiling. Production carries one fault " +
			"class under four source names (Device-Down / Devices-up~down / -SNMP-unreachable / " +
			"-no-ICMP-response); grouping on the raw string splits that page four ways and hides the " +
			"recurrence — the same non-folding that made a class of incident impossible to confirm as " +
			"recovered (core/knowledge/rulefamily.go)")
	}
	// The skips from the rule compile must reach the envelope too, or a refused rule vanishes silently.
	if !strings.Contains(text, "skips = append(skips, ruleSkips...)") {
		t.Error("rule refusals must reach the envelope — a refused rule that is merely absent is " +
			"indistinguishable from a rule that never fired")
	}
}

// TestWikiCompileEmitsOpClassPagesKeyedOnUse — the wiring half, and the inversion that makes it useful.
//
// Production holds ZERO ratified op-classes against SEVEN actually used and 460 executions. A page set
// gated on the ratified catalogue renders nothing and reads as "nothing to see" rather than "the earning
// ladder is not built yet" — the same failure that left #manifest and #proposals blank.
//
// Two independent things are asserted because they fail independently: the lane must CALL the compiler,
// and it must report an unreadable catalogue as UNKNOWN rather than as "not ratified".
//
// KILLING MUTATIONS: delete the CompileOpClasses block; or set rok=true on a failed Ratified read. Both RED.
func TestWikiCompileEmitsOpClassPagesKeyedOnUse(t *testing.T) {
	src, err := os.ReadFile("wiki_compile.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "wikicompile.CompileOpClasses(") {
		t.Fatal("the wiki lane never calls CompileOpClasses — the op-class family would be a tested pure " +
			"function nothing ever produces")
	}
	if !strings.Contains(text, "articles = append(articles, opArts...)") {
		t.Error("op-class pages are compiled but not appended to the article set")
	}
	if !strings.Contains(text, "skips = append(skips, opSkips...)") {
		t.Error("op-class refusals must reach the envelope")
	}
	// The pages must be fed the SESSIONS, not the ratified set — that is the keyed-on-use property.
	if !strings.Contains(text, "Sessions: rsess, Ratified: ratified") {
		t.Error("op-class pages must be compiled from the SESSIONS with ratification as an attribute. " +
			"Keying on the ratified catalogue renders nothing against production's empty one.")
	}
	// The unknown-vs-unratified property is asserted BEHAVIOURALLY below, not by grepping for
	// "rok = false" — that string also appears in the nil-dep branch, so a mutation gutting the ERROR
	// branch passed a source check cleanly. Third time today a name-presence assertion has been too weak;
	// running the lane is the fix.
}

// TestUnreadableRatifiedCatalogueRendersUnknownNotUnratified — behavioural, through the real lane.
//
// A failed read of the earned catalogue must produce pages whose STANDING says "could not be read", not
// pages that confidently assert NOT RATIFIED. The second is a claim about the catalogue derived from not
// having seen it — the defect class this whole day has been about.
//
// RED MUTATION CONTROL (executed 2026-08-01): swallowing the error in the Ratified branch (leaving rok
// true) makes every page assert "NOT in the earned catalogue" over a read that failed; restored green.
func TestUnreadableRatifiedCatalogueRendersUnknownNotUnratified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wiki.articles.json")
	deps := wikiCompileDeps{
		Roster:       func(context.Context) ([]string, error) { return nil, nil },
		SourceCounts: func(context.Context) (int, int, error) { return 0, 0, nil },
		PriorFor:     func(context.Context, string, int) ([]db.PriorTriage, error) { return nil, nil },
		Edges:        func(context.Context) ([]wikicompile.HostEdge, error) { return nil, nil },
		Corpus:       func() []knowledge.Incident { return nil },
		Now:          func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
		RuleSessions: func(context.Context, int) ([]db.WikiRuleSession, error) {
			return []db.WikiRuleSession{{
				ExternalRef: "x", Host: "h1", AlertRule: "Device-Down", Outcome: "proposed",
				OpClass: "start-guest", Mutated: true, ConfirmedClear: true,
				CreatedAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
			}}, nil
		},
		// THE POINT: this read fails.
		Ratified:   func(context.Context) (map[string]bool, error) { return nil, errors.New("catalogue unreachable") },
		Candidates: func(context.Context) (map[string]string, error) { return nil, nil },
	}
	if _, _, err := compileWikiArticles(context.Background(), path, deps); err != nil {
		t.Fatalf("compile: %v", err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	env, err := wikicompile.ParseArticles(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	var page string
	for _, a := range env.Articles {
		if a.Slug == "opclass-start-guest" {
			page = a.Body
		}
	}
	if page == "" {
		t.Fatal("no op-class page was produced from a session carrying an op_class")
	}
	if !strings.Contains(page, "could not be read") {
		t.Errorf("a failed catalogue read must render UNKNOWN standing; page:\n%s", page)
	}
	if strings.Contains(page, "NOT in the earned catalogue") {
		t.Error("a failed catalogue read rendered as a confident 'NOT in the earned catalogue' — that is " +
			"an assertion about the catalogue derived from not having seen it")
	}
}

// TestDecisionsDigestReachesTheEnvelope — BEHAVIOURAL, through the real lane, because a source grep has
// now been too weak three times in one day (the darkNotify/darkLessons list, the wiring-samples append
// check, and the ratified-catalogue error branch).
//
// Asserts both directions: a healthy ledger read produces the digest with its real numbers, and a FAILED
// ledger read costs the digest ALONE — the host and coverage pages must survive, because dropping the
// whole compile over one unreadable source would retire every page in the wiki.
//
// KILLING MUTATIONS: delete the CompileDecisions block (digest missing); or return early on a ledger
// error (the whole envelope collapses). Both RED.
func TestDecisionsDigestReachesTheEnvelope(t *testing.T) {
	base := func() wikiCompileDeps {
		return wikiCompileDeps{
			Roster:       func(context.Context) ([]string, error) { return []string{"h1"}, nil },
			SourceCounts: func(context.Context) (int, int, error) { return 1, 1, nil },
			PriorFor:     func(context.Context, string, int) ([]db.PriorTriage, error) { return nil, nil },
			Edges:        func(context.Context) ([]wikicompile.HostEdge, error) { return nil, nil },
			Corpus:       func() []knowledge.Incident { return nil },
			Now:          func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
		}
	}
	compile := func(t *testing.T, d wikiCompileDeps) wikicompile.Envelope {
		t.Helper()
		path := filepath.Join(t.TempDir(), "w.json")
		if _, _, err := compileWikiArticles(context.Background(), path, d); err != nil {
			t.Fatalf("compile: %v", err)
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		env, err := wikicompile.ParseArticles(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return env
	}
	find := func(env wikicompile.Envelope, slug string) (wikicompile.Article, bool) {
		for _, a := range env.Articles {
			if a.Slug == slug {
				return a, true
			}
		}
		return wikicompile.Article{}, false
	}

	t.Run("healthy ledger produces the digest", func(t *testing.T) {
		d := base()
		d.Decisions = func(context.Context) ([]db.WikiDecisionTally, int, error) {
			return []db.WikiDecisionTally{
				{Decision: "classify:POLL_PAUSE", Total: 1315, Withheld: 1315},
				{Decision: "human:poll-obsolete:subject-recovered", Total: 956},
			}, 8570, nil
		}
		page, ok := find(compile(t, d), wikicompile.DecisionsSlug)
		if !ok {
			t.Fatal("the governance digest is not in the envelope — a tested pure function nothing produces")
		}
		if !strings.Contains(page.Body, "956 poll(s) went obsolete") {
			t.Errorf("the digest must carry the ledger's real figures; body:\n%s", page.Body)
		}
	})

	t.Run("failed ledger read costs the digest alone", func(t *testing.T) {
		d := base()
		d.Decisions = func(context.Context) ([]db.WikiDecisionTally, int, error) {
			return nil, 0, errors.New("ledger unreachable")
		}
		env := compile(t, d)
		if _, ok := find(env, wikicompile.DecisionsSlug); ok {
			t.Error("a failed ledger read must produce NO digest rather than an empty-looking one")
		}
		if _, ok := find(env, wikicompile.CoverageSlug); !ok {
			t.Error("the coverage page must survive an unreadable ledger — dropping the whole compile over " +
				"one bad source would retire every page in the wiki")
		}
		if _, ok := find(env, "host-h1"); !ok {
			t.Error("host pages must survive an unreadable ledger")
		}
	})
}

// TestLaneHealthPageAndItsBootOrdering — behavioural, and the ordering is the interesting half.
//
// The lane-health page renders which seams are live. The wiring manifest is only complete once every
// Bind/Absent site has run, and several run hundreds of lines below where the wiki lane is armed — so
// compiling at arm time would publish a page declaring those later seams DARK. That is precisely the
// false positive the report's own ordering guard exists to prevent, reproduced inside a page an operator
// reads. Hence the first compile is deferred past the report.
//
// KILLING MUTATIONS: call compileWiki() at arm time again (the deferred call disappears); or render only
// the dark seams (live lanes vanish). Both RED.
func TestLaneHealthPageAndItsBootOrdering(t *testing.T) {
	t.Run("the page renders live and dark lanes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "w.json")
		d := wikiCompileDeps{
			Roster:       func(context.Context) ([]string, error) { return nil, nil },
			SourceCounts: func(context.Context) (int, int, error) { return 0, 0, nil },
			PriorFor:     func(context.Context, string, int) ([]db.PriorTriage, error) { return nil, nil },
			Edges:        func(context.Context) ([]wikicompile.HostEdge, error) { return nil, nil },
			Corpus:       func() []knowledge.Incident { return nil },
			Now:          func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
			Seams: func() []wikicompile.SeamStatus {
				return []wikicompile.SeamStatus{
					{Name: "wiki.compile"},
					{Name: "escalation.page", Dark: true, Critical: true,
						Consequence: "the escalation is permanently lost"},
				}
			},
		}
		if _, _, err := compileWikiArticles(context.Background(), path, d); err != nil {
			t.Fatalf("compile: %v", err)
		}
		blob, _ := os.ReadFile(path)
		env, err := wikicompile.ParseArticles(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		var body string
		for _, a := range env.Articles {
			if a.Slug == wikicompile.LanesSlug {
				body = a.Body
			}
		}
		if body == "" {
			t.Fatal("no lane-health page in the envelope")
		}
		if !strings.Contains(body, "permanently lost") {
			t.Error("a dark lane must render the consequence prose declared with its seam — that text " +
				"currently reaches only a boot log and a ledger row")
		}
		if !strings.Contains(body, "`wiki.compile`") {
			t.Error("a LIVE lane must be named, not omitted")
		}
	})

	t.Run("the first compile runs AFTER the wiring report", func(t *testing.T) {
		src, err := os.ReadFile("main.go")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		text := string(src)
		// THE PROPERTY IS "NO COMPILE BEFORE THE REPORT", not "a deferred call exists". Checking only for
		// the call let a mutation through: removing the ASSIGNMENT (firstWikiCompile = compileWiki) leaves
		// the deferred call present but nil, so a name check passes while the boot page is compiled at arm
		// time anyway. Assert all three parts — assignment, no early invocation, call after the report.
		report := strings.Index(text, "wiringManifest.Report(")
		if report < 0 {
			t.Fatal("no wiring report in main.go")
		}
		if !strings.Contains(text, "firstWikiCompile = compileWiki") {
			t.Error("the first compile must be ASSIGNED for deferral, not invoked at arm time")
		}
		// A bare `compileWiki()` before the report is the defect itself.
		before := text[:report]
		if i := strings.Index(before, "\n\t\tcompileWiki()"); i >= 0 {
			t.Errorf("compileWiki() is invoked at offset %d, BEFORE the wiring report at %d — the "+
				"lane-health page would declare every seam bound later as DARK, reproducing inside an "+
				"operator-facing page the exact false positive "+
				"deploy.TestWiringReportIsTakenAfterEveryBind exists to prevent", i, report)
		}
		firstCall := strings.Index(text, "firstWikiCompile()")
		if firstCall < 0 || firstCall < report {
			t.Errorf("the deferred first compile (offset %d) must run AFTER the wiring report (offset %d)",
				firstCall, report)
		}
	})
}
