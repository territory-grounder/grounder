package eval

// This is the ON-BOX integration harness: it runs the corpus through the REAL Runner (mutation OFF) with
// the REAL model gateway + the REAL estate graph, judges each session with the gateway, and writes
// scorecard.json + REPORT.md. It SKIPS unless TG_EVAL_GATEWAY is set, so `make all` (CI, no gateway) is
// unaffected. Run it against the deployed gateway via an SSH tunnel (see run-on-box.sh).

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/actorevidence"
	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/attribution"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/modules/bootstrap"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/modules/observability/hostdiag"
	"github.com/territory-grounder/grounder/modules/observability/syslogng"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// evalActorReader is the covered-but-empty actor-evidence shape (TG-533): one affirmative pve
// CoverageMarker for exactly the incident's host, zero actor entries — the REQ-2304-half-2 state the
// confighash signal exists to discriminate. Mirrors temporal/runner's unit-test reader; a separate small
// type here because a _test.go helper cannot be imported cross-package.
type evalActorReader struct{ host string }

func (r evalActorReader) Domain() string { return "pve" }
func (r evalActorReader) ReadOnly() bool { return true }
func (r evalActorReader) Read(_ context.Context, _ string, _, _ time.Time) ([]attribution.Evidence, error) {
	return []attribution.Evidence{attribution.CoverageMarker("pve", r.host, time.Now())}, nil
}

// attributionDepsFor wires the TG-466 attribution seams for an incident that OPTS IN via
// ConfighashChanged (TG-533). nil opt-in wires NOTHING — ship-dark parity, byte-identical deps for every
// pre-existing incident. Opt-in: the PRODUCTION default ruleset (attribution.DefaultConfigDocument — the
// same document the composition root parses, so a defaults change is visible to this gate rather than
// silently diverged from), the covered-but-empty reader above, and a HOST-SCOPED confighash answer (a
// foreign guest never reads changed — the subject-scoping the unit suite pins).
func attributionDepsFor(t *testing.T, inc Incident) func(*runner.Deps) {
	t.Helper()
	if inc.ConfighashChanged == nil {
		return func(*runner.Deps) {}
	}
	mapping, cfg, err := attribution.ParseConfig(attribution.DefaultConfigDocument())
	if err != nil {
		t.Fatalf("default attribution config must parse: %v", err)
	}
	changed := *inc.ConfighashChanged
	host := inc.Host
	return func(d *runner.Deps) {
		d.ActorReaders = []actorevidence.Reader{evalActorReader{host: host}}
		d.AttributionMapping = mapping
		d.AttributionConfig = cfg
		d.GuestConfigChangedWithin = func(_ context.Context, guest string, _ time.Duration) (bool, error) {
			return changed && guest == host, nil
		}
	}
}

// capturingManifestSink records the sealed action so the harness can report op/opClass/target.
type capturingManifestSink struct{ last *manifest.ActionManifest }

func (c *capturingManifestSink) Seal(_ context.Context, m *manifest.ActionManifest) error {
	c.last = m
	return nil
}

