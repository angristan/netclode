package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"connectrpc.com/connect"
	v1 "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
	"github.com/angristan/netclode/services/control-plane/internal/session"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Ensure ConnectAgentServiceHandler implements the interface
var _ netclodev1connect.AgentServiceHandler = (*ConnectAgentServiceHandler)(nil)

// ConnectAgentServiceHandler implements the Connect AgentService.
type ConnectAgentServiceHandler struct {
	netclodev1connect.UnimplementedAgentServiceHandler
	manager *session.Manager
	server  *Server
}

// NewConnectAgentServiceHandler creates a new Connect agent service handler.
func NewConnectAgentServiceHandler(manager *session.Manager, server *Server) *ConnectAgentServiceHandler {
	return &ConnectAgentServiceHandler{
		manager: manager,
		server:  server,
	}
}

// AgentConnection represents a connected agent's bidirectional stream.
type AgentConnection struct {
	stream    *connect.BidiStream[v1.AgentMessage, v1.ControlPlaneMessage]
	manager   *session.Manager
	server    *Server
	sessionID string
	podName   string // For warm pool mode

	// For sending messages to agent
	outbound chan *v1.ControlPlaneMessage
	done     chan struct{}
	writeMu  sync.Mutex
}

// Ensure AgentConnection implements WarmAgentConnection interface
var _ session.WarmAgentConnection = (*AgentConnection)(nil)

