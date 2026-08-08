package controlplane

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
	"golang.org/x/net/http2"
)

// Client wraps the Connect protocol client for control-plane communication.
type Client struct {
	client netclodev1connect.ClientServiceClient
}

// New creates a new control-plane client.
// Uses h2c (HTTP/2 cleartext) for in-cluster plaintext connections,
// which is required for Connect protocol bidi streaming.
func New(baseURL, tokenPath string) *Client {
	h2cTransport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	httpClient := &http.Client{
		Transport: &bearerFileRoundTripper{
			tokenPath: tokenPath,
			next:      otelhttp.NewTransport(h2cTransport),
		},
	}
	return &Client{
		client: netclodev1connect.NewClientServiceClient(httpClient, baseURL),
	}
}

type bearerFileRoundTripper struct {
	tokenPath string
	next      http.RoundTripper
}

func (t *bearerFileRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := os.ReadFile(t.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read control-plane token: %w", err)
	}
	trimmedToken := strings.TrimSpace(string(token))
	if trimmedToken == "" {
		return nil, fmt.Errorf("control-plane token is empty")
	}
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+trimmedToken)
	return t.next.RoundTrip(cloned)
}

// RunSessionOpts contains options for running a session.
type RunSessionOpts struct {
	Repo        string // "owner/repo"
	RepoAccess  pb.RepoAccess
	SdkType     pb.SdkType
	Model       string
	Prompt      string // The initial prompt to send
	SessionName string
	OnSessionID func(sessionID string) // Called when session ID is known (for tracking)
}

// RunSessionResult contains the result of a session run.
type RunSessionResult struct {
	SessionID string
	Response  string // Accumulated assistant response text
}