// evalTools builds the agent's read-only toolset for one incident. It always registers the deterministic
// get-device-context (the alert framing, so the eval still runs offline/CI). When LIBRENMS_TOKEN is set it
// ALSO registers the REAL read-only LibreNMS investigation tools (device status / eventlog / active alerts)
// pointed at live NL — so the agent grounds triage in OBSERVED device state, which is the lift this harness
// measures. TG_LIBRENMS_INSECURE=true accepts the internal self-signed cert; TG_LIBRENMS_URL overrides the base.
//
// EXCEPT for a fixture-armed incident (the B4a deterministic arm, fixtures.go): its session is served
// entirely from the captured tool outputs — same tool NAMES as the production-parity live set, zero live
// network calls, no env-gating — so the expected-propose supply stays measurable after the estate heals.
func evalTools(inc Incident, g *estate.Graph) *agent.ToolSet {
	if inc.FixtureArmed() {
		return NewFixtureToolSet(inc, g)
	}
	tools := agent.NewReadOnlyToolSet()
	_ = tools.Register(IncidentContextTool(inc))
	// The estate-context tool over the SAME fixture graph the prediction gate reasons with — the eval
	// exercises the worker's real toolset, cascade discipline included.
	for _, tl := range estatetools.New(func() *estate.Graph { return g }) {
		_ = tools.Register(tl)
	}
	if os.Getenv("LIBRENMS_TOKEN") != "" {
		base := os.Getenv("TG_LIBRENMS_URL")
		if base == "" {
			base = "https://dc1nms01.example.net"
		}
		client := &http.Client{Timeout: 20 * time.Second}
		if v := os.Getenv("TG_LIBRENMS_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // internal self-signed estate endpoint, opt-in
		}
		for _, tl := range librenms.NewTools([]librenms.Deployment{{Site: "nl", BaseURL: base, TokenRef: "env:LIBRENMS_TOKEN"}}, client) {
			if err := tools.Register(tl); err != nil {
				panic("register eval librenms tool: " + err.Error())
			}
		}
	}
	// Toolset PARITY with the worker (Phase B5): the eval used to register a strict subset of production's
	// tools — no hostdiag, no syslog — so on a genuinely-down service the eval agent "could not name the
	// failed unit" and correctly refused a blind restart, while production (with check-host-services) proposed
	// fine. The eval was measuring an agent that does not ship. Same env-gating as cmd/worker/main.go: the
	// tools exist iff the box declares the deployments; identity resolves through the same fail-closed
	// credential path (native hostdiag source; no OpenBao/AWX sources needed for the eval's read-only reads).
	if hdAccess := hostdiag.ParseAccess(os.Getenv("TG_HOSTDIAG_DEPLOYMENTS")); len(hdAccess) > 0 {
		credEngine, _, err := bootstrap.BuildSyncEngine(bootstrap.CredentialConfig{
			NativeRules:         os.Getenv("TG_CREDENTIAL_NATIVE_RULES"),
			HostDiagDeployments: os.Getenv("TG_HOSTDIAG_DEPLOYMENTS"),
		})
		if err != nil {
			panic("eval hostdiag credential engine (fail-closed): " + err.Error())
		}
		for _, tl := range hostdiag.NewTools(hdAccess, nil, credential.NewAuditedResolver(credEngine)) {
			if err := tools.Register(tl); err != nil {
				panic("register eval hostdiag tool: " + err.Error())
			}
		}
	}
	if sgServers := syslogng.ParseServers(os.Getenv("TG_SYSLOGNG_DEPLOYMENTS")); len(sgServers) > 0 {
		for _, tl := range syslogng.NewTools(sgServers, nil) {
			if err := tools.Register(tl); err != nil {
				panic("register eval syslogng tool: " + err.Error())
			}
		}
	}
	return tools
}

func severityOf(s string) ingest.Severity {
	switch strings.ToLower(s) {
	case "critical":
		return ingest.SeverityCritical
	case "warning":
		return ingest.SeverityWarning
	default:
		return ingest.SeverityInfo
	}
}

// loadEstateGraph builds the REAL estate.Graph from the captured snapshot — the SAME graph type the deployed
// worker's prediction gate reasons over (all runs_on + depends_on edges, correct direction). It replaces the
// former flat-adjacency loader, which kept ONLY the 11 depends_on edges (dropping all 372 runs_on placement
// edges where the real blast radius lives) AND inverted their direction (adj[From]=To), starving every
// prediction. estate.Edge treats From as depends-on To and walks edges INTO the target for the blast radius,
// so no manual inversion is needed here.
func loadEstateGraph(t *testing.T, path string) *estate.Graph {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("estate fixture: %v", err)
	}
	var snap struct {
		Nodes []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"nodes"`
		Edges []struct {
			From       string  `json:"from"`
			To         string  `json:"to"`
			Rel        string  `json:"rel"`
			Confidence float64 `json:"confidence"`
			Source     string  `json:"source"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("estate fixture json: %v", err)
	}
	types := make(map[string]estate.EntityType, len(snap.Nodes))
	for _, n := range snap.Nodes {
		// The snapshot flattens a MULTI-SOURCE capture per name, and sources disagree: pve reports
		// dc1pve01/02 as pve_node while netbox lists the same devices as host/vm, and the capture
		// carries BOTH rows. Last-write-wins here silently destroyed the node identity (review
		// 2026-08-25, TG-78 node-plane slice): IsPveNode went false for two of the three node-incident
		// hosts and their corpus rows graded a routing that never ran. Production has no such flattening
		// — every adapter edge carries its own entity types, so ANY pve-sourced edge preserves
		// node-ness — and the loader mirrors that by letting pve_node win a per-name conflict.
		if types[n.Name] == estate.TypePVENode {
			continue
		}
		types[n.Name] = estate.EntityType(n.Type)
	}
	typeOf := func(name string) estate.EntityType {
		if tp, ok := types[name]; ok && tp != "" {
			return tp
		}
		return estate.TypeHost // an endpoint absent from nodes[] (e.g. a bare pve/switch name) is a generic host
	}
	relOf := func(r string) estate.RelType {
		if strings.EqualFold(r, string(estate.RelRunsOn)) {
			return estate.RelRunsOn
		}
		return estate.RelDependsOn
	}
	g := estate.NewGraph()
	for _, e := range snap.Edges {
		if e.From == "" || e.To == "" {
			continue
		}
		g.Upsert(estate.Edge{
			From:       estate.Entity{Type: typeOf(e.From), Name: e.From},
			To:         estate.Entity{Type: typeOf(e.To), Name: e.To},
			Rel:        relOf(e.Rel),
			Confidence: e.Confidence,
			Source:     estate.Source(e.Source),
		})
	}
	return g
}

