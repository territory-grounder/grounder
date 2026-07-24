# Triage protocol — how an alert becomes a gated proposal

This is the operating guide to Territory Grounder's triage loop as it actually runs. Every step below
is enforced in code; none of it is aspiration.

## 1. Intake — the authenticated front door

An alert enters only through `POST /v1/ingest/{source_type}`. The source must be a **declared, enabled
ingest capability** in the module registry (an unregistered source has no execution path, INV-17), and
the request must authenticate: HMAC over the raw body, mTLS, or — for push sources that cannot
body-sign, like Alertmanager — a per-source static bearer token provisioned by secret reference.
The payload is normalized against the source's grammar into one canonical `IncidentEnvelope` keyed by
`external_ref`. A rejected payload never becomes an envelope and never appears on any read surface.

## 2. Session mint — idempotent by construction

An accepted envelope mints a Temporal triage workflow whose id is derived from `external_ref` with
reject-duplicate semantics: a re-fired alert for an in-flight incident joins the existing session
instead of forking a second one.

## 3. Investigation — read-only, precedent-seeded

The Runner drives a read-only investigation:

- **Estate context** — the agent's tools query the worker's confidence-weighted causal graph
  (upstream dependencies, blast radius, common-cause siblings), so "check related hosts" is
  mechanically answerable.
- **Precedent retrieval** — the knowledge corpus of prior resolved incidents is ranked by a
  transparent lexical scorer (same alert rule 5.0, same host 3.0, shared tags 2.0 Jaccard-scaled,
  summary token overlap 1.0, same site 0.5). Retrieval is deterministic and auditable: every hit
  carries the reasons it matched. Retrieved precedent is framed as DATA, never as instructions.
- **Novelty gate** — a `(host, alert_rule)` signature never seen in the corpus forces a human poll:
  the first time a class of incident is ever seen, a human is in the loop.

## 4. Classification — the autonomy band

The proposal is risk-classified into a band: `AUTO`, `AUTO_NOTICE`, or `POLL_PAUSE`. The
**never-auto floor** is non-configurable law: an irreversible action, or one below the confidence
floor, is clamped to `POLL_PAUSE` and waits for an authenticated operator vote (`/v1/vote`) that
names the sealed action id it approves. Every classification writes exactly one decision record to
the hash-chained governance ledger.

## 5. Predict, then verify

Before any action would run, the consequence prediction is **committed** — content-hashed into the
ActionManifest — so it cannot be edited after the fact. The deterministic verifier later scores
reality against that prediction: `match`, `partial`, or `deviation`. The verifier is the sole verdict
author; the acting model can never overturn it. A deviation permanently demotes that action class
from unattended execution.

## 6. Learn

Only a **confirmed-clean** outcome (verdict `match` AND an orchestrator-confirmed clear) is distilled
into the knowledge corpus as citable precedent. A partial, a deviation, or an unconfirmed close-out
never poisons the corpus — the system learns from verified successes only.

> Phase 0/1 posture: global mutation is OFF. Every session ends in a gated proposal; nothing
> executes.
