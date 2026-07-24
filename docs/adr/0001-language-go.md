# ADR 0001 — Control-plane language: Go

## Status
Accepted.

## Context
Territory Grounder (TG) is a greenfield [R] reimplementation of a governed-autonomy SRE platform whose predecessor was an n8n/bash/Python assembly [F]. The core structural property TG must make *structural rather than conventional* is the deterministic-orchestrator / untrusted-model split [F][O S8-1]: the control-plane is the only authority, and model output is typed data. That control-plane must own control flow, actuation as fixed argv vectors (INV-02) [O], parameterized persistence (INV-03) [O], one typed `ActionManifest` threaded and re-asserted at every stage (INV-07) [O], and it must integrate natively with the chosen orchestration substrate (see ADR-0002). The choice was Go vs Rust.

## Decision
Write the control-plane, core, native agent loop, and adapters in **Go** [R corrections/stack].
- Temporal's **Go SDK is GA and the flagship** — the durable-workflow substrate (ADR-0002) is first-class in Go, not a binding afterthought.
- Go is the **cloud-native lingua franca** (Kubernetes, Prometheus, the actuation MCP/kubectl/Proxmox ecosystem TG adapts), maximising contributor reach for an Apache-2.0 (ADR-0007) open-source product.
- `exec.Command(bin, args...)`, `pgx`/`sqlc`, `html/template`, and typed interfaces make the audit invariants (INV-02/03/04/17) cheap to satisfy *by construction* — a CI grep can ban `sh -c` and string-built SQL (INV-02/03) [O].

## Consequences
- Compiled Go orchestration means there is **no exportable workflow JSON to embed a secret into** (INV-13) [O] and no runtime "mode string" backend (INV-17) [O].
- Type-driven fail-closed defaults are idiomatic: a `Band` enum whose zero-value is the most-restrictive band (INV-09) [O] falls out of Go's zero-value semantics.
- Team must accept Go's weaker compile-time guarantees than Rust (no borrow-checker); mitigated by the test mandate in INV-22 [O].

## Alternatives
- **Rust** — stronger memory/typing guarantees, but the Temporal Rust SDK is not GA/flagship and the cloud-native/actuation ecosystem is thinner. Rejected: substrate maturity and ecosystem reach outweigh the marginal safety delta, which INV-02/03/22 already close in Go.
- **Keep Python/bash** (predecessor [F]) — rejected as the source of the injection class (C-02/03/04) [O]; greenfield [R paradigm-rule 7] discards it.
