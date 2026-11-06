-- 0045_session_admin_eligible (REQ-522/REQ-2014): the ROLE a session carries must survive a restart, exactly
-- as the session itself already does.
--
-- operator_sessions was made durable (migration 0006) for a stated reason: "browser operator sessions persist
-- across grounder restarts/redeploys, so a valid cookie keeps working instead of forcing a re-login on every
-- deploy". The LDAP-admin eligibility that grants the elevated trace-read and admin-write tiers was left in a
-- process-local map, so every restart emptied it while leaving every cookie valid. The operator stayed logged
-- in, the console rendered normally, and one elevated surface began refusing them with no signal.
--
-- Observed live 2026-07-29: the grounder restarted at 00:21:56Z on a routine deploy; the owner's 403s on
-- /v1/sessions/{ref} began at 00:21:58Z; no re-authentication in the following six hours. A fresh login on the
-- same account returned 200 on the same endpoint. Every deploy silently downgraded every logged-in operator.
--
-- Additive and defaulted FALSE, so existing rows and any writer that does not yet set it keep working and are
-- treated as NOT eligible — fail closed: a session whose role we cannot establish gets the read-only console,
-- never the elevated tier.
ALTER TABLE operator_sessions
    ADD COLUMN admin_eligible boolean NOT NULL DEFAULT false;
