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

/// A grouped event combining start/end events, with optional nested children for Task/subagent hierarchies
struct GroupedEvent: Identifiable {
    let id: UUID
    let event: AgentEvent
    let timestamp: Date
    var endEvent: AgentEvent?
    var children: [GroupedEvent] = []  // Nested tool events for Task/subagent
    
    /// Returns the toolUseId if this is a tool event
    var toolUseId: String? {
        switch event {
        case .toolStart(let e): return e.toolUseId
        case .toolEnd(let e): return e.toolUseId
        default: return nil
        }
    }
    
    /// Returns the parentToolUseId if this is a child tool event
    var parentToolUseId: String? {
        switch event {
        case .toolStart(let e): return e.parentToolUseId
        case .toolEnd(let e): return e.parentToolUseId
        default: return nil
        }
    }
    
    /// Whether this event has nested children (e.g., Task with sub-tools)
    var hasChildren: Bool { !children.isEmpty }
}

// MARK: - Chat View

struct ChatView: View {
    let sessionId: String

    @Environment(ChatStore.self) private var chatStore
    @Environment(EventStore.self) private var eventStore
    @Environment(SessionStore.self) private var sessionStore
    @Environment(ConnectService.self) private var connectService
    @Environment(SettingsStore.self) private var settingsStore
    @Environment(AppStateCoordinator.self) private var coordinator

    @State private var inputText = ""
    @FocusState private var isInputFocused: Bool

    // Cached timeline to avoid recomputing on every view update
    @State private var cachedTimeline: [TimelineItem] = []
    @State private var lastContentLength: Int = 0
    @State private var lastThinkingContentLength: Int = 0
    @State private var lastToolInputContentLength: Int = 0
    @State private var lastProcessingState: Bool = false
    @State private var lastRepoOrderSignature: String = ""
    
    // Status pill visibility
    @State private var showStatusPill = false
    @State private var lastKnownStatus: SessionStatus?
    @State private var lastScrollOffset: CGFloat = 0
    @State private var isScrollingUp = false
    @State private var hideStatusPillTask: Task<Void, Never>?
    
    // Track content visibility for fade-in animation
    @State private var isContentVisible = false
    
    // Store scroll proxy for programmatic scrolling
    @State private var scrollProxy: ScrollViewProxy?

    var messages: [ChatMessage] {
        chatStore.messages(for: sessionId)
    }

    var events: [AgentEvent] {
        eventStore.events(for: sessionId)
    }

    var isProcessing: Bool {
        sessionStore.isProcessing(sessionId)
    }
    
    var session: Session? {
        sessionStore.sessions.first { $0.id == sessionId }
    }
    
    var isConnectionUsable: Bool {
        connectService.connectionState.isUsable
    }
    
    var hasQueuedMessage: Bool {
        coordinator.messageQueue.hasQueuedMessage(for: sessionId)
    }
    
    private var sessionReposSignature: String {
        session?.repos.joined(separator: "|") ?? ""
    }

    /// Total content length of all thinking events (to detect streaming updates)
    private var thinkingContentLength: Int {
        events.reduce(0) { sum, event in
            if case .thinking(let e) = event {
                return sum + e.content.count
            }
            return sum
        }
    }
    
    /// Total content length of tool inputs (to detect streaming tool input updates)
    private var toolInputContentLength: Int {
        events.reduce(0) { sum, event in
            if case .toolStart(let e) = event {
                return sum + e.input.values.reduce(0) { $0 + $1.description.count }
            }
            return sum
        }
    }

    /// Unified timeline combining messages and events, sorted by timestamp
    private func computeTimeline() -> [TimelineItem] {
        // Track items with their original insertion order for stable sorting
        var indexedItems: [(item: TimelineItem, insertionOrder: Int)] = []
        var insertionOrder = 0
        let repoOrder = repoOrderMap()

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

            indexedItems.append((.message(message, isStreaming: isStreaming, turnDuration: turnDuration), insertionOrder))
            insertionOrder += 1
        }

        // Add grouped events
        let groupedEvents = groupEvents(events)
        let adjustedEvents = adjustRepoCloneTimestamps(groupedEvents, repoOrder: repoOrder)
        for grouped in adjustedEvents {
            indexedItems.append((.event(grouped), insertionOrder))
            insertionOrder += 1
        }

