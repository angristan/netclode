import Foundation
import SwiftUI

@MainActor
@Observable
final class MessageRouter {
    private let connectService: ConnectService
    private let sessionStore: SessionStore
    private let chatStore: ChatStore
    private let eventStore: EventStore
    private let terminalStore: TerminalStore
    private let githubStore: GitHubStore
    private let gitStore: GitStore
    private let modelsStore: UnifiedModelsStore
    private let snapshotStore: SnapshotStore
    private let settingsStore: SettingsStore

    private var routingTask: Task<Void, Never>?



    init(
        connectService: ConnectService,
        sessionStore: SessionStore,
        chatStore: ChatStore,
        eventStore: EventStore,
        terminalStore: TerminalStore,
        githubStore: GitHubStore,
        gitStore: GitStore,
        modelsStore: UnifiedModelsStore,
        snapshotStore: SnapshotStore,
        settingsStore: SettingsStore
    ) {
        self.connectService = connectService
        self.sessionStore = sessionStore
        self.chatStore = chatStore
        self.eventStore = eventStore
        self.terminalStore = terminalStore
        self.githubStore = githubStore
        self.gitStore = gitStore
        self.modelsStore = modelsStore
        self.snapshotStore = snapshotStore
        self.settingsStore = settingsStore

        startRouting()
    }

    private func startRouting() {
        routingTask = Task { [weak self] in
            guard let self else { return }

            for await message in connectService.messages {
                self.route(message)
            }
        }
    }

