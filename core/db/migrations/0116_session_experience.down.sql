-- Revert 0116: drop the experience reflector's record (TG-579). Observability + flywheel input only.
DROP TABLE IF EXISTS session_experience;