// RunSession creates a session, sends a prompt via initial_prompt, streams the
// response until completion, and returns the accumulated result.
func (c *Client) RunSession(ctx context.Context, opts RunSessionOpts) (*RunSessionResult, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	// Build CreateSessionRequest with initial_prompt
	req := &pb.CreateSessionRequest{
		Repos:         []string{opts.Repo},
		InitialPrompt: &opts.Prompt,
	}
	if opts.SessionName != "" {
		req.Name = &opts.SessionName
	}
	if opts.RepoAccess != pb.RepoAccess_REPO_ACCESS_UNSPECIFIED {
		req.RepoAccess = &opts.RepoAccess
	}
	if opts.SdkType != pb.SdkType_SDK_TYPE_UNSPECIFIED {
		req.SdkType = &opts.SdkType
	}
	if opts.Model != "" {
		req.Model = &opts.Model
	}

	slog.Info("Creating session", "repo", opts.Repo, "sdk", opts.SdkType.String(), "model", opts.Model)

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_CreateSession{
			CreateSession: req,
		},
	}); err != nil {
		return nil, fmt.Errorf("send CreateSession: %w", err)
	}

	result := &RunSessionResult{}
	var wasRunning bool
	var responseBuilder strings.Builder

	// Read loop: process server messages until agent completes
	for {
		select {
		case <-ctx.Done():
			// Timeout or cancellation — return partial result alongside the error
			// so the caller can decide whether to post a partial response
			result.Response = responseBuilder.String()
			slog.Warn("Session timed out", "sessionID", result.SessionID, "responseLen", len(result.Response))
			return result, fmt.Errorf("session timed out: %w", ctx.Err())
		default:
		}

		msg, err := stream.Receive()
		if err != nil {
			// Stream closed or context cancelled — return accumulated response
			// alongside the error so the caller knows the session was interrupted.
			result.Response = responseBuilder.String()
			return result, fmt.Errorf("receive: %w", err)
		}

		switch m := msg.GetMessage().(type) {
		case *pb.ServerMessage_SessionCreated:
			result.SessionID = m.SessionCreated.GetSession().GetId()
			slog.Info("Session created", "sessionID", result.SessionID, "status", m.SessionCreated.GetSession().GetStatus().String())
			if opts.OnSessionID != nil {
				opts.OnSessionID(result.SessionID)
			}

		case *pb.ServerMessage_SessionUpdated:
			sess := m.SessionUpdated.GetSession()
			status := sess.GetStatus()
			slog.Debug("Session updated", "sessionID", sess.GetId(), "status", status.String())

			if result.SessionID == "" {
				result.SessionID = sess.GetId()
			}

			switch status {
			case pb.SessionStatus_SESSION_STATUS_RUNNING:
				wasRunning = true
			case pb.SessionStatus_SESSION_STATUS_ERROR:
				return result, fmt.Errorf("session entered error state")
			}

		case *pb.ServerMessage_StreamEntry:
			entry := m.StreamEntry.GetEntry()
			if entry == nil {
				continue
			}

			switch payload := entry.GetPayload().(type) {
			case *pb.StreamEntry_Event:
				evt := payload.Event
				if evt.GetKind() == pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE {
					msgPayload := evt.GetMessage()
					if msgPayload != nil && msgPayload.GetRole() == pb.MessageRole_MESSAGE_ROLE_ASSISTANT {
						if !entry.GetPartial() {
							// Keep only the last assistant message (discard intermediate narration)
							responseBuilder.Reset()
							responseBuilder.WriteString(msgPayload.GetContent())
						}
					}
				}

			case *pb.StreamEntry_SessionUpdate:
				status := payload.SessionUpdate.GetStatus()
				if wasRunning && status == pb.SessionStatus_SESSION_STATUS_READY {
					// Agent finished — this is the completion signal
					slog.Info("Agent completed", "sessionID", result.SessionID, "responseLen", responseBuilder.Len())
					result.Response = responseBuilder.String()
					return result, nil
				}
				if status == pb.SessionStatus_SESSION_STATUS_RUNNING {
					wasRunning = true
				}
				if status == pb.SessionStatus_SESSION_STATUS_ERROR {
					result.Response = responseBuilder.String()
					return result, fmt.Errorf("session error during execution")
				}

			case *pb.StreamEntry_Error:
				result.Response = responseBuilder.String()
				return result, fmt.Errorf("stream error: %s: %s", payload.Error.GetCode(), payload.Error.GetMessage())
			}

		case *pb.ServerMessage_Error:
			return result, fmt.Errorf("server error: %s: %s", m.Error.GetError().GetCode(), m.Error.GetError().GetMessage())

		case *pb.ServerMessage_SessionState:
			// OpenSession response — we don't send OpenSession, but handle it just in case
			sess := m.SessionState.GetSession()
			if sess != nil {
				result.SessionID = sess.GetId()
			}
		}
	}
}

// DeleteSession sends a delete request for a session.
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_DeleteSession{
			DeleteSession: &pb.DeleteSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return fmt.Errorf("send DeleteSession: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive: %w", err)
	}

	if msg.GetSessionDeleted() != nil {
		return nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.GetError().GetCode(), errResp.GetError().GetMessage())
	}

	return nil
}

