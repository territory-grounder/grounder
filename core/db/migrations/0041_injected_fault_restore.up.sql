-- 0041 — durable RESTORE state on the injected-fault ledger (roadmap P0-5).
--
-- WHY. The fault-injection engine tracked "which guest is currently faulted, and when must it be put back"
-- in an in-process bash associative array plus a systemd timer armed INSIDE the target guest. Both are
-- volatile, and on 2026-07-26 both failed in production, stranding two live guests at 97% root disk ~80
-- minutes past their restore deadline:
--
--   PATH A (timer dies with its guest): a disk-fill armed a transient in-guest cleanup unit; the SAME engine
--   later device-downed that guest; the guest stopped, the transient unit was destroyed, and nothing re-armed
--   it. The fill was never cleaned.
--
--   PATH B (memory resets on restart): the engine died and was restarted; its in-memory BUSY_UNTIL map came
--   back empty, so it immediately re-faulted a host that still had an un-restored fault outstanding.
--
-- Both paths are the same defect: the intent to restore was never DURABLE. A restore obligation that lives
-- only in a process, or only inside the thing being broken, is not an obligation. This migration moves it to
-- the ledger so a restarted (or crashed-and-replaced) injector can reconcile: read every fault whose restore
-- is still pending and repair it, and never re-fault a host that already owes a restore.
--
-- Additive and backward compatible: existing rows get restore_state='none' (historic faults whose restore is
-- unknown/already handled), so `cmd/faultledger` and the axis-A1 denominator query are unaffected.

ALTER TABLE injected_fault
  -- 'none'      → nothing to undo (self-releasing fault, e.g. a memory hog with a built-in timeout), or a
  --               historic row from before this migration.
  -- 'pending'   → an undo IS owed. The reconciler MUST repair this and the planner MUST treat the host busy.
  -- 'restored'  → the undo completed and was verified.
  -- 'failed'    → the undo was attempted and did not succeed; needs an operator. Still counts as busy, so a
  --               failing host is quarantined from further injection rather than silently re-faulted.
  ADD COLUMN restore_state  text        NOT NULL DEFAULT 'none',
  -- When the undo is due. NULL when restore_state='none'. The reconciler repairs anything due or overdue.
  ADD COLUMN restore_due_at timestamptz,
  -- When the undo actually completed (audit: proves the estate was put back, and how late).
  ADD COLUMN restored_at    timestamptz,
  -- The handle the undo needs, per class — e.g. the fill path for disk-fill, the vmid for device-down.
  -- Without this a reconciler in a FRESH process cannot know what to undo.
  ADD COLUMN fault_ref      text        NOT NULL DEFAULT '',
  -- The Proxmox node the guest lives on. A device-down undo (`pct start`) must run on the OWNING node, and a
  -- fresh process has no other way to learn it.
  ADD COLUMN node           text        NOT NULL DEFAULT '';

ALTER TABLE injected_fault
  ADD CONSTRAINT injected_fault_restore_state_chk
  CHECK (restore_state IN ('none', 'pending', 'restored', 'failed'));

-- A pending restore MUST carry its deadline, otherwise "overdue" is not computable and the obligation is
-- unenforceable — exactly the failure this migration exists to prevent.
ALTER TABLE injected_fault
  ADD CONSTRAINT injected_fault_pending_needs_due_chk
  CHECK (restore_state <> 'pending' OR restore_due_at IS NOT NULL);

-- The reconciler's hot path: "everything still owed, soonest first". Partial index — pending/failed rows are
-- a tiny minority of a growing ledger.
CREATE INDEX injected_fault_restore_pending
  ON injected_fault (restore_due_at)
  WHERE restore_state IN ('pending', 'failed');
