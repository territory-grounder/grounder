package main

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/safety"
)

// TG-112. Both worker processes published the literal "worker" to runtime_posture, which is keyed on
// `component` with ON CONFLICT DO UPDATE — so the two planes shared ONE row and whichever heartbeated
// last won. Measured on the live database 2026-08-06: exactly one row, `component=worker`,
// `effect_capability=actuation.local.readonly`.
//
// The grounder reads that single key and reports it on /v1/whoami and /v1/governance, so the ACTUATION
// plane — the only plane that can mutate the estate — was unrepresentable. It had not yet produced a
// wrong answer only because both planes currently publish identical values; it becomes wrong the moment
// they diverge, which is when mutation is switched on.
//
// The metrics half was already fixed (tg_may_actuate carries a `plane` label). The table was left behind.

// planePosture is a reader over a fixed set of component rows.
type planePosture map[string]db.PostureRow

func (p planePosture) Latest(_ context.Context, component string) (db.PostureRow, error) {
	if row, ok := p[component]; ok {
		return row, nil
	}
	return db.PostureRow{Found: false}, nil
}

func freshRow(component, capability string, mayActuate bool) db.PostureRow {
	mode := "Shadow"
	if mayActuate {
		mode = "Semi-auto"
	}
	return db.PostureRow{
		Component: component, Mode: mode, MayActuate: mayActuate, EffectCapability: capability,
		UpdatedAt: time.Now(), Found: true,
	}
}

// TestTheActuationPlanesPostureWins is the defect. With both rows present and DIFFERENT — the state that
// arrives the moment mutation is switched on — the grounder must report the plane that can actuate.
func TestTheActuationPlanesPostureWins(t *testing.T) {
	reader := planePosture{
		"worker-triage":    freshRow("worker-triage", "actuation.local.readonly", false),
		"worker-actuation": freshRow("worker-actuation", "actuation.ssh.native", true),
	}
	gate := safety.NewReadOnlyChokepoint()

	v := resolvePosture(context.Background(), reader, gate, time.Hour)

	if v.EffectCapability != "actuation.ssh.native" {
		t.Errorf("reported effect_capability %q — the grounder is answering with the TRIAGE plane's row. "+
			"This value answers \"can this system mutate the estate\", and the triage plane holds no "+
			"actuation credential and registers no effect leaf, so its answer is not the one to publish.",
			v.EffectCapability)
	}
	if !v.MayActuate {
		t.Error("reported may-actuate=false while the ACTUATION plane published true — the console would " +
			"show the estate as un-mutatable while the plane that mutates it says otherwise")
	}
	if v.Mode != "Semi-auto" {
		t.Errorf("reported mode %q — the mode must ride the same actuation-plane row (TG-112)", v.Mode)
	}
	if v.Stale {
		t.Error("a fresh actuation row was reported as stale")
	}
}

// TestASingleProcessDeploymentStillWorks — plane=both publishes the bare "worker" key, and that
// deployment must be byte-identical to before this change.
func TestASingleProcessDeploymentStillWorks(t *testing.T) {
	reader := planePosture{"worker": freshRow("worker", "actuation.local.readonly", true)}

	v := resolvePosture(context.Background(), reader, safety.NewReadOnlyChokepoint(), time.Hour)

	if v.Source == "grounder-gate" {
		t.Fatal("a plane=both deployment publishing the legacy \"worker\" key resolved to nothing")
	}
	if v.Source != "worker" || v.Stale {
		t.Errorf("source=%q stale=%v — a fresh legacy row must still be authoritative", v.Source, v.Stale)
	}
	if v.EffectCapability != "actuation.local.readonly" {
		t.Errorf("effect_capability=%q, want the legacy row's value", v.EffectCapability)
	}
}

// TestAMidRolloutDeploymentDoesNotDropToUnknown. Old worker (writes "worker"), new grounder (looks for
// "worker-actuation" first). Without the fallback this reports unknown for the whole rollout window — a
// regression dressed as caution.
func TestAMidRolloutDeploymentDoesNotDropToUnknown(t *testing.T) {
	reader := planePosture{"worker": freshRow("worker", "actuation.local.readonly", true)}

	v := resolvePosture(context.Background(), reader, safety.NewReadOnlyChokepoint(), time.Hour)

	if v.Source == "grounder-gate" {
		t.Error("fell back to the grounder's own gate while a fresh legacy worker row existed — the " +
			"console would read \"unknown\" for the entire rollout window")
	}
}

// TestNoRowIsStillFailSafe pins the property this resolver was built for: absence must never become a
// confident answer. Adding a lookup chain must not weaken it.
func TestNoRowIsStillFailSafe(t *testing.T) {
	v := resolvePosture(context.Background(), planePosture{}, safety.NewReadOnlyChokepoint(), time.Hour)

	if v.Source != "grounder-gate" || !v.Stale {
		t.Errorf("no posture row resolved to source=%q stale=%v — with nothing published the answer must "+
			"be the flagged-stale grounder-gate fallback, never a confident reading", v.Source, v.Stale)
	}
	if v.MayActuate {
		t.Error("no posture row reported may-actuate=TRUE — absence must never advertise the estate as " +
			"mutatable")
	}
	if v.Mode != "" {
		t.Errorf("no posture row invented mode %q — absence must read as mode-unknown (\"\"), never a guess", v.Mode)
	}
}

// TestAStaleActuationRowIsNotSilentlyReplacedByAFreshTriageRow. The chain tries actuation first and only
// falls back when it is ABSENT — not when it is old. A stale actuation row is still the right plane's
// answer, and swapping in a fresh triage row would answer the wrong question confidently.
func TestAStaleActuationRowIsNotSilentlyReplacedByAFreshTriageRow(t *testing.T) {
	stale := freshRow("worker-actuation", "actuation.ssh.native", true)
	stale.UpdatedAt = time.Now().Add(-24 * time.Hour)
	reader := planePosture{
		"worker-actuation": stale,
		"worker":           freshRow("worker", "actuation.local.readonly", true),
	}

	v := resolvePosture(context.Background(), reader, safety.NewReadOnlyChokepoint(), time.Minute)

	if v.EffectCapability != "actuation.ssh.native" {
		t.Errorf("effect_capability=%q — a STALE actuation row was replaced by a fresher row from another "+
			"key. Staleness is reported via Source, not repaired by answering a different question.",
			v.EffectCapability)
	}
	if v.Source != "worker-stale" || !v.Stale {
		t.Errorf("source=%q stale=%v — an old actuation row must be reported as stale, not as fresh",
			v.Source, v.Stale)
	}
}
