# Control Plane

Go service that orchestrates Netclode. Manages sessions, proxies communication between clients and agents, persists state to Redis.

## What it does

- Session lifecycle (create, pause, resume, delete)
- Creates Sandbox CRDs, monitors readiness via k8s informers
- Bridges Connect clients to Connect agents
- Stores sessions, messages, and events in Redis
- Real-time sync across clients via Redis Streams

## Architecture

```
services/control-plane/
├── cmd/control-plane/     # Entry point
└── internal/
    ├── api/               # Connect protocol server
    ├── session/           # Session manager
    ├── k8s/               # Kubernetes client (Sandbox CRDs)
    ├── storage/           # Redis persistence
    ├── protocol/          # Message types
    └── config/            # Configuration
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CLIENT_PORT` | `3000` | Tailscale-only ClientService listener |
| `AGENT_PORT` | `3001` | Sandbox AgentService listener |
| `INTERNAL_PORT` | `3002` | Secret-proxy validation listener |
| `BOT_PORT` | `3003` | Authenticated GitHub bot listener |
| `K8S_NAMESPACE` | `netclode` | Kubernetes namespace |
| `AGENT_IMAGE` | `ghcr.io/angristan/netclode-agent:latest` | Agent image |
| `SANDBOX_TEMPLATE` | `netclode-agent` | SandboxTemplate name |
| `REDIS_URL` | `redis://redis-sessions...` | Redis URL |
| `WARM_POOL_ENABLED` | `true` | Use warm pool |
| `MAX_ACTIVE_SESSIONS` | `5` | Max concurrent sessions |
| `IDLE_TIMEOUT_MINUTES` | `0` (disabled) | Auto-pause sessions after N minutes of inactivity |
| `HOST_CPUS` | `16` | Total host CPUs (for 50% limit validation) |
| `HOST_MEMORY_MB` | `32768` | Total host memory in MB (for 50% limit) |
| `DEFAULT_CPUS` | `4` | Default vCPUs per session |
| `DEFAULT_MEMORY_MB` | `4096` | Default memory per session |

## Connect API

Human clients connect to port `3000` through Tailscale Ingress. Sandboxes can reach only
the AgentService listener on port `3001`; internal workloads use projected Kubernetes identity.
See `proto/netclode/v1/client.proto` for the client protocol.

### Bidirectional streaming

The main client API is a single bidirectional stream:

```protobuf
rpc Connect(stream ClientMessage) returns (stream ServerMessage);
```

### Client → Server messages

| Message Type | Fields | Description |
|--------------|--------|-------------|
| `create_session` | `name`, `repos?`, `repo_access?`, `network_config?` | Create session ([network options](../../docs/network-access.md)) |
| `list_sessions` | | List sessions |
| `open_session` | `session_id`, `last_stream_id?` | Open with history |
| `resume_session` | `session_id` | Resume paused |
| `pause_session` | `session_id` | Pause |
| `delete_session` | `session_id` | Delete |
| `delete_all_sessions` | | Delete all |
| `send_prompt` | `session_id`, `text` | Send prompt |
| `interrupt_prompt` | `session_id` | Interrupt |
| `expose_port` | `session_id`, `port` | Expose port |
| `terminal_input` | `session_id`, `data` | Terminal input |
| `terminal_resize` | `session_id`, `cols`, `rows` | Resize terminal |
| `sync` | | Get all sessions with metadata |
| `git_status` | `session_id` | Get git status |
| `git_diff` | `session_id`, `file?` | Get git diff |
| `list_github_repos` | | List available GitHub repos |
| `list_snapshots` | `session_id` | List session snapshots |
| `restore_snapshot` | `session_id`, `snapshot_id` | Restore to snapshot |
| `list_models` | `sdk_type`, `copilot_backend?` | List available models for an SDK |
| `get_copilot_status` | | Get Copilot auth status and premium quota |
| `update_repo_access` | `session_id`, `repo_access` | Change repository permission level |

### Server → Client messages

