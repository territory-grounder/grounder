## Goal
Vet a Kubernetes alert against the cluster's known recurring patterns and its live state before
treating it as a novel fault. NOTE — tool-gated content: TG has no Kubernetes read tools wired; this
runbook is knowledge-library material until a vendor-official, read-only cluster surface lands (skills
on unwired tools are not authored, per ADR-0012).

## Required evidence
- The alert's name, namespace, severity, and its rule's hold duration (`for:`) — a rule tuned with a
  long hold already encodes "this flaps benignly"; respect what the tuning says.
- The cluster's own answer for the named object: current status, restart count, recent events, and the
  relevant container log tail — read from the control plane, not inferred from the monitor.
- The estate's known-recurring-issue notes for this alert class on this cluster, and
  `get-incident-history <host>` for the backing nodes.
- For storage-adjacent symptoms: the health and latency of the backing storage layer.

## Decision rules
- Match against the known-pattern list FIRST: clusters accumulate documented false positives (a
  capacity alert that misreads network-backed volumes, an agent flapping during tunnel re-connects). A
  match is recorded WITH its verification step — a known pattern is a hypothesis to confirm cheaply,
  never a license to dismiss unverified.
- Verify liveness against the cluster, not the metrics pipeline: scrape intervals and pressure make the
  monitor stale; a pod the monitor calls dead may be running. The control plane's answer wins.
- When control-plane components flap in unison — API timeouts, datastore leader churn, restarts —
  suspect the dependency layer below them: storage latency surfaces as control-plane instability. Check
  the backing store's latency before debugging the components themselves.
- A workload's fix shape follows the two-lane split in the live k8s-triage protocol (runtime fault vs
  declarative drift); this vetting runbook decides only whether the alert is real and which layer owns
  it.
- Never propose a node drain from triage: drains are gated on placement and replication facts (a
  single-copy storage system turns a routine drain into data loss). Surface the drain as an
  escalation carrying those facts.
- Declared cluster-change discipline (GitOps-only mutation paths) is the estate's law, not the triage
  seat's option — a "quick fix" mutation proposal that bypasses the declared path is malformed.

## Verification
- The record shows the known-pattern verdict (matched-and-confirmed, matched-but-refuted, or novel)
  with the confirming observation.
- Any liveness claim cites the cluster read, not the monitor value alone.
- If the dependency layer was implicated, the record contains the storage-side observation that
  implicates it — not just the correlation.
