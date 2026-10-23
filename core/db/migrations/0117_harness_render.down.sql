-- Revert 0117: drop the harness render record and its build lineage (TG-600). Observability only; nothing gates on it.
DROP TABLE IF EXISTS harness_render_build;
DROP TABLE IF EXISTS harness_render;
