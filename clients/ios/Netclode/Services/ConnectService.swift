import Foundation
import Connect
import ConnectNIO
import SwiftProtobuf
import os.log

private let logger = Logger(subsystem: "com.netclode", category: "ConnectService")

/// Errors that can occur during Connect protocol operations.
enum ConnectError: Error, LocalizedError {
    case connectionTimeout
    case clientCreationFailed
    case streamCreationFailed
    case connectionFailed(message: String)
    case sendFailed(underlying: Error)
    
    var errorDescription: String? {
        switch self {
        case .connectionTimeout:
            return "Connection timed out"
        case .clientCreationFailed:
            return "Failed to create Connect client"
        case .streamCreationFailed:
            return "Failed to create bidirectional stream"
        case .connectionFailed(let message):
            return "Connection failed: \(message)"
        case .sendFailed(let error):
            return "Failed to send message: \(error.localizedDescription)"
        }
    }
}

/// Reason for disconnection
enum DisconnectReason: Equatable, Sendable {
    case initial
    case networkLost
    case serverError(String)
    case authFailure
    case userInitiated
    case backgrounded
    
    var description: String {
        switch self {
        case .initial: "Not connected"
        case .networkLost: "Network unavailable"
        case .serverError(let msg): msg
        case .authFailure: "Authentication failed"
        case .userInitiated: "Disconnected by user"
        case .backgrounded: "App was backgrounded"
        }
    }
}

/// Connection state for the service
enum ConnectionState: Equatable, Sendable {
    case disconnected(reason: DisconnectReason)
    case connecting
    case connected
    case reconnecting(attempt: Int, maxAttempts: Int)
    case suspended  // App backgrounded, connection intentionally closed

    var isConnected: Bool {
        if case .connected = self { return true }
        return false
    }
    
    /// Whether the connection is usable for sending messages
    var isUsable: Bool {
        if case .connected = self { return true }
        return false
    }
    
    /// Whether we should attempt automatic reconnection
    var shouldAttemptReconnect: Bool {
        switch self {
        case .disconnected(let reason):
            switch reason {
            case .authFailure, .userInitiated:
                return false
            default:
                return true
            }
        case .suspended:
            return false
        default:
            return false
        }
    }

    var displayName: String {
        switch self {
        case .disconnected(let reason): "Disconnected: \(reason.description)"
        case .connecting: "Connecting..."
        case .connected: "Connected"
        case .reconnecting(let attempt, let max): "Reconnecting (\(attempt)/\(max))..."
        case .suspended: "Suspended"
        }
    }

    var systemImage: String {
        switch self {
        case .disconnected, .suspended: "wifi.slash"
        case .connecting, .reconnecting: "wifi.exclamationmark"
        case .connected: "wifi"
        }
    }
}

/// Strategy for reconnection attempts with exponential backoff
struct ReconnectionStrategy {
    var baseDelay: TimeInterval = 1.0
    var maxDelay: TimeInterval = 32.0
    var maxAttempts: Int = 10
    var jitterFactor: Double = 0.3
    var foregroundMultiplier: Double = 0.5 // Faster reconnection in foreground
    
    func delay(for attempt: Int, isForeground: Bool) -> TimeInterval {
        let exponentialDelay = min(baseDelay * pow(2, Double(attempt - 1)), maxDelay)
        let jitter = exponentialDelay * jitterFactor * Double.random(in: -1...1)
        let delay = exponentialDelay + jitter
        
        return isForeground ? delay * foregroundMultiplier : delay
    }
}

/// ConnectService provides gRPC/Connect protocol communication with the control plane.
@MainActor
@Observable
final class ConnectService {
    private(set) var connectionState: ConnectionState = .disconnected(reason: .initial)
    
    private var client: ProtocolClient?
    private var serviceClient: Netclode_V1_ClientServiceClient?
    private var stream: (any BidirectionalAsyncStreamInterface<Netclode_V1_ClientMessage, Netclode_V1_ServerMessage>)?
    private var receiveTask: Task<Void, Never>?
    private var reconnectTask: Task<Void, Never>?
    private var keepAliveTask: Task<Void, Never>?
    private var serverURL: String = ""
    private var connectPortOverride: String = ""
    private var lastActivityAt = Date()
    
    // Message stream for consumers
    private var _messagesContinuation: AsyncStream<ServerMessage>.Continuation?
    private var _messagesStream: AsyncStream<ServerMessage>?
    
    // Network monitoring (injected by AppStateCoordinator)
    var networkMonitor: NetworkMonitor?
    
    // Reconnection strategy
    private let strategy = ReconnectionStrategy()
    private var isForeground: Bool = true
    private var isNetworkReconnecting: Bool = false
    
    static let maxReconnectAttempts = 10
    static let connectionTimeoutSeconds: UInt64 = 15
    private let keepAliveInterval: UInt64 = 30_000_000_000
    private let keepAliveIdleThreshold: TimeInterval = 30
    
    var messages: AsyncStream<ServerMessage> {
        if let stream = _messagesStream {
            return stream
        }
        let stream = AsyncStream<ServerMessage> { [weak self] continuation in
            self?._messagesContinuation = continuation
        }
        _messagesStream = stream
        return stream
    }
    
    /// Connect to the server.
    /// - Parameters:
    ///   - serverURL: The base server URL (e.g., "netclode-control-plane" or "http://localhost:3000")
    ///   - connectPort: Optional port override for the Connect protocol. If empty, uses default logic.
    func connect(to serverURL: String, connectPort: String = "") {
        // Always honor explicit user reconnect requests, even while reconnecting.
        // This lets users recover from a stale/incorrect host without restarting the app.
        self.serverURL = serverURL
        self.connectPortOverride = connectPort

        // Cancel any in-flight reconnect loop before starting a fresh connect attempt.
        reconnectTask?.cancel()

        // Tear down active state so the next attempt uses the updated target.
        receiveTask?.cancel()
        keepAliveTask?.cancel()
        stream = nil
        client = nil
        serviceClient = nil
        connectionState = .disconnected(reason: .userInitiated)

        Task {
            await performConnect()
        }
    }
    
