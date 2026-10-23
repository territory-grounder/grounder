-- 0077 down: drop the co-occurrence learner snapshot (TG-388 face c). The learner falls back to its
-- in-memory-only lifetime — a restart re-learns from zero, the pre-0077 behaviour.
DROP TABLE IF EXISTS co_occurrence_host;
DROP TABLE IF EXISTS co_occurrence;
