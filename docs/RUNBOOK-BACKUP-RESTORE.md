# Backup and restore — the operational workflow `[O]`

> **Reference material, not a work queue.** Read on demand; the board (`docs/BOARD.md`) is the queue.

This is the procedure that was executed on 2026-11-07 (TG-674, deep-review F16 / evidence-pack item 9) and is
now the documented workflow: a logical dump of every database the deployment depends on, an **isolated**
restore check that proves the dump restores and the audit spine inside it is intact, a hypervisor-level
snapshot of the whole guest, and — the part that is new — what a restore does to TG's **authority** when the
restored rows disagree with the ledger. Hostnames, paths and credentials are deployment-specific and are
deliberately absent; the compose stack names below are the ones in `deploy/docker-compose.yml`.

## 1. What must be backed up, and why each

| Store | Compose service | Why it matters |
|---|---|---|
| the application database (`grounder`) | `postgres` | the governance ledger (`governance_ledger`, hash-chained, append-only), the actuation record (`action_execution`, `action_verdict`), the posture rows (`policy_mode`, `mutation_breaker_state`, `policy_graduation`, `graduation_credit`), the hold memory (`hold_decision`), the triage/judge corpus |
| Temporal's own persistence (`temporal`, `temporal_visibility`) | `temporal-postgres` | workflow histories (24 h retention) and the **independent ledger witnesses** (`WitnessAnchorWorkflow` results, `temporal/ledgeranchor/witness.go`) — the second credential domain the anchor cross-check relies on |
| the maintained knowledge corpus and the caution lane | files under the worker's data volume (`TG_KNOWLEDGE_FILE`, the caution path) | TG's learned precedent; losing it costs retrieval quality, never safety |
| the box `.env` and bao-delivered secrets | host | compose interpolation + the secret references; secrets themselves live in OpenBao and are not in any dump |

## 2. Taking the dump (executed 2026-11-07 18:27Z)

Run on the deployment host, from the compose directory. Each dump is a custom-format `pg_dump` taken inside
the database container so no port is published and no credential leaves the container:

```sh
TS=$(date -u +%Y%m%dT%H%M%SZ)
docker compose exec -T postgres          pg_dump -U postgres -Fc grounder            > grounder-$TS.dump
docker compose exec -T temporal-postgres pg_dump -U temporal -Fc temporal            > temporal-$TS.dump
docker compose exec -T temporal-postgres pg_dump -U temporal -Fc temporal_visibility > temporal_visibility-$TS.dump
sha256sum *-$TS.dump > SHA256SUMS
```

Keep the three dumps **and** `SHA256SUMS` together, off the box. A dump without its checksum cannot later prove
it is the file that was restore-checked.

## 3. The isolated restore check (executed 2026-11-07 18:31Z)

Never restore over production to "see if it works". Restore into a throwaway `pgvector/pgvector:pg16` container
(the `vector` extension is required by migration 0013) and interrogate it:

```sh
docker run -d --name tg-restore-check -e POSTGRES_PASSWORD=check -e POSTGRES_DB=grounder pgvector/pgvector:pg16
docker cp grounder-$TS.dump tg-restore-check:/tmp/
docker exec tg-restore-check pg_restore -U postgres -d grounder --no-owner --no-privileges /tmp/grounder-$TS.dump
```

Then prove the spine inside it is whole — the three questions a dump has to answer before it is trusted:

```sql
-- 1. how much ledger is there, and does it end where production said it ended?
SELECT count(*), max(seq) FROM governance_ledger;
-- 2. are there GAPS (a dump taken mid-write, a partial restore)?
SELECT count(*) AS gaps FROM (
  SELECT seq, lag(seq) OVER (ORDER BY seq) AS prev FROM governance_ledger) s
WHERE prev IS NOT NULL AND seq <> prev + 1;
-- 3. does every row still link to the one before it?
SELECT count(*) AS broken_links FROM (
  SELECT hash, prev_hash, lag(hash) OVER (ORDER BY seq) AS expected_prev FROM governance_ledger) s
WHERE expected_prev IS NOT NULL AND prev_hash <> expected_prev;
```

