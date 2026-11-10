# TG-491 — Honest retrieval-quality evaluation (options + chosen design)

> **[R] design memo.** Answers "how do we measure whether TG's memory retrieval is any good, without a
> machine inventing its own relevance answer key?" Drawn from the two retrieval distillates
> (`docs/source-audits/openai-retrieval-guide.md`, `.../agentic-rag-survey-2501-09136.md`), the
> predecessor's mechanism (`docs/PREDECESSOR-MECHANISM-INVENTORY.md`), and TG's eval canon
> (`docs/TESTING-AND-BENCHMARK.md`). Built + tested: `eval/retrieval/resolution_recall.go` (+ test) —
> a public-API consumer of the retriever, deliberately outside the `core/knowledge/` behavior plane.

## The problem
`eval/retrieval/production-queries.json` has 227 `(host, alert_rule)` queries with `must_retrieve_any_of`
**deliberately empty** — a machine-invented gold label would make hit@k measure the labeller, not the
retriever, and the merged artifact-law test (`core/knowledge/production_queries_test.go`) reds the build
if a machine writes them. Outcome-derived (op_class) labels were **measured 96% recoverable** from
(host, alert_rule) and rejected. So the only signal shipped was tie-saturation (distinctness) — which
measures the corpus, not the retriever.

## Options considered

**A. `must_contain` mechanical grader (openai-evals-guide, "per-incident must_contain assertions").**
Add a human-authored field to each query row; grade by string-check, not opinion:
```jsonc
{
  "host": "dc1ghostfolio01",
  "alert_rule": "Service-up/down",
  "must_contain": ["restart", "systemctl"],   // expected fact(s) a RELEVANT hit's resolution must contain
  "must_not_surface": ["pred-ik-1620"]         // optional adversarial near-miss (already tracked)
}
```
Grader: a query "hits" iff any top-k retrieved incident's `resolution`+`summary` contains any `must_contain`
string (case-folded). Honest **only if a named human who knows the estate writes the facts** — cheap
per-query (a fact, not a full relevance judgement), but it does need a human, at scale, for 227 rows.

**B. Outcome-ablation (`docs/TESTING-AND-BENCHMARK.md`, the RAG-disabled control).** Run TG with memory
ON vs OFF over incidents; if ON doesn't beat OFF on whole-trajectory outcome, retrieval isn't paying for
itself. Label-free and whole-system — but needs the agent loop + gateway (heavy), and is a *system*
metric, not a *retrieval* metric.

**C. Leave-one-out RESOLUTION-RECALL (chosen).** `must_contain` where the expected fact is
**auto-derived from the incident's OWN recorded human `resolution`** — a ground-truth label (1) no one
invented and (2) the retriever never scores on (`retriever.go` ranks rule/host/site/tags/summary+recency;
`Resolution` is rendered but not scored). For each incident, hold it out, query with its own signature,
and check whether any top-k hit carries the same recorded fix. This is A's mechanism with B's no-human
honesty, scoped to retrieval and runnable AFK over the shipped corpus.

## Chosen design & rationale
**Resolution-recall (C)** is the best fit: honest (recorded resolutions, not invented), **non-circular**
(the graded field is not a scoring channel), needs **no hand-labels**, and is a pure, deterministic,
reproducible Go test over the real `deploy/knowledge/corpus.seed.json` — no gateway, no live model. It
answers the thing the corpus exists for: *when this alert fires again, would memory surface a past
incident with the applicable fix?* It also **punishes** (not rewards) the known tie bug: where tied
same-(host,rule) rows carry different fixes, the alphabetical cut drops the applicable one and recall
falls. A **circularity guard** is reported alongside — the recall a host+rule-ONLY ranker would get — so
the number can never quietly become a restatement of "same host, same rule".

## Measured result (2026-08-16, shipped 140-row seed, k=3, match ≥0.5 shared tokens)
| metric | value | meaning |
|---|---|---|
| **of-findable recall@3** | **0.933** | of incidents whose fix IS recoverable, the fraction the retriever surfaced |
| ceiling | 0.857 | 14% of fixes are novel — no same-fix peer exists (corpus limit, not retriever fault) |
| gap (miss rate) | 0.057 | findable applicable precedent the retriever failed to surface (the tie-bug cost) |
| **trivial host+rule-only baseline** | **0.908** | recall of a degenerate exact-match-only ranker |

**Reading:** TG's memory surfaces the applicable prior fix **93%** of the time it's recoverable — genuinely
good. The extra channels (summary/tags/recency) earn **~+2.5 points** over pure host+rule exact-match, so
the retriever is *host+rule-dominated but not circular* — the honest answer to the "is it just matching
the hostname?" worry TG-491 was right to raise. The 5.7% gap is real headroom (the tie bug); the 14%
ceiling is a corpus-coverage ceiling, not a retriever defect.

## Disposition
- `must_retrieve_any_of` **stays empty** (correct; the artifact-law is right). No machine labels written.
- Retrieval quality is now measured honestly by **resolution-recall**, ratcheted at of-findable ≥ 0.90 in
  `eval/retrieval/resolution_recall_test.go` (raise-only, mirroring the saturation ratchet).
- **Optional future layer:** a small human-authored `must_contain` hard-set (option A, ~50 rows) for a
  true per-query hit@k, and option B (outcome-ablation) once it's cheap to run — both complement, neither
  blocks. TG-491 can close on this footing: the retriever is measured, honestly, and the number moves the
  right way when the retriever improves.
