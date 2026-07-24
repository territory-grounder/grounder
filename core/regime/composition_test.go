package regime

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestCompositionInvariant_NoExportedEffectPath is the STANDING STRUCTURAL check for REQ-1702: no Lane may
// expose an exported effect path that skips the spec/013 interceptor. It parses the package's own source
// (go/ast) and asserts four structural properties, and reflects the Lane interface for a fifth. The whole
// engine is a violation if any lane can mutate around the chain, so this test fails LOUD the moment a future
// lane grows an exported actuator accessor or a direct Exec call.
func TestCompositionInvariant_NoExportedEffectPath(t *testing.T) {
	fset := token.NewFileSet()
	files := parseNonTestGoFiles(t, fset)

	// (a) The Lane interface exposes the effect leaf ONLY through an UNEXPORTED accessor, and NO exported
	//     method that returns an actuation.Actuator.
	iface := findInterface(files, "Lane")
	if iface == nil {
		t.Fatal("REQ-1702: the Lane interface must be declared in package regime")
	}
	var unexportedLeafAccessor, exportedActuatorAccessor bool
	for _, m := range iface.Methods.List {
		if len(m.Names) == 0 {
			continue // embedded interface (there are none, but be defensive)
		}
		name := m.Names[0].Name
		returnsActuator := funcReturnsActuator(fset, m.Type)
		if returnsActuator {
			if ast.IsExported(name) {
				exportedActuatorAccessor = true
			} else {
				unexportedLeafAccessor = true
			}
		}
	}
	if !unexportedLeafAccessor {
		t.Fatal("REQ-1702: Lane must expose its effect leaf through an UNEXPORTED accessor (effectLeaf)")
	}
	if exportedActuatorAccessor {
		t.Fatal("REQ-1702 VIOLATION: the Lane interface exposes an EXPORTED method returning an actuation.Actuator — an exported effect path that skips the interceptor")
	}

	// (b) NO exported function or method anywhere in the package returns an actuation.Actuator — there is no
	//     exported escape hatch to grab a lane's raw actuator and call Exec around the chain.
	// (c) The package NEVER calls `.Exec(` — regime never invokes an actuator's effect itself; the interceptor
	//     owns the effect. The only route is Interceptor.Do (asserted in (d)).
	// (d) effect.go (the seam) DOES call `.Do(` — positive proof the effect path routes through the chain.
	exportedActuatorReturn := ""
	directExecCall := ""
	seamCallsDo := false
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Name.IsExported() && funcReturnsActuator(fset, node.Type) {
					exportedActuatorReturn = path + ":" + node.Name.Name
				}
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "Exec":
						directExecCall = path
					case "Do":
						if strings.HasSuffix(path, "effect.go") {
							seamCallsDo = true
						}
					}
				}
			}
			return true
		})
	}
	if exportedActuatorReturn != "" {
		t.Fatalf("REQ-1702 VIOLATION: exported symbol %s returns an actuation.Actuator — an exported effect path that skips the interceptor", exportedActuatorReturn)
	}
	if directExecCall != "" {
		t.Fatalf("REQ-1702 VIOLATION: package regime calls Exec directly in %s — the effect must be reached ONLY through Interceptor.Do", directExecCall)
	}
	if !seamCallsDo {
		t.Fatal("REQ-1702: effect.go must route the selected leaf through the spec/013 Interceptor.Do (no Do call found)")
	}

	// (e) Reflection cross-check: the Lane interface's method SET exposes Regime() and no EXPORTED method that
	//     returns an actuation.Actuator (belt-and-suspenders over the AST check).
	laneT := reflect.TypeOf((*Lane)(nil)).Elem()
	actuatorT := reflect.TypeOf((*actuation.Actuator)(nil)).Elem()
	if _, ok := laneT.MethodByName("Regime"); !ok {
		t.Fatal("REQ-1702: Lane must expose Regime()")
	}
	for i := 0; i < laneT.NumMethod(); i++ {
		m := laneT.Method(i)
		if m.PkgPath != "" {
			continue // unexported (e.g. effectLeaf) — not reachable outside the package
		}
		for o := 0; o < m.Type.NumOut(); o++ {
			if m.Type.Out(o) == actuatorT {
				t.Fatalf("REQ-1702 VIOLATION: exported Lane method %q returns an actuation.Actuator", m.Name)
			}
		}
	}
}

