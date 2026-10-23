-- JUDGMENT ATTRIBUTION (TG-194 + TG-195): every LLM judgment becomes attributable to the RUBRIC that
-- produced it and to the ACTION the judged session executed.
--
-- TG-194: the rubric has already changed once (the hollow-proposal rule, 2026-07-18) with nothing
-- recording which rows were scored under which wording — so every pooled mean silently mixed
-- incomparable scores, and "graded against which rubric?" had no answer. rubric_version carries the
-- declared version from core/judge/rubric.json; a bump-enforcement test pins version to content, so an
-- un-bumped rubric edit cannot ship.
--
-- '' means "judged before versioning existed — rubric unknown". It is NOT backfilled with a guess: the
-- 14,923 existing rows span at least five instrumentation epochs and there is no deploy history to
-- attribute them from (the 0027/0035 convention: the empty sentinel over a backdated assertion).
ALTER TABLE session_judgment ADD COLUMN rubric_version text NOT NULL DEFAULT '';

-- TG-195: the mechanical verdict is action-bound (action_manifest.verdict) and the LLM judgment was not
-- — the schema root of the disjoint-verdict-surface defect. action_id binds the judgment to the ONE
-- action its session sealed, '' when the session sealed none (a triage that acted on nothing has no
-- action to bind — that '' is the true value, not a gap) or when it sealed several (refuse to guess).
ALTER TABLE session_judgment ADD COLUMN action_id text NOT NULL DEFAULT '';

-- Backfill where DERIVABLE, exactly as TG-195 orders. session_risk_audit is the baseline spine with
-- full historical depth carrying both keys; the guard admits only sessions whose audit rows name
-- exactly ONE distinct action — anything else stays '', because a backfill that picks among several
-- actions would be an inference wearing a fact's costume. (First SQL backfill in this repo's migration
-- history; admitted because the derivation is exact, not estimated. Live check 2026-08-03: every judged
-- session with any action has exactly one.)
UPDATE session_judgment j
SET action_id = one.action_id
FROM (
  SELECT external_ref, min(action_id) AS action_id
  FROM session_risk_audit
  WHERE action_id <> ''
  GROUP BY external_ref
  HAVING count(DISTINCT action_id) = 1
) one
WHERE one.external_ref = j.external_ref;

-- The poolers partition by rubric_version (means combine one version only; counts stay version-blind).
CREATE INDEX session_judgment_rubric_version ON session_judgment (rubric_version, dimension);
-- The A3 attribution join.
CREATE INDEX session_judgment_action ON session_judgment (action_id) WHERE action_id <> '';

COMMENT ON COLUMN session_judgment.rubric_version IS
  'Declared version of core/judge/rubric.json that produced this score (TG-194). Empty = judged before versioning; pooled means must never mix versions.';
COMMENT ON COLUMN session_judgment.action_id IS
  'The ONE action the judged session sealed (TG-195), via session_risk_audit at write time; empty = the session sealed zero actions, or more than one (refuse to guess).';