// Connect implements the bidirectional streaming RPC for agents.
func (h *ConnectAgentServiceHandler) Connect(ctx context.Context, stream *connect.BidiStream[v1.AgentMessage, v1.ControlPlaneMessage]) error {
	conn := &AgentConnection{
		stream:   stream,
		manager:  h.manager,
		server:   h.server,
		outbound: make(chan *v1.ControlPlaneMessage, 64),
		done:     make(chan struct{}),
	}

	// Wait for registration message first
	msg, err := stream.Receive()
	if err != nil {
		slog.Warn("Agent connection failed before registration", "error", err)
		return err
	}

	reg := msg.GetRegister()
	if reg == nil {
		return errors.New("first message must be AgentRegister")
	}

	// All agents must provide k8s_token for identity verification
	sessionID := reg.GetSessionId()
	k8sToken := reg.GetK8SToken()

	if k8sToken == "" {
		slog.Warn("Agent registration failed - no k8s_token", "version", reg.Version)
		conn.send(&v1.ControlPlaneMessage{
			Message: &v1.ControlPlaneMessage_Registered{
				Registered: &v1.AgentRegistered{
					Success: false,
					Error:   strPtr("k8s_token required for authentication"),
				},
			},
		})
		return errors.New("k8s_token required for authentication")
	}

	// Verify the agent's identity using Kubernetes TokenReview API
	// This prevents rogue agents from impersonating legitimate ones
	podName, err := h.manager.VerifyAgentToken(ctx, k8sToken)
	if err != nil {
		slog.Warn("Agent token verification failed", "error", err, "version", reg.Version)
		conn.send(&v1.ControlPlaneMessage{
			Message: &v1.ControlPlaneMessage_Registered{
				Registered: &v1.AgentRegistered{
					Success: false,
					Error:   strPtr("token verification failed: " + err.Error()),
				},
			},
		})
		return err
	}

	// Warm pool mode: no session_id, agent waits for SessionAssigned message
	if sessionID == "" {
		slog.Info("Warm pool agent connecting (verified)", "podName", podName, "version", reg.Version)

		conn.podName = podName

		// Send registration success (no config yet - will be pushed via SessionAssigned)
		if err := conn.send(&v1.ControlPlaneMessage{
			Message: &v1.ControlPlaneMessage_Registered{
				Registered: &v1.AgentRegistered{
					Success: true,
					// No config - agent waits for SessionAssigned
				},
			},
		}); err != nil {
			return err
		}

		// Register as warm agent - session will be assigned when claim binds
		h.manager.RegisterWarmAgentConnection(podName, conn)
		defer h.manager.UnregisterWarmAgentConnection(podName)

		slog.Info("Warm pool agent registered, waiting for session assignment", "podName", podName)

		// Start outbound message sender
		go conn.sendLoop()

		// Handle incoming messages (will wait for SessionAssigned to set sessionID)
		err = conn.receiveLoop(ctx)

		close(conn.done)
		slog.Info("Warm pool agent disconnected", "podName", podName, "sessionID", conn.sessionID)

		// If session was assigned, unregister from active agents
		if conn.sessionID != "" {
			h.manager.UnregisterAgentConnection(conn.sessionID)
		}

		return err
	}

	// Direct mode: session_id provided
	// Verify the pod name matches the expected pattern for this session
	// Direct mode pods are named "sess-<sessionID>" or "sess-<sessionID>-<suffix>"
	expectedPrefix := "sess-" + sessionID
	if !strings.HasPrefix(podName, expectedPrefix) {
		err := fmt.Errorf("pod name %q doesn't match session %q (expected prefix %q)", podName, sessionID, expectedPrefix)
		slog.Warn("Agent session verification failed", "error", err, "podName", podName, "sessionID", sessionID)
		conn.send(&v1.ControlPlaneMessage{
			Message: &v1.ControlPlaneMessage_Registered{
				Registered: &v1.AgentRegistered{
					Success: false,
					Error:   strPtr(err.Error()),
				},
			},
		})
		return err
	}

	conn.sessionID = sessionID
	slog.Info("Agent connecting (direct mode, verified)", "sessionID", sessionID, "podName", podName, "version", reg.Version)

	// Get session config
	config, err := h.manager.GetSessionConfig(ctx, sessionID)
	if err != nil {
		slog.Warn("Agent registration failed - session not found", "sessionID", sessionID, "error", err)
		conn.send(&v1.ControlPlaneMessage{
			Message: &v1.ControlPlaneMessage_Registered{
				Registered: &v1.AgentRegistered{
					Success: false,
					Error:   strPtr("session not found: " + err.Error()),
				},
			},
		})
		return err
	}

	// Send registration success with config
	sessionConfig := &v1.SessionConfig{
		SessionId:       sessionID,
		WorkspaceDir:    "/workspace",
		ControlPlaneUrl: "http://control-plane.netclode.svc.cluster.local",
		SdkType:         config.SdkType,
		CopilotBackend:  config.CopilotBackend,
	}
	if len(config.Repos) > 0 {
		sessionConfig.Repos = config.Repos
	}
	if config.RepoAccess != nil {
		sessionConfig.RepoAccess = config.RepoAccess
	}
	if config.GitHubToken != "" {
		sessionConfig.GithubToken = &config.GitHubToken
	}
	if config.GitHubCopilotToken != "" {
		sessionConfig.GithubCopilotToken = &config.GitHubCopilotToken
	}
	if config.Model != "" {
		sessionConfig.Model = &config.Model
	}
	if config.CodexAccessToken != "" {
		sessionConfig.CodexAccessToken = &config.CodexAccessToken
	}
	if config.CodexIdToken != "" {
		sessionConfig.CodexIdToken = &config.CodexIdToken
	}
	if config.OpenAIAPIKey != "" {
		sessionConfig.OpenaiApiKey = &config.OpenAIAPIKey
	}
	if config.MistralAPIKey != "" {
		sessionConfig.MistralApiKey = &config.MistralAPIKey
	}
	if config.ReasoningEffort != "" {
		sessionConfig.ReasoningEffort = &config.ReasoningEffort
	}
	if config.OllamaURL != "" {
		sessionConfig.OllamaUrl = &config.OllamaURL
	}
	if config.OpenCodeAPIKey != "" {
		sessionConfig.OpencodeApiKey = &config.OpenCodeAPIKey
	}
	if config.ZaiAPIKey != "" {
		sessionConfig.ZaiApiKey = &config.ZaiAPIKey
	}

	if err := conn.send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_Registered{
			Registered: &v1.AgentRegistered{
				Success: true,
				Config:  sessionConfig,
			},
		},
	}); err != nil {
		return err
	}

	// Register this agent connection with the session manager
	h.manager.RegisterAgentConnection(sessionID, conn)
	defer h.manager.UnregisterAgentConnection(sessionID)

	slog.Info("Agent registered (direct mode)", "sessionID", sessionID)

	// Start outbound message sender
	go conn.sendLoop()

	// Handle incoming messages from agent
	err = conn.receiveLoop(ctx)

	close(conn.done)
	slog.Info("Agent disconnected", "sessionID", conn.sessionID)

	return err
}

