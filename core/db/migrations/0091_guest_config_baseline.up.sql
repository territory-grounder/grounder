-- 0091: guest_config_baseline — the per-guest PVE config-hash baseline (TG-466 slice 1).
--
-- TG-407 built the intrusion classifier: covered-but-empty attribution + a CONFIRMED observed
-- mutation ⇒ attributed-suspicious. The estate emits no positive observed-mutation source, so the
-- classifier is armed by construction and reachable by nothing. This table is the grounded source's
-- memory: one row per guest vmid holding the last CONFIG hash the sweep computed (volatile,
-- machine-managed keys excluded — see modules/cmdb/pve/confighash). A hash that moved is a
-- deliberate act by construction; a lifecycle state diff could never be this source, because an
-- organic crash is a state change with no actor and would flood SECURITY (INV-09).
--
-- MUTABLE single-writer projection like guest_liveness (0079), NOT an append-only spine table —
-- latest-wins per vmid, so tg_runtime keeps UPDATE here (no 0015-style REVOKE): the sweep's
-- read-compare-roll is the whole write path, and freezing rows would freeze every baseline at its
-- first sighting, turning every later sweep into a phantom "changed". changed_at survives until the
-- NEXT change (not the next sweep), which is what lets the slice-2 reader ask "did the config change
-- within the attribution window?" long after the sweep that saw it.
--
-- Guests that VANISH from the sweep keep their rows: a dead node's guests dropping off
-- /cluster/resources mid-incident (the pve03 shape) must not delete the baseline — on reappearance
-- an UNCHANGED config diffs clean instead of first-sighting away a real intervening edit.
CREATE TABLE guest_config_baseline (
  vmid          bigint PRIMARY KEY CHECK (vmid > 0),
  guest         text NOT NULL CHECK (length(btrim(guest)) > 0),
  node          text NOT NULL DEFAULT '',
  kind          text NOT NULL DEFAULT '',
  config_hash   text NOT NULL CHECK (length(config_hash) > 0),
  prev_hash     text NOT NULL DEFAULT '',
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  observed_at   timestamptz NOT NULL DEFAULT now(),
  changed_at    timestamptz
);

-- The slice-2 read is by attribution subject (guest name), the write key is vmid.
CREATE INDEX guest_config_baseline_guest_idx ON guest_config_baseline (guest);

-- plane: both — an OBSERVATION projection (the 0077/0079 class), not an actuation record: the TRIAGE
-- plane's estate sweep writes it, and the attribution activity reads it. It authorises nothing by
-- itself — its signal can only ESCALATE a session toward suspicion (which withholds actuation), and
-- the reader treats absent/never-changed as false, never as evidence.
COMMENT ON TABLE guest_config_baseline IS 'plane: both';
