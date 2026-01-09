import SwiftUI

// MARK: - Timeline Item

/// Represents an item in the unified chat timeline
enum TimelineItem: Identifiable {
    case message(ChatMessage, isStreaming: Bool, turnDuration: TimeInterval?)
    case event(GroupedEvent)

    var id: UUID {
        switch self {
        case .message(let msg, _, _): return msg.id
        case .event(let grouped): return grouped.id
        }
    }

    var timestamp: Date {
        switch self {
        case .message(let msg, _, _): return msg.timestamp
        case .event(let grouped): return grouped.timestamp
        }
    }
}

/// A grouped event combining start/end events
struct GroupedEvent: Identifiable {
    let id: UUID
    let event: AgentEvent
    let timestamp: Date
    var endEvent: AgentEvent?
}

// MARK: - Chat View

struct ChatView: View {
    let sessionId: String

    @Environment(ChatStore.self) private var chatStore
    @Environment(EventStore.self) private var eventStore
    @Environment(SessionStore.self) private var sessionStore
    @Environment(WebSocketService.self) private var webSocketService
    @Environment(SettingsStore.self) private var settingsStore

    @State private var inputText = ""
    @FocusState private var isInputFocused: Bool

    // Cached timeline to avoid recomputing on every view update
    @State private var cachedTimeline: [TimelineItem] = []
    @State private var timelineVersion: Int = 0

    var messages: [ChatMessage] {
        chatStore.messages(for: sessionId)
    }

    var events: [AgentEvent] {
        eventStore.events(for: sessionId)
    }

    var isProcessing: Bool {
        sessionStore.isProcessing(sessionId)
    }

