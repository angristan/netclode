# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files and download deps (cached unless go.mod/go.sum change)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code
COPY . .

# Build the binary
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /secret-proxy ./cmd/secret-proxy

# Runtime stage
FROM alpine:3.21

# Install ca-certificates for HTTPS
RUN apk --no-cache add ca-certificates

# Copy binary
COPY --from=builder /secret-proxy /usr/local/bin/secret-proxy

# Create directory for CA certificates (owned by nobody:nobody, UID 65534)
# Using UID 65534 (nobody) - must match the UID in iptables rules
RUN mkdir -p /etc/secret-proxy && chown 65534:65534 /etc/secret-proxy

# Default environment
ENV LISTEN_ADDR=:8080
ENV CA_CERT_PATH=/etc/secret-proxy/ca.crt
ENV CA_KEY_PATH=/etc/secret-proxy/ca.key

# Run as nobody (UID 65534) - must match iptables exclusion
USER nobody

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/secret-proxy"]
