-- THE EARNED OP-CLASS OVERLAY — the second tamper domain (spec/028 REQ-2803, ADR-0016 decision 1).
--
-- WHAT THIS IS. TG's actuatable capabilities live in `opschema.json`, go:embed-compiled and lockstep-hashed:
-- admitting a capability is a CODE RELEASE, reviewed and hash-bound (spec/007). That is the strongest tamper
-- story available — and it is also why a fresh TG can do nothing until an administrator hand-authors a
-- catalog. This table is the runtime-admitted half: op-classes an OPERATOR ratified from an evidence dossier
-- (spec/028's candidacy plane), composed UNDER the embedded registry at exactly one seam.
--
-- WHY A SEPARATE TABLE AND NOT A ROW IN THE CANDIDATE LIFECYCLE.
-- The lifecycle row (opclass_candidate, migration 0048) is mutable adjudication state: statuses advance, a
-- dismiss expires, a key re-observes. THIS table is operator-authored ACTUATION DATA — the argv template that
-- will run as root somewhere. Mixing the two would put a mutable status column on the same row as an executed
-- vector. They are separate tamper domains and they get separate tables.
--
-- APPEND-ONLY, AND REVOCATION IS A NEW ROW.
-- PK (op_class, seq): every ratification, re-ratification, and revocation is a NEW row, so the table reads as
-- the complete grant history of a capability rather than its current opinion. UPDATE and DELETE are revoked
-- from the runtime role (migration 0015/0042/0043/0048 precedent) — a grant that can be edited after the fact
-- is not a grant, it is a suggestion. To revoke, an operator writes a row with revoked=true; the composed
-- registry drops the class within one refresh and it falls back to rung 0 (registry ABSENCE) by construction.
--
-- entry_hash IS THE CHAIN BINDING.
-- entry_hash = SHA-256 over the canonical `spec` JSON. That same hash is embedded in the row's
-- `opclass:ratify` GovDecision on the ONE hash-chained governance ledger (REQ-2817's ActionID rule: ratified-
-- phase rows carry the entry_hash as action_id). So the ROW CONTENT is chain-covered, not merely the fact
-- that a ratification happened: the loader re-verifies each row's hash at load and DROPS a mismatching row
-- LOUDLY. A tampered overlay row therefore removes a capability rather than granting a forged one — the
-- failure direction is toward FEWER capabilities, which is the only direction a security control may fail in.
--
-- promote_threshold: THE LADDER'S PER-CLASS N, SET FROM TIER AT RATIFY.
-- CHECK (promote_threshold >= 5) with the compile-time default (core/policy.DefaultPromoteThreshold = 5) as
-- the floor: ratification may only ever RAISE the bar (tier table: low-reversible => 5, medium => 10), never
-- lower it. An operator cannot ratify a class into a faster climb than the code's own conservative default.
--
-- THE CEILING THIS TABLE DOES NOT ENCODE. An overlay class caps at LevelAutoNotice ("acts and pages") — full
-- AUTO requires membership in the EMBEDDED registry, i.e. a code release via embed-export. That ceiling is
-- enforced in core/policy (structurally, by consulting embedded membership), NOT by a column here: a
-- constraint in this table would be a rule the overlay applies to itself, and self-applied ceilings are the
-- ones that get edited. The rung where no human watches always lives in the strongest tamper domain.
CREATE TABLE opclass_ratified (
  op_class          text        NOT NULL CHECK (length(btrim(op_class)) > 0),
  seq               bigint      NOT NULL GENERATED ALWAYS AS IDENTITY,

  -- The exact opschema entry shape. Validated server-side by opschema.ValidateSpec BEFORE insert (the
  -- error-returning refactor of the registry's own admission checks) — a live worker must never panic on
  -- operator input, and the overlay must never admit a spec the embedded registry would have rejected.
  spec              jsonb       NOT NULL,
  -- SHA-256 over the canonical spec JSON; mirrored into the opclass:ratify GovDecision action_id.
  entry_hash        text        NOT NULL CHECK (length(btrim(entry_hash)) > 0),

  family            text        NOT NULL DEFAULT '',
  tier              text        NOT NULL DEFAULT '',
  -- Only ever raises the compile-time default (core/policy/graduation.go DefaultPromoteThreshold).
  promote_threshold int         NOT NULL CHECK (promote_threshold >= 5),

  -- Revocation is a NEW ROW, never an UPDATE. revoked=true removes the class from the composed registry
  -- within one refresh; the class returns to rung 0 (absence) and can execute nothing.
  revoked           boolean     NOT NULL DEFAULT false,

  -- Provenance: which candidate dossier this grant came from, and who typed the template.
  candidate_key     text        NOT NULL DEFAULT '',
  approver          text        NOT NULL DEFAULT '' CHECK (revoked OR length(btrim(approver)) > 0),
  rationale         text        NOT NULL DEFAULT '',
  -- The governance-chain sequence of the decision that authorized this row (ledger-before-row).
  ledger_seq        bigint      NOT NULL DEFAULT 0 CHECK (ledger_seq >= 0),

  created_at        timestamptz NOT NULL DEFAULT now(),
  schema_version    int         NOT NULL DEFAULT 1 CHECK (schema_version > 0),

  PRIMARY KEY (op_class, seq)
);

-- ONE LIVE GRANT PER OP-CLASS. A partial unique index over the non-revoked rows: the history is append-only,
-- but at most one row may be in force at a time. Two live grants for one slug would make "which template
-- runs?" depend on row order — an actuation vector decided by a sort.
CREATE UNIQUE INDEX opclass_ratified_live ON opclass_ratified (op_class) WHERE NOT revoked;

CREATE INDEX opclass_ratified_hash      ON opclass_ratified (entry_hash);
CREATE INDEX opclass_ratified_candidate ON opclass_ratified (candidate_key);
CREATE INDEX opclass_ratified_created   ON opclass_ratified (created_at);

COMMENT ON TABLE opclass_ratified IS
  'Append-only earned op-class overlay (spec/028 REQ-2803, ADR-0016). Composed UNDER the embedded lockstep-hashed registry: embedded always wins a slug collision. Every row is entry_hash-bound to its opclass:ratify GovDecision and re-verified at load; a mismatch DROPS the row (fail closed to FEWER capabilities). Revocation is a NEW row, never an UPDATE. Overlay classes cap at auto_notice — full AUTO requires a code release (embed-export).';

COMMENT ON COLUMN opclass_ratified.entry_hash IS
  'SHA-256 of the canonical spec JSON, mirrored into the opclass:ratify GovDecision action_id so ROW CONTENT is chain-covered. Re-verified at every load; mismatch drops the row loudly.';

COMMENT ON COLUMN opclass_ratified.promote_threshold IS
  'Per-class ladder N, set from tier at ratify (low-reversible 5, medium 10). CHECK >= 5 keeps it at or above the compile-time default — ratification may only RAISE the bar.';

-- A grant that can be rewritten is not a grant (INV-19; migration 0015/0042/0043/0048 precedent).
REVOKE UPDATE, DELETE ON opclass_ratified FROM tg_runtime;
