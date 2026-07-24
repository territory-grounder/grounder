# Contributing to Territory Grounder

Territory Grounder (TG) is Apache-2.0, developed in the open as a monorepo under the GitLab
group `products/territory-grounder`, flagship project `grounder`. This document is the
**build culture** — the way TG is developed, reviewed, and self-graded.

It is deliberately separated from the product runtime. The *runtime* analogs of these values —
mechanical verdicts the acting agent can never write, markers parsed-not-trusted-as-authority,
agent output verified against live source, and safety-critical-file↔spec drift guarding — are
**first-class in the product constitution** and must never be dropped when the culture half is
relocated here [R paradigm-rule 10; overlay S8-preserve-meta]. If you are looking for how the
*system* behaves, read [docs/CONSTITUTION.md](docs/CONSTITUTION.md) and
[docs/00-README.md](docs/00-README.md). This file is about how *we* behave.

---

## 1. Honesty over marketing

Grade weak work honestly and never inflate. The predecessor's self-assessments were
deliberately critical — "complete control overstates it" was written about the system's own
control model — and that habit is load-bearing, not decoration [F, honesty principle].

- Write down known limitations plainly. A documented gap is an asset; a hidden one is a
  liability that will be found by an attacker or an outage instead of a reviewer.
- Distinguish *intent* from *binding*. The predecessor's safety story was "real in intent but
  false in binding" — every trust boundary leaked because a control's *existence* was mistaken
  for its *enforcement* [O verdict]. Claims of safety are claims about bindings; state them
  that way.
- No headline number without a method behind it. Every generated count, scorecard, or coverage
  figure carries a non-null `generated_at`, a source hash, and a coverage scope, or it does not
  ship [O, INV-15/22, M-14].

## 2. Deliberately critical self-scorecards → remediation sprints

TG grades itself against external frameworks and treats the results as work, not trophies.
The predecessor ran repeated scorecards (Gulli, Claude Certified Architect, NVIDIA DLI, OpenAI
Agents SDK, Anthropic *Building Effective Agents*) and, crucially, followed each with a
**remediation sprint** rather than a press release [F, honesty principle].

- A weak dimension is graded C or D on purpose. Rounding a C up to a B is a process failure.
- Every scorecard produces a tracked remediation backlog. The scorecard is not "done" when it
  is scored; it is done when the gaps it found are closed or explicitly, auditably accepted.
- Synthetic self-scoring is a **smoke test only**, never a deployment authority. A green
  synthetic scorecard over synthetic incidents that exercises none of the real trust boundaries
  is *theatre* and is prohibited as a release signal [O, INV-22, M-02/M-14].

## 3. Verify agent-generated claims against the live source (in audits)

Treat any doc, count, audit finding, or "X is broken/untracked/dark" produced by a sub-agent or
tool as **untrusted until checked against the live source of record** [F/R paradigm-rule 10].
This rule exists because a real cross-audit's *headline* finding — "git-untracked config" — was
simply false: it had run its check against a diverged working branch, not the mainline.

- When reviewing an agent-authored change, re-derive every concrete claim (section number,
  count, file path, metric, status) from the owning file or live system before merging it.
  Fabricated specifics read as authoritative and are invisible in a diff.
- Ask doc-generators to read the *owning* file for counts, never the prompt.
- This is the *build-time* twin of the runtime invariant that model output enters the system
  only as typed, validated data and never as authority [O, INV-08/INV-11]. Keep both.

## 4. Spec ↔ code lockstep is a build gate

Safety-critical code stays in **provable lockstep** with its EARS specification. This is not
hygiene advice — it is an enforced gate [F, spec/007; O, INV-22].

- Every safety-critical Go file is bound to its owning spec (`spec/001`–`spec/007`) in a
  content-hash manifest (`.lockstep.lock`). Changing a governed file **without** updating its
  spec is reported as drift and fails CI [F, spec/007 REQ-701/702].
- The drift check is **semantic**: a cosmetic comment edit cannot clear genuine drift, and a
  re-stamp is an explicit, audited operator action [F, spec/007 REQ-703/704].
- The manifest may **not** exclude governed files. The predecessor's lockstep once excluded 11
  of 12 governed files and several acceptance steps were no-op `pass` — that is exactly the
  hole INV-22 closes. Governed code cannot be excluded from the runnable suite, and
  source-string / schema-presence-only assertions do not count as coverage for safety-critical
  behavior [O, INV-22, M-02].
