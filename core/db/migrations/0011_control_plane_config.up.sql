-- 0011 control_plane_config — console-written configuration overrides (task #27 Phase C, REQ-523).
-- The signed-off design: the layered resolver honors a row here ONLY for a console-writable non-LAW
-- registry key (the clamp is structural in Go — a LAW key never reads this table), and every write is
-- appended to the hash-chained governance ledger BEFORE the row commits (single writer: the worker's
-- config-write workflow). One row per key = the CURRENT override; history lives on the ledger.
CREATE TABLE control_plane_config (
  key            text PRIMARY KEY CHECK (length(btrim(key)) > 0),
  value          text NOT NULL CHECK (length(value) BETWEEN 1 AND 2048),
  -- rationale: mandatory — every override states why it exists (mirrors the skill-store discipline).
  rationale      text NOT NULL CHECK (length(btrim(rationale)) > 0),
  updated_by     text NOT NULL,
  ledger_seq     bigint,
  schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  updated_at     timestamptz NOT NULL DEFAULT now()
);