    /// Routes a server message to the appropriate store.
    /// Internal for testing - allows tests to call this directly with mock messages.
    func route(_ message: ServerMessage) {
        switch message {
        // Session messages
        case .sessionCreated(let session):
            print("[MessageRouter] session.created received: id=\(session.id), pendingPromptText=\(sessionStore.pendingPromptText ?? "nil")")
            sessionStore.addSession(session)
            sessionStore.clearPendingCreationError()

            // If there's a pending prompt, set up navigation and mark as processing
            // Note: The prompt itself is sent via initialPrompt in session.create,
            // and the backend will broadcast user.message which we handle separately
            if sessionStore.pendingPromptText != nil {
                sessionStore.pendingSessionId = session.id
                sessionStore.setProcessing(for: session.id, processing: true)
                print("[MessageRouter] Session created with initial prompt, navigating to session \(session.id)")
                sessionStore.pendingPromptText = nil
            }

        case .sessionUpdated(let session):
            print("[MessageRouter] session.updated received: id=\(session.id), name=\(session.name), status=\(session.status)")
            sessionStore.updateSession(session)
            
            // Handle status transitions
            if session.status == .running {
                // Session is processing - show streaming indicator
                sessionStore.setProcessing(for: session.id, processing: true)
            } else if session.status == .ready {
                // Session is done - stop processing indicator and finalize messages
                // This replaces the old agentDone handling in the unified streaming model
                if sessionStore.isProcessing(session.id) {
                    if settingsStore.hapticFeedbackEnabled {
                        HapticFeedback.success()
                    }
                    sessionStore.setProcessing(for: session.id, processing: false)
                    chatStore.finalizeLastMessage(sessionId: session.id)
                    // Clear pending state - agent has responded
                    if sessionStore.pendingSessionId == session.id {
                        sessionStore.pendingPromptText = nil
                        sessionStore.pendingSessionId = nil
                    }
                }
            }

        case .sessionDeleted(let id):
            withAnimation {
                sessionStore.removeSession(id: id)
            }
            chatStore.clearMessages(for: id)
            eventStore.clearEvents(for: id)
            terminalStore.clearOutput(for: id)

        case .sessionsDeletedAll(let deletedIds):
            print("[MessageRouter] sessions.deletedAll received: \(deletedIds.count) sessions deleted")
            withAnimation {
                sessionStore.removeAllSessions()
            }
            // Clear all chat, event, and terminal data
            for id in deletedIds {
                chatStore.clearMessages(for: id)
                eventStore.clearEvents(for: id)
                terminalStore.clearOutput(for: id)
            }

        case .sessionList(let sessions):
            sessionStore.setSessions(sessions)

        case .sessionError(let id, let error):
            print("Session error \(id ?? "unknown"): \(error)")
            if let id {
                sessionStore.setError(for: id, error: error)
            } else if sessionStore.pendingPromptText != nil {
                sessionStore.failPendingCreation(with: error)
            }

        // Agent messages
        case .agentMessage(let sessionId, let content, let partial, let messageId):
            print("[MessageRouter] agentMessage received: partial=\(partial), messageId=\(messageId ?? "nil"), contentLength=\(content.count), preview=\"\(String(content.prefix(50)))\"")
            if partial {
                chatStore.appendAssistantPartial(sessionId: sessionId, delta: content, messageId: messageId)
            } else {
                // Final message - only add if we don't already have an assistant message being built
                // (the content was already accumulated via partials)
                let existingMessages = chatStore.messages(for: sessionId)
                let hasStreamingAssistant = existingMessages.last?.role == .assistant
                if !hasStreamingAssistant {
                    // No streaming in progress - this is a complete message (no partials were sent)
                    chatStore.appendMessage(
                        sessionId: sessionId,
                        message: ChatMessage(role: .assistant, content: content)
                    )
                }
                // If hasStreamingAssistant, the content is already there from partials - nothing to do
            }

        case .agentEvent(let sessionId, let event):
            // Handle thinking events specially for streaming
            if case .thinking(let thinkingEvent) = event {
                // Check if this is actually tool output disguised as thinking
                // Tool output has thinkingId like "output_<toolUseId>"
                if thinkingEvent.thinkingId.hasPrefix("output_") {
                    let toolUseId = String(thinkingEvent.thinkingId.dropFirst("output_".count))
                    // Accumulate tool output for later merging into tool_end
                    eventStore.appendToolOutput(
                        sessionId: sessionId,
                        toolUseId: toolUseId,
                        output: thinkingEvent.content
                    )
                } else if thinkingEvent.partial {
                    // Streaming thinking - accumulate content
                    eventStore.appendThinkingPartial(
                        sessionId: sessionId,
                        thinkingId: thinkingEvent.thinkingId,
                        content: thinkingEvent.content,
                        timestamp: thinkingEvent.timestamp
                    )
                } else {
                    // Complete thinking block - finalize existing or add new if no partials were sent
                    let existingEvents = eventStore.events(for: sessionId)
                    let hasStreamingThinking = existingEvents.contains { e in
                        if case .thinking(let t) = e, t.thinkingId == thinkingEvent.thinkingId {
                            return true
                        }
                        return false
                    }
                    if hasStreamingThinking {
                        // Finalize the existing thinking event
                        eventStore.finalizeThinking(sessionId: sessionId, thinkingId: thinkingEvent.thinkingId)
                    } else {
                        // No partials were sent - add as complete event
                        eventStore.appendEvent(sessionId: sessionId, event: event)
                    }
                }
            } else if case .toolInput(let inputEvent) = event {
                // Accumulate streaming tool input delta
                eventStore.appendToolInputDelta(
                    sessionId: sessionId,
                    toolUseId: inputEvent.toolUseId,
                    inputDelta: inputEvent.inputDelta
                )
            } else if case .toolInputComplete(let inputEvent) = event {
                // Update existing tool_start event with full input
                eventStore.updateToolInput(
                    sessionId: sessionId,
                    toolUseId: inputEvent.toolUseId,
                    input: inputEvent.input
                )
            } else if case .toolEnd(let endEvent) = event {
                // Consume accumulated tool output and merge into result
                let accumulatedOutput = eventStore.consumeToolOutput(toolUseId: endEvent.toolUseId)
                if let output = accumulatedOutput, !output.isEmpty {
                    // Create updated tool_end with result
                    let updatedEvent = ToolEndEvent(
                        id: endEvent.id,
                        timestamp: endEvent.timestamp,
                        tool: endEvent.tool,
                        toolUseId: endEvent.toolUseId,
                        parentToolUseId: endEvent.parentToolUseId,
                        result: output,
                        error: endEvent.error,
                        durationMs: endEvent.durationMs
                    )
                    eventStore.appendEvent(sessionId: sessionId, event: .toolEnd(updatedEvent))
                } else {
                    eventStore.appendEvent(sessionId: sessionId, event: event)
                }
            } else {
                eventStore.appendEvent(sessionId: sessionId, event: event)
            }

        case .agentDone(let sessionId):
            if settingsStore.hapticFeedbackEnabled {
                HapticFeedback.success()
            }
            sessionStore.setProcessing(for: sessionId, processing: false)
            chatStore.finalizeLastMessage(sessionId: sessionId)
            // Clear pending state - agent has responded
            if sessionStore.pendingSessionId == sessionId {
                sessionStore.pendingPromptText = nil
                sessionStore.pendingSessionId = nil
            }

        case .agentError(let sessionId, let error):
            sessionStore.setProcessing(for: sessionId, processing: false)
            chatStore.appendMessage(
                sessionId: sessionId,
                message: ChatMessage(role: .assistant, content: "Error: \(error)")
            )

        case .userMessage(let sessionId, let content):
            // User message from another client - add if not duplicate
            let messages = chatStore.messages(for: sessionId)
            if let lastMessage = messages.last,
               lastMessage.role == .user && lastMessage.content == content {
                // Skip duplicate (message was sent by this client)
                break
            }
            chatStore.appendMessage(
                sessionId: sessionId,
                message: ChatMessage(role: .user, content: content)
            )
            // Mark as processing - agent will start working on this message
            sessionStore.setProcessing(for: sessionId, processing: true)

        // Terminal messages
        case .terminalOutput(let sessionId, let data):
            terminalStore.appendOutput(sessionId: sessionId, data: data)

        // Port exposure responses (event comes via agent.event, this is just confirmation)
        case .portExposed(let sessionId, let port, let previewUrl):
            print("[MessageRouter] Port \(port) exposed for session \(sessionId): \(previewUrl)")

        case .portError(let sessionId, let port, let error):
            print("[MessageRouter] Failed to expose port \(port) for session \(sessionId): \(error)")

        // General errors
        case .error(let message):
            print("Server error: \(message)")
            if sessionStore.pendingPromptText != nil {
                sessionStore.failPendingCreation(with: message)
            }
            // Notify GitHubStore in case it's waiting for a response
            if githubStore.isLoading {
                githubStore.handleError(message)
            }

        // Sync responses
        case .syncResponse(let sessions, _):
            // Update sessions from server sync
            sessionStore.setSessions(sessions.map { $0.toSession() })

        case .sessionState(let session, let messages, let events, _, let lastNotificationId):
            print("[MessageRouter] session.state: status=\(session.status), \(messages.count) messages, \(events.count) events")
            
            sessionStore.updateSession(session)
            
            // Set processing state based on session status
            sessionStore.setProcessing(for: session.id, processing: session.status == .running)
            
            // Replace local state with server state
            chatStore.loadMessages(sessionId: session.id, messages: messages)
            eventStore.loadEvents(sessionId: session.id, events: events)
            
            if let notificationId = lastNotificationId {
                sessionStore.setLastNotificationId(for: session.id, notificationId: notificationId)
            }
            
            // Only load from server if session is NOT currently streaming
            // (if streaming, real-time events are populating local state)
            if session.status != .running {
                chatStore.loadMessages(sessionId: session.id, messages: messages)
                eventStore.loadEvents(sessionId: session.id, events: events)
            }
            
            if let notificationId = lastNotificationId {
                sessionStore.setLastNotificationId(for: session.id, notificationId: notificationId)
            }

        // GitHub messages
        case .githubRepos(let repos):
            print("[MessageRouter] github.repos received: \(repos.count) repos")
            githubStore.handleReposReceived(repos)

        // Git operations
        case .gitStatusResponse(let sessionId, let files):
            print("[MessageRouter] git.status received: \(files.count) files for session \(sessionId)")
            gitStore.setLoadingStatus(false, for: sessionId)
            gitStore.setFiles(files, for: sessionId)

        case .gitDiffResponse(let sessionId, let diff):
            print("[MessageRouter] git.diff received: \(diff.count) chars for session \(sessionId)")
            gitStore.setLoadingDiff(false, for: sessionId)
            gitStore.setDiff(diff, for: sessionId)

        case .gitError(let sessionId, let error):
            print("[MessageRouter] git.error for session \(sessionId): \(error)")
            gitStore.setLoadingStatus(false, for: sessionId)
            gitStore.setLoadingDiff(false, for: sessionId)
            gitStore.setError(error, for: sessionId)

        // Models messages (all SDK types via unified store)
        case .modelsResponse(let models, let sdkType):
            print("[MessageRouter] models received: \(models.count) models, sdkType=\(String(describing: sdkType))")
            if let sdkType = sdkType {
                modelsStore.updateModels(models, sdkType: sdkType)
            }

        case .copilotStatusResponse(let status):
            print("[MessageRouter] copilot status received: authenticated=\(status.auth.isAuthenticated)")
            modelsStore.updateCopilotStatus(status)

        // Snapshot messages
        case .snapshotCreated(let sessionId, let snapshot):
            print("[MessageRouter] snapshot.created received: id=\(snapshot.id) for session \(sessionId)")
            withAnimation(.smooth) {
                snapshotStore.addSnapshot(snapshot)
            }

        case .snapshotList(let sessionId, let snapshots):
            print("[MessageRouter] snapshot.list received: \(snapshots.count) snapshots for session \(sessionId)")
            snapshotStore.setSnapshots(for: sessionId, snapshots: snapshots)

        case .snapshotRestored(let sessionId, let snapshotId, let messageCount):
            print("[MessageRouter] snapshot.restored received: snapshot=\(snapshotId) for session \(sessionId), messageCount=\(messageCount)")
            snapshotStore.setRestoreInProgress(for: sessionId, inProgress: false)
            // Request session refresh to reload truncated messages and events from server
            connectService.openSession(id: sessionId, resume: false)

        // Repo access
        case .repoAccessUpdated(let sessionId, let repoAccess):
            print("[MessageRouter] repo.access.updated received: session=\(sessionId), repoAccess=\(repoAccess)")
            // Update local state immediately (don't wait for Redis notification)
            sessionStore.updateRepoAccess(sessionId: sessionId, repoAccess: repoAccess)

        // Resource limits
        case .resourceLimitsResponse(let limits):
            print("[MessageRouter] resource.limits received: maxVcpus=\(limits.maxVcpus), maxMemoryMB=\(limits.maxMemoryMB)")
            modelsStore.updateResourceLimits(limits)
        }
    }

    func stop() {
        routingTask?.cancel()
    }

    static var preview: MessageRouter {
        MessageRouter(
            connectService: ConnectService(),
            sessionStore: SessionStore(),
            chatStore: ChatStore(),
            eventStore: EventStore(),
            terminalStore: TerminalStore(),
            githubStore: GitHubStore(),
            gitStore: GitStore(),
            modelsStore: UnifiedModelsStore(),
            snapshotStore: SnapshotStore(),
            settingsStore: SettingsStore()
        )
    }
}
