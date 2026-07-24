# shadowbench — TG vs predecessor shadow head-to-head

A read-only harness that compares **Territory Grounder (TG)** against its
predecessor (**claude-gateway**) on the *same* estate over the *same* shadow
window, and produces a blind-judged scorecard. Everything here is **read-only** and
requires **no mutation** on either system — both run in `MUTATIONS=OFF` shadow mode
and every intent is logged-not-run.

**v2 is fully scripted and headless** — `run.sh` drives the whole loop with **no
Claude workflow in it**, and `judge.py` is a scripted blind LLM judge. The benchmark
now yields a **real number that grows as incidents accumulate**, appended to
`scorecard.jsonl`.

The first live scorecard is [`REPORT-2026-07-18.md`](REPORT-2026-07-18.md).

---

## What's in here

| File | Role |
|------|------|
| `extract_predecessor.py` | Extracts the predecessor's shadow-mode reasoning for a date (local `gateway.db`, read-only). Emits `--json`. |
| `extract_tg.sh` | Extracts TG's own `session_triage` records for a window (SSH + `psql`, read-only). `TG_FORMAT=jsononly` → clean JSON array. |
| `judge.py` | **Scripted BLIND unified judge.** Scores each system 1..5 on TG's five eval dims via the box LiteLLM; presents the two trajectories as System A/B in randomized order; degrades honestly to `judge_unavailable`. |
| `run.sh` | **Headless driver.** Runs both extractors → aligns incidents → calls `judge.py` per incident → appends to `scorecard.jsonl` → prints the rolling aggregate. |
| `_driver.py` | `run.sh`'s alignment + judge-dispatch + aggregation core (kept a real module, not a shell heredoc, so it is testable and never shell-interpolated). |
| `scorecard.jsonl` | Append-only ledger: one blind verdict per judged incident, with date + dedup key. The rolling number lives here. |
| `REPORT-<date>.md` | The dated narrative scorecard (coverage matrix, S/N, findings, caveats). |
| `README.md` | This file. |

Both extractors emit **JSON** so their outputs can be aligned into per-incident
**pairs** (and tagged single-sided incidents) and handed to the blind judge.

---

## The one-command way (v2): `run.sh`

```bash
./run.sh                                     # date=today, window ≥ 2026-07-18 17:20 UTC
DATE=2026-07-18 ./run.sh                      # a specific predecessor date
SHADOW_FROM='2026-07-18 00:00:00+00' DATE=2026-07-18 ./run.sh   # wider TG window
ALIGN_HOURS=24 ./run.sh                        # loosen host+time alignment
DRY_JUDGE=1 ./run.sh                           # build prompts, do NOT call LiteLLM (offline check)
SCORECARD=/tmp/try.jsonl ./run.sh              # append to a throwaway ledger
```

`run.sh` is **idempotent-ish**: each judged incident is keyed by `date+incident`, so
re-running the same day does **not** re-judge or double-count — it only picks up new
incidents. The rolling aggregate is recomputed over the **whole** `scorecard.jsonl`
every run (predecessor vs TG mean per dimension, n, coverage, head-to-head wins).

The three steps below are what `run.sh` automates; run them by hand to debug.

---

## How to re-run the benchmark (the steps `run.sh` automates)

### 1. Extract the predecessor side (read-only)

```bash
# Human summary:
./extract_predecessor.py --date 2026-07-18
# Machine-readable record (feed this to the pairing/judge step):
./extract_predecessor.py --date 2026-07-18 --json
```

- Reproduces the shadow-log classification (scheduled-cron **noise** vs **agentic**
  dispatched-session intents) for the date, and joins each agentic incident to its
  per-session reasoning + conclusion + judge verdict from the live `gateway.db`.
- Opens `gateway.db` strictly read-only (`sqlite3 file:…?mode=ro`); **never** runs
  any gateway script/hook and **never** writes.
- **Credential safety:** the shadow log embeds live secrets (e.g. a NetBox
  `Authorization: Token <hex>`). Every string that leaves the script passes through
  `redact()`; the raw `full_command` carrying the token is never surfaced.
- Date-parametrized; classification sources and redaction rules are configurable
  constants at the top of the file.

### 2. Extract the TG side (read-only)

```bash
./extract_tg.sh
# Override the window boundary (defaults to the predecessor shadow-start 17:20 UTC):
SHADOW_FROM='2026-07-18 00:00:00+00' ./extract_tg.sh
# Point at the box / key if needed:
TG_HOST=dc1tg01 SSH_KEY=~/.ssh/one_key ./extract_tg.sh
```

- Pulls TG's `session_triage` rows joined with mean judge score
  (`session_judgment`) for the window, plus a `librespeed` cross-check and an
  unfiltered window-sanity listing. Emits a psql table **and** JSON per row.
