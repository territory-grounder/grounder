-- A PROMOTION MUST BE GROUNDED IN CREDIT (TG-321, the half migration 0064 did not close).
--
-- 0064 bound `graduation_credit` to evidence: a credit may not be written for an incident that produced no
-- action_execution. It left the AUTHORITY SURFACE itself unbound. `policy_graduation.level` is what decides
-- which op-classes may mutate production without a human vote, and its only writer is a blanket upsert
-- (core/db/policy_graduation_store.go Save: `ON CONFLICT (op_class) DO UPDATE SET level = EXCLUDED.level`).
-- Any role holding UPDATE can set any class to `auto` directly, skipping the credit path entirely.
--
-- MEASURED IN PRODUCTION 2026-08-07, and this is why it matters rather than being theoretical:
--
--     op_class          | level   | clean_run_count | graduation_credit rows
--     restart-service   | auto    | 0               | 0
--     start-container   | auto    | 0               | 0
--     start-service     | auto    | 0               | 0
--
-- Three op-classes hold SILENT AUTONOMY over the estate with zero recorded grounding of any kind. Those
-- rows predate the credit mechanism (0064's note: "the 6 rows in policy_graduation were not set through
-- this path"), so they are history rather than an attack — but they are exactly the end state TG-321
-- describes, reached by ordinary means, and nothing stops another one being written the same way.
--
-- WHAT THIS ENFORCES. An ADVANCEMENT up the ladder (approve -> auto_notice -> auto) requires at least one
-- graduation_credit row for that op-class. Composed with 0064 the chain is transitive: a promotion needs a
-- credit, and a credit needs an execution that actually happened. Authority can no longer be acquired by
-- bookkeeping alone.
--
-- WHAT IT DELIBERATELY DOES NOT ENFORCE, named here rather than left for a reader to discover:
--   * It does not re-implement the streak thresholds. The ladder's arithmetic lives in core/policy and
--     belongs there; duplicating "how many clean runs" in SQL would create a second source of truth that
--     drifts. The property here is GROUNDED / NOT GROUNDED, which is the part an attacker bypasses.
--   * It does not check that the credits belong to the right incidents beyond 0064's own binding.
--   * It does not touch DEMOTION, a same-level rewrite, or registering a class at `approve`. All three are
--     always allowed: the safe direction must never be blocked, Save() rewrites the whole row on every
--     counter update, and `approve` grants no autonomy at all — it routes to a human vote.
--
-- WHY THIS DOES NOT FREEZE THE LADDER, checked before writing it. graduation_credit holds ZERO rows and
-- every clean_run_count is 0, so no promotion path is currently running — this refuses nothing that works
-- today. When the earn loop is repaired (TG-348 tracks it as one of four never-closed loops) it must write
-- a credit before it promotes, which is the correct ordering rather than an obstacle. The three existing
-- `auto` rows are untouched: a trigger fires on write, and re-Saving them at the SAME level is not an
-- advancement.
--
-- FAIL DIRECTION. A refused promotion means an op-class does not gain autonomy on that run — the same
-- direction 0064 already fails in. The worst a blocked promotion does is withhold autonomy; the worst an
-- ungrounded one does is grant it.

CREATE OR REPLACE FUNCTION graduation_level_rank(lvl text) RETURNS int
LANGUAGE sql IMMUTABLE AS $$
  SELECT CASE lvl
    WHEN 'approve'     THEN 1
    WHEN 'auto_notice' THEN 2
    WHEN 'auto'        THEN 3
    ELSE 0  -- an unknown spelling ranks BELOW approve, so moving to it is never an advancement and
            -- moving away from it always is. Matches parseLevel's unknown->approve fail-closed contract.
  END;
$$;

CREATE OR REPLACE FUNCTION graduation_promotion_requires_credit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  old_rank int := 0;
BEGIN
  IF TG_OP = 'UPDATE' THEN
    old_rank := graduation_level_rank(OLD.level);
  END IF;
  -- CONSTRAINED ONLY WHEN AUTONOMY IS BEING GAINED. Two conditions, both required:
  --   (a) the NEW level grants autonomy — rank >= 2, i.e. auto_notice or auto. `approve` grants none: it
  --       routes to a human vote, and it is the fail-closed default a brand-new op-class is registered at.
  --       An earlier draft of this trigger treated INSERT-at-approve as an advancement from rank 0 and
  --       refused it; core/db's own TestTriageRoleCanStillDoEverythingTriageNeeds caught that immediately
  --       and called it correctly — "this is an OUTAGE, not a hardening". Registering a class must never
  --       need credit; only granting it autonomy must.
  --   (b) it is an ADVANCEMENT — strictly above where the class already was. A same-level rewrite (Save()
  --       upserts the whole row on every counter update) and any demotion pass untouched.
  IF graduation_level_rank(NEW.level) < 2 OR graduation_level_rank(NEW.level) <= old_rank THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM graduation_credit WHERE op_class = NEW.op_class) THEN
    RAISE EXCEPTION
      'promotion of op_class % from % to % refused: no graduation_credit row grounds it, so this would grant autonomy on bookkeeping alone (TG-321)',
      NEW.op_class, COALESCE(OLD.level, '(new)'), NEW.level
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

-- IDEMPOTENT, for the same reason 0064 is: schema_migrations keys on the FILENAME, so a rename would
-- re-apply this on a database where the old name already ran and a bare CREATE TRIGGER would fail there.
DROP TRIGGER IF EXISTS graduation_promotion_grounded ON policy_graduation;
CREATE TRIGGER graduation_promotion_grounded
  BEFORE INSERT OR UPDATE ON policy_graduation
  FOR EACH ROW EXECUTE FUNCTION graduation_promotion_requires_credit();

COMMENT ON FUNCTION graduation_promotion_requires_credit() IS
  'TG-321: advancing policy_graduation.level requires a graduation_credit row for that op-class. Enforced in the database so it holds whichever role writes — the credential plane split must grant the triage plane UPDATE here (the earn loop runs on tg.runner), and as of 2026-08-06 the DB split is not in force at all (TG-368).';