    /// Unified timeline combining messages and events, sorted by timestamp
    private func computeTimeline() -> [TimelineItem] {
        var items: [TimelineItem] = []

        // Add messages with turn duration calculation
        var precedingUserTimestamp: Date?
        for (index, message) in messages.enumerated() {
            let isLastAssistant = message.role == .assistant && index == messages.count - 1
            let isStreaming = isLastAssistant && isProcessing

            var turnDuration: TimeInterval? = nil

            if message.role == .user {
                precedingUserTimestamp = message.timestamp
            } else if message.role == .assistant, !isStreaming, let userTime = precedingUserTimestamp {
                // Find the end of this turn (must be after user message, before next user message)
                // Use a small buffer after the message timestamp to catch final events
                let messageEndBound = message.timestamp.addingTimeInterval(2.0)

                // Find next user message to bound this turn (if any)
                let nextUserTime = messages[(index + 1)...].first { $0.role == .user }?.timestamp

                // Use the earlier of: next user message or message end bound
                let upperBound = nextUserTime.map { min($0, messageEndBound) } ?? messageEndBound

                // Find the last event within this turn window
                let turnEndTime = events.last { $0.timestamp > userTime && $0.timestamp <= upperBound }?.timestamp

                // Fall back to the message's own timestamp if no events found
                let endTime = turnEndTime ?? message.timestamp
                turnDuration = endTime.timeIntervalSince(userTime)
            }

            items.append(.message(message, isStreaming: isStreaming, turnDuration: turnDuration))
        }

        // Add grouped events
        let groupedEvents = groupEvents(events)
        for grouped in groupedEvents {
            items.append(.event(grouped))
        }

        // Sort by timestamp
        return items.sorted { $0.timestamp < $1.timestamp }
    }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(spacing: Theme.Spacing.md) {
                    ForEach(cachedTimeline) { item in
                        timelineItemView(item)
                            .id(item.id)
                    }

                    // Streaming indicator (shows at end when processing)
                    if isProcessing {
                        StreamingIndicator()
                            .id("streaming")
                    }

                    // Scroll anchor
                    Color.clear
                        .frame(height: 1)
                        .id("bottom")
                }
                .padding()
            }
            .scrollDismissesKeyboard(.interactively)
            .onChange(of: messages.count) {
                updateTimelineIfNeeded()
                withAnimation(.glassSpring) {
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
            }
            .onChange(of: events.count) {
                updateTimelineIfNeeded()
                withAnimation(.glassSpring) {
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
            }
            .onChange(of: isProcessing) {
                updateTimelineIfNeeded()
                withAnimation(.glassSpring) {
                    proxy.scrollTo("bottom", anchor: .bottom)
                }
            }
            .onAppear {
                updateTimelineIfNeeded()
                proxy.scrollTo("bottom", anchor: .bottom)
            }
            .task(id: sessionId) {
                // Recompute timeline when session changes
                updateTimelineIfNeeded()
            }
        }
        .safeAreaInset(edge: .bottom) {
            ChatInputBar(
                text: $inputText,
                isProcessing: isProcessing,
                isFocused: $isInputFocused,
                onSend: sendMessage,
                onInterrupt: interruptAgent
            )
        }
    }

    @ViewBuilder
    private func timelineItemView(_ item: TimelineItem) -> some View {
        switch item {
        case .message(let message, let isStreaming, let turnDuration):
            ChatMessageRow(message: message, isStreaming: isStreaming, turnDuration: turnDuration)
        case .event(let grouped):
            groupedEventView(grouped)
        }
    }

    @ViewBuilder
    private func groupedEventView(_ grouped: GroupedEvent) -> some View {
        switch grouped.event {
        case .toolStart:
            let endEvent: ToolEndEvent? = {
                if case .toolEnd(let e) = grouped.endEvent {
                    return e
                }
                return nil
            }()
            ToolEventCard(event: grouped.event, endEvent: endEvent)

        case .commandStart(let start):
            let endEvent: CommandEndEvent? = {
                if case .commandEnd(let e) = grouped.endEvent {
                    return e
                }
                return nil
            }()
            CommandEventCard(startEvent: start, endEvent: endEvent)

        case .fileChange(let e):
            FileChangeCard(event: e)

        case .thinking(let e):
            ThinkingCard(event: e)

        case .portExposed(let e):
            PortExposedCard(event: e)

        default:
            EmptyView()
        }
    }

    /// Groups related events (tool_start with tool_end, etc.)
    private func groupEvents(_ events: [AgentEvent]) -> [GroupedEvent] {
        var result: [GroupedEvent] = []
        var toolStartMap: [String: Int] = [:]

        for event in events {
            switch event {
            case .toolStart(let e):
                let grouped = GroupedEvent(id: e.id, event: event, timestamp: e.timestamp)
                toolStartMap[e.toolUseId] = result.count
                result.append(grouped)

            case .toolEnd(let e):
                if let index = toolStartMap[e.toolUseId] {
                    result[index].endEvent = event
                }

            case .toolInput:
                break // Skip tool input events

            case .commandStart(let e):
                result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))

            case .commandEnd(let e):
                if let index = result.lastIndex(where: {
                    if case .commandStart(let start) = $0.event {
                        return start.command == e.command && $0.endEvent == nil
                    }
                    return false
                }) {
                    result[index].endEvent = event
                }

            case .fileChange(let e):
                result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))

            case .thinking(let e):
                result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))

            case .portExposed(let e):
                result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))
            }
        }

        return result
    }

    private func sendMessage() {
        let text = inputText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }

        if settingsStore.hapticFeedbackEnabled {
            HapticFeedback.light()
        }

        // Add user message locally
        chatStore.appendMessage(
            sessionId: sessionId,
            message: ChatMessage(role: .user, content: text)
        )

        // Start processing state immediately (shows thinking animation)
        sessionStore.setProcessing(for: sessionId, processing: true)

        // Send to server
        webSocketService.send(.prompt(sessionId: sessionId, text: text))

        // Clear input
        inputText = ""
    }

    private func interruptAgent() {
        if settingsStore.hapticFeedbackEnabled {
            HapticFeedback.warning()
        }
        webSocketService.send(.promptInterrupt(sessionId: sessionId))
    }

    /// Update cached timeline only when data changes
    private func updateTimelineIfNeeded() {
        // Compute new version based on data state
        let newVersion = messages.count + events.count + (isProcessing ? 1000 : 0)
        guard newVersion != timelineVersion else { return }

        timelineVersion = newVersion
        cachedTimeline = computeTimeline()
    }
}

