-- 0022 pending_verification — the DURABLE pending-verification store of the GLOBAL async-verify channel
-- (spec/017 T-017-8, REQ-1709/1711/1712, TG-110). This is the cross-restart state that makes a launched
-- AWX job's deferred verify SURVIVE a worker restart — a flip-prerequisite. Without it the pending record
-- lived in memory, so a worker restart between a launch and its terminal poll would forget the launch and
-- lose the launch-as-prediction discipline (a mutation whose effect was never confirmed would vanish, never
-- staying a visible pending/unverified record).
--
-- WHY THIS TABLE IS DIFFERENT FROM THE 0020 REGIME AUDIT TABLES: 0020 (regime_resolution / regime_actuation /
-- deferred_verdict) is APPEND-ONLY history — INSERT/SELECT only, UPDATE+DELETE revoked. THIS record is MUTABLE
-- by design: a launch is RESERVED (state='pending'), a job handle is bound (still pending), then the deferred
-- verify transitions it EXACTLY ONCE to a terminal state ('verified') or a bound-elapsed state ('unverified').
-- So — like the latest-wins mutation_breaker_state (0021) and policy_mode (0019) — the runtime DML role KEEPS
-- UPDATE (the status transition needs it). It does NOT keep DELETE: a resolved record is retained for audit
-- and, above all, for IDEMPOTENCY (REQ-1712) — the action_id must stay claimed forever so a retry / re-poll /
-- redelivery can never reserve a SECOND launch for an action that already carried one (live OR terminal).
--
-- THE IDEMPOTENCY GUARANTEE IS THE PRIMARY KEY. action_id is the PRIMARY KEY (UNIQUE + NOT NULL), so a second
-- Reserve for the same action_id fails ATOMICALLY at the database with a unique-violation (SQLSTATE 23505) —
-- the durable pgx store maps that to regime.ErrDuplicatePending, which regime.AsyncVerify.Reserve turns into
-- ErrDuplicateLaunch, and the async lane MUST NOT launch a second job. The no-double-launch guard is enforced
-- by the DB, not by a read-then-write race in the app.
--
-- Single-organization (ADR-0010, paradigm-rule 1): NO tenant_id; the DML-only runtime role is the privilege
-- boundary. NON-SECRET by construction (INV-13): no argv / host / credential / password / private_key column —
-- the launch's AWX token is a SecretRef held elsewhere, never here; the only launch-bound value stored is the
-- non-secret AWX job_id handle. The committed launch prediction is stored NUL-free as jsonb (the deterministic
-- verifier diffs the terminal outcome against it after a restart). schema_version guards the row shape for a
-- forward-compatible reader. This migration is VERIFY-STATE persistence only; it launches nothing and actuates
-- nothing (mutation stays OFF at Shadow — zero launches occur, so the table is empty until the flip).

CREATE TABLE pending_verification (
  action_id       text PRIMARY KEY CHECK (length(btrim(action_id)) > 0),  -- idempotency key (REQ-1712): one launch per action_id, atomic at the DB
  op_class        text NOT NULL DEFAULT '',                               -- the graduation bucket the deferred verdict feeds (non-secret)
  lane            text NOT NULL CHECK (lane IN ('native-ssh', 'awx-job', 'gitops-mr', 'k8s-declarative', 'api')),  -- the async effect lane
  job_id          text NOT NULL DEFAULT '',                               -- the async AWX job handle; '' until BindHandle records it (non-secret)
  state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'verified', 'unverified')),          -- deferred-verify lifecycle
  prediction      jsonb NOT NULL DEFAULT '{}'::jsonb,                     -- the committed launch prediction (NUL-free; verify.Prediction), non-secret
  launched_at     timestamptz NOT NULL,                                   -- when Reserve claimed the action_id; the verification bound is measured from here (REQ-1711)
  terminal_status text NOT NULL DEFAULT '' CHECK (terminal_status = '' OR terminal_status IN ('successful', 'failed', 'error', 'canceled')),  -- terminal AWX status once reached
  verdict         text NOT NULL DEFAULT '' CHECK (verdict = '' OR verdict IN ('match', 'partial', 'deviation')),   -- mechanical verdict against the prediction; '' while pending / on a timeout
  verified        boolean NOT NULL DEFAULT false,                         -- did a terminal let the verifier adjudicate the post-state (feeds graduation semantics)
  resolved_at     timestamptz,                                            -- when it reached terminal/unverified; NULL while pending
  schema_version  int NOT NULL DEFAULT 1 CHECK (schema_version > 0),      -- reader-guard invariant
  created_at      timestamptz NOT NULL DEFAULT now()
);
-- The visible pending-verification / unverified queues the console (REQ-1716) and escalation read, ordered by state.
CREATE INDEX pending_verification_state_idx ON pending_verification (state);

-- Mutable current-state (NOT append-only): the runtime DML role KEEPS INSERT/SELECT/UPDATE (Reserve inserts,
-- BindHandle + the deferred verify UPDATE the status transition) but LOSES DELETE — a claimed action_id is
-- never removed, so the idempotency guard (REQ-1712) holds forever and a resolved record stays auditable.
REVOKE DELETE ON pending_verification FROM tg_runtime;