        // Sort by timestamp with special handling:
        // 1. Repo clone events preserve session repo order
        // 2. Use insertion order as tiebreaker for stable sorting when timestamps are equal
        let sorted = indexedItems.sorted { lhs, rhs in
            // Handle repo clone ordering
            if case .event(let lhsGrouped) = lhs.item,
               case .event(let rhsGrouped) = rhs.item,
               case .repoClone(let lhsRepo) = lhsGrouped.event,
               case .repoClone(let rhsRepo) = rhsGrouped.event {
                let lhsIndex = repoOrder[normalizeRepoName(lhsRepo.repo)] ?? Int.max
                let rhsIndex = repoOrder[normalizeRepoName(rhsRepo.repo)] ?? Int.max
                if lhsIndex != rhsIndex {
                    return lhsIndex < rhsIndex
                }
            }
            
            // Primary sort by timestamp, secondary by insertion order for stability
            if lhs.item.timestamp != rhs.item.timestamp {
                return lhs.item.timestamp < rhs.item.timestamp
            }
            return lhs.insertionOrder < rhs.insertionOrder
        }
        
        return sorted.map { $0.item }
    }

    private func repoOrderMap() -> [String: Int] {
        guard let repos = session?.repos, !repos.isEmpty else { return [:] }
        var map: [String: Int] = [:]
        for (index, repo) in repos.enumerated() {
            let normalized = normalizeRepoName(repo)
            if map[normalized] == nil {
                map[normalized] = index
            }
        }
        return map
    }

    /// Normalizes repo strings to "owner/repo" for comparison.
    private func normalizeRepoName(_ repo: String) -> String {
        if let range = repo.range(of: "github.com/") {
            let afterGithub = String(repo[range.upperBound...])
            return afterGithub.replacingOccurrences(of: ".git", with: "")
        }
        return repo.replacingOccurrences(of: ".git", with: "")
    }

    private func adjustRepoCloneTimestamps(_ groupedEvents: [GroupedEvent], repoOrder: [String: Int]) -> [GroupedEvent] {
        let cloneEvents = groupedEvents.compactMap { grouped -> (GroupedEvent, RepoCloneEvent)? in
            if case .repoClone(let event) = grouped.event {
                return (grouped, event)
            }
            return nil
        }

        guard let baseTimestamp = cloneEvents.map({ $0.0.timestamp }).min() else {
            return groupedEvents
        }

        return groupedEvents.map { grouped in
            guard case .repoClone(let cloneEvent) = grouped.event else {
                return grouped
            }

            let normalized = normalizeRepoName(cloneEvent.repo)
            let index = repoOrder[normalized] ?? 0
            let adjustedTimestamp = baseTimestamp.addingTimeInterval(Double(index) * 0.001)

            var updated = GroupedEvent(id: grouped.id, event: grouped.event, timestamp: adjustedTimestamp)
            updated.endEvent = grouped.endEvent
            updated.children = grouped.children
            return updated
        }
    }

    var body: some View {
        mainContent
            .safeAreaInset(edge: .bottom) {
                ChatInputBar(
                    text: $inputText,
                    isProcessing: isProcessing,
                    isFocused: $isInputFocused,
                    onSend: sendMessage,
                    onInterrupt: interruptAgent,
                    isConnected: isConnectionUsable,
                    hasQueuedMessage: hasQueuedMessage
                )
            }
            .overlay(alignment: .top) { statusPillOverlay }
            .animation(.snappy, value: showStatusPill)
            .onChange(of: session?.status) { _, newStatus in
                handleStatusChange(newStatus)
            }
            .task(id: sessionId) {
                await handleSessionAppear()
            }
            .onChange(of: isInputFocused) { _, focused in
                handleInputFocusChange(focused)
            }
    }
    
    private var mainContent: some View {
        ScrollViewReader { proxy in
            scrollContent
                .opacity(isContentVisible ? 1 : 0)
                .onAppear { scrollProxy = proxy }
                .onChange(of: messages.count) { updateTimelineIfNeeded() }
                .onChange(of: messages.last?.content) { updateTimelineIfNeeded() }
                .onChange(of: events.count) { updateTimelineIfNeeded() }
                .onChange(of: thinkingContentLength) { updateTimelineIfNeeded() }
                .onChange(of: isProcessing) { updateTimelineIfNeeded() }
                .onChange(of: sessionReposSignature) { updateTimelineIfNeeded() }
                .onChange(of: toolInputContentLength) { updateTimelineIfNeeded() }
                .onChange(of: cachedTimeline.isEmpty) { _, isEmpty in
                    handleTimelineChange(isEmpty)
                }
                .task(id: sessionId) {
                    await handleSessionChange()
                }
        }
    }
    
    @ViewBuilder
    private var statusPillOverlay: some View {
        if showStatusPill || !isConnectionUsable || hasQueuedMessage, let status = session?.status {
            StatusPill(
                status: status,
                isOffline: !isConnectionUsable,
                hasQueuedMessage: hasQueuedMessage
            )
            .transition(.move(edge: .top).combined(with: .opacity))
            .padding(.top, Theme.Spacing.sm)
        }
    }
    
    private func handleTimelineChange(_ isEmpty: Bool) {
        if !isEmpty && !isContentVisible {
            withAnimation(.easeOut(duration: 0.20)) {
                isContentVisible = true
            }
        }
    }
    
    private func handleSessionChange() async {
        isContentVisible = false
        updateTimelineIfNeeded()
        
        if !cachedTimeline.isEmpty {
            withAnimation(.easeOut(duration: 0.20)) {
                isContentVisible = true
            }
        }
    }
    
    private func handleStatusChange(_ newStatus: SessionStatus?) {
        guard let newStatus, newStatus != lastKnownStatus else { return }
        lastKnownStatus = newStatus
        
        withAnimation {
            showStatusPill = true
        }
        
        hideStatusPillTask?.cancel()
        if session?.status == .ready {
            hideStatusPillTask = Task {
                try? await Task.sleep(for: .seconds(2))
                guard !Task.isCancelled else { return }
                if !isScrollingUp {
                    withAnimation {
                        showStatusPill = false
                    }
                }
            }
        }
    }
    
    private func handleSessionAppear() async {
        lastKnownStatus = session?.status
        if let status = session?.status {
            withAnimation {
                showStatusPill = true
            }
            hideStatusPillTask?.cancel()
            if status == .ready {
                hideStatusPillTask = Task {
                    try? await Task.sleep(for: .seconds(2))
                    guard !Task.isCancelled else { return }
                    if !isScrollingUp {
                        withAnimation {
                            showStatusPill = false
                        }
                    }
                }
            }
        }
    }
    
    private func handleInputFocusChange(_ focused: Bool) {
        if focused && session?.status == .paused {
            connectService.send(.sessionResume(id: sessionId))
        }
    }
    
    private var scrollContent: some View {
        ScrollView(.vertical) {
            VStack(spacing: Theme.Spacing.sm) {
                // Scroll position tracker
                GeometryReader { geo in
                    Color.clear
                        .preference(
                            key: ScrollOffsetKey.self,
                            value: geo.frame(in: .named("scroll")).minY
                        )
                }
                .frame(height: 0)
                
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
        .defaultScrollAnchor(.bottom)
        .coordinateSpace(name: "scroll")
        .onPreferenceChange(ScrollOffsetKey.self) { offset in
            let scrollingUp = offset > lastScrollOffset
            if scrollingUp != isScrollingUp {
                isScrollingUp = scrollingUp
                // Only hide on scroll for .ready status - other statuses stay visible
                let shouldShow = scrollingUp || session?.status != .ready
                withAnimation(.snappy) {
                    showStatusPill = shouldShow
                }
            }
            lastScrollOffset = offset
        }
        .scrollDismissesKeyboard(.interactively)
    }
    
    private func scrollToBottom(animated: Bool = true) {
        guard let proxy = scrollProxy else { return }
        if animated {
            withAnimation(.glassSpring) {
                proxy.scrollTo("bottom", anchor: .bottom)
            }
        } else {
            proxy.scrollTo("bottom", anchor: .bottom)
        }
    }

    @ViewBuilder
    private func timelineItemView(_ item: TimelineItem) -> some View {
        switch item {
        case .message(let message, _, let turnDuration):
            // Compute isStreaming at render time to ensure it reflects current processing state
            let isLastAssistant = message.role == .assistant && message.id == messages.last?.id
            let currentlyStreaming = isLastAssistant && isProcessing
            let isPending = chatStore.isMessagePending(message.id)
            ChatMessageRow(message: message, isStreaming: currentlyStreaming, turnDuration: turnDuration, isPending: isPending)
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
            ToolEventCard(event: grouped.event, endEvent: endEvent, children: grouped.children)

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

        case .repoClone(let e):
            RepoCloneCard(event: e)

        case .agentDisconnected, .agentReconnected:
            SystemEventCard(event: grouped.event)

        default:
            EmptyView()
        }
    }

    /// Groups related events (tool_start with tool_end, etc.) and builds parent-child hierarchy for Task/subagent tools
    private func groupEvents(_ events: [AgentEvent]) -> [GroupedEvent] {
        var result: [GroupedEvent] = []
        var toolStartMap: [String: Int] = [:]  // toolUseId -> index in result
        var childIndices: Set<Int> = []        // Indices of events that are children (to filter from top-level)

        // First pass: Create all grouped events and pair start/end
        for event in events {
            switch event {
            case .toolStart(let e):
                let grouped = GroupedEvent(id: e.id, event: event, timestamp: e.timestamp)
                let index = result.count
                toolStartMap[e.toolUseId] = index
                result.append(grouped)

            case .toolEnd(let e):
                if let index = toolStartMap[e.toolUseId] {
                    result[index].endEvent = event
                }

            case .toolInput, .toolInputComplete:
                break // Skip tool input events (input is merged into tool_start)

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

            case .repoClone(let e):
                // Group by repo - update existing entry if same repo, otherwise add new
                if let index = result.lastIndex(where: {
                    if case .repoClone(let existing) = $0.event {
                        return existing.repo == e.repo
                    }
                    return false
                }) {
                    // Update the existing entry with the latest event
                    result[index] = GroupedEvent(id: result[index].id, event: event, timestamp: result[index].timestamp)
                } else {
                    result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))
                }

            case .agentDisconnected(let e):
                result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))

            case .agentReconnected(let e):
                result.append(GroupedEvent(id: e.id, event: event, timestamp: e.timestamp))
            }
        }
        
        // Second pass: Build parent-child hierarchy using parentToolUseId
        // Iterate through all tool events and move children under their parents
        for (index, grouped) in result.enumerated() {
            if let parentId = grouped.parentToolUseId,
               let parentIndex = toolStartMap[parentId] {
                // This is a child event - add it to parent's children
                result[parentIndex].children.append(grouped)
                childIndices.insert(index)
            }
        }
        
        // Filter out events that are now children (keep only top-level)
        return result.enumerated()
            .filter { !childIndices.contains($0.offset) }
            .map { $0.element }
    }

    private func sendMessage() {
        let text = inputText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty else { return }

        if settingsStore.hapticFeedbackEnabled {
            HapticFeedback.light()
        }
        
        // Scroll to bottom to see the new message
        scrollToBottom()

        let userMessage = ChatMessage(role: .user, content: text)
        
        // Clear input first
        inputText = ""

        // If offline, queue the message for later and mark as pending
        if !isConnectionUsable {
            chatStore.appendPendingMessage(sessionId: sessionId, message: userMessage)
            coordinator.queueMessage(sessionId: sessionId, content: text)
            return
        }
        
        // Add user message locally (online path)
        chatStore.appendMessage(sessionId: sessionId, message: userMessage)

        // Mark as processing before sending
        sessionStore.setProcessing(for: sessionId, processing: true)

        // Resume session if paused (lazy resume on first message)
        if session?.status == .paused {
            connectService.send(.sessionResume(id: sessionId))
        }

        // Send to server
        connectService.send(.prompt(sessionId: sessionId, text: text))
    }

    private func interruptAgent() {
        if settingsStore.hapticFeedbackEnabled {
            HapticFeedback.warning()
        }
        connectService.send(.promptInterrupt(sessionId: sessionId))
    }

    /// Update cached timeline only when data changes
    private func updateTimelineIfNeeded() {
        // Compute new version based on data state including content length
        let currentContentLength = messages.last?.content.count ?? 0
        let currentThinkingLength = thinkingContentLength
        let currentToolInputLength = toolInputContentLength
        let currentProcessing = isProcessing
        let currentRepoOrderSignature = sessionReposSignature
        
        let messageCountChanged = messages.count != cachedTimeline.filter {
            if case .message = $0 { return true }
            return false
        }.count
        let eventCountChanged = events.count != cachedTimeline.filter {
            if case .event = $0 { return true }
            return false
        }.count
        let contentLengthChanged = currentContentLength != lastContentLength
        let thinkingLengthChanged = currentThinkingLength != lastThinkingContentLength
        let toolInputLengthChanged = currentToolInputLength != lastToolInputContentLength
        let processingStateChanged = currentProcessing != lastProcessingState
        let repoOrderChanged = currentRepoOrderSignature != lastRepoOrderSignature
        
        let dataChanged = messageCountChanged || eventCountChanged || contentLengthChanged
            || thinkingLengthChanged || toolInputLengthChanged || processingStateChanged || repoOrderChanged

        guard dataChanged else { return }

        lastContentLength = currentContentLength
        lastThinkingContentLength = currentThinkingLength
        lastToolInputContentLength = currentToolInputLength
        lastProcessingState = currentProcessing
        lastRepoOrderSignature = currentRepoOrderSignature
        
        cachedTimeline = computeTimeline()
    }
    

}

