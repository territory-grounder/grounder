-- 0080 down: drop the seal-time precondition observation (TG-378). Manifests stop recording what
-- satisfied a state precondition — the pre-0080 shape.
ALTER TABLE action_manifest DROP COLUMN IF EXISTS precondition_observation;
