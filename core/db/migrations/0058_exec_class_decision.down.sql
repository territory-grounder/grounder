-- 0058 down: drop the routing-decision audit trail. Nothing else referenced it — before TG-169 the
-- execution class was persisted nowhere at all, which is exactly why this table was added.
DROP TABLE IF EXISTS exec_class_decision;
