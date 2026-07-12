# Secret Proxy

MITM proxy that injects API secrets into HTTP headers. Runs outside sandbox microVMs.

For architecture overview and security analysis, see [docs/secret-proxy.md](../../docs/secret-proxy.md).

## Building

```bash
go build -o secret-proxy ./cmd/secret-proxy
go build -o genca ./cmd/genca
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | Address to listen on |
| `CONTROL_PLANE_URL` | required | URL of control-plane for token validation |
| `CONTROL_PLANE_TOKEN_PATH` | `/var/run/secrets/control-plane-auth/token` | Projected `secret-proxy` workload identity token |
| `CA_CERT_PATH` | `/etc/secret-proxy/ca.crt` | Path to CA certificate for MITM |
| `CA_KEY_PATH` | `/etc/secret-proxy/ca.key` | Path to CA private key |
| `SECRETS_PATH` | `/etc/secret-proxy/secrets.json` | Path to secrets JSON file |

### Secrets JSON Format

```json
{
  "anthropic": "sk-ant-api03-xxxxx...",
  "openai": "sk-xxxxx...",
  "mistral": "xxxxx...",
  "github_copilot": "gho_xxxxx..."
}
```

Keys match the `secret_key` returned by control-plane validation.

## Running Locally

```bash
# Generate CA certificate
go run ./cmd/genca -out /tmp/secret-proxy

# Run proxy
CONTROL_PLANE_URL=http://localhost:3002 \
CONTROL_PLANE_TOKEN_PATH=/var/run/secrets/control-plane-auth/token \
CA_CERT_PATH=/tmp/secret-proxy/ca.crt \
CA_KEY_PATH=/tmp/secret-proxy/ca.key \
SECRETS_PATH=./secrets.json \
go run ./cmd/secret-proxy
```

## Testing

```bash
go test ./...
```

## Docker

```bash
docker build -t secret-proxy .
docker run \
  -e CONTROL_PLANE_URL=http://control-plane-internal:3002 \
  -e CONTROL_PLANE_TOKEN_PATH=/var/run/secrets/control-plane-auth/token \
  -v /path/to/workload-token:/var/run/secrets/control-plane-auth/token:ro \
  -v /path/to/ca.crt:/etc/secret-proxy/ca.crt \
  -v /path/to/ca.key:/etc/secret-proxy/ca.key \
  -v /path/to/secrets.json:/etc/secret-proxy/secrets.json \
  secret-proxy
```

## How It Works

1. Receives CONNECT request with `Proxy-Authorization: Bearer <token>` header
2. Validates token with control-plane (`POST /internal/validate-proxy-auth`)
3. If allowed, establishes HTTPS tunnel with MITM
4. Scans request headers for placeholder values (e.g., `NETCLODE_PLACEHOLDER_anthropic`)
5. Replaces with real secret from `secrets.json`
6. Forwards to destination

Secrets are **only injected into headers**, never request bodies (prevents reflection attacks).
