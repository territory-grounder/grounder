-- THE INCIDENT'S SUBJECT, WHEN IT IS AN IP (TG-373).
--
-- IncidentEnvelope.IP is written by FOUR ingest modules and read by NOTHING:
--
--   modules/ingest/crowdsec/crowdsec.go:216       raw2.IP = ipVal
--   modules/ingest/prometheus-alertmanager:156    raw.IP = target
--   modules/ingest/librenms/normalize.go:148      raw.IP = p.Host   -- LibreNMS "host" carries the device IP
--   modules/ingest/authlog/authlog.go:244         raw.IP = ip
--   core/ingest/normalize.go:36                   ip, err := validateIP(raw.IP)
--
-- That is the complete non-test list. Four sources populate it, core parses and validates it into the
-- envelope, and no consumer exists anywhere in core/, temporal/ or cmd/. It is a declared-but-dead field on
-- the ingest spine — the same shape TG-66 deleted from the agent-step spine, on a spine its floor does not
-- cover.
--
-- THE COST IS NOT ABSTRACT. Measured 2026-08-06: 48 of 165 prometheus-alertmanager rows (29.1%) carry no
-- host, and 40 of those 48 carry an `instance` label. The module handles them correctly —
-- hostFromInstance("10.0.2.193:8080") -> "10.0.2.193", which net.ParseIP accepts, so it is assigned to
-- raw.IP and Host is correctly left empty. The subject is extracted, parsed, validated, and then dropped one
-- layer later at the alert-log projection. Among those 40 are the three alerts TG received about its OWN AWX
-- outage (03:11-03:23), each of which minted a triage session that could not say what it was about.
--
-- NAMED subject_ip, NOT ip. Migration 0062 added delivery_peer/delivery_host to this same table — how the
-- alert REACHED TG. This is what the alert is ABOUT. A bare `ip` beside a `delivery_peer` invites exactly
-- the confusion the two columns exist to prevent.
--
-- WHAT THIS DOES NOT FIX, stated so nobody reads more into it. The estate graph resolves by NAME
-- (dc1pve01, …), so 10.0.2.193 will not match a node there either: this makes the incident ATTRIBUTABLE
-- and gives dedup and the TG-354 tracker join a key, but blast-radius resolution needs an IP->name mapping
-- that TG does not have today.
--
-- inet, not text: the database validates the shape, and an inet column can be indexed and compared by
-- network. Nullable, because most incidents identify their subject by NAME and an empty string is not an
-- address -- here NULL genuinely means "not identified by IP", which is different from 0062's columns where
-- '' means "did not arrive over HTTP".

ALTER TABLE ingest_alert ADD COLUMN subject_ip inet;

COMMENT ON COLUMN ingest_alert.subject_ip IS
  'TG-373: the incident SUBJECT when the source identified it by address rather than name (IncidentEnvelope.IP). NULL = identified by name, or not identified. Distinct from delivery_peer, which is how the alert reached TG.';
