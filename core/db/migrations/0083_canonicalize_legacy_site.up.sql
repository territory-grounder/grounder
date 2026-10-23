-- 0083: BACKFILL the canonical site vocabulary into the pre-TG-456 legacy-spelled rows (TG-458).
--
-- TG-456 put CanonicalizeSite (core/ingest/site.go) at the ingest chokepoint so every NEWLY stamped or
-- stored site speaks ONE vocabulary — the deployment-key form (dc1 / dc2). But that change was
-- WRITE-FORWARD ONLY: it folded new writes and left every row written before it in its original spelling.
-- Production still holds ~2,400 legacy-spelled rows ('NL', 'nl', 'gr', ...), and a stored site that does
-- not match the canonical vocabulary is not cosmetic:
--
--   * core/predict.ScoreControl (reached from core/falsify/scorer.go) drops a predicted host as "cross-site
--     background noise" whenever alert.Site <> prediction.Site — a plain string compare. A prediction
--     stamped 'NL' scored against an alert stamped 'dc1' reads as CROSS-site, so a genuine same-site host
--     is silently excluded from the negative control. Two spellings of ONE site look like two sites.
--   * any WHERE site = 'dc1' read (e.g. TG-454's active-alert pull) silently excludes the legacy 'NL'
--     rows — a numerator/denominator freshness mismatch, one level down.
--
-- This backfill folds every legacy spelling to the deployment-key form using the SAME normalization
-- CanonicalizeSite applies, expressed in pure SQL. The normalized key strips the separators (-, _, space)
-- and lowercases — exactly as strings.NewReplacer("-","","_",""," ","") + strings.ToLower do — so 'dc1',
-- 'dc1' and 'NL' share one cluster key; then key = 'nl' or a 'nllei' prefix folds to dc1, and key =
-- 'gr' or a 'grskg' prefix folds to dc2. Every OTHER spelling (an unknown site such as 'ch'/'no'/'tx',
-- or the empty '') passes through UNCHANGED — the estate makes no site claim it cannot ground, matching
-- CanonicalizeSite's honest passthrough. It fires over the SAME nine site-bearing tables the pipeline
-- stamps from env.Site.
--
-- IDEMPOTENT: each UPDATE carries a `site <> '<target>'` guard, so an already-canonical row is never
-- rewritten (an already-'dc1' row normalizes to the 'nllei' key and would otherwise match) and a second
-- run touches ZERO rows. Re-running this migration — or running it after the estate has already been folded
-- — is a no-op, and it never touches 'ch', 'no', 'tx' or '' (none normalizes to nl / nllei… / gr / grskg…).
--
-- ROLE: this runs under the MIGRATION (owner) role, so the `REVOKE UPDATE, DELETE ... FROM tg_runtime` on
-- the six append-only tables here (action_execution, action_verdict, ingest_alert, ingest_alert_occurrence,
-- ingest_transition, prediction_verdict) does NOT block it — an owner is not subject to a role's revoked
-- grant. This is a one-time deploy-time correction of historical rows, not a runtime write.

-- action_execution
UPDATE action_execution SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE action_execution SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- action_verdict
UPDATE action_verdict SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE action_verdict SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- discovery_deviation
UPDATE discovery_deviation SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE discovery_deviation SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- infragraph_prediction
UPDATE infragraph_prediction SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE infragraph_prediction SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- ingest_alert
UPDATE ingest_alert SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE ingest_alert SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- ingest_alert_occurrence
UPDATE ingest_alert_occurrence SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE ingest_alert_occurrence SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- ingest_transition
UPDATE ingest_transition SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE ingest_transition SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- pending_decision
UPDATE pending_decision SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE pending_decision SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');

-- prediction_verdict
UPDATE prediction_verdict SET site = 'dc1'
  WHERE site <> 'dc1'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'nl'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'nllei%');
UPDATE prediction_verdict SET site = 'dc2'
  WHERE site <> 'dc2'
    AND (lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) = 'gr'
      OR lower(replace(replace(replace(trim(site), '-', ''), '_', ''), ' ', '')) LIKE 'grskg%');
