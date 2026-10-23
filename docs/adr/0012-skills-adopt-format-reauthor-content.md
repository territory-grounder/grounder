# ADR-0012 — Skills & prompts: adopt the format standards, re-author the content

**Status:** Accepted (2026-07-17).
**Layers:** [F] predecessor skills/runbooks · [R] product distribution · [O] INV-08/INV-11, threat T-supply-chain.

## Context

TG's agent competence (behavioral skills + per-alert-class playbooks, `agent/skills`) had to come from
somewhere: hand-port the predecessor's prompts verbatim, adopt a community/industry library, or re-author.
Three independent 2026 analyses of the agent-skills landscape (two commissioned external reports + an
in-session multi-agent research sweep) converged on the same facts:

1. **The packaging standardized; the content did not.** Agent Skills / `SKILL.md` (Linux Foundation / Agentic
   AI Foundation, adopted by 30+ tools incl. HolmesGPT and kagent — HolmesGPT is TG's architectural twin:
   alert-matched markdown injected into a ReAct loop) and MCP (tools) are real, durable standards. There is
   **no** curated, vetted library of infra triage skills — no Terraform-Registry equivalent.
2. **The only reusable runbook content is Kubernetes/Prometheus-scoped** (prometheus-operator/runbooks,
   kubernetes-mixin — Apache-2.0). TG's estate is network/LibreNMS/SNMP + Proxmox/Ceph, where upstream skill
   content is effectively **zero**. TG's differentiating competence must be authored in-house.
3. **Community skill marketplaces are a live supply chain hazard**: ~1,184 malicious skills found on ClawHub,
   13.4% critical rate in Snyk's ToxicSkills audit, 30+ MCP CVEs (incl. CVE-2025-6514, CVSS 9.6). This
   *validates* TG's invariants (no model token becomes control flow; fail-closed interceptor; never-auto
   floor) rather than arguing against tools.
4. **Runbooks beat model-chasing**: the CNCF/HolmesGPT field data shows the same model scoring 4.6/5 with
   good runbooks vs 3.6 without (tool calls 16 → 2-3). Skills are the highest-leverage eval investment.

## Decision

- **Adopt the format layer**: structure every TG skill as goal → required evidence (naming the exact TG
  tool) → decision rules → verification (a named post-fix observation). Keep skills terse and hand-authored;
  align with `SKILL.md` packaging when the skill store lands (the store's rows map 1:1 onto SKILL.md
  frontmatter + body, so conformance is an export format, not a rewrite).
- **Re-author the content layer in-house**, grounded ONLY in tools the agent actually has (LibreNMS
  status/eventlog/alerts, `get-estate-context`); distill external material (Apache-2.0/CC-BY with
  attribution; vendor docs rewritten, never copied) instead of importing it.
- **Domain skills are gated on their tools**: no k8s/Cisco/Proxmox skill until the corresponding
  **vendor-official, read-only** MCP tool is wired behind TG's interceptor (pinned tool-description hashes;
  `--read-only` / `--disable-write` server flags). High-risk surfaces (Cisco ASA/FTD, Proxmox, raw SSH) are
  re-author-only and floored at never-auto.
- **Third-party skills enter as procedural knowledge only** — any bundled script is stripped; an executable
  action exists only as a proposed, manifest-bound action through the governed gate (kagent's auto-registered
  `bash` tool is explicitly rejected).

## Consequences

- `agent/skills` carries original TG-authored bodies (enforcement + competence tiers), evaluated by the
  on-box harness; the eval scorecard is the acceptance bar for every skill change.
- The planned skill store (versioned, console-editable, flywheel-graduated — see the skill-store epic) is
  the delivery mechanism for this decision at runtime; INV-08 holds throughout: a pure typed-signal selector
  chooses what loads, never a model token.
- TG can consume MCP servers and publish/export SKILL.md without re-architecting.
- We accept the cost of authoring network-estate content ourselves — it is the moat, and no upstream source
  exists to shortcut it.

## Alternatives rejected

- **Verbatim port of predecessor prompts** — carries its homelab constants and `claude -p` idioms into a
  product; estate specifics belong in the estate graph/config, not prose.
- **Adopt a community skill pack** — nothing vetted exists for the domain; supply-chain risk is documented
  and material.
- **Model-generated skill files** — LLM-authored instruction files reduced task success in the majority of
  published comparisons; the flywheel instead generates *candidates* that must win an eval A/B before
  graduating.
