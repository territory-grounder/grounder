# Prior-Art & Differentiation — Decision-Tracer and Federation

> **Status: RESEARCH CAPTURE (competitive prior-art scan, not a task-bearing spec).** This document
> is the formal record of already-completed competitive research on TG's two owner-approved ideas:
> the **decision-tracer** and **federation of sovereign instances**. It bears no `tasks.json`, no
> acceptance oracles, and no build obligation; it exists so the landscape findings cannot drift and
> can be cited when framing TG's differentiation, positioning, and IP posture.

> ⚠️ **VERIFICATION CAVEAT — READ FIRST.** Compiled by automated web research (7 agents, ~90 web
> lookups). Product-level claims are well-grounded, but specific paper identifiers (arXiv IDs) and
> some URLs are agent-surfaced and **UNVERIFIED** — verify before external citation or IP reliance.
> This is a landscape scan, **NOT** a formal freedom-to-operate (FTO) opinion; obtain patent-counsel
> FTO before any commercialization or public claim of invention.

*Provenance tags follow the house three-layer model (see `00-README.md`): **[F]** foundation
inherited from the predecessor, **[R]** the single-organization product reframe, **[O]** the audit
hardening / invariants. This capture is **[R+O]** research grounding for the decision-tracer and the
federation north-star (`docs/FEDERATION-VISION.md`); it introduces no new obligation and relaxes
nothing in `CONSTITUTION.md`.*

---

# Part I — Decision-Tracer

# PRIOR-ART & DIFFERENTIATION REPORT — Territory Grounder Decision-Tracer

*Synthesized from three research briefs (LLM-observability, policy/audit/explainability, AI-SRE). Every claim is grounded in the briefs; URLs cited where the briefs gave them.*

TG's decision-tracer, restated for scoring: an **ASA-Packet-Tracer-style per-workflow step-through inspector** for a governed-autonomy SRE control plane. For queued/live/executed workflows you click each step and see the matched policy **rule + rationale**, tools, prompts, skills, agent **confidence + reason**, the **access-list in force**, and the **credential identity** — over a **tamper-resistant ledger**, archived as an **outcome-paired learning corpus**. Claimed novelty: unifying LLM-agent reasoning + a policy/ACL decision-walk + credential resolution + a mechanical verify verdict into **one governance packet-trace**.

---

## 1. Competitive LANDSCAPE table

Governance-packet-trace coverage judged on TG's exact axis: does the product model, as first-class inspectable objects, the {matched rule · ACL/floor · mode · graduation · credential identity · approval/verify verdict} fused onto the execution/reasoning trace?