    private func performConnect(attempt: Int = 0) async {
        // Cancel any existing stream
        receiveTask?.cancel()
        keepAliveTask?.cancel()
        stream = nil
        
        if attempt > 0 {
            connectionState = .reconnecting(attempt: attempt, maxAttempts: strategy.maxAttempts)
        } else {
            connectionState = .connecting
        }
        
        let grpcHost = buildConnectHost()
        print("[Connect] Connecting to: \(grpcHost) (attempt \(attempt))")
        
        // Create the Connect client with timeout
        do {
            try await withThrowingTaskGroup(of: Void.self) { group in
                group.addTask {
                    try await Task.sleep(nanoseconds: Self.connectionTimeoutSeconds * 1_000_000_000)
                    throw ConnectError.connectionTimeout
                }
                
                group.addTask {
                    try await self.establishConnection(to: grpcHost)
                }
                
                // Wait for first to complete (either connection or timeout)
                try await group.next()
                group.cancelAll()
            }
        } catch ConnectError.connectionTimeout {
            print("[Connect] Connection timed out after \(Self.connectionTimeoutSeconds)s")
            connectionState = .disconnected(reason: .serverError("Connection timed out"))
            return
        } catch {
            print("[Connect] Connection failed: \(error)")
            connectionState = .disconnected(reason: .serverError(error.localizedDescription))
            return
        }
        
        guard let currentStream = stream else {
            print("[Connect] Failed to create stream")
            connectionState = .disconnected(reason: .serverError("Failed to create stream"))
            return
        }
        
        // Use a continuation to wait for validation result
        let isValid: Bool = await withCheckedContinuation { continuation in
            var hasResumed = false
            
            // Start receiving with validation callback
            startReceiving(stream: currentStream, onValidation: { success in
                guard !hasResumed else { return }
                hasResumed = true
                continuation.resume(returning: success)
            })
            
            // Send sync to trigger the actual HTTP connection
            send(.sync)
            
            // Set up timeout
            Task {
                try? await Task.sleep(nanoseconds: 10_000_000_000) // 10 seconds
                guard !hasResumed else { return }
                hasResumed = true
                continuation.resume(returning: false)
            }
        }
        
        guard isValid else {
            print("[Connect] Connection validation failed or timed out")
            connectionState = .disconnected(reason: .serverError("Connection validation failed"))
            receiveTask?.cancel()
            stream = nil
            return
        }
        
        print("[Connect] Connected and validated successfully")
        connectionState = .connected
        recordActivity()

        // Keep-alive to detect dead connections
        startKeepAlive()
    }
    
    /// Build the Connect protocol host URL from serverURL and optional port override.
    private func buildConnectHost() -> String {
        let normalized = normalizeServerURL(serverURL)
        guard var components = URLComponents(string: normalized) else {
            return normalized
        }

        // If explicit port override provided, use it
        let override = connectPortOverride.trimmingCharacters(in: .whitespacesAndNewlines)
        if !override.isEmpty, let overridePort = Int(override) {
            components.port = overridePort
            return components.string ?? "\(normalized):\(overridePort)"
        }

        // For HTTPS (Tailscale Ingress), use default port 443
        if components.scheme == "https" {
            // Don't modify port for HTTPS - use default 443
            return components.string ?? normalized
        }
        
        // For HTTP (local dev), map 3000 → 3001 or default to 3001
        if components.port == 3000 {
            components.port = 3001
            return components.string ?? normalized.replacingOccurrences(of: ":3000", with: ":3001")
        }

        if components.port == nil {
            components.port = 3001
        }

        return components.string ?? normalized
    }

    private func normalizeServerURL(_ rawURL: String) -> String {
        var urlString = rawURL.trimmingCharacters(in: .whitespacesAndNewlines)

        if urlString.hasPrefix("ws://") {
            urlString = "http://" + String(urlString.dropFirst("ws://".count))
        } else if urlString.hasPrefix("wss://") {
            urlString = "https://" + String(urlString.dropFirst("wss://".count))
        } else if !urlString.hasPrefix("http://") && !urlString.hasPrefix("https://") {
            // Default to HTTPS for Tailscale domains (.ts.net)
            if urlString.contains(".ts.net") {
                urlString = "https://\(urlString)"
            } else {
                urlString = "http://\(urlString)"
            }
        }

        guard var components = URLComponents(string: urlString) else {
            return urlString
        }

        components.path = ""
        components.query = nil
        components.fragment = nil

        return components.string ?? urlString
    }
    
    /// Establish the actual connection (called within timeout context).
    private func establishConnection(to grpcHost: String) async throws {
        // Use NIOHTTPClient instead of URLSessionHTTPClient for better HTTP/2
        // handling through iOS Tailscale network extension. NIOHTTPClient uses
        // Swift NIO's HTTP/2 implementation which bypasses URLSession.
        client = ProtocolClient(
            httpClient: NIOHTTPClient(host: grpcHost, timeout: 120),
            config: ProtocolClientConfig(
                host: grpcHost,
                networkProtocol: .connect,
                codec: ProtoCodec()
            )
        )
        
        guard let client = client else {
            throw ConnectError.clientCreationFailed
        }
        
        serviceClient = Netclode_V1_ClientServiceClient(client: client)
        
        // Open the bidirectional stream
        stream = serviceClient?.connect(headers: [:])
        
        guard stream != nil else {
            throw ConnectError.streamCreationFailed
        }
    }
    
    private func startReceiving(
        stream: any BidirectionalAsyncStreamInterface<Netclode_V1_ClientMessage, Netclode_V1_ServerMessage>,
        onValidation: ((Bool) -> Void)? = nil
    ) {
        receiveTask = Task { [weak self] in
            var validationCallback = onValidation
            var hasValidated = false
            
            for await result in stream.results() {
                guard let self, !Task.isCancelled else { break }
                
                switch result {
                case .headers:
                    print("[Connect] Received headers")
                    self.recordActivity()
                    // Connection validated on first headers
                    if !hasValidated {
                        hasValidated = true
                        validationCallback?(true)
                        validationCallback = nil
                    }
                    
                case .message(let protoMessage):
                    self.recordActivity()
                    // Connection validated on first message (if headers weren't received first)
                    if !hasValidated {
                        hasValidated = true
                        validationCallback?(true)
                        validationCallback = nil
                    }
                    // Convert proto message to ServerMessage
                    if let serverMessage = self.convertProtoMessage(protoMessage) {
                        self._messagesContinuation?.yield(serverMessage)
                    }
                    
                case .complete(let code, let error, _):
                    print("[Connect] Stream completed: code=\(code), error=\(String(describing: error))")
                    // If we haven't validated yet, this is a connection failure
                    if !hasValidated {
                        hasValidated = true
                        validationCallback?(false)
                        validationCallback = nil
                    }
                    // Trigger reconnection
                    await self.handleDisconnection()
                }
            }
            
            // If loop exits without validation (shouldn't happen), mark as failed
            if !hasValidated {
                validationCallback?(false)
            }
        }
    }
    
