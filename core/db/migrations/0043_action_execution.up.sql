-- 0043 — record EVERY execution, not just the first of each action shape (roadmap P2-1).
--
-- THE DEFECT. action_id = SHA-256(canonicalJSON(Action)) is content-addressed over THE OPERATION ALONE
-- (core/manifest: "identity is the operation alone"), and action_verdict is PRIMARY KEY (action_id) written
-- ON CONFLICT DO NOTHING. So the second and every later execution of the same action shape records NOTHING.
-- Measured live before this migration: 113 `execute|pass` events collapsed into 28 distinct action_ids —
-- roughly three quarters of all executions left no durable outcome.
--
-- That makes the breadth criterion the roadmap depends on ("N INDEPENDENT hands-off heals of class X")
-- literally unrecordable, and any per-execution actuation-accuracy rate uncomputable.
--
-- WHY THIS IS ADDITIVE AND action_verdict IS UNTOUCHED. The obvious fix — re-key action_verdict — is wrong on
-- two counts:
--
--   1. Its first-wins semantics are a DELIBERATE, DOCUMENTED CONTRACT, not an accident. spec/012: "The durable
--      per-action-shape row remains the decision tracer's record of the action shape's FIRST verified outcome
--      (TG-124)." Four readers depend on exactly one row per action_id — core/db/sessions_read.go's LEFT JOIN
--      would DUPLICATE session rows, and verdict.go's Get() plus trace_spine_read.go's LIMIT-1 lookup would
--      each start returning an arbitrary row with no ordering.
--   2. It is an append-only ledger with UPDATE/DELETE revoked (migration 0015). Re-keying it means rewriting
--      audited history, which that design exists to prevent.
--
-- INV-07 IS NOT AFFECTED and needs no amendment. The invariant governs PLAN ADHERENCE — that action_id is
-- computed once, threaded unchanged, and asserted at every stage so the executed thing is provably the
-- authorized thing. It says nothing about a verdict store's primary key. action_id keeps its exact meaning
-- and its exact computation here.
--
-- WHAT IS RECORDED. The FRESH per-execution verdict the interceptor already computes against THIS execution's
-- post-state. spec/012 already establishes that value as the correct one to reason about (the novelty
-- writeback deliberately reads it instead of the durable per-shape row, precisely because a re-cycled shape
-- otherwise inherits its first execution's verdict forever). It exists and is currently discarded after use;
-- this gives it somewhere durable to live.

CREATE TABLE action_execution (
  -- Per-OCCURRENCE identity. Deliberately NOT action_id: the whole point is that one action shape executes
  -- many times, each with its own independently-observed outcome.
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- The plan-adherence fingerprint, unchanged (INV-07). Indexed, NOT unique.
  action_id      text        NOT NULL CHECK (length(btrim(action_id)) > 0),
  -- The incident this execution answered, so an execution joins back to its session without guessing.
  external_ref   text        NOT NULL DEFAULT '',
  -- The FRESH verdict for THIS execution, or NULL when the post-state was unobservable. NULL is the honest
  -- record of "we executed and could not verify" — fail-closed (TG-182) — and must never read as a match.
  verdict        verdict,
  -- Set when the post-state could not be read; pairs with a NULL verdict so the two are never confused.
  unverifiable   boolean     NOT NULL DEFAULT false,
  target_host    text        NOT NULL DEFAULT '',
  site           text        NOT NULL DEFAULT '',
  executed_at    timestamptz NOT NULL DEFAULT now(),
  schema_version int         NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  -- An unverifiable execution has no verdict, and a verdict means it was verified. Enforced so the pair
  -- cannot drift into a state where a withheld verdict is later mistaken for a clean one.
  CONSTRAINT action_execution_verdict_pairing_chk
    CHECK ((unverifiable AND verdict IS NULL) OR (NOT unverifiable AND verdict IS NOT NULL))
);

CREATE INDEX action_execution_action ON action_execution (action_id, executed_at);
CREATE INDEX action_execution_time   ON action_execution (executed_at);
CREATE INDEX action_execution_ref    ON action_execution (external_ref);

-- Same tamper-resistance as every other execution/verdict surface: append and read, never rewrite. An
-- execution record that can be edited after the fact is not evidence (INV-19; migration 0015 precedent).
REVOKE UPDATE, DELETE ON action_execution FROM tg_runtime;
