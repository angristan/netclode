import Foundation
import SwiftUI

@MainActor
@Observable
final class MessageRouter {
    private let webSocketService: WebSocketService
    private let sessionStore: SessionStore
    private let chatStore: ChatStore
    private let eventStore: EventStore
    private let terminalStore: TerminalStore

    private var routingTask: Task<Void, Never>?

    init(
        webSocketService: WebSocketService,
        sessionStore: SessionStore,
        chatStore: ChatStore,
        eventStore: EventStore,
        terminalStore: TerminalStore
    ) {
        self.webSocketService = webSocketService
        self.sessionStore = sessionStore
        self.chatStore = chatStore
        self.eventStore = eventStore
        self.terminalStore = terminalStore

        startRouting()
    }

    private func startRouting() {
        routingTask = Task { [weak self] in
            guard let self else { return }

            for await message in webSocketService.messages {
                self.route(message)
            }
        }
    }

    private func route(_ message: ServerMessage) {
        switch message {
        // Session messages
        case .sessionCreated(let session):
            sessionStore.addSession(session)
            
            // If there's a pending prompt, associate it with this session and navigate
            if sessionStore.pendingPromptText != nil {
                sessionStore.pendingSessionId = session.id
            }

        case .sessionUpdated(let session):
            sessionStore.updateSession(session)

        case .sessionDeleted(let id):
            withAnimation {
                sessionStore.removeSession(id: id)
            }
            chatStore.clearMessages(for: id)
            eventStore.clearEvents(for: id)
            terminalStore.clearOutput(for: id)

        case .sessionList(let sessions):
            sessionStore.setSessions(sessions)

        case .sessionError(let id, let error):
            print("Session error \(id ?? "unknown"): \(error)")
            if let id {
                sessionStore.setError(for: id, error: error)
            }

        // Agent messages
        case .agentMessage(let sessionId, let content, let partial):
            if partial {
                chatStore.appendAssistantPartial(sessionId: sessionId, delta: content)
            } else {
                chatStore.appendMessage(
                    sessionId: sessionId,
                    message: ChatMessage(role: .assistant, content: content)
                )
            }
            sessionStore.setProcessing(for: sessionId, processing: true)

        case .agentEvent(let sessionId, let event):
            // Handle thinking events specially for streaming
            if case .thinking(let thinkingEvent) = event {
                if thinkingEvent.partial {
                    // Streaming thinking - accumulate content
                    eventStore.appendThinkingPartial(
                        sessionId: sessionId,
                        thinkingId: thinkingEvent.thinkingId,
                        content: thinkingEvent.content,
                        timestamp: thinkingEvent.timestamp
                    )
                } else {
                    // Complete thinking block - add as final event
                    eventStore.appendEvent(sessionId: sessionId, event: event)
                }
            } else {
                eventStore.appendEvent(sessionId: sessionId, event: event)
            }

        case .agentDone(let sessionId):
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

        // Sync responses
        case .syncResponse(let sessions, _):
            // Update sessions from server sync
            sessionStore.setSessions(sessions.map { $0.toSession() })

        case .sessionState(let session, let messages, let events, _, let lastNotificationId):
            // Load session history from server
            print("[MessageRouter] session.state received: \(messages.count) messages, \(events.count) events for session \(session.id)")
            sessionStore.updateSession(session)
            chatStore.loadMessages(sessionId: session.id, messages: messages)
            eventStore.loadEvents(sessionId: session.id, events: events)
            // Store the notification cursor for reconnection
            if let notificationId = lastNotificationId {
                sessionStore.setLastNotificationId(for: session.id, notificationId: notificationId)
            }
            print("[MessageRouter] Loaded messages and events for session \(session.id)")
            
            // Send pending initial prompt after session state is loaded
            if let promptText = sessionStore.pendingPromptText,
               sessionStore.pendingSessionId == session.id {
                // Check if server already has our message (agent responded)
                let serverHasMessages = !messages.isEmpty
                
                if serverHasMessages {
                    // Agent already received and processed, clear pending state
                    print("[MessageRouter] Server has messages, clearing pending state for session \(session.id)")
                    sessionStore.pendingPromptText = nil
                    sessionStore.pendingSessionId = nil
                } else {
                    // Add user message to chat and send to agent
                    chatStore.appendMessage(
                        sessionId: session.id,
                        message: ChatMessage(role: .user, content: promptText)
                    )
                    webSocketService.send(.prompt(sessionId: session.id, text: promptText))
                    print("[MessageRouter] Sent initial prompt for session \(session.id)")
                    // DON'T clear pending state yet - wait for agent response
                }
            }
        }
    }

    func stop() {
        routingTask?.cancel()
    }

    static var preview: MessageRouter {
        MessageRouter(
            webSocketService: WebSocketService(),
            sessionStore: SessionStore(),
            chatStore: ChatStore(),
            eventStore: EventStore(),
            terminalStore: TerminalStore()
        )
    }
}