    /// Convert proto ServerMessage to the app's ServerMessage type
    private func convertProtoMessage(_ proto: Netclode_V1_ServerMessage) -> ServerMessage? {
        switch proto.message {
        case .sessionCreated(let msg):
            return .sessionCreated(session: convertSession(msg.session))
            
        case .sessionUpdated(let msg):
            return .sessionUpdated(session: convertSession(msg.session))
            
        case .sessionDeleted(let msg):
            return .sessionDeleted(id: msg.sessionID)
            
        case .sessionsDeletedAll(let msg):
            return .sessionsDeletedAll(deletedIds: msg.deletedIds)
            
        case .sessionList(let msg):
            return .sessionList(sessions: msg.sessions.map { convertSession($0) })
            
        case .sessionState(let msg):
            let sessionId = msg.session.id
            // Convert unified StreamEntry list to separate messages and events for backward compatibility
            let (messages, events) = convertStreamEntriesToMessagesAndEvents(msg.entries, sessionId: sessionId)
            return .sessionState(
                session: convertSession(msg.session),
                messages: messages,
                events: events,
                hasMore: msg.hasMore_p,
                lastNotificationId: msg.hasLastStreamID ? msg.lastStreamID : nil
            )
            
        case .syncResponse(let msg):
            return .syncResponse(
                sessions: msg.sessions.map { convertSessionSummary($0) },
                serverTime: msg.serverTime.date
            )
            
        case .streamEntry(let msg):
            // Handle unified stream entry - route to appropriate message type
            return convertStreamEntryToServerMessage(msg.sessionID, msg.entry)
            
        case .portExposed(let msg):
            return .portExposed(sessionId: msg.sessionID, port: Int(msg.port), previewUrl: msg.previewURL)
            
        case .githubRepos(let msg):
            return .githubRepos(repos: msg.repos.map { convertGitHubRepo($0) })
            
        case .gitStatus(let msg):
            return .gitStatusResponse(sessionId: msg.sessionID, files: msg.files.map { convertGitFileChange($0) })
            
        case .gitDiff(let msg):
            return .gitDiffResponse(sessionId: msg.sessionID, diff: msg.diff)
            
        case .error(let msg):
            // Unified error response - extract session ID if present
            let sessionId = msg.error.hasSessionID ? msg.error.sessionID : nil
            let errorMessage = msg.error.message
            let errorCode = msg.error.code
            
            // Route to appropriate error type based on code
            if let sid = sessionId {
                if errorCode == "SESSION_ERROR" {
                    return .sessionError(id: sid, error: errorMessage)
                } else if errorCode == "AGENT_ERROR" {
                    return .agentError(sessionId: sid, error: errorMessage)
                } else if errorCode == "PORT_ERROR" {
                    return .portError(sessionId: sid, port: 0, error: errorMessage)
                } else if errorCode == "GIT_ERROR" {
                    return .gitError(sessionId: sid, error: errorMessage)
                }
            }
            return .error(message: errorMessage)
            
        case .models(let msg):
            let models = msg.models.map { proto in
                CopilotModel(
                    id: proto.id,
                    name: proto.name,
                    provider: proto.hasProvider ? proto.provider : nil,
                    capabilities: proto.capabilities,
                    reasoningEffort: proto.hasReasoningEffort ? proto.reasoningEffort : nil
                )
            }
            let sdkType: SdkType? = msg.hasSdkType ? convertSdkType(msg.sdkType) : nil
            return .modelsResponse(models: models, sdkType: sdkType)

        case .copilotStatus(let msg):
            let auth = CopilotAuthStatus(
                isAuthenticated: msg.auth.isAuthenticated,
                authType: msg.auth.hasAuthType ? msg.auth.authType : nil,
                login: msg.auth.hasLogin ? msg.auth.login : nil
            )
            let quota: CopilotPremiumQuota?
            if msg.hasQuota {
                quota = CopilotPremiumQuota(
                    used: Int(msg.quota.used),
                    limit: Int(msg.quota.limit),
                    remaining: Int(msg.quota.remaining),
                    resetAt: msg.quota.hasResetAt ? msg.quota.resetAt : nil
                )
            } else {
                quota = nil
            }
            return .copilotStatusResponse(status: CopilotStatus(auth: auth, quota: quota))

        case .snapshotCreated(let msg):
            let snapshot = convertSnapshot(msg.snapshot)
            return .snapshotCreated(sessionId: msg.sessionID, snapshot: snapshot)

        case .snapshotList(let msg):
            let snapshots = msg.snapshots.map { convertSnapshot($0) }
            return .snapshotList(sessionId: msg.sessionID, snapshots: snapshots)

        case .snapshotRestored(let msg):
            return .snapshotRestored(sessionId: msg.sessionID, snapshotId: msg.snapshotID, messageCount: Int(msg.messagesRestored))

        case .repoAccessUpdated(let msg):
            return .repoAccessUpdated(sessionId: msg.sessionID, repoAccess: convertRepoAccess(msg.repoAccess))

        case .resourceLimits(let msg):
            return .resourceLimitsResponse(limits: ResourceLimits(
                maxVcpus: msg.maxVcpus,
                maxMemoryMB: msg.maxMemoryMb,
                defaultVcpus: msg.defaultVcpus,
                defaultMemoryMB: msg.defaultMemoryMb
            ))

        case .none:
            return nil
        }
    }
    
    // MARK: - Proto to Model Conversions
    
    private func convertSession(_ proto: Netclode_V1_Session) -> Session {
        Session(
            id: proto.id,
            name: proto.name,
            status: convertSessionStatus(proto.status),
            repos: proto.repos,
            repoAccess: proto.hasRepoAccess ? convertRepoAccess(proto.repoAccess) : nil,
            createdAt: proto.createdAt.date,
            lastActiveAt: proto.lastActiveAt.date,
            sdkType: proto.hasSdkType ? convertSdkType(proto.sdkType) : nil,
            model: proto.hasModel ? proto.model : nil,
            copilotBackend: proto.hasCopilotBackend ? convertCopilotBackend(proto.copilotBackend) : nil
        )
    }

    private func convertSnapshot(_ proto: Netclode_V1_Snapshot) -> Snapshot {
        Snapshot(
            id: proto.id,
            sessionId: proto.sessionID,
            name: proto.name,
            createdAt: proto.createdAt.date,
            sizeBytes: proto.sizeBytes,
            turnNumber: proto.turnNumber,
            messageCount: proto.messageCount
        )
    }

