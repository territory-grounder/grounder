-- 0081: relabel edge_disproof.decayed_to as a DECAY-TIME SNAPSHOT, not the live confidence (TG-444).
--
-- 0075 documented this column as "the edge confidence AFTER this pass's decay" — which reads as the edge's
-- CURRENT confidence. Since TG-388 that is false: the graph-side decay (oldConfidence * factor) is
-- TRANSIENT — the 5-minute estate refresh REBUILDS every learned edge's confidence from the SOURCE counts
-- (LaplaceConfidence over the learner-decayed counts), a different formula AND magnitude. So decayed_to is
-- an audit record of the decay EVENT at the instant it happened, NOT a value any reader may trust as the
-- edge's live confidence. No consumer reads it as live today (verified: one writer, one store, zero live
-- readers); this closes the label before one does. Comment-only — no data or shape change.
COMMENT ON COLUMN edge_disproof.decayed_to IS
  'TG-444: decay-time SNAPSHOT (oldConfidence*factor at this disproof pass), NOT the edge''s live confidence — the 5-minute source recompute (TG-388, LaplaceConfidence over learner-decayed counts) supersedes it. Audit of the decay event, never read as current.';
