FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files first and download deps (cached unless go.mod/go.sum change)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source and build (re-runs on code changes, but modules are cached)
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o control-plane ./cmd/control-plane

# Runtime image
FROM alpine:3.21

RUN apk --no-cache add ca-certificates

COPY --from=builder /app/control-plane /control-plane

# Run as non-root user
USER 65534:65534

EXPOSE 3000

ENTRYPOINT ["/control-plane"]
