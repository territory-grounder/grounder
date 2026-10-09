-- 0053: the GROUND TRUTH behind each recorded reasoning step (TG-272).
--
-- The #reasoning view renders, under every step, a button labelled "ground truth <tool>", and the view's own
-- header promises "every claim, one click from ground truth". The button did nothing — it raised a toast
-- reading "Pivoting to ground truth: …" and stopped. There was nowhere to pivot TO: agent_step.observation
-- holds a 30–40 character REFERENCE ("observed incident-history-dc1pve01"), never the content, and no
-- other table, endpoint or retained store carried the tool's actual output. Measured 2026-08-03: 3241
-- sessions, 17759 steps, zero stored tool results. Temporal's history is not an answer either — it retains
-- 395 executions against those 3241 sessions, so it expires long before an audit would ask.
--
-- WHAT IS STORED IS THE SCREENED PAYLOAD, NOT THE RAW OUTPUT. agent/loop.go already runs every tool result
-- through screenToolOutput (neutralize prompt-injection spans, redact secret-shaped text) before it re-enters
-- the model prompt; that SAME screened value is what lands here, then passes screen.Scrub on the way in like
-- every other agent_step field. A tool result is attacker-influenceable data (INV-08) and may carry a leaked
-- token (INV-13): the console must never be the surface that un-redacts it.
--
-- APPEND-ONLY (REQ-2016), matching agent_step (0031), interceptor_gate_verdict (0030) and graduation_credit
-- (0050): the runtime role INSERTs and SELECTs, and holds no UPDATE/DELETE. Evidence that can be rewritten
-- after the fact is not evidence — least of all on the surface an operator uses to audit the agent.
CREATE TABLE agent_step_evidence (
  id             bigserial PRIMARY KEY,
  external_ref   text NOT NULL CHECK (length(btrim(external_ref)) > 0),  -- session correlation key (non-secret)
  cycle          integer NOT NULL,                                       -- 1-based ReAct cycle ordinal
  evidence_id    text NOT NULL CHECK (length(btrim(evidence_id)) > 0),   -- the tool result's own id (tr.ID)
  tool           text NOT NULL DEFAULT '',                               -- tool name invoked this cycle
  payload        text NOT NULL DEFAULT '',                               -- SCREENED + SCRUBBED tool output
  -- Whether the stored payload is shorter than what the tool returned. A truncated body that does not SAY it
  -- is truncated is a quiet lie on an evidence surface — the console renders this, it is not bookkeeping.
  truncated      boolean NOT NULL DEFAULT false,
  full_bytes     integer NOT NULL DEFAULT 0 CHECK (full_bytes >= 0),     -- pre-truncation length, for honesty
  created_at     timestamptz NOT NULL DEFAULT now(),
  schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  -- One evidence row per (session, cycle, evidence id). A retried activity must not double-append the same
  -- observation: Temporal retries the investigate activity, and the emit path is deliberately best-effort.
  UNIQUE (external_ref, cycle, evidence_id)
);

-- The console's read is always "this session's evidence, by id" — index the way it is queried.
CREATE INDEX agent_step_evidence_ref ON agent_step_evidence (external_ref, evidence_id);

COMMENT ON TABLE agent_step_evidence IS
  'Screened, scrubbed tool output backing each agent_step — the ground truth the #reasoning citation opens (TG-272). Append-only; stores the SCREENED payload, never the raw tool result.';

-- Evidence that can be rewritten is not evidence.
REVOKE UPDATE, DELETE ON agent_step_evidence FROM tg_runtime;
