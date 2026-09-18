# ADR 0007 — License: Apache-2.0

## Status
Accepted.

## Context
TG is an **open-source, self-hosted, distributable** product [R paradigm-rule / corrections], not a private one-estate tool [F]. Its stack and ecosystem are squarely CNCF-adjacent: Go (ADR-0001), Temporal, Kubernetes/Prometheus/kubectl actuation adapters, LiteLLM (ADR-0004). A **module SDK and third-party modules** (ADR-0005) mean external contributors and downstream redistributors need clear patent and contribution terms. The license must invite adoption across the full audience spectrum — from a single-node self-hosted deployment to a multi-team production estate [R rule / mission] — and must be compatible with the dependency graph.

## Decision
License TG under **Apache-2.0** [R corrections/identity].
- **Matches the CNCF-adjacent ecosystem** TG lives in (Temporal, Kubernetes, Prometheus, and most Go cloud-native infrastructure ship Apache-2.0), minimising license-compatibility friction for adopters and module authors.
- Provides an **explicit patent grant** and clear contribution terms — important for a governed-autonomy product whose safety controls (the prediction gate, ledger, manifest binding) may attract patent concern, and for the third-party module marketplace (ADR-0005).
- Permissive terms maximise **self-hosted redistribution** across the full audience spectrum.

## Consequences
- Downstream may create closed derivatives; accepted — reach and ecosystem fit outweigh copyleft's redistribution guarantees for this product.
- All bundled reference modules, the SDK, `adapters/`, and generated contracts (INV-15) [O] ship Apache-2.0; a per-file/`NOTICE` provenance discipline is required, aligning with the secret-scrubbed, open-source-safe-by-default distribution posture (INV-13, S8-7) [O].
- Contributions are inbound-Apache-2.0 (or DCO); the module SDK carries the same terms so third-party modules interoperate cleanly.

## Alternatives
- **AGPL-3.0** — would force network-service source disclosure (attractive for a hosted-SaaS moat), but creates compatibility friction across the Apache-heavy dependency graph and deters some self-hosting adopters and module authors. Rejected for ecosystem fit.
- **MIT / BSD** — permissive and simple, but lacks Apache-2.0's explicit patent grant, which matters for the governed-autonomy IP and a plugin ecosystem. Rejected.
- **BSL / SSPL (source-available)** — rejected: contradicts the "open-source, not a black box" product promise [R].