// evalGuestRunning is the fixture-truth state reader for TG-378's seal-time precondition: THIS
// session's alerting host under a down-class rule is observed not-running (the incident IS the
// ground truth the corpus transcribed); every other target answers could-not-establish (ok=false),
// so the gate's fail-closed default still refuses what the fixture holds no truth about.
func evalGuestRunning(inc Incident) func(context.Context, string) (bool, string, bool) {
	downRule := strings.Contains(strings.ToLower(inc.AlertRule), "down")
	return func(_ context.Context, target string) (bool, string, bool) {
		if target == inc.Host && downRule {
			return false, "eval-fixture: the session's incident declares this host down (corpus ground truth)", true
		}
		return false, "eval-fixture: no ground truth for target " + target, false
	}
}

// evalCommitConfirmRecorder is the harness's in-memory armed-revert store (spec/029 T-029-2 — see
// the Deps comment at the wiring site). Arms and resolutions always succeed: the eval judges
// TRIAGE behavior, and the durable store's failure modes belong to the runner package's drills.
type evalCommitConfirmRecorder struct {
	mu   sync.Mutex
	rows map[string]string // "actionID|externalRef" → state, for post-run sanity only
}

func (r *evalCommitConfirmRecorder) ArmCommitConfirm(_ context.Context, row db.CommitConfirmRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[string]string{}
	}
	r.rows[row.ActionID+"|"+row.ExternalRef] = db.CommitConfirmArmed
	return nil
}

func (r *evalCommitConfirmRecorder) Resolve(_ context.Context, actionID, externalRef, state, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[string]string{}
	}
	r.rows[actionID+"|"+externalRef] = state
	return nil
}

