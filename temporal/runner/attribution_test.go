package runner

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/safety"
)

// fakeActorReader is a test actorevidence.Reader returning a fixed evidence set (or an error) for a host.
type fakeActorReader struct {
	domain string
	ev     []attribution.Evidence
	err    error
}

func (f fakeActorReader) Domain() string { return f.domain }
func (f fakeActorReader) ReadOnly() bool { return true }
func (f fakeActorReader) Read(_ context.Context, _ string, _, _ time.Time) ([]attribution.Evidence, error) {
	return f.ev, f.err
}

var _ actorevidence.Reader = fakeActorReader{}

// attributeDeps builds the attribution wiring for a workflow test: the default rules-as-data mapping +
// a config whose PVE self-identity is TG's actuation principal, over the supplied readers.
func attributeDeps(t *testing.T, readers ...actorevidence.Reader) func(*Deps) {
	t.Helper()
	mapping, cfg, err := attribution.ParseConfig(attribution.DefaultConfigDocument())
	if err != nil {
		t.Fatalf("default attribution config must parse: %v", err)
	}
	cfg.SelfActors["pve"] = "root@pam!tg-actuate"
	// The embedded default is generic (no sanctioned principals — those are site-specific and supplied by
	// the deploy-time override). A configured deployment declares its admins; simulate that here so the
	// authorized-stand-down path exercises a sanctioned principal rather than reading it as suspicious.
	cfg.Sanctioned["pve"] = []string{"root@pam"}
	cfg.Window = 30 * time.Minute
	return func(d *Deps) {
		d.ActorReaders = readers
		d.AttributionMapping = mapping
		d.AttributionConfig = cfg
	}
}

// REQ-2303: with NO reader registered the attribute step yields unattributable and the ladder is
// byte-identical to the pre-feature behavior — the incident still flows to a sealed, gated proposal and
// the taxonomy is recorded as unattributable.
func TestRunnerAttributionUnattributableWhenNoReader(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	attributeDeps(t)(&deps) // no readers at all
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-1", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if !res.Proposed || res.ActionID == "" {
		t.Fatalf("an unattributable incident must flow to a proposal exactly as before, got %+v", res)
	}
	if res.Attribution != "unattributable" {
		t.Fatalf("no readers ⇒ unattributable recorded, got %q", res.Attribution)
	}
}

// REQ-2302: reader evidence naming the platform's OWN actuation identity on the target terminates the
// session already-remediated — NO classification, NO gate, NO execute (the redundant re-actuation gap,
// killed by construction).
func TestRunnerAttributionSelfNoopTerminates(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	reader := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam!tg-actuate", ActionKind: "vzstart", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:1", Covered: true},
	}}
	attributeDeps(t, reader)(&deps)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-2", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if res.Outcome != "already-remediated" {
		t.Fatalf("a self-attributed incident must terminate already-remediated, got outcome %q (%+v)", res.Outcome, res)
	}
	if res.Attribution != "attributed-self" {
		t.Fatalf("the self attribution must be recorded, got %q", res.Attribution)
	}
	if res.ActionID != "" || res.Mutated {
		t.Fatalf("a self-noop must NOT seal or execute an action (no redundant re-actuation): %+v", res)
	}
}

// REQ-2301: reader evidence attributing the change to a sanctioned NON-TG principal (an admin) forces
// POLL_PAUSE routed to the approver graph — coordinate with the actor, never undo an intentional change.
func TestRunnerAttributionAuthorizedStandsDown(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	reader := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:3", Covered: true},
	}}
	attributeDeps(t, reader)(&deps)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-4", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if res.Band != safety.BandPollPause.String() {
		t.Fatalf("an admin-attributed change must stand down to POLL_PAUSE (coordinate), got band %q", res.Band)
	}
	if res.Attribution != "attributed-authorized" {
		t.Fatalf("the authorized attribution must be recorded, got %q", res.Attribution)
	}
	if res.Mutated {
		t.Fatal("an admin-attributed change must never be auto-undone (that reverses an intentional change)")
	}
}

// REQ-2309: a currently-valid carve-out (a sanctioned harness fault on an allowlisted pool host) resolves
// authorized-test — the heal ladder proceeds UNCHANGED (the learning regime's purpose) and the attribution
// is recorded honestly, distinguishing a manufactured fault from an organic one.
func TestRunnerAttributionCarveOutHealsUnchanged(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	reader := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "root@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:4", Covered: true},
	}}
	mapping, cfg, err := attribution.ParseConfig(attribution.DefaultConfigDocument())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SelfActors["pve"] = "root@pam!tg-actuate"
	cfg.Window = 30 * time.Minute
	cfg.CarveOuts = []attribution.CarveOut{{ID: "pool", Domain: "pve", Actors: []string{"root@pam"}, Hosts: []string{"web01"},
		ValidFrom: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour)}}
	deps.ActorReaders = []actorevidence.Reader{reader}
	deps.AttributionMapping = mapping
	deps.AttributionConfig = cfg
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-5", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	// authorized-test ⇒ ladder-unchanged: the incident still flows to a sealed proposal (NOT a stand-down).
	if !res.Proposed || res.ActionID == "" {
		t.Fatalf("a carve-out (authorized-test) fault must traverse the ladder unchanged, got %+v", res)
	}
	if res.Band == safety.BandPollPause.String() {
		t.Fatalf("a carve-out must NOT stand down (the learning regime heals pool faults), got band %q", res.Band)
	}
	if res.Attribution != "authorized-test" {
		t.Fatalf("the authorized-test attribution must be recorded honestly, got %q", res.Attribution)
	}
}

