#!/usr/bin/env bash
# deploy/temporal-tls/issue.sh — mint the Temporal frontend mTLS material (TG-660 part a) and store it in OpenBao KV.
#
# OpenBao has no PKI mount in this deployment, so the CA and its leaves are minted here with openssl and written to
# the KV engine the TG processes already read from. Nothing is written to disk outside a private temp directory
# that is removed on exit; the CA KEY goes to a path OUTSIDE every TG policy's prefix (tg-ops/), the leaves to
# secret/tg/temporal-tls/<leaf> with the fields ca, cert, key.
#
# Leaves:  server   — the frontend AND internode certificate (SAN: the compose service name + localhost;
#                     EKU serverAuth + clientAuth, because Temporal's own roles dial the frontend with it)
#          grounder, worker, actuate, ui, cli — client leaves (EKU clientAuth), one identity per dialer
#
# Rotation = re-run (new KV versions) then `docker compose up -d --force-recreate temporal temporal-ui grounder
# worker worker-actuate`. Revoke = rotate the CA (every old leaf stops chaining).
#
# Usage: BAO_ADDR=… BAO_TOKEN=… ./issue.sh [--server-name temporal] [--ca-days 1825] [--leaf-days 365]
#        [--kv-prefix secret/tg/temporal-tls] [--ca-key-path secret/tg-ops/temporal-tls-ca] [--dry-run]
set -euo pipefail

server_name="temporal"
ca_days=1825
leaf_days=365
kv_prefix="secret/tg/temporal-tls"
ca_key_path="secret/tg-ops/temporal-tls-ca"
dry_run=0
while [ $# -gt 0 ]; do
  case "$1" in
    --server-name) server_name="$2"; shift 2 ;;
    --ca-days) ca_days="$2"; shift 2 ;;
    --leaf-days) leaf_days="$2"; shift 2 ;;
    --kv-prefix) kv_prefix="$2"; shift 2 ;;
    --ca-key-path) ca_key_path="$2"; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 3; }
if [ "$dry_run" -eq 0 ]; then
  command -v bao >/dev/null || { echo "bao CLI is required (or pass --dry-run)" >&2; exit 3; }
  : "${BAO_ADDR:?set BAO_ADDR}"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 700 "$work"
cd "$work"

# The CA: ECDSA P-256, pathlen 0, no reuse outside this purpose.
openssl ecparam -name prime256v1 -genkey -noout -out ca.key 2>/dev/null
openssl req -new -x509 -key ca.key -days "$ca_days" -subj "/CN=tg-temporal-ca" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out ca.pem 2>/dev/null

leaf() { # name  eku  san-or-empty
  local name="$1" eku="$2" san="$3"
  openssl ecparam -name prime256v1 -genkey -noout -out "$name.key" 2>/dev/null
  openssl req -new -key "$name.key" -subj "/CN=$name" -out "$name.csr" 2>/dev/null
  {
    echo "basicConstraints=CA:FALSE"
    echo "keyUsage=critical,digitalSignature,keyEncipherment"
    echo "extendedKeyUsage=$eku"
    [ -n "$san" ] && echo "subjectAltName=$san"
  } > "$name.ext"
  openssl x509 -req -in "$name.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -days "$leaf_days" \
    -extfile "$name.ext" -out "$name.pem" 2>/dev/null
}
leaf server "serverAuth,clientAuth" "DNS:$server_name,DNS:localhost,IP:127.0.0.1"
for n in grounder worker actuate ui cli; do leaf "$n" "clientAuth" ""; done

echo "minted: ca (CN=tg-temporal-ca, $ca_days d) + leaves server/grounder/worker/actuate/ui/cli ($leaf_days d), server SAN DNS:$server_name"
openssl verify -CAfile ca.pem server.pem grounder.pem worker.pem actuate.pem ui.pem cli.pem

if [ "$dry_run" -eq 1 ]; then
  echo "dry run: nothing written to OpenBao"
  exit 0
fi
# The CA private key: outside tg/ so no TG process policy can read it. Needed only to issue more leaves.
bao kv put "$ca_key_path" ca=@ca.pem key=@ca.key >/dev/null
for n in server grounder worker actuate ui cli; do
  bao kv put "$kv_prefix/$n" ca=@ca.pem cert=@"$n.pem" key=@"$n.key" >/dev/null
  echo "stored $kv_prefix/$n (ca, cert, key)"
done
echo "stored the CA key at $ca_key_path"
echo "next: set TG_TEMPORAL_TLS_ENABLED=1 and the *_REF lines (deploy/.env.example) and recreate the stack."
