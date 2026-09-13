-- 0013 down — remove the semantic-retrieval sidecar. Retrieval degrades to the lexical channel exactly
-- (the corpus file is untouched; embeddings are derived data and are recomputed by backfill on re-up).
DROP INDEX IF EXISTS knowledge_embedding_cosine_hnsw;
DROP TABLE IF EXISTS knowledge_embedding;
-- The vector EXTENSION is deliberately left installed: it is database-scoped, other objects may come to
-- depend on it, and dropping it is the superuser bootstrap's decision — this migration removes only what
-- it created under the migration role.
