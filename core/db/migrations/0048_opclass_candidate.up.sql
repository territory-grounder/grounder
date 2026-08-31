-- THE EARNED OP-CLASS CATALOG'S EVIDENCE PLANE (spec/028 REQ-2801/REQ-2802, epic TG-227 plane 3, Stage 2).
--
-- Plane 1 (spec/026) made TG PROPOSE instead of stand down when no op-class is registered: a free-form
-- proposal diverts to a shadow branch — recorded, ledgered, rendered, never executable. Those proposals are
-- the raw evidence. This migration is where the evidence ACCRUES into candidacy, so that months later an
-- operator ratifies a capability from a dossier of what actually kept happening, instead of hand-authoring a
-- catalog before TG can do anything (the predecessor EARNED its autonomy; TG demanded its authorship —
-- ADR-0016's problem statement).
--
-- TWO TABLES, DELIBERATELY SEPARATE:
--
--   opclass_candidate            — the LIFECYCLE row. Mutable status, driven ONLY through the audited
--                                  Transition chokepoint (core/opclasscat), ledger-before-row.
--   opclass_candidate_occurrence — the APPEND-ONLY evidence journal. Screened model text lives HERE, not on
--                                  the lifecycle row, so untrusted free text carries its own retention and
--                                  can never be mistaken for adjudicated state.
--
-- WHY THE OCCURRENCE PK IS (candidate_key, external_ref) AND NOT AN IDENTITY COLUMN.
-- This is the 4x-credit lesson, paid for once already. One stopped guest raises FOUR LibreNMS rules, so the
-- same incident re-proposes the same remedy up to four times; a session that pauses for a vote and resumes
-- re-proposes again. Keyed by identity, "three distinct incidents" would be satisfied by ONE incident
-- proposed three times, and candidacy — the thing that decides whether an operator is ASKED to grant a
-- capability — would be manufactured by alert-rule multiplicity. Keying on (key, ref) makes evidence credit
-- exactly-once BY KEY rather than by join correctness downstream: the dedup is structural, in the primary
-- key, where no later query can get it wrong. ON CONFLICT DO NOTHING keeps the FIRST observation (the
-- earliest is the honest one; a re-proposal is not new evidence).
--
-- WHY UPDATE/DELETE ARE REVOKED ON THE JOURNAL (migration 0015/0042/0043 precedent).
-- Evidence that can be edited after the fact is not evidence. The dossier an operator reads before granting
-- a capability must be the observations as they landed, not as they were later tidied.
--
-- WHAT THIS MIGRATION DELIBERATELY DOES NOT ADD.
-- The registry overlay (opclass_ratified, migration 0049) and the ladder widening (migration 0050) belong to
-- Stage 4 and to the ladder task (spec/028 T-028-3, which carries a Law-Change trailer). Stage 2 is the
-- OBSERVATION arm only: it accrues evidence and can create no executable capability whatsoever. Rung 0
-- ("proposes only") stays registry ABSENCE — fail-closed by construction, zero new code (REQ-2805).

-- ---------------------------------------------------------------------------------------------------
-- The lifecycle row.
-- ---------------------------------------------------------------------------------------------------
CREATE TABLE opclass_candidate (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  -- The cluster identity: SHA-256("v1|"+norm(op_class)+"|"+norm(op)+"|"+sorted-param-names), where norm is
  -- opschema's INV-08 normalization (lowercase+trim). Host and rule_family are deliberately EVIDENCE, not
  -- identity — cross-host and cross-rule recurrence is exactly what "this remedy generalizes" means, and
  -- keying on rule_family would fragment one remedy across the four alert families a single fault raises.
  candidate_key  text        NOT NULL CHECK (length(btrim(candidate_key)) > 0),

  -- The normalized identity components, stored so a dossier renders without re-deriving them (and so a key
  -- collision is debuggable). NOT the identity itself — candidate_key is.
  op_class       text        NOT NULL CHECK (length(btrim(op_class)) > 0),
  op             text        NOT NULL DEFAULT '',
  param_names    text[]      NOT NULL DEFAULT '{}',

  status         text        NOT NULL DEFAULT 'observing'
                 CHECK (status IN ('observing','candidate','ratify_ready','ratified','dismissed','expired')),

  -- Server-derived safety screen. DEFAULT TRUE = fail-closed: a candidate is barred from ever climbing the
  -- ladder until the screen has actually run and stamped it, so a row that skips the screen cannot be
  -- mistaken for one that passed it. Stamped from safety.IsNeverAuto / safety.IsDestructiveOp over the
  -- OBSERVED op and params — never from anything the model declared about itself (a model cannot
  -- under-declare its own blast radius into a lower tier).
  auto_barred    boolean     NOT NULL DEFAULT true,

  -- Mechanically assigned from the closed sets at ratify_ready (empty until then).
  family         text        NOT NULL DEFAULT '',
  tier           text        NOT NULL DEFAULT '',

  -- The dossier snapshot the operator's decision was taken against, plus its hash. The hash is bound into
  -- the candidacy GovDecision so the ledger covers WHAT WAS SHOWN, not merely that something was shown.
  dossier        jsonb       NOT NULL DEFAULT '{}'::jsonb,
  dossier_hash   text        NOT NULL DEFAULT '',

  -- Dismiss TTL (30 days, read-path expiry — the core/governance/demote.go pattern): a dismissed candidate
  -- stops nagging without being erased, and the key becomes re-observable when the TTL lapses.
  dismissed_at   timestamptz,
  dismiss_until  timestamptz,

  -- Rationale log (append-only text, same shape as the skill version log) + the ledger seq of the last
  -- transition, so a row points back at the chain entry that authorized its current state.
  rationale      text        NOT NULL DEFAULT '',
  ledger_seq     bigint      NOT NULL DEFAULT 0,

  first_seen_at  timestamptz NOT NULL DEFAULT now(),
  last_seen_at   timestamptz NOT NULL DEFAULT now(),
  status_changed_at timestamptz NOT NULL DEFAULT now(),
  schema_version int         NOT NULL DEFAULT 1 CHECK (schema_version > 0),

  -- A dismissed row carries its TTL; a non-dismissed row never does. Keeps "is this suppressed" a single
  -- readable fact instead of two columns that can drift apart.
  CONSTRAINT opclass_candidate_dismiss_pairing_chk
    CHECK ((status = 'dismissed' AND dismissed_at IS NOT NULL AND dismiss_until IS NOT NULL)
        OR (status <> 'dismissed' AND dismissed_at IS NULL AND dismiss_until IS NULL))
);

-- ONE LIVE ROW PER KEY. Terminal rows (dismissed/expired) are excluded so the same key can be observed
-- fresh afterwards — the design's "the key is re-observable" property — while two concurrent live
-- candidacies for one remedy remain structurally impossible.
CREATE UNIQUE INDEX opclass_candidate_live_key
  ON opclass_candidate (candidate_key)
  WHERE status NOT IN ('dismissed','expired');

CREATE INDEX opclass_candidate_status    ON opclass_candidate (status, last_seen_at);
CREATE INDEX opclass_candidate_opclass   ON opclass_candidate (op_class);

COMMENT ON TABLE opclass_candidate IS
  'Earned op-class candidacy lifecycle (spec/028 REQ-2801). Status changes ONLY through the audited Transition chokepoint (core/opclasscat), ledger-before-row. Creates no executable capability: rung 0 is registry ABSENCE.';
COMMENT ON COLUMN opclass_candidate.auto_barred IS
  'Server-derived never-auto screen, DEFAULT TRUE (fail-closed until stamped). A barred candidate stays RATIFIABLE but is ceiling-capped at "asks first" forever — a recurring destructive desire is visible, never climbable.';

-- ---------------------------------------------------------------------------------------------------
-- The append-only evidence journal.
-- ---------------------------------------------------------------------------------------------------
CREATE TABLE opclass_candidate_occurrence (
  -- Exactly-once evidence credit, structurally. See the PK rationale in this file's header.
  candidate_key text        NOT NULL CHECK (length(btrim(candidate_key)) > 0),
  external_ref  text        NOT NULL CHECK (length(btrim(external_ref)) > 0),

  host          text        NOT NULL DEFAULT '',
  target        text        NOT NULL DEFAULT '',
  op            text        NOT NULL DEFAULT '',
  op_class      text        NOT NULL DEFAULT '',

  -- SCREENED model free text (screen.Scrub'd by the shadow-record activity BEFORE it reaches this table —
  -- spec/026 REQ-2606). Untrusted DATA (INV-08): rendered as labelled exhibits, never executed, never
  -- parsed into control flow. It lives on the journal rather than the lifecycle row so untrusted text
  -- carries its own retention and can never be confused with adjudicated state.
  rationale     text        NOT NULL DEFAULT '',
  undo_sketch   text        NOT NULL DEFAULT '',

  -- The model's stated confidence for this proposal. A BAR, never a weight (REQ-2811): the cron requires a
  -- mean at or above the bar, and nothing a model can inflate ever buys candidacy FASTER.
  confidence    double precision NOT NULL DEFAULT 0,

  -- Binder-verified evidence ids and the migration-0035 minimized actor-evidence blob, so a dossier can
  -- show WHY the proposal was grounded and WHO last touched the target.
  evidence_ids  text[]      NOT NULL DEFAULT '{}',
  actor_evidence jsonb      NOT NULL DEFAULT '[]'::jsonb,

  band          text        NOT NULL DEFAULT '',
  outcome       text        NOT NULL DEFAULT '',

  observed_at   timestamptz NOT NULL DEFAULT now(),
  schema_version int        NOT NULL DEFAULT 1 CHECK (schema_version > 0),

  PRIMARY KEY (candidate_key, external_ref)
);

CREATE INDEX opclass_candidate_occurrence_key  ON opclass_candidate_occurrence (candidate_key, observed_at);
CREATE INDEX opclass_candidate_occurrence_time ON opclass_candidate_occurrence (observed_at);
CREATE INDEX opclass_candidate_occurrence_host ON opclass_candidate_occurrence (candidate_key, host);

COMMENT ON TABLE opclass_candidate_occurrence IS
  'Append-only evidence journal for op-class candidacy (spec/028 REQ-2802). PK (candidate_key, external_ref) makes evidence credit exactly-once BY KEY — the 4x-credit lesson: one fault raises four alert rules, and identity-keyed rows would let ONE incident manufacture candidacy.';

-- Evidence that can be rewritten is not evidence (INV-19; migration 0015/0042/0043 precedent).
REVOKE UPDATE, DELETE ON opclass_candidate_occurrence FROM tg_runtime;
