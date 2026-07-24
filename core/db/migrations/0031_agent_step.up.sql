-- 0031: per-ReAct-cycle agent-step transcript for the decision tracer (spec/020 T-020-8, REQ-2008/REQ-2016).
--
-- The agent loop emits, per cycle, an optional CoT thought + a tool call and its result (agent.Result.Thoughts
-- / ToolResults). These are UNTRUSTED DATA and may carry secret-shaped text (a leaked token in a tool output, a
-- prompt-injection span in a thought). The tracer needs the reasoning walk, but a governed row must NEVER hold a
-- value-shaped secret (INV-13) and a thought must NEVER re-enter the decision path (INV-08). So every field is
-- run through core/screen.Scrub (redact secrets + neutralize injections) BEFORE it is written here — this table
-- stores only the SCRUBBED, non-secret projection of one cycle.
--
-- APPEND-ONLY evidence (REQ-2016): like interceptor_gate_verdict (0030) and the accountability spine (0015),
-- the runtime role may INSERT + SELECT but holds NO UPDATE/DELETE — a persisted agent step is tamper-resistant.
-- NON-SECRET columns only: a scrubbed thought, the tool name, a scrubbed observation summary, and a per-cycle
-- outcome label. No argv/host/credential column exists.
CREATE TABLE agent_step (
  id             bigserial PRIMARY KEY,
  external_ref   text NOT NULL CHECK (length(btrim(external_ref)) > 0),  -- the session correlation key (non-secret)
  cycle          integer NOT NULL,                                       -- 1-based ReAct cycle ordinal
  thought        text NOT NULL DEFAULT '',                               -- SCRUBBED CoT reasoning (INV-08 data-only)
  tool           text NOT NULL DEFAULT '',                               -- the tool name invoked this cycle (non-secret)
  observation    text NOT NULL DEFAULT '',                               -- SCRUBBED tool-result summary
  outcome        text NOT NULL DEFAULT '',                               -- per-cycle outcome ('success'/'failure'/'')
  created_at     timestamptz NOT NULL DEFAULT now(),
  schema_version integer NOT NULL DEFAULT 1
);

CREATE INDEX agent_step_ref_idx ON agent_step (external_ref, cycle);

-- Evidence immutability: revoke UPDATE + DELETE for the DML runtime role. tg_runtime keeps INSERT + SELECT
-- (granted by the blanket ALTER DEFAULT PRIVILEGES in deploy/postgres-init/00-roles.sh), so the agent-step
-- transcript can be appended and read but never rewritten (REQ-2016).
REVOKE UPDATE, DELETE ON agent_step FROM tg_runtime;
