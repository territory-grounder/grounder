# Image signature verification — REMOVED (TG-417)

> Reference, not a queue. The open work is YouTrack `project: TG #Unresolved`.

## Status: there is no cosign signature verification in this pipeline.

It was removed on 2026-08-08 (TG-417, owner decision). The `deploy` job gates only on the three image
builds and runs as it did before the supply chain was added: **build → push → pull → up**.

## Why it was removed

The signing half worked; the **registry did not durably retain the signatures**. Images were signed in
CI (the first `cosign-verify` pass found the signatures) and the `sha256-*.sig` artifacts were then dropped
from the registry within hours — verified with cosign v2.6.5 against the registry: `no signatures found`
for every image including the running one, the committed `deploy/cosign.pub` matches the OpenBao signing
key byte-for-byte, `--insecure-ignore-tlog` makes no difference, and a fresh re-sign does not persist for
seconds. So `cosign-verify` red **every** main pipeline and `deploy: needs: [cosign-verify]` skipped the
rollout — the estate was frozen on a stale image while every merge piled up undeployed and emailed a
failed pipeline.

Removed jobs: `cosign-sign`, `cosign-sidecar`, `cosign-attest`, `cosign-verify`, `deploy-host-cosign`,
and the `supply-chain-consumption` drill; `deploy` / `deploy-sidecar` no longer `need` them.

## What still exists

The orphaned `scripts/cosign-*.sh` family, `scripts/check-deploy-host-cosign*.sh` (+ tests), and
`deploy/cosign.pub` were **deleted on 2026-08-10** (owner ruling, TG-417) — recover them from git history
if signing returns. The build-side integrity gates that do NOT depend on the registry keeping extra
artifacts still run: `govulncheck`, `image-pins` (+ its drill), `trivy-image-scan`, `sbom`.

## Before re-introducing signing

Do not re-add cosign until the registry is proven to **retain** `sha256-*.sig` / `.att` artifacts (its
cleanup policy keeps only `latest`, and an instance-level GC appears to prune the rest). Confirm retention
first — sign an image, wait, and check the signature is still verifiable — then wire signing back in. See
TG-417 for the full diagnosis.
