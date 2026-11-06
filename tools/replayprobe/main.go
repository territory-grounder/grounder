// Command replayprobe runs the offline gate's replay probe ONCE, by hand, against a live session's recording — the
// measurement an operator takes before arming TG_SKILL_PROBE_REPLICATES (TG-583 slice B).
//
//	LITELLM_MASTER_KEY=... replayprobe -gateway http://127.0.0.1:4000 -dsn postgres://... -ref <external_ref> [-k 1] [-skill triage-protocol -body-file candidate.md]
//
// It loads the store's production rows and the session's recorded trajectory, builds the replay tool-set per
// replicate, runs the REAL investigate + classify stages k times (with -body-file swapped into -skill when given)
// and prints each deliverable, the replicates' divergence and the WALL TIME — the number the arming decision needs:
// a six-incident gate runs 2 × k of these per incident inside an 8-minute deadline, three incidents at a time.
//
// Read-only against the database; the probe never actuates and persists nothing. Exit 1 on any failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/skillgen"
)

func main() {
	gwURL := flag.String("gateway", os.Getenv("TG_EVAL_GATEWAY"), "model gateway base url (LITELLM_MASTER_KEY in the environment)")
	dsn := flag.String("dsn", "", "session_triage / agent_step / skill_version DSN (a read-only role is enough)")
	ref := flag.String("ref", "", "the recorded session (external_ref) to probe")
	k := flag.Int("k", 1, "replicates")
	skill := flag.String("skill", "", "skill whose body -body-file replaces (optional; without it the production rows run as they are)")
	bodyFile := flag.String("body-file", "", "the candidate body for -skill")
	timeout := flag.Duration("timeout", 8*time.Minute, "overall deadline (the gate's OfflineDeadline)")
	flag.Parse()
	if *gwURL == "" || *dsn == "" || *ref == "" {
		fmt.Fprintln(os.Stderr, "usage: replayprobe -gateway URL -dsn DSN -ref EXTERNAL_REF [-k N] [-skill NAME -body-file FILE]")
		os.Exit(2)
	}
	if (*skill == "") != (*bodyFile == "") {
		fmt.Fprintln(os.Stderr, "replayprobe: -skill and -body-file go together")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	pool, err := db.Connect(ctx, *dsn)
	if err != nil {
		fail("connect: %v", err)
	}
	defer pool.Close()
	row, ok, err := db.NewTriageStore(pool).Row(ctx, *ref)
	if err != nil || !ok {
		fail("session %s: %v (found=%v)", *ref, err, ok)
	}
	rec, err := skillgen.StepRecordings{Steps: db.NewAgentStepStore(pool), Evidence: db.NewAgentStepEvidenceStore(pool)}.Load(ctx, *ref)
	if err != nil {
		fail("recording: %v", err)
	}
	if len(rec.Calls) == 0 {
		fail("session %s has NO recorded observations — nothing to replay", *ref)
	}
	var override func([]skillstore.ProductionRow) []skillstore.ProductionRow
	if *skill != "" {
		body, err := os.ReadFile(*bodyFile)
		if err != nil {
			fail("body file: %v", err)
		}
		override = runner.WithBody(*skill, string(body))
	}
	gw := model.NewGateway(*gwURL, config.SecretRef("env:LITELLM_MASTER_KEY"))
	g := estate.NewGraph() // the probe's graph is NOT the session's; stated in the output
	einc := eval.Incident{ExternalRef: row.ExternalRef, SourceID: "replayprobe", AlertRule: row.AlertRule, Host: row.Host, Severity: "warning"}
	pinc := runner.ProbeIncident{ExternalRef: row.ExternalRef, SourceID: "replayprobe", AlertRule: row.AlertRule, Host: row.Host, Severity: ingest.SeverityWarning}
	deps := runner.ProbeDeps{Model: gw, Limits: agent.DefaultLimits(), Graph: func() *estate.Graph { return g }, SkillRows: db.NewSkillStore(pool).ProductionRows}
	start := time.Now()
	reps, err := runner.ReplayProbe(ctx, deps, func() *agent.ToolSet { return eval.NewReplayToolSet(rec, einc, g) }, pinc, override, *k)
	elapsed := time.Since(start)
	if err != nil {
		fail("probe (%s elapsed, %d deliverable(s) before the failure): %v", elapsed.Round(time.Second), len(reps), err)
	}
	fmt.Printf("replayprobe: %s — %d recorded observation(s), %d replicate(s), %s wall time (empty estate graph; framing WARNING, no summary)\n", *ref, len(rec.Calls), len(reps), elapsed.Round(time.Second))
	fmt.Printf("  original session: band=%q outcome=%q proposed=%v op=%q\n", row.Band, row.Outcome, row.Proposed, row.Op)
	for i, d := range reps {
		fmt.Printf("  replicate %d: band=%q outcome=%q proposed=%v op=%q steps=%d tools=%s evidence=%d\n", i+1, d.Band, d.Outcome, d.Proposed, d.Op, d.Steps, strings.Join(d.Tools, ">"), len(d.Evidence))
		if d.Prediction != "" {
			fmt.Printf("    prediction: %s\n", d.Prediction)
		}
		if d.Conclusion != "" {
			fmt.Printf("    conclusion: %s\n", d.Conclusion)
		}
	}
	fmt.Printf("  divergence: %s\n", runner.Diverge(reps).String())
	fmt.Printf("  arming estimate: a 6-incident gate ≈ 2 bodies × %d replicate(s) × 6 ÷ 3 concurrent ≈ %s of wall time per candidate (deadline 8m)\n", *k, (elapsed * time.Duration(2*(*k)*6/3)).Round(time.Second))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "replayprobe: "+format+"\n", args...)
	os.Exit(1)
}
