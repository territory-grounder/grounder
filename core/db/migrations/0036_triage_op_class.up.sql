-- 0036: carry the canonical op_class into the durable triage record (axis A5, docs/BENCHMARK-AXES.md —
-- fault-class / remediation-op breadth).
--
-- session_triage persists only the free-text `op` verb (the proposal's Action.Op, e.g. "restart", "start",
-- "grow"), never the canonical Action.OpClass ("restart-service", "start-guest", "disk-grow"). The op_class
-- is the correct unit for A5 breadth: the raw verb is ambiguous ("restart" is restart-service OR reboot) and
-- some rows carry a verbose phrase rather than a token, so a distinct-`op` count both under- and over-counts
-- the true op-class breadth. The proposal already carries Action.OpClass (core/proposal); the Runner discards
-- it at the DB boundary. This persists it so the live-axis scorer (cmd/axisscore) measures A5 faithfully.
--
-- OBSERVABILITY ONLY — op_class is a persisted decision RECORD, it does not re-enter any gate. Additive,
-- defaulted, backward-compatible: pre-migration rows read as '' (op_class unknown), and op_class is NOT
-- derivable from the stored verb, so no backfill is possible — the faithful A5 count accrues forward.
ALTER TABLE session_triage
    ADD COLUMN op_class text NOT NULL DEFAULT '';