func runOne(t *testing.T, gw agent.Completer, g *estate.Graph, inc Incident) Session {
	sess := Session{Ref: inc.ExternalRef, AlertRule: inc.AlertRule, Host: inc.Host, Severity: inc.Severity,
		Expected: inc.Expected, FixtureArmed: inc.FixtureArmed()}
	tools := evalTools(inc, g)
	ledger := audit.NewLedger()
	sink := &capturingManifestSink{}
	predStore := predict.NewMemPredictionStore()
	deps := runner.Deps{
		Model:  gw,
		Tools:  tools,
		Limits: agent.DefaultLimits(),
		// TG-78 parity: production wires HostIsGuest from the live estate graph so a guest-DOWN incident loads
		// proxmox-triage; the harness wires it from the SAME fixture graph, so the change gate measures the
		// skill on the corpus's real guest-down scenarios (eval-01/02 start-guest, eval-03/12 escalate) instead
		// of leaving proxmox competence unmeasured. Without this the eval would be blind to a prod behavior.
		HostIsGuest: g.IsGuest,
		// TG-78 node-plane parity: production wires HostIsPveNode from the same live graph; the harness
		// wires the fixture graph's, so the tg78-node/cluster/storage incidents measure the node routing
		// (the estate snapshot types dc1pve01/02/03 as pve_node) instead of leaving it eval-blind.
		HostIsPveNode: g.IsPveNode,
		Gate: &predict.PredictionGate{
			Store: predStore,
			Model: &predict.InfragraphModel{Estate: g, DefaultRules: []string{"HostDown", "HighLatency"}, MaxDepth: 3},
			Mode:  predict.ModeEnforce,
			// TG-378 PARITY (found 2026-08-14 after six degraded gate arms): production wires
			// GuestRunning from the guest_liveness projection; this harness wired NOTHING, so any
			// session whose triage proposed a state-preconditioned class (start-guest) refused at
			// seal and ERRORED the arm — and whether an arm degraded depended on whether the model
			// happened to propose it that run, which is why the degradation looked random for a day.
			// No change gate ran between the precondition landing (08-11) and 08-14, so the gap sat
			// invisible. The fixture answers from the corpus's own ground truth; foreign targets
			// stay could-not-establish, preserving the fail-closed default.
			GuestRunning: evalGuestRunning(inc),
		},
		Ledger:       ledger,
		Mutation:     safety.NewReadOnlyChokepoint(), // OFF
		ManifestSink: sink,
		// spec/029 T-029-2 parity: production wires the durable commit_confirm store; a nil store
		// REFUSES the forward for every commit-confirmed-eligible class (restart-service is in the
		// corpus), which would swap "proposed" outcomes for refusals across whole arms — the exact
		// harness-parity failure class the GuestRunning gap above was. In-memory here: the eval
		// measures triage behavior, not Postgres.
		CommitConfirm: &evalCommitConfirmRecorder{},
	}
	// TG-533: the attribution seams wire ONLY when the incident opts in — every other session's deps stay
	// byte-identical to the pre-TG-533 harness (ship-dark parity; the gate must not change what it measures
	// for the rest of the corpus).
	attributionDepsFor(t, inc)(&deps)
	env := ingest.IncidentEnvelope{
		ExternalRef: inc.ExternalRef, SourceID: inc.SourceID, AlertRule: inc.AlertRule,
		Host: inc.Host, Severity: severityOf(inc.Severity), Site: inc.Site,
		// Summary mirrors prod ingest (the LibreNMS normalizer sets it from the alert title). Without it
		// the eval envelope carried no free-text summary, so any change to how the SUMMARY seeds the
		// prompt (delimiting, screening, compaction) rendered against an empty string and the A/B gate
		// was structurally blind to it — found during the R2 input-screen gate run (2026-07-18).
		Summary: inc.Summary,
	}

	var wts testsuite.WorkflowTestSuite
	tenv := wts.NewTestWorkflowEnvironment()
	tenv.SetTestTimeout(3 * time.Minute)
	acts := runner.NewActivities(deps)
	// The canonical registration list — identical to the production worker's, by construction.
	runner.RegisterActivities(tenv, acts)
	// The armed-revert child (spec/029 T-029-2): dispatched by RunnerWorkflow before any eligible
	// effect; the testsuite resolves child workflows only through explicit registration.
	tenv.RegisterWorkflow(runner.CommitConfirmWorkflow)
	tenv.ExecuteWorkflow(runner.RunnerWorkflow, env)

	if !tenv.IsWorkflowCompleted() {
		sess.Err = "workflow did not complete"
		return sess
	}
	if werr := tenv.GetWorkflowError(); werr != nil {
		sess.Err = werr.Error()
		return sess
	}
	var res runner.RunnerResult
	if err := tenv.GetWorkflowResult(&res); err != nil {
		sess.Err = "decode result: " + err.Error()
		return sess
	}
	sess.Band = res.Band
	sess.Proposed = res.Proposed
	sess.Evidence = res.EvidenceIDs // the tool-result ids the proposal (or grounded stop) cited
	sess.Conclusion = res.Conclusion
	sess.ActionID = res.ActionID
	sess.Outcome = res.Outcome
	sess.StepCount = res.StepCount   // A6 decision-efficiency: the loop's investigation-cycle count
	sess.Trajectory = res.Trajectory // TG-525: the digested ordered tool path, for trajectory_grounded
	sess.Mutated = res.Mutated
	// TG-201: the typed claim, as the agent loop bound it against the ids the orchestrator captured. The
	// offline scorecard scores diagnosis_grounded off exactly this — a harness that dropped it would report
	// the axis as N/A for the whole corpus, which is indistinguishable from an agent that never claims
	// anything. Recorded=true unconditionally: this harness ran the session, so the field WAS offered, and an
	// empty claim here is the agent's silence, not the schema's.
	sess.Diagnosis = res.Diagnosis
	sess.DiagnosisRecorded = true
	// TG-533: carry the attribution outcome + the incident's expectation so SecurityCheck can grade the
	// TG-466 escalation mechanically (outside the judged dims).
	sess.Attribution = res.Attribution
	// The REAL security-disposition bit (RunnerResult.SecurityEscalated ← AttributeResult.Security), not a
	// taxonomy+band proxy: a mapping regression that fell attributed-suspicious back to GENERIC escalate
	// would still land POLL_PAUSE and a proxy would pass for the wrong reason (review finding 2026-08-25).
	sess.Security = res.SecurityEscalated
	sess.SecurityExpected = inc.ConfighashChanged
	// committed prediction (grounding signal)
	if res.ActionID != "" {
		if rec, ok, _ := predStore.Get(context.Background(), runner.PlanHash(env.ExternalRef, res.ActionID)); ok {
			sess.Predicted = true
			sess.Prediction = rec.Prediction.Summary()
		}
	}
	if sink.last != nil {
		a := sink.last.Action
		sess.OpClass = a.OpClass // A5 fault-class breadth: the distinct op-class the agent proposed a fix for
		sess.Prediction = strings.TrimSpace(fmt.Sprintf("%s %s on %s (reversible=%v); %s", a.Op, a.OpClass, a.Target, a.Reversible, sess.Prediction))
	}
	for _, e := range ledger.Entries() {
		if e.ActionID == res.ActionID || res.ActionID == "" {
			sess.Decisions = append(sess.Decisions, e.Decision)
		}
	}
	return sess
}