    private func convertSdkType(_ proto: Netclode_V1_SdkType) -> SdkType {
        switch proto {
        case .claude: return .claude
        case .opencode: return .opencode
        case .copilot: return .copilot
        case .codex: return .codex
        case .unspecified, .UNRECOGNIZED: return .claude
        }
    }
    
    private func convertRepoAccess(_ proto: Netclode_V1_RepoAccess) -> RepoAccess {
        switch proto {
        case .write: return .write
        case .read, .unspecified, .UNRECOGNIZED: return .read
        }
    }
    
    private func convertToProtoRepoAccess(_ access: RepoAccess) -> Netclode_V1_RepoAccess {
        switch access {
        case .read: return .read
        case .write: return .write
        }
    }

    private func convertToProtoSdkType(_ sdkType: SdkType) -> Netclode_V1_SdkType {
        switch sdkType {
        case .claude: return .claude
        case .opencode: return .opencode
        case .copilot: return .copilot
        case .codex: return .codex
        }
    }

    private func convertToProtoCopilotBackend(_ backend: CopilotBackend) -> Netclode_V1_CopilotBackend {
        switch backend {
        case .github: return .github
        case .anthropic: return .anthropic
        }
    }

    private func convertCopilotBackend(_ proto: Netclode_V1_CopilotBackend) -> CopilotBackend {
        switch proto {
        case .github: return .github
        case .anthropic: return .anthropic
        case .unspecified, .UNRECOGNIZED: return .anthropic
        }
    }
    
    private func convertSessionSummary(_ proto: Netclode_V1_SessionSummary) -> SessionWithMeta {
        let session = proto.session
        return SessionWithMeta(
            id: session.id,
            name: session.name,
            status: convertSessionStatus(session.status).rawValue,
            repos: session.repos,
            repoAccess: session.hasRepoAccess ? convertRepoAccess(session.repoAccess) : nil,
            createdAt: session.createdAt.date,
            lastActiveAt: session.lastActiveAt.date,
            messageCount: proto.hasMessageCount ? Int(proto.messageCount) : nil,
            lastMessageId: proto.hasLastStreamID ? proto.lastStreamID : nil,
            sdkType: session.hasSdkType ? convertSdkType(session.sdkType) : nil,
            model: session.hasModel ? session.model : nil,
            copilotBackend: session.hasCopilotBackend ? convertCopilotBackend(session.copilotBackend) : nil
        )
    }
    
    private func convertSessionStatus(_ proto: Netclode_V1_SessionStatus) -> SessionStatus {
        switch proto {
        case .creating: return .creating
        case .resuming: return .resuming
        case .ready: return .ready
        case .running: return .running
        case .paused: return .paused
        case .error: return .error
        case .interrupted: return .interrupted
        case .unspecified, .UNRECOGNIZED: return .paused
        }
    }
    
    /// Convert AgentEvent proto to internal AgentEvent type
    /// Note: timestamp and partial flag come from the parent StreamEntry, not the AgentEvent itself
    private func convertAgentEvent(_ proto: Netclode_V1_AgentEvent, timestamp: Date = Date(), partial: Bool = false) -> AgentEvent {
        let id = UUID()
        let correlationId = proto.correlationID
        
        switch proto.kind {
        case .toolStart:
            let payload = proto.toolStart
            return .toolStart(ToolStartEvent(
                id: id,
                timestamp: timestamp,
                tool: payload.tool,
                toolUseId: correlationId,
                parentToolUseId: payload.hasParentToolUseID ? payload.parentToolUseID : nil,
                input: [:]  // Input comes via toolInput events
            ))
            
        case .toolInput:
            let payload = proto.toolInput
            // Check if this is a partial delta or complete input
            if payload.hasDelta {
                return .toolInput(ToolInputEvent(
                    id: id,
                    timestamp: timestamp,
                    toolUseId: correlationId,
                    parentToolUseId: nil,
                    inputDelta: payload.delta
                ))
            } else {
                // Complete input
                return .toolInputComplete(ToolInputCompleteEvent(
                    id: id,
                    timestamp: timestamp,
                    toolUseId: correlationId,
                    parentToolUseId: nil,
                    input: payload.hasInput ? convertProtoStruct(payload.input) : [:]
                ))
            }
            
        case .toolOutput:
            // Tool output is handled as part of tool lifecycle, convert to thinking for display
            let payload = proto.toolOutput
            let output = payload.hasDelta ? payload.delta : (payload.hasOutput ? payload.output : "")
            return .thinking(ThinkingEvent(
                id: id,
                timestamp: timestamp,
                thinkingId: "output_\(correlationId)",
                content: output,
                partial: payload.hasDelta
            ))
            
        case .toolEnd:
            let payload = proto.toolEnd
            return .toolEnd(ToolEndEvent(
                id: id,
                timestamp: timestamp,
                tool: "",  // Tool name comes from toolStart
                toolUseId: correlationId,
                parentToolUseId: nil,
                result: payload.hasResult ? payload.result : nil,
                error: payload.hasError ? payload.error : nil,
                durationMs: payload.hasDurationMs ? payload.durationMs : nil
            ))
            
        case .thinking:
            let payload = proto.thinking
            return .thinking(ThinkingEvent(
                id: id,
                timestamp: timestamp,
                thinkingId: correlationId.isEmpty ? "thinking_\(id.uuidString)" : correlationId,
                content: payload.content,
                partial: partial  // partial flag from StreamEntry
            ))
            
        case .portExposed:
            let payload = proto.portExposed
            return .portExposed(PortExposedEvent(
                id: id,
                timestamp: timestamp,
                port: Int(payload.port),
                process: payload.hasProcess ? payload.process : nil,
                previewUrl: payload.hasPreviewURL ? payload.previewURL : nil
            ))
            
        case .repoClone:
            let payload = proto.repoClone
            let stage: RepoCloneStage
            switch payload.stage {
            case .starting: stage = .starting
            case .cloning: stage = .cloning
            case .error: stage = .error
            case .done, .unspecified, .UNRECOGNIZED: stage = .done
            }
            return .repoClone(RepoCloneEvent(
                id: id,
                timestamp: timestamp,
                repo: payload.repo,
                stage: stage,
                message: payload.message
            ))
            
        case .agentDisconnected:
            return .agentDisconnected(AgentDisconnectedEvent(
                id: id,
                timestamp: timestamp,
                message: "Agent connection lost. Send a message to continue when reconnected."
            ))
            
        case .agentReconnected:
            return .agentReconnected(AgentReconnectedEvent(
                id: id,
                timestamp: timestamp,
                message: "Agent reconnected. Send a message to continue."
            ))
            
        case .message, .UNRECOGNIZED, .unspecified:
            // MESSAGE kind is handled separately, return placeholder for unknown types
            return .thinking(ThinkingEvent(
                id: id,
                timestamp: timestamp,
                thinkingId: "unknown_\(id.uuidString)",
                content: "Unknown event type",
                partial: false
            ))
        }
    }
    
