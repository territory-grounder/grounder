package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/correlate"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// correlationHarness drives the REAL RunnerWorkflow with a scripted correlation window and captures what
// the routing decision actually did: the class the workflow adopted, the durable decision row it recorded,
// and — crucially — the model tier the INVESTIGATION really ran on, read off the triage row the workflow
// hands the store rather than off any activity's return value.
type correlationHarness struct {
	res       RunnerResult
	decisions []correlate.Decision
	triage    []judge.TriageRow
}

func runWithWindow(t *testing.T, ref string, sev ingest.Severity, window correlate.Window, windowErr error) correlationHarness {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	h := correlationHarness{}
	deps := testDeps(proposeWeb01)
	deps.CorrelationWindow = func(context.Context, time.Time) (correlate.Window, error) {
		return window, windowErr
	}
	deps.ExecClassRecord = func(_ context.Context, d correlate.Decision) error {
		h.decisions = append(h.decisions, d)
		return nil
	}
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		h.triage = append(h.triage, row)
		return nil
	}
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: ref, Host: "web01", AlertRule: "HostDown", Severity: sev, Site: "dc1",
		ReceivedAt: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	if err := env.GetWorkflowResult(&h.res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if len(h.triage) != 1 {
		t.Fatalf("expected exactly one durable triage row, got %d — the tier assertions would be vacuous", len(h.triage))
	}
	return h
}

func warnObs(ref, source, host string, off time.Duration) correlate.Observation {
	return correlate.Observation{
		ExternalRef: ref, SourceType: source, Host: host, AlertRule: "HostDown", Severity: "warning",
		At: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC).Add(off),
	}
}

// THE WIRED ORACLE FOR TG-169 — the correlation stage must change where a real session is ROUTED, not just
// what a pure function returns.
//
// It drives the whole RunnerWorkflow: the correlation activity reads a scripted window, the workflow adopts
// the class, threads it into InvestigateActivity, and the investigation's model tier lands on the durable
// triage row. Every link has to hold for this to pass, which is the point — the exec class had a history of
// being computed and discarded (TG-210), and a stage that decides correctly into a void is the same defect
// with better prose.
//
// KILLING MUTATION (executed): restore the defect in CorrelateActivity —
// `inputs := execclass.Input{Correlated: severityCorrelated(in.Severity)}` — i.e. route on the single
// upstream severity field again. RED with:
//
//	a multi-host WARNING cascade was routed "STANDARD_AGENT" and investigated on the "fast" tier —
//	this is the shape a real compromise makes (many weak signals, no single critical) and it went to
//	the cheapest reasoning TG has
//
// Restored ⇒ green. The same mutation also reds the control below in the other direction.
func TestAWarningCascadeIsRoutedDeepAndInvestigatedOnTheReasoner(t *testing.T) {
	cascade := correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		warnObs("TG-169-cascade", "librenms", "web01", 0),
		warnObs("peer-1", "librenms", "web02", 90*time.Second),
		warnObs("peer-2", "librenms", "db01", 3*time.Minute),
	}}
	h := runWithWindow(t, "TG-169-cascade", ingest.SeverityWarning, cascade, nil)

	if h.res.ExecClass != string(execclass.DeepInvestigation) {
		t.Fatalf("a multi-host WARNING cascade was routed %q, want %q — this is the shape a real compromise "+
			"makes (many weak signals, no single critical)", h.res.ExecClass, execclass.DeepInvestigation)
	}
	if tier := h.triage[0].ModelTier; tier != "primary" {
		t.Fatalf("a multi-host WARNING cascade was routed %q but investigated on the %q tier — the decided "+
			"class reached the record and not the investigation, which is the class being decorative again",
			h.res.ExecClass, tier)
	}
	// The routing decision must leave an audit trail, WITH the premises (migration 0058).
	if len(h.decisions) != 1 {
		t.Fatalf("expected exactly one durable routing decision, got %d — a routing decision that leaves no "+
			"trail cannot be reviewed", len(h.decisions))
	}
	d := h.decisions[0]
	if d.ExternalRef != "TG-169-cascade" || d.ExecClass != execclass.DeepInvestigation {
		t.Fatalf("recorded decision = %+v, want the deep class bound to this session", d)
	}
	if !d.Inputs.Correlated {
		t.Fatal("the recorded classifier INPUT does not say correlated — the decision cannot be re-derived " +
			"against a future classifier, which is why the inputs are persisted at all")
	}
	if d.Verdict.Reason != correlate.ReasonMultiHost || len(d.Verdict.Hosts) != 3 {
		t.Fatalf("recorded evidence = reason %q over hosts %v, want %q over three distinct hosts",
			d.Verdict.Reason, d.Verdict.Hosts, correlate.ReasonMultiHost)
	}
}

