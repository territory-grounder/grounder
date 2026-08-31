-- 0058: the ROUTING DECISION gets an audit trail (TG-169).
--
-- The execution topology — DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED —
-- is chosen for every incident BEFORE any expensive context is built, and until this table it was written
-- NOWHERE. It was computed at the top of the Runner workflow, returned on RunnerResult, and dropped: no
-- column on session_triage, no ledger entry, nothing. So for the thousands of sessions already recorded,
-- "why did this incident get the deep path?" is unanswerable against TG's own history, and a change to the
-- routing rule could not be measured against the behaviour it replaced. A decision that leaves no trail
-- cannot be reviewed.
--
-- THE INPUTS ARE PERSISTED, NOT JUST THE CLASS. `inputs_json` is the whole core/execclass.Input the
-- classifier was called with. The class alone is a conclusion; re-deriving the premises later is guesswork
-- the moment the rules move, and the rules are expected to move (Input carries eight signals and the
-- correlation stage feeds one of them today). Storing the input makes a past decision REPLAYABLE against a
-- future classifier, which is the only way to tell "the rule changed" from "the estate changed".
--
-- CORRELATION EVIDENCE, NOT A BARE BOOLEAN. `correlated` used to be `severity == critical` — a property of
-- ONE alert standing in for a claim about the RELATIONSHIP between alerts, which made 81% of live incidents
-- (2,434 of 2,995 admitted alerts are critical) assert they "span multiple systems" with nothing behind it.
-- The columns beside it are what the claim now rests on: the distinct hosts and ingest sources found in the
-- window, the window itself, and the member alert refs. `reason` is a CONTROLLED vocabulary
-- (isolated / multi-host-window / cross-source-window / window-unavailable) so the population is groupable.
--
-- 'window-unavailable' IS NOT 'isolated'. A session whose correlation window could not be read records the
-- degraded reason and `degraded = true`, never a quiet "not correlated" — otherwise a dead reader is
-- indistinguishable from a quiet estate and a broken correlation stage ships unnoticed. Same discipline as
-- 0057's '' decision tier: the honest value for "TG did not record this" is never a real value.
--
-- APPEND-ONLY evidence (REQ-2016): like ingest_alert (0033), agent_step (0031), interceptor_gate_verdict
-- (0030) and the accountability spine (0015), the runtime role may INSERT + SELECT but holds NO
-- UPDATE/DELETE — a recorded routing decision is tamper-resistant. NON-SECRET columns only: host names,
-- source slugs, external refs and counts. Never a payload, never a credential (INV-13).
--
-- OBSERVABILITY + REVIEW ONLY: nothing reads a row here back into a gate (INV-08).
CREATE TABLE exec_class_decision (
  id               bigserial PRIMARY KEY,
  external_ref     text NOT NULL CHECK (length(btrim(external_ref)) > 0),  -- the session correlation key
  exec_class       text NOT NULL CHECK (length(btrim(exec_class)) > 0),    -- the chosen topology (core/execclass.Class)
  correlated       boolean NOT NULL DEFAULT false,                         -- the signal the topology routed on
  reason           text NOT NULL DEFAULT '',                               -- controlled vocabulary (see above)
  degraded         boolean NOT NULL DEFAULT false,                         -- the window could not be read
  window_seconds   integer NOT NULL DEFAULT 0,                             -- the span the verdict was reached over
  distinct_hosts   integer NOT NULL DEFAULT 0,                             -- distinct hosts inside the window
  distinct_sources integer NOT NULL DEFAULT 0,                             -- distinct ingest sources inside the window
  member_count     integer NOT NULL DEFAULT 0,                             -- FULL cluster size (evidence_json's list is capped)
  inputs_json      jsonb NOT NULL DEFAULT '{}',                            -- the whole execclass.Input
  evidence_json    jsonb NOT NULL DEFAULT '{}',                            -- {hosts:[], sources:[], members:[]} (non-secret)
  decided_at       timestamptz NOT NULL DEFAULT now(),
  schema_version   integer NOT NULL DEFAULT 1
);

-- ONE canonical routing decision per session. The stage runs once per incident, but its activity retries
-- like any other, and a retry must not accumulate duplicate rows on a table whose DELETE is revoked
-- (the writer is ON CONFLICT (external_ref) DO NOTHING — first write wins, matching ingest_alert 0033).
CREATE UNIQUE INDEX exec_class_decision_ref_uidx ON exec_class_decision (external_ref);
-- The review read is "what did TG route, and how, over this period".
CREATE INDEX exec_class_decision_at_idx ON exec_class_decision (decided_at DESC);

-- Evidence immutability: revoke UPDATE + DELETE for the DML runtime role. tg_runtime keeps INSERT + SELECT
-- (granted by the blanket ALTER DEFAULT PRIVILEGES in deploy/postgres-init/00-roles.sh), so a recorded
-- routing decision can be appended and read but never rewritten (REQ-2016).
REVOKE UPDATE, DELETE ON exec_class_decision FROM tg_runtime;
