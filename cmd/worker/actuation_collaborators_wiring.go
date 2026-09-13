package main

// wireActuationCollaborators builds the four DURABLE collaborators the actuation chain shares, carved out
// of main()'s composition root (the TG-501 ratchet: wiring belongs in a wire*() file, not the god-file).
//
// AND IT SAYS SO AT BOOT, which is the half that was missing. Each of these is a CONTROL, and two of them
// (the pre-state recorder, TG-58; the durable target admission, TG-81 b2) shipped with no boot evidence at
// all: the log carried six `actuation:` lines and not one of them said whether either was armed. An
// operator — or the next session reading the log to answer "is this wired?" — could not tell a live
// control from a dark one, which is the exact reading failure that lets a control sit unwired for weeks.
// One line, naming all four, is enough to make the answer legible.
//
// All four are SHARED with every builder-produced per-lane interceptor (see the builder in main.go): a
// per-chain instance would recreate the per-process blindness the durable rows exist to remove.

import (
	"log"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/db"
	tracepkg "github.com/territory-grounder/grounder/core/trace"
)

func wireActuationCollaborators(pool *db.Pool) (
	actuate.ExecutionSink, tracepkg.GateVerdictSink, actuate.PreStateSink, actuate.TargetAdmission,
) {
	executions := db.NewActionExecutionStore(pool)
	gateVerdicts := db.NewGateVerdictStore(pool)
	preStates := db.NewActionPreStateStore(pool)
	admission := db.NewActuationTargetStore(pool)
	log.Print("actuation collaborators: ARMED — per-execution record (action_execution), per-gate verdict trail " +
		"(interceptor_gate_verdict), pre-mutation state capture (action_prestate, TG-58) and durable cross-process " +
		"target admission + cooldown (actuation_target_state, TG-81 b2); all four SHARED with every routed lane, " +
		"and all four inert until the mode chokepoint permits a mutation")
	return executions, gateVerdicts, preStates, admission
}
