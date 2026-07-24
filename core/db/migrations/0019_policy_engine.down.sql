-- Reverse 0019: drop the four policy-engine tables. Dropping a table removes every grant/revoke scoped to
-- it in one step — the REVOKE on the append-only policy_decision is created and destroyed with the table, so
-- no separate GRANT restore is needed.
DROP TABLE IF EXISTS policy_ruleset;
DROP TABLE IF EXISTS policy_graduation;
DROP TABLE IF EXISTS policy_mode;
DROP TABLE IF EXISTS policy_decision;
