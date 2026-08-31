-- Revert 0114: remove the tg_runtime grant on actuation_target_state (back to owner-only, the pre-0114 state
-- that caused TG-554). tg_actuate/tg_triage grants are boot-derived from tg_runtime, so revoking the base is
-- sufficient; the next tg_apply_plane_grants run reconciles the mirror.
REVOKE SELECT, INSERT, UPDATE ON actuation_target_state FROM tg_runtime;
