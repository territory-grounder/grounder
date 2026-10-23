package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/safety"
)

// fakeShadowLister drives the adapter without a database so the composition oracle is always-on.
type fakeShadowLister struct {
	rows  []db.ShadowProposalRow
	total int
	// cfIncidents/cfAddressed are what the store "holds"; gotSince captures the instant the adapter
	// derived from the window, which is the only place the window→timestamp arithmetic can go wrong.
	cfIncidents, cfAddressed, cfExecuted int
	gotSince                             *time.Time
}

func (f fakeShadowLister) ListShadowProposals(_ context.Context, limit int) ([]db.ShadowProposalRow, error) {
	if limit < len(f.rows) {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}
func (f fakeShadowLister) CountShadowProposals(_ context.Context) (int, error) { return f.total, nil }
func (f fakeShadowLister) CounterfactualSince(_ context.Context, since time.Time) (int, int, int, error) {
	if f.gotSince != nil {
		*f.gotSince = since
	}
	return f.cfIncidents, f.cfAddressed, f.cfExecuted, nil
}

// TestCounterfactualAdapterLooksBackwardsOverTheWindow — the adapter's whole job is turning a DURATION
// into an INSTANT, and the sign of that subtraction is the one thing it can get wrong silently: querying
// `now + 7d` finds no rows, reports "0 of 0 incidents", and reads on the console as a quiet week rather
// than as a bug. The store cannot catch this (it is handed a valid timestamp either way) and the handler
// cannot (it is handed two valid ints), so it has to be caught here.
//
// RED mutation control (executed 2026-08-01): with `.Add(-window)` changed to `.Add(window)` this fails
// `the adapter must look BACKWARDS`; restored green.
func TestCounterfactualAdapterLooksBackwardsOverTheWindow(t *testing.T) {
	var since time.Time
	fake := fakeShadowLister{cfIncidents: 17, cfAddressed: 14, gotSince: &since}
	before := time.Now()
	inc, addr, _, err := (proposalsReadStore{s: fake}).Counterfactual(context.Background(), auth.Principal{}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("counterfactual: %v", err)
	}
	if inc != 17 || addr != 14 {
		t.Fatalf("the store's counts must pass through unaltered, got %d/%d", addr, inc)
	}
	if !since.Before(before) {
		t.Fatalf("the adapter must look BACKWARDS: asked the store for rows since %v, which is not before now (%v)", since, before)
	}
	if d := before.Sub(since); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Fatalf("a 7-day window must look back ~7 days, looked back %v", d)
	}
}

// TestProposalsAdapterMapsEveryFieldAndTheHonestTotal is the COMPOSITION oracle for spec/026 REQ-2607.
// The Stage-1 adversarial review proved the shipped binary served a permanently-dead /v1/proposals: every
// unit oracle was green (parser, handler, console) while Deps.Proposals was never wired, and the fail-closed
// 503 made deadness look intentional. This test pins the adapter's field mapping and the REAL total
// (store count, never page size); TestBuildPublicAPIMountsReadOnlySurface's sibling below pins the wiring.
func TestProposalsAdapterMapsEveryFieldAndTheHonestTotal(t *testing.T) {
	created := time.Date(2026, 7, 31, 13, 31, 2, 0, time.UTC)
	fake := fakeShadowLister{
		rows: []db.ShadowProposalRow{{
			ExternalRef: "librenms-dc1-1406",
			Host:        "dc1excalidraw01",
			AlertRule:   "Devices up/down",
			Op:          "start guest",
			OpClass:     "start-guest",
			Rationale:   "guest stopped by authored action; propose start, human confirms",
			UndoSketch:  "stop-guest returns the estate to the observed state",
			Confidence:  0.92,
			Attribution: `[{"actor":"root@pam","verb":"vzstop"}]`,
			CreatedAt:   created,
			// TG-307: the derived diagnosis signal must reach the console DTO — an operator reviewing this
			// proposal cannot see "the agent held evidence against its own root cause" if the adapter drops it.
			DiagnosisRecorded:     true,
			DiagnosisContradicted: true,
			DiagnosisUncited:      2,
		}},
		total: 7, // more than the page — the badge must render the STORE count, not len(rows)
	}
	got, total, err := proposalsReadStore{s: fake}.ShadowProposals(context.Background(), auth.Principal{}, 100)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if total != 7 {
		t.Fatalf("honest total must be the store count 7, got %d", total)
	}
	if len(got) != 1 {
		t.Fatalf("rows: want 1, got %d", len(got))
	}
	r := got[0]
	if r.ExternalRef != "librenms-dc1-1406" || r.Host != "dc1excalidraw01" ||
		r.AlertRule != "Devices up/down" || r.Op != "start guest" || r.OpClass != "start-guest" ||
		r.Rationale == "" || r.UndoSketch == "" || r.Confidence != 0.92 ||
		r.Attribution == "" || !r.CreatedAt.Equal(created) {
		t.Fatalf("adapter dropped or mutated a field: %+v", r)
	}
	// TG-307: the diagnosis signal is the reason this lane changed; a dropped field here is the render going
	// dark with every other test green (the exact shape the composition oracle above exists to prevent).
	if !r.DiagnosisRecorded || !r.DiagnosisContradicted || r.DiagnosisUncited != 2 {
		t.Fatalf("adapter dropped the diagnosis signal: recorded=%v contradicted=%v uncited=%d (want true/true/2) — "+
			"an operator reviewing this proposal would never learn the agent recorded evidence against its own "+
			"root cause", r.DiagnosisRecorded, r.DiagnosisContradicted, r.DiagnosisUncited)
	}
}