| Product | Category | What it traces | Covers TG's governance-packet-trace? | On-prem / OSS |
|---|---|---|---|---|
| **Langfuse** | LLM observability | prompt/completion, tokens, cost, tool-calls, retrieval, latency, evals | No | Yes — OSS, self-host |
| **LangSmith** | LLM observability | full run tree, tool calls, reasoning steps, trajectory evals; RBAC on store | No (RBAC ≠ decision provenance) | Partial — self-host Enterprise only |
| **Arize Phoenix** | LLM observability | OTel/OpenInference spans, LLM/retrieval/agent steps, evals | No | Yes — OSS, fully local |
| **Helicone** | LLM observability | per-call prompt/response, tokens, cost, sessions, scoring | No | Yes — OSS, Docker/K8s |
| **W&B Weave** | LLM observability | call tree, prompts/completions/tool calls, eval leaderboards | No | Partial — enterprise self-host |
| **Braintrust** | LLM eval/obs | per-call traces, scorers, trace search/compare | No | Partial — hybrid, not OSS |
| **Traceloop / OpenLLMetry** | OTel exporter | prompts/completions, tokens, model/version | No | Yes — OSS SDK |
| **Datadog LLM/Agent Obs.** | LLM observability | workflow+LLM/tool/retrieval spans; guardrails, PII redaction, RBAC | No (guardrails/RBAC ≠ authz provenance) | No — SaaS only |
| **New Relic AI Monitoring** | LLM observability | prompt-response pairs, tokens, agent spans in APM | No | No — SaaS |
| **Pydantic Logfire** | LLM observability | OTel traces, agent behavior, tool-call inspection, SQL over traces | No | Partial — closed platform |
| **Comet Opik** | LLM observability | trace/span tree, evals; guardrails (block violations, PII) | No (guardrail firing ≠ authz/mode/graduation) | Yes — Apache-2.0 |
| **HoneyHive** | LLM observability | OTel agent traces, online evaluators, misuse/loop/PII detection | No | Partial — Enterprise self-host |
| **OTel GenAI SemConv** | Open standard | model/request/response/token/tool-call span attrs | **No — spec has no authz/policy/credential attribute** | Yes — open spec |
| **Cisco ASA Packet Tracer** | Network packet | phase-by-phase UN-NAT→ACL→NAT→VPN→FLOW → ALLOW/DROP | No — the *metaphor archetype*, wrong domain (packets, no LLM/credential) | On-box |
| **Palo Alto `test security-policy-match`** | Network policy | which rule matches a flow tuple | No (single match, no agent/credential) | On-box |
| **AWS IAM Policy Simulator** | IAM authz | per-action Allow/Deny + responsible statement | No (no reasoning/confidence/verify) | SaaS/API |
| **GCP Policy Troubleshooter** | IAM authz | per-binding allow/deny + overallAccessState | No (no agent dimension) | SaaS/API |
| **HashiCorp Sentinel** | Policy-as-code | per-rule Pass/Fail trace + file:line | No (apply-time IaC, no agent) | Self-host |
| **OPA decision logs / `--explain=full`** | Generic policy | per-decision input/policy/result; rego trace | No (debug text, no curated gate-walk, no agent fuse) | Yes — OSS |
| **Styra DAS / Enterprise OPA** | Generic policy | decision logs, rule tracing, Log Replay what-if | No | Self-host/SaaS |
| **Microsoft Agent Control Spec / Governance Toolkit** | Agent governance | 8 interception points, allow/warn/deny/escalate + evidence (policy version, why, upstream result) | **Partial — same primitives, but enforcement+evidence *schema*, not a step-through UI; no confidence viz, no packet-tracer walk** | OSS toolkit (github.com/microsoft/agent-governance-toolkit) |
| **PolicyLayer** | Agent tool-call gateway | inspect tool+args → allow/deny/hide/rate-limit/approve | No (enforcement gateway, not an inspector) | SaaS |
| **Streamkap "Decision Traces" pattern** | Agent audit *pattern* | trigger→context→reasoning(rules fired, confidence, constraints)→action→outcome, correlation-ID | **Partial — closest component set, but retrospective ledger pattern, not live gate-walk, not a product** | N/A (pattern) |
| **Temporal / Conductor / Camunda** | Workflow history | event-history timeline, replay, BPMN token flow | No — execution path, no authz gate/verdict | Yes — OSS/self-host |
| **AIOps: Moogsoft / BigPanda / ServiceNow ITOM / Dynatrace Davis** | AIOps RCA | why events grouped / root cause (causal graph) | No — RCA explainability, different axis | Mixed |
| **Shoreline.io** | AI-SRE actuator | remediation loop, Notebooks/runbooks; RBAC+rate controls | No (governance not per-decision-exposed) | SaaS/self-host |
| **Rootly** | AI-SRE actuator | "governance-by-design", approve-gate workflows, retrospective timelines | No (approval workflow-level, not per-decision trace) | SaaS |
| **NeuBird Hawkeye** | AI-SRE actuator | dual-model reasoning; "graduate to safeguarded automation" | No (graduation is a claim, not a per-op-class exposed inspector) | SaaS |
| **PagerDuty (Runbook/agentic)** | AI-SRE actuator | recommends actions vs runbooks; "governed automation" at job level | No | SaaS |
| **Datadog Bits AI SRE** | AI-SRE actuator | investigation output+evidence; "semi-supervised" mode + RBAC | No (coarse mode dial, not per-decision) | SaaS |
| **Cleric / Parity / Flip / Deductive / Causely / Traversal** | AI-SRE diagnose-only | causal/investigation reasoning traces, confidence | No (read-only → nothing to govern; trace is explanation, not gate) | Mixed |
| **Robusta / HolmesGPT** | AI-SRE (OSS) | evidence gathered pre-LLM; K8s remediation MCP w/ allow-listing | No (allow-list at MCP layer, not exposed decision trace) | Yes — OSS |
| **incident.io / FireHydrant** | Incident mgmt | real-time timeline + AI post-mortem/retrospective | No (post-hoc narrative, not live authz gate) | SaaS |

---

## 2. CLOSEST analogues — and exactly where they stop short

Four true nearest-neighbors, in descending closeness:

1. **Microsoft Agent Control Specification / Agent Governance Toolkit** (closest on *substance*) — techcommunity.microsoft.com/blog/linuxandopensourceblog/agent-governance-toolkit-architecture-deep-dive.../4510105, github.com/microsoft/agent-governance-toolkit. It defines the same primitives TG unifies: interception points, allow/warn/deny/escalate verdicts, evidence recording "which policy version decided, why allowed/stopped, upstream result," and runtime authorization that uses the agent's stated reasoning + cumulative trajectory to grant/withhold credentials. **Stops short:** it is explicitly an *enforcement + evidence schema*, not an interactive step-through inspector — no confidence visualization, no packet-tracer-style phase-by-phase walk, and it does not render a single fused visual trace.

