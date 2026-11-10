-- Revert 0129: drop the published self-identity (TG-684). The triage plane then has no TG identity for the journal
-- domain, and the journal reader fails closed (indeterminate) on any read that holds a login.
DROP TABLE IF EXISTS self_identity;
