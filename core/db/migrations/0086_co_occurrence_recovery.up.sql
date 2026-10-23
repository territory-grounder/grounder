-- TG-188 organic recovery learning: the co-occurrence learner now accrues per-host observed time-to-recover
-- evidence (onset→clear pairing over ingest_transition kind='recovery'), and its durable snapshot (TG-388
-- face c, migration 0077) must carry it or a restart silently forgets every learned MTTR while keeping the
-- co-occurrence counts — a partial restore that would misreport "unlearned" for hosts the learner had
-- measured. Additive, defaulted columns on the MUTABLE competence-plane cache (not the append-only spine):
-- Save() REPLACES the whole snapshot atomically, so existing rows simply carry 0/0 (= no recovery evidence)
-- until the next Save writes real values.
ALTER TABLE co_occurrence_host
  ADD COLUMN recovery_sum   double precision NOT NULL DEFAULT 0,
  ADD COLUMN recovery_count double precision NOT NULL DEFAULT 0;
