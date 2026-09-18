package main

// ORACLES FOR CATALOG-WIDE REGISTRY DECLARATION (TG-267). Each names its killing mutation.
//
// The defect: TG-251's projection channel worked and still showed 25 of 29 modules as "state not
// reported here", because the registry it projects held 18 pairs — most connectors are constructed
// without registering. These prove both halves and, in the floor, that the gap cannot silently return.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/territory-grounder/grounder/modules"
	"github.com/territory-grounder/grounder/modules/catalog"
)

func capsOf(reg *modules.Registry) map[string]bool {
	out := map[string]bool{}
	for _, c := range reg.Capabilities() {
		out[c.Surface+"/"+c.SourceType] = c.Enabled
	}
	return out
}

// KILLING MUTATION: drop the declare hook from probeRegistry.offer. RED — a constructed module would
// stop reaching the registry, which is the TG-267 defect exactly.
func TestAnOfferedConstructionRegistersAsEnabled(t *testing.T) {
	reg := modules.NewRegistry()
	probes := newProbeRegistry()
	probes.declare = declareConstructed(reg)

	probes.offer("credsource", "openbao", struct{}{})
	caps := capsOf(reg)
	enabled, present := caps["credsource/openbao"]
	if !present {
		t.Fatal("an offered construction never reached the module registry — the projection cannot report it")
	}
	if !enabled {
		t.Fatal("a CONSTRUCTED module registered as disabled — constructed means running")
	}
}

// KILLING MUTATION: make declareOffered hook future offers only (drop the replay of probes.seen). RED —
// syslogng and the librenms tool set are offered at the top of main, long before moduleReg exists, so a
// hook-only design silently loses them to construction order.
func TestConstructionsOfferedBeforeTheHookAreStillDeclared(t *testing.T) {
	reg := modules.NewRegistry()
	probes := newProbeRegistry()

	// offered with NO hook wired — the pre-moduleReg window
	probes.offer("observability", "syslog-ng", struct{}{})
	probes.offer("ingest", "librenms", struct{}{})
	if len(reg.Capabilities()) != 0 {
		t.Fatal("fixture wrong: something registered before the replay")
	}

	if n := declareOffered(reg, probes); n != 2 {
		t.Fatalf("replay declared %d, want 2 — early constructions are being lost to ordering", n)
	}
	caps := capsOf(reg)
	for _, k := range []string{"observability/syslog-ng", "ingest/librenms"} {
		if !caps[k] {
			t.Fatalf("%s missing or disabled after replay", k)
		}
	}
}

// KILLING MUTATION: register absent catalog modules as ENABLED (or skip them). RED — "the worker looked
// and it is off" and "nobody can see it" must stay distinct; that distinction is the whole point of the
// three-state render TG-251 shipped.
func TestCatalogModulesNeverConstructedAreDeclaredOff(t *testing.T) {
	reg := modules.NewRegistry()
	probes := newProbeRegistry()
	probes.declare = declareConstructed(reg)
	probes.offer("notifier", "matrix", struct{}{}) // one constructed module

	added := declareCatalogAbsent(reg)
	if added == 0 {
		t.Fatal("no catalog module was declared — the second half is inert")
	}
	caps := capsOf(reg)
	if !caps["notifier/matrix"] {
		t.Fatal("the constructed module was flipped to disabled by the absent-declaration pass")
	}

	all, err := catalog.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range all {
		key := d.Surface + "/" + d.SourceType
		enabled, present := caps[key]
		if !present {
			t.Fatalf("catalog module %s is still absent from the registry — it would render as unknown", key)
		}
		if key != "notifier/matrix" && enabled {
			t.Fatalf("%s was never constructed yet registered ENABLED — a declared-off module must not "+
				"claim to be running", key)
		}
	}
}

// THE VACUITY FLOOR. KILLING MUTATION: remove either declaration half from main. RED — this is the
// assertion that makes "18 of 29" impossible to reintroduce silently.
func TestEveryCatalogModuleIsRepresentedInTheRegistry(t *testing.T) {
	reg := modules.NewRegistry()
	probes := newProbeRegistry()
	probes.declare = declareConstructed(reg)
	declareOffered(reg, probes)
	declareCatalogAbsent(reg)

	all, err := catalog.All()
	if err != nil {
		t.Fatal(err)
	}
	caps := capsOf(reg)
	var missing []string
	for _, d := range all {
		if _, present := caps[d.Surface+"/"+d.SourceType]; !present {
			missing = append(missing, d.Surface+"/"+d.SourceType)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d catalog modules are unrepresented in the registry, so the capability projection "+
			"reports them as unknown (TG-267): %v", len(missing), len(all), missing)
	}
	t.Logf("all %d catalog modules represented", len(all))
}

// KILLING MUTATION: delete either declaration call from main, or move the declare-hook assignment BEFORE
// bootstrap.Reconcile (which would refuse boot on any deployment pinning TG_EXPECTED_CAPABILITIES).
func TestBothDeclarationHalvesAreWiredInMainAfterReconcile(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var offeredLine, absentLine, reconcileLine int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		line := fset.Position(call.Pos()).Line
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			switch fn.Name {
			case "declareOffered":
				offeredLine = line
			case "declareCatalogAbsent":
				absentLine = line
			}
		case *ast.SelectorExpr:
			// QUALIFIED match: main.go holds several .Reconcile selectors (the estate reconciler among
			// them) and an unqualified match picked the LAST one, 1,800 lines away — the test failed for
			// the wrong reason on its first run, which is the cheap way to learn a control is imprecise.
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "bootstrap" && fn.Sel.Name == "Reconcile" {
				reconcileLine = line
			}
		}
		return true
	})
	if offeredLine == 0 {
		t.Error("main never calls declareOffered — constructed modules stay invisible to the projection")
	}
	if absentLine == 0 {
		t.Error("main never calls declareCatalogAbsent — described-but-unbuilt modules render as unknown " +
			"instead of off")
	}
	if reconcileLine != 0 && offeredLine != 0 && offeredLine < reconcileLine {
		t.Errorf("declareOffered (line %d) runs BEFORE bootstrap.Reconcile (line %d) — the expected-capability "+
			"pin governs the bootstrap families, so declaring the full catalog first would refuse boot on a "+
			"pinned deployment", offeredLine, reconcileLine)
	}
}