    /// Convert protobuf Struct to Swift dictionary.
    private func convertProtoStruct(_ protoStruct: SwiftProtobuf.Google_Protobuf_Struct) -> [String: AnyCodableValue] {
        var result: [String: AnyCodableValue] = [:]
        for (key, value) in protoStruct.fields {
            result[key] = convertProtoValue(value)
        }
        return result
    }
    
    /// Convert protobuf Value to AnyCodableValue.
    ///
    /// Note: Protobuf only has `double` for numeric types, so we use a heuristic to convert
    /// whole numbers to Int for better JSON compatibility. This means values like `1.0` will
    /// become Int(1). If you need to preserve the original double type, the source system
    /// should encode numbers as strings or use a different serialization format.
    private func convertProtoValue(_ value: SwiftProtobuf.Google_Protobuf_Value) -> AnyCodableValue {
        switch value.kind {
        case .nullValue:
            return .null
        case .numberValue(let num):
            // Heuristic: treat whole numbers within Int range as integers
            // This matches typical JSON number handling behavior
            if num.truncatingRemainder(dividingBy: 1) == 0 && num >= Double(Int.min) && num <= Double(Int.max) {
                return .int(Int(num))
            }
            return .double(num)
        case .stringValue(let str):
            return .string(str)
        case .boolValue(let bool):
            return .bool(bool)
        case .listValue(let list):
            return .array(list.values.map { convertProtoValue($0) })
        case .structValue(let structVal):
            return .dictionary(convertProtoStruct(structVal))
        case .none:
            return .null
        }
    }
    
    private func convertMessageRole(_ proto: Netclode_V1_MessageRole) -> PersistedMessage.ChatRole {
        switch proto {
        case .user: return .user
        case .assistant: return .assistant
        case .unspecified, .UNRECOGNIZED: return .user
        }
    }
    
    // MARK: - StreamEntry Conversion
    
    /// Convert unified StreamEntry array to separate messages and events arrays (for sessionState backward compat)
    private func convertStreamEntriesToMessagesAndEvents(_ entries: [Netclode_V1_StreamEntry], sessionId: String) -> ([PersistedMessage], [PersistedEvent]) {
        var messages: [PersistedMessage] = []
        var events: [PersistedEvent] = []
        
        for entry in entries {
            let timestamp = entry.hasTimestamp ? entry.timestamp.date : Date()
            
            switch entry.payload {
            case .event(let agentEvent):
                // MESSAGE kind becomes PersistedMessage, others become PersistedEvent
                if agentEvent.kind == .message {
                    let msgPayload = agentEvent.message
                    let message = PersistedMessage(
                        id: entry.id,
                        sessionId: sessionId,
                        role: convertMessageRole(msgPayload.role),
                        content: msgPayload.content,
                        timestamp: timestamp
                    )
                    messages.append(message)
                } else {
                    // Convert to PersistedEvent
                    let eventData = convertAgentEventToRaw(agentEvent, timestamp: timestamp)
                    let event = PersistedEvent(
                        id: entry.id,
                        sessionId: sessionId,
                        event: eventData,
                        timestamp: timestamp
                    )
                    events.append(event)
                }
                
            case .terminalOutput, .sessionUpdate, .error, .none:
                // These don't map to persisted messages/events
                break
            }
        }
        
        return (messages, events)
    }
    
    /// Convert a single StreamEntry to a ServerMessage for real-time streaming
    private func convertStreamEntryToServerMessage(_ sessionId: String, _ entry: Netclode_V1_StreamEntry) -> ServerMessage? {
        switch entry.payload {
        case .event(let agentEvent):
            switch agentEvent.kind {
            case .message:
                let msgPayload = agentEvent.message
                let role = msgPayload.role
                
                if role == .user {
                    return .userMessage(sessionId: sessionId, content: msgPayload.content)
                } else {
                    // Assistant message
                    return .agentMessage(
                        sessionId: sessionId,
                        content: msgPayload.content,
                        partial: entry.partial,
                        messageId: agentEvent.correlationID.isEmpty ? nil : agentEvent.correlationID
                    )
                }
                
            case .thinking, .toolStart, .toolInput, .toolOutput, .toolEnd, .portExposed, .repoClone, .agentDisconnected, .agentReconnected:
                let timestamp = entry.hasTimestamp ? entry.timestamp.date : Date()
                let event = convertAgentEvent(agentEvent, timestamp: timestamp, partial: entry.partial)
                return .agentEvent(sessionId: sessionId, event: event)
                
            case .unspecified, .UNRECOGNIZED:
                return nil
            }
            
        case .terminalOutput(let termOut):
            return .terminalOutput(sessionId: sessionId, data: termOut.data)
            
        case .sessionUpdate(let session):
            return .sessionUpdated(session: convertSession(session))
            
        case .error(let err):
            if err.hasSessionID {
                return .agentError(sessionId: err.sessionID, error: err.message)
            } else {
                return .error(message: err.message)
            }
            
        case .none:
            return nil
        }
    }
    
