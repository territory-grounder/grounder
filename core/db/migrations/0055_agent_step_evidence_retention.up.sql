-- 0055: a RETENTION BOUND for agent_step_evidence, and the one privileged path allowed to enforce it (TG-295).
--
-- WHAT 0053 SHIPPED, AND WHAT IT LEFT OPEN. 0053 gave untrusted host output a durable write primitive —
-- `payload text NOT NULL DEFAULT ''` — and made it tamper-resistant with `REVOKE UPDATE, DELETE FROM
-- tg_runtime`. Append-only was right: evidence an operator audits must not be rewritable after the fact.
-- But append-only was implemented as UNDELETABLE BY ANYONE, and nothing anywhere bounded the corpus. There
-- was no retention policy, no reaper, and no path — privileged or otherwise — by which a row could ever
-- leave. A screened tool result recorded on 2026-08-03 was, before this migration, scheduled to outlive the
-- deployment.
--
-- That is the wrong side of the split this repository already drew. docs/DATA-MODEL.md §5.1/§5.2 separates
-- the AUDIT SPINE (derived, de-identified decision facts — governance_ledger, session_risk_audit,
-- infragraph_prediction: preserved by integrity-preserving archival, never deleted to meet a TTL) from the
-- PURGEABLE OPERATIONAL BODY (raw, PII-bearing, high-cardinality content: transcripts, tool output, chat —
-- governed by configurable TTL and hard delete, INV-14). agent_step_evidence is verbatim output from a host,
-- screened but not sealed; it is the operational body wearing the spine's grants. Tamper-resistance and
-- immortality are different properties, and 0053 bought the second while only needing the first.
--
-- WHY A SECURITY DEFINER FUNCTION RATHER THAN `GRANT DELETE` TO A SECOND ROLE.
-- Both reconcile the reaper with the REVOKE. They are not equivalent, and the difference is what each one
-- makes POSSIBLE on the day something goes wrong:
--
--   1. A DELETE grant is a privilege over the TABLE; this function is a privilege over one OPERATION. A role
--      holding DELETE can delete any row, including the single row that documents the tool result an
--      attacker would most want gone. reap_agent_step_evidence can only ever delete rows strictly older than
--      a caller-supplied cutoff, and cannot be aimed at a session, an evidence_id, or a payload — there is
--      no parameter with which to say WHICH row. The shape of the privilege matches the shape of retention.
--
--   2. The audit is not a convention the caller can skip. The DELETE and the journal INSERT are one function
--      body, so they are one transaction: there is no interleaving in which rows leave without
--      agent_step_evidence_reap gaining the row that says so. And tg_runtime holds no INSERT on that journal,
--      so the runtime can neither forge a purge that did not happen nor bury one that did. Compare a second
--      role with DELETE, where "and it also writes an audit row" is a promise made in Go, on the honest path,
--      by the same process the audit exists to hold to account.
--
--   3. A distinct login role is a distinct CREDENTIAL — a second DSN in the worker's environment and a second
--      pool, whose one distinguishing power is the ability to erase evidence, living in the same process that
--      writes it. This function needs no new secret: EXECUTE is granted to the role that is already connected,
--      and the elevation lasts exactly one statement.
--
--   4. THE FLOOR. The function refuses any cutoff inside the last 24 hours, so this path cannot be used to
--      erase the recent window — which is precisely the window that would contain the steps of whatever went
--      wrong. A retention reaper legitimately never needs a cutoff of now(); an attacker with the runtime
--      role does, and no longer has one.
--
-- The cost, stated plainly because SECURITY DEFINER is a real escalation: the body runs as the function's
-- OWNER (tg_migration in the compose deployment — the DDL role), so a defect in it is a defect with owner
-- privileges. It is written to keep that surface as small as a purge can be: no dynamic SQL, no identifier
-- ever comes from a parameter, both parameters are typed (timestamptz, integer) and range-checked before use,
-- and search_path is pinned so no caller-controlled schema can shadow a name inside it.
--
-- ONE INDEX. The reaper's predicate is created_at; without an index every sweep seq-scans the whole corpus,
-- which is the table this migration exists because it grows.
CREATE INDEX agent_step_evidence_created ON agent_step_evidence (created_at);