// MARK: - Scroll Offset Key

struct ScrollOffsetKey: PreferenceKey {
    nonisolated(unsafe) static var defaultValue: CGFloat = 0
    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) {
        value = nextValue()
    }
}

// MARK: - Status Pill

struct StatusPill: View {
    let status: SessionStatus
    var isOffline: Bool = false
    var hasQueuedMessage: Bool = false
    
    private var displayColor: Color {
        if isOffline || hasQueuedMessage {
            return .orange
        }
        return status.tintColor.color
    }
    
    private var displayText: String {
        if hasQueuedMessage {
            return "Queued"
        }
        if isOffline {
            return "Offline"
        }
        return status.displayName
    }
    
    var body: some View {
        HStack(spacing: 6) {
            // Icon: wifi.slash when offline, otherwise status dot
            if isOffline {
                Image(systemName: "wifi.slash")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(displayColor)
            } else {
                Circle()
                    .fill(displayColor)
                    .frame(width: 8, height: 8)
                    .pulsing(status == .running && !hasQueuedMessage)
            }
            
            Text(displayText)
                .font(.system(size: 13, weight: .medium))
                .contentTransition(.numericText())
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
        .glassEffect(
            .regular.tint(displayColor.glassTint),
            in: Capsule()
        )
        .animation(.smooth, value: status)
        .animation(.smooth, value: isOffline)
        .animation(.smooth, value: hasQueuedMessage)
    }
}

// MARK: - Expose Port Sheet

struct ExposePortSheet: View {
    @Binding var portText: String
    let onExpose: (Int) -> Void

    @Environment(\.dismiss) private var dismiss

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
            VStack(spacing: Theme.Spacing.md) {
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

                // Info
                HStack(alignment: .top, spacing: Theme.Spacing.sm) {
                    Image(systemName: "info.circle.fill")
                        .foregroundStyle(.cyan)

                    Text("The port will be accessible via Tailscale on your private network.")
                        .font(.netclodeCaption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                .frame(maxWidth: .infinity, alignment: .leading)

                Spacer()
            }
            .padding()
            .background(Theme.Colors.background)
            .navigationTitle("Expose Port")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        dismiss()
                    }
                }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Expose") {
                        if let port = portNumber, isValidPort {
                            onExpose(port)
                        }
                    }
                    .fontWeight(.semibold)
                    .disabled(!isValidPort)
                }
            }
        }
        .presentationDetents([.height(200)])
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
    .environment(ConnectService())
}
