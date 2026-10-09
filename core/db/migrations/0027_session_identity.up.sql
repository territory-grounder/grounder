-- 0027: persist the session's prompt/seed/model PROVENANCE on session_triage
-- (spec/020 T-020-9, REQ-2009 — the decision tracer's per-session identity, design-wisdom observability).
--
-- The Runner composes each session from a versioned trusted preamble, a content-composed agent seed, and a
-- model tier (compose_seed.go + the investigate activity), but dropped all three at the DB boundary — so the
-- decision-tracer inspector could not show WHICH prompt version and model tier composed a given decision.
-- These three additive, defaulted, NON-SECRET columns close the gap:
--   prompt_version — the trusted-preamble template version (compile-time constant, bumped on preamble change),
--   seed_hash      — the SHA-256 of the composed agent seed. Only the HASH is persisted, never the seed TEXT:
--                    the seed embeds UNTRUSTED incident data (alert/ticket/CMDB bodies), so a content
--                    fingerprint gives the inspector "same seed / changed seed" without ever landing a secret
--                    or an injection payload in a governed row (INV-13),
--   model_tier     — the LLM tier the investigation ran on ("fast"/"primary"), non-secret.
-- Additive, defaulted, backward-compatible: pre-fix rows read as empty strings (provenance unknown), which is
-- exactly what they were. session_triage is NOT on the append-only spine (0015) — the judge cron already
-- UPDATEs it — so no grant change is needed.
ALTER TABLE session_triage
    ADD COLUMN prompt_version text NOT NULL DEFAULT '',
    ADD COLUMN seed_hash      text NOT NULL DEFAULT '',
    ADD COLUMN model_tier     text NOT NULL DEFAULT '';
