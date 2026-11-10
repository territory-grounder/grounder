-- 0014: carry the committed machine prediction into the durable triage record (TG-61 seq B DB layer).
--
-- seq B (!290) added judge.TriageRow.Prediction/Predicted plus the Runner->TriageRow workflow plumbing,
-- but the pgx session_triage INSERT/SELECT never carried the two fields, so the durable round-trip
-- silently DROPPED the committed prediction and the judge cron kept scoring falsifiable_prediction BLIND
-- even for proposing sessions (the in-memory fake sink hid the gap -- the pgx path is compose-only). These
-- two additive, defaulted columns close the persistence gap. Backward-compatible: existing rows read as an
-- empty (no) prediction -- exactly what they were, since every pre-fix row was scored blind anyway.
ALTER TABLE session_triage
    ADD COLUMN prediction text    NOT NULL DEFAULT '',
    ADD COLUMN predicted  boolean NOT NULL DEFAULT false;
