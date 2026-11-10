-- 0123: the INDETERMINATE per-run disposition (TG-663, G4A finding F4).
--
-- WHAT WAS WRONG. core/actuate mapped EVERY effect-leaf Exec error to a refusal, and the Runner read a refusal as
-- "the forward effect provably did not execute" and signalled the armed commit-confirm window to ABORT. But an
-- SSH command whose channel died after it was launched, a Proxmox task the API accepted whose status poll failed,
-- a running command the caller cancelled — those requests ENTERED the transport, and the effect may have landed.
-- The one guard covering a possibly-live mutation was stood down on a lost acknowledgement.
--
-- WHAT THIS ADDS. A fourth value of the `verdict` enum, `indeterminate`, written ONLY by the interceptor's
-- indeterminate branch (core/db ActionExecutionStore.RecordIndeterminate) with unverifiable=false (the 0043 pairing
-- CHECK holds: a verdict is present). It is a per-run DISPOSITION, not a core/safety mechanical verdict: the
-- verifier never produces it, the graduation ladder never credits it, and the commit-confirm consult reads it as
-- "re-read the object at elapse; hold if it cannot be confirmed healthy" — never as confirmed, never as aborted.
-- Readers that switch on match/partial/deviation treat it as neither (every existing default branch is
-- fail-closed: unverifiable/hold). Existing rows are untouched.
--
-- ALTER TYPE ... ADD VALUE is permitted inside a transaction on PostgreSQL 12+ (the new value is not USED in this
-- same transaction — the writer is application code, later). The fixture harness applies this file transactionally.
ALTER TYPE verdict ADD VALUE IF NOT EXISTS 'indeterminate';

COMMENT ON COLUMN action_execution.verdict IS
    'the fresh mechanical verdict for THIS execution (match/partial/deviation); NULL when the post-state was unobservable (unverifiable=true); indeterminate (TG-663) when the acknowledgement was lost after the request entered the transport — the effect may or may not have landed, never a verified outcome';