// evalGatewayConcurrency bounds the eval's in-flight model calls (TG-534): TG_MODEL_MAX_CONCURRENCY when set to
// a positive int, else 3 — deliberately below the prod default of 8, because the change gate's many-session
// burst needs a tighter bound than steady prod traffic to stay under the provider's per-minute rate cap instead
// of 429-saturating it. Read identically by both arms, so the bound cancels in the candidate-vs-base delta.
func evalGatewayConcurrency() int {
	if v := os.Getenv("TG_MODEL_MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

func TestEvalCorpusOnBox(t *testing.T) {
	gwURL := os.Getenv("TG_EVAL_GATEWAY")
	if gwURL == "" {
		t.Skip("set TG_EVAL_GATEWAY (e.g. http://localhost:4000 via an SSH tunnel to dc1tg01) + LITELLM_MASTER_KEY to run the on-box eval")
	}
	limit := len(mustCorpus(t))
	if v := os.Getenv("TG_EVAL_LIMIT"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	corpus := mustCorpus(t)
	if limit < len(corpus) {
		corpus = corpus[:limit]
	}
	// Toolset-parity fail-closed (2026-07-30 review): the parity registration below is env-gated, and the
	// only place those vars were set was the worker container — so the "fixed" eval would silently keep
	// measuring the pre-parity toolset (fails OPEN to the old subset). With expected-propose incidents in
	// the corpus, an eval without host-diagnostics cannot name a failed unit, so its recall measures the
	// missing tool, not the agent. Refuse to run rather than measure the wrong system; eval-gate.sh
	// provisions the vars (and the key material they reference) from the box. A FIXTURE-armed propose
	// incident is exempt: its hostdiag reads are served from the capture, so the env gates nothing for it.
	for _, inc := range corpus {
		if inc.Expected == "propose" && !inc.FixtureArmed() && os.Getenv("TG_HOSTDIAG_DEPLOYMENTS") == "" {
			t.Fatalf("toolset parity missing: corpus has expected-propose incidents (%s) but TG_HOSTDIAG_DEPLOYMENTS is empty — "+
				"this arm would measure an agent production does not ship. Run through eval/eval-gate.sh (which provisions the "+
				"hostdiag env + keys from the box), or export the vars, or strip the propose labels if you truly mean to measure toolless triage.", inc.ExternalRef)
		}
	}
	g := loadEstateGraph(t, "estate_fixture.json")
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))
	// Bound the eval's IN-FLIGHT model calls (TG-534). The gate runs Concurrency() sessions at once, each firing
	// investigate/judge calls; an UNBOUNDED gateway let that burst 429-saturate the provider's per-minute cap
	// (measured 1400+ 429s in a single change-gate arm), degrading arms to the "N sessions errored" abort — the
	// exact self-DoS TG-534 exists to remove. The 429 backoff (also TG-534) rides out a TRANSIENT throttle, but
	// only a concurrency BOUND keeps the burst from SUSTAIN-saturating the cap. SetMaxConcurrency serialises the
	// Azure-facing calls under it. It is SYMMETRIC across the candidate and base arms (both read the same env),
	// so it cancels in the delta and is NOT an agent-behaviour change — exactly like the dispatch-width knob.
	gw.SetMaxConcurrency(evalGatewayConcurrency())

	// Corpus-freshness pass (Phase B4, 2026-07-30): this corpus was captured against a past estate; devices
	// get disabled and faults heal. An expected-propose incident whose LIVE evidence contradicts it (device
	// gone/disabled, nothing firing) is marked stale-vs-live: it still runs and is judged — a grounded
	// stand-down on it is CORRECT — but it leaves the recall denominator, because scoring corpus drift as an
	// agent miss is exactly how 0.45→0.00 once read as a capability collapse. Exclusions are logged and
	// published (Scorecard.StaleExcluded), never silent. Fails LOUD on API errors: a freshness check that
	// guesses is worse than none.
	stale := map[string]bool{}
	if os.Getenv("LIBRENMS_TOKEN") != "" {
		base := os.Getenv("TG_LIBRENMS_URL")
		if base == "" {
			base = "https://dc1nms01.example.net"
		}
		freshClient := &http.Client{Timeout: 20 * time.Second}
		if v := os.Getenv("TG_LIBRENMS_INSECURE"); v == "1" || strings.EqualFold(v, "true") {
			freshClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // internal self-signed estate endpoint, opt-in
		}
		deps := []librenms.Deployment{{Site: "nl", BaseURL: base, TokenRef: "env:LIBRENMS_TOKEN"}}
		for _, inc := range corpus {
			// Only LIVE-armed expected-propose incidents are freshness-checked. A fixture-armed incident is
			// stale-proof by construction (the captured world IS its evidence — nothing to drift), which is
			// exactly why the fixture arm exists: the 2026-07-30 trend run stale-excluded ALL propose
			// incidents and left falsifiable_prediction floored with no refresh path.
			if !NeedsFreshnessCheck(inc) {
				continue
			}
			found, disabled, firing, err := librenms.LiveIncidentState(context.Background(), deps, freshClient, inc.Host)
			if err != nil {
				t.Fatalf("corpus freshness for %s: %v (a guessed freshness verdict is worse than none)", inc.ExternalRef, err)
			}
			if !found || disabled || firing == 0 {
				stale[inc.ExternalRef] = true
				t.Logf("freshness: %s (%s on %s) is STALE vs live (found=%v disabled=%v firing=%d) — excluded from recall, stand-down on it is correct",
					inc.ExternalRef, inc.AlertRule, inc.Host, found, disabled, firing)
			}
		}
	}

	// Run sessions + judging through the bounded, order-stable dispatcher (eval/dispatch.go, TG-467) — a
	// controlled on-box burst proved the gateway sustains a small parallel load, so the old sequential loop just
	// wasted wall-clock (~4-5x). Dispatch caps in-flight work at Concurrency() (TG_EVAL_CONCURRENCY, default 6)
	// and writes each result to its INPUT index, so the assembled slice is identical whatever order the gateway
	// answers in. Race-free by construction: each call writes its own index (no shared write), runOne builds all
	// its state per-call, and gw/g are read-only. The per-session error re-run (R1 resilience) stays inside the
	// closure, and a persisted failure rides Session.Err -> sc.Errors -> the integrity gate refuses the arm
	// (TG-64); one blip never aborts the run, so the dispatcher's returned error is deliberately not fatal here.
	conc := Concurrency()
	// TG-204: bracket the SESSION phase in wall-clock. The three-arm model-tier A/B attributes the proxy's
	// served-model / cost / latency telemetry to an arm by time window, and the window must cover the AGENT's
	// calls only — the judging phase below runs on `primary` in EVERY arm (core/judge/rubric.json pins
	// params.model), so a window that included it would stamp the judge's brain onto every arm's served-model
	// signature and load every arm with an identical cost/latency constant, biasing ΔUSD and Δwall-clock
	// toward zero. The obvious alternative — filtering on the `user` field TG sends — does NOT work through
	// today's gateway: LiteLLM drops `user` before an openai/-provider upstream, so the proxy logs caller=""
	// (measured 2026-08-04). The phase boundary is the mechanism that actually holds.
	sessionsStart := time.Now().UTC()
	sessions, _ := Dispatch(corpus, conc, func(_ int, inc Incident) (Session, error) {
		t.Logf("triage %s: %s @ %s (%s)", inc.ExternalRef, inc.AlertRule, inc.Host, inc.Severity)
		var sess Session
		for attempt := 1; attempt <= 3; attempt++ {
			sess = runOne(t, gw, g, inc)
			if sess.Err == "" {
				break
			}
			t.Logf("session %s attempt %d errored: %s", inc.ExternalRef, attempt, sess.Err)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * 3 * time.Second)
			}
		}
		sess.StaleVsLive = stale[inc.ExternalRef]
		return sess, nil // soft errors ride Session.Err; a session never hard-fails the arm (see Dispatch + TG-64)
	})
	sessionsEnd := time.Now().UTC()
	writePhase(t, sessionsStart, sessionsEnd)
	// TG-533: the mechanical TG-466 escalation check — must-fire on the confirmed-observed-mutation
	// incident AND must-NOT-fire on its changed=false control. Outside the judged dims by design (a
	// taxonomy check must not widen a judged denominator); the checked count is logged so a corpus with
	// no opted-in incident reads out-of-scope, never as a vacuous pass.
	if n, unreached, viol := SecurityCheck(sessions); len(viol) > 0 {
		for _, v := range viol {
			t.Errorf("security escalation check FAILED: %s", v)
		}
	} else {
		for _, u := range unreached {
			t.Logf("security escalation check UNREACHED: %s", u)
		}
		t.Logf("security escalation checks: %d checked, %d unreached, 0 violation(s)", n, len(unreached))
	}
	// judge each captured session through the SAME bounded dispatcher (retry each on a transient gateway
	// error). A judge failure returns a nil score AND its error: the nil leaves that slot empty so the session
	// drops out of `scores` and Judged<N makes the integrity gate refuse the arm (TG-64); the returned error is
	// surfaced by Dispatch, never silently swallowed. Stable indexing keeps each slot aligned with its session
	// regardless of which judgments land first.
	scored, _ := Dispatch(sessions, conc, func(_ int, s Session) (*Score, error) {
		raw, err := retryComplete(3, func(attempt int) { time.Sleep(time.Duration(attempt) * 2 * time.Second) }, func() (string, error) {
			return gw.Complete(context.Background(), "eval-judge", judge.DefaultParams().Model, []model.Message{{Role: "user", Content: judgePrompt(s)}})
		})
		if err != nil {
			t.Logf("judge %s (after retries): %v", s.Ref, err)
			return nil, err
		}
		sc, perr := ParseScore(s.Ref, raw)
		if perr != nil {
			t.Logf("judge parse %s: %v", s.Ref, perr)
			return nil, perr
		}
		return &sc, nil
	})
	var scores []Score
	for _, sc := range scored {
		if sc != nil {
			scores = append(scores, *sc)
		}
	}
	// TG-525 slice 2: grade each session's ordered tool PATH with the LLM ordered-path judge, in the SAME
	// bounded dispatcher + model channel as the main judge. A soft failure (gateway flake / unparseable) leaves
	// the grade 0 and trajectoryScore falls back to its deterministic default — it NEVER fails the arm (the
	// axis is report-only). The deterministic agent.TrajectoryVeto overrides this grade in the scorer.
	trajGrades, _ := Dispatch(sessions, conc, func(_ int, s Session) (*trajectoryVerdict, error) {
		if len(s.Trajectory) == 0 {
			return nil, nil
		}
		raw, err := retryComplete(3, func(attempt int) { time.Sleep(time.Duration(attempt) * 2 * time.Second) }, func() (string, error) {
			return gw.Complete(context.Background(), "eval-trajectory-judge", judge.DefaultParams().Model, []model.Message{{Role: "user", Content: trajectoryJudgePrompt(s.Trajectory)}})
		})
		if err != nil {
			t.Logf("trajectory-judge %s (after retries): %v", s.Ref, err)
			return nil, nil // soft: no grade -> the scorer falls back deterministically, arm unaffected
		}
		if v, ok := parseTrajectoryVerdict(raw); ok {
			return &v, nil
		}
		return nil, nil
	})
	for i, v := range trajGrades {
		if v != nil {
			sessions[i].TrajectoryJudgeScore = v.Score
			sessions[i].TrajectoryJudgeReason = v.Comment
		}
	}
	SortSessions(sessions)
	card := Aggregate(sessions, scores)
	if err := os.WriteFile("scorecard.json", ScorecardJSON(card), 0o644); err != nil {
		t.Fatalf("write scorecard: %v", err)
	}
	writeSessions(t, sessions, scores)
	writeReport(t, card, len(sessions), len(scores))
	if card.MutationCount != 0 {
		t.Fatalf("SAFETY: mutation occurred during read-only eval (count=%d) — must be 0", card.MutationCount)
	}
	t.Logf("EVAL DONE: %d sessions, overall %.2f/5, bands %v, proposal-rate %.0f%%, prediction-rate %.0f%%",
		card.N, card.Overall, card.Bands, card.ProposalRate*100, card.PredictionRate*100)
}