| Message Type | Description |
|--------------|-------------|
| `session_created` | Session created |
| `session_updated` | Status changed |
| `session_deleted` | Deleted |
| `sessions_deleted_all` | All deleted |
| `session_list` | List of sessions |
| `session_state` | Session with history |
| `session_error` | Operation failed |
| `sync_response` | All sessions with metadata |
| `agent_message` | Text from agent (`partial` for streaming) |
| `agent_event` | Tool event |
| `agent_done` | Finished |
| `agent_error` | Error |
| `user_message` | User prompt (cross-client sync) |
| `port_exposed` | Port exposed with `preview_url` |
| `port_unexposed` | Port exposure removed |
| `port_error` | Port exposure failed |
| `terminal_output` | Terminal output |
| `github_repos` | Available GitHub repos |
| `git_status` | Git status |
| `git_diff` | Git diff |
| `git_error` | Git operation failed |
| `snapshot_created` | Auto-snapshot created after turn |
| `snapshot_list` | List of session snapshots |
| `snapshot_restored` | Snapshot restored with message count |
| `models` | List of available models for an SDK |
| `copilot_status` | Copilot auth status and premium quota |
| `repo_access_updated` | Repository access level changed |
| `error` | Generic error |

### Agent events

Delivered via `agent_event`:

| Kind | Description |
|------|-------------|
| `tool_start` | Tool started (includes `input` if available) |
| `tool_input` | Input delta (streaming) |
| `tool_input_complete` | Tool input finished (full `input` object) |
| `tool_end` | Tool completed |
| `file_change` | File created/edited/deleted |
| `command_start` | Shell command started |
| `command_end` | Shell command completed |
| `thinking` | Agent reasoning |
| `port_exposed` | Port exposed |
| `port_unexposed` | Port exposure removed |
| `repo_clone` | Repository clone progress |

## HTTP

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Health check |

## Redis

| Key | Type | Description |
|-----|------|-------------|
| `sessions:all` | Set | All session IDs |
| `session:{id}` | Hash | Session metadata (name, status, timestamps, messageCount) |
| `session:{id}:stream` | Stream | Unified stream for all session data |
| `session:{id}:snapshots` | Sorted Set | Snapshot IDs scored by timestamp |
| `session:{id}:snapshot:{snapId}` | Hash | Snapshot metadata |

### Unified Stream Model

All session data flows through a single Redis Stream per session (`session:{id}:stream`). Each entry has a type field:

| Type | Description |
|------|-------------|
| `event` | Agent events (messages, tools, thinking, file changes, etc.) |
| `terminal_output` | Terminal data |
| `session_update` | Session status changes |
| `error` | Error notifications |

Each entry also has:
- `partial`: true for streaming deltas, false for final/complete data
- `timestamp`: ISO8601 timestamp (used for ordering on reload)
- `payload`: Type-specific JSON data

### Why a Unified Stream

Previously we had separate stores (messages list, events stream, notifications stream). This caused ordering issues on reload - messages and events had different timestamps and would interleave incorrectly.

The unified stream ensures:
1. **Correct ordering**: All data is in a single timeline with consistent timestamps
2. **Atomic snapshots**: Truncating to a snapshot point is a single stream operation
3. **Simpler sync**: Clients track one cursor instead of multiple

### Stream Cursors

Redis Streams provide cursor-based reading. Each entry has an ID (e.g., `1234567890123-0`). When a client opens a session:

1. Server returns current state + `lastStreamId` (the latest stream ID)
2. Client stores this cursor
3. Server starts a blocking read with `XREAD BLOCK 0 STREAMS session:{id}:stream {cursor}`
4. New entries get pushed to the client as they arrive

On reconnect, the client sends its stored `lastStreamId`. The server resumes from that position. Entries that happened while disconnected are delivered immediately.

