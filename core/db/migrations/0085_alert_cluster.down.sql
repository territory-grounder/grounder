-- 0085 down: drop the durable cluster identity and the routing-decision columns it added (TG-385/TG-376).
ALTER TABLE exec_class_decision DROP COLUMN IF EXISTS elect_rule;
ALTER TABLE exec_class_decision DROP COLUMN IF EXISTS runner_up_ref;
ALTER TABLE exec_class_decision DROP COLUMN IF EXISTS elected_ref;
ALTER TABLE exec_class_decision DROP COLUMN IF EXISTS cluster_id;
DROP TABLE IF EXISTS alert_cluster;
