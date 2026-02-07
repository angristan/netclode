import Foundation

/// Model information for Copilot SDK
struct CopilotModel: Identifiable, Hashable, Sendable {
    let id: String
    let name: String
    let provider: String?
    let capabilities: [String]
    let reasoningEffort: String?
}

/// Copilot authentication status
struct CopilotAuthStatus: Sendable {
    let isAuthenticated: Bool
    let authType: String?
    let login: String?
}

/// Copilot premium request quota
struct CopilotPremiumQuota: Sendable {
    let used: Int
    let limit: Int
    let remaining: Int
    let resetAt: String?

    var usagePercentage: Double {
        guard limit > 0 else { return 0 }
        return Double(used) / Double(limit)
    }

    var displayString: String {
        "\(remaining) of \(limit) remaining"
    }
}

/// Copilot status response
struct CopilotStatus: Sendable {
    let auth: CopilotAuthStatus
    let quota: CopilotPremiumQuota?
}

/// Sandbox resource limits for session creation
struct ResourceLimits: Sendable, Equatable {
    /// Maximum vCPUs allowed per session
    let maxVcpus: Int32
    /// Maximum memory in MiB allowed per session
    let maxMemoryMB: Int32
    /// Default vCPUs (warm pool configuration)
    let defaultVcpus: Int32
    /// Default memory in MiB (warm pool configuration)
    let defaultMemoryMB: Int32
}

enum ServerMessage: Sendable {
    case sessionCreated(session: Session)
    case sessionUpdated(session: Session)
    case sessionDeleted(id: String)
    case sessionsDeletedAll(deletedIds: [String])
    case sessionList(sessions: [Session])
    case sessionError(id: String?, error: String)

    case agentMessage(sessionId: String, content: String, partial: Bool, messageId: String?)
    case agentEvent(sessionId: String, event: AgentEvent)
    case agentDone(sessionId: String)
    case agentError(sessionId: String, error: String)
    case userMessage(sessionId: String, content: String)

    case terminalOutput(sessionId: String, data: String)

    case portExposed(sessionId: String, port: Int, previewUrl: String)
    case portUnexposed(sessionId: String, port: Int)
    case portError(sessionId: String, port: Int, error: String)

    case error(message: String)

    // Sync responses
    case syncResponse(sessions: [SessionWithMeta], serverTime: Date)
    case sessionState(session: Session, messages: [PersistedMessage], events: [PersistedEvent], hasMore: Bool, lastNotificationId: String?)

    // GitHub
    case githubRepos(repos: [GitHubRepo])

    // Git operations
    case gitStatusResponse(sessionId: String, files: [GitFileChange])
    case gitDiffResponse(sessionId: String, diff: String)
    case gitError(sessionId: String, error: String)

    // Models (for Copilot, Codex, etc.)
    case modelsResponse(models: [CopilotModel], sdkType: SdkType?)
    case copilotStatusResponse(status: CopilotStatus)

    // Snapshots
    case snapshotCreated(sessionId: String, snapshot: Snapshot)
    case snapshotList(sessionId: String, snapshots: [Snapshot])
    case snapshotRestored(sessionId: String, snapshotId: String, messageCount: Int)

    // Repo access
    case repoAccessUpdated(sessionId: String, repoAccess: RepoAccess)

    // Resource limits
    case resourceLimitsResponse(limits: ResourceLimits)
}

extension ServerMessage: Decodable {
    private enum CodingKeys: String, CodingKey {
        case type
        case session, sessions, id, error, message
        case sessionId, content, partial, event, data, messageId
        case port, previewUrl
        case serverTime, messages, events, hasMore, lastNotificationId
        case repos
        case deletedIds
        case files, diff
        case snapshot, snapshots, snapshotId, messageCount
        case repoAccess
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let type = try container.decode(String.self, forKey: .type)

