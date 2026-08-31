-- 0082: stamp the self-dependency DEGRADED-CAPABILITY set on the durable session record (TG-394 slice 3, part 4).
--
-- TG-394 exists because TG held NO live signal that one of its OWN dependency capabilities was degraded. During
-- the 2026-08-06 pve03 cascade its only embedding backend went unreachable and retrieval silently ran
-- lexical-only for 11h12m, with nothing reporting a reduced capability. Slice 3 publishes that live signal
-- (tg_capability_degraded); this column makes it DURABLE on each session record, so a lexical-only investigation
-- is LEGIBLE AFTERWARDS. The judge and any post-hoc analysis run HOURS later, when the estate graph has
-- recovered and a degraded-retrieval session is otherwise indistinguishable from an ordinary one — the reason
-- retrieval was weaker has to be written onto the record at session time or it is lost.
--
-- A CONTROLLED, NON-SECRET VOCABULARY: the values are capability names (embed / journal-evidence / secrets /
-- tracker / notify), never a host, an argv, or a credential. text[] mirrors evidence_ids (a small string set).
--
-- NULLABLE ON PURPOSE — the backward-compatible default, the same discipline as diagnosis (0056). NULL means
-- "this session predates the column"; an explicit empty array '{}' means "checked, nothing degraded". Those are
-- two different facts, and the writer (core/db.RecordTriage) ALWAYS writes the array non-null so NULL only ever
-- marks a pre-feature row. Additive and defaulted: every existing row and every pre-field writer stays valid,
-- and — like step_count/diagnosis before it — this needs no GRANT (session_triage's table privileges already
-- cover new columns) and no schema_version bump (the row shape stays readable to older readers, which do not
-- select this column). OBSERVABILITY ONLY: the set gates nothing and releases nothing (INV-08).
ALTER TABLE session_triage ADD COLUMN IF NOT EXISTS degraded_capabilities text[];

COMMENT ON COLUMN session_triage.degraded_capabilities IS
  'TG-394 slice 3: the self-dependency capabilities (embed / journal-evidence / secrets / tracker / notify) that were DEGRADED when this session ran — any with a backing host that had no fresh edge in the estate graph — so a lexical-only investigation is legible afterwards. Controlled non-secret vocabulary (capability names). NULL = the session predates this column; ''{}'' = checked, nothing degraded. Observability only — it gates nothing (INV-08).';
