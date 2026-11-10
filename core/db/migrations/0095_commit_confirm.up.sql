-- spec/029 T-029-2 (REQ-2901, REQ-2906): the armed revert's DURABLE state. One row per
-- (action_id, external_ref) — action_id is content-addressed and REUSED across incidents of the
-- same action shape (the manifest is sealed first-wins), so the incident key participates in the
-- identity: each incident's actuation arms its OWN revert window.
--
-- Writer discipline (single-writer per phase): the ARM insert is the parent workflow's
-- ArmCommitConfirmActivity, BEFORE the effect executes; every LATER transition (aborted /
-- elapsed_unconfirmed now; confirmed / held_unverifiable / reverted / revert_failed under
-- T-029-3) is owned by the CommitConfirmWorkflow child — transitions are guarded
-- `WHERE state = 'armed'` so a lost/duplicate signal can never resurrect a resolved row.
CREATE TABLE commit_confirm (
    action_id         TEXT        NOT NULL,
    external_ref      TEXT        NOT NULL,
    op_class          TEXT        NOT NULL,
    target_host       TEXT        NOT NULL,
    site              TEXT        NOT NULL DEFAULT '',
    plan_hash         TEXT        NOT NULL DEFAULT '',
    -- armed | aborted | elapsed_unconfirmed | confirmed | held_unverifiable | reverted | revert_failed
    -- (the last four are written by T-029-3's confirm/inverse arms; the vocabulary is fixed here so
    -- the console and queries never chase a moving enum)
    state             TEXT        NOT NULL,
    window_seconds    BIGINT      NOT NULL,
    armed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline_at       TIMESTAMPTZ NOT NULL,
    resolved_at       TIMESTAMPTZ,
    resolution_detail TEXT        NOT NULL DEFAULT '',
    -- the fired inverse's sealed action id (T-029-3 cross-link; REQ-2906) — '' until an inverse fires
    inverse_action_id TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (action_id, external_ref),
    CONSTRAINT commit_confirm_state_check CHECK (state IN
        ('armed','aborted','elapsed_unconfirmed','confirmed','held_unverifiable','reverted','revert_failed')),
    CONSTRAINT commit_confirm_window_positive CHECK (window_seconds > 0)
);

-- The console timeline (T-029-5) and the T-029-3 elapse consult both scan for LIVE windows.
CREATE INDEX commit_confirm_live_idx ON commit_confirm (state, deadline_at)
    WHERE state IN ('armed', 'held_unverifiable');

COMMENT ON TABLE commit_confirm IS 'plane: both';
