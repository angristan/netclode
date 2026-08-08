package session

import (
	"context"
	"log/slog"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
	"github.com/google/uuid"
)

// RoleAssistant is the role constant for assistant messages.
const RoleAssistant = pb.MessageRole_MESSAGE_ROLE_ASSISTANT

// HandleAgentResponse processes streaming responses from the agent.
func (m *Manager) HandleAgentResponse(ctx context.Context, sessionID string, resp *pb.AgentStreamResponse) error {
	state := m.getState(sessionID)
	if state == nil {
		return nil
	}

	switch r := resp.Response.(type) {
	case *pb.AgentStreamResponse_TextDelta:
		return m.handleTextDelta(ctx, sessionID, state, r.TextDelta)
	case *pb.AgentStreamResponse_Event:
		return m.handleAgentEvent(ctx, sessionID, state, r.Event)
	case *pb.AgentStreamResponse_SystemMessage:
		slog.Debug("Agent system message", "sessionID", sessionID, "message", r.SystemMessage.Message)
		return nil
	case *pb.AgentStreamResponse_Result:
		return m.handleAgentResult(ctx, sessionID, state, r.Result)
	case *pb.AgentStreamResponse_Error:
		return m.handleAgentStreamError(ctx, sessionID, state, r.Error)
	default:
		slog.Warn("Unknown agent response type", "sessionID", sessionID)
		return nil
	}
}

// handleTextDelta processes text delta from agent streaming.
func (m *Manager) handleTextDelta(ctx context.Context, sessionID string, state *SessionState, delta *pb.AgentTextDelta) error {
	m.mu.Lock()
	// Use message ID from delta if provided, otherwise use current
	messageID := delta.MessageId
	if messageID == "" {
		messageID = state.CurrentMessageID
	}
	if messageID == "" {
		messageID = "msg_" + uuid.NewString()[:12]
	}

	// If message ID changed and we have accumulated content, persist the previous message
	var contentToPersist string
	var idToPersist string
	var previousMessageStartTime time.Time
	if state.CurrentMessageID != "" && state.CurrentMessageID != messageID {
		previousContent := state.ContentBuilder.String()
		if previousContent != "" {
			contentToPersist = previousContent
			idToPersist = state.CurrentMessageID
			previousMessageStartTime = state.CurrentMessageStartTime
		}
		// Reset content builder for new message
		state.ContentBuilder.Reset()
		// Reset start time for new message
		state.CurrentMessageStartTime = time.Time{}
	}

	// Track when this message's content first arrived
	if state.CurrentMessageID != messageID || state.CurrentMessageStartTime.IsZero() {
		state.CurrentMessageStartTime = time.Now()
	}

	state.CurrentMessageID = messageID
	state.ContentBuilder.WriteString(delta.Content)
	m.mu.Unlock()

	// Persist previous message outside the lock
	if contentToPersist != "" {
		m.appendMessage(ctx, sessionID, idToPersist, pb.MessageRole_MESSAGE_ROLE_ASSISTANT, contentToPersist, previousMessageStartTime)
	}

	if delta.Partial {
		// Emit streaming deltas to clients
		m.emitAgentMessage(ctx, sessionID, messageID, delta.Content, true)
	} else {
		// Final text event (partial=false) - store the message immediately
		// This ensures correct ordering with other events (like thinking blocks)
		m.mu.Lock()
		finalContent := state.ContentBuilder.String()
		finalMessageID := state.CurrentMessageID
		finalStartTime := state.CurrentMessageStartTime
		// Clear state so handleAgentResult won't store it again
		state.ContentBuilder.Reset()
		state.CurrentMessageID = ""
		state.CurrentMessageStartTime = time.Time{}
		m.mu.Unlock()

		if finalContent != "" && finalMessageID != "" {
			m.appendMessage(ctx, sessionID, finalMessageID, pb.MessageRole_MESSAGE_ROLE_ASSISTANT, finalContent, finalStartTime)
		}
	}

	return nil
}

