-- 0071 (TG-404) — an executed INVERSE is an execution, distinguishable from a forward action.
--
-- THE GAP. When an inverse (a compensating rollback) runs, "did the rollback run, and did it succeed?" is
-- only answerable by parsing prose out of an execution_log string (core/actuate/interceptor.go's exec-log
-- record). No column separates a forward action from its inverse, so the loop-closure register (TG-348) had
-- to EXCLUDE this loop, and TG-82's commit-confirmed semantics (arm a timer, verify, auto-revert on FAIL or
-- timeout) has nowhere durable to record what the revert did.
--
-- THE SHAPE, and why it reuses action_execution rather than a parallel table. An inverse IS an execution: it
-- has its own content-addressed action_id (the rollback argv's hash), its own fresh verdict from the same
-- deterministic verifier, and it belongs in the same append-only ledger with the same banding and audit. A
-- parallel inverse_execution table would need its own copies of all of that. So an inverse is an
-- action_execution row that additionally names the FORWARD action it undoes.
--
-- inverts_action_id: NULL for a forward action (the overwhelming majority); the forward action_id for an
-- inverse. It is a plan-adherence fingerprint reference, NOT a foreign key — action_id is content-addressed
-- over the operation alone and is deliberately non-unique here (one shape runs many times), so there is no
-- unique row to reference. "the inverse ran and succeeded" and "the inverse ran and FAILED" are opposite
-- safety outcomes and must never be the same silence; the fresh `verdict` on the same row records which,
-- exactly as it does for a forward action.
ALTER TABLE action_execution ADD COLUMN inverts_action_id text
  CHECK (inverts_action_id IS NULL OR length(btrim(inverts_action_id)) > 0);

-- Reverse lookup: "has an inverse of forward X ever run, and how did it go?" Partial — forward rows (the
-- majority) carry NULL and are not indexed.
CREATE INDEX action_execution_inverts ON action_execution (inverts_action_id)
  WHERE inverts_action_id IS NOT NULL;
