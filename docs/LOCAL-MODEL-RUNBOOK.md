> **NOT A WORK QUEUE — reference only.** The one authoritative queue is
> [`BOARD.md`](BOARD.md): work that, top-down. Anything below that reads as a priority, a ranking, a
> "first step" or a "do this next" is a RECORD OF WHAT WAS THOUGHT AT THE TIME and does not steer.
> The complete inventory of open work is YouTrack `project: TG #Unresolved` — the board is the ranked
> queue, not the inventory, and its silence is not closure.
>
> *Why this banner exists: on 2026-08-02 seventeen files in this repo told a reader what to do next. One
> of them had listed "Wire `VoteAdmission`" as a named GATE; that file was later quarantined for being
> unreadable and the gate went with it, leaving the approval control enforced by nothing from July to
> August (TG-254). A second authority is how a finding gets lost.*

# Local-model runbook — gpu01 recon, ladder plan, and the gated migration path

*2026-07-31, recovery follow-through (a)–(d). Facts below verified live, not assumed.*

## (b) The hardware, recon'd

`dc1gpu01`: **RTX 3090 Ti, 24 GB VRAM** (≈17 GB in use at recon time), 31 GB RAM, 185 GB free
disk. Ollama 0.32.5 runs **as a container in host-network mode** (`:11434`, reachable from the TG
box — verified). Co-tenants that hold VRAM: the predecessor's stack keeps `qwen2.5:7b` (~4.9 GB)
and `nomic-embed-text` (~0.3 GB) warm; Plex and friends also live here. Treat **~14–18 GB as the
honest planning envelope**, not 24.

Model library already pulled: `phi4` (14B, 9.1 GB), `deepseek-r1:14b` (9.0 GB),
`qwen2.5-coder:14b` (9.0 GB), `codellama:13b`, `llava:13b`, `bge-m3`, `llama3.2:1b`, and the
loaded pair above.

## (a) Embeddings — DONE (TG-211)

`embed-nomic` → `ollama/nomic-embed-text` via `os.environ/TG_OLLAMA_BASE` (no estate literal in
the repo; STONITH-clean). The estate already served this exact 768-dim model for the predecessor —
TG now embeds against the same endpoint; incident text never leaves the network. Box flipped
2026-07-31 (`TG_EMBED_MODEL=embed-nomic`, `TG_EMBED_DIM=768`); backfill sweeps every 10 min.
Eval evidence rides the nightly trend-watch record (`evidence_grounded` must hold — TG-211's
done-shape).

## OWNER DECISION 2026-07-31 — gpu01 is the AUXILIARY plane, not the brain

> The owner has settled the question this runbook left gated: **gpu01 serves small auxiliary
> models only** (embeddings — live; potentially future small helpers such as a TG-214 reranker).
> **Serious agentic work — triage, investigation, decisions, and the judge — stays on external
> API LLMs.** The agent-A/B and judge-migration sections below are PARKED as owner-declined
> reference material; they are not a backlog, and no session should arm them without a new,
> explicit owner instruction. The recon math itself supports the decision: the 14–18 GB envelope
> caps local candidates at ~14B-class models, while TG's open diagnosis gap vs the predecessor
> exists even on frontier API models — a local brain would risk widening the one number Phase D
> measures.

## (c) Agent-model ladder — PARKED (owner-declined); kept as reference only

**Shortlist for the A/B, in order:**
1. **`phi4` (14B)** — primary candidate: strong general reasoning, already pulled, fits beside
   co-tenants (9.1 GB).
2. **`qwen2.5:14b-instruct`** (pull needed, ~9 GB) — strong tool-following; the coder variant on
   disk is NOT the instruct model TG needs.
3. `deepseek-r1:14b` — reasoning-heavy but verbose/slow for a tool loop; secondary.
4. `qwen2.5:32b-instruct-q4` (~19 GB) — only viable if co-tenant load is rescheduled; park it.

**The design is a LADDER, not a swap:** add an inert `primary-local` entry
(`ollama/<candidate>` via `os.environ/TG_OLLAMA_BASE`) and, when it earns its place, make it the
first rung with the current cloud `primary` as fallback — availability preserved, egress/cost cut
on the common case.

**The gate is the law:** the candidate flips nothing until `eval/eval-gate.sh change` passes with
the candidate arm running `primary-local` — TG already *loses* diagnosis quality to the
predecessor; an unmeasured swap to a 14B model could widen exactly the gap Phase D measures. Run
the A/B when the gateway is quiet (the flock lock serializes with the nightlies). **Timing rule:**
the flip is cleanest while confirmatory accrual is ~0 (system-under-test homogeneity); if pairs
have accrued, the flip waits for the verdict.

## (d) Judge migration — PARKED (owner-declined); calibration harness kept as measurement tooling

The LLM-judge anchors every score (eval history, baseline, head-to-head pairs). It stays **fixed
until the Phase-D verdict**. What runs now: `tools/shadowbench/judge-calibrate.py` re-judges an
already-judged sample with a candidate local judge and reports per-dimension agreement (weighted
kappa, within-1 %, signed delta) against the frozen primary. **Proposed** migration bar (owner
ratifies): pooled weighted κ ≥ 0.75, no dimension κ < 0.6, mean signed delta within ±0.25.
Migration itself = a new judge model entry + a dated cut line in the quality record, never a
silent swap.

## Sequencing (the whole point)

embeddings (done) → candidate A/B under the gate (gateway-quiet window) → ladder flip while
accrual ≈ 0 or post-verdict → judge calibration report → judge migration after the verdict.
Every step leaves a record (eval/history, calibration JSON, this file).