// appendMessage appends a complete message as an AgentEvent with MESSAGE kind to the unified stream.
// The timestamp parameter should be when the message content first started arriving (for correct ordering on reload).
func (m *Manager) appendMessage(ctx context.Context, sessionID string, messageID string, role pb.MessageRole, content string, timestamp time.Time) {
	event := &pb.AgentEvent{
		Kind:          pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE,
		CorrelationId: messageID,
		Payload: &pb.AgentEvent_Message{
			Message: &pb.MessagePayload{
				Role:    role,
				Content: content,
			},
		},
	}
	payload, err := protoJsonOpts.Marshal(event)
	if err != nil {
		slog.Warn("Failed to marshal message event", "sessionID", sessionID, "error", err)
		return
	}
	// Use the provided timestamp (when content first arrived) for correct ordering on reload
	// Fall back to now if no timestamp provided
	ts := timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	entry := &storage.StreamEntry{
		Type:      storage.StreamEntryTypeEvent,
		Partial:   false, // Final message
		Payload:   payload,
		Timestamp: ts.UTC().Format(time.RFC3339),
	}
	if _, err := m.storage.AppendStreamEntry(ctx, sessionID, entry); err != nil {
		slog.Warn("Failed to persist message", "sessionID", sessionID, "error", err)
		return
	}
	// Increment message counter
	if err := m.storage.IncrementMessageCount(ctx, sessionID); err != nil {
		slog.Warn("Failed to increment message count", "sessionID", sessionID, "error", err)
	}
}

// handleAgentEvent processes events from agent execution.
func (m *Manager) handleAgentEvent(ctx context.Context, sessionID string, state *SessionState, event *pb.AgentEvent) error {
	if event == nil {
		return nil
	}

	// Determine if this is a partial (streaming) event based on kind
	// Tool input/output deltas are partial; start/end events are final
	partial := false
	var eventTimestamp time.Time

	switch event.Kind {
	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_START:
		// Track when this tool started (for correct timestamp ordering of related events)
		toolUseId := event.CorrelationId
		if toolUseId != "" {
			m.mu.Lock()
			state.ToolStartTimes[toolUseId] = time.Now()
			eventTimestamp = state.ToolStartTimes[toolUseId]
			m.mu.Unlock()
		}
	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_INPUT,
		pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_OUTPUT:
		// Check if the payload indicates streaming
		if event.GetToolInput() != nil && event.GetToolInput().Delta != nil {
			partial = true
		}
		if event.GetToolOutput() != nil && event.GetToolOutput().Delta != nil {
			partial = true
		}
		// Use the tool's start time for correct ordering
		toolUseId := event.CorrelationId
		if toolUseId != "" {
			m.mu.RLock()
			if startTime, exists := state.ToolStartTimes[toolUseId]; exists {
				eventTimestamp = startTime
			}
			m.mu.RUnlock()
		}
	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_END:
		// Use the tool's start time for correct ordering, then clean up
		toolUseId := event.CorrelationId
		if toolUseId != "" {
			m.mu.Lock()
			if startTime, exists := state.ToolStartTimes[toolUseId]; exists {
				eventTimestamp = startTime
			}
			delete(state.ToolStartTimes, toolUseId)
			m.mu.Unlock()
		}
	case pb.AgentEventKind_AGENT_EVENT_KIND_THINKING:
		// Use the partial field from the proto - true for streaming deltas, false for final complete content
		if event.GetThinking() != nil {
			partial = event.GetThinking().Partial
		}
		// Track when this thinking block started (for correct timestamp ordering)
		// Use correlation_id as the thinking ID
		thinkingID := event.CorrelationId
		if thinkingID != "" {
			m.mu.Lock()
			if _, exists := state.ThinkingStartTimes[thinkingID]; !exists {
				state.ThinkingStartTimes[thinkingID] = time.Now()
			}
			eventTimestamp = state.ThinkingStartTimes[thinkingID]
			m.mu.Unlock()
		}
	}

	// Events are persisted and emitted via emitAgentEvent
	// which writes to the unified stream
	// For thinking events, use the start time for correct ordering
	if !eventTimestamp.IsZero() {
		m.emitAgentEvent(ctx, sessionID, event, partial, eventTimestamp)
	} else {
		m.emitAgentEvent(ctx, sessionID, event, partial)
	}

	return nil
}

