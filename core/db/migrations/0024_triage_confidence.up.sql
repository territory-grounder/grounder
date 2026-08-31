-- 0024: carry the agent's emitted confidence scalar into the durable triage record
-- (spec/020 T-020-1, REQ-2003 — the SAFE observability half of the decision-tracer keystone).
--
-- The proposal grammar parses a 0..1 confidence (core/proposal) that the Runner discards at the DB
-- boundary; the decision tracer and confidence calibration both need it persisted. This is OBSERVABILITY
-- ONLY: it does NOT thread confidence into the actuation-path policy min_confidence clamp (that clamp reads
-- r.Confidence at the interceptor and is fed a hardwired 0 at the execute build site — a separate reviewed
-- change). Additive, defaulted, backward-compatible: pre-fix rows read as 0.0 (confidence unknown), exactly
-- what they were.
ALTER TABLE session_triage
    ADD COLUMN confidence double precision NOT NULL DEFAULT 0;
