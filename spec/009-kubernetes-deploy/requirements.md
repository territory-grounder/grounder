<!-- spec/009 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/009 — Kubernetes / Helm as a first-class deploy target

**Owning behavior family:** the deploy surface (see [`docs/ARCHITECTURE.md`](../../docs/ARCHITECTURE.md) §5).
**Constitution / invariants:** INV-01, INV-09, INV-13, INV-15, INV-16.
**Phase:** P0 read-only foundation (the chart ships at parity with the compose profile). **Status:** Approved.

Territory Grounder ships one guided `docker-compose` single-node profile today. This spec makes a Helm
chart an equal, supported deploy target — parity with the compose profile, not a future afterthought.
A `helm install` brings up the same multi-service stack (grounder control-plane, application Postgres
with pgvector, Temporal and its backing Postgres, the Temporal UI, and the LiteLLM model-gateway) from
one authoritative values contract, with credentials supplied by reference and actuation withheld by
construction (the runtime mode chokepoint; there is no deploy-time actuation switch — TG-112).

This spec is **single-org**: authorization is RBAC over one organization, and no `tenant_id`
partitioning appears in the deploy contract. This document is the requirement source of record; the
design is in `design.md`, the runnable acceptance oracles are in `acceptance/`, and the engineering
tasks are in `tasks.json`.

## Requirements

- **REQ-901** — [R] deploy-target parity · [F] docker-compose profile.
  WHEN an operator runs `helm install` for the grounder chart, the system SHALL bring the full service
  set — the grounder control-plane, the application Postgres with the pgvector extension, Temporal with
  its backing Postgres, the Temporal UI, and the LiteLLM model-gateway — to a Ready state that mirrors
  the topology of the `deploy/docker-compose.yml` single-node profile.

- **REQ-902** — [R] deploy-target parity.
  The Helm chart SHALL be a first-class deploy target held at parity with the docker-compose single-node
  profile: the two targets SHALL render the identical service set from one shared configuration
  contract, and a service present in one target SHALL be present in the other.

- **REQ-903** — [R] stack · [O] INV-13.
  The grounder Deployment SHALL run the identical distroless static `grounder` image produced by
  `deploy/Dockerfile`, pinned by immutable image digest, executed as a non-root user with a read-only
  root filesystem and privilege escalation disabled.

- **REQ-904** — [O] INV-13.
  IF a chart value carries a credential — a Postgres DSN or password, the LiteLLM master key, or a
  model-provider API key — THEN the system SHALL source that value from a referenced Kubernetes Secret
  or an ExternalSecrets object, and SHALL NOT render the literal credential into any templated manifest,
  ConfigMap, or the committed `values.yaml`.

- **REQ-905** — [O] INV-16 · [F] two-role model.
  WHEN the grounder workload connects to the application Postgres, the system SHALL use the DML-only
  `tg_runtime` role for the runtime DSN and SHALL restrict the DDL `tg_migration` role to the startup
  migration Job, preserving the two-role least-privilege split of the compose profile.

- **REQ-906** — [O] INV-09 · [F] read-only foundation.
  The chart SHALL expose NO deploy-time actuation switch: the values contract SHALL NOT carry a
  mutation-enable flag (the former `mutation.enabled` no-op is REMOVED — TG-112) and SHALL name the
  runtime mode chokepoint as the only actuation authority, so a rendered workload can mutate a managed
  estate only after an owner-audited runtime mode transition — never by a chart value.

- **REQ-907** — [O] INV-15.
  The chart SHALL expose exactly one authoritative `values.yaml` configuration contract from which every
  rendered manifest draws its settings, and SHALL NOT carry a second hand-maintained configuration
  surface that can diverge from it.

- **REQ-908** — [O] INV-15/INV-13.
  WHEN a change touches the chart, the CI pipeline SHALL run `helm lint` and a `helm template` render,
  and SHALL fail the pipeline IF the chart does not lint or IF a rendered manifest contains a literal
  credential value.

- **REQ-909** — [O] INV-01.
  The chart SHALL expose the grounder public API through a Kubernetes Service gated by the control-plane
  authentication middleware, and SHALL NOT place the elevated admin listener, the Temporal frontend, or
  the Postgres port on a default-routable Ingress.

- **REQ-910** — [F] compose health ordering.
  WHEN the grounder Deployment starts, the system SHALL gate readiness on liveness and readiness probes
  against the control-plane health endpoint, and SHALL order dependent workload startup so the
  control-plane becomes Ready only after the application Postgres reports healthy, matching the compose
  `service_healthy` ordering.

## Configuration contract

The single `values.yaml` (REQ-907) is the authoritative input. It carries per-service image references
pinned by digest, replica counts, resource requests/limits, NO deploy-time actuation switch (the mode
chokepoint note, REQ-906/TG-112), Service/Ingress exposure toggles (REQ-909), and — for every credential — only a
`secretRef`/`externalSecret` name, never a literal (REQ-904). Rendered manifests draw exclusively from
this contract; there is no parallel config file.

## Deploy-parity invariant

A standing check SHALL FAIL if the Helm-rendered service set diverges from the compose service set, if
any rendered manifest embeds a literal credential, or if the values contract reintroduces a deploy-time
actuation switch (the retired `mutation.enabled` class — TG-112).
