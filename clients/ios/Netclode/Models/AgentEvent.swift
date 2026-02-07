import Foundation

protocol AgentEventProtocol: Identifiable, Codable, Sendable {
    var id: UUID { get }
    var kind: AgentEventKind { get }
    var timestamp: Date { get }
}

enum AgentEventKind: String, Codable, Sendable {
    case toolStart = "tool_start"
    case toolInput = "tool_input"
    case toolInputComplete = "tool_input_complete"
    case toolEnd = "tool_end"
    case fileChange = "file_change"
    case commandStart = "command_start"
    case commandEnd = "command_end"
    case thinking
    case portExposed = "port_exposed"
    case portUnexposed = "port_unexposed"
    case repoClone = "repo_clone"
    case agentDisconnected = "agent_disconnected"
    case agentReconnected = "agent_reconnected"

    var displayName: String {
        switch self {
        case .toolStart: "Tool Started"
        case .toolInput: "Tool Input"
        case .toolInputComplete: "Tool Input Complete"
        case .toolEnd: "Tool Finished"
        case .fileChange: "File Changed"
        case .commandStart: "Command Started"
        case .commandEnd: "Command Finished"
        case .thinking: "Thinking"
        case .portExposed: "Port Exposed"
        case .portUnexposed: "Port Removed"
        case .repoClone: "Repository"
        case .agentDisconnected: "Connection Lost"
        case .agentReconnected: "Reconnected"
        }
    }

    var systemImage: String {
        switch self {
        case .toolStart, .toolInput, .toolInputComplete, .toolEnd: "wrench.and.screwdriver.fill"
        case .fileChange: "doc.fill"
        case .commandStart, .commandEnd: "terminal.fill"
        case .thinking: "brain.head.profile"
        case .portExposed: "network"
        case .portUnexposed: "network.slash"
        case .repoClone: "arrow.down.circle.fill"
        case .agentDisconnected: "wifi.slash"
        case .agentReconnected: "wifi"
        }
    }
}

enum FileAction: String, Codable, Sendable {
    case create
    case edit
    case delete

    var displayName: String {
        rawValue.capitalized
    }

    var systemImage: String {
        switch self {
        case .create: "plus.circle.fill"
        case .edit: "pencil.circle.fill"
        case .delete: "minus.circle.fill"
        }
    }
}

enum AgentEvent: Identifiable, Sendable {
    case toolStart(ToolStartEvent)
    case toolInput(ToolInputEvent)
    case toolInputComplete(ToolInputCompleteEvent)
    case toolEnd(ToolEndEvent)
    case fileChange(FileChangeEvent)
    case commandStart(CommandStartEvent)
    case commandEnd(CommandEndEvent)
    case thinking(ThinkingEvent)
    case portExposed(PortExposedEvent)
    case portUnexposed(PortUnexposedEvent)
    case repoClone(RepoCloneEvent)
    case agentDisconnected(AgentDisconnectedEvent)
    case agentReconnected(AgentReconnectedEvent)

    var id: UUID {
        switch self {
        case .toolStart(let e): e.id
        case .toolInput(let e): e.id
        case .toolInputComplete(let e): e.id
        case .toolEnd(let e): e.id
        case .fileChange(let e): e.id
        case .commandStart(let e): e.id
        case .commandEnd(let e): e.id
        case .thinking(let e): e.id
        case .portExposed(let e): e.id
        case .portUnexposed(let e): e.id
        case .repoClone(let e): e.id
        case .agentDisconnected(let e): e.id
        case .agentReconnected(let e): e.id
        }
    }

    var kind: AgentEventKind {
        switch self {
        case .toolStart: .toolStart
        case .toolInput: .toolInput
        case .toolInputComplete: .toolInputComplete
        case .toolEnd: .toolEnd
        case .fileChange: .fileChange
        case .commandStart: .commandStart
        case .commandEnd: .commandEnd
        case .thinking: .thinking
        case .portExposed: .portExposed
        case .portUnexposed: .portUnexposed
        case .repoClone: .repoClone
        case .agentDisconnected: .agentDisconnected
        case .agentReconnected: .agentReconnected
        }
    }

    var timestamp: Date {
        switch self {
        case .toolStart(let e): e.timestamp
        case .toolInput(let e): e.timestamp
        case .toolInputComplete(let e): e.timestamp
        case .toolEnd(let e): e.timestamp
        case .fileChange(let e): e.timestamp
        case .commandStart(let e): e.timestamp
        case .commandEnd(let e): e.timestamp
        case .thinking(let e): e.timestamp
        case .portExposed(let e): e.timestamp
        case .portUnexposed(let e): e.timestamp
        case .repoClone(let e): e.timestamp
        case .agentDisconnected(let e): e.timestamp
        case .agentReconnected(let e): e.timestamp
        }
    }
}

struct ToolStartEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .toolStart }
    let timestamp: Date
    let tool: String
    let toolUseId: String
    let parentToolUseId: String?  // Set when this tool runs inside a Task/subagent
    let input: [String: AnyCodableValue]
}

struct ToolInputEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .toolInput }
    let timestamp: Date
    let toolUseId: String
    let parentToolUseId: String?  // Set when this tool runs inside a Task/subagent
    let inputDelta: String
}

struct ToolInputCompleteEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .toolInputComplete }
    let timestamp: Date
    let toolUseId: String
    let parentToolUseId: String?  // Set when this tool runs inside a Task/subagent
    let input: [String: AnyCodableValue]
}

struct ToolEndEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .toolEnd }
    let timestamp: Date
    let tool: String
    let toolUseId: String
    let parentToolUseId: String?  // Set when this tool runs inside a Task/subagent
    let result: String?
    let error: String?
    let durationMs: Int64?  // Duration in milliseconds

    var isSuccess: Bool { error == nil }
}

