package main

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/skillstore"
)

type nopCompleter struct{}

func (nopCompleter) Complete(context.Context, string, string, []model.Message) (string, error) {
	return "", nil
}

// The replay probe ships DARK: with the knob unset or 0 the gate gets no probe; any positive k arms it with the
// production collaborators and the agent_step recordings.
func TestOfflineProbeConfigShipsDarkAndArmsOnDemand(t *testing.T) {
	rows := func(context.Context) ([]skillstore.ProductionRow, error) { return nil, nil }
	t.Setenv("TG_SKILL_PROBE_REPLICATES", "")
	if cfg := offlineProbeConfig(nopCompleter{}, estate.NewHolder(estate.NewGraph()), rows, nil); cfg != nil {
		t.Fatalf("unset ⇒ dark, got %+v", cfg)
	}
	t.Setenv("TG_SKILL_PROBE_REPLICATES", "0")
	if cfg := offlineProbeConfig(nopCompleter{}, estate.NewHolder(estate.NewGraph()), rows, nil); cfg != nil {
		t.Fatal("0 ⇒ dark")
	}
	t.Setenv("TG_SKILL_PROBE_REPLICATES", "2")
	cfg := offlineProbeConfig(nopCompleter{}, estate.NewHolder(estate.NewGraph()), rows, nil)
	if cfg == nil || cfg.Replicates != 2 || cfg.Recordings == nil || cfg.Deps.Model == nil || cfg.Deps.Graph == nil || cfg.Deps.SkillRows == nil || cfg.Run != nil {
		t.Fatalf("2 ⇒ armed with the production collaborators and the default (real) probe: %+v", cfg)
	}
}
