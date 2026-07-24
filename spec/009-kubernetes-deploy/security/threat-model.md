<!-- spec/009 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/009 — Threat model: Kubernetes / Helm deploy target (STRIDE slice)

Per-feature threat slice for the Helm deploy surface. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the deploy artifact's own trust
boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The chart is a build/deploy-time artifact rendered by `helm template`/`helm install`
into a cluster. Its inputs are `values.yaml` (non-secret settings + secret *names*) and referenced
Kubernetes Secrets / ExternalSecrets. Its outputs are Kubernetes manifests that stand up the same
service set as the compose profile. The adversary of interest is anyone who can read the repository or a
rendered manifest and hopes to recover a credential, or who can nudge the default render into exposing an
internal listener or enabling the mutation effect channel.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | An unauthenticated caller reaches the public API as if authorized | The public Service fronts only the auth-gated control-plane port; the auth middleware is non-bypassable and an auth=none route fails to register | REQ-909, INV-01 |
| **Tampering** | A chart change silently flips the mutation flag or drifts the service set from compose | The deploy-parity invariant + CI `helm template` render fail on a service-set divergence or a default render that enables mutation | REQ-902, REQ-906, INV-09 |
| **Repudiation** | A rendered manifest cannot be traced back to its authoritative input | Exactly one `values.yaml` contract feeds every template; no second hand-maintained config surface exists to diverge unaudited | REQ-907, INV-15 |
| **Information disclosure** | A credential literal is committed to git or emitted into a rendered manifest | Credentials are sourced only by `secretKeyRef`/ExternalSecret reference; `values.yaml` holds names, never values; CI greps the render for literal-credential patterns and fails on a hit | REQ-904, REQ-908, INV-13 |
| **Denial of service** | An internal port (Temporal frontend, admin listener, Postgres) is exposed and abused | Those ports are ClusterIP-only and absent from the default Ingress; only the public API is routable | REQ-909, INV-01 |
| **Elevation of privilege** | The runtime workload gains DDL / cross-schema power on Postgres | The runtime DSN binds the DML-only `tg_runtime` role; DDL is confined to the `tg_migration` role used only by the startup migration Job | REQ-905, INV-16 |
| **Elevation of privilege** | A compromised container escalates on the node | The grounder pod runs the distroless image as non-root with a read-only root filesystem and privilege escalation disabled | REQ-903, INV-13 |

**Adversarial acceptance (boundary checks, CI).** Render the chart with default values and assert: the
service set equals the compose set; no rendered manifest contains a literal credential; the mutation flag
is off; the admin/Temporal/Postgres ports carry no default Ingress; and the grounder pod's security
context is non-root + read-only-rootfs + no-privilege-escalation. These drive the actual rendered output
(INV-22), not chart prose.
