package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/google/uuid"
)


// SendPrompt sends a prompt to the agent and streams the response.
// If the sandbox isn't ready yet, queues the prompt to be sent when ready.
// If the session is paused or no agent is connected, automatically resumes it first.
func (m *Manager) SendPrompt(ctx context.Context, sessionID, text string) error {
	state := m.getOrLoadState(ctx, sessionID)
	if state == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}

	// Check if agent is connected
	agent := m.GetAgentConnection(sessionID)

	// Auto-resume if session is paused OR if no agent is connected (sandbox may not exist)
	if state.Session.Status == pb.SessionStatus_SESSION_STATUS_PAUSED || agent == nil {
		slog.Info("Auto-resuming session for prompt", "sessionID", sessionID, "status", state.Session.Status.String(), "hasAgent", agent != nil)
		if _, err := m.Resume(ctx, sessionID); err != nil {
			return fmt.Errorf("failed to resume session: %w", err)
		}
		// Re-check agent connection after resume
		agent = m.GetAgentConnection(sessionID)
	}

	if agent == nil {
		// Agent still not connected - queue the prompt (sandbox is starting)
		slog.Info("Queueing prompt until agent connects", "sessionID", sessionID)
		m.mu.Lock()
		state.PendingPrompt = text
		currentStatus := state.Session.Status
		m.mu.Unlock()
		// Only set status to RUNNING if we were PAUSED. If RESUMING (restore in progress),
		// keep that status until createSandboxDirect completes and sets RUNNING.
		if currentStatus == pb.SessionStatus_SESSION_STATUS_PAUSED {
			m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_RUNNING)
		}
		return nil
	}

	// Persist and broadcast user message (emitUserMessage does both via publishStreamEntry)
	m.emitUserMessage(ctx, sessionID, text)

	// Update session status to running and last active time
	m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_RUNNING)
	m.updateLastActiveAt(ctx, sessionID)

	// Initialize streaming state
	m.mu.Lock()
	state.CurrentMessageID = "msg_" + uuid.NewString()[:12]
	state.ContentBuilder.Reset()
	state.OriginalPrompt = text
	m.mu.Unlock()

	// Send prompt to agent via bidirectional stream
	if err := agent.ExecutePrompt(text); err != nil {
		slog.Error("Failed to send prompt to agent", "sessionID", sessionID, "error", err)
		m.emitAgentError(ctx, sessionID, err.Error())
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_READY)
		return err
	}

	return nil
}

// pendingGitRequests tracks pending git status/diff requests with response channels
type gitStatusResult struct {
	files []*pb.GitFileChange
	err   error
}

type gitDiffResult struct {
	diff string
	err  error
}

var (
	pendingGitStatusRequests = make(map[string]chan gitStatusResult)
	pendingGitDiffRequests   = make(map[string]chan gitDiffResult)
	pendingGitMu             sync.Mutex
)

// GetGitStatus fetches git status from the agent.
func (m *Manager) GetGitStatus(ctx context.Context, sessionID string) ([]*pb.GitFileChange, error) {
	agent := m.GetAgentConnection(sessionID)
	if agent == nil {
		return nil, fmt.Errorf("no agent connected for session %s", sessionID)
	}

	requestID := uuid.NewString()[:12]
	resultCh := make(chan gitStatusResult, 1)

	pendingGitMu.Lock()
	pendingGitStatusRequests[requestID] = resultCh
	pendingGitMu.Unlock()

	defer func() {
		pendingGitMu.Lock()
		delete(pendingGitStatusRequests, requestID)
		pendingGitMu.Unlock()
	}()

	if err := agent.GetGitStatus(requestID); err != nil {
		return nil, fmt.Errorf("failed to request git status: %w", err)
	}

	select {
	case result := <-resultCh:
		return result.files, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("git status request timed out")
	}
}

// GetGitDiff fetches git diff for a file from the agent.
func (m *Manager) GetGitDiff(ctx context.Context, sessionID, file string) (string, error) {
	agent := m.GetAgentConnection(sessionID)
	if agent == nil {
		return "", fmt.Errorf("no agent connected for session %s", sessionID)
	}

	requestID := uuid.NewString()[:12]
	resultCh := make(chan gitDiffResult, 1)

	pendingGitMu.Lock()
	pendingGitDiffRequests[requestID] = resultCh
	pendingGitMu.Unlock()

	defer func() {
		pendingGitMu.Lock()
		delete(pendingGitDiffRequests, requestID)
		pendingGitMu.Unlock()
	}()

	var filePtr *string
	if file != "" {
		filePtr = &file
	}

	if err := agent.GetGitDiff(requestID, filePtr); err != nil {
		return "", fmt.Errorf("failed to request git diff: %w", err)
	}

	select {
	case result := <-resultCh:
		return result.diff, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("git diff request timed out")
	}
}

// Interrupt sends an interrupt signal to the agent.
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
	agent := m.GetAgentConnection(sessionID)
	if agent == nil {
		return fmt.Errorf("no agent connected for session %s", sessionID)
	}

	return agent.Interrupt()
}
