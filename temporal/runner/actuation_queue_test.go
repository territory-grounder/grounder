package runner

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
	tg "github.com/territory-grounder/grounder/temporal"
)

// actuation_queue_test.go — TG-153: the estate-mutating activity must be SCHEDULED onto the actuation plane's
// task queue, and every untrusted-content activity must stay on the triage queue.
//
// Why this is asserted at the QUEUE and not at a call site: the split is only real if the two workloads can
// run in DIFFERENT PROCESSES. Temporal routes work by task queue, so the queue named in ActivityOptions is
// the thing that decides which process — and therefore which credential set — executes a mutation. A guard
// inside the activity would still run inside whichever process polled for it.

// TestActuationActivityIsScheduledOnTheActuationQueue drives the FULL Runner workflow with mutation ON and
// records the task queue every activity was actually scheduled onto.
//
// KILLING MUTATION (executed 2026-08-04): in workflow.go's execCtx, change
// `TaskQueue: tg.TaskQueueActuate` to `TaskQueue: tg.TaskQueueRunner` — i.e. leave the estate-mutating
// activity on the triage queue, which is the pre-TG-153 state and the realistic regression (someone
// "tidies" the field back to the workflow's own queue). This test then FAILS with
//
//	"ExecuteActivity ran on task queue "tg.runner" but must run on "tg.actuate" — it is the only Runner
//	 activity that can reach a credential which mutates the estate, so leaving it on the triage queue means
//	 the process that reads untrusted alert/syslog/host content is the process that executes the mutation.
//	 That is TG-153 unfixed."
//
// restored, green. (Deleting the field OUTRIGHT is not the mutation to record: it orphans the `tg` import
// and the package stops compiling, which is a build error rather than this oracle demonstrating anything.)
func TestActuationActivityIsScheduledOnTheActuationQueue(t *testing.T) {
	investigateThenPropose := []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		`{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-q1","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`,
	}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	gate := safety.NewActuatingChokepoint() // mutation ON (test-only) so the execute branch is really reached
	act := &recordingActuator{}
	sink := &fakeManifestSink{}
	deps := testDeps(investigateThenPropose...)
	deps.Mutation = gate
	deps.Interceptor = withPermissivePolicy(actuate.NewInterceptor(gate, act, audit.NewLedger()))
	deps.Manifests = sink
	deps.ManifestSink = sink
	// A readable, quiet post-state so the execute branch runs to completion exactly as the sibling
	// end-to-end workflow tests drive it. Nothing here concerns the queue; it is the scaffolding that gets a
	// REAL execution to happen, which is what the vacuity floor below insists on.
	deps.PostStateObserve = func(context.Context, string, string) ([]verify.ObservedAlert, bool) { return nil, true }
	// Faulted before the effect, quiet after (TG-166b): an always-quiet reader now REFUSES at the necessity
	// gate ("no longer necessary"), which would stop the execute path and make the vacuity floor below fire.
	deps.ClearObserve = faultedUntilHealed("web01", "NginxDown", act)
	acts := NewActivities(deps)
	registerAll(env, acts)
	env.RegisterActivity(acts.BackfillManifestActivity)
	env.RegisterActivity(acts.ObserveClearedActivity)
	env.RegisterActivity(acts.RecoveredSinceActivity)
	env.RegisterActivity(acts.ReconcileActivity)

	// The observation: the task queue each activity was dispatched onto, keyed by activity type name.
	queues := map[string]string{}
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		queues[info.ActivityType.Name] = info.TaskQueue
	})

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-q1", Host: "web01", AlertRule: "NginxDown", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}

	// VACUITY FLOOR. This oracle filters a recorded map; a run in which nothing was recorded — a workflow that
	// short-circuited before execute, a listener that never fired — would make every assertion below pass over
	// an empty set and prove precisely nothing.
	if len(queues) == 0 {
		t.Fatal("no activity was observed at all — the queue oracle matched nothing and would pass vacuously")
	}
	if act.execs != 1 {
		t.Fatalf("the execute path must actually have been reached (execs=%d); a workflow that stopped at "+
			"propose would let this test pass without ever scheduling the actuation activity", act.execs)
	}

	got, ok := queues["ExecuteActivity"]
	if !ok {
		t.Fatal("ExecuteActivity never ran — the actuation queue assertion has nothing to stand on")
	}
	if got != tg.TaskQueueActuate {
		t.Fatalf("ExecuteActivity ran on task queue %q but must run on %q — it is the only Runner activity that "+
			"can reach a credential which mutates the estate, so leaving it on the triage queue means the "+
			"process that reads untrusted alert/syslog/host content is the process that executes the mutation. "+
			"That is TG-153 unfixed.", got, tg.TaskQueueActuate)
	}

	// The mirror, and the half that protects existing deployments: nothing ELSE moved. In particular the
	// untrusted-content activity must stay where it was, or a split deployment would run the LLM agent inside
	// the process holding the actuation key — the same defect, with the two processes swapped.
	// CorrelateActivity (TG-169) is on this list because it is a triage-plane DATABASE reader: it queries the
	// front-door alert ledger for every incident, and a reader scheduled onto the actuation queue would put a
	// per-incident query inside the process holding the estate-mutating key for no benefit at all.
	untrusted := []string{"InvestigateActivity", "SuppressActivity", "CorrelateActivity", "ClassifyActivity", "GateActivity", "VerifyActivity"}
	checked := 0
	for _, name := range untrusted {
		q, seen := queues[name]
		if !seen {
			continue
		}
		checked++
		if q == tg.TaskQueueActuate {
			t.Fatalf("%s was scheduled onto the ACTUATION queue %q — the actuation worker would then read "+
				"untrusted alert/syslog/host content in the process that holds the estate-mutating key", name, q)
		}
	}
	if checked == 0 {
		t.Fatal("observed none of the triage activities — the 'nothing else moved' half of this oracle is vacuous")
	}
}

