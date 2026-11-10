-- 0122: persist the session TERMINUS FACTS the credit predicate and the commit-confirm window decide on (TG-662,
-- G4A findings F3/F8).
--
-- WHAT WAS WRONG. A commit-confirmed class (restart-service, restart-container, reload-service, start-guest) was
-- credited `verified_clean` when its window read the execute-time verdict `match` alone. That verdict EXCLUDES
-- the action's own target host (core/verify/verdict.go), so a heal whose target kept alerting still confirmed,
-- credited and graduated. The orchestrator-confirmed clear (ConfirmedClear) and the terminus collateral re-check
-- (TG-483) were computed in the parent workflow and never reached the window: the window's nudge was sent BEFORE
-- the clear loop even started, and the window itself (600 s) elapses before most clears settle (3-min ticks,
-- 30-min bound). ConfirmedClear itself was host-quiet only — a quiet host with the unit still down read as clear.
--
-- WHAT THIS ADDS. Three nullable, additive columns the terminus writes ONCE after the clear loop and the
-- collateral re-check have settled:
--   confirmed_clear_at   — when the incident confirmed clear (host quiet AND the healed OBJECT positively read in
--                          its goal state); NULL while unconfirmed. `confirmed_clear` (0039) keeps its boolean.
--   collateral_observed  — the TG-483 re-check's reading: true (a sibling lit up), false (surveyed and quiet), or
--                          NULL = UNKNOWN (no graph members / no reader). Unknown is INDETERMINATE: it never
--                          credits and never confirms (TG-668 observation half).
--   terminus_facts_at    — when the facts settled. NULL ⇒ the terminus has not spoken yet; the window reads
--                          that as PENDING (keeps watching), never as clear.
-- The commit-confirm consult reads these (never the model, never the absence of alerts) and confirms ONLY on
-- `match ∧ confirmed_clear ∧ collateral_observed = false`. The ladder credit (reconcile.go) reads the same facts
-- in-process. Pre-migration rows read NULL/false: unconfirmed, exactly as before.
--
-- OBSERVABILITY + GOVERNANCE RECORD: tg_runtime already holds UPDATE on session_triage (0039 MarkCleared); no new
-- grant. The row is TRIAGE-plane content (0060) and stays so — the actuation plane never writes it.
ALTER TABLE session_triage
    ADD COLUMN confirmed_clear_at timestamptz,
    ADD COLUMN collateral_observed boolean,
    ADD COLUMN terminus_facts_at timestamptz;

COMMENT ON COLUMN session_triage.confirmed_clear_at IS
    'TG-662: when the incident confirmed clear (host quiet AND the healed object positively in its goal state); NULL = unconfirmed';
COMMENT ON COLUMN session_triage.collateral_observed IS
    'TG-662/TG-483: the terminus collateral re-check — true observed, false surveyed-quiet, NULL unknown (indeterminate, never credits)';
COMMENT ON COLUMN session_triage.terminus_facts_at IS
    'TG-662: when the terminus facts settled; NULL = the terminus has not spoken (the commit-confirm window reads PENDING)';

-- CAUSALITY (owner prompt 2026-11-07 §2.D). A verified target state is not the claim that TG caused it: an
-- execution that precedes a recovery does not exclude a spontaneous recovery, another operator or the injector's
-- own restore. The terminus classifies causality from TG's durable evidence (injected_fault restores/injections,
-- ingest_transition recoveries before the execution, the actor-evidence readers' attribution over the interval)
-- and the clean credit requires 'tg-caused'. Recorded on the facts row for the commit-confirm window's feed …
ALTER TABLE session_triage
    ADD COLUMN causality text,
    ADD COLUMN causality_evidence text;
ALTER TABLE session_triage
    ADD CONSTRAINT session_triage_causality_chk
    CHECK (causality IS NULL OR causality IN ('tg-caused', 'external', 'injector-restore', 'indeterminate'));
COMMENT ON COLUMN session_triage.causality IS
    'TG-662: tg-caused | external | injector-restore | indeterminate — the terminus causality classification; NULL = not classified (legacy); only tg-caused credits';
COMMENT ON COLUMN session_triage.causality_evidence IS
    'TG-662: the one-line evidence record behind causality (what each reader saw over the heal interval)';

-- … and APPEND-ONLY in terminus_record: one row per session terminus with the explicit credit disposition
-- (clean | withheld:<reason> | not-clean:<reason>), whether the ladder was fed, the target-state facts, and the
-- causality with its evidence. The G4A pre-registration and the credit audit (TG-666) read this table; corrections
-- are appended elsewhere, never written over these rows. plane: both — the triage worker writes it (the terminus
-- runs on the triage queue), the audit reads it from either plane.
CREATE TABLE terminus_record (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    external_ref       text NOT NULL CHECK (external_ref <> ''),
    action_id          text NOT NULL DEFAULT '',
    op_class           text NOT NULL CHECK (op_class <> ''),
    host               text NOT NULL DEFAULT '',
    target_host        text NOT NULL DEFAULT '',
    site               text NOT NULL DEFAULT '',
    executed_at        timestamptz,
    cleared_at         timestamptz,
    executed           boolean NOT NULL DEFAULT false,
    has_verdict        boolean NOT NULL DEFAULT false,
    verdict            text NOT NULL DEFAULT '',
    confirmed_clear    boolean NOT NULL DEFAULT false,
    clear_reason       text NOT NULL DEFAULT '',
    object_state       text NOT NULL DEFAULT '',
    collateral         text NOT NULL DEFAULT 'unknown' CHECK (collateral IN ('quiet', 'positive', 'unknown')),
    credit_disposition text NOT NULL CHECK (credit_disposition <> ''),
    credit_fed         boolean NOT NULL DEFAULT false,
    credit_clean       boolean NOT NULL DEFAULT false,
    causality          text NOT NULL CHECK (causality IN ('tg-caused', 'external', 'injector-restore', 'indeterminate')),
    causality_evidence text NOT NULL DEFAULT '',
    window_state       text NOT NULL DEFAULT '',
    predicate_version  int NOT NULL DEFAULT 2,
    recorded_at        timestamptz NOT NULL DEFAULT now(),
    source             text NOT NULL DEFAULT 'terminus' CHECK (source IN ('terminus', 'window')),
    CONSTRAINT terminus_record_clean_is_tg_caused CHECK (NOT credit_clean OR causality = 'tg-caused')
);
-- source: 'terminus' = the session terminus's own decision (recordGraduationCredit); 'window' = the commit-confirm
-- window's later credit/demotion feed for the same run (ResolveCommitConfirmActivity). A commit-confirmed class is
-- usually WITHHELD at the terminus (window still armed) and credited or demoted by the window, so the window's
-- decision is a row of its own — the audit reads both and never infers the ladder feed from a withheld terminus.
-- One window resolves once per (action_id, external_ref) — commit_confirm's own key — so a retried resolve tail
-- appends nothing twice (ON CONFLICT DO NOTHING against this partial index; INSERT privilege suffices).
CREATE UNIQUE INDEX terminus_record_window_once ON terminus_record (external_ref, action_id) WHERE source = 'window';
CREATE INDEX terminus_record_ref ON terminus_record (external_ref);
CREATE INDEX terminus_record_class_time ON terminus_record (op_class, recorded_at);
COMMENT ON TABLE terminus_record IS 'plane: both';
GRANT SELECT, INSERT ON terminus_record TO tg_runtime;
