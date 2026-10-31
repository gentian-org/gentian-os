# Build stage
# BUILDPLATFORM keeps the compile on the native runner while GOARCH targets the
# requested platform — a cross-compile, not emulation, which for a static Go
# binary is both correct and far faster than running the toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS builder
ARG TARGETARCH

WORKDIR /workspace

# Cache module downloads
COPY go.mod go.sum ./
# The API is a module of its own that this one replaces with ./api, so its
# go.mod has to be here before anything can be resolved.
COPY api/go.mod api/go.sum api/
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

# The usher beside it: the reads a signed-in person's desktop asks for, from a
# process that holds nothing but the two things the bouncer holds.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o usher ./cmd/usher

# The custodian: what sets a credential in the vault for the person entitled
# to. Its own process and ServiceAccount, so the identity that can write a
# secret is not shared with anything that can read one.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o custodian ./cmd/custodian

# The registrar: what keeps the list of people at Keycloak. Its own process
# and ServiceAccount, so the credential that manages who may sign in is not
# held by the process that pushes to git.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o registrar ./cmd/registrar

# The arbiter: the one way into the app instance of OpenFGA, run in the
# system namespace beside it with its own ServiceAccount. The only holder of
# that instance's key.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o arbiter ./cmd/arbiter

# vault-migrator: what a release changes in the vault, run once per release.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o vault-migrator ./cmd/vault-migrator

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3

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
COPY --from=builder /workspace/usher /usher
COPY --from=builder /workspace/custodian /custodian
COPY --from=builder /workspace/registrar /registrar
COPY --from=builder /workspace/arbiter /arbiter
COPY --from=builder /workspace/vault-migrator /vault-migrator

USER 65532:65532

ENTRYPOINT ["/manager"]