The 2026-11-07 check read **22,352 rows, 0 gaps**. Question 3 is the linkage half of what `core/audit.VerifyChain`
re-walks; the content-hash half (every row re-hashed from its own fields) is what the worker's scheduled
`ChainVerifyJob` runs against production (TG-674) — on a restored copy, boot a worker against the restore-check
database with a read-only DSN and read its first `ledger chain verify:` log line, or trust the production re-walk
that ran against the same rows before the dump. Record the numbers next to the checksums. Then
`docker rm -f tg-restore-check`.

## 4. The hypervisor-level snapshot

The deployment runs as one guest on a virtualisation cluster; the cluster's scheduled guest backup (`vzdump`
or its equivalent) captures the whole filesystem — data volumes, `.env`, host units — as a second, coarser
restore point. On 2026-11-07 the scheduled run had been missed since 10-31 and was re-run by hand; **check the
schedule actually fired** rather than assuming it — the backup that is configured and the backup that exists
are different facts. Disk on the guest was at 73 %; dumps are kept off the guest for that reason too.

## 5. What a restore does to TG's authority — read this before restoring

A restore rewinds every rewritable row to the dump. Three of them *are* TG's authority:

- `policy_mode` — may TG actuate at all;
- `mutation_breaker_state` — did a deviation trip turn mutation off;
- `policy_graduation` — which op-classes act without a vote.

The hash-chained `governance_ledger` carries every mode transition, every `/halt`, every breaker trip and
rearm as an appended row, and a restore cannot rewind it unnoticed because the Temporal witnesses
(`temporal/ledgeranchor`) live in a different database under a different credential. So on every boot the
worker **reconciles** the stored rows against the ledger's last governing rows (`cmd/worker/boot_reconcile.go`):

- **Dangerous direction** — a stored row claims MORE authority than the ledger records (an actuating
  `policy_mode` over a ledgered transition to Shadow, a `/halt` or a breaker trip; a closed breaker over a
  ledgered trip with no rearm after it): the worker ledgers `boot:inconsistent-state`, publishes
  `tg_boot_posture_inconsistent=1` and `tg_boot_posture_held=1`, and **forces itself to Shadow** through the
  mode controller — durably, so every sibling process follows. The only release is an authorised
  `POST /v1/mode` transition by an operator who has decided the restored authority is wanted.
- **Safe direction** — the row claims LESS (a Shadow row under a ledgered Semi-auto; an open breaker after a
  ledgered rearm): the worker ledgers and publishes the disagreement and **changes nothing**; the alert
  `TGBootPostureInconsistent` pages, and an operator decides whether to restore the authority.
- **The ladder** is compared database-against-database (`policy_graduation` rungs against the exactly-once
  `graduation_credit` rows) and is labelled weak evidence that never holds — ladder transitions are not yet
  ledgered.

Expect, therefore, that **restoring an older dump into a deployment that was later killed or demoted brings TG
back in Shadow**, with a critical page and a ledger row saying why. That is the intended outcome. Also expect
the anchor cross-checks to fire (`GOVERNANCE LEDGER TAMPER DETECTED` in the worker log) when the restored chain
is shorter than what Temporal witnessed: the Temporal witness store remembers the real head, and the own-history
walk (TG-674) treats a witnessed seq the chain no longer reaches as a rollback. Both signals are correct; the
operator response is to confirm the restore was deliberate, record it on the tracker, and transition the mode
back by hand once satisfied — never to silence the checks.

## 6. Order of operations for a real restore

1. Stop the stack (`docker compose down`), keeping volumes.
2. Take a fresh dump of whatever is currently there first — a restore destroys the present state, and the
   present state may be the one you later need.
3. Restore the application database, then Temporal's two databases, from the SAME timestamped set
   (`SHA256SUMS` verified). Mixed sets make the Temporal witnesses disagree with the chain by construction.
4. Bring the stack up and read the worker boot log for `boot reconcile:` and `ledger chain verify:`; expect the
   Shadow hold described in §5 if the dump predates a later kill or demotion.
5. Only then decide, as an operator, whether to transition the mode back.
