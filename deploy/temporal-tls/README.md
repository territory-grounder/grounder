# Temporal frontend mTLS (TG-660 part a)

> Reference material, read on demand. The queue is `docs/BOARD.md`; open work lives in YouTrack.

## What it closes

Until this change the Temporal frontend (`temporal:7233`, backplane-only) admitted **any** dialer on
`tg-backplane`: a plaintext `temporal workflow start` of an admin-write workflow with `AdminAuthorized`
set, a `VoteSignal` from anything that could reach the port, a rogue worker polling the task queue. The
signed authorization envelope (TG-660 part b) makes forged *starts* worthless; mutual TLS makes the
**connection** conditional on a client certificate issued by the deployment's own Temporal CA. With
`requireClientAuth` on, the frontend refuses the TLS handshake from everything that is not one of the
five issued identities.

## The identities

| leaf       | presented by                                 | EKU                      |
|------------|----------------------------------------------|--------------------------|
| `server`   | the Temporal frontend and its internode roles | serverAuth + clientAuth |
| `grounder` | the grounder (signs admin writes and votes)   | clientAuth               |
| `worker`   | worker-1 (the triage plane)                   | clientAuth               |
| `actuate`  | worker-actuate (the actuation plane)          | clientAuth               |
| `ui`       | temporal-ui                                   | clientAuth               |
| `cli`      | auto-setup's bundled `temporal` CLI; the operator's recipe | clientAuth    |

