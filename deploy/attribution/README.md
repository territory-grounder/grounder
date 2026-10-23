# Actor-attribution ruleset — deploy-time override

The actor-attribution plane (spec/023 — *WHO is the actor behind the observed change?*) runs on loadable
rules-as-data. The worker ships a **generic** embedded default (`core/attribution/default_config.json`)
that carries **only** the portable taxonomy→disposition mapping. It names **no** sanctioned principals and
**no** carve-outs, because those are site-specific and must never be baked into the shipped binary.

## What lives where

| Concern | Where | Portable? |
|---|---|---|
| Taxonomy → disposition mapping | embedded `default_config.json` | yes — install-neutral |
| Sanctioned principals (a realm's admins) | **this override** | no — per site |
| Test-pool carve-outs (a lab's guinea-pig hosts + validity window) | **this override** | no — per site |

## How the worker loads it

At boot the worker reads `TG_ATTRIBUTION_CONFIG`:

- **set + readable** → that file is parsed as the **complete** ruleset and **replaces** the embedded
  default. Copy `config.example.json`, fill in your realm + pool, mount it into the container, and point
  `TG_ATTRIBUTION_CONFIG` at the in-container path.
- **unset / unreadable** → the generic embedded default is used. With no sanctioned principals declared,
  every non-self actor reads `attributed-suspicious` (→ POLL_PAUSE + security escalation) until you
  declare your admins. This is the safe, evidence-gated direction — TG fails toward human review, never
  toward silent auto-heal.
- A parse failure of **either** fails **closed** to the empty mapping (every non-unattributable
  attribution escalates to the approver graph).

## Authoring the override

Start from `config.example.json`. Replace every `REPLACE-…` placeholder:

- `sanctioned_principals[].actors` — the principals your admins act as, exactly as the domain records
  them (e.g. the PVE task log's `user` field: `root@<realm>`).
- `carve_outs[].actors` — the injector principal your test harness acts as.
- `carve_outs[].hosts` — your test-pool guest **names** (not IPs).
- `carve_outs[].valid_from` / `valid_until` — bound the carve-out; an expired window reverts pool faults
  to stand-down (the safe direction), so keep it current.

The mapping block is portable — copy it verbatim (the loader replaces the whole document, so the override
must include it).

**Never commit a filled-in override with real hostnames/realms to the repo** — keep the real file on the
box (e.g. under the deploy stack, mounted read-only), the same discipline as the knowledge corpus's
`seed ∪ maintained` split. This example carries placeholders only.