// TestMainWiresTheProposalsReader is the aliveness half — and until TG-258 it was a gate that COULD NOT
// FAIL. Its entire body was `var _ = proposalsReadStore{s: db.NewTriageStore(nil)}`: a compile-time
// interface check over a locally constructed value, which stays green no matter what cmd/grounder/main.go
// passes at the proposals positions. Replacing BOTH proposals arguments in main.go's buildPublicAPI call
// with nil — the exact Stage-1 defect, Deps.Proposals unwired, /v1/proposals permanently 503, the console
// rendering "proposals unavailable" forever while every unit oracle stayed green — left this test PASSING,
// and review prose cited it as proof the surface is wired. A named oracle that survives its own defect is
// worse than no oracle: it converts "nobody checked" into "CI checked and it is fine".
//
// The mechanism follows this repo's established precedent for exactly this shape —
// cmd/worker's TestCompositionRootDoesNotBypassTheKnowledgeHolderGuard and
// TestWorldDiscoveryIsReachableFromTheCompositionRoot — which read cmd/*/main.go as SOURCE. A unit test
// proves a function works; it structurally cannot prove that anything CALLS it, so the call site has to be
// read at the one place that decides whether the surface is alive: the composition root.
//
// THREE halves that fail independently, because the surface can go dark at three unrelated hops. The first
// version of this fix had only the first two, and an adversarial re-review killed the shipped surface twice
// while it stayed green — a wiring guard that follows the chain PART of the way certifies exactly the hops it
// walks and silently blesses the rest:
//
//	main.go        — must pass a LIVE adapter over the real triage store at both proposals positions
//	                 (asserted structurally; there is no seam to call, main() is not callable from a test);
//	buildPublicAPI — must thread those two parameters into httpapi.Deps (asserted BEHAVIOURALLY by serving
//	                 a real signed request, because a source grep for `Proposals: proposalsRead` has been
//	                 too weak in this repo three times — see cmd/worker/world_discovery_wiring_test.go);
//	main.go again  — must actually SERVE the router it just built, at the root pattern, on the listener.
//
// KILLING MUTATIONS (all executed 2026-08-03, each reverted green):
//   - main.go: pass `nil, nil` at the two proposals positions        -> RED, "passes a literal nil"
//   - deps.go: set `Proposals: nil` in the Deps literal              -> RED, "answered 503"
//   - main.go: `db.NewTriageStore(pool)` -> `db.NewTriageStore(noPool)` for a declared-but-never-connected
//     `var noPool *db.Pool`                                          -> RED, "not the pool main.go connects"
//   - main.go: replace `root.Handle("/", api.Mux())` with `_ = api`  -> RED, "builds the public router and
//     never serves it"
func TestMainWiresTheProposalsReader(t *testing.T) {
	t.Run("main.go passes a live proposals reader at both composition positions", func(t *testing.T) {
		params := buildPublicAPIParams(t)
		args := mainBuildPublicAPIArgs(t)
		pools := connectedPoolNames(t, mainAST(t))
		// Arity is checked before anything is indexed: buildPublicAPI takes dozens of positional dependencies
		// (48 as of TG-163, and the count moves every time a surface is wired), so a
		// guard that mapped a hard-coded index would silently certify some OTHER dependency the day a
		// parameter is inserted, while the proposals surface went dark. Positions are derived from the
		// declared parameter TYPES in deps.go and matched against the call in main.go; a mismatch means this
		// guard can no longer say which argument is which, and it must say so rather than guess.
		if len(args) != len(params) {
			t.Fatalf("buildPublicAPI declares %d parameters but main.go passes %d arguments — this guard "+
				"cannot map argument positions to parameters and is therefore asserting NOTHING about the "+
				"proposals wiring", len(params), len(args))
		}
		for _, want := range []struct{ typ, consequence string }{
			{"httpapi.ProposalsReader",
				"httpapi.Deps.Proposals is nil, so GET /v1/proposals fail-closes to 503 for the life of the " +
					"process and the console renders 'proposals unavailable' — the Stage-1 defect verbatim: a " +
					"surface with a parser, a handler, a console view, a migration and a full green unit suite, " +
					"serving nothing"},
			{"httpapi.CounterfactualReader",
				"httpapi.Deps.Counterfactual is nil, so the list still serves and the HEADLINE silently " +
					"disappears — 'TG saw N incidents this week and would have addressed M' just stops " +
					"rendering, with no error anywhere, which reads as a quiet week rather than as a dead seam"},
		} {
			idx := -1
			for i, p := range params {
				if p.typ == want.typ {
					idx = i
				}
			}
			// Vacuity floor: if the parameter type is gone (renamed, removed, replaced by a struct), this
			// guard is looking for something that no longer exists and must fail LOUDLY rather than pass by
			// finding nothing — "absent is visible; skipped is not".
			if idx < 0 {
				t.Fatalf("VACUITY: buildPublicAPI has no %s parameter — this guard checked nothing for the "+
					"proposals surface. Re-aim it at whatever now carries that dependency.", want.typ)
			}
			arg := args[idx]
			if id, ok := arg.(*ast.Ident); ok && id.Name == "nil" {
				t.Errorf("cmd/grounder/main.go passes a literal nil for %s (argument %d, parameter %q): %s",
					want.typ, idx, params[idx].name, want.consequence)
				continue
			}
			// Not-nil is not enough: `proposalsReadStore{}` and `proposalsReadStore{s: db.NewTriageStore(nil)}`
			// both compile and both produce a surface that panics or errors on every request while looking
			// wired at the call site. The argument must construct the adapter over the REAL pool.
			store, ok := triageStoreArg(arg)
			if !ok {
				t.Errorf("cmd/grounder/main.go passes %q for %s (argument %d), which never calls "+
					"db.NewTriageStore — the proposals surface is not backed by the triage store, and %s",
					types.ExprString(arg), want.typ, idx, want.consequence)
				continue
			}
			if id, ok := store.(*ast.Ident); ok && id.Name == "nil" {
				t.Errorf("cmd/grounder/main.go builds %s over db.NewTriageStore(nil) (argument %d): a store "+
					"with no pool answers every read with an error, so the handler maps it to the SAME 503 as "+
					"an unwired reader and %s", want.typ, idx, want.consequence)
				continue
			}
			// Rejecting only the literal `nil` spelling was this guard's own version of the defect it exists
			// for. `var noPool *db.Pool` (a second pool that is declared and never connected — the shape a
			// read-replica or a "connect it later" refactor produces) is a NIL POOL wearing an identifier, and
			// db.NewTriageStore(noPool) then answers every read with an error, which the handler maps to the
			// same permanent 503 as an unwired reader. Executed against the guard before this line existed: it
			// passed green. So the pool must be an identifier main.go actually BINDS from db.Connect, not
			// merely one that is not spelled "nil".
			id, ok := store.(*ast.Ident)
			if !ok || !pools[id.Name] {
				t.Errorf("cmd/grounder/main.go builds %s over db.NewTriageStore(%s) (argument %d), which is "+
					"not the pool main.go connects (%s): a store over an unconnected pool answers every read "+
					"with an error, the handler maps it to the SAME 503 as an unwired reader, and %s",
					want.typ, types.ExprString(store), idx, poolNamesForMessage(pools), want.consequence)
			}
		}
	})

	t.Run("main.go serves the router it builds", func(t *testing.T) {
		// The two halves above prove the reader reaches httpapi.Deps and that Deps serves it. Neither walks
		// the LAST hop, and a guard that stops one hop short certifies the hops it walked and blesses the
		// rest: replacing `root.Handle("/", api.Mux())` with `_ = api` leaves the composition root perfect,
		// the router fully populated, every argument live — and /v1/proposals (with every other authenticated
		// route) 404s in the shipped binary, which is the Stage-1 symptom exactly, reached by a different
		// road. Executed against the previous version of this file: green. So the chain
		// buildPublicAPI(...) -> <router>.Mux() -> mounted at "/" -> handed to the listener is walked here.
		file := mainAST(t)
		router := buildPublicAPIResultName(t, file)
		mux := router + ".Mux()"
		served := servedHandlers(t, file)
		if served[mux] {
			return // handed straight to the listener — nothing can shadow it
		}
		mounts := handleMounts(file, mux)
		if len(mounts) == 0 {
			t.Fatalf("cmd/grounder/main.go builds the public router (%s := buildPublicAPI(...)) and never "+
				"mounts %s on anything: every authenticated route — /v1/proposals among them — is absent from "+
				"the served binary while this file's other halves, the handler tests, the contract check and "+
				"the console all stay green. That is the Stage-1 defect with a different last hop.",
				router, mux)
		}
		for _, m := range mounts {
			// The pattern matters as much as the mount: http.ServeMux routes by prefix, so mounting the
			// authenticated router at anything narrower than "/" (say "/v1/stats") silently drops every path
			// outside that prefix — /v1/proposals 404s with the router demonstrably built and demonstrably
			// mounted, which is the hardest version of this bug to see in a diff.
			if m.pattern == "/" && served[m.mux] {
				return
			}
		}
		t.Errorf("cmd/grounder/main.go mounts %s (%s) but no mount both uses the root pattern \"/\" and lands "+
			"on a handler passed to the listener (served: %s): the public router is built and dropped, so "+
			"/v1/proposals answers 404 in the shipped binary with every unit oracle green",
			mux, mountsForMessage(mounts), poolNamesForMessage(served))
	})

	t.Run("buildPublicAPI serves the proposals surface from those parameters", func(t *testing.T) {
		// The structural half above cannot see inside buildPublicAPI: main.go can pass a perfect adapter into
		// a Deps literal that drops it on the floor (`Proposals: nil`), which is the same dead surface with a
		// clean call site. This half drives the router main.go actually serves, with the fake standing in for
		// the pgx store, and asserts the store's OWN rows and counts reach the wire.
		secret := []byte("proposals-composition-oracle-not-a-credential")
		v, err := auth.NewVerifier(fixedSource{secret: secret}, freshNonces{}, time.Minute)
		if err != nil {
			t.Fatalf("verifier: %v", err)
		}
		reader := proposalsReadStore{s: fakeShadowLister{
			rows:        []db.ShadowProposalRow{{ExternalRef: "librenms-dc1-1406", Host: "dc1excalidraw01"}},
			total:       7, // more than the page — the badge must carry the STORE count
			cfIncidents: 41, cfAddressed: 29, cfExecuted: 0,
		}}
		api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(),
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, // ledger…modeTransition + engineToggle + posture: every
			// other dependency stays nil, which is the same fail-closed 503 in this router that an unwired
			// proposals reader would be — so a 200 below can only come from the reader threaded through.
			0,                            // postureStaleAfter
			nil, nil, nil, nil, nil, nil, // sessionDetailRead, sessionEvidenceRead, sessionDiagnosisRead, credentialOnboarding, actions, suppression
			reader, reader, // proposalsRead, counterfactualRead — THE SEAM UNDER TEST
			nil, nil, nil, nil, nil, nil, nil, nil, 0, nil, nil)

		w := httptest.NewRecorder()
		api.Mux().ServeHTTP(w, signedGet(t, "/v1/proposals?limit=100", secret))
		if w.Code != http.StatusOK {
			t.Fatalf("the served proposals surface answered %d, not 200 — the reader handed to buildPublicAPI "+
				"never reaches httpapi.Deps.Proposals, which is the permanently-dead /v1/proposals this whole "+
				"file exists to make un-shippable; body: %s", w.Code, w.Body.String())
		}
		var page struct {
			Proposals []struct {
				ExternalRef string `json:"external_ref"`
				Host        string `json:"host"`
			} `json:"proposals"`
			Total          int `json:"total"`
			Counterfactual *struct {
				Incidents int `json:"incidents"`
				Addressed int `json:"addressed"`
			} `json:"counterfactual"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v (body %s)", err, w.Body.String())
		}
		if len(page.Proposals) != 1 || page.Proposals[0].ExternalRef != "librenms-dc1-1406" {
			t.Fatalf("the store's row did not reach the wire — the reader is mounted but not threaded: %s",
				w.Body.String())
		}
		if page.Total != 7 {
			t.Errorf("total = %d, want the STORE's 7: a total equal to the page length is the fabricated "+
				"count INV-15 bans", page.Total)
		}
		if page.Counterfactual == nil {
			t.Fatal("no counterfactual headline in the served view — the CounterfactualReader parameter is " +
				"not threaded into httpapi.Deps, so the one legible number on the proposals lane ('TG saw N " +
				"incidents and would have addressed M') is absent with nothing saying why")
		}
		if page.Counterfactual.Incidents != 41 || page.Counterfactual.Addressed != 29 {
			t.Errorf("the headline must carry the store's real figures, got %d/%d, want 41/29",
				page.Counterfactual.Incidents, page.Counterfactual.Addressed)
		}
	})
}

// param is one positional dependency of buildPublicAPI, flattened out of its (possibly grouped) declaration.
type param struct{ name, typ string }

// buildPublicAPIParams reads the composition function's parameter list from deps.go IN ORDER, so the guard
// above can locate the proposals dependencies by TYPE instead of by a hard-coded index that would rot into a
// silent mis-aim the first time a parameter is inserted ahead of them.
func buildPublicAPIParams(t *testing.T) []param {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "deps.go", nil, 0)
	if err != nil {
		t.Fatalf("parse deps.go: %v", err)
	}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "buildPublicAPI" || fn.Recv != nil {
			continue
		}
		var out []param
		for _, field := range fn.Type.Params.List {
			typ := types.ExprString(field.Type)
			if len(field.Names) == 0 {
				out = append(out, param{typ: typ})
				continue
			}
			for _, n := range field.Names {
				out = append(out, param{name: n.Name, typ: typ})
			}
		}
		return out
	}
	t.Fatal("deps.go declares no buildPublicAPI — the composition root this guard reads has moved, and the " +
		"guard is now asserting nothing about the proposals surface")
	return nil
}

// mainAST parses cmd/grounder/main.go — the composition root, the one file that decides whether the shipped
// binary serves anything. A parse failure is a t.Fatal rather than a skip: a guard that cannot read the file
// it certifies must say so, because "absent is visible; skipped is not" is the whole reason TG-258 exists.
func mainAST(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v — this guard can no longer read the composition root and is asserting "+
			"NOTHING about the proposals surface", err)
	}
	return file
}

// connectedPoolNames returns every identifier main.go binds from db.Connect(...), i.e. the names that hold a
// pool with a real connection behind it. It exists so the guard above can reject db.NewTriageStore(x) for any
// x that is not one of them — a declared-but-unconnected `var noPool *db.Pool` is as dead as a literal nil and
// produces the identical permanent 503, but is not spelled "nil".
func connectedPoolNames(t *testing.T, file *ast.File) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Connect" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "db" {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			names[id.Name] = true
		}
		return true
	})
	// Vacuity floor: with no known-live pool this guard cannot distinguish a real store from a dead one, and
	// would pass every argument it is shown. It must fail loudly and be re-aimed, never quietly wave things
	// through — that quiet wave-through is the exact class of defect TG-258 is closing.
	if len(names) == 0 {
		t.Fatal("VACUITY: cmd/grounder/main.go binds no identifier from db.Connect(...) — this guard cannot " +
			"tell a live pool from a never-connected one, so it can no longer prove the proposals surface is " +
			"backed by a real database. Re-aim it at however the pool is now obtained.")
	}
	return names
}

// buildPublicAPIResultName returns the identifier main.go assigns the buildPublicAPI result to, so the serve
// half can follow that exact value to the listener. A result assigned to `_` (or to nothing) is already the
// bug: the router is built and discarded.
func buildPublicAPIResultName(t *testing.T, file *ast.File) string {
	t.Helper()
	name := ""
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "buildPublicAPI" {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			name = id.Name
		}
		return true
	})
	if name == "" || name == "_" {
		t.Fatal("cmd/grounder/main.go does not bind the buildPublicAPI result to a name it can serve — the " +
			"authenticated public router, /v1/proposals included, is constructed and thrown away")
	}
	return name
}

// servedHandlers returns the expressions main.go actually hands to an HTTP listener — the ListenAndServe
// handler argument and any http.Server Handler field. This is the far end of the chain; a mount that never
// reaches one of these is decoration.
func servedHandlers(t *testing.T, file *ast.File) map[string]bool {
	t.Helper()
	served := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "http" {
				return true
			}
			switch sel.Sel.Name {
			case "ListenAndServe":
				if len(v.Args) == 2 {
					served[types.ExprString(v.Args[1])] = true
				}
			case "ListenAndServeTLS":
				if len(v.Args) == 4 {
					served[types.ExprString(v.Args[3])] = true
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := v.Key.(*ast.Ident); ok && id.Name == "Handler" {
				served[types.ExprString(v.Value)] = true
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Handler" && i < len(v.Rhs) {
					served[types.ExprString(v.Rhs[i])] = true
				}
			}
		}
		return true
	})
	if len(served) == 0 {
		t.Fatal("VACUITY: cmd/grounder/main.go passes no handler to any listener this guard recognises " +
			"(http.ListenAndServe / http.ListenAndServeTLS / an http.Server Handler) — it cannot tell whether " +
			"the public router is served at all. Re-aim it at however the process now listens.")
	}
	return served
}

// mount is one `<mux>.Handle(<pattern>, <handler>)` registration in main.go.
type mount struct{ mux, pattern string }

// handleMounts finds every registration of the given handler expression on a ServeMux, with its pattern, so
// the serve half can insist on BOTH facts that matter: the pattern is the root (http.ServeMux routes by
// prefix, so a narrower one drops /v1/proposals) and the mux it lands on is itself served.
func handleMounts(file *ast.File, handler string) []mount {
	var out []mount
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Handle" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || types.ExprString(call.Args[1]) != handler {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out = append(out, mount{mux: recv.Name, pattern: pattern})
		return true
	})
	return out
}

// poolNamesForMessage / mountsForMessage render a set deterministically, so a failure message names the exact
// alternatives the guard saw rather than a map printed in random order (a diagnostic that reads differently on
// every run trains readers to ignore it).
func poolNamesForMessage(set map[string]bool) string {
	var names []string
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func mountsForMessage(mounts []mount) string {
	var out []string
	for _, m := range mounts {
		out = append(out, m.mux+".Handle("+strconv.Quote(m.pattern)+")")
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// mainBuildPublicAPIArgs returns the arguments of THE call to buildPublicAPI in main.go. It insists on
// exactly one call: two calls would mean the served router is chosen somewhere this guard does not read, and
// checking either one of them would be a coin flip dressed up as proof.
func mainBuildPublicAPIArgs(t *testing.T) []ast.Expr {
	t.Helper()
	file := mainAST(t)
	var calls []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "buildPublicAPI" {
				calls = append(calls, call)
			}
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("cmd/grounder/main.go calls buildPublicAPI %d times, want exactly 1: with none the public "+
			"API is built somewhere this composition guard cannot see (and /v1/proposals could be dead in the "+
			"shipped binary with every test green); with several, this guard cannot tell which router is "+
			"served", len(calls))
	}
	return calls[0].Args
}

// triageStoreArg finds the db.NewTriageStore(...) call inside a composition argument and returns the
// expression it is handed. It searches the whole expression rather than pattern-matching
// `proposalsReadStore{s: db.NewTriageStore(pool)}` verbatim, so an honest refactor of the adapter type keeps
// passing while the property that matters — this surface is backed by the real triage store over the real
// pool — is still enforced.
func triageStoreArg(arg ast.Expr) (ast.Expr, bool) {
	var found ast.Expr
	ast.Inspect(arg, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewTriageStore" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "db" {
			return true
		}
		if len(call.Args) == 1 {
			found = call.Args[0]
		}
		return true
	})
	return found, found != nil
}

// The compile-time contract, stated at PACKAGE scope where the compiler is what checks it: if db.TriageStore
// ever stops satisfying shadowProposalLister, this fails to BUILD instead of 503-ing in production. It used
// to live inside TestMainWiresTheProposalsReader, where it made a test that asserted nothing report PASS —
// the defect TG-258 records. Same statement, honest venue. (Mirrors manifest_read_test.go's tail.)
var _ = proposalsReadStore{s: db.NewTriageStore(nil)}
