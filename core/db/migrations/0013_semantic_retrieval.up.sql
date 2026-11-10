-- 0013 semantic retrieval — the pgvector embedding sidecar for the knowledge corpus (spec/012
-- REQ-1110/REQ-1111, TG-40 / TG-38 audit item R1: pgvector was provisioned in deploy but no migration
-- ever created a vector column, so retrieval stayed a lexical top-3).
--
-- The corpus itself stays the operator-owned JSON file (TG_KNOWLEDGE_FILE) the lexical retriever loads;
-- this table stores ONLY the per-precedent embedding keyed by external_ref plus a content hash of the
-- embedded text — a changed precedent nulls its vector (it re-embeds), a vanished one is pruned, and a
-- stale index row can never resurrect precedent the corpus no longer holds (the corpus is truth).
--
-- `embedding` is NULLABLE by design: a row without a vector is legal and the lexical channel still serves
-- its precedent — the semantic channel only ever ADDS recall, never gates it. Vectors are computed
-- best-effort by the worker's backfill loop; an embedding outage leaves rows honestly NULL.
--
-- Dimension 768 = nomic-embed-text (the default embedding model); TG_EMBED_DIM must match this column and
-- the worker refuses a mismatch at boot. Changing the dimension is a NEW migration (alter column + reindex
-- + re-embed), never an in-place edit.
--
-- CREATE EXTENSION needs privileges the DDL role may lack: migrations run under TG_MIGRATION_DSN
-- (db.Migrate), and the compose/helm postgres-init creates the extension as the superuser at first init —
-- so on an EXISTING database the extension must exist before this migration runs (one-time, as superuser:
--   CREATE EXTENSION IF NOT EXISTS vector;
-- ). With the extension present the line below is a no-op notice and the migration proceeds under the
-- normal migration role. The deploy image (pgvector/pgvector:pg16, pgvector >= 0.5) supports HNSW.
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE knowledge_embedding (
  external_ref   text PRIMARY KEY,
  content_hash   text NOT NULL,
  embedding      vector(768),
  embed_model    text NOT NULL DEFAULT '',
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Approximate cosine top-K over the embedded rows (NULL embeddings are simply absent from the index).
-- HNSW over ivfflat: no training step, sane recall on a small growing corpus, supported by the pinned
-- pgvector/pgvector:pg16 image.
CREATE INDEX knowledge_embedding_cosine_hnsw
  ON knowledge_embedding USING hnsw (embedding vector_cosine_ops);