        switch type {
        case "session.created":
            let session = try container.decode(Session.self, forKey: .session)
            self = .sessionCreated(session: session)

        case "session.updated":
            let session = try container.decode(Session.self, forKey: .session)
            self = .sessionUpdated(session: session)

        case "session.deleted":
            let id = try container.decode(String.self, forKey: .id)
            self = .sessionDeleted(id: id)

        case "sessions.deletedAll":
            let deletedIds = try container.decodeIfPresent([String].self, forKey: .deletedIds) ?? []
            self = .sessionsDeletedAll(deletedIds: deletedIds)

        case "session.list":
            let sessions = try container.decode([Session].self, forKey: .sessions)
            self = .sessionList(sessions: sessions)

        case "session.error":
            let id = try container.decodeIfPresent(String.self, forKey: .id)
            let error = try container.decode(String.self, forKey: .error)
            self = .sessionError(id: id, error: error)

        case "agent.message":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let content = try container.decode(String.self, forKey: .content)
            let partial = try container.decodeIfPresent(Bool.self, forKey: .partial) ?? false
            let messageId = try container.decodeIfPresent(String.self, forKey: .messageId)
            self = .agentMessage(sessionId: sessionId, content: content, partial: partial, messageId: messageId)

        case "agent.event":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let event = try container.decode(RawAgentEvent.self, forKey: .event)
            self = .agentEvent(sessionId: sessionId, event: event.toAgentEvent())

        case "agent.done":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            self = .agentDone(sessionId: sessionId)

        case "agent.error":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let error = try container.decode(String.self, forKey: .error)
            self = .agentError(sessionId: sessionId, error: error)

        case "user.message":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let content = try container.decode(String.self, forKey: .content)
            self = .userMessage(sessionId: sessionId, content: content)

        case "terminal.output":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let data = try container.decode(String.self, forKey: .data)
            self = .terminalOutput(sessionId: sessionId, data: data)

        case "port.exposed":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let port = try container.decode(Int.self, forKey: .port)
            let previewUrl = try container.decode(String.self, forKey: .previewUrl)
            self = .portExposed(sessionId: sessionId, port: port, previewUrl: previewUrl)

        case "port.unexposed":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let port = try container.decode(Int.self, forKey: .port)
            self = .portUnexposed(sessionId: sessionId, port: port)

        case "port.error":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let port = try container.decode(Int.self, forKey: .port)
            let error = try container.decode(String.self, forKey: .error)
            self = .portError(sessionId: sessionId, port: port, error: error)

        case "error":
            let message = try container.decode(String.self, forKey: .message)
            self = .error(message: message)

        case "sync.response":
            let sessions = try container.decode([SessionWithMeta].self, forKey: .sessions)
            let serverTime = try container.decode(Date.self, forKey: .serverTime)
            self = .syncResponse(sessions: sessions, serverTime: serverTime)

        case "session.state":
            let session = try container.decode(Session.self, forKey: .session)
            let messages = try container.decodeIfPresent([PersistedMessage].self, forKey: .messages) ?? []
            let events = try container.decodeIfPresent([PersistedEvent].self, forKey: .events) ?? []
            let hasMore = try container.decodeIfPresent(Bool.self, forKey: .hasMore) ?? false
            let lastNotificationId = try container.decodeIfPresent(String.self, forKey: .lastNotificationId)
            self = .sessionState(session: session, messages: messages, events: events, hasMore: hasMore, lastNotificationId: lastNotificationId)

        case "github.repos":
            let repos = try container.decodeIfPresent([GitHubRepo].self, forKey: .repos) ?? []
            self = .githubRepos(repos: repos)

        case "git.status":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let files = try container.decodeIfPresent([GitFileChange].self, forKey: .files) ?? []
            self = .gitStatusResponse(sessionId: sessionId, files: files)

        case "git.diff":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let diff = try container.decodeIfPresent(String.self, forKey: .diff) ?? ""
            self = .gitDiffResponse(sessionId: sessionId, diff: diff)

        case "git.error":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let error = try container.decode(String.self, forKey: .error)
            self = .gitError(sessionId: sessionId, error: error)

        case "snapshot.created":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let snapshot = try container.decode(Snapshot.self, forKey: .snapshot)
            self = .snapshotCreated(sessionId: sessionId, snapshot: snapshot)

        case "snapshot.list":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let snapshots = try container.decodeIfPresent([Snapshot].self, forKey: .snapshots) ?? []
            self = .snapshotList(sessionId: sessionId, snapshots: snapshots)

        case "snapshot.restored":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let snapshotId = try container.decode(String.self, forKey: .snapshotId)
            let messageCount = try container.decodeIfPresent(Int.self, forKey: .messageCount) ?? 0
            self = .snapshotRestored(sessionId: sessionId, snapshotId: snapshotId, messageCount: messageCount)

        case "repo.access.updated":
            let sessionId = try container.decode(String.self, forKey: .sessionId)
            let repoAccessStr = try container.decode(String.self, forKey: .repoAccess)
            let repoAccess = RepoAccess(rawValue: repoAccessStr) ?? .read
            self = .repoAccessUpdated(sessionId: sessionId, repoAccess: repoAccess)

        default:
            throw DecodingError.dataCorruptedError(
                forKey: .type,
                in: container,
                debugDescription: "Unknown message type: \(type)"
            )
        }
    }
}

// MARK: - Raw Agent Event for Decoding

private struct RawAgentEvent: Decodable {
    let kind: String
    let timestamp: Date