- **`TG_FORMAT=jsononly ./extract_tg.sh`** (used by `run.sh`) emits **only** a clean
  JSON array of the window's real triage rows (`psql -tAq`), dropping the
  librenms-only restriction so the driver can align/tag **every** real TG triage in
  the window (still excludes the synthetic `%tz-%` / `%probe%` rows). Each row now
  also carries `outcome`, `proposed`, `predicted`.
- **Read-only (SELECT only), no `sh -c`, no string-mangled query:** the SQL is
  written to a file, `docker cp`'d into the `territory-grounder-postgres-1`
  container, and run with `psql -f`. `PGPASSWORD` is read from
  `/srv/tg/deploy/.env` *inside the container only* — never printed. Output is
  piped through a `sed` redaction guard.

### 3. Blind-judge each incident + append the scorecard — `judge.py`

`run.sh` calls `judge.py` per incident; you can also run it by hand:

```bash
# single-sided (predecessor only) — the sparse-data smoke test:
./extract_predecessor.py --date 2026-07-18 --json > /tmp/pred.json
./judge.py --pred-from /tmp/pred.json --incident-key IFRNLLEI01PRD-1824

# head-to-head: one predecessor incident + one TG row:
./judge.py --pred pred_incident.json --tg tg_row.json --incident-key <ref>

# inspect the exact (redacted) prompt without calling the model:
./judge.py --pred-from /tmp/pred.json --incident-key IFRNLLEI01PRD-1824 --dry-run
```

- **One unified rubric = TG's five eval dims** (`correct_diagnosis`,
  `evidence_grounded`, `sensible_proposal`, `appropriate_band`,
  `falsifiable_prediction`), scored **1..5**, carried **verbatim** from
  `core/judge/judge.go` — including the HOLLOW-PROPOSAL rule and the
  grounded-stand-down guidance. `falsifiable_prediction` is scored **null (N/A)**,
  not a low number, for a correct stand-down (nothing to falsify). The two systems'
  *native* judges use different rubrics and are **not** commensurable — this single
  blind judge re-scores both trajectories so a paired number is fair.
- **BLIND.** Each side is normalized into an **identical** field template; its
  **native judge score is stripped**; system-identifying jargon is neutralized
  (`shadow gate`→`safety guard`, `claude-gateway`/`territory-grounder`→`the agent`,
  `OpenClaw`→`the upstream triage layer`); and the two are presented as **System
  A / System B in randomized order**. The pred↔TG `mapping` is applied only *after*
  the model replies — the model never sees it.
- **Judge model = the box LiteLLM `primary` reasoning alias** (the same alias TG's
  eval uses: kimi-k3, with LiteLLM's own fallback → deepseek-v4-pro when kimi is
  overloaded). Reached over `ssh … root@dc1tg01` → `curl 127.0.0.1:4000`; the
  `LITELLM_MASTER_KEY` is read from `/srv/tg/deploy/.env` **inside the box** and
  never touches this host's argv/env/stdout/logs. The served model is recorded in
  every verdict.
- **Robust parse:** strict JSON → light repair (fences, `//` comments, trailing
  commas) → regex-scrape of the A/B integers (recovers real scores even when an
  unescaped quote in a comment breaks strict JSON). One automatic retry on a bad
  draw or transient network error.
- **Honest degrade:** if LiteLLM is unreachable / errors / yields no parseable
  verdict, the emitted record carries `"judge_unavailable": true` + an error string
  and **null** scores — **never a fabricated number**.
- **Redaction:** every trajectory string passes through the v1 `redact()` before it
  enters the prompt, the payload, stdout, or any log.

Verdict shape (one line appended to `scorecard.jsonl`):

```json
{"incident_key":"…","subject_host":"…","single_sided":true,
 "mapping":{"A":null,"B":"pred"},
 "dims":{"A":{…nulls…},"B":{"correct_diagnosis":5,"evidence_grounded":5,
   "sensible_proposal":5,"appropriate_band":5,"falsifiable_prediction":null,"comment":"…"}},
 "winner":null,"judge_model_served":"deepseek-v4-pro","judge_unavailable":false,
 "key":"2026-07-18|pred|IFRNLLEI01PRD-1824","date":"2026-07-18","kind":"pred_only"}
```

---

## Cron recipe (daily, read-only, on THIS host)

The predecessor shadow log + `gateway.db` are **local to this runner box**, and the
TG box is SSH-reachable from here — so the natural home for a daily run is **this
host**. The recipe below is **documented, not installed** (per the read-only /
no-surprise-side-effects posture). Install it only with owner sign-off.

