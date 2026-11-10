-- 0035_actor_attribution (spec/023 REQ-2311): the WHO-CAUSED-THIS dimension on the triage record.
-- actor_attribution is the resolved taxonomy value ('' = a pre-feature row / unattributable — the
-- zero/unknown convention, never a permissive default); actor_evidence is the minimized, redacted
-- reader-captured evidence blob ([]attribution.Evidence — actor, verb, timestamp, ref; never raw log
-- lines). Both are additive + defaulted, so existing rows and the pre-feature insert are untouched.
ALTER TABLE session_triage
    ADD COLUMN actor_attribution text  NOT NULL DEFAULT '',
    ADD COLUMN actor_evidence    jsonb NOT NULL DEFAULT '[]';