2. **Streamkap "Decision Traces" pattern** (closest on *component set*) — streamkap.com/resources-and-guides/decision-traces-ai-agents. The record has every ingredient: trigger → context → reasoning (rules fired, **confidence scores**, policy constraints) → action → outcome, joined by correlation-ID. **Stops short:** it is a *retrospective audit-ledger pattern, not a live gate-stack walk*, and it is not a shipping product — no interactive per-decision inspector.

3. **Cisco ASA / Firepower Packet Tracer** (the *metaphor archetype*) — cisco.com/c/en/us/support/docs/security/adaptive-security-appliance-asa-software/217679-asa-access-control-list-configuration-ex.html. The gold standard for multi-phase step-through → verdict (UN-NAT→ACCESS-LIST→NAT→VPN→FLOW, each ALLOW/DROP with drop-reason). **Stops short:** wrong domain — network packets only; no LLM reasoning, no confidence, no credential identity, no learning corpus. The metaphor "packet tracer for an agent action" does **not** appear as a named product.

4. **Rootly + NeuBird** (closest single-vendor *SRE threats*) — rootly.com/ai-sre-guide/concepts, neubird.ai. Rootly ships "governance-by-design," approve-button workflows and retrospective timelines; NeuBird claims "graduate to safeguarded automation with policies and guardrails." **Stop short:** governance is coarse (org/mode/workflow-level), graduation is marketing posture or a global mode dial — neither exposes a **per-decision, per-op-class governed step-through** fusing reasoning × policy-verdict × confidence × blast-radius.

Non-neighbors worth naming so the differentiation is honest: **IAM authz tools** (AWS Simulator, GCP Policy Troubleshooter, Sentinel, OPA/Styra) already do per-decision "which rule/statement decided, and why" walks — but non-visual, no agent/credential/verify fuse. **AIOps** (Davis, BigPanda Open Box) solve RCA/correlation explainability — a *different axis*, no policy gate. **Workflow engines** (Temporal/Camunda) show execution history and replay — no authorization verdict.

---

## 3. TG's genuine DIFFERENTIATION (honest — including what is NOT novel)

**What is genuinely differentiated (the defensible core):** TG occupies the **union of three product categories that no single incumbent occupies** — LLM-observability (owns the execution/reasoning half), policy/authz-explainability (owns the decision-walk half), and AI-SRE actuation (owns the remediation half). The novelty is a **recombination, not a new primitive**: a single ASA-Packet-Tracer-style step-through that walks ONE governed agent action through a domain-specific gate-stack (ACL → min-confidence(0.6) → band → floor → mode → graduation → credential → verdict) **and adjoins the mechanical verify outcome**, across queued→live→executed states, over a tamper-resistant ledger, archived as an outcome-paired learning corpus. Both governance-brief verdicts land here independently: "genuinely differentiated… uncovered white space" (observability angle) and "novel recombination… the metaphor does not appear as an existing named product" (policy angle).

**Second differentiator (scarcest per the AI-SRE brief): per-op-class graduation as a first-class engine** — an action-type earns auto-execution via an ACL rule + demonstrated success — vs. every competitor's single global autonomy dial. NeuBird and Resolve/Rootly *gesture* at it ("graduate to safeguarded automation," "expand as accuracy proven") but none expose it as a per-action-type earned-trust ladder.

**Third differentiator: governance surfaced at the decision boundary, not just at config/RBAC time** — proving not just *what the agent did* but *whether it was allowed to*, which rule fired, at what confidence, touching what. Governance-category vendors flag this exact gap as missing in SRE-remediation products.

**Be honest — what is NOT novel:**
- **Span-tree LLM-I/O tracing** (prompts, completions, tokens, tool-calls, evals, click-through step inspector) is a fully commoditized, well-executed category. TG should **not** rebuild it, and could emit OTel GenAI spans for interop.
- **Per-decision "which rule decided, and why" walks** already exist for network (ASA/Palo Alto) and IAM (AWS/GCP/Sentinel/OPA). The step-through-to-verdict concept is mature; only TG's *domain and fusion* are new.
- **The individual primitives TG fuses** (reasoning, confidence, policy version, matched rule, credential grant, upstream/outcome) are **converging in 2026** across Microsoft ACS, PolicyLayer, Streamkap, and multiple arXiv preprints (Before the Tool Call 2603.20953; Policies on Paths 2603.16586; AARM 2602.09433; DEMM-Bench 2606.20634). TG is ahead on *presentation and unification*, not on inventing the primitives.
- **"Guardrails" and **"RBAC"** are frequently mislabeled as governance but are orthogonal — content-safety filtering and view-permissions, not authorization provenance. TG should not claim novelty over them; it should claim a *different axis*.
- **Retrospective incident timelines / AI RCA** (Rootly, incident.io, FireHydrant, AIOps) narrate what happened — not novel, and not what TG does (live authorization gate).

