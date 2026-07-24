# Territory Grounder — Helm chart (first-class Kubernetes deploy target)

Governed by [`spec/009-kubernetes-deploy`](../../../spec/009-kubernetes-deploy/). This chart is an
**equal, supported deploy target at parity with the `deploy/docker-compose.yml` single-node profile** —
not a future afterthought. A `helm install` brings up the same multi-service stack from one authoritative
`values.yaml` contract, with credentials supplied by reference and the mutation effect channel held off
by construction in Phase 0/1.

## What it deploys (parity with the compose profile — REQ-901/REQ-902)

| Compose service | Helm workload | Exposure |
|---|---|---|
| `grounder` | Deployment + Service (`grounder-deployment.yaml`, `grounder-service.yaml`) | public API `:8080` (auth-gated); admin `:8443` cluster-internal |
| `postgres` (pgvector) | StatefulSet + Service + role-init ConfigMap (`postgres.yaml`) | ClusterIP |
| `litellm` | Deployment + Service + config ConfigMap (`litellm.yaml`) | ClusterIP |
| `temporal-postgres` / `temporal` / `temporal-ui` | `temporal.yaml` | ClusterIP (frontend + UI cluster-internal) |

The React `frontend/` console is out of scope for this chart (deferred with the rest of the frontend).

## Design guarantees (mapped to spec/009 requirements)

- **Same distroless binary (REQ-903).** The grounder Deployment runs the identical static image built by
  `deploy/Dockerfile`, pinnable by immutable digest (`grounder.image.digest`), as non-root with a
  read-only root filesystem and no privilege escalation.
- **Secrets by reference, never literals (REQ-904 / INV-13).** `values.yaml` carries only Secret *names*
  and credential *keys*. No DSN, password, master key, or provider API key is ever a literal in the
  chart. `secrets.mode=reference` expects an operator-supplied Secret; `secrets.mode=external` renders an
  `ExternalSecret` (External Secrets Operator).
- **Two-role Postgres (REQ-905 / INV-16).** `tg_runtime` (DML-only) backs the runtime DSN; `tg_migration`
  (DDL) is confined to the startup migration. The role-init script mirrors the compose `00-roles.sh`.
- **Actuation off in P0/1 (REQ-906 / INV-09).** Actuation is OFF by construction: the retired
  `TG_MUTATION_ENABLED` env knob has been absorbed into the mode chokepoint (spec/015 REQ-1520). The worker
  boots read-only (mode Shadow, the durable default); enabling actuation is an operator-authorized, audited
  mode transition, never a deploy-time flag. The `mutation.enabled` value is retained only as a legacy no-op.
- **One values contract (REQ-907 / INV-15).** Every template reads only from `.Values`; there is no
  second hand-maintained config surface.
- **Auth-gated exposure (REQ-909 / INV-01).** Only the authenticated public API is published; the admin
  listener, Temporal frontend, and Postgres ports are never on a default Ingress.
- **Health + ordering (REQ-910).** Liveness/readiness probes hit `/healthz` and `/readyz`; an init
  container blocks on `pg_isready` so the control-plane starts only after Postgres is healthy (the
  compose `service_healthy` equivalent).

## Usage

Provide the credential Secret out-of-band (reference mode), then install:

```sh
# 1. Create the credential Secret (keys per values.secrets.keys). Never commit these.
kubectl create secret generic grounder-secrets \
  --from-literal=TG_RUNTIME_DSN='postgres://tg_runtime:...@grounder-postgres:5432/grounder' \
  --from-literal=TG_MIGRATION_DSN='postgres://tg_migration:...@grounder-postgres:5432/grounder' \
  --from-literal=LITELLM_MASTER_KEY='...' \
  --from-literal=PG_SUPERUSER_PASSWORD='...' \
  --from-literal=TG_RUNTIME_PASSWORD='...' \
  --from-literal=TG_MIGRATION_PASSWORD='...' \
  --from-literal=TEMPORAL_PG_PASSWORD='...' \
  --from-literal=ZAI_API_KEY='...' # + the other provider keys

# 2. Install the chart (mutation stays OFF by default — Phase 0/1 read-only foundation).
helm install grounder ./deploy/helm/grounder

# ExternalSecrets alternative (no operator-supplied Secret):
helm install grounder ./deploy/helm/grounder \
  --set secrets.mode=external \
  --set secrets.externalSecretStore.name=my-cluster-store
```

Pin the image by digest for a real deploy:

```sh
helm install grounder ./deploy/helm/grounder \
  --set grounder.image.digest=sha256:<digest>
```

## CI gate (REQ-908)

`deploy/helm/ci/helm-lint.sh` runs `helm lint` + `helm template` and scans the rendered output for
literal-credential patterns, failing on a lint error or any leaked secret. Wire it into the pipeline for
every change that touches the chart.

## Configuration

`values.yaml` is the single authoritative contract; every knob is documented inline there. The
load-bearing defaults: `mutation.enabled=false`, `secrets.mode=reference`, `grounder.ingress.enabled=false`.