// MARK: - Expose Port Sheet

struct ExposePortSheet: View {
    @Binding var portText: String
    let onExpose: (Int) -> Void

    @Environment(\.dismiss) private var dismiss
    @Environment(SettingsStore.self) private var settingsStore

    @FocusState private var isInputFocused: Bool

    private var portNumber: Int? {
        Int(portText)
    }

    private var isValidPort: Bool {
        guard let port = portNumber else { return false }
        return port > 0 && port <= 65535
    }

    var body: some View {
        NavigationStack {
            VStack(spacing: Theme.Spacing.lg) {
                // Header
                VStack(spacing: Theme.Spacing.md) {
                    Image(systemName: "globe")
                        .font(.system(size: 48))
                        .foregroundStyle(
                            LinearGradient(
                                colors: [.cyan, .blue],
                                startPoint: .topLeading,
                                endPoint: .bottomTrailing
                            )
                        )

                    Text("Expose Port")
                        .font(.netclodeTitle)
                }
                .padding(.top, Theme.Spacing.lg)

                // Input
                VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
                    Text("Port Number")
                        .font(.netclodeSubheadline)
                        .foregroundStyle(.secondary)

                    GlassTextField(
                        "8080",
                        text: $portText,
                        icon: "number"
                    )
                    .keyboardType(.asciiCapableNumberPad)
                    .textContentType(.none)
                    .autocorrectionDisabled()
                    .focused($isInputFocused)
                }
                .padding(.horizontal)

                // Info
                GlassCard {
                    HStack(alignment: .top, spacing: Theme.Spacing.sm) {
                        Image(systemName: "info.circle.fill")
                            .foregroundStyle(.cyan)

                        Text("The port will be accessible via Tailscale on your private network.")
                            .font(.netclodeCaption)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .padding(.horizontal)

                Spacer()

                // Action button
                GlassButton(
                    "Expose Port",
                    icon: "arrow.up.right.circle.fill",
                    tint: .cyan.opacity(0.3)
                ) {
                    if let port = portNumber, isValidPort {
                        onExpose(port)
                    }
                }
                .disabled(!isValidPort)
                .opacity(isValidPort ? 1 : 0.5)
                .padding(.horizontal)
                .padding(.bottom, Theme.Spacing.lg)
            }
            .background(Theme.Colors.background)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        dismiss()
                    }
                }
            }
        }
        .presentationDetents([.medium])
        .presentationDragIndicator(.visible)
        .onAppear {
            isInputFocused = true
        }
    }
}

// MARK: - Preview

#Preview {
    let chatStore = ChatStore()
    chatStore.appendMessage(sessionId: "test", message: ChatMessage.previewUser)
    chatStore.appendMessage(sessionId: "test", message: ChatMessage.previewAssistant)

    return NavigationStack {
        ChatView(sessionId: "test")
    }
    .environment(chatStore)
    .environment(EventStore())
    .environment(SessionStore())
    .environment(SettingsStore())
    .environment(WebSocketService())
}

#Preview("Processing State") {
    let chatStore = ChatStore()
    let sessionStore = SessionStore()
    
    chatStore.appendMessage(sessionId: "test", message: ChatMessage.previewUser)
    sessionStore.setProcessing(for: "test", processing: true)

    return NavigationStack {
        ChatView(sessionId: "test")
    }
    .environment(chatStore)
    .environment(EventStore())
    .environment(sessionStore)
    .environment(SettingsStore())
    .environment(WebSocketService())
}
