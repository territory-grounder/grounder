package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/estate"
	actorevidencetool "github.com/territory-grounder/grounder/modules/actorevidence/tool"
	"github.com/territory-grounder/grounder/modules/cmdb/netbox"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/incidenthistory"
	"github.com/territory-grounder/grounder/modules/observability/openobserve"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
	"github.com/territory-grounder/grounder/modules/tracker/trackerhistory"
)

// EVERY LIVE TOOL FAMILY MUST PUBLISH ITS ACI SCHEMA (TG-197).
//
// THE DEFECT. agent.ACITool — Description() + Params() — is the contract that lets a model call a tool "from
// its description and parameters alone", and agent.ToolSet.Catalog() renders it into the preamble while
// agent.ValidateArgs screens a call against it. It is OPT-IN: a tool that does not implement it is listed as
// a bare "- get-estate-context" and validates trivially. Four of the seven families registered in main()
// never adopted it, so for ten of the thirteen live tools the model was handed a name and had to GUESS the
// argument keys — inside a 5-cycle poll budget against a 5.4-step live mean, where a guessed key costs a
// cycle the investigation does not have.
//
// WHY IT IS CHECKED FROM main.go. "Which tool families are LIVE" is a composition-root fact, not a package
// fact: a family can publish a perfect schema and never be registered, and — the direction that matters — a
// NEW family can be registered tomorrow with no schema at all and every package test still passes. So the
// live set is read out of main.go by AST and each member must be built and inspected here. A family wired in
// main() that this file does not build FAILS the test rather than being skipped, which is what stops the
// adoption count from silently rotting back down.
//
// VACUITY FLOOR (house rule 3). This test enumerates, so it must fail when the enumeration is empty: zero
// register-loops found, a range expression it cannot resolve to a constructor, or a family whose constructor
// returns no tools all FAIL. Without that, deleting the registrations — or a refactor that renames the loop
// variable — would leave a test that checks an empty set and passes forever.
//
// KILLING MUTATIONS, all four EXECUTED on 2026-08-04 (restored and re-run green after each):
//  1. Reverted the whole adoption (stashed the four modules/ files) — RED, naming all ten previously-bare
//     tools: "get-estate-context (family estatetools.New) publishes NO ACI schema: the preamble lists it by
//     bare name, so the model must GUESS its argument names…", and logging "3 of 13 live tools".
//  2. Made librenms hostParam return nil — RED on all three LibreNMS tools ("declares a description but ZERO
//     parameters"), which is the half-adoption a description-only check would have passed.
//  3. Blanked one ParamSpec.Description — RED ("parameter \"host\" declares no description").
//  4. Pointed the enumeration at a Register method that does not exist, so it found no families — RED on the
//     VACUITY FLOOR ("found NO tool-registration loops in main.go"), not a vacuous pass.

// aciBuiltFamilies constructs EVERY agent tool family with inert test doubles. Nothing is Invoked, so no
// deployment, credential or connection is needed — the constructors only require a non-empty config slice
// (and, for hostdiag, a resolver), which is exactly the "is the tool present" gate main() relies on. A nil
// runner selects the production native SSH runner, which performs no I/O until a read is attempted.
//
// The KEY is the "pkg.Func" call as it is spelled in main.go, so the AST enumeration below can match the
// live set against this one by name.
func aciBuiltFamilies() map[string][]agent.Tool {
	return map[string][]agent.Tool{
		"librenms.NewTools": librenms.NewTools(
			[]librenms.Deployment{{Site: "t", BaseURL: "https://librenms.invalid", TokenRef: "env:TG_TEST_TOKEN"}}, nil),
		"netbox.NewTools": netbox.NewTools(
			netbox.New("https://netbox.invalid", config.SecretRef("env:TG_TEST_NB_TOKEN"))),
		"syslogng.NewTools": syslogng.NewTools(
			[]syslogng.Server{{Site: "t", SSHHost: "dc1syslogng01", SSHUser: "root", BasePath: "/mnt/logs"}}, nil),
		"hostdiag.NewTools": hostdiag.NewTools(
			[]hostdiag.Access{{Site: "t", HostGlob: "*", SSHUser: "root"}}, nil, aciStubResolver{}),
		"estatetools.New": estatetools.New(func() *estate.Graph { return nil }),
		"openobserve.NewCorrelateTools": openobserve.NewCorrelateTools(
			openobserve.NewReader("https://openobserve.invalid/api/default", config.SecretRef("env:TG_TEST_OO_TOKEN")),
			func() *estate.Graph { return nil }),
		"incidenthistory.New": incidenthistory.New(
			func(context.Context, string, int) ([]incidenthistory.PriorIncident, error) { return nil, nil }),
		"trackerhistory.New": trackerhistory.New(
			func(context.Context, string, string, int) ([]trackerhistory.TrackedIncident, error) { return nil, nil }),
		"actorevidencetool.New": actorevidencetool.New(
			func(context.Context, string, time.Time, time.Time) ([]attribution.Evidence, error) { return nil, nil },
			30*time.Minute),
	}
}

