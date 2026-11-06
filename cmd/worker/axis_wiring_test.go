package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE SAMPLER MUST BE ARMED, and only the call site can prove it.
//
// Same shape as the expiry-risk wiring above, and for the same reason: startAxisSampler lives in main(), which
// no test calls. The sampler can be constructed, unit-tested and left unstarted — in which case /metrics stays
// silent forever and the axes remain CLI-only, which is exactly the gap this work exists to close. A control
// that only proves Collect() renders correctly would pass on a build where nothing ever samples.
func TestAxisSamplerIsActuallyArmedInMain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var started, withStore, published int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// NOTE: check the SELECTOR form too, and do not return early on it. The first draft of this control
		// bailed out on every non-Ident call before reaching the selector branch, so `adm.withAxisSampler(...)`
		// — a SelectorExpr — was never seen and the check failed for the wrong reason. It failed loudly rather
		// than passing vacuously, which is the only reason the mistake was cheap.
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "startAxisSampler" {
			started++
			// arg 3 must be a real store constructor, not nil: a sampler with no store never reads
			if len(call.Args) >= 3 {
				if inner, ok := call.Args[2].(*ast.CallExpr); ok {
					if sel, ok := inner.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "NewAxisReadStore" {
						withStore++
					}
				}
			}
		}
		// and the sampler must reach the admin surface, or nothing renders it
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "withAxisSampler" {
			published++
		}
		return true
	})
	if started == 0 {
		t.Error("startAxisSampler is never called in main.go — the axes stay CLI-only, which is the defect this closes")
	}
	if withStore == 0 {
		t.Error("startAxisSampler is called without db.NewAxisReadStore(...) — a sampler with no store never reads")
	}
	if published == 0 {
		t.Error("withAxisSampler is never called — the sampler reads and nothing renders it on /metrics")
	}
}