    /// Convert AgentEvent to raw data for persistence
    /// Note: timestamp comes from the parent StreamEntry, passed as parameter
    private func convertAgentEventToRaw(_ proto: Netclode_V1_AgentEvent, timestamp: Date = Date()) -> PersistedEvent.RawAgentEventData {
        let kind: String
        var tool: String? = nil
        var toolUseId: String? = nil
        var parentToolUseId: String? = nil
        var input: [String: AnyCodableValue]? = nil
        var inputDelta: String? = nil
        var result: String? = nil
        let path: String? = nil      // file_change events no longer exist in new proto
        let action: String? = nil    // file_change events no longer exist in new proto
        let linesAdded: Int? = nil   // file_change events no longer exist in new proto
        let linesRemoved: Int? = nil // file_change events no longer exist in new proto
        let command: String? = nil   // command events no longer exist in new proto
        let cwd: String? = nil       // command events no longer exist in new proto
        let exitCode: Int? = nil     // command events no longer exist in new proto
        let output: String? = nil    // command events no longer exist in new proto
        var content: String? = nil
        var thinkingId: String? = nil
        var partial: Bool? = nil
        var port: Int? = nil
        var process: String? = nil
        var previewUrl: String? = nil
        var repo: String? = nil
        var stage: String? = nil
        var message: String? = nil
        var error: String? = nil
        var durationMs: Int64? = nil
        
        // Use correlationID as the tool/thinking ID
        let correlationId = proto.correlationID
        
        switch proto.kind {
        case .toolStart:
            kind = "tool_start"
            let payload = proto.toolStart
            tool = payload.tool
            toolUseId = correlationId
            parentToolUseId = payload.hasParentToolUseID ? payload.parentToolUseID : nil
            
        case .toolInput:
            let payload = proto.toolInput
            if payload.hasDelta {
                kind = "tool_input"
                inputDelta = payload.delta
            } else {
                kind = "tool_input_complete"
                input = payload.hasInput ? convertProtoStruct(payload.input) : nil
            }
            toolUseId = correlationId
            
        case .toolOutput:
            kind = "tool_output"
            let payload = proto.toolOutput
            if payload.hasDelta {
                result = payload.delta
            } else if payload.hasOutput {
                result = payload.output
            }
            toolUseId = correlationId
            
        case .toolEnd:
            kind = "tool_end"
            let payload = proto.toolEnd
            toolUseId = correlationId
            result = payload.hasResult ? payload.result : nil
            error = payload.hasError ? payload.error : nil
            durationMs = payload.hasDurationMs ? payload.durationMs : nil
            
        case .thinking:
            kind = "thinking"
            let payload = proto.thinking
            thinkingId = correlationId
            content = payload.content
            partial = payload.partial
            
        case .portExposed:
            kind = "port_exposed"
            let payload = proto.portExposed
            port = Int(payload.port)
            process = payload.hasProcess ? payload.process : nil
            previewUrl = payload.hasPreviewURL ? payload.previewURL : nil
            
        case .repoClone:
            kind = "repo_clone"
            let payload = proto.repoClone
            repo = payload.repo
            switch payload.stage {
            case .starting: stage = "starting"
            case .cloning: stage = "cloning"
            case .error: stage = "error"
            case .done, .unspecified, .UNRECOGNIZED: stage = "done"
            }
            message = payload.message
            
        case .agentDisconnected:
            kind = "agent_disconnected"
            message = "Agent connection lost. Send a message to continue when reconnected."
            
        case .agentReconnected:
            kind = "agent_reconnected"
            message = "Agent reconnected. Send a message to continue."
            
        case .message, .unspecified, .UNRECOGNIZED:
            kind = "unknown"
        }
        
        return PersistedEvent.RawAgentEventData(
            kind: kind,
            timestamp: timestamp,
            tool: tool, toolUseId: toolUseId, parentToolUseId: parentToolUseId, input: input, inputDelta: inputDelta, result: result, durationMs: durationMs,
            path: path, action: action, linesAdded: linesAdded, linesRemoved: linesRemoved,
            command: command, cwd: cwd, exitCode: exitCode, output: output,
            content: content, thinkingId: thinkingId, partial: partial,
            port: port, process: process, previewUrl: previewUrl,
            repo: repo, stage: stage, message: message, error: error
        )
    }
    
    private func convertGitHubRepo(_ proto: Netclode_V1_GitHubRepo) -> GitHubRepo {
        GitHubRepo(
            name: proto.name,
            fullName: proto.fullName,
            isPrivate: proto.`private`,
            description: proto.hasDescription_p ? proto.description_p : nil
        )
    }
    
    private func convertGitFileChange(_ proto: Netclode_V1_GitFileChange) -> GitFileChange {
        GitFileChange(
            path: proto.path,
            status: convertGitFileStatus(proto.status),
            staged: proto.staged,
            linesAdded: proto.hasLinesAdded ? Int(proto.linesAdded) : nil,
            linesRemoved: proto.hasLinesRemoved ? Int(proto.linesRemoved) : nil,
            repo: proto.repo
        )
    }
    
    private func convertGitFileStatus(_ proto: Netclode_V1_GitFileStatus) -> GitFileStatus {
        switch proto {
        case .modified: return .modified
        case .added: return .added
        case .deleted: return .deleted
        case .renamed: return .renamed
        case .untracked: return .untracked
        case .copied: return .copied
        case .ignored: return .ignored
        case .unmerged: return .unmerged
        case .UNRECOGNIZED, .unspecified:
            // Log unknown status for debugging
            return .modified
        }
    }
    
    // MARK: - Handle Disconnection
    
    private func handleDisconnection(reason: DisconnectReason = .serverError("Connection lost")) async {
        // If network-initiated reconnection is in progress, don't interfere
        if isNetworkReconnecting {
            logger.info("Network reconnection in progress, skipping handleDisconnection")
            return
        }
        
        connectionState = .disconnected(reason: reason)
        receiveTask?.cancel()
        keepAliveTask?.cancel()
        stream = nil
        
        guard !serverURL.isEmpty else { return }
        
        // Don't auto-reconnect if reason doesn't support it
        guard connectionState.shouldAttemptReconnect else { return }
        
        // Check network availability before attempting reconnection
        if let networkMonitor = networkMonitor, !networkMonitor.currentState.isConnected {
            logger.info("Network unavailable, skipping reconnection")
            connectionState = .disconnected(reason: .networkLost)
            return
        }
        
        // Cancel any existing reconnect task
        reconnectTask?.cancel()
        
        // Start reconnection in a detached task to avoid blocking the main actor
        reconnectTask = Task.detached { [weak self] in
            await self?.performReconnection()
        }
    }
    
    /// Perform reconnection attempts with network-aware exponential backoff.
    private func performReconnection() async {
        for attempt in 1...strategy.maxAttempts {
            guard !Task.isCancelled else {
                logger.info("Reconnection cancelled")
                return
            }
            
            // Check network before attempting (on main actor for networkMonitor access)
            let hasNetwork = await MainActor.run {
                self.networkMonitor?.currentState.isConnected ?? true
            }
            
            guard hasNetwork else {
                logger.info("No network, pausing reconnection attempts")
                await MainActor.run {
                    self.connectionState = .disconnected(reason: .networkLost)
                }
                return
            }
            
            await MainActor.run {
                self.connectionState = .reconnecting(attempt: attempt, maxAttempts: self.strategy.maxAttempts)
            }
            logger.info("Reconnect attempt \(attempt)/\(self.strategy.maxAttempts)")
            
            // Use strategy for delay calculation
            let delay = await MainActor.run {
                self.strategy.delay(for: attempt, isForeground: self.isForeground)
            }
            try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            
            guard !Task.isCancelled else { return }
            
            await performConnect(attempt: attempt)
            
            let isConnected = connectionState == .connected
            if isConnected {
                logger.info("Reconnected successfully on attempt \(attempt)")
                return
            }
        }
        
        logger.warning("Max reconnect attempts (\(self.strategy.maxAttempts)) reached")
        await MainActor.run {
            self.connectionState = .disconnected(reason: .serverError("Max reconnection attempts exceeded"))
        }
    }
    
