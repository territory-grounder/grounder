-- 0116: session_experience — the EXPERIENCE REFLECTOR's durable record (TG-579, Harness² learnings H2-5).
--
-- After a remediation that did NOT verify as a confirmed-clean resolution, one model session reads the whole
-- record (trajectory, action, committed prediction, mechanical verdict) and writes at most ONE transferable
-- lesson {cue, lesson, evidence, confidence} plus its per-decision rows (moments). The row feeds the skill
-- flywheel's LESSON source (core/db.ExperienceLessonSource, joined to the escalation feed when the lesson
-- flywheel is armed) — never the agent's own prompt (the <caution> block keeps its factual template).
-- One row per session (external_ref PK; first write wins). NON-SECRET by construction: every field is
-- input-screened on the way in (core/reflect.Parse refuses the whole entry on a screen hit) and length-capped.
-- Confidence is a closed vocabulary. tg_runtime writes and reads; nothing updates or deletes a row.
CREATE TABLE session_experience (
    external_ref text PRIMARY KEY,
    host         text NOT NULL DEFAULT '',
    alert_rule   text NOT NULL DEFAULT '',
    verdict      text NOT NULL DEFAULT '',
    cue          text NOT NULL,
    lesson       text NOT NULL,
    evidence     text NOT NULL,
    confidence   text NOT NULL CHECK (confidence IN ('high', 'medium', 'low')),
    moments      text NOT NULL DEFAULT '',
    model        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX session_experience_created_idx ON session_experience (created_at DESC);
-- Triage-plane CONTENT (plane_roles.go TriageContentTables): written by the triage worker's reconcile terminus
-- (the reflector), read by the skill flywheel; the actuation plane never writes it.
COMMENT ON TABLE session_experience IS 'plane: triage';
GRANT SELECT, INSERT ON session_experience TO tg_runtime;