-- THE JOURNAL. One row per purge that actually removed something (rows_deleted > 0 is a CHECK, so a row here
-- is a statement that evidence left the table, never a heartbeat). It records the cutoff the caller asked
-- for, how many rows went, the created_at span they covered, and WHO ASKED.
--
-- Who-asked is deliberately not current_user: inside a SECURITY DEFINER body current_user is the function's
-- OWNER, so it would write the same name on every line — an attribution column that cannot distinguish two
-- callers is worse than none, because it reads like one. The function resolves the caller as "the role in
-- effect at the call, else the login role" (see below), which names tg_runtime both when the worker connects
-- as it and when a session assumes it.
--
-- Deliberately NOT written on a no-op sweep. A journal that gains a row every tick is a timer, it would need
-- its own retention bound, and this ticket exists because a table nothing bounds was allowed to exist.
CREATE TABLE agent_step_evidence_reap (
  id             bigserial   PRIMARY KEY,
  reaped_at      timestamptz NOT NULL DEFAULT now(),
  cutoff         timestamptz NOT NULL,                                 -- rows strictly older than this went
  rows_deleted   bigint      NOT NULL CHECK (rows_deleted > 0),        -- a journal row means a real deletion
  oldest_deleted timestamptz NOT NULL,                                 -- created_at span actually removed,
  newest_deleted timestamptz NOT NULL,                                 --   so a purge can be reconstructed
  invoked_by     text        NOT NULL DEFAULT '',                      -- the CALLER's role, never the owner's
  schema_version integer     NOT NULL DEFAULT 1 CHECK (schema_version > 0)
);

COMMENT ON TABLE agent_step_evidence_reap IS
  'Append-only journal of every agent_step_evidence purge (TG-295). Written INSIDE reap_agent_step_evidence, in the same transaction as the DELETE, by a role tg_runtime does not have — so a deletion cannot happen unrecorded and a record cannot be forged.';

-- THE ONE PRIVILEGED PATH. Deletes at most max_rows rows older than cutoff, journals the purge, returns the
-- count so the caller can log a real number instead of "the sweep ran".
--
-- The batch cap is not decoration: the first sweep after an operator shortens retention can be arbitrarily
-- large, and one unbounded DELETE on the evidence corpus holds locks and bloats WAL for as long as it takes.
-- Bounded batches drain across ticks. Rows are selected BY PRIMARY KEY with FOR UPDATE SKIP LOCKED, so two
-- workers sweeping concurrently divide the work instead of racing (and never by ctid, which a concurrent
-- insert can reuse under a delete).
CREATE FUNCTION reap_agent_step_evidence(cutoff timestamptz, max_rows integer DEFAULT 50000)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
  deleted_count bigint := 0;
  oldest        timestamptz;
  newest        timestamptz;
  -- WHO ASKED, resolved before anything is deleted. The `role` GUC survives the SECURITY DEFINER switch and
  -- holds the role the caller had assumed ('none' when they are simply logged in as it), so this reads
  -- tg_runtime whether the worker connects as tg_runtime (the compose deployment) or an operator session
  -- assumed it. current_user here would be the function's owner in both cases.
  invoker       text := coalesce(nullif(current_setting('role', true), 'none'), session_user);