// TestActuationActivitiesAreRegisteredAndRouted pins the THREE lists that must agree, because they are the
// same claim written in three places and a disagreement between any two is a silent stall in production:
//
//  1. RegisterActuationActivities — what the ACTUATION worker registers (cmd/worker/main.go).
//  2. RegisterActivities         — the canonical full list the triage worker registers.
//  3. workflow.go's execCtx      — what is actually dispatched to tg.actuate.
//
// If (1) omits an activity that (3) routes there, the actuation worker never registers it and the action
// queues forever on a queue nothing polls — which surfaces to an operator as "TG proposed and nothing
// happened", the least debuggable failure an actuation path has. If (1) contains something (3) does not
// route, the actuation worker advertises a triage activity and can be handed untrusted work.
func TestActuationActivitiesAreRegisteredAndRouted(t *testing.T) {
	reg := &capturingRegistry{names: map[string]bool{}}
	RegisterActuationActivities(reg, &Activities{})
	if len(reg.names) == 0 {
		t.Fatal("RegisterActuationActivities registered NOTHING — the actuation worker would poll tg.actuate " +
			"with an empty registry and every gated action would stall there forever")
	}
	if !reg.names["ExecuteActivity"] {
		t.Fatalf("RegisterActuationActivities must register ExecuteActivity (got %v) — it is the activity the "+
			"workflow dispatches to %s", keysOf(reg.names), tg.TaskQueueActuate)
	}
	// Every actuation-registered activity must also be in the canonical full list, or the `both` plane (the
	// default, one process) would register it twice under different sets and drift.
	full := &capturingRegistry{names: map[string]bool{}}
	RegisterActivities(full, &Activities{})
	for name := range reg.names {
		if !full.names[name] {
			t.Fatalf("%s is registered on the actuation queue but is NOT in the canonical RegisterActivities "+
				"list — the two composition roots would disagree about what exists", name)
		}
	}
	// And the routing itself: workflow.go must name the actuation queue exactly once, at the execute site.
	// A source scan, deliberately narrow, with its own vacuity floor.
	src := readWorkflowCode(t)
	if n := strings.Count(src, "tg.TaskQueueActuate"); n != 1 {
		t.Fatalf("workflow.go references tg.TaskQueueActuate %d time(s); expected exactly 1 (the execute "+
			"ActivityOptions). More than one means another activity was pinned to the mutating worker's "+
			"process; zero means nothing is routed there and the actuation worker is dark.", n)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// readWorkflowCode returns workflow.go with whole-line `//` comments stripped, so the routing scan counts
// CODE and not the prose that explains it (this file's own comments name the constant too).
//
// It fails LOUDLY on an unreadable, empty, or all-comment file rather than returning "" — a source scan over
// an empty string matches nothing and passes everything, which is the vacuity failure this repo's gates are
// explicitly written to refuse.
func readWorkflowCode(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatalf("read workflow.go: %v — the routing scan cannot run, and a scan that cannot run must fail, not pass", err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	out := strings.Join(code, "\n")
	if strings.TrimSpace(out) == "" {
		t.Fatal("workflow.go yielded no code lines — the routing scan would match nothing and pass vacuously")
	}
	// The session BODY (runSession — RunnerWorkflow itself is the synthetic-terminal wrapper in
	// terminal.go since TG-81 b1) is what holds the execute routing this scan counts.
	if !strings.Contains(out, "func runSession(") {
		t.Fatal("workflow.go no longer contains runSession — the routing scan is looking at the wrong file " +
			"and would report whatever it happened to find")
	}
	return out
}
