package agent

import "github.com/territory-grounder/grounder/adapters/model"

// DURABLE PER-TURN CHECKPOINTING (TG-47). The investigation loop (Run) runs inside ONE Temporal activity, so a
// worker crash mid-investigation re-runs the whole loop from cycle 1 — every model call and read-only tool call
// redone. This is masked by bounded triage today but load-bearing for DeepInvestigation and Phase-2, where a
// long investigation losing all progress to a transient worker restart is expensive.
//
// The mechanism is a CYCLE-BOUNDARY checkpoint: at the top of every cycle, before the model call, the loop's
// evolving state is snapshotted; on a resumed run that snapshot is restored and the loop continues from the
// saved cycle. It is SOUND precisely because the investigation loop is READ-ONLY — re-running the cycle the
// crash interrupted re-issues only estate READS (actuation is a separate, later, non-looping step), so there is
// no mutation to double-execute; at worst a resumed cycle repeats an idempotent read.
//
// OFF BY DEFAULT: an Agent with a nil Checkpoint hooks pointer behaves byte-identically to before this existed —
// no snapshot, no restore. The Temporal activity wires the hooks only when TG_INVESTIGATE_DURABLE_CHECKPOINT is
// armed, so the merge is dormant and arming is a separate operator step.

// Checkpoint is the complete resumable state of the investigation loop at a cycle boundary — everything Run's
// loop reads across iterations, so restoring it and resuming from Cycle reproduces the run the crash interrupted.
// Every field is exported DATA (no funcs/channels), so it round-trips through Temporal's data converter into an
// activity heartbeat. Msgs is already observation-compacted (TG-47 compaction, ≤ ObservationBudgetBytes) before
// each cycle, which also bounds the snapshot size; the caller applies a size guard and falls back to no-resume
// when even the compacted snapshot is too large (see CheckpointHooks.Emit).
type Checkpoint struct {
	Cycle   int              // the cycle the run had reached (resume re-runs FROM here — a clean read-only cycle)
	Msgs    []model.Message  // the full transcript (preamble+seed prefix + observation history), post-compaction
	SeedLen int              // the preamble+seed prefix boundary — never compacted (must survive resume verbatim)
	Res     Result           // the accumulated Result so far (Steps/ToolResults/Thoughts/… for a COMPLETE final Result)
	Traj    []TrajectoryStep // the deterministic stuck-loop record (INV-08) — restored so veto state carries across a resume
	// SessionID is the run's session identity (TG-297): the key the read-lane recon budget (TG-165) and any
	// per-session tool cap are metered on, and the correlation id in logs/spans. Restored so a resumed run
	// stays the SAME logical investigation rather than minting a fresh identity. NOTE: the recon governor's
	// and tool caps' per-session COUNTS are process-local (not carried here), so a cross-process resume still
	// starts those counts fresh — a fail-open, crash-only property (a resumed investigation may re-spend read
	// budget). Acceptable dormant; an ARMING consideration to weigh before flag-ON in production.
	SessionID string

	// The at-most-once nudge flags (REQ-1008 stop-nudge, TG-60 decide-nudge, and the forced-decision marker):
	// restored so a resumed run neither re-fires nor skips a nudge it had/hadn't yet reached.
	StopNudged   bool
	DecideNudged bool
	DecideCycle  bool
}

// CheckpointHooks wires the loop to a durable store. Both fields are optional; a nil *CheckpointHooks on the
// Agent (the default) disables checkpointing entirely (byte-identical to pre-TG-47).
type CheckpointHooks struct {
	// Resume, when non-nil, is restored into the loop before the first iteration; the loop then starts at
	// Resume.Cycle. Nil ⇒ a fresh run from cycle 1.
	Resume *Checkpoint
	// Emit, when non-nil, is called at the TOP of every cycle (before the model call) with that cycle's
	// snapshot. The callback MUST snapshot/serialize immediately and retain no reference to the passed slices
	// (the loop mutates them in place afterwards). Nil ⇒ no checkpoints emitted.
	Emit func(Checkpoint)
}
