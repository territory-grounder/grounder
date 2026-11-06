# skills/ — distilled prose-artifact drafts (TG-477, TG-478, TG-479, TG-85, TG-78)

> Reference content, not a work queue — and GENERATED, not hand-edited. This tree is the
> byte-exact output of `go run ./tools/prosedistill` from the committed manifest and the
> re-authored bodies in `tools/prosedistill/distilled/`. Edit THOSE and re-run the tool;
> `go run ./tools/prosedistill --verify` (and the package's tests) red on any drift or stray.

Batches 1-3: the predecessor's prose corpus — skill library, specialist agents, runbooks —
re-authored per ADR-0012 (format adopted, content re-authored, estate specifics stripped) into
draft prose artifacts for the ADR-0017 store classes. Batches 4 (TG-85) and 5 (TG-78) depart
from that: their entries are newly authored, grounded directly in vendor documentation rather
than distilled from predecessor prose — see each entry's own manifest notes and the artifact's
Doc basis section. The
manifest — `tools/prosedistill/manifest.json` — is the honest inventory: every source, including
every one NOT distilled, with its disposition and reason.

THIS TREE IS INERT. Nothing at runtime reads it: the agent's live competence is the compiled
seed library (`agent/skills/`), and skill-store rows are written only through the store's
governed write path. Seeding these drafts as store rows is a separate, later wire — after the
content itself passes owner review. Prose becomes behavior only through that store seeding/
graduation rail, and the eval gate binds THERE (per-artifact admission + trial) — which is why
`skills/` was removed from the merge-time eval `behavior_re` on 2026-08-14 (owner ruling): a red
pipeline on inert files is not a control. The seeding wire, when built, carries the binding evals.

- `runbook/` (36): causal-graph-stewardship, cisco-acl-nat-triage, cisco-asa-failover-context-triage, cisco-asa-vpn-ipsec-triage, cisco-bgp-ospf-adjacency-triage, cisco-high-cpu-memory-triage, cisco-interface-line-protocol-triage, config-backup-tiers, cost-review, credential-rotation-safety, diverged-replica-triage, external-visibility-triage, fault-injection-exercise, firewall-triage-patterns, incident-chain-trace, incident-lifecycle, k8s-alert-vetting, k8s-control-plane-degradation-triage, k8s-node-notready-triage, k8s-pod-crashloop-oom-triage, k8s-pod-pending-scheduling-triage, k8s-pvc-storageclass-triage, k8s-service-ingress-dns-triage, knowledge-base-hygiene, live-workflow-edit-safety, platform-self-healing, proxmox-cluster-quorum-triage, proxmox-guest-lifecycle-triage, proxmox-node-storage-triage, scheduled-event-suppression, security-deep-verification, security-finding-triage, site-isolation-triage, storage-alert-triage, synthetic-probe-isolation, wan-uplink-degradation-triage
- `skill/` (12): alert-burst-correlation, alert-deduplication, alert-queue-review, amber-zone-discovery, drift-check, escalation-boundary, failure-handoff, identity-first-lookup, independent-review, precedent-recall, recovery-revalidation, research-brief
