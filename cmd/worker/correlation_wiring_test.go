package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// THE ALIVENESS GUARD FOR THE CORRELATION STAGE (TG-169).
//
// The failure this exists to prevent has happened here twice already: a surface with a parser, a handler, a
// migration and a full unit suite, DEAD in the shipped binary because nobody assigned the seam at the
// composition root (/v1/proposals), and two discovery probes linked into no binary while the boot log
// announced their lane as armed. The correlation stage has exactly that shape — two nil-inert func fields
// on runner.Deps, both of which fall back QUIETLY to the pre-TG-169 severity rule when unset. Unwired, TG
// would keep routing 81% of incidents to the deep path on a guess, every test would still be green, and the
// only symptom would be an exec_class_decision table that stayed empty forever.
//
// KILLING MUTATION (executed): delete the `CorrelationWindow:` and `ExecClassRecord:` lines from the
// runner.Deps literal in main.go. RED with both field names reported — while `go build`, `go vet` and the
// entire temporal/runner suite stay green, which is precisely why this guard is at the composition root and
// not in the package under test.
func TestCorrelationStageIsWiredAtTheCompositionRoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// The two pgx stores must actually be CONSTRUCTED (imported-but-unused would compile-fail; imported and
	// never called would not).
	calls := map[string]bool{}
	// ...and the seams must be SET on the runner.Deps literal. A constructed store assigned to nothing is
	// the same dead wiring wearing a busier diff.
	depsFields := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			switch fn := x.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fn.X.(*ast.Ident); ok {
					calls[id.Name+"."+fn.Sel.Name] = true
				}
			case *ast.Ident:
				calls[fn.Name] = true
			}
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Deps" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "runner" {
				return true
			}
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						depsFields[key.Name] = true
					}
				}
			}
		}
		return true
	})

	// Vacuity floor: a walk that matched nothing would certify nothing, and both maps must be populated
	// before any absence below means anything.
	if len(calls) < 10 {
		t.Fatalf("vacuity floor: the AST walk found only %d call sites in main.go — the matcher is broken", len(calls))
	}
	if len(depsFields) < 10 {
		t.Fatalf("vacuity floor: the AST walk found only %d fields on the runner.Deps literal — it did not "+
			"find the literal, so every field assertion below would pass over an empty set", len(depsFields))
	}

	for _, want := range []string{"db.NewCorrelationStore", "db.NewExecClassStore"} {
		if !calls[want] {
			t.Errorf("%s is never called in cmd/worker/main.go — the correlation stage has no reader/recorder "+
				"in the shipped binary, so every incident silently falls back to the pre-TG-169 "+
				"`severity == critical` guess and exec_class_decision stays empty forever", want)
		}
	}
	for _, want := range []string{"CorrelationWindow", "ExecClassRecord"} {
		if !depsFields[want] {
			t.Errorf("runner.Deps.%s is not set at the composition root — the seam is nil-inert by design, so "+
				"the Runner degrades quietly to the pre-TG-169 routing with nothing failing", want)
		}
	}
}

// The seams main.go assembles must have the EXACT shapes runner.Deps declares. A signature drift between
// the pgx store and the seam is a compile error in main.go — but only once main.go is written against the
// store, which is what this pins independently of the AST walk above.
func TestCorrelationSeamShapesMatchTheStores(t *testing.T) {
	// A nil pool is fine: nothing here executes a query.
	store := db.NewCorrelationStore(nil)
	const span = 10 * time.Minute // the value is irrelevant here, the shape is not
	var d runner.Deps
	d.CorrelationWindow = func(ctx context.Context, at time.Time) (correlate.Window, error) {
		obs, err := store.Window(ctx, at, span)
		if err != nil {
			return correlate.Window{}, err
		}
		return correlate.Window{Span: span, Observations: obs}, nil
	}
	d.ExecClassRecord = db.NewExecClassStore(nil).Record
	if d.CorrelationWindow == nil || d.ExecClassRecord == nil {
		t.Fatal("the correlation seams are not constructible from the composition root's stores — the stage is dead")
	}
}