    // Tool events
    let tool: String?
    let toolUseId: String?
    let parentToolUseId: String?  // Set when tool runs inside a Task/subagent
    let input: [String: AnyCodableValue]?
    let inputDelta: String?
    let result: String?
    let durationMs: Int64?  // Duration in milliseconds (for tool_end)

    // File change
    let path: String?
    let action: String?
    let linesAdded: Int?
    let linesRemoved: Int?

    // Command
    let command: String?
    let cwd: String?
    let exitCode: Int?
    let output: String?

    // Thinking
    let content: String?
    let thinkingId: String?
    let partial: Bool?

    // Port detected
    let port: Int?
    let process: String?
    let previewUrl: String?

    // Repo clone
    let repo: String?
    let stage: String?
    let message: String?

    // Error
    let error: String?

    func toAgentEvent() -> AgentEvent {
        let id = UUID()

        switch kind {
        case "tool_start":
            return .toolStart(ToolStartEvent(
                id: id,
                timestamp: timestamp,
                tool: tool ?? "Unknown",
                toolUseId: toolUseId ?? "",
                parentToolUseId: parentToolUseId,
                input: input ?? [:]
            ))

        case "tool_input":
            return .toolInput(ToolInputEvent(
                id: id,
                timestamp: timestamp,
                toolUseId: toolUseId ?? "",
                parentToolUseId: parentToolUseId,
                inputDelta: inputDelta ?? ""
            ))

        case "tool_input_complete":
            return .toolInputComplete(ToolInputCompleteEvent(
                id: id,
                timestamp: timestamp,
                toolUseId: toolUseId ?? "",
                parentToolUseId: parentToolUseId,
                input: input ?? [:]
            ))

        case "tool_end":
            return .toolEnd(ToolEndEvent(
                id: id,
                timestamp: timestamp,
                tool: tool ?? "Unknown",
                toolUseId: toolUseId ?? "",
                parentToolUseId: parentToolUseId,
                result: result,
                error: error,
                durationMs: durationMs
            ))

        case "file_change":
            let fileAction: FileAction
            switch action {
            case "create": fileAction = .create
            case "delete": fileAction = .delete
            default: fileAction = .edit
            }
            return .fileChange(FileChangeEvent(
                id: id,
                timestamp: timestamp,
                path: path ?? "",
                action: fileAction,
                linesAdded: linesAdded,
                linesRemoved: linesRemoved
            ))

        case "command_start":
            return .commandStart(CommandStartEvent(
                id: id,
                timestamp: timestamp,
                command: command ?? "",
                cwd: cwd
            ))

        case "command_end":
            return .commandEnd(CommandEndEvent(
                id: id,
                timestamp: timestamp,
                command: command ?? "",
                exitCode: exitCode ?? -1,
                output: output
            ))

        case "thinking":
            return .thinking(ThinkingEvent(
                id: id,
                timestamp: timestamp,
                thinkingId: thinkingId ?? "thinking_\(id.uuidString)",
                content: content ?? "",
                partial: partial ?? false
            ))

        case "tool_output":
            // Tool output is converted to thinking with "output_" prefix for aggregation
            // The EventStore.loadEvents() will recognize this and merge it into tool_end result
            return .thinking(ThinkingEvent(
                id: id,
                timestamp: timestamp,
                thinkingId: "output_\(toolUseId ?? id.uuidString)",
                content: result ?? "",
                partial: false
            ))

        case "port_exposed":
            return .portExposed(PortExposedEvent(
                id: id,
                timestamp: timestamp,
                port: port ?? 0,
                process: process,
                previewUrl: previewUrl
            ))

        case "port_unexposed":
            return .portUnexposed(PortUnexposedEvent(
                id: id,
                timestamp: timestamp,
                port: port ?? 0
            ))

        case "repo_clone":
            let cloneStage: RepoCloneStage
            switch stage {
            case "starting": cloneStage = .starting
            case "cloning": cloneStage = .cloning
            case "error": cloneStage = .error
            default: cloneStage = .done
            }
            return .repoClone(RepoCloneEvent(
                id: id,
                timestamp: timestamp,
                repo: repo ?? "",
                stage: cloneStage,
                message: message ?? ""
            ))

        case "agent_disconnected":
            return .agentDisconnected(AgentDisconnectedEvent(
                id: id,
                timestamp: timestamp,
                message: message ?? "Agent connection lost. Send a message to continue when reconnected."
            ))

        case "agent_reconnected":
            return .agentReconnected(AgentReconnectedEvent(
                id: id,
                timestamp: timestamp,
                message: message ?? "Agent reconnected. Send a message to continue."
            ))

        default:
            return .thinking(ThinkingEvent(
                id: id,
                timestamp: timestamp,
                thinkingId: "unknown_\(id.uuidString)",
                content: "Unknown event: \(kind)",
                partial: false
            ))
        }
    }
}
