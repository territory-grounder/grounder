-- 0127: hold_decision — the durable record of every HOLD (TG-667, deep-review F5 of the G4A program).
--
-- A `fallback:autonomous-hold:<reason>` or `human:deny` decision wrote one governance-ledger row and one chat
-- notice; the rationale sentence lived only in Temporal history (24 h). Nothing listed open holds, nothing
-- alerted on a held incident that stayed actionable, and a person-caused stop that TG correctly HELD was
-- eligible to be HEALED when the same alert re-fired after the 30-minute attribution lookback had expired.
--
-- This table is a PROJECTION beside the ledger, like pending_decision (0007): the governance_ledger row stays
-- the authoritative decision record, and a row here can only ADD caution — the classifier polls
-- `prior-hold-still-open` while a sticky hold is open for (host, alert_rule), and the fallback approver holds
-- again. A forged or stray row therefore costs a heal, never buys one, which is why it is `plane: triage`
-- (written by RecordVoteActivity on tg.runner, read by ClassifyActivity and the console): the actuation plane
-- may read it and may not write it. INSERT-only for the runtime role (no UPDATE/DELETE): a renewal is a new
-- row, and "open" is a predicate on expires_at, not a mutable status.
CREATE TABLE hold_decision (
    seq               bigserial PRIMARY KEY,
    external_ref      text NOT NULL CHECK (external_ref <> ''),
    host              text NOT NULL DEFAULT '',
    alert_rule        text NOT NULL DEFAULT '',
    op_class          text NOT NULL DEFAULT '',
    action_id         text NOT NULL DEFAULT '',
    reason_key        text NOT NULL CHECK (reason_key <> ''),
    why               text NOT NULL DEFAULT '',
    actor_attribution text NOT NULL DEFAULT '',
    decided_by        text NOT NULL DEFAULT '',
    decided_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL
);
-- the classifier's read: the newest still-open hold for one (host, rule)
CREATE INDEX hold_decision_subject_idx ON hold_decision (host, alert_rule, expires_at DESC);
-- the console/metrics read: everything still open
CREATE INDEX hold_decision_expires_idx ON hold_decision (expires_at);
COMMENT ON TABLE hold_decision IS 'plane: triage';
GRANT SELECT, INSERT ON hold_decision TO tg_runtime;
