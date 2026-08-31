# Multi-stage build for a TG control-plane binary → a small static distroless image. One Dockerfile builds
# either command via the BIN arg (grounder = the HTTP control plane, worker = the Temporal Runner worker).
# [R] one guided compose; static binary keeps the image tiny and CVE surface minimal.
ARG BIN=grounder
FROM golang:1.25.12@sha256:9006890ecba0a168034d99516084099ae3114d9f2b7d6572c77f2dde57ebc980 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BIN
# CGO off → fully static binary; trimpath for reproducibility.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BIN}
# tg-secretenv ships in every image (spec/024 REQ-2403): the deploy-time shim that resolves a THIRD-PARTY
# container's secrets from the vault into a memory-backed env file, so a fixed upstream image (e.g. the LiteLLM
# gateway) gets its key from OpenBao with no plaintext in .env. It is a second static binary, not the
# entrypoint — an init service overrides the entrypoint to invoke it.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tg-secretenv ./cmd/tg-secretenv

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
COPY --from=build /out/app /app
COPY --from=build /out/tg-secretenv /usr/local/bin/tg-secretenv
USER nonroot:nonroot
# Read-only foundation: mutation is disabled by construction (see core/safety); both binaries refuse to
# start with mutation enabled.
ENTRYPOINT ["/app"]
