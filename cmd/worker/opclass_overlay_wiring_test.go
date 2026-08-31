package main

// WIRING ALIVENESS for the ratified-overlay refresh (TG-227 blockers 2+3).
//
// The behaviour oracles in opclass_overlay_refresh_test.go prove the refresher WORKS; this file proves
// main actually USES it. The distinction is the whole of TG-227: SetOverlay worked and had no caller,
// WithPerClassThreshold worked and no composition root installed it. A refactor that quietly drops one
// of these calls recreates the original defect with every unit test still green — so the calls
// themselves are pinned here, in the same AST style as axis_wiring_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// KILLING MUTATION: revert main.go:1931 to the bare policy.NewLadder call (dropping the resolver), or
// delete the boot RefreshOnce / go Run lines. Each counter below goes to zero and the test names which.
func TestRatifiedOverlayIsActuallyWiredInMain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var (
		ladderBuilt   int // buildGraduationLadder(policyGradStore, overlayRef.ThresholdFor)
		bootRefresh   int // overlayRef.RefreshOnce(...) — the synchronous pre-ladder load
		loopStarted   int // go overlayRef.Run(...)
		kickWired     int // Refreshed: overlayRef.Kick in the opclassratify Deps literal
		bareNewLadder int // policy.NewLadder called DIRECTLY in main — the regression shape
	)
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "buildGraduationLadder" {
				// The second argument must be the refresher's resolver, not nil — a nil resolver is
				// exactly TG-248 wearing a helper function.
				if len(node.Args) == 2 {
					if sel, ok := node.Args[1].(*ast.SelectorExpr); ok && sel.Sel.Name == "ThresholdFor" {
						ladderBuilt++
					}
				}
			}
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "RefreshOnce":
					bootRefresh++
				case "NewLadder":
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "policy" {
						bareNewLadder++
					}
				}
			}
		case *ast.GoStmt:
			if sel, ok := node.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "overlayRef" {
					loopStarted++
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := node.Key.(*ast.Ident); ok && k.Name == "Refreshed" {
				if sel, ok := node.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "Kick" {
					kickWired++
				}
			}
		}
		return true
	})

	if ladderBuilt != 1 {
		t.Errorf("buildGraduationLadder(…, overlayRef.ThresholdFor) appears %d times in main, want exactly 1 — "+
			"without it a class ratified at N=10 graduates at the compiled bar (TG-248)", ladderBuilt)
	}
	if bareNewLadder != 0 {
		t.Errorf("policy.NewLadder is called directly %d time(s) in main — every production ladder must go "+
			"through buildGraduationLadder so the per-class resolver cannot be dropped in a refactor", bareNewLadder)
	}
	if bootRefresh == 0 {
		t.Error("no synchronous overlayRef.RefreshOnce before the ladder is built — a grant ratified before " +
			"this boot would not be live for the first decision")
	}
	if loopStarted != 1 {
		t.Errorf("go overlayRef.Run(...) appears %d times, want exactly 1 — without the loop a revoke stays "+
			"live until restart", loopStarted)
	}
	if kickWired != 1 {
		t.Errorf("Refreshed: overlayRef.Kick appears %d times in the ratify Deps, want exactly 1 — without "+
			"the kick an operator's grant waits out the full TTL", kickWired)
	}
}
