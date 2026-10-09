# Knowledge corpus seed (TG-125)

Territory Grounder's precedent corpus is seeded from the predecessor `claude-gateway`
`incident_knowledge` table so that **known incident shapes stop classifying as `ood-novel`** (the
novelty gate's autonomy converges instead of forcing a poll on every first-seen class) and precedent
**retrieval becomes real** instead of ranking over an empty corpus.

- **Source (read-only):** `/home/tg/gitlab/products/cubeos/claude-context/gateway.db`
  (SQLite, opened `?mode=ro`), table `incident_knowledge`.
  Do **not** use `n8n/claude-gateway/gateway.db` — that path is a 0-byte trap.
- **Tool:** `tools/seed-knowledge/` (an isolated nested Go module so its pure-Go sqlite driver never
  enters the runtime `go.mod`). Regenerate:
  `cd tools/seed-knowledge && go run . --out ../../deploy/knowledge/corpus.seed.json`
- **Output (committed):** `deploy/knowledge/corpus.seed.json` — a deterministic (dedup + sort by
  `external_ref` via `knowledge.MergeCorpus`) JSON array of `core/knowledge.Incident`.
- **Deploy wiring (seed ∪ maintained split):** the worker/grounder read the UNION of
  `TG_KNOWLEDGE_SEED_FILE=/knowledge/corpus.seed.json` (this tracked seed — read-only bootstrap,
  re-synced from git on every deploy) and `TG_KNOWLEDGE_FILE=/knowledge/corpus.maintained.json` (the
  untracked, deploy-persistent corpus the worker alone writes: novelty writeback, lessons merge, decay
  prune). Compose defaults wire both. **Never point `TG_KNOWLEDGE_FILE` at the seed path** — the deploy
  overwrites tracked files, so runtime rows written there are wiped by the next deploy (observed live
  2026-07-23: a morning's 9 writebacks vanished at the next deploy). The `deploy/knowledge/.gitignore`
  encodes the split: only `corpus.seed.json` is tracked; every runtime file the worker writes stays
  untracked and survives.

## The #1 correctness requirement — provable-same-slug

The novelty gate is `knowledge.Count(host, alertRule)` — an `eqFold` (trim + case-fold) match on **both**
host and rule. The classifier calls it with `host` = the action target (the LibreNMS `device.hostname`)
and `alertRule = env.AlertRule = librenms.SlugifyRule(rawRuleName)`. If a seeded row's stored
`alert_rule` is not **exactly** that slug, the corpus populates but `Count` stays `0` — a silent no-op.

Defeated **by construction**: the seed tool and the guard test both call the SAME exported
`librenms.SlugifyRule` the live ingester uses (`modules/ingest/librenms/normalize.go`). The guard
`core/knowledge/seed_roundtrip_test.go` (wired into `make test`) parses the committed file and asserts
`Count(host, SlugifyRule(rawLiveRule)) > 0` for the six guinea-pig shapes — one function, one slug.

## Mapping (predecessor row → `core/knowledge.Incident`)

The TG record allows exactly 7 JSON fields (`ParseCorpus` uses `DisallowUnknownFields`), so the tool
emits only these:

| TG field       | Source                                              | Notes |
|----------------|-----------------------------------------------------|-------|
| `external_ref` | `"pred-ik-" + id`                                   | required, unique, the dedup key |
| `host`         | `hostname` **verbatim**                             | same LibreNMS `device.hostname` both sides; no FQDN strip |
| `alert_rule`   | `librenms.SlugifyRule(canonicalize(alert_rule))`    | see Caveat A |
| `site`         | derived: `dc1*`→`nl`, `grskg0*`→`gr`, else `""` | cosmetic — `Count` ignores site |
| `summary`      | `root_cause`, redacted then clipped ~500 runes      | |
| `resolution`   | `resolution`, redacted then clipped ~500 runes      | |
| `tags`         | `tags` comma-split, redacted, empties dropped       | |

Dropped predecessor columns (not in the TG struct): `confidence`, `duration_seconds`, `cost_usd`,
`embedding` (predecessor-model vector — never carried), `session_id`, `project`, `valid_until`,
`suppression_status`, `demotion_reason`, `created_at`.

## Extract filters

Kept rows are **keyed** (`TRIM(alert_rule) <> '' AND TRIM(hostname) <> ''`) and **not bi-temporally
invalidated** (`valid_until IS NULL OR valid_until > datetime('now')` — the predecessor's own validity
predicate from `scripts/kb-semantic-search.py`).

- A **future** `valid_until` is a still-open suppression window → **KEEP** (highest Tier-0 stand-down
  value; `confidence = -1.0 / suppression_status = analysis_only` rows are kept — `Count` ignores
  confidence). A **past** `valid_until` is superseded knowledge → **DROP** (e.g. the expired
  `InfragraphPrecisionDrop` wildcard row).
  > NOTE — this **corrects** the recon plan's literal "DROP `valid_until IS NOT NULL`": that would have
  > silently dropped 4 of the 6 mandatory guinea-pig shapes (their suppression windows are future-dated).
- **Wildcard** `hostname = '*'` rows are kept **in the main seed** (the merged-to-main `Count` supports
  fleet-wide `'*'` de-novel): `TargetDown`, `HighPodRestartRate`, `ContainerOOMKilled`, `KubeClientErrors`.
- **DROP non-LibreNMS hosts** (their shapes can never be the target of a LibreNMS `(host,rule)` alert;
  the Prometheus/k8s-origin shapes are carried by the `'*'` rows instead): `my-awx-web`,
  `prometheus-*`.

Phase-1 result: **146 keyed/non-invalidated scanned → 2 non-LibreNMS dropped → 144 emitted** (109 `nl`,
31 `gr`, 4 wildcard).

## Caveat A — rule-name canonicalization (evidence-based, rule-specific)

Historical rule strings drift from the current live LibreNMS rule names. Because `.` is **in** the slug
charset, `Service up/down.` slugs *differently* than `Service up/down`. We canonicalize to the current
live name **before** slugging — but NOT with a blanket "strip trailing dot", because some live rule names
legitimately end in `.`. The collapse table is grounded in `incident_knowledge.created_at` recency:

| Predecessor string                                        | → Canonical (current live)                | Evidence |
|-----------------------------------------------------------|-------------------------------------------|----------|
| `… - Critical Alert.` suffix                              | strip suffix, keep base                   | suffix only on 2026-04-03; base current |
| `Devices up/down.`                                        | `Devices up/down`                         | dotted only 2026-04-03; plain → 2026-07-18 |
| `Service up/down.`                                        | `Service up/down`                         | dotted only 2026-04-03; plain → 2026-07-19 |
| `Port status up/down.`                                    | `Port status up/down`                     | dotted only 2026-04-03; plain → 2026-07-16 |
| `Device rebooted.`                                        | `Device rebooted`                         | dotted only 2026-04-03; `eval/corpus.json` plain |
| `Device Down! Due to no ICMP response.`                   | **unchanged** (its `.` IS the live name)  | current 2026-06-21 → 2026-07-13, no plain variant |
| `Device Down (SNMP unreachable)`, `Sensor over limit - …` | **unchanged**                             | current, no drift |

Cross-checked against the live rule strings in `tools/shadowbench/inject-tier1-diskfill.sh`
(`Space on / is >= 90% and < 95% in use`), `eval/corpus.json`, and the `/api/v0/rules` mock in
`modules/ingest/librenms/tools_test.go`. 15 rows were canonicalized in Phase 1.

## Redaction

`summary`, `resolution`, and each `tag` are passed through `core/screen.Redact` (the repo's existing
deterministic secret/credential redactor) **before** clipping — so no token/password/API-key/PEM/basic-auth
value can reach the committed file, and clipping can never bisect a secret. Phase-1: no real secret was
present in the kept rows; the redactor conservatively over-redacted a `"pass": false` chaos-experiment
JSON boolean to `[REDACTED:password]` on 2 rows (`pred-ik-49`, `pred-ik-57`) — safe over-redaction of
benign text, and irrelevant to de-novelling (`Count` reads only host + rule). Structured host/rule/site
values are low-risk and left as-is (the public mirror sanitizer, `github-sync/replacements.txt`, handles
real host/site/domain genericization on push).

## Deliberate non-fabrication

A guinea-pig **memory-pressure** shape (LibreNMS rule 21, `Linux High Memory Usage, >= 90% in use`) is
**not** seeded for `dc1librespeed01` — that shape SHOULD stay novel so the first time it is seen a
human enters the loop. The guard test asserts it stays `Count = 0`.
