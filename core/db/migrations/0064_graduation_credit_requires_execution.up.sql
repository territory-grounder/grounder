-- CREDIT MUST BE GROUNDED IN AN EXECUTION THAT ACTUALLY HAPPENED (TG-321, spec/028 REQ-2804 kin).
--
-- NUMBERED 0064, NOT 0061. It was 0061 until 2026-08-06, when a scan of the open merge requests found
-- 0061_estate_snapshot_plane already claiming that number on the TG-346 branch (!1043) — two different
-- migrations, same number, both unmerged. The runner keys schema_migrations on the FULL FILENAME and sorts
-- lexically, so both would in fact have applied in a defined order; the hazard is not a lost migration but
-- an ambiguous schema version and a directory no reader can order. Renumbered after this branch's own 0062
-- and 0063 so the sequence stays monotonic.
--
-- THE GAP. `graduation_credit` is the exactly-once claim that gates a streak increment, and a streak is how
-- an op-class earns the right to actuate WITHOUT a human vote. Both writers of that table -- GraduationActivity
-- and ReconcileActivity -- run on the TRIAGE queue, so TG-164 had to grant the triage plane INSERT on it;
-- withholding it would have broken the earn loop entirely, which is an outage rather than a hardening.
--
-- The consequence, stated in TG-321 and unchanged by anything since: a compromised triage worker cannot
-- execute an action and cannot write action_verdict/action_execution/policy_decision -- but it CAN credit an
-- op-class against incidents and promote it, and the promotion then changes what a LATER, legitimate proposal
-- is allowed to do without approval. Authority is acquired by bookkeeping.
--
-- WHY A TRIGGER AND NOT A GRANT. TG-321 offers three shapes and argues the strongest is to bind the credit to
-- EVIDENCE rather than to the identity of the writer: "the database enforces the grounding rather than the
-- plane". A grant-based fix protects only while the grants are right -- and as of 2026-08-06 they are not:
-- both planes connect as tg_runtime and the DB split is not in force at all (TG-368). A trigger holds
-- regardless of which role writes, which is the property that survives that.
--
-- WHAT IT ENFORCES, EXACTLY, AND WHAT IT DOES NOT. action_execution carries no op_class column, so the
-- available binding is on external_ref: a credit may not be written for an incident that produced NO recorded
-- execution. That kills "credit an op-class on a run that never occurred", which is the attack TG-321 names.
-- It does NOT stop crediting the WRONG op_class against an incident that did execute -- naming that limit
-- here rather than letting the trigger's name overclaim. Closing it needs op_class on action_execution, which
-- is a schema change with its own write-path work.
--
-- SAFE TO ADD NOW, AND CHEAPER THAN IT WILL EVER BE. graduation_credit holds ZERO rows in production
-- (measured 2026-08-06) -- the claim path has never run. No backfill, no historical row to grandfather, and
-- no risk of refusing a credit that some past promotion depends on. The 6 rows in policy_graduation were not
-- set through this path.
--
-- FAIL DIRECTION. A refused credit means the op-class does not climb on that run. That is the same direction
-- Claim already fails in ("unclaimable means uncredited"): the worst a lost credit does is withhold autonomy,
-- and the worst an ungrounded one does is grant it.

CREATE OR REPLACE FUNCTION graduation_credit_requires_execution() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM action_execution WHERE external_ref = NEW.external_ref) THEN
    RAISE EXCEPTION
      'graduation credit for op_class % refused: external_ref % has no action_execution row, so this credit would advance the ladder on a run that never happened (TG-321)',
      NEW.op_class, NEW.external_ref
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

-- IDEMPOTENT. schema_migrations keys on the FILENAME, so renumbering this file (0061 -> 0064, to resolve a
-- collision with 0061_estate_snapshot_plane in a separate open MR) makes it a NEW version that re-applies on
-- a database where the old name already ran. A bare CREATE TRIGGER would fail there and stop the migration
-- chain. Dropping first costs nothing on a fresh database and makes the rename safe on an existing one.
DROP TRIGGER IF EXISTS graduation_credit_grounded ON graduation_credit;
CREATE TRIGGER graduation_credit_grounded
  BEFORE INSERT ON graduation_credit
  FOR EACH ROW EXECUTE FUNCTION graduation_credit_requires_execution();

COMMENT ON FUNCTION graduation_credit_requires_execution() IS
  'TG-321: a ladder credit must name an incident that produced a recorded action_execution. Enforced in the database so it holds whichever role writes -- the credential plane split grants the triage plane INSERT here by necessity, and as of 2026-08-06 is not in force at all (TG-368).';
