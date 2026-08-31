-- TG-481: the estate_object_group model — named sets of host-glob / device-class PATTERNS whose membership
-- is unioned into credential.Target.Groups alongside the sync-derived membership (spec/016, the shared
-- object-group model the console editor and the policy engine both consume). Mutable operator CONFIG, not the
-- audit spine — writes are ledgered by the worker lane. `precedence` is per-group; 'union' today (a
-- hand-authored group ADDS to inventory-derived membership, never masks it).
CREATE TABLE estate_object_group (
  id         bigserial   PRIMARY KEY,
  name       text        NOT NULL CHECK (length(btrim(name)) > 0),
  patterns   text[]      NOT NULL CHECK (cardinality(patterns) > 0),
  precedence text        NOT NULL DEFAULT 'union' CHECK (precedence IN ('union')),
  created_by text        NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE estate_object_group IS 'plane: both';
