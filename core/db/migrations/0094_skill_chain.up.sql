-- 0094: TG-489 — tamper-evident hash chain over the distillate corpus (owner ruling TG-488 B24 / TG-146 S6).
--
-- skill_version gains chain_link: a sha256 link binding each row's creation-immutable facts
-- (id, skill_name, version, content hash over body+predicate, author, source, parent) to the entire
-- prior corpus history. Links are computed in Go (core/skillstore/chain.go) — never in SQL — so the
-- digest algorithm lives in exactly one place, and they are BACKFILLED by EnsureChain at worker boot,
-- not here. Until EnsureChain runs, verification reports UNINITIALIZED and store-backed composition
-- refuses (fail closed; the compiled registry composes in full, visibly).
--
-- skill_chain_head is the singleton head row: the latest link and the row count, updated in the same
-- transaction as every append. Its FOR UPDATE lock serializes appends, so chain order and bigserial id
-- order cannot diverge. chain_link stays nullable at the schema: NULL after initialization means "a
-- row written around the chained writer" and is a VERIFICATION FAILURE (missing-link), which is
-- exactly the evidence we want a raw INSERT to leave behind.
ALTER TABLE skill_version ADD COLUMN chain_link text;

CREATE TABLE skill_chain_head (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    head       text NOT NULL,
    row_count  bigint NOT NULL CHECK (row_count >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Credential-plane declaration (0060 discipline): the chain head rides the same plane as the skill
-- family it protects — both planes read it at compose/verify; only the chained writers advance it.
COMMENT ON TABLE skill_chain_head IS 'plane: both';
