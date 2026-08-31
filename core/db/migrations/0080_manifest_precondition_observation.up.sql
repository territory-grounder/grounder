-- 0080: action_manifest.precondition_observation — the seal-time state observation (TG-378).
--
-- The pve03 cascade sealed `start-guest` manifests for guests RUNNING with 897h/2,103h uptimes; nothing
-- between "the model proposed start" and "seal the manifest" asked whether the target was already running.
-- The prediction gate now enforces the op-class's declared state precondition (requires_target_state,
-- opschema) BEFORE sealing, and this column records the observation that satisfied it — evidence bound to
-- the seal, deliberately OUTSIDE the content-hashed `action` jsonb so no action_id moves (INV-07; the
-- 0071 additive-column precedent). '' = the class declares no precondition. Additive, behaviour-neutral
-- for every existing row.
ALTER TABLE action_manifest ADD COLUMN precondition_observation text NOT NULL DEFAULT '';
