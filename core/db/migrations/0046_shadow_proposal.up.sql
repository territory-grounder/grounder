-- THE OPEN PROPOSAL PLANE'S DURABLE RECORD (spec/026 REQ-2604, epic TG-227 plane 1).
--
-- With an EMPTY op-class catalog TG now proposes instead of standing down (the "stand-down generator"
-- conversion): a free-form proposal that matches no registered op-class diverts to a SHADOW branch —
-- recorded, ledgered, rendered, never executable. This migration is the record half:
--
--   * undo_sketch — the model's free-text reversal sketch, the grammar's ONE additive field
--     (spec/002 amendment 2026-07-31). Untrusted DATA (INV-08), screen.Scrub'd before persist;
--     rendered in the console proposals lane; deliberately NOT rendered into the byte-pinned judge
--     prompt in v1 (ADR-0016 OQ-7).
--   * outcome vocabulary gains 'proposed:shadow' — a session that proposed a free-form action and
--     terminated in the shadow lane (no notify, no pending projection, no vote). The column is free
--     text by design (no CHECK), like 'proposed' / 'no-proposal:stop' / 'already-remediated' before it.
--
-- Actor-evidence adds NO column here: structured actor evidence has lived on session_triage since
-- migration 0035 (actor_evidence jsonb, []attribution.Evidence) and the shadow row REUSES it
-- (REQ-2610). Additive and defaulted: every existing row and every pre-field writer stays valid.
ALTER TABLE session_triage ADD COLUMN IF NOT EXISTS undo_sketch text NOT NULL DEFAULT '';

COMMENT ON COLUMN session_triage.undo_sketch IS
  'Model''s free-text reversal sketch for the proposed action (spec/026 REQ-2604). Untrusted DATA, screened before persist; empty when the model offered none. Never part of the content-hashed action identity (INV-07).';
