-- 0094 down: drop the distillate-corpus chain (TG-489). Verification then reports UNINITIALIZED and
-- store-backed composition refuses until the chain is re-initialized — fail closed, never silently open.
DROP TABLE IF EXISTS skill_chain_head;
ALTER TABLE skill_version DROP COLUMN IF EXISTS chain_link;
