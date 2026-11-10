-- 0131: judge provenance on session_judgment (TG-670, F18).
--
-- A judgment row said which RUBRIC graded it (0052) and nothing about the INSTRUMENT: the judge's sampling
-- parameters were declared in rubric.json and never sent, and the gateway's reported model (the alias resolved
-- to its upstream) was never read. judge_model is the provider-reported model the row was graded by
-- ('deterministic' for the Go-scored axes; '' = unrecorded: a pre-0131 row, or a completer that offers no
-- reported model). judge_sampling is what the judge SENT ('temperature=0 seed=0'; '' = nothing sent).
-- Additive, defaulted, NON-SECRET; session_judgment already sits on the append-only spine, its INSERT and UPDATE
-- grants are unchanged (WriteJudgment upserts).
ALTER TABLE session_judgment
    ADD COLUMN judge_model    text NOT NULL DEFAULT '',
    ADD COLUMN judge_sampling text NOT NULL DEFAULT '';
COMMENT ON COLUMN session_judgment.judge_model IS
  'TG-670: the gateway-reported model that produced this score (alias resolved to upstream); deterministic for the Go-scored axes; empty = unrecorded';
COMMENT ON COLUMN session_judgment.judge_sampling IS
  'TG-670: the sampling parameters the judge SENT for this score (temperature/seed); empty = none sent';
