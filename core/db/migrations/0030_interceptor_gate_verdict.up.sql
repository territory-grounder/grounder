-- 0030: the per-interceptor-gate VERDICT TRAIL (spec/020 T-020-7, REQ-2007/REQ-2001/REQ-2016).
--
-- The actuation interceptor (core/actuate) runs an ORDERED chain of gates — admission, never-auto floor,
-- structure, evidence, territory, verifiability, policy, breaker, mode chokepoint, execute, verify — and today
-- only the terminal decision reaches the ledger. The decision tracer needs each gate's verdict-and-reason IN
-- ORDER, so an operator can see exactly which gate an action passed or was refused at. One immutable NON-SECRET
-- row per gate, keyed by BOTH correlation keys (action_id + external_ref) so it joins the per-incident walk.
--
-- NON-SECRET by construction: gate name + verdict (pass/refuse/match/partial/deviation) + a non-secret reason
-- only — NEVER argv, host, or credential (INV-13). APPEND-ONLY (REQ-2016): the runtime role holds no UPDATE and
-- no DELETE, so a persisted gate row is tamper-resistant like the rest of the accountability spine (0015). The
-- interceptor emits observe-only: an emit failure never changes a gate outcome (the chokepoint stays fail-closed).
CREATE TABLE interceptor_gate_verdict (
  id             bigserial   PRIMARY KEY,
  action_id      text        NOT NULL CHECK (length(btrim(action_id)) > 0),  -- INV-07 content-hashed binding (non-secret)
  external_ref   text        NOT NULL DEFAULT '',                            -- the incident this action answers (correlation key)
  ordinal        integer     NOT NULL,                                       -- the gate's position in the ordered chain (1-based)
  gate           text        NOT NULL,                                       -- the gate name (admission/never-auto-floor/.../verify)
  verdict        text        NOT NULL,                                       -- pass | refuse | match | partial | deviation
  reason         text        NOT NULL DEFAULT '',                            -- non-secret refusal/decision reason
  created_at     timestamptz NOT NULL DEFAULT now(),
  schema_version integer     NOT NULL DEFAULT 1
);

CREATE INDEX interceptor_gate_verdict_action_idx ON interceptor_gate_verdict (action_id, ordinal);
CREATE INDEX interceptor_gate_verdict_ref_idx    ON interceptor_gate_verdict (external_ref);

-- Append-only tamper-resistance (REQ-2016), same pattern as the 0015 accountability spine.
REVOKE UPDATE, DELETE ON interceptor_gate_verdict FROM tg_runtime;
