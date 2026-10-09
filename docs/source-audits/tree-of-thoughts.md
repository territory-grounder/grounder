# Tree-of-Thoughts

> **[O] audit-overlay · TG-38 source benchmark.** Category: **Paper** · TG adherence: **2 / 5** · Practices audited: **8** (implemented 2 · partial 3 · gap 3 · N/A-by-design 0).

## Verdict

TG deliberately runs a SINGLE-path, fail-closed ReAct loop, so the core of this source — deliberate search over MULTIPLE reasoning branches with lookahead and backtrack — is largely unmet in-loop. The agent emits one directive per turn and returns the first valid proposal, and the model gateway sends no n/temperature and consumes only Choices[0] (adapters/model/model.go:58-62,108), so there is no per-incident branching, no candidate valuation, and no lookahead. TG does implement two adjacent pieces well: a faithful hardened ReAct base (reason-act-observe), and a genuine offline candidate-search-with-selection at the PROMPT level (flywheel: generate variants -> LLM-judge 5-dim valuation -> one-sided Welch t-test + min-lift + safety guard). Backtracking and self-correction exist only as narrow, grounding-scoped prune+retry (TrajectoryVeto, citation re-prompt); notably the Constitution promises self-consistency/hedging flagging (CONSTITUTION.md:116) that no code delivers.

## Practice-by-practice

| # | Advice / best practice | TG status | Evidence | Remediation |
|---|------------------------|-----------|----------|-------------|
| 1 | Branch into multiple reasoning paths — generate several intermediate steps / alternative solutions forming a tree, rather than one linear chain. | gap | agent/loop.go:192-312 (single-path ReAct loop, one directive per turn, first valid proposal returned at :274-280); adapters/model/model.go:58-62,108 (chatRequest carries no n/temperature; only Choices[0] consumed) | Add orchestrator-spawned candidate branches per incident (N diverse completions or competing root-cause hypotheses), selected mechanically so INV-08 holds (model tokens never become control flow). |
| 2 | Lookahead — evaluate multiple reasoning trajectories against a value function BEFORE finalizing an answer. | gap | agent/loop.go:213-280 (per-step self-reported confidence gate only; the first schema-valid proposal returns immediately, scored against no alternative); no candidate-evaluation code in agent/ | Before finalizing, dry-run the candidate proposal through core/predict + core/verify as a cheap lookahead and prefer the branch with the safest predicted verdict. |
| 3 | Backtracking — abandon a failing branch and return to an earlier node to try an alternative. | partial | agent/trajectory.go:54-77 (TrajectoryVeto prunes a stuck/looping/thrashing path); agent/loop.go:269-273,286-291 (citation re-prompt + grounded-stop nudge retry a bad step) — prune + single-step retry, never branch-to-alternative | Once multi-branch exists, on a pruned/failed branch resume from the last good node into an alternative branch instead of halting the whole run. |
| 4 | Self-correction / self-refinement — internally evaluate intermediate output, identify gaps/inaccuracies, and revise before finalizing. | partial | agent/loop.go:262-291 (mechanical citation gate re-prompts an ungrounded proposal; grounded-stop nudge fires at most once) — deterministic refinement scoped to grounding/citations only; general quality self-review is offline (core/judge/judge.go) | Extend the in-loop critique beyond citation-presence to a bounded quality self-review (does the op fit the diagnosis? is confidence consistent?) before the proposal is finalized. |
| 5 | Provide a state-evaluation valuation plus a selection mechanism (self-consistency / diverse beam search) that generates multiple candidates and picks the most optimal (inference-scaling law). | implemented | core/skillstore/flywheel.go:168-215 (generate multiple candidate variants); core/skillstore/trial.go:159-203 (FinalizeTrials: min-samples + min-lift + one-sided Welch p-threshold + asymmetric safety-regression guard + deterministic tie-break); core/judge/judge.go:23,48-59 (5-dimension LLM-judge valuation) | This is OFFLINE prompt-version search, not per-incident answer selection — reuse the same judge+Welch selection machinery in-loop to realize self-consistency over multiple answers. |
| 6 | Flag self-consistency / hedging mismatches across sampled reasoning as a consensus/selection signal. | gap | docs/CONSTITUTION.md:116 promises 'self-consistency and hedging mismatches are flagged'; grep for self-consist\|hedg across all *.go returns no implementation | Sample the model k times per incident, compare the resulting directives/proposals, and force POLL_PAUSE on disagreement — delivers the Constitution-promised signal and a ToT selection mechanism together. |
| 7 | Inference-time scaling — allocate a larger 'thinking budget' (more compute / more candidates) to harder problems. | partial | agent/loop.go:100-101 (flat DefaultLimits 5/10 cycles) and :192 (iterative multi-cycle ReAct is real test-time compute) but fixed-ceiling and single-candidate (adapters/model/model.go:108 takes Choices[0]) | Scale the cycle/candidate budget by severity or novelty (a critical or ood:novel-incident earns more branches/cycles) instead of a flat 5/10 cap. |
| 8 | Interleave reason->act->observe and dynamically adapt the plan from real feedback (ReAct — the practical realization ToT builds on). | implemented | agent/loop.go:173-254 (Thought / Action(read-only tool) / Observation appended as delimited DATA, iterate; INV-08 hardened); agent/README.md | — |

## Top gaps

- Implement the Constitution-promised self-consistency / hedging-mismatch flagging (multi-sample per incident, flag disagreement, force POLL_PAUSE)
- Add orchestrator-driven multi-hypothesis diagnosis: generate N candidate branches per incident and select mechanically (INV-08-safe)
- Per-incident lookahead: score candidate proposals through the predict/verify path before finalizing
- Adaptive inference budget scaled by severity/novelty instead of the flat 5/10 cycle cap

---

[← Source-benchmark catalog](../SOURCE-BENCHMARK-CATALOG.md) · [Audit index](README.md)
