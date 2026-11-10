-- 0005 estate snapshot — the worker's live causal estate graph, PUBLISHED for the read API (REQ-516).
-- The worker builds and holds the graph (estate.Build/Holder.Refresh) and writes a snapshot after each
-- refresh; the grounder serves the LATEST to the operator console's Estate view. Latest-wins by
-- captured_at. This is a serialized graph blob, not a per-edge relational model: the console reads the
-- whole graph at once, and the prediction gate — not this table — remains the graph's authoritative
-- consumer. Append-only in practice (the worker only INSERTs); every row carries schema_version (REQ-505).
CREATE TABLE estate_snapshot (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  captured_at    timestamptz NOT NULL DEFAULT now(),
  node_count     int NOT NULL DEFAULT 0 CHECK (node_count >= 0),
  edge_count     int NOT NULL DEFAULT 0 CHECK (edge_count >= 0),
  source_count   int NOT NULL DEFAULT 0 CHECK (source_count >= 0),
  graph_json     jsonb NOT NULL DEFAULT '{}'::jsonb,
  schema_version int NOT NULL CHECK (schema_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX estate_snapshot_captured_at_idx ON estate_snapshot (captured_at DESC);
