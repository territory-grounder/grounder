package main

// WIRING ALIVENESS for the judge's estate grounding (TG-202).
//
// The behaviour oracles (core/judge/estate_test.go) prove the topology check WORKS and the batch oracle
// (temporal/skilljudge) proves the cron CALLS it — but both construct their own graph. This file proves the
// production worker hands the judge the LIVE one. That is the whole shape of the defect being closed:
// core/judge had zero estate references while this same process built, held and refreshed a 700-edge causal
// graph two thousand lines further up. A refactor that quietly drops the field recreates it with every unit
// test still green, so the call is pinned here in the same AST style as opclass_overlay_wiring_test.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// KILLING MUTATION: delete `Estate: estateHolder` from the skilljudge.Deps literal in main.go. RED — the
// judge then scores every root cause with no access to the runs_on/depends_on structure that decides whether
// the named cause can reach the alerting host, and a diagnosis blaming the wrong hypervisor is pooled exactly
// like a correct one.
func TestJudgeIsGivenTheLiveEstateGraphInMain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var (
		judgeDeps   int // skilljudge.Deps composite literals seen at all (the VACUITY floor for this scan)
		estateWired int // Estate: estateHolder inside one of them
		holderBuilt int // estate.NewHolder(...) — the live, refreshable snapshot this must be fed from
	)
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewHolder" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "estate" {
					holderBuilt++
				}
			}
		case *ast.CompositeLit:
			sel, ok := node.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Deps" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "skilljudge" {
				return true
			}
			judgeDeps++
			for _, el := range node.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				k, ok := kv.Key.(*ast.Ident)
				if !ok || k.Name != "Estate" {
					continue
				}
				// It must be the HOLDER, not a one-shot graph: the judge has to see the topology as it is
				// now, so a refresh (or a recovery from a source outage) reaches the dimension without a
				// worker restart.
				if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "estateHolder" {
					estateWired++
				}
			}
		}
		return true
	})

	// VACUITY FLOOR: if the scan found no skilljudge.Deps literal at all it is measuring nothing, and every
	// assertion below would pass by accident on a renamed package or a moved composition root.
	if judgeDeps == 0 {
		t.Fatal("no skilljudge.Deps literal found in main.go — this scan matched NOTHING, so it proves nothing " +
			"about the judge's wiring; fix the scan before trusting it")
	}
	if holderBuilt == 0 {
		t.Fatal("main.go builds no estate.NewHolder — the live graph this dimension reads does not exist here")
	}
	if estateWired != 1 {
		t.Errorf("`Estate: estateHolder` appears %d time(s) in the skilljudge Deps, want exactly 1 — without it "+
			"the durable judge scores a stated root cause with NO access to the causal estate graph this same "+
			"worker builds and refreshes, which is TG-202's defect verbatim (core/judge: zero estate references)",
			estateWired)
	}
}
