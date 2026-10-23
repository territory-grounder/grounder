# ADR 0010 — Single-organization, multi-user (NOT multi-tenant SaaS)

## Status
Accepted. Supersedes the "multi-tenant by default" framing (the earlier paradigm-rule 1) wherever it
appears in the doc set and specs.

## Context
An earlier reframe over-corrected. When the product was de-scoped away from a single solo operator, that
was translated into **multi-tenant SaaS** — `tenant_id` + PostgreSQL row-level security isolating
mutually-distrusting organizations that share one deployment. That is the wrong model. Territory
Grounder is an **open-source, self-hosted tool an organization runs for its own infrastructure**. It is
**not a SaaS product**; there are no external paying tenants to isolate.

The correct generalization of "not solo" is **multi-user**, not multi-tenant: one organization, one
deployment, **many operators** — with roles, teams, an on-call/approver graph, and least-privilege
credentials — rather than one absent human. Cross-organization data isolation (the only thing
`tenant_id` + RLS buys) is scaffolding this product does not need and should not carry.

## Decision
Territory Grounder is **single-organization and multi-user** by construction.

- **No `tenant_id`, no cross-org RLS isolation.** One deployment serves one organization. The
  `(tenant_id, external_ref)` correlation key collapses to **`external_ref`** (ids are unique within the
  org's own trackers).
- **Keep the multi-user governance layer** — this half of the earlier reframe was correct: RBAC roles,
  teams, on-call rotation / escalation policy, quorum, and a **fallback approver graph** replace "the
  operator." Authority is checked against the acting **user/role**, not a tenant.
- **Keep least-privilege identity** — per-**source** HMAC secrets and per-**agent** scoped
  credentials/mTLS with credential-revoke-as-kill. That is defense-in-depth, not tenancy, and stays.
- **Site / environment is data, not a security boundary.** The predecessor's NL/GR sites are a
  descriptive `site`/`estate` label on hosts and incidents (for filtering and routing), never an
  isolation boundary — every operator can see the whole estate subject to RBAC.
- **Governance, retention, and the audit ledger stay** exactly as specified; they are simply
  org-global, not tenant-scoped.

The `[R]` provenance layer keeps its meaning ("the product reframe away from a solo homelab tool"), but
its content is corrected from "multi-tenant + RLS isolation" to "multi-user + RBAC/roles + single-org."

## Consequences
- The 7 governed-behavior specs, `DATA-MODEL`, `CONSTITUTION`, `PRODUCT`, `ARCHITECTURE`, `THREAT-MODEL`,
  `CAPABILITY-INVENTORY`, and `00-README` drop tenant-isolation language and keep RBAC/roles/estate.
- The Phase-0 Go code is refactored: `TenantID` is removed from `auth.Principal` / `auth.Source` /
  `manifest.Action`; the DB drops `tenant_id` columns + RLS (a single-org schema); `Principal` carries
  the authenticated **user/source + role**, not a tenant.
- A future SaaS/managed-service offering (multiple isolated orgs on one install) would be a **separate,
  explicit ADR** re-introducing a tenant boundary — it is not assumed here, and the product is simpler
  and safer without it.

## Alternatives
- **Multi-tenant SaaS (the superseded model)** — rejected: this is a self-hosted OSS tool for an org's
  own infra; org-isolation is unused complexity and a larger attack surface.
- **Multi-estate isolation (RLS by site)** — rejected as the default: within one org, operators should
  see the whole estate under RBAC; site is a filter, not a wall. (Re-introducible per-deployment later
  if a real need appears, as a scoping — not isolation — key.)