struct FileChangeEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .fileChange }
    let timestamp: Date
    let path: String
    let action: FileAction
    let linesAdded: Int?
    let linesRemoved: Int?

    var fileName: String {
        (path as NSString).lastPathComponent
    }

    var changeDescription: String {
        var parts: [String] = []
        if let added = linesAdded, added > 0 {
            parts.append("+\(added)")
        }
        if let removed = linesRemoved, removed > 0 {
            parts.append("-\(removed)")
        }
        return parts.isEmpty ? action.displayName : parts.joined(separator: " ")
    }
}

struct CommandStartEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .commandStart }
    let timestamp: Date
    let command: String
    let cwd: String?
}

struct CommandEndEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .commandEnd }
    let timestamp: Date
    let command: String
    let exitCode: Int
    let output: String?

    var isSuccess: Bool { exitCode == 0 }
}

struct ThinkingEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .thinking }
    let timestamp: Date
    let thinkingId: String  // Correlate streaming updates for same thinking block
    var content: String     // Mutable to accumulate streaming content
    let partial: Bool       // true = streaming delta, false = complete block
}

struct PortExposedEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .portExposed }
    let timestamp: Date
    let port: Int
    let process: String?
    let previewUrl: String?
}

struct PortUnexposedEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .portUnexposed }
    let timestamp: Date
    let port: Int
}

/// Repository clone/pull progress event.
enum RepoCloneStage: String, Codable, Sendable {
    case starting
    case cloning
    case done
    case error
}

struct RepoCloneEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .repoClone }
    let timestamp: Date
    let repo: String
    let stage: RepoCloneStage
    let message: String
    
    var isComplete: Bool {
        stage == .done || stage == .error
    }
    
    var isError: Bool {
        stage == .error
    }
}

/// Agent connection lost event - shown when agent disconnects during execution.
struct AgentDisconnectedEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .agentDisconnected }
    let timestamp: Date
    let message: String
}

/// Agent reconnected event - shown when agent reconnects after disconnect.
struct AgentReconnectedEvent: AgentEventProtocol {
    let id: UUID
    var kind: AgentEventKind { .agentReconnected }
    let timestamp: Date
    let message: String
}

// MARK: - AnyCodableValue for dynamic JSON

enum AnyCodableValue: Codable, Sendable, CustomStringConvertible {
    case string(String)
    case int(Int)
    case double(Double)
    case bool(Bool)
    case array([AnyCodableValue])
    case dictionary([String: AnyCodableValue])
    case null

    var description: String {
        switch self {
        case .string(let s): return s
        case .int(let i): return String(i)
        case .double(let d): return String(d)
        case .bool(let b): return String(b)
        case .array(let a): return a.map(\.description).joined(separator: ", ")
        case .dictionary(let d): return d.map { "\($0): \($1)" }.joined(separator: ", ")
        case .null: return "null"
        }
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() {
            self = .null
        } else if let bool = try? container.decode(Bool.self) {
            self = .bool(bool)
        } else if let int = try? container.decode(Int.self) {
            self = .int(int)
        } else if let double = try? container.decode(Double.self) {
            self = .double(double)
        } else if let string = try? container.decode(String.self) {
            self = .string(string)
        } else if let array = try? container.decode([AnyCodableValue].self) {
            self = .array(array)
        } else if let dict = try? container.decode([String: AnyCodableValue].self) {
            self = .dictionary(dict)
        } else {
            throw DecodingError.dataCorruptedError(in: container, debugDescription: "Cannot decode AnyCodableValue")
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .string(let s): try container.encode(s)
        case .int(let i): try container.encode(i)
        case .double(let d): try container.encode(d)
        case .bool(let b): try container.encode(b)
        case .array(let a): try container.encode(a)
        case .dictionary(let d): try container.encode(d)
        case .null: try container.encodeNil()
        }
    }
}

// MARK: - Preview Data

extension AgentEvent {
    static let previewToolStart = AgentEvent.toolStart(ToolStartEvent(
        id: UUID(),
        timestamp: Date(),
        tool: "Read",
        toolUseId: "tool_123",
        parentToolUseId: nil,
        input: ["file_path": .string("/src/auth/AuthService.swift")]
    ))

    static let previewToolEnd = AgentEvent.toolEnd(ToolEndEvent(
        id: UUID(),
        timestamp: Date(),
        tool: "Read",
        toolUseId: "tool_123",
        parentToolUseId: nil,
        result: "File contents...",
        error: nil,
        durationMs: 45
    ))

    static let previewFileChange = AgentEvent.fileChange(FileChangeEvent(
        id: UUID(),
        timestamp: Date(),
        path: "/src/auth/AuthService.swift",
        action: .edit,
        linesAdded: 25,
        linesRemoved: 10
    ))

    static let previewCommandStart = AgentEvent.commandStart(CommandStartEvent(
        id: UUID(),
        timestamp: Date(),
        command: "npm run build",
        cwd: "/workspace"
    ))

    static let previewCommandEnd = AgentEvent.commandEnd(CommandEndEvent(
        id: UUID(),
        timestamp: Date(),
        command: "npm run build",
        exitCode: 0,
        output: "Build successful!"
    ))

    static let previewThinking = AgentEvent.thinking(ThinkingEvent(
        id: UUID(),
        timestamp: Date(),
        thinkingId: "thinking_preview_1",
        content: "Analyzing the codebase structure...",
        partial: false
    ))

    static let previewList: [AgentEvent] = [
        previewToolStart,
        previewToolEnd,
        previewFileChange,
        previewCommandStart,
        previewCommandEnd
    ]
}