// REQ-2307: a reader ERROR is advisory — the domain's evidence is treated as absent, a warning is
// recorded, and the session proceeds exactly as unattributable (never blocked, never suspicious on a dead reader).
func TestRunnerAttributionReaderErrorDegradesToUnattributable(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	reader := fakeActorReader{domain: "pve", err: context.DeadlineExceeded}
	attributeDeps(t, reader)(&deps)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-6", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("a reader error must NOT fail or block the session: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if !res.Proposed || res.Attribution != "unattributable" {
		t.Fatalf("a dead reader must degrade to unattributable + the pre-feature ladder, got %+v", res)
	}
	if res.Band == safety.BandPollPause.String() && res.Attribution != "unattributable" {
		t.Fatalf("a reader error must never produce a restrictive reading, got band %q attribution %q", res.Band, res.Attribution)
	}
}

// REQ-2304: reader evidence positively identifying an UNSANCTIONED actor forces POLL_PAUSE with the
// security_escalation signal — never an auto-heal over a possible intrusion.
func TestRunnerAttributionSuspiciousEscalates(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	reader := fakeActorReader{domain: "pve", ev: []attribution.Evidence{
		{Domain: "pve", Actor: "mallory@pam", ActionKind: "vzstop", Target: "web01", ObservedAt: time.Now().Add(-2 * time.Minute), Ref: "UPID:2", Covered: true},
	}}
	attributeDeps(t, reader)(&deps)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-at-3", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if res.Band != safety.BandPollPause.String() {
		t.Fatalf("an unsanctioned actor must force POLL_PAUSE (security escalation), got band %q", res.Band)
	}
	if res.Attribution != "attributed-suspicious" {
		t.Fatalf("the suspicious attribution must be recorded, got %q", res.Attribution)
	}
	if res.Mutated {
		t.Fatal("a suspicious attribution must never auto-heal (healing masks the intrusion)")
	}
}

// fakeSanctionResolver is a test SanctionResolver returning fixed facts (or an error).
type fakeSanctionResolver struct {
	facts     actorevidence.SanctionFacts
	err       error
	gotGroups []string
}

func (f *fakeSanctionResolver) Dimension() string { return "ldap" }
func (f *fakeSanctionResolver) ReadOnly() bool    { return true }
func (f *fakeSanctionResolver) Resolve(_ context.Context, _ string, _, groups []string) (actorevidence.SanctionFacts, error) {
	f.gotGroups = groups
	return f.facts, f.err
}

// The enrichment fold PROMOTES a confirmed live admin and DEMOTES a disabled one over a per-session COPY of
// Sanctioned, leaving the shared config UNMUTATED (REQ-2316..2318).
func TestEnrichSanctionedPromotesAndDemotes(t *testing.T) {
	base := attribution.Config{
		Sanctioned:       map[string][]string{"journal": {"root", "olddba"}},
		SanctionedGroups: map[string][]string{"journal": {"admins"}},
	}
	res := &fakeSanctionResolver{facts: actorevidence.SanctionFacts{
		Confirmed: []string{"kp"},     // a live admin not on the static list → promote
		Disabled:  []string{"olddba"}, // a statically-sanctioned but disabled account → demote
	}}
	a := &Activities{D: Deps{SanctionResolver: res}}
	ev := []attribution.Evidence{
		{Domain: "journal", Actor: "kp", Target: "web01"},
		{Domain: "journal", Actor: "olddba", Target: "web01"},
	}
	var warnings []string
	got := a.enrichSanctioned(context.Background(), ev, base, &warnings)

	set := map[string]bool{}
	for _, s := range got["journal"] {
		set[s] = true
	}
	if !set["kp"] {
		t.Fatalf("kp must be PROMOTED into the copy, got %+v", got["journal"])
	}
	if set["olddba"] {
		t.Fatalf("olddba must be DEMOTED (removed) from the copy, got %+v", got["journal"])
	}
	if !set["root"] {
		t.Fatalf("an unaffected static entry must remain, got %+v", got["journal"])
	}
	// The SHARED base config must be untouched (no mutation).
	if len(base.Sanctioned["journal"]) != 2 || base.Sanctioned["journal"][0] != "root" || base.Sanctioned["journal"][1] != "olddba" {
		t.Fatalf("the shared base Sanctioned must NOT be mutated, got %+v", base.Sanctioned["journal"])
	}
	// The domain's configured groups must be passed to the resolver.
	if len(res.gotGroups) != 1 || res.gotGroups[0] != "admins" {
		t.Fatalf("the resolver must receive the domain's sanctioned groups, got %+v", res.gotGroups)
	}
}

// A resolver ERROR is advisory (REQ-2319): the fold leaves the static list byte-identical and records a warning.
func TestEnrichSanctionedFailsOpenOnError(t *testing.T) {
	base := attribution.Config{Sanctioned: map[string][]string{"journal": {"root"}}, SanctionedGroups: map[string][]string{"journal": {"admins"}}}
	res := &fakeSanctionResolver{err: context.DeadlineExceeded, facts: actorevidence.SanctionFacts{Confirmed: []string{"kp"}}}
	a := &Activities{D: Deps{SanctionResolver: res}}
	ev := []attribution.Evidence{{Domain: "journal", Actor: "kp", Target: "web01"}}
	var warnings []string
	got := a.enrichSanctioned(context.Background(), ev, base, &warnings)
	// Even though the misbehaving fake returned Confirmed:[kp] ALONGSIDE the error, the fold must apply NO
	// facts on error — the static list is left byte-identical (REQ-2319, robust to resolver misbehavior).
	if len(got["journal"]) != 1 || got["journal"][0] != "root" {
		t.Fatalf("on a resolver error the copy must equal the static list (no promotion applied), got %+v", got["journal"])
	}
	if len(warnings) == 0 {
		t.Fatal("a resolver error must record an advisory warning")
	}
}
