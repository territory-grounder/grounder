-- TG-490 fix (fresh-eyes finding #1): TWO-PHASE filing (reserve → create → complete). The row is
-- now RESERVED (issue_id = '') BEFORE the tracker create fires, so the anti-join removes the
-- incident from the work list the moment the attempt starts — a crash between create and
-- complete leaves a VISIBLE reserved row (never a blind re-create minting an orphan ticket),
-- which the resolver settles by a project-scoped search for the incident key every ticket body
-- carries (adopt what exists, else create). Ticket creation against a remote tracker cannot be
-- exactly-once; this makes the window OBSERVABLE and SELF-HEALING. Shipped as 0098 rather than
-- editing 0097: the migration ledger is append-only even for a migration no durable database has
-- applied yet (deploys are held behind the T-029 train).
ALTER TABLE tracker_entry ALTER COLUMN issue_id SET DEFAULT '';
COMMENT ON COLUMN tracker_entry.issue_id IS $$'' = RESERVED (create in flight or crashed); else the tracker's readable id$$;
