-- HOW THE ALERT GOT IN (TG-372).
--
-- THE GAP. On 2026-08-06 I tried to answer a simple question about the running system — by what path does a
-- LibreNMS alert reach TG? — and could not, from TG. The grounder publishes on 127.0.0.1:8081; the only TG
-- listener exposed off-host is the console's nginx, which returns index.html for every /v1/* GET and 405 for
-- every POST; the public name resolves to dc1npm01 and lands on that same nginx; no second DNS name
-- resolves; no host cron forwards; nothing holds a connection to :8081; and the pull poller is off. 89
-- LibreNMS alerts arrived that day regardless.
--
-- TG could not settle it because the `sources` table stores each source's TOKEN REFERENCE and not the URL it
-- was issued, and an accepted push left no trace of where it came from. An accepted alert is the strongest
-- evidence TG will ever have about its own reachability, and it was being discarded.
--
-- WHAT THESE COLUMNS ARE. delivery_peer is the transport-level remote address the front door saw.
-- delivery_host is the HTTP Host header the caller addressed TG by — which is exactly the fact that
-- distinguishes "posted straight at the container" from "came through the proxy under some name".
--
-- BOTH ARE EVIDENCE, NEVER IDENTITY. Nothing authenticates on them, nothing routes on them, and no
-- constraint depends on them. delivery_host in particular is CALLER-CONTROLLED text: it is recorded because
-- what a caller *claims* to have addressed is diagnostic, and it is bounded (see the CHECK) because
-- caller-controlled text with no bound is a storage-amplification lever. Recording an attacker-chosen string
-- is safe; believing one is not.
--
-- EMPTY IS MEANINGFUL, NOT MISSING. The pve-liveness poller mints envelopes IN-PROCESS and calls the same
-- RecordFromEnvelope constructor, so its rows carry '' — and that correctly reads as "this did not arrive
-- over HTTP" rather than as an unknown. Every historical row is '' for the same reason: nothing recorded it.
-- That is why these default to '' and are NOT NULL: a nullable column would invite "unknown" and "in-process"
-- to share a representation.

ALTER TABLE ingest_alert
  ADD COLUMN delivery_peer text NOT NULL DEFAULT '' CHECK (length(delivery_peer) <= 100),
  ADD COLUMN delivery_host text NOT NULL DEFAULT '' CHECK (length(delivery_host) <= 253);

COMMENT ON COLUMN ingest_alert.delivery_peer IS
  'TG-372: transport-level remote address the ingest front door saw. Empty = not delivered over HTTP (an in-process intake such as pve-liveness) or predates this column. Evidence only — nothing authenticates or routes on it.';
COMMENT ON COLUMN ingest_alert.delivery_host IS
  'TG-372: the HTTP Host header the caller addressed TG by — the fact that distinguishes a direct post from one through a proxy. CALLER-CONTROLLED and length-bounded; recorded, never believed.';
