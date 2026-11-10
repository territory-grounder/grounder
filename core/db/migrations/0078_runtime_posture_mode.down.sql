-- 0078 down: restore the pre-TG-112 shape (the mutation_enabled binary, no mode column).
ALTER TABLE runtime_posture DROP COLUMN IF EXISTS mode;
ALTER TABLE runtime_posture RENAME COLUMN may_actuate TO mutation_enabled;