```cron
# ~/.config/shadowbench/run — daily blind head-to-head, appends tools/shadowbench/scorecard.jsonl.
# Runs at 06:15 local, AFTER the predecessor's day has accumulated. Read-only both sides.
15 6 * * *  cd /home/tg/gitlab/products/territory-grounder/grounder/tools/shadowbench \
  && DATE=$(date -u -d 'yesterday' +\%F) SHADOW_FROM="$(date -u -d 'yesterday' +\%F) 00:00:00+00" \
     ./run.sh >> ~/logs/shadowbench/$(date -u +\%F).log 2>&1
```

Notes:
- `DATE=yesterday` scores the **completed** predecessor day; widen `SHADOW_FROM` to
  the same day's 00:00 so TG's whole day is in the window.
- Idempotent-ish: dedup by `date+incident` means a manual mid-day run and the cron
  run won't double-count.
- Needs `~/.ssh/one_key` reachable to the cron environment (or set `SSH_KEY=`).
- **Do not** point `SCORECARD` at a shared/committed path from cron without deciding
  how the ledger is version-controlled — appends are cheap but the file grows.

---

## Reading the output honestly

- A head-to-head **winner requires ≥1 aligned pair**. With 0 pairs the scorecard
  stays at "no contest yet" — the single-sided means are per-system quality on the
  incidents each actually triaged, **not** a contest. Do not spin a coverage gap as
  a win for either side.
- The predecessor stream is dominated by scheduled-cron self-heal noise; the
  benchmark counts only **real dispatched agentic triage** incidents. The `issue=T`
  style unattributed shadow-gate intents are excluded.
- **Data is sparse until incidents accumulate.** One incident is a data point, not a
  benchmark. `judge.py` will honestly emit `judge_unavailable` rather than invent a
  score; a run with only single-sided entries is expected early. Report what the
  data supports and mark what is not yet proven.
- **Residual blinding limit (documented):** perfect blinding across heterogeneous
  sources is impossible — the predecessor trajectory is a rich ReAct transcript
  while a TG row is a ~600-char conclusion + flags, so *content style* may still
  hint at origin even though labels, native scores, and jargon are stripped. Trust
  aligned-pair numbers more than cross-incident single-sided means.

### Persistent 0/0 miss on ONE host — check the LibreNMS rule scope + evaluator

A host that injects cleanly (`inject-result: OK real-used=90%`) yet polls to `TG=0
pred=0` every time is almost always a **LibreNMS detection** gap, not a TG/predecessor
triage gap — the alert never fired, so neither side is handed an incident. Two causes,
both seen in practice (2026-07-22):

1. **Rule scope.** The routing rule must actually *apply to* the guinea-pig. The
   memory rule `#25 "TG-84 guinea-pig High Memory >=85% (os-agnostic)"` is pinned via
   `alert_device_map` to specific `device_id`s. If a guinea-pig is missing from that
   map it is silently skipped (it will not even appear in the per-device rule list of
   `device:poll <host>`), so it can never pair. Both guinea-pigs (`dc1myspeed01`
   **and** `dc1librespeed01`) must be in the map:
   `INSERT INTO alert_device_map (rule_id, device_id) VALUES (25, <device_id>);`
   (Verify with `device:poll <host>` — the rule must show `Rule #25 (...): Status:
   ALERT`.) Note `#21 "Linux High Memory"` requires `os=linux`, but the LXC guests
   report `os=proxmox`, so only the os-agnostic `#25` covers them.
2. **Evaluator.** Only `lnms`/`php artisan device:poll <host>` runs the **AlertRules**
   evaluator (the "#### Start Alerts ####" phase that creates `alert_log`/`alerts`
   rows). Legacy `poller.php -h <host>` merely refreshes the polled data and never
   evaluates rules — so it cannot fire the alert. `force_detect()` uses `device:poll`
   for this reason; do not revert it to `poller.php -h`.

---

## Safety guarantees (both sides)

- **No mutation** on either system; both stay in shadow / `MUTATIONS=OFF`.
- **Read-only DB access** — predecessor `sqlite3 …?mode=ro`; TG `SELECT`-only via a
  SQL file + `docker cp` (no `sh -c`, no string-built query).
- **No secrets in output** — predecessor via `redact()`; TG via a `sed` redaction
  guard; the judge redacts every trajectory string before it reaches the prompt.
  Live credentials in the *source* logs are a separate estate finding to raise with
  the owner, not something this harness emits.
- **Judge key never leaves the box** — `judge.py` reads `LITELLM_MASTER_KEY` from
  `/srv/tg/deploy/.env` *inside* the remote shell and POSTs to `127.0.0.1:4000`; the
  key is never placed in this host's argv, env, stdout, or logs. The judge only
  READS its input JSON and POSTs a chat completion — it mutates nothing.