    func disconnect(reason: DisconnectReason = .userInitiated) {
        reconnectTask?.cancel()
        receiveTask?.cancel()
        keepAliveTask?.cancel()
        stream = nil
        client = nil
        serviceClient = nil
        connectionState = .disconnected(reason: reason)
    }
    
    func ensureConnected(to serverURL: String, connectPort: String = "") {
        self.serverURL = serverURL
        self.connectPortOverride = connectPort
        
        switch connectionState {
        case .connected:
            // Verify connection is alive with a lightweight sync
            send(.sync)
        case .disconnected, .suspended:
            connect(to: serverURL, connectPort: connectPort)
        case .connecting, .reconnecting:
            // Already trying
            break
        }
    }
    
    /// Waits for connection to establish, with timeout.
    /// Returns true if connected, false if timed out or failed.
    func waitForConnection(timeout: TimeInterval = 15) async -> Bool {
        // Already connected
        if connectionState.isConnected {
            return true
        }
        
        // Poll for connection state changes (works with @Observable)
        let startTime = Date()
        while Date().timeIntervalSince(startTime) < timeout {
            if connectionState.isConnected {
                return true
            }
            // Give up if we've stopped trying (disconnected without reconnecting)
            if case .disconnected = connectionState {
                return false
            }
            if case .suspended = connectionState {
                return false
            }
            // Short poll interval
            try? await Task.sleep(nanoseconds: 100_000_000) // 100ms
        }
        return false
    }
    
    // MARK: - Send Messages
    
    func send(_ message: ClientMessage) {
        // Allow sending during .connecting state for the initial sync message
        guard connectionState == .connected || connectionState == .connecting, let stream = stream else {
            logger.warning("send: dropped message (not connected): \(String(describing: message))")
            return
        }

        recordActivity()
        let protoMessage = convertToProtoMessage(message)
        
        Task {
            do {
                try await stream.send(protoMessage)
            } catch {
                logger.error("Failed to send message: \(error.localizedDescription)")
                // If send fails, the stream may be broken - trigger reconnection
                if !Task.isCancelled {
                    await handleDisconnection()
                }
            }
        }
    }

    private func startKeepAlive() {
        keepAliveTask?.cancel()
        keepAliveTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: self?.keepAliveInterval ?? 30_000_000_000)

                guard let self, !Task.isCancelled else { return }
                guard self.connectionState == .connected else { continue }

