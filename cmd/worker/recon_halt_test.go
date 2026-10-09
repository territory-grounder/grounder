package main

// THE KILL SWITCH REACHES THE READ LANE (TG-165), AND THE COMPOSITION ROOT ACTUALLY WIRES IT.
//
// Measured before this change: POST /halt called chokepoint.ForceShadow and nothing else, so it flipped the
// MUTATION posture — the half that Shadow had already stopped — and recon ran straight through the halt.
// An operator who pressed the only stop button TG has kept an enumerating worker enumerating.
//
// The second oracle here is the one this repository keeps needing: a bound that exists in a package but is
// never constructed at the composition root is a control that reads green and binds nothing.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// KILLING MUTATION: delete the `a.recon.Halt(...)` line from haltHandler. RED — the read lane keeps serving
// estate reads after an operator halt, and the /halt response's recon_halted goes false while claiming
// "halted": true.
func TestHaltStopsTheReadLaneNotOnlyMutation(t *testing.T) {
	a, _, gate := newTestAdmin(t, testHaltToken, true)
	governor := safety.NewReconGovernor(safety.DefaultReconBudget(), gate)
	a.withRecon(governor)

	if err := governor.Admit("triage-1"); err != nil {
		t.Fatalf("reads must be served before the halt: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/halt", nil)
	req.Header.Set("Authorization", "Bearer "+testHaltToken)
	rec := httptest.NewRecorder()
	a.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /halt = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if err := governor.Admit("triage-1"); err == nil {
		t.Fatal("estate reads were still served after POST /halt — the kill switch stopped only the mutation " +
			"posture, leaving recon (the pre-actuation half of the attack chain) running at full rate")
	}
	if gate.MayActuate() {
		t.Fatal("the halt must still drop the mutation posture — this is additive, not a replacement")
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /halt response: %v", err)
	}
	if body["recon_halted"] != true {
		t.Fatalf("the /halt response must tell the operator whether the READ lane stopped too; got %v", body)
	}
}

// The read-lane counters must reach /metrics, or a recon burst is legible only to whoever is tailing the log
// at that minute. tg_recon_reads_total is also the answer to "is this bound wired at all?".
//
// KILLING MUTATION: drop the `if a.recon != nil` block from samples(). RED.
func TestReconCountersReachMetrics(t *testing.T) {
	a, _, gate := newTestAdmin(t, testHaltToken, false)
	body := func() string {
		rec := httptest.NewRecorder()
		a.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	// Unwired: no series at all. An absent series says "not metered"; a zero would say "no reads happened",
	// and those are different claims.
	if got := body(); strings.Contains(got, "tg_recon_reads_total") {
		t.Fatalf("an unwired governor must publish no recon series; got:\n%s", got)
	}
	governor := safety.NewReconGovernor(safety.DefaultReconBudget(), gate)
	a.withRecon(governor)
	governor.Record("triage-1", "get-host-logs", "dc1pve01")
	governor.Record("triage-1", "get-host-logs", "dc1pve02")
	got := body()
	for _, want := range []string{
		"tg_recon_reads_total 2",
		"tg_recon_reads_hour 2",
		"tg_recon_targets_hour 2", // the fan-out signal: two DISTINCT estate objects
		"tg_recon_refused_total 0",
		"tg_recon_halted 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q on /metrics:\n%s", want, got)
		}
	}
}

// THE COMPOSITION ROOT MUST BUILD AND WIRE THE GOVERNOR. A recon budget that is never constructed, or is
// constructed and handed to nobody, is exactly the shape of defect TG-165 exists to remove: a control that
// is present in the tree, tested in its own package, and binding nothing in production.
//
// KILLING MUTATION: delete the `Recon: reconGovernor` line from runner.Deps in main.go (or the
// `.withRecon(reconGovernor)` on the admin surface). RED.
func TestReconGovernorIsWiredByTheCompositionRoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	calls := map[string]bool{}
	fields := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			// Both call shapes, deliberately: a walk that matches only one node kind certifies nothing about
			// the other (cmd/worker/discovery_runner_test.go records the same lesson).
			switch fn := v.Fun.(type) {
			case *ast.SelectorExpr:
				// The bare selector is ALWAYS recorded, and the qualified form additionally when the receiver
				// is a plain identifier — so `safety.NewReconGovernor(...)`, `reconGovernor.SeedFromLedger(...)`
				// and the chained `newWorkerAdmin(...).withRecon(...)` are all visible to one matcher.
				calls[fn.Sel.Name] = true
				if x, ok := fn.X.(*ast.Ident); ok {
					calls[x.Name+"."+fn.Sel.Name] = true
				}
			case *ast.Ident:
				calls[fn.Name] = true
			}
		case *ast.KeyValueExpr:
			if k, ok := v.Key.(*ast.Ident); ok {
				fields[k.Name] = true
			}
		}
		return true
	})
	if !calls["safety.NewReconGovernor"] {
		t.Error("cmd/worker/main.go never constructs a safety.ReconGovernor — the read lane has no volume " +
			"bound in production, whatever core/safety's own tests prove")
	}
	if !calls["withRecon"] {
		t.Error("the governor never reaches the admin surface — POST /halt would again stop mutation only, " +
			"and no recon counter would reach /metrics")
	}
	if !calls["SeedFromLedger"] {
		t.Error("the rolling hour is never seeded from agent_step_evidence — a restart hands whatever was " +
			"mid-burst a brand-new hour, which is the cheapest possible bypass")
	}
	if !fields["Recon"] {
		t.Error("runner.Deps.Recon is never set — the agent loop consults a nil limiter and every estate read " +
			"is admitted, exactly as before TG-165")
	}
	// Vacuity floor: if the walk found nothing, every assertion above is a green tick over an empty scan.
	if len(calls) < 50 || len(fields) < 50 {
		t.Fatalf("vacuity floor: the AST walk found %d calls / %d struct fields in main.go — the matcher is "+
			"broken and a passing run would certify nothing", len(calls), len(fields))
	}
}
