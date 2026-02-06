# Netclode iOS

Native iOS 26 app for Netclode. Built with SwiftUI and the Liquid Glass API.

## Features

- Session management (create, pause, resume, delete)
- Real-time chat with streaming responses
- **Voice input** via SpeechAnalyzer API (iOS 26+) with real-time waveform
- Session history with rollback (restore workspace and chat to any previous turn)
- Terminal emulator via [SwiftTerm](https://github.com/migueldeicaza/SwiftTerm)
- Git changes view with inline unified diffs
- Connects over Tailscale
- Platform-adaptive navigation (sidebar on iPad/Mac, stack on iPhone)
- Connection resilience (WiFi/cellular transitions, background/foreground, offline queueing)

## Requirements

- iOS 26.2+ / macOS
- Xcode 17.0+
- Swift 6.2+

## Building

From repo root:

```bash
# macOS (Catalyst)
make run-macos

# iOS Simulator (default: iPhone 16 Pro)
make run-ios

# iOS Simulator with specific device
make run-ios SIMULATOR="iPhone 16"

# Physical iPhone (requires signing)
make run-device
```

### Signing setup (required for CLI builds)

`xcodebuild` needs an Apple Developer account and a valid development signing certificate in your keychain.

1. Open Xcode → Settings → Accounts, add your Apple ID, and select a team.
2. In that team, click **Manage Certificates...** and create/download a development certificate.
3. Verify certificates are visible to the CLI:

```bash
security find-identity -v -p codesigning
```

If you are not using the default project team, pass your team explicitly:

```bash
make run-macos TEAM_ID=<YOUR_TEAM_ID>
make run-ios TEAM_ID=<YOUR_TEAM_ID>
make run-device TEAM_ID=<YOUR_TEAM_ID>
```

`make` now auto-detects `TEAM_ID` from your local Apple Development certificate (or falls back to your first local provisioning profile) if you do not pass `TEAM_ID`.

Inspect the detected value:

```bash
make print-ios-team-id
```

## Testing

Run unit tests from Xcode (`⌘U`) or via command line:

```bash
# From repo root
make test-ios

# Or directly
cd clients/ios
xcodebuild test -scheme NetclodeTests -destination 'platform=macOS'
```

Tests cover:
- `EventStore.loadEvents()` - aggregates thinking events by `thinkingId`, merges `tool_input_complete` into `tool_start`

## Usage

1. Open the app
2. Settings → enter your server URL: `https://netclode-control-plane-ingress.YOUR-TAILNET.ts.net`
3. The app will connect automatically
4. Tap + to create a session

### Server URL

The iOS app requires **HTTPS** to enable HTTP/2, which is needed for bidirectional streaming.

The control plane is exposed via Tailscale Ingress with automatic Let's Encrypt certificates.
Your server URL will be: `https://netclode-control-plane-ingress.YOUR-TAILNET.ts.net`

To find your tailnet name, check the [Tailscale admin console](https://login.tailscale.com/admin/machines) or run `tailscale status`.

For local development with HTTP (no streaming), use: `http://localhost:3001`

## Architecture

```
Netclode/
├── App/                    # Entry point
├── Models/                 # Session, Messages, Events, ChatMessage
├── Services/
│   ├── ConnectService      # gRPC/Connect bidirectional stream
│   ├── MessageRouter       # Routes server messages to stores
│   ├── NetworkMonitor      # NWPathMonitor wrapper
│   ├── AppStateCoordinator # Lifecycle + network orchestration
│   ├── MessageQueue        # Offline message persistence
│   ├── SessionCache        # Fast startup cache
│   └── ConnectionStateManager # Cursor persistence
├── Stores/                 # @Observable state (Session, Chat, Event, Terminal, Settings)
├── Features/
│   ├── Sessions/           # Session list, sidebar, creation
│   ├── Workspace/          # Chat + Terminal tabs
│   ├── Chat/               # Chat UI
│   ├── Terminal/           # SwiftTerm wrapper
│   └── Settings/           # Server config
├── Components/
│   ├── Connection/         # ConnectionBanner (status + pending messages)
│   └── ...                 # GlassCard, GlassButton, GlassTextField
├── Design/                 # Theme, colors
├── Generated/              # Protobuf generated code
└── Extensions/
```

## Connect protocol

The app communicates with the control plane via Connect protocol (gRPC-compatible) using bidirectional streaming.

### HTTP Client

The app uses `NIOHTTPClient` (from [ConnectNIO](https://github.com/connectrpc/connect-swift)) instead of `URLSessionHTTPClient` for HTTP/2 connections.

**Why?** URLSession's HTTP/2 implementation has compatibility issues with Tailscale's iOS network extension. On physical iPhones, bidirectional streams would drop after ~10-15 seconds. Tailscale also [disables TCP keep-alives on iOS](https://github.com/tailscale/tailscale/blob/main/net/netknob/netknob.go) to save battery, which exacerbates the issue.

`NIOHTTPClient` uses Swift NIO's HTTP/2 implementation with POSIX sockets, bypassing URLSession entirely. This provides stable long-lived connections through Tailscale.

Client → Server:

```swift
// Messages sent via ConnectService
createSession(name: "My Project", repo: "owner/repo", repoAccess: .write, initialPrompt: nil)
openSession(id: "xxx", lastNotificationId: nil)
resumeSession(id: "xxx")
pauseSession(id: "xxx")
sendPrompt(sessionId: "xxx", text: "Fix the bug")
terminalInput(sessionId: "xxx", data: "ls\n")
```

Server → Client:

```swift
// Messages received and routed by MessageRouter
sessionList(sessions: [...])
sessionCreated(session: Session)
agentMessage(sessionId: "xxx", content: "...", partial: true)
agentEvent(sessionId: "xxx", event: AgentEvent)
terminalOutput(sessionId: "xxx", data: "...")
```

On reconnect, the app sends `lastNotificationId` to resume from where it left off.

## Connection Resilience

The app handles network transitions and app lifecycle gracefully:

| Scenario | Behavior |
|----------|----------|
| WiFi ↔ Cellular | Proactive reconnection with 0.5s stabilization delay |
| Network lost | Clean disconnect, automatic reconnect when restored |
| App backgrounded | Stream suspended, cursors persisted |
| App foregrounded | Immediate reconnection, pending messages replayed |
| Offline message | Queued locally, replayed on reconnect (max 3 retries) |

### Services

- **`NetworkMonitor`** - Wraps `NWPathMonitor`, publishes `AsyncStream<NetworkTransition>` for WiFi/cellular/disconnected state changes
- **`AppStateCoordinator`** - Orchestrates lifecycle, network, and connection state; manages background tasks via `BGTaskScheduler`
- **`MessageQueue`** - Persistent offline queue with file-based storage in Documents directory
- **`SessionCache`** - UserDefaults-based cache for fast startup (5-minute staleness threshold)
- **`ConnectionStateManager`** - Persists Redis Stream cursors across app launches

### Reconnection Strategy

Exponential backoff with jitter:
- Base delay: 1s, max: 32s
- Jitter: ±30%
- Foreground multiplier: 0.5x (faster reconnection when app is active)
- Max attempts: 10

## State management

Uses `@Observable` + SwiftUI Environment:

```swift
@Observable
class SessionStore {
    var sessions: [Session] = []
    var currentSessionId: String?
}

@Environment(SessionStore.self) private var sessionStore
```

## Platform-Adaptive Navigation

The app uses `NavigationSplitView` which adapts to different screen sizes:

| Platform | Navigation Style |
|----------|-----------------|
| iPhone | Stack navigation (push/pop) |
| iPad | Sidebar + detail split view |
| Mac (Catalyst) | Persistent sidebar + detail |

On iPhone, tapping a session pushes the workspace view onto the stack. On iPad and Mac, the sidebar remains visible while the detail area shows the selected session's workspace.

```swift
// ContentView.swift
if horizontalSizeClass == .compact {
    NavigationStack { SessionsView() }
} else {
    NavigationSplitView {
        SidebarView(selectedSessionId: $selectedSessionId)
    } detail: {
        WorkspaceView(sessionId: selectedSessionId)
    }
}
```

## Liquid Glass

The app uses iOS 26's glass effects:

```swift
.glassEffect(.regular, in: RoundedRectangle(cornerRadius: 16))
.glassEffect(.regular.interactive().tint(color), in: .capsule)
```

## Terminal

Terminal emulation uses [SwiftTerm](https://github.com/migueldeicaza/SwiftTerm). The app sends terminal input messages to the control plane, which proxies them to the agent's PTY. Output comes back via terminal output messages.

```
SwiftTerminalView ──► ConnectService ──► Control Plane ──► Agent PTY
```

`SwiftTermBridge.swift` adapts SwiftTerm's `LocalProcessTerminalView` delegate to work over the Connect stream instead of a local process.

## Voice Input

Uses Apple's `SpeechAnalyzer` API (iOS 26+). Same engine as Notes, Voice Memos, and Journal.

The ML model downloads per-locale on first use via `AssetInventory`. Runs entirely on-device, outside app memory space. Designed for long-form and distant audio (meetings, lectures), not just close-mic dictation.

Audio flows through `AsyncStream<AnalyzerInput>` to `SpeechAnalyzer`, which routes to `SpeechTranscriber`. Results come back via another `AsyncStream`. Input and output are decoupled so we can capture audio and handle results independently.

Results are either "volatile" (immediate rough guesses) or "finalized" (accurate, after more context).

## License

MIT
