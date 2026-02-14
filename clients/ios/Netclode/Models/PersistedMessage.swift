import Foundation

/// Message persisted on the server
struct PersistedMessage: Codable, Identifiable, Sendable {
    let id: String
    let sessionId: String
    let role: ChatRole
    let content: String
    let timestamp: Date

    enum ChatRole: String, Codable, Sendable {
        case user
        case assistant
    }

    /// Convert to ChatMessage for UI display
    func toChatMessage() -> ChatMessage {
        ChatMessage(
            role: role == .user ? .user : .assistant,
            content: content,
            timestamp: timestamp,
            serverMessageId: id
        )
    }
}

/// Event persisted on the server
struct PersistedEvent: Codable, Sendable {
    let id: String
    let sessionId: String
    let event: RawAgentEventData
    let timestamp: Date

    /// Raw event data for decoding
    struct RawAgentEventData: Codable, Sendable {
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
}

/// Session with sync metadata
struct SessionWithMeta: Codable, Sendable {
    let id: String
    let name: String
    let status: String
    let repos: [String]
    let repoAccess: RepoAccess?
    let createdAt: Date
    let lastActiveAt: Date
    let messageCount: Int?
    let lastMessageId: String?
    let sdkType: SdkType?
    let model: String?
    let copilotBackend: CopilotBackend?

    func toSession() -> Session {
        Session(
            id: id,
            name: name,
            status: SessionStatus(rawValue: status) ?? .paused,
            repos: repos,
            repoAccess: repoAccess,
            createdAt: createdAt,
            lastActiveAt: lastActiveAt,
            sdkType: sdkType,
            model: model,
            copilotBackend: copilotBackend,
            messageCount: messageCount
        )
    }
}