                let idleTime = Date().timeIntervalSince(self.lastActivityAt)
                if idleTime >= self.keepAliveIdleThreshold {
                    self.send(.sync)
                }
            }
        }
    }

    private func recordActivity() {
        lastActivityAt = Date()
    }
    
    private func convertToProtoMessage(_ message: ClientMessage) -> Netclode_V1_ClientMessage {
        var proto = Netclode_V1_ClientMessage()
        
        switch message {
        case .sync:
            proto.message = .sync(Netclode_V1_SyncRequest())
            
        case .sessionCreate(let name, let repos, let repoAccess, let initialPrompt, let sdkType, let model, let copilotBackend, let networkConfig, let resources):
            var req = Netclode_V1_CreateSessionRequest()
            if let name = name {
                req.name = name
            }
            if let repos = repos, !repos.isEmpty {
                req.repos = repos
            }
            if let repoAccess = repoAccess, let repos = repos, !repos.isEmpty {
                req.repoAccess = convertToProtoRepoAccess(repoAccess)
            }
            if let initialPrompt = initialPrompt {
                req.initialPrompt = initialPrompt
            }
            if let sdkType = sdkType {
                req.sdkType = convertToProtoSdkType(sdkType)
            }
            if let model = model {
                req.model = model
            }
            if let copilotBackend = copilotBackend {
                req.copilotBackend = convertToProtoCopilotBackend(copilotBackend)
            }
            if let networkConfig = networkConfig {
                var protoNetworkConfig = Netclode_V1_NetworkConfig()
                protoNetworkConfig.tailnetAccess = networkConfig.tailnetAccess
                req.networkConfig = protoNetworkConfig
            }
            if let resources = resources {
                var protoResources = Netclode_V1_SandboxResources()
                protoResources.vcpus = resources.vcpus
                protoResources.memoryMb = resources.memoryMB
                req.resources = protoResources
            }
            proto.message = .createSession(req)
            
        case .sessionList:
            proto.message = .listSessions(Netclode_V1_ListSessionsRequest())
            
        case .sessionResume(let id):
            var req = Netclode_V1_ResumeSessionRequest()
            req.sessionID = id
            proto.message = .resumeSession(req)
            
        case .sessionPause(let id):
            var req = Netclode_V1_PauseSessionRequest()
            req.sessionID = id
            proto.message = .pauseSession(req)
            
        case .sessionDelete(let id):
            var req = Netclode_V1_DeleteSessionRequest()
            req.sessionID = id
            proto.message = .deleteSession(req)
            
        case .sessionDeleteAll:
            proto.message = .deleteAllSessions(Netclode_V1_DeleteAllSessionsRequest())
            
        case .prompt(let sessionId, let text):
            var req = Netclode_V1_SendPromptRequest()
            req.sessionID = sessionId
            req.text = text
            proto.message = .sendPrompt(req)
            
        case .promptInterrupt(let sessionId):
            var req = Netclode_V1_InterruptPromptRequest()
            req.sessionID = sessionId
            proto.message = .interruptPrompt(req)
            
        case .terminalInput(let sessionId, let data):
            var req = Netclode_V1_TerminalInputRequest()
            req.sessionID = sessionId
            req.data = data
            proto.message = .terminalInput(req)
            
        case .terminalResize(let sessionId, let cols, let rows):
            var req = Netclode_V1_TerminalResizeRequest()
            req.sessionID = sessionId
            req.cols = Int32(cols)
            req.rows = Int32(rows)
            proto.message = .terminalResize(req)
            
        case .portExpose(let sessionId, let port):
            var req = Netclode_V1_ExposePortRequest()
            req.sessionID = sessionId
            req.port = Int32(port)
            proto.message = .exposePort(req)
            
        case .sessionOpen(let id, let lastMessageId, let lastNotificationId):
            var req = Netclode_V1_OpenSessionRequest()
            req.sessionID = id
            // Use lastNotificationId as afterStreamID if available, else use lastMessageId
            if let afterStreamId = lastNotificationId ?? lastMessageId {
                req.afterStreamID = afterStreamId
            }
            proto.message = .openSession(req)
            
        case .githubReposList:
            proto.message = .listGithubRepos(Netclode_V1_ListGitHubReposRequest())
            
        case .gitStatus(let sessionId):
            var req = Netclode_V1_GitStatusRequest()
            req.sessionID = sessionId
            proto.message = .gitStatus(req)
            
        case .gitDiff(let sessionId, let file):
            var req = Netclode_V1_GitDiffRequest()
            req.sessionID = sessionId
            if let file = file {
                req.file = file
            }
            proto.message = .gitDiff(req)

        case .listModels(let sdkType, let copilotBackend):
            var req = Netclode_V1_ListModelsRequest()
            req.sdkType = convertToProtoSdkType(sdkType)
            if let copilotBackend = copilotBackend {
                req.copilotBackend = convertToProtoCopilotBackend(copilotBackend)
            }
            proto.message = .listModels(req)

        case .getCopilotStatus:
            proto.message = .getCopilotStatus(Netclode_V1_GetCopilotStatusRequest())

        case .listSnapshots(let sessionId):
            var req = Netclode_V1_ListSnapshotsRequest()
            req.sessionID = sessionId
            proto.message = .listSnapshots(req)

        case .restoreSnapshot(let sessionId, let snapshotId):
            var req = Netclode_V1_RestoreSnapshotRequest()
            req.sessionID = sessionId
            req.snapshotID = snapshotId
            proto.message = .restoreSnapshot(req)

        case .updateRepoAccess(let sessionId, let repoAccess):
            var req = Netclode_V1_UpdateRepoAccessRequest()
            req.sessionID = sessionId
            req.repoAccess = convertToProtoRepoAccess(repoAccess)
            proto.message = .updateRepoAccess(req)

        case .getResourceLimits:
            proto.message = .getResourceLimits(Netclode_V1_GetResourceLimitsRequest())
        }
        
        return proto
    }
    
    // MARK: - Session Operations
    
    func openSession(id: String, lastMessageId: String? = nil, lastNotificationId: String? = nil, resume: Bool = true) {
        print("[Connect] openSession called for \(id), connectionState=\(connectionState), resume=\(resume)")
        send(.sessionOpen(id: id, lastMessageId: lastMessageId, lastNotificationId: lastNotificationId))
        if resume {
            send(.sessionResume(id: id))
        }
    }
    
    // MARK: - Lifecycle Management
    
    /// Prepare for app backgrounding - close streams gracefully
    func prepareForBackground() {
        logger.info("Preparing for background")
        isForeground = false
        
        // Cancel tasks
        reconnectTask?.cancel()
        keepAliveTask?.cancel()
        
        // Close stream gracefully (server will keep session alive)
        receiveTask?.cancel()
        stream = nil
        
        connectionState = .suspended
    }
    
    /// Restore connection when app foregrounds
    func restoreFromBackground() {
        logger.info("Restoring from background")
        isForeground = true
        
        guard case .suspended = connectionState else {
            // May have been disconnected for other reasons
            if case .disconnected = connectionState {
                Task { await reconnectImmediately() }
            }
            return
        }
        
        connectionState = .disconnected(reason: .backgrounded)
        
        Task {
            await reconnectImmediately()
        }
    }
    
    // MARK: - Network-Aware Reconnection
    
    /// Handle network state changes from NetworkMonitor
    func handleNetworkTransition(_ transition: NetworkMonitor.NetworkTransition) {
        logger.info("Handling network transition: \(transition.from.description) → \(transition.to.description)")
        
        if transition.isDisconnection {
            // Network lost - disconnect cleanly, don't retry
            isNetworkReconnecting = false
            disconnect(reason: .networkLost)
        } else if transition.isReconnection {
            // Network restored - attempt immediate reconnection
            guard !isNetworkReconnecting else {
                logger.info("Already reconnecting, skipping duplicate attempt")
                return
            }
            isNetworkReconnecting = true
            Task {
                defer { isNetworkReconnecting = false }
                await reconnectImmediately()
            }
        } else if transition.isInterfaceChange {
            // WiFi ↔ Cellular - proactive reconnection
            // The old connection may still work briefly, but will likely fail
            guard !isNetworkReconnecting else {
                logger.info("Already reconnecting, skipping interface change reconnection")
                return
            }
            isNetworkReconnecting = true
            logger.info("Network interface changed, initiating proactive reconnection")
            Task {
                defer { isNetworkReconnecting = false }
                await reconnectWithNewInterface()
            }
        }
    }
    
    /// Immediate reconnection for network restoration or foregrounding
    func reconnectImmediately() async {
        // Only reconnect if disconnected and network is available
        switch connectionState {
        case .disconnected, .suspended:
            break
        default:
            return
        }
        
        guard networkMonitor?.currentState.isConnected ?? true else {
            logger.info("No network available for immediate reconnection")
            return
        }
        
        guard !serverURL.isEmpty else { return }
        
        connectionState = .connecting
        
        await performConnect()
        
        if connectionState.isConnected {
            logger.info("Immediate reconnection successful")
        } else {
            // Start regular reconnection loop if immediate fails
            startReconnectionLoop()
        }
    }
    
    /// Reconnect when network interface changes (WiFi ↔ Cellular)
    private func reconnectWithNewInterface() async {
        logger.info("Reconnecting due to network interface change")
        
        // Cancel any existing reconnection attempts
        reconnectTask?.cancel()
        
        // Close existing connection gracefully
        receiveTask?.cancel()
        keepAliveTask?.cancel()
        stream = nil
        client = nil
        serviceClient = nil
        
        // Update state to allow reconnection
        connectionState = .disconnected(reason: .networkLost)
        
        // Brief delay for new interface to stabilize
        try? await Task.sleep(nanoseconds: 500_000_000) // 0.5s
        
        // Check if network is still available after delay
        guard networkMonitor?.currentState.isConnected ?? true else {
            logger.info("Network no longer available after interface change")
            return
        }
        
        // Attempt immediate reconnection
        await performConnect()
        
        if connectionState.isConnected {
            logger.info("Successfully reconnected after interface change")
        } else {
            // Start reconnection loop if immediate attempt fails
            logger.info("Immediate reconnection failed, starting retry loop")
            startReconnectionLoop()
        }
    }
    
    /// Start the reconnection loop
    private func startReconnectionLoop() {
        reconnectTask?.cancel()
        
        reconnectTask = Task.detached { [weak self] in
            await self?.performReconnection()
        }
    }
}
