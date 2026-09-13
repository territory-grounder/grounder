-- THE AUTO-DRAFTED WORLD MODEL (spec/027 REQ-2702, epic TG-227 plane 2, Stage 3).
--
-- WHAT THIS CHANGES ABOUT WHO AUTHORS A GRANT.
-- TG's actuation scope was hand-typed: an administrator wrote TG_ACTUATION_ALLOWED_UNITS before TG could
-- act on anything, which made every fresh deployment a configuration PROJECT. The predecessor never asked
-- that — it discovered the estate and earned its scope under supervision. This table is the reviewable
-- projection of discovery: rows arrive as DRAFT (granting nothing), the operator reviews a diff and adopts,
-- and only then does the entry materialize into the allowlist union.
--
-- THE ENFORCEMENT POINT DOES NOT MOVE. modules/actuation/ssh and modules/actuation/proxmox keep their
-- leaf-level default-deny gates byte-untouched: an adopted row is only ever an ADDITIONAL source of what
-- the operator granted, composed as a UNION with the boot-frozen env allowlists (ADR-0016 OQ-2 — never
-- DB-replaces-env, whose failure mode is silent narrowing on first adopt). Nothing here can widen actuation
-- by inference, by discovery, or by drift.
--
-- STATUS IS DRIVEN BY ONE CHOKEPOINT, NEVER BY UPDATE.
-- core/worldmodel.Transition is the only writer of status: allowedTransitions map, mandatory rationale,
-- ledger append BEFORE the row update, no resurrection (a rework is a NEW draft row). The CHECK below is
-- the structural backstop, not the policy — the policy is the Go state machine, and the ledger is the
-- history (this row is latest-wins, the policy_graduation split precedent).
--
-- WHY 'retired_candidate_stale' IS A STATUS AND NOT A DELETION.
-- When discovery stops seeing an approved unit, the SAFE direction is to mark it stale and keep
-- materializing it — never to auto-retire. A source that blinks (a transport error, a host briefly
-- unreachable) must not silently narrow an operator's grant; absence of evidence is not evidence of
-- absence (REQ-2705). Only an explicit operator retire ends an entry's life.
--
-- INV-14: retention_expires_at is NOT NULL on every row. INV-19: every transition is on the one chain.

CREATE TABLE IF NOT EXISTS manifest_entry (
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  -- Identity is (entity_type, name) — the SAME identity core/estate uses, so an adopted entry names
  -- exactly what the graph names and the two can never drift into describing different things.
  entity_type         text NOT NULL CHECK (entity_type IN (
                        'physical_host','pve_node','vm','lxc','network_device',
                        'tunnel','site','service','host')),
  name                text NOT NULL CHECK (length(btrim(name)) > 0),

  -- The machine the entity lives on (empty for host-typed entries). Materialization is per-target, so the
  -- union needs to know WHERE an adopted unit runs.
  host                text NOT NULL DEFAULT '',

  -- Discovery provenance and its fixed table confidence (REQ-2706). Adoption never lowers confidence
  -- (MAX-ratchet in Go); learned-tier contributions stay hard-capped below the 0.80 suppression cutoff.
  source              text NOT NULL CHECK (length(btrim(source)) > 0),
  confidence          double precision NOT NULL DEFAULT 0
                        CHECK (confidence >= 0 AND confidence <= 1),

  status              text NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft','approved','retired_candidate_stale','retired','rejected')),

  -- Mandatory on every transition; append-only decision log. Full history lives on the ledger.
  rationale           text NOT NULL DEFAULT '',
  -- SERVER-DERIVED at adoption, never client-supplied.
  approver            text NOT NULL DEFAULT '',
  ledger_seq          bigint NOT NULL DEFAULT 0,

  first_seen_at       timestamptz NOT NULL DEFAULT now(),
  last_seen_at        timestamptz NOT NULL DEFAULT now(),
  status_changed_at   timestamptz NOT NULL DEFAULT now(),

  -- INV-14: every row declares when it expires.
  retention_expires_at timestamptz NOT NULL DEFAULT (now() + interval '400 days')
);

-- One LIVE row per identity. Rejected and retired rows are history and do not block a fresh draft — that is
-- how a rework happens without a resurrection path in the state machine.
CREATE UNIQUE INDEX IF NOT EXISTS manifest_entry_live_identity
  ON manifest_entry (entity_type, name)
  WHERE status NOT IN ('rejected','retired');

-- The materialization read: every entry currently feeding the allowlist union. Stale rows are INCLUDED
-- deliberately (see the header) — narrowing a grant is an operator act, never a side effect of discovery.
CREATE INDEX IF NOT EXISTS manifest_entry_materializing
  ON manifest_entry (status, entity_type)
  WHERE status IN ('approved','retired_candidate_stale');

COMMENT ON TABLE manifest_entry IS
  'spec/027 REQ-2702 — the reviewable projection of estate discovery. Rows arrive draft (granting nothing); '
  'an operator adopt materializes them into the allowlist UNION while the leaf default-deny gates stay '
  'byte-untouched. Status changes flow ONLY through core/worldmodel.Transition (ledger-before-row).';

COMMENT ON COLUMN manifest_entry.status IS
  'draft=proposed, grants nothing | approved=operator granted | retired_candidate_stale=discovery stopped '
  'seeing it but it STILL materializes (never auto-retire: absence of evidence is not evidence of absence) '
  '| retired/rejected=terminal operator acts.';
