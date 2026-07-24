# Benchmark Ladder — escalating fault-injection head-to-head (TG vs the predecessor)

*Live A/B benchmark plan. The two agentic systems — **Territory Grounder** and the predecessor **claude-gateway**,
both currently MUTATIONS=OFF (shadow) — reason on the SAME live estate. We inject progressively harder faults on
guinea-pigs, blind-judge both systems at each rung, and climb until cascading multi-system failure. The endgame is
TG flipping mutation-ON (staged canary) while the predecessor stays read-only by design — the demonstrable
**exceed**. Companion to [`predecessor-shadow-mode benchmark`], `tools/shadowbench/`, and the SREGym external-benchmark
decision (YT TG-68).*

## Why a ladder (and not just "run the benchmark")

Easy faults **saturate**: a single powered-off device has exactly one correct triage — a grounded stand-down — and
both systems nail it (first real pair: **5.0 = 5.0 tie**). That measures "do you correctly refuse to act on an
obviously-down box," not "who is the better SRE." The discriminating signal lives in the harder rungs: an
action-warranting fault forces a **proposal + a falsifiable prediction**; a **cascade** forces **root-cause reasoning
under correlated noise** — the single skill that most separates a good SRE agent from a mediocre one.

## THE OBSERVABILITY GUARANTEE (division of responsibility)

**The decision to trigger a real outage — including restarting a live Proxmox host or switch — is the OWNER's risk
call, not a gate this system imposes.** The agent's job during any fault (especially the outage tiers) is a hard
guarantee:

> **Both systems MUST be actively ingesting AND processing the alerts the fault produces, for the full duration of
> the fault.** Pre-flight: both ingest pipelines proven live + burst-safe (a cascade is a flood — neither system may
> silently drop it to dedup/flap/burst-suppression). During: a live dual-system watcher confirms both are ingesting +
> triaging in real time, so a dropout is caught immediately, not discovered afterward. Post: capture the aligned pair
> and blind-judge it.

If either system is not provably capturing + reasoning about the fault, the run is **not a valid benchmark** and must
be re-flagged before (or aborted during) the outage — the failure mode to prevent is a real outage that produces **no
comparable data**.

## The ladder

| Tier | Fault | Discriminates | Guinea-pig / blast radius | Enabling capability (must land first) | Status |
|:-:|---|---|---|---|---|
| **0** | Single device down | Correct grounded **stand-down** | librespeed / myspeed (reversible) | ingest works | ✅ done — 5.0 tie |
| **1** | Single **action-warranting** (disk-fill, memory pressure, stuck service) | The **propose** path + a **falsifiable prediction** (prune → free rose) | librespeed disk-fill (reversible) | **crit-3 skills fix** (propose-rate) | ⏳ next |
| **2** | Ambiguous / multi-signal on one host (disk + load) | Root-**cause selection**; conservative single-cause discipline | one guinea-pig | Tier-1 capability + tool-schema (TG-69 crit 4) | queued |
| **3** | **Cascade** — restart a Proxmox host → all guests down + their services alert | **Shared upstream cause** vs symptom-shotgun; correct **escalate** (host reboot = hard-floor) | **owner's risk** (test node or live host, owner's call) | **estate causal graph** (`get-estate-context`/BlastRadius) + correlated-triage skill | gated on capability + owner trigger |
| **4** | Cross-system cascade — switch/uplink restart → partition → multi-device + VPN + monitoring blind-spots | Full-estate reasoning; graceful degradation; "path broken, don't restart the endpoint" | **owner's risk** | Tier-3 capability + robustness (TG-69 crit 6/7) | endgame fault |

## Capability lockstep (each rung needs its match, or it ties again)

A harder fault only discriminates if the agent can *engage* with it; otherwise both systems escalate and you get
another tie. The ladder therefore advances **in lockstep** with capability landing:

- **Tier 1 ← crit-3 skills fix** (YT TG-69): re-author the ported chatops skills to TG's read-only JSON wire format so
  the propose-rate rises (`loop.go:16` documents the old skills produced 0% proposals). *In flight, eval-gating.*
- **Tier 3–4 ← the estate causal graph** + correlated-triage / tree-of-thought skills: without a populated topology a
  cascade is just "many alerts" and the agent cannot find the upstream cause. **First job before Tier 3: prove the
  graph is populated AND the cascade skill fires.**
- **Clean `falsifiable_prediction` scoring ← #36 / TG-61** (prediction plumbing, sibling-owned).
- **Robustness under nastier inputs ← the TG-69 backlog**: tool-schema (crit 4), token/history compaction (crit 7),
  screening tool *outputs* (crit 6).

## The endgame — the real "exceed"

Both systems are MUTATIONS=OFF, so the ladder compares only **diagnosis + proposal quality**; matching the predecessor
there is table stakes. The demonstrable exceed is **YT #23: mutation-ON, staged canary** — TG actually *remediates* a
narrow, reversible, non-critical fault (e.g. `prune-disk` on a guinea-pig), breaker armed, monitored — while the
predecessor stays read-only *by design*. The ladder + crit-3 + the TG-69 remediations are the evidence that earns the
flip. Never a blind flip.

## Benchmark method

`tools/shadowbench/` extracts both systems' trajectories for the SAME incident, strips native scores, neutralizes
system-identifying jargon, and blind-judges each on TG's five eval dimensions (correct_diagnosis, evidence_grounded,
sensible_proposal, appropriate_band, falsifiable_prediction) in randomized A/B order, over the box LiteLLM judge. The
harness is calibrated (bias 0.00 vs TG's own judge) and applies the hollow-proposal + falsifiable-N/A guards. **Per-tier
tagging** is added so results read "at Tier N, TG vs predecessor = X." Keep the harness honest per the Agentic Benchmark
Checklist (arXiv 2507.02825) as it scales.

## Outage-readiness protocol (Tiers 3–4)

1. **Pre-flight:** confirm TG ingest live on both sites (operations-map fix) + the predecessor's Chatops-n8n path live;
   verify neither drops a correlated **burst** (TG admission dedup/flap/burst-correlate; predecessor n8n dedup) — a
   cascade must be captured + correlated, not silently suppressed.
2. **During:** run a live dual-system watcher (TG `session_triage` + predecessor gateway.db / shadow log) that asserts
   both are ingesting + triaging in real time for the outage window.
3. **Post:** extract the aligned cascade pair → `judge.py` → record the per-tier score; note any dropped/degraded arm.

## Status & next

- **In flight:** crit-3 skills fix — binding eval gate running (`eval/eval-gate.sh change`). PASS → merge → deploy.
- **Next (Tier 1):** disk-fill librespeed → confirm TG now proposes + predicts → first *discriminating* pair.
- **Pre-Tier-3:** audit the estate causal graph is populated + the correlated-triage skill fires; wire the live
  dual-system outage watcher.
- Tracked as a YT epic with the tiers as child issues.
