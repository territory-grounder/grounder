package main

// ORACLES FOR THE CAPABILITY-PROJECTION PUBLISHER (TG-251).

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules"
)

type recordingPublisher struct {
	rows [][]db.CapabilityProjectionRow
	err  error
}

func (r *recordingPublisher) Publish(_ context.Context, rows []db.CapabilityProjectionRow) error {
	r.rows = append(r.rows, rows)
	return r.err
}

// KILLING MUTATION: drop the Enabled bit in capabilityRows (or hardcode it). RED — an enabled/disabled
// transposition here silently recreates the "everything reads as off" defect at the publishing side.
func TestCapabilityRowsCarryTheEnabledBitFaithfully(t *testing.T) {
	rows := capabilityRows([]modules.Capability{
		{Surface: "notifier", SourceType: "matrix", Capability: "notify", Enabled: true},
		{Surface: "actuation", SourceType: "ssh", Capability: "actuate", Enabled: false},
	})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if !rows[0].Enabled || rows[0].Surface != "notifier" || rows[0].SourceType != "matrix" {
		t.Fatalf("row 0 mangled: %+v", rows[0])
	}
	if rows[1].Enabled {
		t.Fatal("a DISABLED capability published as enabled — the transposition this oracle exists for")
	}
}

// KILLING MUTATION: make publishCapabilityProjection fatal (or panic) on a store error. RED — the
// projection is observability; its failure degrades the console to unknown, never the worker to dead.
func TestAPublishFailureIsLoudButNeverFatal(t *testing.T) {
	reg := modules.NewRegistry()
	var logged int
	pub := &recordingPublisher{err: errors.New("db down")}
	publishCapabilityProjection(context.Background(), reg, pub, func(string, ...any) { logged++ })
	if logged == 0 {
		t.Fatal("a failed publish was silent")
	}
}

// KILLING MUTATION: delete the go runCapabilityProjection(...) line in main. RED — the publisher exists
// and nothing runs it: the exact presence-without-wiring shape this repo documents.
func TestCapabilityProjectionIsActuallyStartedInMain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	started := 0
	ast.Inspect(f, func(n ast.Node) bool {
		g, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if id, ok := g.Call.Fun.(*ast.Ident); ok && id.Name == "runCapabilityProjection" {
			started++
		}
		return true
	})
	if started != 1 {
		t.Fatalf("go runCapabilityProjection appears %d times in main, want exactly 1 — the worker must "+
			"publish its module enablement or the API process is guessing again (TG-251)", started)
	}
}
