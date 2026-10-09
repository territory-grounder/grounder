# TG-420 slice 1 — the litellm -> model-provider EGRESS PROXY (tinyproxy), OBSERVE / log-only.
#
# WHY A PROXY AND NOT iptables (TG-420, parent TG-324 item 3). TG's Go process talks to litellm; litellm
# talks to the providers (Anthropic via the sidecar, plus DeepSeek/Mistral/Z.AI/… directly). That second
# hop is in a different container, so the in-process core/egress meter (an http.RoundTripper) structurally
# cannot see it, and it carries the full prompt content off-box. Provider APIs are CDN-fronted, so an IP
# allowlist rots continuously; a DOMAIN allowlist at an HTTP CONNECT proxy is the stable control.
#
# BUILT FROM ALPINE, NOT PULLED. tinyproxy has no first-party signed registry image; building it from the
# official Alpine base (pinned by @sha256 below, and scanned by scripts/lint-image-pins.sh) keeps the
# supply chain to Alpine's own signed package repos rather than a third party's :latest — appropriate for
# a control that sits on the single-brain model path. The compose service carries `build:`, so its image
# name is exempt from the image-pin gate as a locally-built image; THIS base is the pinned surface.
#
# alpine:3.21 digest resolved 2026-08-14 via `docker buildx imagetools inspect alpine:3.21`.
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d

# tinyproxy lives in Alpine's community repo (enabled by default in the official image).
RUN apk add --no-cache tinyproxy

# The OBSERVE-mode config + the FENCE definition (the three owner-ruled endpoints, TG-488 B11) that slice 2
# arms. deploy/egress_proxy_allowlist_test.go asserts the fence == the ruling and reports the stranger delta.
COPY tinyproxy.conf /etc/tinyproxy/tinyproxy.conf
COPY provider-allowlist.txt /etc/tinyproxy/provider-allowlist.txt

# The proxy listens on 8888 for in-stack clients only (no host port is published in compose).
EXPOSE 8888

# Foreground (-d) so `docker compose logs` captures the per-CONNECT destination lines that ARE the observe
# signal. Runs as a non-root user set in compose (`user:`), so no in-process setuid/User directive is
# needed and the container can drop ALL capabilities.
ENTRYPOINT ["tinyproxy", "-d", "-c", "/etc/tinyproxy/tinyproxy.conf"]
