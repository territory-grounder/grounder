# ADR 0003 — State store: PostgreSQL + pgvector

## Status
Accepted.

## Context
The predecessor kept all state in SQLite reached through three aliased paths (no proof of one file), with runtime `ALTER`/`CREATE TABLE IF NOT EXISTS` inside request handling, 48 tables with 5 FKs, FK enforcement off, embeddings stored inline as TEXT, and a separate FAISS index for vectors (M-03, P2-1/2) [O][F]. This made orphaned rows and silent migration failures undetectable, and — fatally for the product reframe — every table assumed a single estate keyed by a bare `issue_id` [F]. TG is **multi-tenant by default** [R paradigm-rule 1]: tenant ids collide, correlation generalizes to `(tenant_id, external_ref)`, and no query may cross a tenant boundary.

## Decision
Adopt **one PostgreSQL database, one DSN, pgvector for embeddings** [R corrections/stack], replacing SQLite + FAISS.
- **Row-Level Security (RLS)** + `NOT NULL` FK `(tenant_id, session_id)` on every state/memory/audit/eval/cost/graph table enforces tenant isolation at the database (INV-12) [O][R rule 1].
- Schema evolves **only via ordered transactional migrations** (golang-migrate/goose) under an advisory lock at deploy/startup, **never** inside request handling; the runtime role has **DML only, no DDL** (INV-16) [O].
- Integrity is by construction: FKs, `NOT NULL`, `CHECK` (e.g. `verdict IN ('match','partial','deviation')`), and enums (INV-16) [O].
- Persistence is **exclusively parameterized** via `pgx`/`sqlc`-generated queries; no string-built SQL, no quote-doubling helper; shells never touch the DB (INV-03) [O].
- **Two stores, drawn explicitly** [R rule 5 / tension]: (a) a compact tamper-evident **audit spine** — `session_risk_audit`, the SHA-256 hash-chained `governance_ledger`, the plan_hash-keyed prediction log — is append-only and retained by integrity-preserving archival/sealing (INV-19) [O]; (b) the **purgeable operational body** — transcripts, diaries, `incident_knowledge`, wiki, embeddings, event/tool streams — honors per-tenant TTL + right-to-erasure, every row `NOT NULL expires_at` (INV-14) [O].

## Consequences
- "Memory never shrinks / indefinite core KB" [F] is **dropped** as a homelab assumption [R rule 5]; only de-identified audit facts are immutable; raw PII-bearing memory is purgeable.
- One DSN + `schema_version` stamping per writer (readers reject future versions) [F] gives forward-incompatible-safe evolution.
- pgvector unifies the 5-signal RRF retrieval [F] onto one engine, RLS-scoped per tenant.

## Alternatives
- **Keep SQLite + FAISS** [F] — rejected [R rule 7]: no RLS, no real FKs, aliased-file ambiguity (M-03) [O].
- **Separate vector DB (Milvus/Qdrant)** — rejected: a second store to isolate per tenant and keep consistent; pgvector keeps one DSN and one RLS boundary. (Predecessor Milvus was VRAM-stealing and unused by its own RAG.)
