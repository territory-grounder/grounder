package runner

// TG-701: an action on a different host than the incident's subject is never autonomous. Live 2026-11-08: a service
// alert on one guest, and start-guest on its sibling that a person had deliberately stopped.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
)

// KILLING MUTATIONS in actionOnOtherHost: compare raw target strings instead of resource hosts — RED on "node +
// params.guest" and "FQDN"; always return false — RED on "the live case".
func TestActionOnOtherHostComparesResourcesNotSpellings(t *testing.T) {
	cases := []struct {
		name, subject, target, opClass string
		params                         map[string]string
		want                           bool
	}{
		{"the live case: alert on one guest, start-guest on its sibling", "app01", "app02", "start-guest", nil, true},
		{"a service action on a sibling", "web01", "web02", "restart-service", map[string]string{"unit": "nginx"}, true},
		{"the guest itself", "web01", "web01", "start-guest", nil, false},
		{"the guest spelled as node + params.guest", "web01", "pve03", "start-guest", map[string]string{"guest": "web01"}, false},
		{"another guest behind the same node spelling", "web01", "pve03", "start-guest", map[string]string{"guest": "web02"}, true},
		{"a node named with no guest param", "web01", "pve03", "start-guest", nil, true},
		{"an FQDN of the subject", "web01", "WEB01.example.net", "restart-service", map[string]string{"unit": "nginx"}, false},
		{"a unit on the subject", "web01", "web01", "restart-service", map[string]string{"unit": "nginx"}, false},
		{"a container on the subject", "app01", "app01", "start-container", map[string]string{"container": "mealie"}, false},
		{"an unknown class on another host", "web01", "db01", "no-such-class", nil, true},
		{"no subject abstains", "", "web02", "start-guest", nil, false},
		{"no target abstains", "web01", "", "start-guest", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := actionOnOtherHost(tc.subject, tc.target, tc.opClass, tc.params)
			if got != tc.want {
				t.Fatalf("got %v (%s), want %v", got, detail, tc.want)
			}
			if got && !strings.Contains(detail, "incident subject") {
				t.Fatalf("the detail must name both hosts, got %q", detail)
			}
		})
	}
}

// Through the real classifier and the fallback approver. Control: without the cross-host fact the same graduated
// action is autonomous. KILLING MUTATION: drop the classifier branch — RED here.
func TestACrossHostActionIsHeldHoweverGraduatedItsClassIs(t *testing.T) {
	gi := risk.GatedInput{OpClass: "start-guest", Reversible: risk.Reversible, HasPrediction: true}
	if d := risk.Classify(gi); d.Band == safety.BandPollPause {
		t.Fatalf("vacuity control: a graduated, predicted, reversible start-guest is autonomous, got %s %v", d.Band, d.Signals)
	}
	gi.ActionOnOtherHost, gi.ActionOnOtherHostDetail = true, "incident subject a, action resource host b"
	d := risk.Classify(gi)
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "action-targets-a-different-host" {
		t.Fatalf("a cross-host action must poll on its own reason, got %s %v", d.Band, d.Signals)
	}
	if !strings.Contains(d.Signals["action_other_host"], "incident subject") {
		t.Fatalf("the detail must reach the audit signals, got %v", d.Signals)
	}
	if r := resolvePollAutonomously(gi, d); !r.Resolved || r.Approve {
		t.Fatalf("the fallback approver must HOLD a cross-host action, got %+v", r)
	}
}