// writePhase records the SESSION phase's wall-clock boundaries (UTC, RFC3339) next to the scorecard, so the
// TG-204 three-arm driver can attribute proxy telemetry to the agent's calls and NOT to the judge's. It is
// written before judging starts, so a run that dies in the judge phase still leaves a usable window.
//
// It is deliberately a separate small file rather than a field on the scorecard: the scorecard is a JUDGED
// aggregate consumed by eval/gate and the committed trend baseline, and adding a wall-clock field to it
// would put a machine-varying value into an artifact whose whole job is to be comparable across machines.
func writePhase(t *testing.T, start, end time.Time) {
	t.Helper()
	b, _ := json.MarshalIndent(struct {
		SessionsStart string `json:"sessions_start"`
		SessionsEnd   string `json:"sessions_end"`
		Note          string `json:"note"`
	}{
		// RFC3339Nano, never RFC3339: the latter TRUNCATES to the whole second, which narrows the window's END
		// and preferentially discards the arm's LAST proxy call — its decide-tier call, the one TG-204 is
		// asking about. Measured 2026-08-04: whole-second ends dropped two of three arms' only completions.
		SessionsStart: start.Format(time.RFC3339Nano),
		SessionsEnd:   end.Format(time.RFC3339Nano),
		Note: "the AGENT phase only — judging runs after sessions_end on the `primary` tier in every arm, " +
			"so including it would stamp the judge's model onto each arm's served-model signature (TG-204)",
	}, "", "  ")
	if err := os.WriteFile("phase.json", append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write phase.json: %v (the TG-204 driver needs it to attribute proxy telemetry)", err)
	}
}

