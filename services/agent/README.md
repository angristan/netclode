# Agent

AI coding agent that runs inside Kata Container VMs. Supports multiple SDK backends (Claude Agent SDK, OpenCode SDK, Copilot SDK, Codex SDK) to execute coding tasks.

## What it does

- Executes prompts via the SDK's `query()` async iterator
- Full access to Docker, sudo, any tools - VM handles isolation
- Persistent workspace survives pause/resume
- Terminal access via bidirectional Connect streaming

## Structure

```
services/agent/
├── src/
│   ├── index.ts           # Entry point
│   ├── connect-client.ts  # Bidirectional Connect client to control plane
│   ├── git.ts             # Git operations
│   ├── sdk/               # SDK abstraction layer
│   │   ├── index.ts       # Public exports
│   │   ├── types.ts       # SDKAdapter interface, event types
│   │   ├── factory.ts     # Creates appropriate adapter based on config
│   │   ├── utils/         # Shared utilities
│   │   │   ├── index.ts   # Tool name normalization, ID generators
│   │   │   └── index.test.ts
│   │   ├── claude/        # Claude Agent SDK
│   │   │   ├── adapter.ts
│   │   │   ├── translator.ts      # Event translation (pure functions)
│   │   │   └── translator.test.ts
│   │   ├── opencode/      # OpenCode SDK
│   │   │   ├── adapter.ts
│   │   │   ├── translator.ts
│   │   │   └── translator.test.ts
│   │   ├── copilot/       # GitHub Copilot SDK
│   │   │   ├── adapter.ts
│   │   │   ├── translator.ts
│   │   │   └── translator.test.ts
│   │   └── codex/         # OpenAI Codex SDK
│   │       ├── adapter.ts
│   │       ├── translator.ts
│   │       └── translator.test.ts
│   └── services/
│       ├── terminal.ts    # PTY management
│       └── title.ts       # Title generation
├── gen/                   # Generated protobuf types
├── package.json
└── tsconfig.json
```

## Configuration

### Environment Variables

| Variable | Description |
|----------|-------------|
| `ANTHROPIC_API_KEY` | Anthropic API key |
| `OPENAI_API_KEY` | OpenAI API key (required for Codex SDK) |
| `GITHUB_TOKEN` | GitHub token with Copilot scope (required for Copilot SDK) |
| `CONTROL_PLANE_URL` | Sandbox-only AgentService URL (default `http://control-plane-agent.netclode.svc.cluster.local:3001`) |
| `SESSION_ID` | Session ID (direct mode only; warm pool mode receives via gRPC) |

### Session Config

The control plane passes `SessionConfig` to the agent on registration. Key fields:

| Field | Description |
|-------|-------------|
| `session_id` | Unique session identifier |
| `workspace_dir` | Absolute path to workspace |
| `sdk_type` | SDK backend (Claude, OpenCode, Copilot, Codex) |
| `model` | Model ID (e.g., `claude-sonnet-4-0`, `gpt-4o`) |
| `reasoning_effort` | Reasoning effort level for Codex SDK: `minimal`, `low`, `medium` (default), `high`, `xhigh` |
| `github_token` | GitHub token for git operations |
| `repos` | Repositories to clone (e.g., `owner/repo`) |
| `repo_access` | Permission level (`read` or `write`) |

## API

The agent connects TO the control plane (not the other way around) using a single bidirectional Connect stream. See `proto/netclode/v1/agent.proto` for full definitions.

```protobuf
service AgentService {
  rpc Connect(stream AgentMessage) returns (stream ControlPlaneMessage);
}
```

### Connection Flow

1. Agent starts and connects to control plane
2. Agent sends `AgentRegister` with session ID
3. Control plane responds with `AgentRegistered` (includes session config)
4. Bidirectional communication begins

### Agent → Control Plane Messages

| Message | Description |
|---------|-------------|
| `register` | Initial registration with session ID |
| `prompt_response` | Streaming response (text, events, result, error) |
| `terminal_output` | PTY output data |
| `title_response` | Generated session title |
| `git_status_response` | Git status result |
| `git_diff_response` | Git diff result |

### Control Plane → Agent Messages

| Message | Description |
|---------|-------------|
| `registered` | Registration acknowledgment with session config |
| `execute_prompt` | Execute a prompt |
| `interrupt` | Stop current execution |
| `generate_title` | Generate session title |
| `get_git_status` | Request git status |
| `get_git_diff` | Request git diff |
| `terminal_input` | Terminal input (data or resize) |

### Terminal

