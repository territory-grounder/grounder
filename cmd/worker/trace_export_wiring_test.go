package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/sessionspan"
	"github.com/territory-grounder/grounder/modules/observability/healthchecks"
	"github.com/territory-grounder/grounder/modules/observability/langfuse"
	"github.com/territory-grounder/grounder/modules/observability/openobserve"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// THE ALIVENESS GUARD FOR SESSION TRACE EXPORT (TG-44).
//
// This is the exact failure shape the guard exists for, and it is not hypothetical here — it is the ticket.
// openobserve.ExportSpans and langfuse.Record have both existed since spec/008, both fully unit-tested,
// both with tracing default-ON, and NEITHER had a composition-root caller. Every test in the tree was
// green, the modules declared the capability in their manifests, the console offered a dialog for it, and
// no trace had ever reached either store. INV-14 was satisfied by a method signature.
//
// So the unit suites cannot be the guard: they were all passing throughout. The guard has to live where the
// assignment happens.
//
// KILLING MUTATION (EXECUTED 2026-08-04): delete the `SessionSpans: sessionSpanSink,` line from the
// runner.Deps literal in main.go. RED here with the field named — while `go build`, `go vet`, and the whole
// temporal/runner + modules/observability suites stay green, which is exactly why this test is at the
// composition root and not in a package.
func TestSessionTraceExportIsWiredAtTheCompositionRoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// depsFields records the VALUE each field is assigned, not merely that the key is present. Presence
	// alone is too weak a guard: `SessionSpans: nil` compiles, keeps the field in the literal, and restores
	// the exact defect — a sink that is declared, documented and inert.
	depsFields := map[string]string{}
	idents := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			idents[x.Name] = true
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Deps" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "runner" {
				return true
			}
			for _, el := range x.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				val := ""
				if vid, ok := kv.Value.(*ast.Ident); ok {
					val = vid.Name
				}
				depsFields[key.Name] = val
			}
		}
		return true
	})

	// VACUITY FLOOR: an AST walk that matched nothing certifies nothing. Both maps must be populated before
	// any assertion below carries information.
	if len(idents) < 100 {
		t.Fatalf("vacuity floor: the walk found only %d identifiers in main.go — the matcher is broken", len(idents))
	}
	if len(depsFields) < 10 {
		t.Fatalf("vacuity floor: the walk found only %d fields on the runner.Deps literal — it did not find "+
			"the literal, so the field assertion below would pass over an empty set", len(depsFields))
	}

	got, present := depsFields["SessionSpans"]
	if !present {
		t.Error("runner.Deps.SessionSpans is not set at the composition root — the sink is nil-inert by " +
			"design, so the investigate activity's export block is skipped silently and TG returns to the " +
			"TG-44 state: ExportSpans exists, tracing is default-on, and no trace ever reaches a store")
	} else if got == "nil" || got == "" {
		t.Errorf("runner.Deps.SessionSpans is assigned %q — a hard nil (or a non-identifier) is the SAME "+
			"dead wiring as an absent field, wearing a busier diff: the field is present, the docs describe "+
			"it, and no trace ships", got)
	}
	// The sink must be BUILT, not just referenced.
	if !idents["sessionSpanSink"] {
		t.Error("main.go never mentions sessionSpanSink — the trace fanout is not constructed")
	}
	if !idents["Fanout"] {
		t.Error("main.go never constructs a sessionspan.Fanout — nothing enumerates the trace-capable exporters")
	}
	if !idents["TraceExporter"] {
		t.Error("main.go never asserts observability.TraceExporter — without the type assertion the " +
			"composition root cannot tell which configured exporter can take a trace, which is how both " +
			"implementations stayed uncalled for a year under two different method names")
	}
}

// TestBothTraceCapableModulesAreDiscoverable pins the type-assertion contract the composition root relies
// on, WITH its vacuity floor.
//
// The floor is the second assertion: healthchecks.io is a dead-man ping with no trace concept and MUST NOT
// satisfy the interface. Without that, an interface every exporter satisfied (say, because a stub was added
// "for symmetry") would make the discovery loop pick up sinks that silently drop what they are handed, and
// the positive assertions above would still pass.
func TestBothTraceCapableModulesAreDiscoverable(t *testing.T) {
	oo := openobserve.New("https://oo.example/api/default", config.SecretRef("env:TG_TEST_OO"))
	lf := langfuse.New("https://lf.example", config.SecretRef("env:TG_TEST_LF_PUB"), config.SecretRef("env:TG_TEST_LF_SEC"))
	hc := healthchecks.New("https://hc.example", config.SecretRef("env:TG_TEST_HC"))

	var found []string
	for _, exp := range []observability.Exporter{oo, lf, hc} {
		if _, ok := exp.(observability.TraceExporter); ok {
			found = append(found, exp.SourceType())
		}
	}
	if len(found) != 2 {
		t.Fatalf("trace-capable exporters discovered: %v — want exactly openobserve and langfuse", found)
	}
	// VACUITY FLOOR: prove the negative case is a real exclusion and not an empty interface set.
	if _, ok := observability.Exporter(hc).(observability.TraceExporter); ok {
		t.Error("healthchecks.io satisfies TraceExporter — a dead-man ping has no trace concept, and handing " +
			"it a span batch would be a sink that drops what it accepts")
	}
}

// TestFanoutSatisfiesTheRunnerSink pins the shape: a signature drift between sessionspan.Fanout and the
// Deps field is a compile error in main.go, but only once main.go is written against it — this fixes it
// independently of the AST walk.
func TestFanoutSatisfiesTheRunnerSink(t *testing.T) {
	oo := openobserve.New("https://oo.example/api/default", config.SecretRef("env:TG_TEST_OO"))
	var d runner.Deps
	d.SessionSpans = sessionspan.Fanout{oo}
	if d.SessionSpans == nil {
		t.Fatal("a Fanout over a real module did not satisfy runner.Deps.SessionSpans")
	}
	// Tracing withdrawn ⇒ ExportSpans is a documented no-op, never an error. Exercised here so the
	// composition root's "an export error is logged, never fatal" contract is not the only thing standing
	// between a disabled tracer and a log full of failures.
	off := openobserve.New("https://oo.example/api/default", config.SecretRef("env:TG_TEST_OO"), openobserve.WithTracing(false))
	if err := (sessionspan.Fanout{off}).ExportSpans(context.Background(), "INC-1", []string{"name=session.investigate"}); err != nil {
		t.Fatalf("a tracing-disabled module must be a silent no-op, got %v", err)
	}
}
