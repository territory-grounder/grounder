-- 0078: runtime_posture speaks the 4-mode model (TG-112) — mutation_enabled becomes may_actuate, plus mode.
--
-- The old column name survived from the retired MUTATIONS=ON/OFF switch (TG_MUTATION_ENABLED, absorbed into
-- the mode chokepoint by spec/015 REQ-1520/1521). The VALUE was already the derived signal — the worker has
-- published chokepoint.MayActuate() into it since the absorption — so this RENAMES the column to what the
-- value has meant all along, and adds the owner-set mode itself (Shadow / HITL / Semi-auto / Full-auto) so
-- the read-only grounder can render the real posture instead of a binary that implies a switch which no
-- longer exists. '' = mode not yet published (first boot heartbeat precedes mode binding); the reader
-- treats '' as unknown, never invents a mode.
ALTER TABLE runtime_posture RENAME COLUMN mutation_enabled TO may_actuate;
ALTER TABLE runtime_posture ADD COLUMN mode text NOT NULL DEFAULT '';
