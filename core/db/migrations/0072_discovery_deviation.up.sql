-- 0072 (TG-206) — durably back the discovery corpus so a live-scored misprediction survives a restart.
--
-- THE GAP. core/falsify.MemDiscoveryCorpus is an in-PROCESS rolling buffer: the verify-time Scorer captures
-- every scored deviation (a committed prediction reality falsified) into it, keyed by the deviation SIGNATURE
-- (target, site, sorted surprise-hosts), counting reproductions across incidents — the richest source of real
-- eval regression cases. But it lives only in the worker's memory, so a restart drops the whole corpus and the
-- "reproduces >= N" promotion signal resets to zero. The pgx-backed DiscoveryStore (core/db/discovery_write.go)
-- satisfies the SAME falsify.DiscoveryWriter seam and persists here.
--
-- THE SHAPE. One row per deviation SIGNATURE (deviation_key PK — first-wins insert, a reproduction increments
-- the count and advances last_seen), mirroring the in-memory dedup exactly. The typed breakdown the Scorer
-- read off verify.VerdictDetail (surprise_hosts, mismatches, observed cascade, confusion-matrix score) is kept
-- as jsonb so a promotable snapshot reconstructs faithfully. NON-SECRET by construction: only host/rule/site
-- slugs and hashes ever land here — no argv, credential, or token material. It is a CURRENT-STATE counting
-- corpus, NOT an append-only ledger: reproductions is legitimately UPDATEd, so no append-only REVOKE applies.
CREATE TABLE discovery_deviation (
  deviation_key    text PRIMARY KEY,
  action_id        text NOT NULL DEFAULT '',
  plan_hash        text NOT NULL DEFAULT '',
  prediction_hash  text NOT NULL DEFAULT '',
  target_host      text NOT NULL DEFAULT '',
  site             text NOT NULL DEFAULT '',
  verdict          text NOT NULL DEFAULT '',
  surprise_hosts   jsonb NOT NULL DEFAULT '[]'::jsonb,
  mismatches       jsonb NOT NULL DEFAULT '[]'::jsonb,
  observed         jsonb NOT NULL DEFAULT '[]'::jsonb,
  score            jsonb NOT NULL DEFAULT '{}'::jsonb,
  committed_at     timestamptz,
  observed_at      timestamptz,
  reproductions    int NOT NULL DEFAULT 1 CHECK (reproductions >= 1),
  first_seen       timestamptz NOT NULL DEFAULT now(),
  last_seen        timestamptz NOT NULL DEFAULT now(),
  created_at       timestamptz NOT NULL DEFAULT now()
);
-- promotion reads "signatures reproduced >= N, most-reproduced first".
CREATE INDEX discovery_deviation_repro_idx ON discovery_deviation (reproductions DESC, last_seen DESC);

-- Credential plane (TG-323 / migration 0060): the discovery corpus neither RECORDS/AUTHORISES an actuation
-- nor is the untrusted-content corpus a mutation is grounded in — it is measurement, written by the
-- verify-time Scorer alongside infragraph_prediction / prediction_verdict (both 'both') — so neither
-- withheld list applies and it takes the default BOTH plane, consistent with its falsifiability siblings.
COMMENT ON TABLE discovery_deviation IS 'plane: both';