// THE CONTROL, AND THE OTHER HALF OF THE DEFECT. A LONE CRITICAL is one system in trouble. It must stop
// claiming to span multiple systems — 2,434 of 2,995 live admitted alerts are critical, so the old rule had
// 81% of incidents asserting a cascade TG never observed — while STILL keeping the MECH-402 model-tier
// floor, which is a severity question and survives the change on its own branch.
//
// KILLING MUTATION (executed): same as above (route on severity). RED here too, with the record claiming a
// multi-system incident on a window that holds exactly one host.
func TestALoneCriticalNoLongerClaimsToSpanMultipleSystems(t *testing.T) {
	alone := correlate.Window{Span: 10 * time.Minute, Observations: []correlate.Observation{
		{ExternalRef: "TG-169-lone", SourceType: "librenms", Host: "web01", AlertRule: "HostDown",
			Severity: "critical", At: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)},
	}}
	h := runWithWindow(t, "TG-169-lone", ingest.SeverityCritical, alone, nil)

	if h.res.ExecClass == string(execclass.DeepInvestigation) {
		t.Fatal("a LONE critical alert was routed DEEP_INVESTIGATION — the class is reserved for a " +
			"multi-system incident, and 81% of live incidents claiming one is how the record stopped meaning anything")
	}
	if len(h.decisions) != 1 || h.decisions[0].Verdict.Correlated {
		t.Fatalf("the durable record claims a correlated incident over a one-host window: %+v", h.decisions)
	}
	if h.decisions[0].Verdict.Reason != correlate.ReasonIsolated {
		t.Fatalf("reason = %q, want %q", h.decisions[0].Verdict.Reason, correlate.ReasonIsolated)
	}
	// The safety floor is NOT collateral damage of the accuracy fix: a critical incident still reads on the
	// reasoning tier (MECH-402), by its own branch rather than by riding on a mis-set Correlated flag.
	if tier := h.triage[0].ModelTier; tier != "primary" {
		t.Fatalf("a CRITICAL incident investigated on the %q tier — the MECH-402 floor was deleted as a side "+
			"effect of no longer calling every critical correlated", tier)
	}
}

// A DEAD CORRELATION READER MUST BE VISIBLE, AND MUST NOT SILENTLY RE-ROUTE AN ESTATE. When the window
// cannot be read the stage falls back to the PRE-TG-169 severity rule and marks the record degraded — so a
// deployment with no durable pool (or a database blip) routes exactly as it did before this shipped, and a
// reviewer can tell "TG looked and saw one system" from "TG could not look".
//
// KILLING MUTATION (executed): make the error branch return `correlate.Verdict{}` instead of
// `correlate.Unavailable(...)`. RED — the failed read is recorded as an ordinary isolated incident, a
// critical stops taking the thorough path the day the database hiccups, and nothing on the record says why.
func TestAnUnreadableWindowFallsBackAndSaysSo(t *testing.T) {
	h := runWithWindow(t, "TG-169-degraded", ingest.SeverityCritical, correlate.Window{}, errors.New("boom"))

	if h.res.ExecClass != string(execclass.DeepInvestigation) {
		t.Fatalf("with an unreadable window a CRITICAL incident routed %q — the pre-TG-169 fallback did not "+
			"hold, so a database blip silently changes how the whole estate is triaged", h.res.ExecClass)
	}
	if len(h.decisions) != 1 {
		t.Fatalf("expected one durable decision even on a degraded read, got %d", len(h.decisions))
	}
	v := h.decisions[0].Verdict
	if !v.Degraded || v.Reason != correlate.ReasonUnavailable {
		t.Fatalf("degraded verdict recorded as %+v — a dead reader must not be indistinguishable from a quiet estate", v)
	}
}

// NO READER WIRED AT ALL is the same fallback, and it is the shape every no-DB harness and every worker
// without a durable pool runs in. Asserted separately because "nil seam" and "seam returned an error" are
// two different code paths and a fallback that only covers one of them is a fallback that is not there.
func TestNoCorrelationReaderKeepsThePreTG169Routing(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01) // CorrelationWindow and ExecClassRecord both nil
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{
		ExternalRef: "TG-169-nodb", Host: "web01", AlertRule: "HostDown",
		Severity: ingest.SeverityCritical, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if res.ExecClass != string(execclass.DeepInvestigation) {
		t.Fatalf("with NO correlation reader a critical incident routed %q, want the unchanged pre-TG-169 %q "+
			"— a deployment that gained no evidence must not silently lose the thorough path",
			res.ExecClass, execclass.DeepInvestigation)
	}
}

// classFor is the single fallback point for the threaded class, and its fallback must be the LEGACY rule —
// not "whatever zero value happens to mean". An activity dispatched with no class (a pre-TG-169 in-flight
// task, or a harness calling InvestigateActivity directly) must behave exactly as it did before.
func TestClassForFallsBackToTheLegacyEnvelopeRule(t *testing.T) {
	crit := ingest.IncidentEnvelope{Severity: ingest.SeverityCritical}
	if got := classFor(crit, ""); got != execclass.DeepInvestigation {
		t.Fatalf("an unthreaded CRITICAL resolved %q, want the legacy %q", got, execclass.DeepInvestigation)
	}
	if got := classFor(crit, "NOT_A_CLASS"); got != execclass.DeepInvestigation {
		t.Fatalf("a GARBAGE class was adopted (%q) instead of falling back — an unknown class selects no "+
			"skills and satisfies no floor", got)
	}
	// ...and a real threaded class wins over the envelope, or the threading buys nothing.
	if got := classFor(crit, string(execclass.FastAgent)); got != execclass.FastAgent {
		t.Fatalf("the decided class was ignored in favour of the envelope rule: %q", got)
	}
}
