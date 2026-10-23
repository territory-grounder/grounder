# omoikane claudecode-runner image — Rust harness + Anthropic Claude Code CLI.
#
# Stage 1: cargo build --release of the Rust axum server.
# Stage 2: debian-slim + Node.js 20 LTS + global @anthropic-ai/claude-code +
#          the Rust binary. ANTHROPIC_API_KEY is read directly by the CLI at
#          subprocess time.
#
# Image size budget: ~250 MB (debian-slim ~80 MB + nodejs ~70 MB + claude-code
# ~80 MB + Rust binary ~5 MB + ca-certificates).

# DIGEST-PINNED (TG-310). Both bases floated, so two builds a week apart could ship different toolchains
# and different libc/openssl under the same Dockerfile — on the sidecar that runs model-directed work on the
# box holding an OpenBao root token.
#
# The pin is also its own evidence: this ticket recorded debian:bookworm-slim at
# sha256:7b140f37… on 2026-08-04, and resolving it again on 2026-08-05 gave
# sha256:abd67ffc…. The tag moved within a day. That is not a hypothetical drift argument, it is the drift.
#
# The tag is kept alongside the digest so a human can still read what this IS; docker resolves by digest and
# ignores the tag, so the two cannot disagree in effect — only in what a reader believes.
ARG RUST_TAG=1.94.0-bookworm@sha256:365468470075493dc4583f47387001854321c5a8583ea9604b297e67f01c5a4f
ARG RUNTIME_TAG=bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241
ARG NODE_MAJOR=20
# AGRIOPS-208: PINNED. `latest` made the image non-reproducible — two builds a
# week apart shipped different CLIs, and the argv contract this harness depends
# on (`--tools` being variadic, `--input-format stream-json` requiring
# --print/--output-format/--verbose) is a CLI-version property. 2.1.220 is the
# version every measurement and every argv test in src/main.rs was taken
# against. Bump deliberately, re-run the tests, never float.
ARG CLAUDE_CODE_VERSION=2.1.220

# ---------- builder ----------
FROM rust:${RUST_TAG} AS builder

WORKDIR /build

# Pre-fetch deps with a stub main so source edits don't invalidate the dep cache.
COPY Cargo.toml Cargo.lock* ./
RUN mkdir -p src && echo 'fn main() {}' > src/main.rs && \
    cargo build --release --locked && \
    rm -rf src target/release/claudecode-runner* target/release/deps/claudecode_runner*

COPY src ./src
# AGRIOPS-208: the test suite is a BUILD GATE, not a nicety. It locks the argv
# invariants that are invisible at compile time (a variadic `--tools` must never
# be last; `--strict-mcp-config` must survive; the stream-json stdin contract
# must stay complete) and the pool's use-once invariant. A broken build must not
# be able to produce an image.
RUN cargo test --release --locked && \
    cargo build --release --locked && \
    strip target/release/claudecode-runner

# ---------- runtime ----------
FROM debian:${RUNTIME_TAG}

ARG NODE_MAJOR
ARG CLAUDE_CODE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && apt-get purge -y curl gnupg \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* /root/.npm /tmp/* \
    && claude --version

COPY --from=builder /build/target/release/claudecode-runner /usr/local/bin/claudecode-runner

# OMOIKANE-706: pre-create the state-volume mount target with UID 1001
# ownership so the OAuth-rotation override file is writable on first
# rotation. Without this, Docker's named-volume initialiser copies the
# directory ownership from the image — and if the dir is owned by root
# (the default for any `mkdir` in a Dockerfile), the named volume is
# also root-owned. The runner then runs as UID 1001 and gets
# `EACCES` on `oauth-override` write, returning 500 to the daemon's
# rotation POST. Observed 2026-05-25 on the first prod deploy of this
# image; recovery required a one-shot `docker exec -u 0 chown -R
# 1001:1001 /home/tg/.claude-state`. With this layer, fresh
# named volumes inherit the right ownership from the image.
RUN mkdir -p /home/tg/.claude-state \
    && chown -R 1001:1001 /home/tg

# Bind to host's mesh IP — same pattern as the colbert encoder + temporal.
# Override at runtime via LISTEN_ADDR=${NODE_ADDRESS}:8093 in compose.
ENV LISTEN_ADDR=0.0.0.0:8093 \
    CLAUDE_BIN=/usr/bin/claude \
    RUST_LOG=info,claudecode_runner=debug

EXPOSE 8093

ENTRYPOINT ["/usr/local/bin/claudecode-runner"]
