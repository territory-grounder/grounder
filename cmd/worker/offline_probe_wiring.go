package main

import (
	"context"
	"log"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/skillstore"
	"github.com/territory-grounder/grounder/temporal/runner"
	"github.com/territory-grounder/grounder/temporal/skillgen"
)

// offlineAdmission is the offline admission gate the generator uses: the single-shot / replay-probe runner that also
// serves as the composer's Prober (skillstore derives it from the Runner — one value, no extra wiring line).
func offlineAdmission(gw agent.Completer, store skillstore.Store, incidents skillgen.IncidentReader, cfg skillgen.OfflineConfig, holder *estate.Holder, skillRows func(context.Context) ([]skillstore.ProductionRow, error), pool *db.Pool) skillgen.OfflineRunner {
	return skillgen.OfflineRunner{Model: gw, Store: store, Incidents: incidents, Cfg: cfg, Probe: offlineProbeConfig(gw, holder, skillRows, pool)}
}

// offlineProbeConfig arms the offline admission gate's replay probe (TG-583 slice B): the REAL investigate loop
// re-run against each discovery incident's recorded trajectory, k replicates per body, with the production
// gateway, the live estate graph and the store's production rows. TG_SKILL_PROBE_REPLICATES is k; the default is
// 0 — SHIPPED DARK: a probe runs 2 × k full loops per discovery incident on the agent's tier inside the gate's
// 8-minute deadline and shares the per-tier model breaker with live triage, so it is armed per deployment after a
// live measurement (`go run ./tools/replayprobe` against one recording prints the loop's wall time), not by
// default. The boot line says which; with k=0 the gate judges single-shot deliverables as before.
func offlineProbeConfig(gw agent.Completer, holder *estate.Holder, skillRows func(context.Context) ([]skillstore.ProductionRow, error), pool *db.Pool) *skillgen.ProbeConfig {
	k := envInt("TG_SKILL_PROBE_REPLICATES", 0)
	if k <= 0 {
		log.Printf("skillgen: replay PROBE OFF (TG_SKILL_PROBE_REPLICATES=%d; set 1+ after a live `tools/replayprobe` measurement) — offline admission judges single-shot deliverables", k)
		return nil
	}
	log.Printf("skillgen: replay PROBE ON (TG-583) — offline admission re-runs the REAL loop against each discovery incident's recording, %d replicate(s) per body; incidents without a recording stay single-shot", k)
	return &skillgen.ProbeConfig{
		Replicates: k,
		Deps:       runner.ProbeDeps{Model: gw, Limits: agent.DefaultLimits(), Graph: holder.Graph, SkillRows: skillRows},
		Recordings: skillgen.StepRecordings{Steps: db.NewAgentStepStore(pool), Evidence: db.NewAgentStepEvidenceStore(pool)},
	}
}
