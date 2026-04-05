# Build stage
FROM golang:1.24.2-bookworm AS builder

WORKDIR /workspace

# Cache module downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY api/ api/
COPY internal/ internal/
COPY cmd/ cmd/

# Build the manager binary (entrypoint added in Increment 2)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager ./cmd/manager/...

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
