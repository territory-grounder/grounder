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
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	estatetools "github.com/territory-grounder/grounder/modules/estate"
	"github.com/territory-grounder/grounder/modules/ingest/librenms"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// capturingManifestSink records the sealed action so the harness can report op/opClass/target.
type capturingManifestSink struct{ last *manifest.ActionManifest }

func (c *capturingManifestSink) Seal(_ context.Context, m *manifest.ActionManifest) error {
	c.last = m
	return nil
}

// evalTool is a read-only investigation tool: it hands the agent the incident's device context so it has
// concrete evidence to cite (INV-11). It never touches live infra during the eval (deterministic input).
type evalTool struct{ ctx string }

func (evalTool) Name() string   { return "get-device-context" }
func (evalTool) ReadOnly() bool { return true }
func (t evalTool) Invoke(_ context.Context, _ map[string]string) (agent.ToolResult, error) {
	return agent.ToolResult{ID: "dev-ctx-1", Tool: "get-device-context", Output: t.ctx, Success: true}, nil
}

// evalTools builds the agent's read-only toolset for one incident. It always registers the deterministic
// get-device-context (the alert framing, so the eval still runs offline/CI). When LIBRENMS_TOKEN is set it
// ALSO registers the REAL read-only LibreNMS investigation tools (device status / eventlog / active alerts)
// pointed at live NL — so the agent grounds triage in OBSERVED device state, which is the lift this harness
// measures. TG_LIBRENMS_INSECURE=true accepts the internal self-signed cert; TG_LIBRENMS_URL overrides the base.
func evalTools(inc Incident, g *estate.Graph) *agent.ToolSet {
	tools := agent.NewReadOnlyToolSet()
	_ = tools.Register(evalTool{ctx: fmt.Sprintf("LibreNMS reports %s on %s (severity %s): %s", inc.AlertRule, inc.Host, inc.Severity, inc.Summary)})
	// The estate-context tool over the SAME fixture graph the prediction gate reasons with — the eval
	// exercises the worker's real toolset, cascade discipline included.
	for _, tl := range estatetools.New(func() *estate.Graph { return g }) {
		_ = tools.Register(tl)
	}
	if os.Getenv("LIBRENMS_TOKEN") == "" {
		return tools
	}
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

func runOne(t *testing.T, gw agent.Completer, g *estate.Graph, inc Incident) Session {
	sess := Session{Ref: inc.ExternalRef, AlertRule: inc.AlertRule, Host: inc.Host, Severity: inc.Severity}
	tools := evalTools(inc, g)
	ledger := audit.NewLedger()
	sink := &capturingManifestSink{}
	predStore := predict.NewMemPredictionStore()
	deps := runner.Deps{
		Model:  gw,
		Tools:  tools,
		Limits: agent.DefaultLimits(),
		Gate: &predict.PredictionGate{
			Store: predStore,
			Model: &predict.InfragraphModel{Estate: g, DefaultRules: []string{"HostDown", "HighLatency"}, MaxDepth: 3},
			Mode:  predict.ModeEnforce,
		},
		Ledger:       ledger,
		Mutation:     safety.NewReadOnlyChokepoint(), // OFF
		ManifestSink: sink,
	}
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
	sess.Mutated = res.Mutated
	// committed prediction (grounding signal)
	if res.ActionID != "" {
		if rec, ok, _ := predStore.Get(context.Background(), runner.PlanHash(env.ExternalRef, res.ActionID)); ok {
			sess.Predicted = true
			sess.Prediction = rec.Prediction.Summary()
		}
	}
	if sink.last != nil {
		a := sink.last.Action
		sess.Prediction = strings.TrimSpace(fmt.Sprintf("%s %s on %s (reversible=%v); %s", a.Op, a.OpClass, a.Target, a.Reversible, sess.Prediction))
	}
	for _, e := range ledger.Entries() {
		if e.ActionID == res.ActionID || res.ActionID == "" {
			sess.Decisions = append(sess.Decisions, e.Decision)
		}
	}
	return sess
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
	g := loadEstateGraph(t, "estate_fixture.json")
	gw := model.NewGateway(gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))

	var sessions []Session
	for _, inc := range corpus {
		t.Logf("triage %s: %s @ %s (%s)", inc.ExternalRef, inc.AlertRule, inc.Host, inc.Severity)
		sessions = append(sessions, runOne(t, gw, g, inc))
	}
	// judge each captured session with the gateway
	var scores []Score
	for _, s := range sessions {
		raw, err := gw.Complete(context.Background(), "eval-judge", judge.DefaultParams().Model, []model.Message{{Role: "user", Content: judgePrompt(s)}})
		if err != nil {
			t.Logf("judge %s: %v", s.Ref, err)
			continue
		}
		sc, perr := ParseScore(s.Ref, raw)
		if perr != nil {
			t.Logf("judge parse %s: %v", s.Ref, perr)
			continue
		}
		scores = append(scores, sc)
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
