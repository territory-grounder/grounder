-- 0093: the observation-probe ledger (TG-180 part 2 — the census's NULL TEST).
--
-- The observation census (TG-180 part 1) classifies a silent estate entity "unobservable" from a never-fired-
-- ever proxy: a HYPOTHESIS that TG is structurally blind to it. That hypothesis is only FALSIFIABLE if a fault
-- injected into the entity is checked for whether it surfaces — census = hypothesis, probe = test. This table
-- is the probe's durable memory: one row per probe, recording the fault injected, the observation window, and
-- the eventual verdict (observable / unobservable_confirmed / inconclusive).
--
-- WHY DURABLE (the faultinjector-ledger lesson, applied to the MEASUREMENT rather than the estate): a probe is
-- injected in one cycle and DECIDED in a later cycle, after its observation window closes. Holding that pending
-- obligation in process memory would lose it across a restart — and a lost pending probe that is silently
-- re-read as "no alert" would manufacture a false coverage gap. The ledger is what lets a restarted worker pick
-- up exactly the probes still awaiting a verdict.
--
-- MUTABLE-verdict ledger like injected_fault (0041), NOT an append-only spine table: a row starts 'pending' and
-- its single verdict transition is written once when the window closes (verdict <> 'pending' ⇒ decided_at set).
-- So tg_runtime keeps UPDATE here (no 0015-style REVOKE) — the read-inject-decide flow IS the write path, and
-- freezing rows would freeze every probe at 'pending' forever.
--
-- The estate-mutating side of a probe (the actual fault) is faultinjector's concern and records injected_fault
-- with its own self-reverting restore obligation; this table records the OBSERVATION, never a restore. The
-- machinery ships DEFAULT-OFF (arming is owner-gated), so on an unarmed deployment this table simply stays
-- empty and the coverage-of-the-unmeasured dimension reads "0 of N unobservable entities probe-confirmed".
CREATE TABLE observation_probe (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host         text NOT NULL CHECK (length(btrim(host)) > 0),  -- the probed guinea-pig (matches ingest_alert.host)
  fault_class  text NOT NULL CHECK (length(btrim(fault_class)) > 0), -- e.g. device-down (a faultinjector.Class)
  injected_at  timestamptz NOT NULL DEFAULT now(),
  window_end   timestamptz NOT NULL,          -- the observation window closes here; the verdict is decided after
  ran          boolean NOT NULL DEFAULT false, -- did the fault actually COMMIT? false ⇒ never perturbed → inconclusive
  -- 'pending'                → injected, window still open; awaiting a verdict on a later cycle.
  -- 'observable'             → an alert surfaced in the window; the census "unobservable" reading was a false
  --                            negative (untested, not blind) — reclassify.
  -- 'unobservable_confirmed' → the probe RAN, the window elapsed, and NO alert surfaced: a real, tested gap.
  -- 'inconclusive'           → the probe never ran (aborted pre-effect); its silence proves nothing. Kept
  --                            DISTINCT from unobservable_confirmed so a never-ran probe is never a coverage gap.
  verdict      text NOT NULL DEFAULT 'pending',
  decided_at   timestamptz,                   -- when the verdict was written; NULL while pending
  note         text NOT NULL DEFAULT '',
  schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),
  CONSTRAINT observation_probe_verdict_chk
    CHECK (verdict IN ('pending','observable','unobservable_confirmed','inconclusive')),
  -- A decided verdict MUST carry its decision time; a pending one must not claim one.
  CONSTRAINT observation_probe_decided_chk
    CHECK ((verdict = 'pending') = (decided_at IS NULL))
);

-- The reconciler's hot path: "every probe still awaiting a verdict, soonest window-close first". Partial index —
-- pending rows are a tiny minority of a growing ledger.
CREATE INDEX observation_probe_pending ON observation_probe (window_end) WHERE verdict = 'pending';
-- The coverage numerator and the per-host "already probed" read are both keyed by host.
CREATE INDEX observation_probe_host ON observation_probe (host);

-- plane: both — OBSERVATION instrumentation, the injected_fault (0038/0041) class: it records a MEASUREMENT of
-- estate observability, authorises nothing, and is written by the worker's observation loop and read by the
-- coverage scorecard. It is not an actuation authority record (nothing gates a mutation on it) and not an
-- untrusted-content corpus, so — like injected_fault — it is granted to both planes rather than withheld.
COMMENT ON TABLE observation_probe IS 'plane: both';