// RecoverSession opens an existing session and collects its result.
// If the session is already done (READY), it extracts the response from stream entries.
// If still RUNNING, it streams until completion.
// Returns the accumulated response, or an error if the session is gone/errored.
func (c *Client) RecoverSession(ctx context.Context, sessionID string) (*RunSessionResult, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	slog.Info("Recovering session", "sessionID", sessionID)

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_OpenSession{
			OpenSession: &pb.OpenSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send OpenSession: %w", err)
	}

	result := &RunSessionResult{SessionID: sessionID}
	var responseBuilder strings.Builder

	// First message should be SessionStateResponse
	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive session state: %w", err)
	}

	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("session not found: %s: %s", errResp.GetError().GetCode(), errResp.GetError().GetMessage())
	}

	state := msg.GetSessionState()
	if state == nil {
		return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
	}

	sess := state.GetSession()
	status := sess.GetStatus()
	slog.Info("Recovered session state", "sessionID", sessionID, "status", status.String(), "entries", len(state.GetEntries()))

	// Extract last assistant message from existing stream entries
	for _, entry := range state.GetEntries() {
		if entry.GetPartial() {
			continue
		}
		if evt := entry.GetEvent(); evt != nil {
			if evt.GetKind() == pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE {
				if msgPayload := evt.GetMessage(); msgPayload != nil && msgPayload.GetRole() == pb.MessageRole_MESSAGE_ROLE_ASSISTANT {
					responseBuilder.Reset()
					responseBuilder.WriteString(msgPayload.GetContent())
				}
			}
		}
	}

	// If session is already done, return what we have
	switch status {
	case pb.SessionStatus_SESSION_STATUS_READY:
		result.Response = responseBuilder.String()
		slog.Info("Session already completed", "sessionID", sessionID, "responseLen", len(result.Response))
		return result, nil
	case pb.SessionStatus_SESSION_STATUS_ERROR:
		result.Response = responseBuilder.String()
		return result, fmt.Errorf("session in error state")
	case pb.SessionStatus_SESSION_STATUS_RUNNING:
		// Continue streaming below
	default:
		// CREATING, PAUSED, etc — not recoverable in a useful way
		return result, fmt.Errorf("session in unexpected state: %s", status.String())
	}

	// Session is still RUNNING — continue streaming until completion
	for {
		select {
		case <-ctx.Done():
			result.Response = responseBuilder.String()
			slog.Warn("Recovery timed out", "sessionID", sessionID, "responseLen", len(result.Response))
			return result, fmt.Errorf("recovery timed out: %w", ctx.Err())
		default:
		}

		msg, err := stream.Receive()
		if err != nil {
			if responseBuilder.Len() > 0 {
				result.Response = responseBuilder.String()
				return result, nil
			}
			return nil, fmt.Errorf("receive during recovery: %w", err)
		}

		if entry := msg.GetStreamEntry(); entry != nil {
			e := entry.GetEntry()
			if e == nil {
				continue
			}

			switch payload := e.GetPayload().(type) {
			case *pb.StreamEntry_Event:
				if payload.Event.GetKind() == pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE {
					if msgPayload := payload.Event.GetMessage(); msgPayload != nil && msgPayload.GetRole() == pb.MessageRole_MESSAGE_ROLE_ASSISTANT && !e.GetPartial() {
						responseBuilder.Reset()
						responseBuilder.WriteString(msgPayload.GetContent())
					}
				}
			case *pb.StreamEntry_SessionUpdate:
				s := payload.SessionUpdate.GetStatus()
				if s == pb.SessionStatus_SESSION_STATUS_READY {
					result.Response = responseBuilder.String()
					slog.Info("Session completed during recovery", "sessionID", sessionID, "responseLen", len(result.Response))
					return result, nil
				}
				if s == pb.SessionStatus_SESSION_STATUS_ERROR {
					result.Response = responseBuilder.String()
					return result, fmt.Errorf("session errored during recovery")
				}
			case *pb.StreamEntry_Error:
				result.Response = responseBuilder.String()
				return result, fmt.Errorf("stream error: %s: %s", payload.Error.GetCode(), payload.Error.GetMessage())
			}
		}

		if errResp := msg.GetError(); errResp != nil {
			return result, fmt.Errorf("server error: %s: %s", errResp.GetError().GetCode(), errResp.GetError().GetMessage())
		}
	}
}

// ParseSdkType converts a string SDK type to protobuf enum.
func ParseSdkType(s string) pb.SdkType {
	switch strings.ToLower(s) {
	case "claude":
		return pb.SdkType_SDK_TYPE_CLAUDE
	case "opencode":
		return pb.SdkType_SDK_TYPE_OPENCODE
	case "copilot":
		return pb.SdkType_SDK_TYPE_COPILOT
	case "codex":
		return pb.SdkType_SDK_TYPE_CODEX
	default:
		return pb.SdkType_SDK_TYPE_CLAUDE
	}
}
