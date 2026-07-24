#!/usr/bin/env bash
# Sign container image refs with the cosign key resolved from OpenBao (spec/022 REQ-2206). The private key and
# its password live ONLY in OpenBao (secret/tg/cosign) — this fetches them into a tmp dir for the single sign
# and removes them on exit; nothing is echoed or committed.
#
# cosign runs NATIVELY in the calling job (not `docker run cosign`) so it shares the job's `docker login`
# credentials (~/.docker/config.json, written by the .image-build before_script) — a cosign container would
# have no registry auth and the signature push would be DENIED. docker:28 is Alpine, so the tooling (cosign +
# curl + python3) is apk-added on demand, with a pinned-binary fallback if the package is unavailable.
# Env: OPENBAO_ADDR, OPENBAO_TOKEN (a read-scoped token — NEVER root). Usage: cosign-sign.sh <ref> [<ref> ...]
set -euo pipefail
: "${OPENBAO_ADDR:?OPENBAO_ADDR unset}" ; : "${OPENBAO_TOKEN:?OPENBAO_TOKEN unset}"
[ "$#" -ge 1 ] || { echo "usage: cosign-sign.sh <image-ref> [<image-ref> ...]"; exit 2; }

apk add --no-cache curl python3 cosign >/dev/null 2>&1 || apk add --no-cache curl python3 >/dev/null 2>&1 || true
if ! command -v cosign >/dev/null 2>&1; then
  echo "cosign not packaged — fetching pinned binary"
  for i in 1 2 3; do
    curl -sSfL -o /usr/local/bin/cosign "https://github.com/sigstore/cosign/releases/download/v2.4.1/cosign-linux-amd64" && break
    echo "cosign fetch attempt $i failed (github egress); retrying in $((i*5))s"; sleep $((i*5))
  done
  chmod +x /usr/local/bin/cosign
  command -v cosign >/dev/null 2>&1 || { echo "FATAL: cosign unavailable (apk + github both failed)"; exit 1; }
fi

export KEYDIR=$(mktemp -d)
trap 'rm -rf "$KEYDIR"' EXIT
COSIGN_PASSWORD=$(curl -sk --max-time 20 -H "X-Vault-Token: $OPENBAO_TOKEN" "$OPENBAO_ADDR/v1/secret/data/tg/cosign" \
  | python3 -c 'import sys,json,os
d=json.load(sys.stdin)["data"]["data"]
open(os.environ["KEYDIR"]+"/cosign.key","w").write(d["private_key"])
print(d["password"])')
export COSIGN_PASSWORD
[ -s "$KEYDIR/cosign.key" ] || { echo "FATAL: could not resolve the cosign key from OpenBao ($OPENBAO_ADDR)"; exit 1; }

for ref in "$@"; do
  echo "cosign sign: $ref"
  cosign sign --yes --key "$KEYDIR/cosign.key" "$ref"
done
echo "cosign: signed $# image(s) with the OpenBao-held key"

# Optional SLSA provenance attestation (TG-158): when COSIGN_ATTEST_PREDICATE points at a provenance/SBOM
# predicate file, attach a signed attestation (default predicate type cyclonedx; override via
# COSIGN_ATTEST_TYPE) to each ref's registry entry. `cosign attest` REQUIRES the ref to already be signed
# (it extends the signature, not create it), so this runs AFTER the sign loop above. Same key + registry
# auth; the predicate file itself is a build artifact (no secret).
if [ -n "${COSIGN_ATTEST_PREDICATE:-}" ]; then
  ATTEST_TYPE="${COSIGN_ATTEST_TYPE:-cyclonedx}"
  [ -f "$COSIGN_ATTEST_PREDICATE" ] || { echo "FATAL: attestation predicate not found: $COSIGN_ATTEST_PREDICATE"; exit 1; }
  for ref in "$@"; do
    echo "cosign attest ($ATTEST_TYPE): $ref"
    cosign attest --yes --key "$KEYDIR/cosign.key" --predicate "$COSIGN_ATTEST_PREDICATE" --type "$ATTEST_TYPE" "$ref"
  done
  echo "cosign: attached $ATTEST_TYPE provenance attestation to $# image(s)"
fi
