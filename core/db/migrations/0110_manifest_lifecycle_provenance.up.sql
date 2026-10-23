-- 0110 — WHOSE session do a sealed manifest's lifecycle labels describe? (TG-532)
--
-- THE DEFECT, measured on production 2026-08-22. action_id is content-addressed over the operation SHAPE
-- (INV-07), and action_manifest is sealed first-wins (ON CONFLICT (action_id) DO NOTHING). So every session
-- that proposes the SAME operation shares ONE manifest row: 69 action_ids on this deployment are shared by
-- more than one session, the worst by 198 distinct sessions. The two non-hashed LIFECYCLE columns —
-- approval_choice (a human's vote) and verdict (one run's mechanical outcome) — are per-SESSION facts written
-- onto that per-SHAPE row, last-write-wins. A reader (the #actions ribbon, the decision tracer) therefore sees
-- "approved / match" against a session that was never approved and never executed: row fd92a9b1… was sealed
-- 2026-07-24 and still carries approved/match, which is what a 2026-08-22 session's reviewer saw.
--
-- The seal is right and stays: an action's identity IS its shape. What was wrong is a label that answers
-- "which session?" while carrying no session. These two columns record that, so a lifecycle label is
-- self-describing and a reader can tell a shared shape's history from this session's outcome. '' = the label
-- predates this migration (its owning session is unrecoverable) or no label is set — honest absence, never a
-- guess.
ALTER TABLE action_manifest ADD COLUMN approval_ref text NOT NULL DEFAULT '';
ALTER TABLE action_manifest ADD COLUMN verdict_ref  text NOT NULL DEFAULT '';
