-- Reverting restores 0059's unconditional REVOKE arm, which aborts the whole derivation on any table the
-- caller cannot REVOKE on. Left as a no-op deliberately: the down path would reintroduce a live outage of
-- the plane split rather than undo a schema change. 0059's definition is in git if it is ever wanted.
SELECT 1;