BEGIN
  IF cutoff IS NULL THEN
    RAISE EXCEPTION 'reap_agent_step_evidence: cutoff is NULL — a purge with no bound is not a retention policy';
  END IF;
  IF max_rows IS NULL OR max_rows <= 0 THEN
    RAISE EXCEPTION 'reap_agent_step_evidence: max_rows must be positive, got %', max_rows;
  END IF;
  -- The floor (see 4 above). A retention reaper never needs to reach into the last day; the only caller that
  -- does is one trying to erase what just happened.
  IF cutoff > now() - interval '24 hours' THEN
    RAISE EXCEPTION 'reap_agent_step_evidence: cutoff % is inside the 24h floor — the most recent day of evidence is the window that would contain an incident, and this path cannot delete it', cutoff;
  END IF;

  WITH doomed AS (
    SELECT id FROM public.agent_step_evidence
    WHERE created_at < cutoff
    ORDER BY created_at
    LIMIT max_rows
    FOR UPDATE SKIP LOCKED
  ), gone AS (
    DELETE FROM public.agent_step_evidence e USING doomed d
    WHERE e.id = d.id
    RETURNING e.created_at
  )
  SELECT count(*), min(created_at), max(created_at) INTO deleted_count, oldest, newest FROM gone;

  IF deleted_count > 0 THEN
    INSERT INTO public.agent_step_evidence_reap
      (cutoff, rows_deleted, oldest_deleted, newest_deleted, invoked_by)
    VALUES (cutoff, deleted_count, oldest, newest, invoker);
  END IF;
  RETURN deleted_count;
END;
$$;

COMMENT ON FUNCTION reap_agent_step_evidence(timestamptz, integer) IS
  'The ONLY path by which an agent_step_evidence row can be deleted (TG-295). SECURITY DEFINER rather than a DELETE grant: the privilege is over the operation (rows older than a cutoff, never a named row), the journal write cannot be skipped, no new credential exists, and a cutoff inside 24h is refused.';

-- Nothing may reach this function by default, and it must not be reachable by a future role that merely
-- happens to connect. EXECUTE goes to the runtime role and to nobody else.
REVOKE ALL ON FUNCTION reap_agent_step_evidence(timestamptz, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION reap_agent_step_evidence(timestamptz, integer) TO tg_runtime;

-- STATE THE APPEND PATH EXPLICITLY, DO NOT INHERIT IT. tg_runtime's INSERT/SELECT on this table came from a
-- blanket `ALTER DEFAULT PRIVILEGES ... GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES` in
-- deploy/postgres-init/00-roles.sh, of which 0053 then subtracted two verbs. A privilege model assembled by
-- subtraction from a wildcard is a model no reader can check and no test can assert against, and it is why
-- the same table is append-only in one database and unrestricted in another (any database whose tables were
-- not created by tg_migration gets no default privileges at all). Spell out both halves here so the intent
-- is legible in one place and provable in any migrated database: append and read, never modify, never
-- delete — except through the function above.
-- The sequence is part of the append path and is a SEPARATE privilege: `id bigserial` means every INSERT
-- calls nextval, and a table grant alone leaves it failing with "permission denied for sequence
-- agent_step_evidence_id_seq". Measured while writing the oracle for this migration — which is the argument
-- for stating the model rather than inheriting it, because the wildcard in 00-roles.sh has a second wildcard
-- (`ALTER DEFAULT PRIVILEGES ... ON SEQUENCES`) that is just as invisible and just as absent from any
-- database whose objects tg_migration did not create.
GRANT SELECT, INSERT ON agent_step_evidence TO tg_runtime;
GRANT USAGE, SELECT ON SEQUENCE agent_step_evidence_id_seq TO tg_runtime;
REVOKE UPDATE, DELETE ON agent_step_evidence FROM tg_runtime;

-- The journal is READ-ONLY to the runtime: it can show an operator what was purged and can never author,
-- amend or remove an entry. The only writer is the function, which runs as the table's owner — and gets its
-- sequence value the same way, so no sequence grant is issued here either.
GRANT SELECT ON agent_step_evidence_reap TO tg_runtime;
REVOKE INSERT, UPDATE, DELETE ON agent_step_evidence_reap FROM tg_runtime;