The PTY is managed by [node-pty](https://github.com/microsoft/node-pty). It's spawned lazily on first input to avoid idle shell processes. The shell runs as the `agent` user (with passwordless sudo) in `/agent/workspace`.

```
iOS ──► Control Plane ──► Agent ──► node-pty ──► bash
        (Connect)         (Connect)    (PTY)
```

Terminal input/output flows through the same bidirectional stream as prompts. Multiple clients can share the same terminal session via the control plane.

### Health

Available at `GET /health` for k8s probes.

## SDK Adapters

The agent supports multiple AI SDK backends. Users select which SDK to use when creating a session.

### How SDK routing works

When the agent registers with the control plane, it receives a `SessionConfig` containing `sdk_type`. The agent uses a factory pattern to instantiate the correct adapter:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              connect-client.ts                               │
│                                                                              │
│  1. Agent connects to control plane                                          │
│  2. Receives SessionConfig with sdk_type, model, credentials                 │
│  3. Calls createSDKAdapter(config)                                           │
│  4. On executePrompt: iterates adapter.executePrompt() → streams events      │
└─────────────────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              sdk/factory.ts                                  │
│                                                                              │
│  switch (config.sdkType) {                                                   │
│    case "opencode": adapter = new OpenCodeAdapter(); break;                  │
│    case "copilot":  adapter = new CopilotAdapter();  break;                  │
│    case "codex":    adapter = new CodexAdapter();    break;                  │
│    case "claude":                                                            │
│    default:         adapter = new ClaudeSDKAdapter(); break;                 │
│  }                                                                           │
│  await adapter.initialize(config);                                           │
└─────────────────────────────────────────────────────────────────────────────┘
                                    │
                    ┌───────────────┼───────────────┬───────────────┐
                    ▼               ▼               ▼               ▼
             ┌───────────┐   ┌───────────┐   ┌───────────┐   ┌───────────┐
             │  claude/  │   │ opencode/ │   │ copilot/  │   │  codex/   │
             │ adapter.ts│   │ adapter.ts│   │ adapter.ts│   │ adapter.ts│
             └─────┬─────┘   └─────┬─────┘   └─────┬─────┘   └─────┬─────┘
                   │               │               │               │
                   ▼               ▼               ▼               ▼
             ┌───────────┐   ┌───────────┐   ┌───────────┐   ┌───────────┐
             │translator │   │translator │   │translator │   │translator │
             │    .ts    │   │    .ts    │   │    .ts    │   │    .ts    │
             └─────┬─────┘   └─────┬─────┘   └─────┬─────┘   └─────┬─────┘
                   │               │               │               │
                   ▼               ▼               ▼               ▼
             ┌───────────┐   ┌───────────┐   ┌───────────┐   ┌───────────┐
             │ @anthropic│   │  opencode │   │  @github/ │   │  @openai/ │
             │ /claude-  │   │   serve   │   │  copilot- │   │  codex-   │
             │ agent-sdk │   │ REST+SSE  │   │    sdk    │   │    sdk    │
             └───────────┘   └───────────┘   └───────────┘   └───────────┘
```

All adapters implement the `SDKAdapter` interface (`sdk/types.ts`):

```typescript
interface SDKAdapter {
  initialize(config: SDKConfig): Promise<void>;
  executePrompt(sessionId: string, text: string, config?: PromptConfig): AsyncGenerator<PromptEvent>;
  setInterruptSignal(): void;
  clearInterruptSignal(): void;
  isInterrupted(): boolean;
  shutdown(): Promise<void>;
}
```

Each adapter uses a **translator module** (pure functions) to convert SDK-native events to a unified `PromptEvent` type. This separation enables unit testing of translation logic without SDK dependencies.

Event types:
- `textDelta` – Streaming text content
- `toolStart` / `toolEnd` – Tool invocations with inputs and results
- `toolInput` / `toolInputComplete` – Streaming tool input
- `thinking` – Extended thinking/reasoning content
- `repoClone` – Repository clone progress
- `result` – Token usage and turn counts
- `error` – Errors with retry hints

### Claude Agent SDK (default)

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

### OpenCode SDK

Uses the OpenCode CLI in server mode. The agent spawns `opencode serve` and communicates via REST API + SSE.

```typescript
// Start server
const process = spawn("opencode", ["serve", "--port", port]);

// Create session
const res = await fetch(`http://localhost:${port}/session`, {
  method: "POST",
  body: JSON.stringify({ path: workspaceDir }),
});

// Send message and stream response via SSE
const eventSource = new EventSource(`http://localhost:${port}/session/${id}/message`);
```

OpenCode supports multiple model providers (Anthropic, OpenAI, Ollama, etc.). The model is specified in the session config (e.g., `anthropic/claude-sonnet-4-0`, `ollama/qwen2.5-coder:32b`).

#### Ollama Support (Local GPU Inference)

When the model ID starts with `ollama/`, the adapter configures OpenCode to use the local Ollama server:

```typescript
// Session config from control-plane includes:
// - model: "ollama/qwen2.5-coder:32b"
// - ollamaUrl: "http://ollama.netclode.svc.cluster.local:11434"

const opencodeConfig = {
  model: "ollama/qwen2.5-coder:32b",
  provider: {
    ollama: {
      baseURL: "http://ollama.netclode.svc.cluster.local:11434"
    }
  }
};
```

Tool calling behavior varies between models - local models may not execute tool calls as reliably as larger models.

### Copilot SDK

Uses the GitHub Copilot CLI via `@github/copilot-sdk`. The agent creates a `CopilotClient` that manages the CLI process and communicates via JSON-RPC.

```typescript
import { CopilotClient } from "@github/copilot-sdk";

const client = new CopilotClient({ cwd: workspaceDir });
const session = await client.createSession({
  model: "claude-sonnet-4-20250514",
  streaming: true,
  onPermissionRequest: async () => ({ kind: "approved" }),
});

session.on((event) => {
  // assistant.message_delta, tool.execution_start, session.idle, etc.
});

await session.send({ prompt: text });
```

**Authentication:** Requires a GitHub token with the `copilot` scope. Create a fine-grained PAT at https://github.com/settings/tokens?type=beta with Account permissions > Copilot > Read-only. Pass it as `GITHUB_TOKEN` environment variable.

### Codex SDK

Uses the OpenAI Codex SDK (`@openai/codex-sdk`). The agent creates a `Codex` client and manages threads for conversation persistence.

```typescript
import { Codex } from "@openai/codex-sdk";

const codex = new Codex();
const thread = codex.startThread({
  workingDirectory: "/agent/workspace",
  sandboxMode: "danger-full-access",
  approvalPolicy: "never",
  model: "codex-mini-latest",
});

const { events } = await thread.runStreamed(prompt);
for await (const event of events) {
  // thread.started, item.started, item.completed, turn.completed, etc.
}
```

**Event types:**
- `thread.started` - Thread created, provides thread ID for resumption
- `item.started` / `item.completed` - Tool executions, file changes, reasoning
- `turn.completed` - Turn finished with usage stats

**Item types:**
- `command_execution` - Shell commands
- `file_change` - File modifications
- `mcp_tool_call` - MCP server tool calls
- `agent_message` - Final text response
- `reasoning` - Model reasoning/thinking

**Authentication:** Requires `OPENAI_API_KEY` environment variable. Alternatively, OAuth tokens can be passed for ChatGPT subscription authentication.

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

The control plane assigns session IDs (`sess-abc123`). Both the Claude Agent SDK and OpenCode SDK have their own session IDs for conversation persistence. These are different.

When you pause and resume a session, you get a new VM, but the JuiceFS PVC is the same. The agent needs to know which SDK session to resume.

`.session-mapping.json` maps control-plane session IDs to SDK session IDs:

```json
{
  "sess-abc123": "sdk-session-xyz789"
}
```

On first prompt, the agent stores the SDK session ID. On resume, it reads the mapping and resumes the conversation. This works for both Claude and OpenCode SDKs.

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

Agents have internet access but are blocked from reaching internal networks via NetworkPolicy:

- Can reach: internet (any external IP)
- Blocked:
  - Pod network (10.42.0.0/16), service network (10.43.0.0/16), node IPs
  - Private ranges (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16)
  - Tailscale CGNAT range (100.64.0.0/10) - sandboxes cannot access other devices on your tailnet

This prevents a compromised agent from attacking other pods, the k8s API, Redis, or pivoting to other resources on your tailnet. The only allowed internal traffic is to the control plane (for session config and health checks).

### Port exposure (previews)

When a client sends `port.expose`, the control plane creates a Tailscale Service for the sandbox pod, giving it a MagicDNS hostname like `sandbox-abc123.tailnet-name.ts.net`.

The preview URL is then `http://sandbox-abc123.tailnet-name.ts.net:3000`. Accessible from any device on your tailnet.

Ports can be removed via `port.unexpose`, which deletes the port from the Tailscale Service and NetworkPolicy.

## Development

```bash
npm install
npm run dev
npm run typecheck
npm run test
npm run build
```

## Docker image

```bash
docker build -t ghcr.io/angristan/netclode-agent:latest -f services/agent/Dockerfile .
```

Includes Debian trixie-slim, Node.js via mise, Docker, Git, gh (GitHub CLI), curl, build-essential, sudo, Claude CLI, Copilot CLI, Codex CLI.

## Agent Events

Events streamed during prompt execution:

| Event Kind | Description | Fields |
|------------|-------------|--------|
| `tool_start` | Tool invocation started | `tool`, `toolUseId`, `input` |
| `tool_input` | Streaming tool input | `toolUseId`, `inputDelta` |
| `tool_input_complete` | Tool input finished | `toolUseId`, `input` |
| `tool_end` | Tool completed | `tool`, `toolUseId`, `result?`, `error?` |
| `thinking` | Extended thinking content | `thinkingId`, `content`, `partial` |
| `file_change` | File created/edited/deleted | `path`, `action`, `linesAdded?`, `linesRemoved?` |
| `command_start` | Shell command started | `command`, `cwd` |
| `command_end` | Shell command completed | `command`, `exitCode`, `output` |
| `port_exposed` | Port exposed for preview | `port`, `process?`, `previewUrl?` |
| `port_unexposed` | Port exposure removed | `port` |
| `repo_clone` | Repository clone progress | `repo`, `stage`, `message` |

All events include a `timestamp` field.

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
curl http://control-plane-agent.netclode.svc.cluster.local:3001/health
```
