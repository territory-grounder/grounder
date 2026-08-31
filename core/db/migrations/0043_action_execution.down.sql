-- Reverse 0043. Per-execution outcomes are no longer recorded; only the FIRST execution of each action shape
-- leaves a durable verdict (in action_verdict), and "N independent heals of class X" becomes unrecordable
-- again — see the up-migration.
DROP INDEX IF EXISTS action_execution_ref;
DROP INDEX IF EXISTS action_execution_time;
DROP INDEX IF EXISTS action_execution_action;
DROP TABLE IF EXISTS action_execution;
