-- 0109 — conversation-turn memory: cross-session continuity for a recurring incident lineage (TG-80 P2-8;
-- clean-room from h-apache-stack's hot conversation memory, attribution: docs/SOURCE-BENCHMARK-CATALOG).
--
-- One row per SESSION TERMINAL, keyed by the incident's lineage (canonical rule family + host — the same
-- stable subject novelty keys on, TG-124): the DIGEST of what the session concluded (outcome + the typed
-- claim), never raw tool output and never model free-text beyond the already-screened conclusion fields
-- the triage row itself carries. The runner folds a lineage's recent turns into the next session's seed
-- as the <conversation_memory> UNTRUSTED block — "what did we conclude the last times this exact thing
-- happened here" — the temporal half the precedent block (similar incidents anywhere) does not carry.
--
-- RETENTION: this is the PURGEABLE operational body (docs/DATA-MODEL.md §5.2, INV-14), not the audit
-- spine — every row is a digest re-derivable from session_triage, so it takes a TTL (expires_at) and a
-- hard delete. Deletion runs ONLY through reap_conversation_turn (SECURITY DEFINER, the 0055 discipline):
-- the append-only default privileges (0105) leave tg_runtime without DELETE, and the function can only
-- remove EXPIRED rows in bounded batches — there is no parameter with which to name a row. No purge
-- journal, deliberately unlike 0055: these are TG's own outcome digests, not operator-audited evidence,
-- and the source of truth (session_triage) is untouched by the reap.
--
-- Plane: triage — written by the triage worker's terminal recorder, read at seed assembly. The actuation
-- plane neither writes nor needs it (TriageContentTables mirror).
CREATE TABLE conversation_turn (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  conversation_key text NOT NULL,                     -- canonical rule family + '|' + incident host (the lineage)
  external_ref     text NOT NULL,                     -- the session that wrote this turn
  content          text NOT NULL,                     -- the terminal digest (outcome + typed claim), pre-screened upstream
  created_at       timestamptz NOT NULL DEFAULT now(),
  expires_at       timestamptz NOT NULL
);

CREATE INDEX conversation_turn_lineage_idx ON conversation_turn (conversation_key, created_at DESC);

COMMENT ON TABLE conversation_turn IS 'plane: triage';

CREATE FUNCTION reap_conversation_turn(batch int) RETURNS int
LANGUAGE sql SECURITY DEFINER AS $$
  WITH doomed AS (
    SELECT id FROM conversation_turn WHERE expires_at < now() ORDER BY expires_at LIMIT batch
  ), deleted AS (
    DELETE FROM conversation_turn t USING doomed d WHERE t.id = d.id RETURNING t.id
  )
  SELECT count(*)::int FROM deleted
$$;

REVOKE ALL ON FUNCTION reap_conversation_turn(int) FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_runtime') THEN
    EXECUTE 'GRANT EXECUTE ON FUNCTION reap_conversation_turn(int) TO tg_runtime';
  END IF;
END $$;
