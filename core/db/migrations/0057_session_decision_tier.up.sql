-- 0057: record WHICH MODEL DECIDED, not only which model read (TG-198).
--
-- Migration 0027 added `model_tier`, and the runner filled it from the agent's INVESTIGATION tier
-- (`ag.ModelName`) — the tier the read-only loop makes its many tool calls on, "fast" for an ordinary
-- incident. But TG-60 switches the ONE forced-decision cycle at the poll limit to the reasoning tier
-- (`DecisionModelName`, "primary"), because the fast tier is too weak to obey the "decide now" nudge and
-- hands off empty. Nothing recorded that switch. So every session that was nudged read on "fast", DECIDED
-- on "primary", and stored "fast" — and the whole 537-incident corpus attributes 100% of its decisions to
-- the cheap tier, including the ones the cheap tier never made.
--
-- The consequence is not cosmetic: "did the expensive tier actually decide better?" is unanswerable against
-- TG's own history, and the three-arm model-tier A/B (TG-204) has no dependent variable to measure. A
-- corpus that cannot attribute a decision to a model cannot compare models.
--
-- A SECOND COLUMN, NOT AN OVERLOAD — this is the deliberate part. Re-pointing `model_tier` at the decision
-- tier would have been one line, and it would have made one column mean "investigation tier" for every row
-- written before this migration and "decision tier" for every row after, with nothing on the row saying
-- which. Any aggregate over the corpus would then silently mix the two populations. It would also DESTROY a
-- fact TG still needs: a session may investigate on one tier and decide on another, and the pair is the
-- interesting datum (it is exactly the TG-60 effect). Two columns keep both facts and keep every historical
-- row's meaning intact.
--
-- '' MEANS UNATTRIBUTABLE, NOT "fast". Pre-migration rows and pre-deploy workflow histories read back
-- empty, which is the honest value — those sessions genuinely did not record who decided. Defaulting them
-- to a tier NAME would have manufactured the exact false attribution this migration exists to end. The
-- empty string is distinguishable from every real tier, so an analysis excludes them rather than counting
-- them (the same discipline as 0056's NULL diagnosis: a pre-feature row is scored N/A, never floored).
--
-- Additive, defaulted, backward-compatible: every existing row stays valid and every pre-field writer keeps
-- working. session_triage is NOT on the append-only spine (0015) — the judge cron already UPDATEs it — so
-- no grant change is needed. OBSERVABILITY ONLY: nothing reads this to decide anything (INV-08).
ALTER TABLE session_triage ADD COLUMN IF NOT EXISTS decision_model_tier text NOT NULL DEFAULT '';

COMMENT ON COLUMN session_triage.decision_model_tier IS
  'The LLM tier that produced the TERMINAL proposal or grounded stop — which model DECIDED (TG-198). Distinct from model_tier, which is the tier the read-only INVESTIGATION ran on; the two differ whenever the TG-60 poll-limit decide-nudge hands the final cycle to the reasoning tier. '''' = the session predates this column and is UNATTRIBUTABLE (never read as "fast"). Observability only — it gates nothing (INV-08).';