// ClassifyActivity feeds the fact from the proposal's own fields. KILLING MUTATION: drop ActionOnOtherHost from the
// GatedInput literal — RED here; drop ActionParams from the helper call — RED on the node-spelled case.
func TestClassifyActivityHoldsTheCrossHostProposal(t *testing.T) {
	a := NewActivities(Deps{Ledger: audit.NewLedger()})
	base := ClassifyInput{ExternalRef: "r", ActionID: "a", PlanHash: "p", RiskLevel: "low", AlertRule: "Service-up/down", OpClass: "start-guest",
		Op: "start", Reversible: true}
	cross := base
	cross.Host, cross.IncidentHost = "app02", "app01"
	d, err := a.ClassifyActivity(context.Background(), cross)
	if err != nil {
		t.Fatal(err)
	}
	if d.Band != safety.BandPollPause || d.Signals["poll_reason"] != "action-targets-a-different-host" {
		t.Fatalf("the live case must poll on the cross-host rule, got %s %v", d.Band, d.Signals)
	}
	same := base
	same.Host, same.IncidentHost, same.ActionParams = "pve03", "app01", map[string]string{"guest": "app01"}
	d, err = a.ClassifyActivity(context.Background(), same)
	if err != nil {
		t.Fatal(err)
	}
	if d.Signals["poll_reason"] == "action-targets-a-different-host" {
		t.Fatalf("the subject spelled as node + params.guest is the same host, got %v", d.Signals)
	}
}

// Through the REAL RunnerWorkflow: an alert on web01 and a proposal on web02 is held and never executes. The same
// guest sealed as node + params.guest stays allowed: TestRunnerRefusesNoOpWhenTargetRecoversAfterSeal/pve03.
func TestTheWorkflowHoldsACrossHostProposal(t *testing.T) {
	const crossHost = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-559","target":"web02","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.9,"evidence_ids":["tr-1"]}}`
	o := fallbackApproverRun(t, true, crossHost)
	if o.execs != 0 || !fallbackLedgerHas(o.ledger, "fallback:autonomous-hold:action-targets-a-different-host") {
		t.Fatalf("a proposal on another host must be held and never execute, execs=%d ledger=%+v", o.execs, o.ledger)
	}
}

// Every ClassifyInput that names an incident subject must also carry the action's params, or a guest sealed as node +
// params.guest reads as the node: a different host. The production path, the probe the eval measures with, and the
// transaction plan all build this literal separately, and the review of this fix found two of three had missed it.
// KILLING MUTATION: remove ActionParams from any one literal in the package — RED here, naming the file.
func TestEveryClassifyInputWithASubjectCarriesTheActionParams(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "ClassifyInput" {
					return true
				}
				keys := map[string]bool{}
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok {
							keys[k.Name] = true
						}
					}
				}
				if keys["IncidentHost"] {
					found++
					if !keys["ActionParams"] {
						t.Errorf("%s: a ClassifyInput with IncidentHost but no ActionParams reads a node-spelled guest as a different host", fset.Position(lit.Pos()))
					}
				}
				return true
			})
		}
	}
	if found < 3 {
		t.Fatalf("expected the workflow, probe and plan literals, found %d: the check has stopped seeing them", found)
	}
}

func TestTheCrossHostHoldTurnsOnTheTargetAlone(t *testing.T) {
	const onTheSubject = `{"action":"propose","confidence":0.9,"proposal":{"external_ref":"TG-701-pair","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.9,"evidence_ids":["tr-1"]}}`
	onASibling := strings.Replace(onTheSubject, `"target":"web01"`, `"target":"web02"`, 1)
	allowed := fallbackApproverRun(t, true, onTheSubject)
	if allowed.execs != 1 || !fallbackLedgerHas(allowed.ledger, "fallback:autonomous-approve:") || len(allowed.gateLedger) == 0 {
		t.Fatalf("allowed twin: the same action on the incident's own host must be approved, clear the chain and execute once, execs=%d gates=%d ledger=%+v",
			allowed.execs, len(allowed.gateLedger), allowed.ledger)
	}
	held := fallbackApproverRun(t, true, onASibling)
	if !fallbackLedgerHas(held.ledger, "fallback:autonomous-hold:action-targets-a-different-host") {
		t.Fatalf("the sibling twin must be held for the cross-host reason, ledger=%+v", held.ledger)
	}
	if held.execs != 0 || len(held.gateLedger) != 0 {
		t.Fatalf("the hold must act before the chain, so no gate can be what stopped it: execs=%d gates=%+v", held.execs, held.gateLedger)
	}
}
