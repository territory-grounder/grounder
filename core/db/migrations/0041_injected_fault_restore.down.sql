-- Reverse 0041. Drops the durable restore-tracking columns; the ledger returns to injection-only records.
DROP INDEX IF EXISTS injected_fault_restore_pending;

ALTER TABLE injected_fault
  DROP CONSTRAINT IF EXISTS injected_fault_pending_needs_due_chk,
  DROP CONSTRAINT IF EXISTS injected_fault_restore_state_chk;

ALTER TABLE injected_fault
  DROP COLUMN IF EXISTS node,
  DROP COLUMN IF EXISTS fault_ref,
  DROP COLUMN IF EXISTS restored_at,
  DROP COLUMN IF EXISTS restore_due_at,
  DROP COLUMN IF EXISTS restore_state;
