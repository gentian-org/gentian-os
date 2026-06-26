# Build stage
FROM golang:1.25.0-bookworm AS builder

WORKDIR /workspace

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/

# Build the manager binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager ./cmd

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /workspace/manager /manager

USER 65532:65532

ENTRYPOINT ["/manager"]
