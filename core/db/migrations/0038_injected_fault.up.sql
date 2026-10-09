-- 0038: the injected-fault ground-truth ledger (benchmark axis A1 — detection recall, docs/BENCHMARK-AXES.md).
--
-- A1 (detection recall = did TG catch the fault?) was an unmeasurable coverage gap: nothing recorded WHICH
-- faults were injected, so there was no denominator to score detection against. This table is that ground
-- truth — one row per deliberately-injected fault (the benchmark methodology: inject a known fault on a
-- guinea-pig, then measure whether LibreNMS→TG detected it). The live-axis scorer (cmd/axisscore) computes
-- recall by correlating each injected fault with an ingest_alert for the same host inside a detection window.
--
-- Written by the `faultledger` tool at injection time (append-only in practice; no runtime code writes it —
-- it is out-of-band benchmark instrumentation, never on the agent decision path). detected_at is nullable and
-- set/observed by the scorer, not required at insert.
CREATE TABLE injected_fault (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host           text NOT NULL,                 -- the faulted host (matches ingest_alert.host / session_triage.host)
  fault_type     text NOT NULL,                 -- e.g. device-down | disk-fill | service-down | memory
  injected_at    timestamptz NOT NULL DEFAULT now(),
  note           text NOT NULL DEFAULT '',      -- free-text context (the guinea-pig, the drill)
  schema_version int NOT NULL DEFAULT 1
);

CREATE INDEX injected_fault_host_time ON injected_fault (host, injected_at);