---

## 4. IP / PATENT-risk flags

The briefs surface **no patent that claims TG's specific combination**, but flag several convergent efforts to watch:

- **Microsoft Agent Control Specification / Agent Governance Toolkit (highest watch)** — shipped ~April 2026; open-source (github.com/microsoft/agent-governance-toolkit). Same primitives (policy version + matched rule + credential grant + reasoning-based authz + evidence). Currently an enforcement/evidence *schema* not a step-through UI, so no direct IP collision on the *inspector/packet-trace presentation* — but a large vendor sitting on the same substance is the primary competitive-and-IP threat; monitor its roadmap for a UI/graduation move. File TG's own claims around the *fused visual step-through + per-op-class graduation ladder* before this converges.
- **PolicyLayer** (policylayer.com) — commercial agent tool-call gateway; enforcement, not tracing. Adjacent, lower risk, but a commercial entity in the exact governance lane.
- **Streamkap decision-trace pattern** — published pattern (not a product/patent); prior-art *disclosure* that could be cited against a TG patent on the retrospective-ledger component. Because it is public prose, it both weakens a broad TG claim and provides cover to narrow TG's claims to the live-gate-walk + graduation novelty.
- **Cisco ASA/Firepower Packet Tracer** — long-standing prior art for the *step-through-to-verdict UX*; means TG cannot patent "phase-by-phase inspector to a verdict" generically — only its application to governed LLM-agent actions with the credential/verify/graduation fuse.
- **Fast-moving regulatory/standards pull** — OWASP Agentic Top-10 (Dec 2025), CSA Levels of Autonomy (Jan 2026), EU AI Act high-risk logging (Aug 2026). Not IP risk, but they make this combination *convergent and likely to be productized by others soon* — a timing pressure, not a blocking claim.

**Net IP posture:** low collision risk today; the combination appears unclaimed as a shipping product or patent. The window is narrowing — Microsoft ACS is the one entity with both the primitives and the reach to close it.

---

## 5. BOTTOM LINE