```
Client A connects
    │
    ▼
Server: XREAD BLOCK ... $  ($ = only new entries)
    │
    ├──── Entry 1 arrives ──► Client A receives
    ├──── Entry 2 arrives ──► Client A receives
    │
Client A disconnects (cursor = "1234567890123-1")
    │
    ├──── Entry 3 arrives ──► stored in stream
    ├──── Entry 4 arrives ──► stored in stream
    │
Client A reconnects with cursor "1234567890123-1"
    │
    ▼
Server: XREAD BLOCK ... 1234567890123-1
    │
    ├──── Entry 3 delivered immediately
    ├──── Entry 4 delivered immediately
    └──── Resume blocking for new entries
```

Multi-client sync works the same way. Multiple clients on the same session all get entries through separate XREAD consumers on the same stream.

## Data flow

```
┌─────────┐      Connect       ┌───────────────┐      Connect       ┌─────────┐
│ Client  │◄──────────────────►│ Control Plane │◄──────────────────►│  Agent  │
└─────────┘                    └───────┬───────┘                    └─────────┘
                                       │
                                       ▼
                               ┌───────────────┐
                               │     Redis     │
                               └───────────────┘
```

1. Client sends prompt via Connect stream
2. Control plane persists to Redis, publishes to notifications stream
3. Control plane forwards to agent via Connect
4. Agent streams responses back
5. Control plane persists and publishes to Redis Stream
6. All clients read via XREAD BLOCK

## Terminal proxy

The control plane proxies terminal I/O between clients and the agent's PTY:

```
Client                Control Plane              Agent
  │                        │                       │
  │ terminal_input ───────►│                       │
  │                        │ Connect stream ──────►│
  │                        │                       │ node-pty
  │                        │◄─── Connect stream ───│
  │◄─── terminal_output ───│                       │
```

The control plane maintains one Connect bidirectional stream per active session to the agent's Terminal RPC. Multiple clients can connect to the same session and share the same PTY.

Terminal data is ephemeral (not persisted to Redis).

## Preview URLs

When a client sends `expose_port`, the control plane:

1. Creates a Tailscale Service for the sandbox pod (if not already created)
2. Waits for Tailscale to assign a MagicDNS hostname
3. Returns `port_exposed` with the preview URL

```
sandbox-{sessionId}.tailnet-name.ts.net:{port}
```

The sandbox pod gets its own Tailscale identity, so preview URLs are accessible from any device on your tailnet. Each sandbox can expose multiple ports on the same hostname.

When a client sends `unexpose_port`, the control plane removes the port from the Tailscale Service and the sandbox NetworkPolicy ingress rules, then returns `port_unexposed`. Exposed port state is persisted as events, so pause/resume correctly restores only ports that haven't been unexposed.

## Session lifecycle

```
create → creating → running ↔ paused
              │         │        │
              └─────────┴────────┼──→ deleted
                   error ←───────┘
```

### Auto-pause

Sessions are auto-paused in two scenarios:

1. **Capacity limit**: When `MAX_ACTIVE_SESSIONS` is reached and a new session needs to run, the oldest session (by `LastActiveAt`) is paused to make room.
2. **Idle timeout**: When `IDLE_TIMEOUT_MINUTES` is set (default `0` = disabled), a background reaper checks every minute and pauses any active session that has been idle longer than the configured duration.

Many paused sessions (cheap, just S3 storage), limited concurrent VMs (expensive, memory/CPU).

### Warm pool

When `WARM_POOL_ENABLED=true`, the control plane creates `SandboxClaim` resources instead of `Sandbox` resources directly. The agent-sandbox-controller assigns a pre-booted VM from the warm pool.

Pre-booted VMs are already running and have their JuiceFS PVC mounted, so session start is nearly instant (~1s vs ~30s cold start).

Since warm pool pods are created before we know which session they'll serve, they can't receive per-session env vars at boot. Instead, the agent receives its configuration via the gRPC bidirectional stream (`SessionAssigned` message) after the control plane binds a session to the pod.

## Development

```bash
go run ./cmd/control-plane
go test ./...
go build -o control-plane ./cmd/control-plane
```

## Docker

```bash
docker build -t control-plane -f Dockerfile .
```
