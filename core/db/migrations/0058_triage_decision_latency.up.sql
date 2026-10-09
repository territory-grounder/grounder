-- 0058: persist the agent loop's WALL-CLOCK decision latency on the triage record (TG-205, axis A6b).
--
-- docs/BENCHMARK-AXES.md defined A6 as MTTR — "resolving faster … detection latency, decision latency,
-- actuation path" — while EVERY implementation measured decision STEPS (cmd/axisscore's
-- a6a_mean_decision_steps, eval/gate's MeanDecisionSteps). The frozen axis vocabulary and its
-- implementations had silently drifted apart, so TG can state how MANY cycles a decision took and nothing
-- at all about how LONG it took. TG-205 splits the axis (A6a = steps, the merge gate; A6b = wall-clock,
-- reported) and this column is the half that was never recorded anywhere durable.
--
-- THE NUMBER ALREADY EXISTS IN PROCESS AND IS THROWN AWAY. temporal/runner/activities.go times the agent
-- loop (`loopDur`) and hands it to observe.RecordAgentLoop, whose Emitter is nil unless metrics are wired —
-- and even when wired it lands in ONE CUMULATIVE COUNTER, tg_agent_run_seconds_total: a running sum of every
-- loop's seconds, with no distribution, no per-incident attribution, no way to slice by op-class/tier/outcome,
-- and a reset on every restart. A total is not a measurement of a decision. This is exactly the gap migration
-- 0037 closed for step_count: the Runner computed the value and dropped it at the DB boundary.
--
-- 0 MEANS UNMEASURED, NOT INSTANT. Pre-migration rows, and sessions that never ran the loop (a suppressed
-- incident, an investigation that failed before ag.Run), read back 0 — and the A6b aggregate EXCLUDES them
-- rather than averaging a zero in, the same discipline step_count uses. Flooring them at 0 would publish TG
-- as deciding INSTANTLY on precisely the sessions where it never decided at all, which is the direction
-- that flatters the axis.
--
-- MILLISECONDS, and bigint rather than int: a stuck-but-eventually-returning loop is exactly the tail A6b
-- exists to show, and seconds would quantise away the fast half of a distribution whose median is tens of
-- seconds. bigint cannot overflow at any duration a bounded loop can produce.
--
-- OBSERVABILITY ONLY — decision_ms re-enters no gate (INV-08). session_triage is NOT on the append-only
-- spine (0015), so no grant change is needed. Additive, defaulted, backward-compatible: every existing row
-- stays valid and every pre-field writer keeps working.
ALTER TABLE session_triage ADD COLUMN IF NOT EXISTS decision_ms bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN session_triage.decision_ms IS
  'Wall-clock milliseconds the agent loop spent reaching this session''s TERMINAL decision — proposal or grounded stop (TG-205, benchmark axis A6b). Distinct from step_count (A6a), which counts investigation CYCLES: a run can be few-stepped and slow, or many-stepped and fast, and MTTR is the time question. 0 = UNMEASURED (the session predates this column or never ran the loop) and is EXCLUDED from the A6b percentiles, never read as an instant decision. Observability only — it gates nothing (INV-08).';