This is a **novel combination of known parts, sitting in genuine but narrow whitespace** — not a novel primitive, and not (yet) a crowded field. Every ingredient exists and is maturing fast: LLM-observability owns commoditized span-tree execution tracing; IAM/policy tools own non-visual per-decision "which-rule-decided" walks; ASA Packet Tracer owns the step-through-to-verdict UX metaphor; and a 2026 wave of agent-governance efforts (Microsoft ACS/AGT, PolicyLayer, Streamkap's pattern, several arXiv preprints) is independently converging on TG's exact primitive set. What no shipping product does — confirmed independently by all three briefs — is **fuse LLM-agent reasoning + confidence, a policy/ACL decision-walk (rule · floor · mode · per-op-class graduation), credential identity resolution, and a mechanical verify verdict into a single interactive ASA-Packet-Tracer-style step-through for a governed agent action, across queued→live→executed, over a tamper-resistant ledger, archived as an outcome-paired learning corpus.** TG's two scarcest wedges are the **decision-tracer's fused per-decision governance step-through** and **per-op-class earned-trust graduation** — the recommended go-to-market wedge, since that intersection is currently unoccupied. Honest caveat: the defensibility is the *unification and presentation plus the graduation engine*, not the parts; the primitives are convergent, Microsoft's Agent Control Spec is the nearest neighbor on substance and the main competitive/IP threat, and the window to establish priority is months, not years.

---

# Part II — Federation

# Prior-Art & Differentiation Report — Territory Grounder Federation

*(Synthesized from two research briefs: community threat/knowledge-sharing networks, and federated-learning / artifact-sharing frameworks. Every row is grounded in the cited sources.)*

**TG's idea, as the yardstick:** a federation of *sovereign* autonomous-SRE instances that opt-in (default-OFF) to exchange **re-validated remediation distillate** (graduated skills/runbooks/rubrics + alert-class→resolution→verified-outcome patterns) — never raw telemetry, never the estate-specific layer (hosts/IPs/topology/creds/traces). Each consumer **re-graduates** an imported artifact locally before it earns trust; imported wisdom is **subordinate** (still passes the local policy gate), never authority; reputation is weighted by **verified-outcome quality**, not volume; contributions are **signed/attested**; members are **authenticated**.

---

## 1. Competitive landscape

| System | What is shared | Reputation / anti-poison | Authority vs hint | Membership | On-prem / sovereignty |
|---|---|---|---|---|---|
| **CrowdSec** | Distilled *signals* (timestamp, scenario, aggressive IP) → consensus community blocklist. Not remediation. | Strongest of the set: per-reporter **Trust Rank**, content-hash of official scenarios, honeypot cross-check, reporter diversity/profiling, commitment-weighting | **Near-authority** — blocklist feeds "bouncers" that enforce block/captcha directly | Opt-in on deploy; tiered by contribution; TLS-auth to LAPI | Local agent, central API pool |
| **MISP** | Distilled indicators + context (IOCs, events, attributes, sightings). Not remediation. | **Governance, not algorithmic**: TLP tags, sightings, warninglists, confidence tags; trust is human/out-of-band | **Hint** — import → local correlation, no auto-enforcement | **Authenticated sharing groups**, 5 concentric distribution levels, federated instance sync, delegation | Self-hosted instances, federated sync |
| **Sigstore / in-toto / SLSA** | **Signed attestations & provenance** (build metadata, predicates) — envelope+signature, not intel | **Provenance, not reputation**: keyless OIDC signing, Rekor transparency log, SLSA levels. Anti-forgery, not anti-poisoning of *content quality* | **Verify-then-trust policy input** — consumer's local policy gate evaluates the evidence; never auto-authority | Public transparency log; trust roots per verifier | Verification runs locally |
| **AbuseIPDB** | Distilled indicators → 0–100 confidence-of-abuse score | Reputation-weighted: log(distinct reporters), time decay, credibility weight, "no single reporter overpowers", diversity | **Hint** trending to authority when auto-blocked | Public/registered API; anyone reports | Central DB, API consumption |
| **GreyNoise / Spamhaus** | Vendor-curated indicators / DNSBL reputation | **Centralized authority** — proprietary sensors, ML+heuristics+human OSINT | Mostly **authority** (block feeds) | Vendor→subscriber | Central feeds |
| **Emerging Threats / Suricata** | Distilled detection **rules** (signatures) | Maintainer-curated + community FP reports; MD5 bundle integrity; no contributor reputation | **Hint / operator-tuned** | Central maintainer publishes; open download | Rules run locally |
| **Sigma / Elastic detection-rules** | Portable detection **logic** (YAML) | PR review + CI; no runtime reputation/consensus | **Hint** — convert, enable, tune, FP-manage locally (strong "subordinate to local judgment" ethos, for detections) | Open GitHub, public PR governance | Local conversion/execution |
| **OSV.dev / GitHub Advisories** | Curated vulnerability records | **Authoritative curation**, not crowd reputation | **Hint/fact** — consumers decide remediation | Open aggregation, public API | Scanners run locally |
| **Robusta / Keep / Splunk ESCU-SOAR** | **Remediation/response logic as templates** (restart/rollback/scale; SOAR playbooks) — *the only intel-family row sharing remediation* | **None as a network** — static catalogs, git PR review; no outcome validation, no reputation, no re-graduation | **Template you adopt**, then locally configure/gate | Vendor + open catalogs; pull-and-configure | Runs in your cluster |
| **Flower / NVIDIA FLARE / Intel OpenFL / TF-Federated** | **Model weight/gradient updates** → FedAvg global model | **Server-side only** (robust aggregation, DP, node attestation/SGX); no per-consumer trust earning | **Adopt-as-is** — global model pushed back, no consumer gate | Institutional collaborators / clients | **Strong**: raw data never leaves the node; only weights leave |
| **Google Gboard / Apple Private FL** | On-device weight updates under **formal DP** | Central DP accounting; no per-device re-validation | Adopt-as-is (OTA model) | Device fleet | Strong: typing/usage data stays on device |
| **Tesla fleet learning** | **Raw video clips** uploaded → central training → OTA model | Central curation/auto-labeling; shadow-mode is a *central* signal | Adopt-as-is (OTA) | Vehicle fleet | **Weak** — raw sensor data *leaves* the car (opposite of TG) |
| **Windows telemetry / Watson** | One-way telemetry/crashes → central fixes → Windows Update | Microsoft-side prioritization; enterprise ring-testing is manual policy | Downstream ring-test before adopt (org policy, not platform) | Device fleet | Weak — data leaves device |
| **Hugging Face Hub** | **Artifact sharing** (models, datasets, Spaces) | Download counts, likes, leaderboards, model cards + malware scanning (**demonstrably bypassable**) | **Optional, unenforced** self-eval; leaderboards are central | Public/private repos | No estate-split concept |
| **Ansible Galaxy** | **Operational automation** (roles, collections) — closest in *kind* (runbook-like) | Download counts, computed quality scores. Central, not per-consumer | **Best-practice, not enforced** — SRE guidance says stage-test first, platform imposes no gate | Open public catalog | Partial analog: roles parameterized so inventory/vars/creds stay local — code hygiene, not a guarantee |
| **Backstage** | Software templates, golden paths, TechDocs | Ownership/lifecycle metadata + platform-team curation | Consume golden path as-is | **Intra-org only** | No cross-org split |
| **MCP registries / LangSmith Hub / skill marketplaces** | Prompts, skills, MCP servers, SKILL.md — closest in *kind* to TG skills | Nascent/weak: gateway registries, curation, community signals | No enforced re-validation | Public/registry | No inherent estate split |
| **Federated distillation** (research) | **Logits/features** (knowledge, not weights) | Research robustness; no per-consumer ladder | Adopt-as-is (server aggregates logits) | Research setting | Strong: data stays local |
| **Byzantine-robust FL / FLTrust** (defense) | Applies to weight training | **Server-side "don't trust blindly"** (Krum, trimmed-mean, FLTrust root-dataset validation, client reputation) | Central aggregator decision | n/a (defense layer) | n/a |
| **→ Territory Grounder** | **Re-validated remediation distillate** (graduated skills/runbooks/rubrics + alert-class→resolution→verified-outcome) | **Reputation by verified remediation outcome** (did the fix actually resolve, confirmed post-hoc) + signed/attested + local re-graduation | **Subordinate hint** — re-graduated through local policy gate before it can act | **Authenticated, opt-in (default-OFF)** federation of sovereign control-planes | **Strong by design**: estate layer (hosts/IPs/topology/creds/traces) never leaves |

---

## 2. How close the closest analogues get — and where each stops short

**CrowdSec** — architecturally the nearest single system, and TG's own acknowledged precedent. It matches TG on *opt-in, share-distilled-signals-not-raw-telemetry, tiered contribution*, and it has the **most mature reputation/anti-poisoning consensus in the entire landscape** (Trust Rank, hash-verified scenarios, honeypot cross-check, diversity/profiling, commitment-weighting). Two decisive gaps: (1) it shares **detections/indicators — malicious IPs — never remediation**; (2) the redistributed blocklist is **near-authority** enforced directly by bouncers — *the crowd's verdict is the action*, not a hint re-graduated through each consumer's own policy gate. Contributions are authenticated by scenario-hash, not per-item signatures.
→ Sources: crowdsec.net/our-data · docs.crowdsec.net/docs/next/central_api/community_blocklist · docs.crowdsec.net/docs/local_api/tls_auth

**MISP** — nearest on the **membership / trust-fabric** axis: authenticated **sharing groups**, concentric distribution levels, federated instance sync, delegation/pseudonymization. But trust is **human/governance** (TLP, sightings) not algorithmic reputation; it shares **indicators, not remediation**; and it's explicitly a **hint** (import→local correlation) with **no anti-poisoning consensus**.
→ misp-project.org/features · gomisp.com/post/distribution-sharing-groups-in-misp · misp.github.io/misp-website/communities

**Sigstore / in-toto / SLSA** — nearest on TG's **"signed + subordinate-not-authority"** property. A signed attestation carried in a transparency log that **the consumer's local policy verifies before trusting** ("require SLSA L3 from builder X") is *exactly* TG's signed-contribution-passes-local-gate pattern. But it applies to **build provenance, not remediation**, and it proves **who built it, never whether it worked** — no reputation/efficacy dimension.
→ slsa.dev/blog/2023/05/in-toto-and-slsa · secure-pipelines.com/ci-cd-security/artifact-provenance-attestations-slsa-in-toto · safeguard.sh/resources/blog/what-is-sigstore

**Federated-learning frameworks (Flower, FLARE, OpenFL, TFF, Gboard, Apple PFL, federated distillation)** — nearest on TG's **privacy split** (the estate/raw-data stays home; only a generalizable signal leaves — this is FL's core contribution and TG's DNA). But (1) the unit of exchange is **model weights/gradients/logits**, not symbolic operational artifacts; (2) there is **no consumer-side re-validation** — the aggregated global model is **adopted as-is**; quality control lives **server-side** (robust aggregation, DP, FLTrust). **FLTrust / Byzantine-robust aggregation** is the closest *conceptual* cousin to TG's skepticism ("don't trust an incoming update blindly") — but it operates on **gradients**, lives on the **central aggregator**, and is about **detecting poisoning**, not a consumer **earning trust locally**.
→ flower.ai/docs · nvflare.readthedocs.io · intel.com/.../deploy-federated-learning-more-securely-openfl · tensorflow.org/federated · research.google/blog/federated-learning-with-formal-differential-privacy-guarantees · nature.com/articles/s41467-023-44383-9 · emergentmind.com/topics/byzantine-robust-federated-learning

**Ansible Galaxy / Robusta / Keep / Splunk ESCU** — nearest on **kind of payload**: they are the *only* systems that share **remediation/operational automation** rather than indicators. But they share it as **static, un-attested, un-reputation-weighted, un-outcome-validated templates**, distributed pull-and-configure. Re-validation is **best-practice staging advice, never a structural gate**; quality is **central** (download counts, community scores); there is **no verified-outcome reputation and no per-consumer re-graduation**.
→ docs.ansible.com/.../galaxy/user_guide · docs.robusta.dev/master/playbook-reference/what-are-playbooks · github.com/keephq/keep · github.com/splunk/security_content

---

## 3. TG's genuine differentiation — honestly

**Each ingredient is well-precedented in isolation.** Do not overclaim on any single one:

- *Estate-stays-home privacy split* → federated learning's core contribution (Flower/FLARE/OpenFL).
- *Share reusable operational artifacts across orgs* → Ansible Galaxy, MCP/skill hubs, HuggingFace.
- *Signed artifact verified by a local policy gate, subordinate-not-authority* → Sigstore/in-toto/SLSA.
- *Reputation + anti-poisoning consensus on shared intel* → CrowdSec, AbuseIPDB.
- *Authenticated sharing-group federation* → MISP.
- *"Don't trust an incoming update blindly"* → FLTrust/Byzantine-robust FL (server-side), SRE runbook staging, enterprise patch-ring testing.

**The novelty is the synthesis, and it is real.** No surveyed system combines TG's defining properties. The landscape splits cleanly and leaves TG's exact ground unoccupied:

1. **Payload = outcome-re-validated *remediation* distillate.** Every intel-sharing network (CrowdSec, MISP, AbuseIPDB, GreyNoise, ET, Sigma, OSV) stops at **detections/indicators — *what is bad***. The only systems sharing **remediation — *what to do*** (Galaxy, Robusta, Keep, Splunk) share **un-validated static templates**. TG shares the **outcome-verified fix**.

2. **Reputation weighted by *verified remediation outcome*** (did the fix actually resolve the incident, confirmed post-hoc) — a stronger, remediation-specific signal than CrowdSec's report-accuracy Trust Rank or AbuseIPDB's report-volume confidence, and **entirely absent from the playbook world**.

3. **Trust is non-transferable and re-earned locally ("federated graduation").** FL adoption is *trust-by-aggregation* (accept-as-is); marketplace download is *trust-by-central-reputation*; CrowdSec/GreyNoise/Spamhaus trend to *auto-authority*. TG is **trust-by-local-re-graduation**: the artifact's provenance/reputation does **not** confer authority — it must re-run the consumer's own eval + autonomy ladder and pass the **local policy gate** before it can act. This relocates the FLTrust-style "don't trust blindly" check **from the central aggregator to each sovereign consumer**, and hardens it from a defense heuristic into a **mandatory re-graduation gate**.

4. **Federation of sovereign autonomous *control-planes*** (instances that both act and consume), with **signed/attested contributions**. MISP has the authenticated sharing-group fabric but for **human analysts consuming indicators**, not autonomous actuators exchanging graduated fixes.

**Honest one-line framing:** *TG = federated learning's privacy split, applied to symbolic operational artifacts instead of model weights, with an FLTrust-style "don't-trust-blindly" check relocated from the central aggregator to each sovereign consumer and hardened into a mandatory local re-graduation gate, carrying Sigstore-style signed-attestation-verified-by-local-policy, over a MISP-style authenticated federation — but with a payload (outcome-validated remediation) that none of them share.* The defensible novel claim is the **recombination + the payload substitution + the relocation/hardening of the trust check**, not any single ingredient.

The nearest whole-system relatives are **CrowdSec** (proves reputation/anti-poison consensus works at scale) and **federated learning** (proves the privacy split); the unclaimed ground is precisely the shift from **shared detection-as-enforced-authority** to **shared remediation-as-subordinate-attested-locally-re-graduated-hint.**

---

## 4. IP / patent-risk flags

*(Directional risk assessment from the landscape, not a formal freedom-to-operate opinion; a patent-counsel FTO search is advised before any commercialization or public claim of invention.)*

- **Lowest-risk / defensive-publication candidates (the core novelty):** "federated graduation" as **mandatory local re-graduation of an imported operational artifact through the consumer's own eval/autonomy ladder before trust," and **reputation weighted by post-hoc verified remediation outcome.** No surveyed system implements these; they are the strongest candidates both for a defensible novel claim *and* for a defensive publication to preempt others patenting the pattern. Publishing early (blog/spec/whitepaper) establishes prior art and reduces others' ability to block TG.

- **Prior-art-encumbered — assume NOT novel, avoid method claims:**
  - *Reputation/Trust-Rank + anti-poisoning consensus over crowd-reported signals* — CrowdSec (and CrowdSec is a company with its own IP posture); AbuseIPDB. Building a "reputation-weighted community intel pool" method risks reading onto CrowdSec.
  - *Keyless signing + transparency-log attestation verified by local policy* — Sigstore/in-toto/SLSA (Linux Foundation / OpenSSF, broadly open). Reuse the open standards; do not attempt to claim signed-attestation-verified-by-policy as invention.
  - *Privacy-preserving collaborative improvement without centralizing raw data* — a **dense, well-funded patent thicket**: Google (Gboard FL, DP-FTRL), Apple (private FL), NVIDIA (FLARE), Intel (OpenFL/SGX attestation). Framing TG as "federated learning" invites collision with this thicket. **Recommendation: deliberately frame TG as symbolic-artifact exchange, explicitly NOT model-weight/gradient federated learning** — this both is accurate and steers clear of the FL patent estate.
  - *Authenticated sharing groups / concentric distribution levels* — MISP (open-source, CIRCL). Reuse conceptually; low risk but not novel.
  - *Byzantine-robust aggregation / FLTrust root-dataset validation* — heavily published academic prior art (defensive; hard for anyone to patent, hard for TG to claim).

- **CrowdSec-specific flag:** because CrowdSec is TG's acknowledged precedent *and* a commercial entity, keep TG's public positioning crisp on the two differentiators that avoid its lane — **(a) remediation not indicators, (b) subordinate-re-graduated-hint not enforced-blocklist-authority.** These are also the two cleanest talking points for differentiation *and* the two safest from an infringement standpoint.

- **Marketplace-scanning bypass note (not IP, but a design-risk flag worth capturing):** HuggingFace's central malware/pickle scanning is **demonstrably incomplete/bypassable** (ReversingLabs; SafePickle arXiv). This is affirmative evidence for *why* TG's local-re-graduation-before-trust is architecturally safer than central-scan-then-download — useful in both the novelty narrative and the security rationale.

---

## 5. Bottom line

**A known pattern applied to a new domain — but the recombination is substantive and the domain shift is load-bearing, so it clears the bar for "genuinely differentiated," not merely "novel marketing."**

Every *ingredient* has prior art: FL owns the estate-stays-home split; Galaxy/MCP-hubs own cross-org operational-artifact sharing; Sigstore owns signed-artifact-verified-by-local-policy; CrowdSec owns reputation + anti-poisoning consensus; MISP owns authenticated sharing-group federation. **What no one has built is the fusion of all five onto a fundamentally different payload — *outcome-re-validated remediation distillate exchanged between sovereign autonomous control-planes as a locally-re-graduated, subordinate, attested hint rather than an enforced verdict.***

Two elements are the actual inventive core and should anchor the differentiation story (and a defensive publication): **(1) reputation earned by verified remediation outcome**, and **(2) federated graduation — non-transferable trust that each sovereign consumer must re-earn locally before an imported fix can act.** CrowdSec is the nearest neighbor and the honest reference point; the honest positioning is *"CrowdSec for remediation, but the pooled wisdom is a re-graduated hint that stays subordinate to each estate's own policy gate, never an enforced authority."*

Recommendation: **do not frame TG as federated learning** (accuracy + FL patent-thicket avoidance), **do publish the federated-graduation + verified-outcome-reputation mechanisms defensively and early**, and reuse the open Sigstore/in-toto/MISP standards rather than reinventing them.

---

## Implications for TG

- **Decision-tracer = novel recombination in narrow, currently-unoccupied whitespace** — not a new primitive; every ingredient is converging fast in 2026.
- Nearest neighbor on substance is the **Microsoft Agent Control Spec / Agent Governance Toolkit** (same primitives, but an enforcement/evidence schema, not a step-through inspector) — the primary competitive and IP threat to watch.
- The window to establish priority is **months, not years**; file/publish around the fused visual step-through + per-op-class graduation ladder before it converges.
- Tracer defensibility lives in the **unification + presentation + the graduation engine**, not the parts.
- **Federation = a known pattern in a new (load-bearing) domain** — genuinely differentiated because the payload and trust-relocation are substantive, not because any single ingredient is new.
- The two inventive cores to **defensively publish early** are **(1) verified-outcome reputation** and **(2) federated graduation** (non-transferable, locally re-earned trust).
- **Do NOT frame federation as federated learning** — it is accurate to call it symbolic-artifact exchange, and it dodges the dense FL patent thicket (Google/Apple/NVIDIA/Intel).
- **Reuse the open standards** — Sigstore/in-toto for signed attestation, MISP for authenticated sharing groups — rather than reinventing them; CrowdSec is the honest reference point ("CrowdSec for remediation, but a subordinate re-graduated hint, never enforced authority").