The server leaf carries both EKUs because, per
[docs.temporal.io/references/service-configuration](https://docs.temporal.io/references/service-configuration)
(`global.tls`), when client authentication is enabled the service roles use the **internode server
certificate as their client certificate**, and `requireClientAuth` is one flag shared by the frontend
and the internode sections of the auto-setup config template.

## Minting and storing

OpenBao has no PKI mount here, so `issue.sh` mints with openssl and stores in KV:

```
BAO_ADDR=… BAO_TOKEN=… deploy/temporal-tls/issue.sh            # add --dry-run to only verify the chain
```

- `secret/tg/temporal-tls/<leaf>` → fields `ca`, `cert`, `key` (readable by the policies the TG
  processes already hold for `secret/data/tg/*`; the actuation plane's `tg-actuate-ro` policy needs the
  one added path `secret/data/tg/temporal-tls/actuate`).
- `secret/tg-ops/temporal-tls-ca` → the CA certificate and **key**, outside every TG policy's prefix.
  Needed only to issue further leaves.

Nothing touches a disk outside a private temp directory that is removed on exit.

## Who can read which key (stated, not glossed)

Read from the live OpenBao policies on 2026-11-07, not from intent:

- `tg-actuate-ro` (worker-actuate's AppRole) reads exactly one path here: `secret/data/tg/temporal-tls/actuate`.
- `tg-credential-ro` (the grounder's cert identity, also the identity the two inits authenticate with) and
  `tg-triage-ro` both read `secret/data/tg/*`, so each can read EVERY leaf here, the server key included.
  worker-1 carries both a cert identity and AppRole refs, and the credential validator gives the cert
  bootstrap precedence, so which of the two it runs under is not asserted here.
- The CA KEY sits at `secret/tg-ops/temporal-tls-ca`, outside every TG policy's prefix.

What that means: the Temporal frontend admits any certificate this CA issued, with no per-identity
authorization (no authorizer or claim mapper is configured), so the leaves separate identities for
attribution and rotation, not for permission. mTLS closes the path for everything that holds NO leaf — a
container on the backplane, a rogue process, a plaintext CLI. A compromised TG process that already holds a
leaf is bounded by the signed envelopes (TG-660 part b) and the execute-time re-derivation (part c), not by
this. Moving the server/CLI/UI leaves out of `tg/*` to a dedicated init identity is a follow-up on the ticket.

## Turning it on

1. `issue.sh` (above).
2. In `.env`: `TG_TEMPORAL_TLS_ENABLED=1` plus the `*_REF` lines from `deploy/.env.example` — the
   server/cli refs for the `temporal` service's file drop, the ui refs for `temporal-ui`'s drop, and the
   three in-process refs for each of grounder, worker and worker-actuate (`TG_ACTUATE_TEMPORAL_TLS_*`).
3. `docker compose up -d` — one convergence recreates the inits, `temporal`, `temporal-ui` and the three
   TG processes together. The worker's fatal dial retries under `restart: unless-stopped`; the grounder's
   patient dial covers the race while the frontend restarts. Workflow history lives in temporal-postgres and
   survives the recreate (rehearsed below).

How the material reaches each consumer:

- the Go processes resolve `TG_TEMPORAL_TLS_{CA,CERT,KEY}_REF` **in-process** (`temporal/clienttls`);
  the PEM never touches a shared disk;
- `temporal` and `temporal-ui` read **files**, so `tg-secretenv -files` (the same binary that drops the
  litellm and sidecar secrets) writes a tmpfs drop each container mounts read-only — written by the uid
  that reads it (1000 = temporal, 5000 = temporal-ui) with gid 65532 so the init can read the OpenBao
  client certificate. `temporal-tls-perms` owns and modes the two drops first (the sidecar-perms pattern).

With the switch unset every TLS value interpolates to empty (`${TG_TEMPORAL_TLS_ENABLED:+…}`): the frontend
stays plaintext and every dialer dials plaintext. It is NOT byte-identical: the three inits still run (and exit
0 having written nothing), the two tmpfs drops exist, and `temporal` waits for its init, so `temporal` now also
needs the worker image to be pullable.

## Fail-closed rules

- A TG process with **any** of its three refs set and not all three resolving and parsing **refuses to
  boot** (`temporal: … refusing to boot rather than dial the frontend without the configured client
  identity`). Configured-for-mTLS-but-dialing-plaintext is the gap, never a fallback.
- Unset refs ⇒ a plaintext dial, and the boot line says `PLAINTEXT dial (TG_TEMPORAL_TLS_*_REF unset)`.
- `tg-secretenv -files` resolves every ref before it writes the first byte; a failure writes nothing and
  the dependent container does not start.
- Host verification stays on: the dialer verifies the frontend as `TG_TEMPORAL_TLS_SERVER_NAME`
  (default `temporal`, the SAN on the server leaf).

## Verifying it live

- Boot lines: grounder / worker / worker-actuate log `temporal: mTLS dial, server name "temporal", client
  certificate CN="…"`; the UI connects (`http://127.0.0.1:8233` on the host loads workflows).
- The plaintext path is gone. From a container that holds no leaf, on the backplane:
  `docker run --rm --network <backplane> --entrypoint temporal <the pinned auto-setup image> operator cluster
  health --address temporal:7233` fails (`error reading server preface: EOF`).
- The operator path works with no flags: `docker exec <temporal container> temporal operator cluster health`
  answers `SERVING`, because the container's environment carries the CLI leaf under the names the bundled CLI
  reads (`TEMPORAL_ADDRESS`, `TEMPORAL_TLS_CERT`, `TEMPORAL_TLS_KEY`, `TEMPORAL_TLS_CA`,
  `TEMPORAL_TLS_SERVER_NAME`). Those names were read from the binary; `TEMPORAL_TLS_*_PATH` is NOT read by it.
- A console mode read still works; the next organic autonomous heal still executes (no fault injection).

## Rehearsal (2026-11-07, disposable stack, the pinned images)

The cutover and the rollback were rehearsed on a throwaway network with the pinned `temporalio/auto-setup:1.25.2`,
`temporalio/ui:2.34.0` and `postgres:16`, a throwaway CA minted with this directory's openssl recipe, and the
`temporal` / `temporal-ui` environments taken from `docker compose config` of this file (switch off, then on):

- Plaintext boot first (namespace + search attributes created), then one plaintext `ModeTransitionWorkflow` start
  with `AdminAuthorized:true` — accepted, which is the defect this closes.
- Switch on: the server loaded the server leaf and CA, logged no error, and auto-setup's own CLI health loop
  reached it (`Temporal server started.`). The empty `TEMPORAL_TLS_CLIENT2_CA_CERT` entry is dropped by the
  server, not read as a path.
- Refused: a plaintext SDK dial, a plaintext `ModeTransitionWorkflow` start, a plaintext worker on `tg.runner`,
  a plaintext CLI start and signal, TLS with the right CA but no client certificate, a leaf from an impostor CA
  with the same CA name, and a client expecting the wrong server name. A partial `TG_TEMPORAL_TLS_*_REF` set is
  refused before any dial.
- Admitted: the worker, actuate and grounder leaves (built exactly as the binaries build them), a worker on
  `tg.runner` with the issued leaf, the in-container CLI with no flags, and the UI over its own leaf (lists the
  namespaces and the pre-cutover workflow).
- Fail closed: switch on with an EMPTY drop ⇒ the server exits 1 (`open /run/temporal-tls/cli.pem: no such
  file`); it never falls back to plaintext.
- Rollback: switch off again ⇒ plaintext serves and the pre-cutover workflow is still listed.

## Rotation and revocation

Rotation: re-run `issue.sh` (new KV versions), then `docker compose up -d --force-recreate temporal
temporal-ui grounder worker worker-actuate`. Revocation of a single leaf is not available (no CRL/OCSP
in this shape); rotating the CA invalidates every old leaf at once.

## Rollback

1. In `.env`, remove `TG_TEMPORAL_TLS_ENABLED` AND every `TG_TEMPORAL_TLS_*_REF` / `TG_ACTUATE_TEMPORAL_TLS_*_REF`
   line (restore the pre-cutover backup). Removing only the switch leaves the TG processes dialing TLS against
   a plaintext frontend, and they refuse to boot.
2. `docker compose up -d` — recreates `temporal`, `temporal-ui`, the inits and the three TG processes on
   plaintext. History is untouched (rehearsed above). The leaves in OpenBao can stay.