// receiveLoop handles incoming messages from the agent.
func (c *AgentConnection) receiveLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		default:
		}

		msg, err := c.stream.Receive()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			slog.Warn("Agent recv error", "sessionID", c.sessionID, "error", err)
			return err
		}

		if err := c.handleMessage(ctx, msg); err != nil {
			slog.Warn("Agent message handler error", "sessionID", c.sessionID, "error", err)
		}
	}
}

// handleMessage processes an incoming message from the agent.
func (c *AgentConnection) handleMessage(ctx context.Context, msg *v1.AgentMessage) error {
	switch m := msg.Message.(type) {
	case *v1.AgentMessage_PromptResponse:
		return c.handlePromptResponse(ctx, m.PromptResponse)
	case *v1.AgentMessage_TerminalOutput:
		return c.handleTerminalOutput(ctx, m.TerminalOutput)
	case *v1.AgentMessage_TitleResponse:
		return c.handleTitleResponse(ctx, m.TitleResponse)
	case *v1.AgentMessage_GitStatusResponse:
		return c.handleGitStatusResponse(ctx, m.GitStatusResponse)
	case *v1.AgentMessage_GitDiffResponse:
		return c.handleGitDiffResponse(ctx, m.GitDiffResponse)
	default:
		slog.Warn("Unknown agent message type", "sessionID", c.sessionID)
		return nil
	}
}

// handlePromptResponse forwards agent streaming responses to clients.
func (c *AgentConnection) handlePromptResponse(ctx context.Context, resp *v1.AgentStreamResponse) error {
	return c.manager.HandleAgentResponse(ctx, c.sessionID, resp)
}

// handleTerminalOutput forwards terminal output to clients.
func (c *AgentConnection) handleTerminalOutput(ctx context.Context, output *v1.AgentTerminalOutput) error {
	return c.manager.HandleTerminalOutput(ctx, c.sessionID, output.Data)
}

// handleTitleResponse handles title generation response.
func (c *AgentConnection) handleTitleResponse(ctx context.Context, resp *v1.AgentTitleResponse) error {
	return c.manager.HandleTitleResponse(ctx, c.sessionID, resp.RequestId, resp.Title)
}

// handleGitStatusResponse handles git status response.
func (c *AgentConnection) handleGitStatusResponse(ctx context.Context, resp *v1.AgentGitStatusResponse) error {
	return c.manager.HandleGitStatusResponse(ctx, c.sessionID, resp.RequestId, resp.Files)
}

// handleGitDiffResponse handles git diff response.
func (c *AgentConnection) handleGitDiffResponse(ctx context.Context, resp *v1.AgentGitDiffResponse) error {
	return c.manager.HandleGitDiffResponse(ctx, c.sessionID, resp.RequestId, resp.Diff)
}

// sendLoop sends outbound messages to the agent.
func (c *AgentConnection) sendLoop() {
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.outbound:
			if err := c.send(msg); err != nil {
				slog.Warn("Failed to send to agent", "sessionID", c.sessionID, "error", err)
				return
			}
		}
	}
}

// send sends a message to the agent.
func (c *AgentConnection) send(msg *v1.ControlPlaneMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.stream.Send(msg)
}

// Send queues a message to be sent to the agent (called by session manager).
func (c *AgentConnection) Send(msg *v1.ControlPlaneMessage) error {
	select {
	case <-c.done:
		return errors.New("agent connection closed")
	case c.outbound <- msg:
		return nil
	default:
		return errors.New("agent outbound channel full")
	}
}

// ExecutePrompt sends a prompt execution request to the agent.
func (c *AgentConnection) ExecutePrompt(text string) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_ExecutePrompt{
			ExecutePrompt: &v1.ExecutePromptRequest{
				Text: text,
			},
		},
	})
}

// Interrupt sends an interrupt request to the agent.
func (c *AgentConnection) Interrupt() error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_Interrupt{
			Interrupt: &v1.InterruptRequest{},
		},
	})
}

// GenerateTitle sends a title generation request to the agent.
func (c *AgentConnection) GenerateTitle(requestID, prompt string) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_GenerateTitle{
			GenerateTitle: &v1.GenerateTitleRequest{
				RequestId: requestID,
				Prompt:    prompt,
			},
		},
	})
}

// GetGitStatus sends a git status request to the agent.
func (c *AgentConnection) GetGitStatus(requestID string) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_GetGitStatus{
			GetGitStatus: &v1.GetGitStatusRequest{
				RequestId: requestID,
			},
		},
	})
}

