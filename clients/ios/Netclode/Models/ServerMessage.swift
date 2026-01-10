import Foundation

enum ServerMessage: Sendable {
    case sessionCreated(session: Session)
    case sessionUpdated(session: Session)
    case sessionDeleted(id: String)
    case sessionList(sessions: [Session])
    case sessionError(id: String?, error: String)

    case agentMessage(sessionId: String, content: String, partial: Bool)
    case agentEvent(sessionId: String, event: AgentEvent)
    case agentDone(sessionId: String)
    case agentError(sessionId: String, error: String)
    case userMessage(sessionId: String, content: String)

    case terminalOutput(sessionId: String, data: String)

    case portExposed(sessionId: String, port: Int, previewUrl: String)
    case portError(sessionId: String, port: Int, error: String)

    case error(message: String)

    // Sync responses
    case syncResponse(sessions: [SessionWithMeta], serverTime: Date)
    case sessionState(session: Session, messages: [PersistedMessage], events: [PersistedEvent], hasMore: Bool, lastNotificationId: String?)
}

extension ServerMessage: Decodable {
    private enum CodingKeys: String, CodingKey {
        case type
        case session, sessions, id, error, message
        case sessionId, content, partial, event, data
        case port, previewUrl
        case serverTime, messages, events, hasMore, lastNotificationId
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
            self = .agentMessage(sessionId: sessionId, content: content, partial: partial)

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
    let input: [String: AnyCodableValue]?
    let inputDelta: String?
    let result: String?

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
                input: input ?? [:]
            ))

        case "tool_input":
            return .toolInput(ToolInputEvent(
                id: id,
                timestamp: timestamp,
                toolUseId: toolUseId ?? "",
                inputDelta: inputDelta ?? ""
            ))

        case "tool_end":
            return .toolEnd(ToolEndEvent(
                id: id,
                timestamp: timestamp,
                tool: tool ?? "Unknown",
                toolUseId: toolUseId ?? "",
                result: result,
                error: error
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

        case "port_exposed":
            return .portExposed(PortExposedEvent(
                id: id,
                timestamp: timestamp,
                port: port ?? 0,
                process: process,
                previewUrl: previewUrl
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