// TestCompositionInvariant_EffectRoutesThroughInterceptor is the behavioral half of REQ-1702: a selected
// lane's effect is reachable ONLY by traversing the spec/013 interceptor chain. Under a read-only (Shadow)
// chokepoint the mode gate refuses the mutation BEFORE Exec (the leaf never fires — proving the effect goes
// through the gates); under a test-only actuating chokepoint the SAME leaf reaches Exec exactly once, and only
// via Interceptor.Do. It also proves the seam fails loud when unwired.
func TestCompositionInvariant_EffectRoutesThroughInterceptor(t *testing.T) {
	leaf := &fakeActuator{cap: "ssh", ro: false}
	lane := NewNativeSSHLane(leaf)

	// Read-only (Shadow) chokepoint: the interceptor's mode chokepoint refuses before execute. This is the
	// production posture — mutation stays OFF — and proves the lane's effect only exists BEHIND the chain.
	seamOff := NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(safety.NewReadOnlyChokepoint(), l, audit.NewLedger())
	})
	out, err := seamOff.Apply(context.Background(), lane, goodRequest(t))
	if err != nil {
		t.Fatalf("Apply must not error on a wired seam under Shadow: %v", err)
	}
	if !out.Refused || out.Executed || leaf.execs != 0 {
		t.Fatalf("under Shadow the mode chokepoint must refuse BEFORE Exec (the effect goes through the chain): %+v execs=%d", out, leaf.execs)
	}

	// Test-only actuating chokepoint (mutation-ON, proving the chain executes exactly like the existing
	// interceptor tests): the SAME leaf now reaches Exec — and ONLY through Interceptor.Do.
	seamOn := NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(safety.NewActuatingChokepoint(), l, audit.NewLedger())
	})
	out2, err := seamOn.Apply(context.Background(), lane, goodRequest(t))
	if err != nil {
		t.Fatalf("Apply must not error on a wired seam: %v", err)
	}
	if !out2.Executed || leaf.execs != 1 {
		t.Fatalf("the selected leaf must reach Exec exactly once via Interceptor.Do: %+v execs=%d", out2, leaf.execs)
	}

	// An unwired seam (nil builder) or a nil lane fails loud — never reaches an effect around the chain.
	if _, err := (&LaneEffect{}).Apply(context.Background(), lane, goodRequest(t)); err != ErrSeamUnwired {
		t.Fatalf("an unwired seam must fail loud with ErrSeamUnwired, got %v", err)
	}
	if _, err := seamOn.Apply(context.Background(), nil, goodRequest(t)); err != ErrSeamUnwired {
		t.Fatalf("a nil lane must fail loud with ErrSeamUnwired, got %v", err)
	}
}

// The awx-job lane's placeholder leaf refuses (fail closed): even if the mode chokepoint were open, the
// not-yet-wired actuator (T-017-3) can only refuse — mutation stays OFF and the effect is never a silent
// no-op. Proven by driving the SAME seam with a test-only actuating chokepoint.
func TestAWXJobLanePlaceholderRefuses(t *testing.T) {
	seam := NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(safety.NewActuatingChokepoint(), l, audit.NewLedger())
	})
	out, err := seam.Apply(context.Background(), NewAWXJobLane(), goodRequest(t))
	if err != nil {
		t.Fatalf("Apply must not error: %v", err)
	}
	if out.Executed || !out.Refused {
		t.Fatalf("the awx-job placeholder leaf must refuse (not-yet-wired), got %+v", out)
	}
}

// ---------------------------------------------------------------------------------------------------------
// AST helpers.
// ---------------------------------------------------------------------------------------------------------

// parseNonTestGoFiles parses every non-test .go file in the package directory (the cwd at test time).
func parseNonTestGoFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatal("no non-test .go files found in the package directory")
	}
	return out
}

// findInterface returns the named interface type declared anywhere in the parsed files, or nil.
func findInterface(files map[string]*ast.File, name string) *ast.InterfaceType {
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					return it
				}
			}
		}
	}
	return nil
}

// funcReturnsActuator reports whether a *ast.FuncType has a result whose rendered type mentions
// actuation.Actuator (the effect leaf type). It is deliberately textual so it catches any spelling of the
// return — a value, a pointer, or an alias — of the actuator interface.
func funcReturnsActuator(fset *token.FileSet, typ ast.Expr) bool {
	ft, ok := typ.(*ast.FuncType)
	if !ok || ft.Results == nil {
		return false
	}
	for _, r := range ft.Results.List {
		if s := renderExpr(fset, r.Type); strings.Contains(s, "actuation.Actuator") || s == "Actuator" {
			return true
		}
	}
	return false
}

// renderExpr renders an AST type expression to source text.
func renderExpr(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}

// ---------------------------------------------------------------------------------------------------------
// Request fixture (mirrors core/actuate's admissible mutating request so the seam drives the REAL chain).
// ---------------------------------------------------------------------------------------------------------

func noObserved(context.Context) []verify.ObservedAlert { return []verify.ObservedAlert{} }

func goodRequest(t *testing.T) actuate.Request {
	t.Helper()
	m, err := manifest.New(
		manifest.Action{Target: "web01", OpClass: "restart-service", Op: "restart", Reversible: true},
		safety.BandAuto, "plan#regime", "pred#regime",
	)
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	return actuate.Request{
		Manifest: m,
		Gated:    true,
		Argv:     []string{"systemctl", "restart", "nginx"},
		Evidence: []actuate.Evidence{{ToolResultID: "tr-1", Captured: true, Successful: true, Recent: true, Relevant: true}},
		Observe:  noObserved,
		Band:     safety.BandAuto, // fresh per-incident band (TG-126): AUTO admits at 1b, the lane/mode/policy gate the rest
	}
}
