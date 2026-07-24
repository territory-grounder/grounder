#!/usr/bin/env bash
# Supply-chain gate (TG-159 / wisdom-audit #3): every THIRD-PARTY base/runtime image in the deployed path
# (deploy/Dockerfile + deploy/docker-compose.yml) MUST be pinned by @sha256. A floating tag silently changes
# the trusted computing base under a signed governance system — most acutely `litellm:main-stable`, which holds
# every provider API key. The TG-built worker/grounder images are the pipeline's OWN output (registry +
# ${TG_TAG}), not a third-party base, so they are exempt. Resolve a digest with:
#   docker buildx imagetools inspect <repo>:<tag> --format '{{.Manifest.Digest}}'
# then write it as  repo:tag@sha256:<digest>  (keep the tag for readability; the digest is what's enforced).
set -u
unpinned=$(grep -hnE '^[[:space:]]*(FROM|image:)[[:space:]]' deploy/Dockerfile deploy/docker-compose.yml 2>/dev/null \
  | grep -iE 'golang|distroless|pgvector|litellm|postgres:|temporalio|prom/|grafana/' \
  | grep -v '@sha256')
if [ -n "$unpinned" ]; then
  echo "FAIL: unpinned third-party image(s) — pin by @sha256 (repo:tag@sha256:<digest>):"
  echo "$unpinned"
  exit 1
fi
echo "image-pin gate: PASS — all third-party base/runtime images pinned by @sha256"
