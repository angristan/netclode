import Foundation

@MainActor
@Observable
final class EventStore {
    private(set) var eventsBySession: [String: [AgentEvent]] = [:]

    private let maxEventsPerSession = 100

    func events(for sessionId: String) -> [AgentEvent] {
        eventsBySession[sessionId] ?? []
    }

    func recentEvents(for sessionId: String, count: Int = 5) -> [AgentEvent] {
        Array((eventsBySession[sessionId] ?? []).suffix(count))
    }

    func appendEvent(sessionId: String, event: AgentEvent) {
        var events = eventsBySession[sessionId] ?? []
        events.append(event)

        // Keep only the most recent events
        if events.count > maxEventsPerSession {
            events = Array(events.suffix(maxEventsPerSession))
        }

        eventsBySession[sessionId] = events
    }

    /// Append partial thinking content or create a new thinking event
    func appendThinkingPartial(sessionId: String, thinkingId: String, content: String, timestamp: Date) {
        var events = eventsBySession[sessionId] ?? []

        // Find existing thinking event with this thinkingId
        if let existingIndex = events.lastIndex(where: { event in
            if case .thinking(let e) = event, e.thinkingId == thinkingId {
                return true
            }
            return false
        }) {
            // Update existing thinking event by appending content
            if case .thinking(var thinkingEvent) = events[existingIndex] {
                thinkingEvent.content += content
                events[existingIndex] = .thinking(thinkingEvent)
            }
        } else {
            // Create new thinking event
            let newEvent = ThinkingEvent(
                id: UUID(),
                timestamp: timestamp,
                thinkingId: thinkingId,
                content: content,
                partial: true
            )
            events.append(.thinking(newEvent))
        }

        eventsBySession[sessionId] = events
    }

    /// Finalize a thinking event (mark as complete)
    func finalizeThinking(sessionId: String, thinkingId: String) {
        guard var events = eventsBySession[sessionId] else { return }

        if let index = events.lastIndex(where: { event in
            if case .thinking(let e) = event, e.thinkingId == thinkingId {
                return true
            }
            return false
        }) {
            if case .thinking(var thinkingEvent) = events[index] {
                // Create a new event with partial = false
                let finalizedEvent = ThinkingEvent(
                    id: thinkingEvent.id,
                    timestamp: thinkingEvent.timestamp,
                    thinkingId: thinkingEvent.thinkingId,
                    content: thinkingEvent.content,
                    partial: false
                )
                events[index] = .thinking(finalizedEvent)
                eventsBySession[sessionId] = events
            }
        }
    }

    func clearEvents(for sessionId: String) {
        eventsBySession.removeValue(forKey: sessionId)
    }

    /// Load events from server sync response
    func loadEvents(sessionId: String, events: [PersistedEvent]) {
        eventsBySession[sessionId] = events.map { $0.event.toAgentEvent() }
    }
}