func TestEveryLiveAgentToolFamilyPublishesAnACISchema(t *testing.T) {
	live := aciLiveToolFamilies(t)
	built := aciBuiltFamilies()

	total, withSchema := 0, 0
	ts := agent.NewReadOnlyToolSet()
	for _, fam := range live {
		tools, ok := built[fam]
		if !ok {
			// The rot-guard: a family reached the agent's tool set without passing through this control.
			t.Fatalf("main() registers agent tools from %s, but this control does not build that family — add it to "+
				"aciBuiltFamilies so its ACI schema is checked, or the next unadopted family ships invisibly (TG-197)", fam)
		}
		if len(tools) == 0 {
			// VACUITY FLOOR: a constructor that returns nothing would make every assertion below run zero times.
			t.Fatalf("family %s built ZERO tools under this control's test doubles — the checks below would pass "+
				"over an empty set and prove nothing", fam)
		}
		for _, tl := range tools {
			total++
			if err := ts.Register(tl); err != nil {
				t.Fatalf("family %s tool %s must register read-only: %v", fam, tl.Name(), err)
			}
			at, ok := tl.(agent.ACITool)
			if !ok {
				t.Errorf("%s (family %s) publishes NO ACI schema: the preamble lists it by bare name, so the model "+
					"must GUESS its argument names inside a 5-cycle poll budget — and agent.ValidateArgs has nothing "+
					"to screen a malformed call against, so the bad call runs against the estate", tl.Name(), fam)
				continue
			}
			withSchema++
			aciAssertSchemaIsUsable(t, fam, at)
		}
	}
	if total == 0 {
		// VACUITY FLOOR: no tools anywhere ⇒ nothing was asserted.
		t.Fatal("no agent tools were built from any live family — this test asserted nothing")
	}

	// WIRING: the schema is only worth anything if it reaches the preamble the loop actually builds, which is
	// ToolSet.Catalog() (agent/loop.go). Render the real catalog over the real tool set and prove each tool's
	// description AND each declared parameter are in it — an interface satisfied but never rendered is the
	// "it exists in the tree" failure this ticket is about.
	catalog := ts.Catalog()
	if strings.TrimSpace(catalog) == "" {
		t.Fatal("the rendered tool catalog is EMPTY — nothing the schema declares reaches the model")
	}
	for _, name := range ts.Names() {
		tl, _ := ts.Get(name)
		at, ok := tl.(agent.ACITool)
		if !ok {
			continue // already reported above
		}
		if !strings.Contains(catalog, aciHead(at.Description())) {
			t.Errorf("%s: its description is not rendered into the tool catalog — the model never reads it", name)
		}
		for _, p := range at.Params() {
			if !strings.Contains(catalog, "- "+p.Name+" (") {
				t.Errorf("%s: parameter %q is declared but not rendered into the tool catalog — the model still has "+
					"to guess it", name, p.Name)
			}
		}
	}

	// The ticket's own headline number, reported honestly rather than asserted into existence.
	t.Logf("ACI adoption: %d of %d live tool families checked, %d of %d live tools implement agent.ACITool "+
		"(their schemas are asserted usable above)", len(live), len(live), withSchema, total)
}

