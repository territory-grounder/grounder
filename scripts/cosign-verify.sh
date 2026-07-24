#!/usr/bin/env bash
# Verify container image refs against the committed public key (spec/022 REQ-2206). This is the deploy-time
# gate: the production host runs ONLY images signed by the OpenBao-held key, so a registry compromise that
# swaps an image cannot reach the estate. Uses deploy/cosign.pub (public, in-repo) — no OpenBao access needed,
# so the verify side holds no secret. cosign runs NATIVELY (shares the job's docker login — reading the
# signature from the registry still needs pull auth). Usage: cosign-verify.sh <image-ref> [<image-ref> ...]
set -euo pipefail
[ "$#" -ge 1 ] || { echo "usage: cosign-verify.sh <image-ref> [<image-ref> ...]"; exit 2; }
PUB="${COSIGN_PUB:-deploy/cosign.pub}"
[ -s "$PUB" ] || { echo "FATAL: public key $PUB not found"; exit 1; }

apk add --no-cache cosign >/dev/null 2>&1 || true
if ! command -v cosign >/dev/null 2>&1; then
  apk add --no-cache curl >/dev/null 2>&1 || true
  for i in 1 2 3; do
    curl -sSfL -o /usr/local/bin/cosign "https://github.com/sigstore/cosign/releases/download/v2.4.1/cosign-linux-amd64" && break
    echo "cosign fetch attempt $i failed (github egress); retrying in $((i*5))s"; sleep $((i*5))
  done
  chmod +x /usr/local/bin/cosign
  command -v cosign >/dev/null 2>&1 || { echo "FATAL: cosign unavailable (apk + github both failed)"; exit 1; }
fi

rc=0
for ref in "$@"; do
  if cosign verify --key "$PUB" "$ref" >/dev/null 2>&1; then
    echo "cosign verify: OK   $ref"
  else
    echo "cosign verify: FAIL $ref  (unsigned or signed by an untrusted key — refusing to deploy)"; rc=1
  fi
done
exit $rc
