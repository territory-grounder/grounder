# Serve the Territory Grounder operator console (spec/010) behind nginx.
# The console is a read-only client of the auth-gated API; the static bundle carries no secret.
#
# ★ THE SERVED CONSOLE IS deploy/console/v2/index.html, AND IT IS NOW THE ONLY THING SERVED.
#
# This image used to run `npm ci && npm run build` over frontend/ and copy the result into the document root,
# then overwrite that build's index.html with the line below. The React entry point therefore never loaded,
# and its bundle was unreachable BY CONSTRUCTION — not through a configuration slip, but because the only
# document that could have referenced it was replaced on the next layer.
#
# Measured live on dc1tg01, 2026-07-29, before this stage was removed:
#   - the served index.html referenced the React bundle 0 times (it is self-contained; fonts inlined base64)
#   - nginx had served index-*.js / index-*.css 0 times across every request it had logged
#   - /assets/ — 973 KB: one JS bundle, one stylesheet, 56 font files — had been requested 0 times
#   - and all 8 spec/010 tasks certified that unreachable half, every one marked "completed"
#
# So every console image carried ~1 MB no operator could load, every pipeline paid for a node build nobody
# would run, and the component's tested% described a program that was never served.
#
# CORRECTED 2026-08-02: the sentence that stood here claimed "frontend/ stays in the repo and its CI job
# still typechecks and vitests it on change". There is NO such CI job — the React source was deleted by
# 4b0e9763 (ADR-0015) and spec/010 REQ-617 now certifies only deploy/console/v2/index.html. What DID stay
# in the repo is 6,647 node_modules files (97.7MB), because that same commit also removed the .gitignore
# lines hiding them; see TG-257.
#
# The load-bearing half of the original claim survives and is why this Dockerfile is written this way: what
# is SERVED and what is CERTIFIED can no longer drift apart silently, because deploy/served_console_test.go
# binds them.
# DIGEST-PINNED (TG-283). The old image-pin gate hard-coded deploy/Dockerfile and deploy/docker-compose.yml
# as its entire scan set, so this file — which builds the console image the pipeline ships, and whose
# container is the ONLY one published on 0.0.0.0 — was never read by it. `1.27-alpine` therefore floated
# under the front door while the gate reported PASS. The digest is the manifest-list digest of the
# nginx:1.27-alpine every measurement in the comment above was taken against; the tag stays for readability.
FROM nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10

# NON-ROOT, READ-ONLY-ROOTFS PREPARATION (TG-289).
#
# Until 2026-08-04 this container ran `root 1 nginx: master process` with RO=False, CapDrop=None,
# SecOpt=None — measured by `docker inspect` on dc1tg01 — while the worker and grounder next to it ran
# RO=True, CapDrop=[ALL], no-new-privileges. The console is the ONLY service published on 0.0.0.0, so the
# hardening had landed on everything except the front door.
#
# The stock nginx image cannot simply be run with `user: 101:101` + `cap_drop: [ALL]` + `read_only: true`.
# Each edit below removes one startup failure that was MEASURED, not guessed (nginx:1.27-alpine, docker
# 20.10.24, the container run exactly as deploy/docker-compose.yml declares it):
#
#   1. `pid /var/run/nginx.pid` → /run is on the rootfs, which is now read-only:
#        [emerg] open() "/run/nginx.pid" failed (30: Read-only file system)
#      The pid file moves onto the one tmpfs the container gets, next to the temp dirs.
#   2. `chmod 1777 /var/cache/nginx` → without it nginx (uid 101) cannot create its temp dirs on the tmpfs:
#        [emerg] mkdir() "/var/cache/nginx/client_temp" failed (13: Permission denied)
#      Docker's tmpfs default of 1777 applies only to a target ABSENT from the image; where the target
#      EXISTS it copies that directory's MODE and mounts the tmpfs root-owned. In this image
#      /var/cache/nginx is root:root 0755, so the tmpfs came up root:root 0755 and uid 101 could not
#      mkdir in it. Passing `mode=`/`tmpfs-mode=` does NOT help — this daemon ignores it on an existing
#      target (measured: `--tmpfs /alertmanager:mode=1777` changed nothing for the alertmanager service,
#      which is why that one is still not read-only). Setting the mode in the IMAGE is therefore the only
#      portable fix, and it is what the tmpfs then inherits: verified 1777 with client_temp, proxy_temp
#      and nginx.pid all created by uid 101.
#      The `chown` is NOT what makes the tmpfs work (a tmpfs is always mounted root-owned, and the sticky
#      1777 is what lets 101 write). It is kept for the deployment that swaps the tmpfs for a named
#      volume, since a volume DOES initialise from the image directory's ownership.
#   3. `user nginx;` is meaningless once the master is already unprivileged and makes nginx log a warning
#      on every start; dropping it keeps the console's logs free of a line that invites a wrong diagnosis.
#
# The listen port moved 80 → 8080 in nginx.conf for the same reason: binding a privileged port needs
# CAP_NET_BIND_SERVICE, and the point is to hold cap_drop [ALL] with NOTHING added back. The PUBLISHED
# port is unchanged (host 8080), so every operator URL and the HTTPS front in front of it still resolve.
RUN sed -i -e 's!^pid .*!pid /var/cache/nginx/nginx.pid;!' -e '/^user  *nginx;/d' /etc/nginx/nginx.conf \
 && chown -R 101:101 /var/cache/nginx \
 && chmod 1777 /var/cache/nginx
COPY deploy/console/nginx.conf /etc/nginx/conf.d/default.conf
COPY deploy/console/v2/index.html /usr/share/nginx/html/index.html
# The pre-auth login page, served by nginx to unauthenticated requests from its OWN root (NOT the SPA
# document root /usr/share/nginx/html). It is not an asset the self-contained bundle references, so keeping
# it out of the served root keeps served_console_test's dead-weight reachability guard exact.
COPY deploy/console/login.html /usr/share/nginx/login/login.html
USER 101:101
EXPOSE 8080