// aciAssertSchemaIsUsable checks the schema is CALLABLE-FROM, not merely present: a description a model can
// act on, at least one declared parameter, and — for every required one — the type/description/example that
// make a first call land. It also proves the poka-yoke is live, since a Required flag that agent.ValidateArgs
// does not act on is decoration.
func aciAssertSchemaIsUsable(t *testing.T, fam string, at agent.ACITool) {
	t.Helper()
	name := at.Name()
	if d := strings.TrimSpace(at.Description()); len(d) < 40 {
		t.Errorf("%s (family %s): description is missing or a stub (%q) — the model chooses BETWEEN tools from "+
			"this line, and %q does not say what it answers", name, fam, d, d)
	}
	params := at.Params()
	if len(params) == 0 {
		t.Errorf("%s (family %s): declares a description but ZERO parameters — the model still has to guess the "+
			"argument keys, which is the defect TG-197 closes", name, fam)
		return
	}
	var required []agent.ParamSpec
	for _, p := range params {
		if strings.TrimSpace(p.Name) == "" {
			t.Errorf("%s: a parameter has no Name — it renders as an unnamed bullet the model cannot use", name)
			continue
		}
		if strings.TrimSpace(p.Type) == "" {
			t.Errorf("%s: parameter %q declares no Type hint", name, p.Name)
		}
		if strings.TrimSpace(p.Description) == "" {
			t.Errorf("%s: parameter %q declares no description — a bare key is barely better than a guessed one",
				name, p.Name)
		}
		if p.Required {
			if strings.TrimSpace(p.Example) == "" {
				t.Errorf("%s: required parameter %q carries no Example — agent.ValidateArgs then refuses a missing "+
					"arg WITHOUT showing the model a valid value, which costs an extra recovery cycle", name, p.Name)
			}
			required = append(required, p)
		}
	}
	if len(required) == 0 {
		return
	}
	// The poka-yoke, end to end: an empty call must be REFUSED before Invoke, with a message naming the arg.
	err := agent.ValidateArgs(at, map[string]string{})
	if err == nil {
		t.Errorf("%s: declares required parameter(s) but an EMPTY call passes agent.ValidateArgs — the malformed "+
			"call reaches the estate instead of becoming an actionable TOOL_ERROR", name)
		return
	}
	if !strings.Contains(err.Error(), required[0].Name) {
		t.Errorf("%s: the refusal for a missing required arg does not name %q (%v) — an unactionable refusal burns "+
			"a cycle without telling the model what to fix", name, required[0].Name, err)
	}
}

// aciHead is the leading slice of a description used for the catalog-rendering check: enough to be unique,
// short enough to survive a later wording tweak inside the same sentence.
func aciHead(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// aciLiveToolFamilies reads main.go and returns the constructors whose results are registered into the
// agent's tool set, as "pkg.Func" strings. Both shapes main() uses are resolved: a direct
// `for _, tl := range pkg.New(...)` and the deferred `x := pkg.NewTools(...)` … `for _, tl := range x` that
// the syslog/hostdiag lanes need because their wiring manifests sit a thousand lines further down.
func aciLiveToolFamilies(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	// varName -> "pkg.Func", for the deferred shape.
	assigned := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if call := aciCallName(as.Rhs[0]); call != "" {
			assigned[id.Name] = call
		}
		return true
	})

	seen := map[string]bool{}
	var fams []string
	unresolved := 0
	ast.Inspect(f, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok || !aciBodyRegistersATool(rs.Body) {
			return true
		}
		name := aciCallName(rs.X)
		if name == "" {
			if id, ok := rs.X.(*ast.Ident); ok {
				name = assigned[id.Name]
			}
		}
		if name == "" {
			// Never under-count in silence: an unrecognised shape means this enumeration has stopped seeing
			// part of the live set, and a smaller set is a test that checks less while still passing.
			unresolved++
			return true
		}
		if !seen[name] {
			seen[name] = true
			fams = append(fams, name)
		}
		return true
	})
	if unresolved > 0 {
		t.Fatalf("%d tool-registration loop(s) in main.go do not resolve to a constructor — this enumeration has "+
			"gone partially blind, so extend it rather than letting it check a shrinking set", unresolved)
	}
	if len(fams) == 0 {
		// VACUITY FLOOR: the whole test hangs off this list.
		t.Fatal("found NO tool-registration loops in main.go — either the agent has no tools at all, or this " +
			"enumeration no longer matches the composition root; both make every assertion below vacuous")
	}
	sort.Strings(fams)
	return fams
}

// aciCallName renders `pkg.Func(...)` as "pkg.Func"; anything else yields "".
func aciCallName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

// aciBodyRegistersATool reports whether a loop body calls tools.Register or tools.RegisterFrom — the two
// calls that put a tool in front of the model (RegisterFrom is Register plus the TG-215 source-namespace
// label; matching only one of them would blind this enumeration to half the live set).
func aciBodyRegistersATool(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || (sel.Sel.Name != "Register" && sel.Sel.Name != "RegisterFrom") {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "tools" {
			found = true
		}
		return true
	})
	return found
}

// aciStubResolver satisfies hostdiag.IdentityResolver so the family can be CONSTRUCTED. It is never called:
// this control reads schemas, it does not invoke tools, and a resolver that returned a usable bundle would
// invite exactly that.
type aciStubResolver struct{}

var errACIStubResolver = errors.New("aci schema control: the stub resolver never resolves a real credential")

func (aciStubResolver) Resolve(context.Context, credential.Target) (credential.Bundle, error) {
	return credential.Bundle{}, errACIStubResolver
}
