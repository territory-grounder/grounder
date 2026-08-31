<!-- spec/009 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/009 — Design: Kubernetes / Helm first-class deploy target

How the requirements in `requirements.md` are realized as a Helm chart at `deploy/helm/grounder/`. Where
this design and the chart disagree, the chart is the bug and this document is the intent.

## Realization on the Go / Temporal / Postgres / React stack

The chart is a 1:1 Kubernetes port of the `deploy/docker-compose.yml` single-node profile. Each compose
service becomes a Kubernetes workload; the compose env block becomes a projected env sourced from a
Secret; the compose `depends_on` / `service_healthy` ordering becomes probe-gated readiness. The
control-plane binary is unchanged — the same distroless static `grounder` image that `deploy/Dockerfile`
builds runs in the pod (REQ-903). No Go, Temporal, Postgres, or React code changes for this spec; the
deploy target is additive.

### Service mapping (REQ-901, REQ-902)

| Compose service | Helm workload | Notes |
|---|---|---|
| `grounder` | `grounder-deployment.yaml` + `grounder-service.yaml` | The Go control-plane. Public API on `:8080`; the elevated admin listener on `:8443` stays cluster-internal (REQ-909). It dials Temporal as a *client* (via `TG_TEMPORAL_HOSTPORT`) to mint the ingest→triage session. |
| `worker` | `worker-deployment.yaml` | The Temporal Runner poller/executor (same image family, `BIN=worker`). It is the ONLY consumer of the `tg.runner` task queue the grounder enqueues onto — without it the platform ingests alerts and reports healthy while triaging nothing, so it is load-bearing for REQ-902 parity, not optional. |
| `postgres` (pgvector/pg16) | `postgres.yaml` (StatefulSet + Service + init) | Application state. The two least-privilege roles (`tg_migration` DDL, `tg_runtime` DML) are created by the same init script the compose profile mounts (REQ-905). |
| `litellm` | `litellm.yaml` (Deployment + Service + ConfigMap) | The OpenAI-compatible model-gateway. `litellm-config.yaml` becomes a ConfigMap; provider keys arrive as Secret env refs (REQ-904). |
| `temporal-postgres` + `temporal` + `temporal-ui` | `temporal.yaml` | Durable orchestration substrate and its backing Postgres; the Temporal frontend (`:7233`) and UI stay cluster-internal (REQ-909). |

The React `frontend/` console is out of scope for this spec (deferred with the rest of the frontend);
the chart exposes only the generated-OpenAPI control-plane API it will later consume.

### One values contract (REQ-907)

`values.yaml` is the single authoritative input. `_helpers.tpl` derives every name/label/selector from
it, and every template reads only from `.Values`. There is no second hand-maintained config surface: the
compose profile's `.env` reference discipline maps to `values.yaml` for non-secret settings and to
referenced Secrets for credentials. This is the INV-15 "one authoritative definition, everything else
generated" discipline applied to the deploy layer.

### Secrets by reference, never literals (REQ-904)

`templates/secrets.yaml` renders either (a) a placeholder `Secret` whose data keys are populated
out-of-band by the operator, or (b) an `ExternalSecret` (External Secrets Operator) that resolves keys
from an upstream store, selected by `secrets.mode`. The chart itself never carries a credential literal;
`values.yaml` holds only secret *names*. Rendered pod env uses `valueFrom.secretKeyRef` for
`TG_RUNTIME_DSN`, `TG_MIGRATION_DSN`, `LITELLM_MASTER_KEY`, and each provider key. The CI gate (REQ-908)
greps the `helm template` output for known credential patterns and fails on any hit — the mechanical
enforcement of INV-13 at the deploy boundary.

### Mutation off by construction in P0/1 (REQ-906)

`values.yaml` defaults `mutation.enabled: false`. The rendered grounder pod carries the read-only
foundation flag, matching the compose comment "mutation is disabled by construction." The deploy-parity
invariant check fails if a default render flips the effect channel on. Turning mutation on is a Phase-2+
opt-in that later specs govern; nothing in this chart lifts the fail-closed baseline.

### Health, ordering, and exposure (REQ-909, REQ-910)

The grounder Deployment declares a liveness probe and a readiness probe against the control-plane health
endpoint. Because Kubernetes has no compose-style `depends_on: service_healthy`, ordering is enforced by
the readiness contract plus an init container (or the migration Job) that blocks on `pg_isready` before
the control-plane accepts traffic — the same effect as the compose health gate. The public Service
fronts `:8080` behind the auth middleware; the admin listener, Temporal frontend, and Postgres port are
`ClusterIP`-only and absent from the default Ingress.

## CI lint gate (REQ-908)

A CI job runs `helm lint deploy/helm/grounder` and `helm template` with the default values, then scans
the rendered output for literal-credential patterns. The job fails the pipeline on a lint error or any
literal-secret hit, so a chart change cannot merge with a drifted or secret-leaking manifest.

## Out of scope

The `frontend/` console packaging, per-org autoscaling policy, and multi-node HA topologies are out of
scope. Runtime authorization semantics are owned by spec/006 (interface contracts); this spec only wires
the deploy surface that exposes them.