func mustCorpus(t *testing.T) []Incident {
	c, err := LoadCorpus("corpus.json")
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	return c
}

func writeSessions(t *testing.T, sessions []Session, scores []Score) {
	byRef := map[string]Score{}
	for _, s := range scores {
		byRef[s.Ref] = s
	}
	type row struct {
		Session Session `json:"session"`
		Score   Score   `json:"score"`
	}
	var rows []row
	for _, s := range sessions {
		rows = append(rows, row{s, byRef[s.Ref]})
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	_ = os.WriteFile("sessions.json", b, 0o644)
}

func writeReport(t *testing.T, card Scorecard, nSessions, nScored int) {
	var b strings.Builder
	b.WriteString("# TG grounding/quality eval — first real run\n\n")
	fmt.Fprintf(&b, "Ran **%d** realistic NL incidents through the REAL Runner (mutation OFF) over the real 359-node estate, on dc1tg01's live model gateway. %d sessions judged.\n\n", nSessions, nScored)
	fmt.Fprintf(&b, "- **Overall quality:** %.2f / 5\n- **Band distribution:** %v\n- **Proposal rate:** %.0f%%\n- **Committed-prediction (falsifiable) rate:** %.0f%%\n- **Mutations:** %d (MUST be 0 — read-only)\n\n", card.Overall, card.Bands, card.ProposalRate*100, card.PredictionRate*100, card.MutationCount)
	b.WriteString("## Per-dimension means (1–5)\n\n")
	for _, d := range Dimensions {
		fmt.Fprintf(&b, "- %s: %.2f\n", d, card.DimMeans[d])
	}
	b.WriteString("\n## vs. the predecessor (claude-gateway)\n\n")
	b.WriteString("The predecessor scores every session with an LLM-as-Judge on 5 dimensions and sits at 12/14 A on the Anthropic/OpenAI agent scorecards. This harness replicates that judging shape AND adds TG's differentiator — a **committed, mechanically falsifiable prediction** per action (the prediction-rate above), which the predecessor's LLM-judge does not measure. The path to 'exceed' is: keep the per-dimension means high AND drive the prediction/match-rate up via the flywheel (next iteration).\n\n")
	b.WriteString("See sessions.json for per-incident detail. Run: `eval/run-on-box.sh`.\n")
	_ = os.WriteFile("REPORT.md", []byte(b.String()), 0o644)
}