// GetGitDiff sends a git diff request to the agent.
func (c *AgentConnection) GetGitDiff(requestID string, file *string) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_GetGitDiff{
			GetGitDiff: &v1.GetGitDiffRequest{
				RequestId: requestID,
				File:      file,
			},
		},
	})
}

// SendTerminalInput sends terminal input to the agent.
func (c *AgentConnection) SendTerminalInput(data string) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_TerminalInput{
			TerminalInput: &v1.AgentTerminalInput{
				Input: &v1.AgentTerminalInput_Data{
					Data: data,
				},
			},
		},
	})
}

// ResizeTerminal sends a terminal resize request to the agent.
func (c *AgentConnection) ResizeTerminal(cols, rows int) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_TerminalInput{
			TerminalInput: &v1.AgentTerminalInput{
				Input: &v1.AgentTerminalInput_Resize{
					Resize: &v1.AgentTerminalResize{
						Cols: int32(cols),
						Rows: int32(rows),
					},
				},
			},
		},
	})
}

// UpdateGitCredentials sends updated git credentials to the agent.
func (c *AgentConnection) UpdateGitCredentials(token string, repoAccess v1.RepoAccess) error {
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_UpdateGitCredentials{
			UpdateGitCredentials: &v1.UpdateGitCredentials{
				GithubToken: token,
				RepoAccess:  repoAccess,
			},
		},
	})
}

// UpdateCodexAuth sends updated short-lived Codex OAuth tokens to the agent.
func (c *AgentConnection) UpdateCodexAuth(accessToken, idToken string, expiresAt *timestamppb.Timestamp) error {
	update := &v1.UpdateCodexAuth{
		AccessToken: accessToken,
		IdToken:     idToken,
	}
	if expiresAt != nil {
		update.ExpiresAt = expiresAt
	}
	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_UpdateCodexAuth{
			UpdateCodexAuth: update,
		},
	})
}

// AssignSession assigns a session to a warm pool agent (implements WarmAgentConnection).
// This pushes the SessionAssigned message to the agent for instant session start.
func (c *AgentConnection) AssignSession(sessionID string, config *session.AgentSessionConfig) error {
	c.sessionID = sessionID

	// Build session config proto
	sessionConfig := &v1.SessionConfig{
		SessionId:       sessionID,
		WorkspaceDir:    "/workspace",
		ControlPlaneUrl: "http://control-plane.netclode.svc.cluster.local",
		SdkType:         config.SdkType,
		CopilotBackend:  config.CopilotBackend,
	}
	if len(config.Repos) > 0 {
		sessionConfig.Repos = config.Repos
	}
	if config.RepoAccess != nil {
		sessionConfig.RepoAccess = config.RepoAccess
	}
	if config.GitHubToken != "" {
		sessionConfig.GithubToken = &config.GitHubToken
	}
	if config.GitHubCopilotToken != "" {
		sessionConfig.GithubCopilotToken = &config.GitHubCopilotToken
	}
	if config.Model != "" {
		sessionConfig.Model = &config.Model
	}
	if config.CodexAccessToken != "" {
		sessionConfig.CodexAccessToken = &config.CodexAccessToken
	}
	if config.CodexIdToken != "" {
		sessionConfig.CodexIdToken = &config.CodexIdToken
	}
	if config.OpenAIAPIKey != "" {
		sessionConfig.OpenaiApiKey = &config.OpenAIAPIKey
	}
	if config.MistralAPIKey != "" {
		sessionConfig.MistralApiKey = &config.MistralAPIKey
	}
	if config.ReasoningEffort != "" {
		sessionConfig.ReasoningEffort = &config.ReasoningEffort
	}
	if config.OllamaURL != "" {
		sessionConfig.OllamaUrl = &config.OllamaURL
	}
	if config.OpenCodeAPIKey != "" {
		sessionConfig.OpencodeApiKey = &config.OpenCodeAPIKey
	}
	if config.ZaiAPIKey != "" {
		sessionConfig.ZaiApiKey = &config.ZaiAPIKey
	}

	slog.Info("Pushing session assignment to warm agent", "sessionID", sessionID, "podName", c.podName)

	return c.Send(&v1.ControlPlaneMessage{
		Message: &v1.ControlPlaneMessage_SessionAssigned{
			SessionAssigned: &v1.SessionAssigned{
				SessionId: sessionID,
				Config:    sessionConfig,
			},
		},
	})
}

func strPtr(s string) *string {
	return &s
}
