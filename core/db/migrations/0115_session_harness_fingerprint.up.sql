-- 0115: persist the session's HARNESS identity on session_triage (TG-575, Harness² learnings H2-1;
-- spec/020 REQ-2009 lineage).
--
-- prompt_version (0027) versions only the trusted seed preamble and seed_hash covers only the user seed, which
-- embeds incident data — so nothing on the row said WHICH CONFIGURATION composed a session: the protocol prose,
-- the tool catalog, the op-class catalog (and its ratified runtime overlay), the store-composed guidance half
-- and the model tiers all changed without a trace. harness_fp is the SHA-256 of exactly that system half bound
-- to the preamble version; it carries NO incident data, so equal values mean "the same configuration". The
-- flywheel's regression trigger and the eval arms can now refuse to pool sessions across a configuration change
-- (Harness²: a rollout from a different harness is never reused as evidence). Additive, defaulted, NON-SECRET;
-- pre-fix rows read as '' (unrecorded). session_triage is not on the append-only spine, so no grant change.
ALTER TABLE session_triage
    ADD COLUMN harness_fp text NOT NULL DEFAULT '';
