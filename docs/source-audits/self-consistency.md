# Self-Consistency

> **[O] audit-overlay · TG-38 source benchmark.** Category: **Paper** · TG adherence: **2 / 5** · Practices audited: **6** (implemented 1 · partial 3 · gap 2 · N/A-by-design 0).

## Verdict

TG does not implement the paper's actual technique anywhere in its live decision path: every LLM call posts a single completion (no `n`, no temperature/diverse sampling) and consumes only Choices[0], and the agent runs exactly ONE ReAct trajectory per incident, so there is no multi-path sampling and no inference-time majority vote. What TG does have serves the same robustness GOAL through different, arguably stronger mechanisms: an independent deterministic verifier the acting LLM may never self-adjudicate (INV-10), a separate LLM judge over the session record, INV-08 (no model token becomes control flow), and HITL approval. It also does 'generate multiple candidates + statistical selection', but at the offline skill/prompt-graduation layer (Welch t-test A/B trials), not per query. Verdict: the paper's headline technique (sample multiple reasoning paths + majority vote) is a genuine gap; the robustness intent is partially met by deterministic/independent-check substitutes.

## Practice-by-practice

| # | Advice / best practice | TG status | Evidence | Remediation |
|---|------------------------|-----------|----------|-------------|
| 1 | For a single high-value decision, sample MULTIPLE independent reasoning paths (diverse samples via temperature/beam), rather than committing to one stochastic generation. | gap | adapters/model/model.go:58 (chatRequest = Model/Messages/User only, no n/temperature) + :108 (returns Choices[0]); agent/loop.go:194 (one Complete per cycle, single trajectory) | Add a bounded self-consistency sampler (N diverse samples at a raised temperature) for the diagnosis/risk-band decision, all samples still entering as typed DATA behind the existing gates (INV-08 preserved). |
| 2 | Aggregate the sampled paths by majority vote / a selection mechanism to pick the most consistent (robust) answer. | gap | none - no inference-time aggregation across samples exists in the agent, judge (core/judge/judge.go scores one session once), or verify path | Majority-vote over the sampled diagnoses/proposals; feed only the agreed result into REQ-1007/risk gates, and treat disagreement as a confidence penalty that forces escalate/POLL_PAUSE. |
| 3 | Do not trust a single model's self-adjudication of its own output; verify robustness with an independent check. | implemented | core/verify/verdict.go:1-8,52 (deterministic verifier is SOLE verdict writer, acting LLM never adjudicates, INV-10); core/judge/judge.go:7-9 (a SEPARATE judge adjudicates the record, read-only); agent/loop.go:227-301 + adapters/model/provider.go:6 (INV-08, no model token becomes control flow) | — |
| 4 | Generate multiple candidate outputs, then apply a selection mechanism to identify the most optimal (Inference Scaling Law). | partial | core/skillstore/welch.go:113 (one-sided Welch t-test selects a winning arm vs concurrent control); temporal/skilltrial/finalizer.go:50-53 (graduate the winner, reject losers) | This is OFFLINE skill/prompt selection aggregated over many sessions, not per-query self-consistency; extend the multi-candidate + selection principle to inference time for the individual incident decision. |
| 5 | Use a voting/approval consensus so no single fallible signal is load-bearing for a risky decision (maps to TG voting/approval). | partial | temporal/runner/workflow.go:302-320 (first vote whose ActionID matches resolves - single approver, not M-of-N); core/safety/safety.go:107 (POLL_PAUSE band); adapters/notifier/notifier.go:20-23 (Vote bound to its decision, INV-12) | This is single-approver HITL (a human vote, not model self-consistency, and not a majority quorum); add an optional M-of-N approver quorum for the highest-risk bands so no single approver is load-bearing. |
| 6 | Spend more inference-time compute ('thinking budget') on harder problems to raise robustness. | partial | agent/loop.go:100-101 (bounded ReAct cycles - extended tool-use inference, escalate at 5 / halt at 10); core/execclass/execclass.go:64-85 (critical/ambiguous/high-criticality incidents route to the thorough/human path) | Compute is scaled by iterative tool use + risk routing, not by sample-and-vote; allocate the extra budget specifically as self-consistency samples for execclass 'thorough' incidents. |

## Top gaps

- Self-consistency sampler: N diverse samples + majority vote on the agent's diagnosis/risk band before the deterministic gate
- Sample-agreement-calibrated confidence (replace the single self-reported CONFIDENCE scalar)
- Independent second-model concurrence required before an AUTO band on higher-risk proposals

---

[← Source-benchmark catalog](../SOURCE-BENCHMARK-CATALOG.md) · [Audit index](README.md)
