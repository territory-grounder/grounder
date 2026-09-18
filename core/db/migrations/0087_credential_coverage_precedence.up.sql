-- TG-109: publish each credential source's PRECEDENCE into the coverage projection. The console's
-- per-target credential map needs "which source wins and why", and the read surface has been honestly
-- OMITTING precedence rather than fabricating it (core/httpapi/credentials.go) because it was worker
-- config absent from these tables. The worker now publishes the compiled rank (modules/bootstrap
-- credential.go's constant table; lower = higher precedence) alongside the coverage count. DEFAULT 0 =
-- "not yet published by a post-0087 worker" — 0 is not a valid rank (compiled ranks start at 10), so a
-- consumer can tell unpublished from ranked without inventing a value.
ALTER TABLE credential_coverage
  ADD COLUMN precedence int NOT NULL DEFAULT 0 CHECK (precedence >= 0);
