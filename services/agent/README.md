# Agent

Claude Code agent that runs inside Kata Container VMs. Uses the Claude Agent SDK to execute coding tasks.

## What it does

- Executes prompts via the SDK's `query()` async iterator
- Full access to Docker, root, any tools - VM handles isolation
- Persistent workspace survives pause/resume
- Terminal access via WebSocket

## Structure

```
services/agent/
├── src/
│   ├── index.ts        # HTTP server
│   ├── config.ts       # Configuration
│   ├── sdk/
│   │   ├── agent.ts    # Claude Agent SDK wrapper
│   │   └── tools.ts    # Tool configuration
│   ├── events/
│   │   └── emitter.ts  # Event streaming
│   └── ipc/
│       └── handler.ts
├── package.json
└── tsconfig.json
```

## Configuration

| Variable | Description |
|----------|-------------|
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `SESSION_ID` | Session ID |
| `GIT_REPO` | Optional repo to clone |

## API

HTTP on port `3002`.

### POST /prompt

Execute a prompt, stream results via SSE.

Request:
```json
{"sessionId": "abc123", "text": "Fix the bug in auth.ts"}
```

Response (SSE):
```
data: {"type":"tool_call","tool":"Read","path":"auth.ts"}
data: {"type":"assistant","content":"I found the issue..."}
```

### POST /interrupt

Interrupt current operation. Returns `{"ok": true}`.

### POST /generate-title

Generate session title from first prompt.

Request:
```json
{"prompt": "Build a REST API"}
```

Response:
```json
{"title": "REST API"}
```

### GET /health

Returns `ok`.

### WebSocket /terminal/ws

Interactive terminal.

Client → Server:
- `{"type": "input", "data": "ls\n"}`
- `{"type": "resize", "cols": 80, "rows": 24}`

Server → Client:
- `{"type": "output", "data": "..."}`

The PTY is managed by [node-pty](https://github.com/microsoft/node-pty). It's spawned lazily on first input (not on WebSocket connect) to avoid idle shell processes. The shell runs as root in `/agent/workspace`.

```
iOS/Web ──► Control Plane ──► Agent ──► node-pty ──► bash
              (proxy)         (WS)       (PTY)
```

The control plane maintains a WebSocket connection to the agent and bridges messages. Multiple clients can share the same terminal session.

## Claude Agent SDK

```typescript
import { query } from "@anthropic-ai/claude-agent-sdk";

const q = query({
  prompt: text,
  options: {
    cwd: workspaceDir,
    permissionMode: "bypassPermissions",
    model: "claude-opus-4-5-20251101",
    persistSession: true,
    systemPrompt: { type: "preset", preset: "claude_code", append: "..." },
    ...(sdkSessionId && { resume: sdkSessionId }),
  },
});

for await (const message of q) {
  // system, assistant (text, tool_use, thinking), user, result, stream_event
}
```

Available tools (all enabled via `bypassPermissions`): Read, Write, Edit, Bash, Glob, Grep, WebSearch, WebFetch.

## VM environment

```
/agent/                     # Home (JuiceFS PVC, persistent)
├── workspace/              # User's code (Claude's cwd)
├── docker/                 # Docker data
├── .local/share/mise/      # Installed tools
├── .cache/                 # Package caches
├── .claude/                # SDK session data
└── .session-mapping.json   # Session ID mapping

/opt/agent/                 # Agent code (read-only)
```

### Session ID mapping

The control plane assigns session IDs (`sess-abc123`). The Claude Agent SDK has its own session IDs for conversation persistence. These are different.

When you pause and resume a session, you get a new VM, but the JuiceFS PVC is the same. The agent needs to know which SDK session to resume.

`.session-mapping.json` maps control-plane session IDs to SDK session IDs:

```json
{
  "sess-abc123": "sdk-session-xyz789"
}
```

On first prompt, the agent stores the SDK session ID. On resume, it reads the mapping and passes `resume: sdkSessionId` to the SDK's `query()` call. Conversations survive pause/resume.

Tools persist via mise:

```bash
mise use node@22
mise use python@3.12
mise use go@latest
```

Docker is available:

```bash
docker run -v /agent/workspace:/app node:20 npm install
```

### Network isolation

Agents have internet access but are blocked from reaching cluster internals via NetworkPolicy:

- Can reach: internet (any external IP)
- Blocked: pod network (10.42.0.0/16), service network (10.43.0.0/16), node IPs

This prevents a compromised agent from attacking other pods, the k8s API, or Redis. The only allowed internal traffic is to the control plane (for session config and health checks).

### Port exposure (previews)

When a client sends `port.expose`, the control plane creates a Tailscale Service for the sandbox pod, giving it a MagicDNS hostname like `sandbox-abc123.tailnet-name.ts.net`.

The preview URL is then `http://sandbox-abc123.tailnet-name.ts.net:3000`. Accessible from any device on your tailnet.

## Development

```bash
npm install
npm run dev
npm run typecheck
```

## Docker image

```bash
docker build -t ghcr.io/angristan/netclode-agent:latest -f services/agent/Dockerfile .
```

Includes Debian bookworm-slim, Node.js via mise, Docker, Git, curl, build-essential, Claude CLI.

## SSE Event Types

Events streamed via Server-Sent Events during prompt execution:

| Type | Description |
|------|-------------|
| `start` | Prompt execution started |
| `agent.system` | SDK system message (init, session ID) |
| `agent.message` | Text content from Claude (`partial: true` for streaming) |
| `agent.event` | Tool/command events (see below) |
| `agent.result` | Final result with `numTurns` and `costUsd` |
| `done` | Prompt execution completed |
| `error` | Error occurred |

### Agent Events (`agent.event`)

| Event Kind | Description | Fields |
|------------|-------------|--------|
| `tool_start` | Tool invocation started | `tool`, `toolUseId`, `input` |
| `tool_input` | Streaming tool input | `toolUseId`, `inputDelta` |
| `tool_end` | Tool completed | `tool`, `toolUseId`, `result?`, `error?` |
| `thinking` | Extended thinking content | `thinkingId`, `content`, `partial` |

All events include a `timestamp` field (ISO 8601).

### Thinking Events

When using models that support extended thinking (e.g., `claude-opus-4-5-20251101`), the agent streams thinking content in real-time:

- `partial: true` - Streaming delta (accumulate by `thinkingId`)
- `partial: false` - Complete thinking block

Clients should accumulate content for events with the same `thinkingId` to build up the full thinking output.

## Debugging

```bash
# List pods
kubectl get pods -n netclode -l sandbox=true

# Logs
kubectl logs -n netclode <agent-pod> -f

# Exec
kubectl exec -it -n netclode <agent-pod> -- /bin/bash
```

Inside the VM:

```bash
ps aux | grep node
ls -la /agent/workspace
curl http://control-plane.netclode.svc.cluster.local:80/health
```
