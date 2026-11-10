-- 0135: the mode RAISE guard and the breaker RESET guard (TG-661, deep-review F2 / pack item 11).
--
-- Measured live 2026-11-07 (information_schema.role_table_grants): tg_runtime, tg_triage AND tg_actuate hold
-- UPDATE on policy_mode and mutation_breaker_state. A plain UPDATE on policy_mode is adopted by every worker
-- within ~10 s (the 10 s resync) with NO ledger row, bypassing the ModeTransition path and its authority check;
-- a plain UPDATE on the mutation breaker un-trips the estate-wide safety kill the same way. The runtime roles
-- NEED those grants for the legitimate writes — the mode controller lowers the mode on a safety kill from any
-- plane, and a deviation trip opens the breaker from any plane — so the grants stay. What changes is the
-- DIRECTION: lowering is open to every plane (the kill path must never depend on an extra credential), RAISING
-- the mode to an actuating one and RE-ARMING the breaker require the dedicated role tg_mode_admin (or
-- tg_migration / a superuser).
--
-- tg_mode_admin is a NOLOGIN group role. The ModeTransition activity (worker-1) reaches it through a dyndb lease
-- (TG_MODE_ADMIN_DSN=dyn:tg_mode_admin; the OpenBao database role GRANTs tg_mode_admin TO the leased login), so
-- the raise is possible only from the process that runs the audited transition, through a short-lived login no
-- other plane holds. The role itself is created OUTSIDE this migration — tg_migration lacks CREATEROLE — by
-- deploy/postgres-init/00-roles.sh on a fresh install, and by hand on an existing one (see the README recipe);
-- every reference below is CONDITIONAL on its existence so CI's fixture (tg_runtime only) still migrates, and a
-- deployment that never created the role simply has NO raise path until it does (fail closed, logged at boot).
--
-- Chosen over the "raise only when a matching mode:transition ledger row exists" alternative because every
-- runtime role holds INSERT on governance_ledger: a forged ledger row is one statement away for the very roles
-- this guard exists to bound, and a trigger cannot verify the TG-660 HMAC without holding the key.

CREATE OR REPLACE FUNCTION tg_policy_mode_raise_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  privileged boolean;
BEGIN
  -- Lowering (Shadow, HITL = the non-actuating modes) and a no-op rewrite are open to every plane: the safety
  -- kill and the resync re-persist must never wait on a credential.
  IF NEW.mode IN ('Shadow', 'HITL') THEN
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.mode = NEW.mode THEN
    RETURN NEW;
  END IF;
  SELECT (current_user = 'tg_migration')
      OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND rolsuper)
      OR (EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_mode_admin')
          AND pg_has_role(current_user, 'tg_mode_admin', 'MEMBER'))
    INTO privileged;
  IF NOT privileged THEN
    RAISE EXCEPTION 'policy_mode_raise_guard: raising the autonomy mode to % requires tg_mode_admin (TG-661); % may only lower it',
      NEW.mode, current_user
      USING ERRCODE = '42501';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS policy_mode_raise_guard ON policy_mode;
CREATE TRIGGER policy_mode_raise_guard
  BEFORE INSERT OR UPDATE ON policy_mode
  FOR EACH ROW EXECUTE FUNCTION tg_policy_mode_raise_guard();

-- The breaker: any plane may TRIP it (-> open) or accrue failures; only the privileged set may move a tripped
-- breaker OUT of open (to closed OR to half_open — gate 4e reads half_open as "not tripped", so admitting
-- open -> half_open would be the same un-trip by another name), close a half-open one, lower its failure count,
-- or RENAME the row (a rename would move it out from under the WHEN filter and let a fresh 'mutation' row be
-- inserted; DELETE is granted to no runtime role, so with renames guarded the row cannot vanish, and an INSERT —
-- construction on a fresh install — can never replace an existing row: the primary key makes it the UPDATE
-- path, which this trigger sees). Scoped to the safety breaker row ('mutation', the name
-- core/safety.NewMutationBreaker registers — which only ever RecordFailure()s and Reset()s it, never half-opens
-- it): the model/judge breakers in the same table recover automatically through half_open -> closed under the
-- runtime role, and that recovery is theirs to keep.
CREATE OR REPLACE FUNCTION tg_breaker_reset_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  privileged boolean;
BEGIN
  IF OLD.name = NEW.name
     AND NOT (OLD.state = 'open' AND NEW.state <> 'open')
     AND NOT (OLD.state = 'half_open' AND NEW.state = 'closed')
     AND NOT (NEW.failure_count < OLD.failure_count) THEN
    RETURN NEW; -- a trip, an accrual, a no-op rewrite: open to every plane
  END IF;
  SELECT (current_user = 'tg_migration')
      OR EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND rolsuper)
      OR (EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_mode_admin')
          AND pg_has_role(current_user, 'tg_mode_admin', 'MEMBER'))
    INTO privileged;
  IF NOT privileged THEN
    RAISE EXCEPTION 'breaker_reset_guard: re-arming or renaming the safety breaker % (% % -> % %) requires tg_mode_admin (TG-661); % may only trip it',
      OLD.name, OLD.state, OLD.failure_count, NEW.state, NEW.failure_count, current_user
      USING ERRCODE = '42501';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS breaker_reset_guard ON mutation_breaker_state;
CREATE TRIGGER breaker_reset_guard
  BEFORE UPDATE ON mutation_breaker_state
  FOR EACH ROW WHEN (OLD.name = 'mutation' OR NEW.name = 'mutation') EXECUTE FUNCTION tg_breaker_reset_guard();

-- The admin role's own table privileges, granted only where the role exists (CI's fixture has no such role and
-- must still migrate). It also reads: the controller re-reads the mode after it persists.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tg_mode_admin') THEN
    GRANT USAGE ON SCHEMA public TO tg_mode_admin;
    GRANT SELECT, INSERT, UPDATE ON policy_mode TO tg_mode_admin;
    GRANT SELECT, INSERT, UPDATE ON mutation_breaker_state TO tg_mode_admin;
  END IF;
END
$$;
