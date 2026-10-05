# Build stage
# BUILDPLATFORM keeps the compile on the native runner while GOARCH targets the
# requested platform — a cross-compile, not emulation, which for a static Go
# binary is both correct and far faster than running the toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.25.7-bookworm AS builder
ARG TARGETARCH

WORKDIR /workspace

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/

# Build the manager binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -a -o manager ./cmd

# The director ships in the same image and runs as its own Deployment with its
# own identity: one image to build, scan and pin, two processes that share no
# credential. Which binary runs is the Deployment's command.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o director ./cmd/director

# The ext-auth bouncer ships the same way: the edge's one enforcement point, run
# in the edge namespace from this image with its own command.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o bouncer ./cmd/bouncer

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

# gnupg: the director signs what it commits (AD-2), and git shells out to
# gpg to do it. Without it every commit is unsigned and Argo CD refuses the
# repository -- which would show up as a cluster that syncs nothing, several
# layers from the missing package.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates gnupg \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/director /director
COPY --from=builder /workspace/bouncer /bouncer

USER 65532:65532

ENTRYPOINT ["/manager"]
