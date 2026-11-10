-- 0007 pending_decisions — the POLL_PAUSE decisions currently awaiting a human vote (REQ-519), projected
-- so the console (grounder process) can LIST what the Runner (worker process) paused. This is OPERATIONAL
-- projection state, NOT a governed audit row: the governance_ledger remains the authoritative decision
-- record, and a row here can never release an action (releasing goes through /v1/vote → the waiting
-- workflow, which accepts a vote only when action_id names its sealed action, INV-12). Hence no
-- schema_version (cf. operator_sessions). Keyed by external_ref: a session holds one open decision at a time.
CREATE TABLE pending_decision (
  external_ref text PRIMARY KEY,
  action_id    text NOT NULL,
  band         text NOT NULL DEFAULT 'POLL_PAUSE',
  approaches   text[] NOT NULL DEFAULT '{}',
  prediction   text NOT NULL DEFAULT '',
  reversible   boolean NOT NULL DEFAULT false,
  site         text NOT NULL DEFAULT '',
  opened_at    timestamptz NOT NULL DEFAULT now(),
  status       text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
  outcome      text NOT NULL DEFAULT '',
  resolved_at  timestamptz
);
-- the console lists open decisions oldest-first
CREATE INDEX pending_decision_open_idx ON pending_decision (status, opened_at);