- CI additionally bans the injection/bypass class *at the source*: no `sh -c` or shell-built
  commands [INV-02], no string-built SQL or manual quote-escaping [INV-03], no credential
  literals (gitleaks) [INV-13], and a `find_dead_code` gate plus a grep gate forbidding retired
  identifiers so nothing ships retired-but-present [INV-17]. A contributor cannot merge past
  these; they are structural, not reviewer discretion.

## 5. No fine-tuning (ADR)

Behavioral adaptation is **prompt-policy iteration + RAG, never model weight updates** — an
explicit, standing Architecture Decision Record forbids fine-tuning [F, no-fine-tuning ADR; see
docs/adr/]. The handbook order is: prompt-engineering → RAG → (fine-tune *only* if a sealed-
holdout evaluation proves it necessary).

- Policy change is **externally judged**: an LLM-judge jury plus one-sided Welch-t A/B
  prompt-patch trials supply ground truth from *outside* the generator. No self-grading.
- The only honest quality signal is a **sealed holdout the system may never tune to**.
  Improving the regression set while the holdout stagnates, or a >20-point regression-vs-holdout
  gap, is *defined* as overfitting failure — not a win [F, eval flywheel].
- Proposing a fine-tune is proposing to amend an ADR. Bring the holdout evidence.

## 6. How to propose a module

Every integration surface — ingest / tracker / notifier+approval / CMDB / actuation /
model-provider / observability — is a **loadable, unloadable module**, not hardcoded [R
paradigm-rule 3; corrections]. `adapters/` holds the module **interfaces**; `modules/` holds
loadable **implementations** plus a small reference-adapter set and an SDK. The default
mechanism is **out-of-process governed plugins** (separate process/container over a stable
protocol — gRPC / HashiCorp go-plugin; **MCP** for tool/actuation modules), per **ADR-0005**.

To propose a new module:

1. **Pick the surface and target an interface.** Implement the stable interface in `adapters/`
   for that surface; do not add a bespoke code path in the core. NL and GR (or any two
   deployments) are two config rows, not two forks — there is exactly one implementation of
   each pipeline stage / source type [O, INV-18].
2. **Govern it by construction.** Modules are signed, capability-scoped, and per-tenant-enabled.
   A disabled or unregistered module must have **no execution path** — that is the whole point;
   it kills the "dead OpenClaw path still executable" class [R corrections; O, INV-17]. A
   capability exists only if its adapter is compiled in and explicitly registered at startup.
3. **Stay inside the mechanical safety core.** A module never gains its own effect channel, never
   builds a command by string interpolation (fixed argv / validated stdin-JSON only) [INV-02],
   never touches the DB except via parameterized queries [INV-03], and traverses the wired-by-
   construction pre-execution policy gate for every actuation [INV-21]. Actuation identity is
   per-tenant + per-agent scoped (mTLS / scoped tokens), never a shared key [INV-13; paradigm].
4. **Bring the contract and the tests.** Ship the generated OpenAPI/AsyncAPI/JSON-Schema for the
   module's surface from its single typed source [INV-15], plus at least one **adversarial** test
   per trust boundary it introduces, drawn from the shared negative-fixture/fuzz corpus
   (metacharacters, separators, newlines, Unicode, oversized, duplicate/replay) [INV-22].
5. **Open an ADR when it is a mechanism decision.** New module *mechanisms* (not just new module
   *instances*) — e.g. a WASM/Extism plugin host — are ADR-worthy. Reference ADR-0005 and state
   the trade-off.

## 7. Working agreements

- **Branch, then MR.** `feature/<description>` or `fix/<description>`; never push to `main`.
- **Provenance tags in docs.** Tag substantive claims `[F]` / `[R]` / `[O]` with their source
  ids (`INV-NN`, `spec/00x`, `paradigm-rule N`). This keeps the three-layer model auditable and
  prevents silently re-inverting the layering [R corrections, layer-tag convention].
- **Keep terminology identical across documents.** `ActionManifest`; the execution classes
  `DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED`; the three
  autonomy bands `AUTO / AUTO_NOTICE / POLL_PAUSE`; the inviolable mechanical safety core; the
  module system; Temporal; the LiteLLM model-gateway; `tenant_id` + RLS. Do not coin synonyms.
- **Multi-tenant by default.** Any new state/memory/audit/eval/cost/graph/session table carries
  `tenant_id` and is isolated by Postgres row-level security; no adapter, credential, rollback
  command, or retrieval query may cross a tenant boundary [R paradigm-rule 1].