// handleAgentResult processes the final result from agent execution.
func (m *Manager) handleAgentResult(ctx context.Context, sessionID string, state *SessionState, result *pb.AgentResult) error {
	m.mu.Lock()
	content := state.ContentBuilder.String()
	messageID := state.CurrentMessageID
	messageStartTime := state.CurrentMessageStartTime
	originalPrompt := state.OriginalPrompt
	titleGenerated := state.TitleGenerated
	m.mu.Unlock()

	// Persist final assistant message if we have content
	// Note: This may be empty if handleTextDelta already stored the message when partial=false arrived
	// appendMessage both stores the message AND increments the message counter
	if content != "" && messageID != "" {
		m.appendMessage(ctx, sessionID, messageID, pb.MessageRole_MESSAGE_ROLE_ASSISTANT, content, messageStartTime)
	}

	// Emit agent done
	m.emitAgentDone(ctx, sessionID)

	// Update status back to ready and update last active time
	m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_READY)
	m.updateLastActiveAt(ctx, sessionID)

	// Generate title if first prompt and not already generated
	if !titleGenerated && originalPrompt != "" {
		m.mu.Lock()
		state.TitleGenerated = true
		m.mu.Unlock()

		// Request title generation from agent
		agent := m.GetAgentConnection(sessionID)
		if agent != nil {
			requestID := uuid.NewString()[:12]
			if err := agent.GenerateTitle(requestID, originalPrompt); err != nil {
				slog.Warn("Failed to request title generation", "sessionID", sessionID, "error", err)
			}
		}
	}

	// Create auto-snapshot after each turn (async with timeout)
	if originalPrompt != "" {
		// Count user messages to get turn number
		msgCount, _ := m.storage.GetMessageCount(ctx, sessionID)
		turnNumber := (msgCount + 1) / 2 // Approximate: each turn is user + assistant
		go func() {
			snapshotCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			m.AutoSnapshot(snapshotCtx, sessionID, turnNumber, originalPrompt)
		}()
	}

	// Reset streaming state for next prompt
	m.mu.Lock()
	state.CurrentMessageID = ""
	state.ContentBuilder.Reset()
	state.OriginalPrompt = ""
	m.mu.Unlock()

	slog.Info("Agent prompt completed", "sessionID", sessionID,
		"inputTokens", result.InputTokens,
		"outputTokens", result.OutputTokens,
		"turns", result.TotalTurns)

	return nil
}

// handleAgentStreamError processes errors during agent execution.
func (m *Manager) handleAgentStreamError(ctx context.Context, sessionID string, state *SessionState, agentErr *pb.AgentError) error {
	slog.Error("Agent execution error", "sessionID", sessionID, "error", agentErr.Message)

	m.emitAgentError(ctx, sessionID, agentErr.Message)
	m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_READY)

	// Reset streaming state
	m.mu.Lock()
	state.CurrentMessageID = ""
	state.ContentBuilder.Reset()
	state.OriginalPrompt = ""
	m.mu.Unlock()

	return nil
}

// HandleTerminalOutput broadcasts terminal output to clients.
func (m *Manager) HandleTerminalOutput(ctx context.Context, sessionID string, data string) error {
	m.emitTerminalOutput(ctx, sessionID, data)
	return nil
}

// HandleTitleResponse processes title generation response from agent.
func (m *Manager) HandleTitleResponse(ctx context.Context, sessionID string, requestID string, title string) error {
	if title != "" {
		m.updateSessionName(ctx, sessionID, title)
		slog.Info("Session title generated", "sessionID", sessionID, "title", title)
	}
	return nil
}

// HandleGitStatusResponse processes git status response from agent.
func (m *Manager) HandleGitStatusResponse(ctx context.Context, sessionID string, requestID string, files []*pb.GitFileChange) error {
	// Send to waiting request
	pendingGitMu.Lock()
	if ch, ok := pendingGitStatusRequests[requestID]; ok {
		ch <- gitStatusResult{files: files, err: nil}
	}
	pendingGitMu.Unlock()

	return nil
}

// HandleGitDiffResponse processes git diff response from agent.
func (m *Manager) HandleGitDiffResponse(ctx context.Context, sessionID string, requestID string, diff string) error {
	pendingGitMu.Lock()
	if ch, ok := pendingGitDiffRequests[requestID]; ok {
		ch <- gitDiffResult{diff: diff, err: nil}
	}
	pendingGitMu.Unlock()

	return nil
}

