package client

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
)

// Client wraps the Connect client for CLI operations.
type Client struct {
	baseURL string
	client  netclodev1connect.ClientServiceClient
}

// New creates a new CLI client.
func New(baseURL string) *Client {
	httpClient := &http.Client{}
	return &Client{
		baseURL: baseURL,
		client:  netclodev1connect.NewClientServiceClient(httpClient, baseURL),
	}
}

// CreateSessionOptions contains options for creating a session.
type CreateSessionOptions struct {
	Name          string
	Repos         []string
	SdkType       pb.SdkType
	Model         string
	TailnetAccess bool  // default false
	VCPUs         int32 // Custom vCPUs (0 = use default)
	MemoryMB      int32 // Custom memory in MB (0 = use default)
}

// CreateSession creates a new session and returns it.
func (c *Client) CreateSession(ctx context.Context, opts CreateSessionOptions) (*pb.Session, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	req := &pb.CreateSessionRequest{}
	if opts.Name != "" {
		req.Name = &opts.Name
	}
	if len(opts.Repos) > 0 {
		req.Repos = opts.Repos
	}
	if opts.SdkType != pb.SdkType_SDK_TYPE_UNSPECIFIED {
		req.SdkType = &opts.SdkType
	}
	if opts.Model != "" {
		req.Model = &opts.Model
	}
	// Set network config if tailnet access is requested (non-default)
	if opts.TailnetAccess {
		req.NetworkConfig = &pb.NetworkConfig{
			TailnetAccess: opts.TailnetAccess,
		}
	}
	// Set custom resources if specified (bypasses warm pool)
	if opts.VCPUs > 0 || opts.MemoryMB > 0 {
		req.Resources = &pb.SandboxResources{
			Vcpus:    opts.VCPUs,
			MemoryMb: opts.MemoryMB,
		}
	}
	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_CreateSession{
			CreateSession: req,
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// May receive SessionCreated followed by SessionUpdated as sandbox boots
	for {
		msg, err := stream.Receive()
		if err != nil {
			return nil, fmt.Errorf("receive response: %w", err)
		}

		if resp := msg.GetSessionCreated(); resp != nil {
			return resp.Session, nil
		}
		if resp := msg.GetSessionUpdated(); resp != nil {
			// Session created and updated (sandbox started)
			return resp.Session, nil
		}
		if errResp := msg.GetError(); errResp != nil {
			return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
		}
		// Skip other message types (e.g., notifications) and wait for session response
	}
}

// ListSessions returns all sessions.
func (c *Client) ListSessions(ctx context.Context) ([]*pb.Session, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_ListSessions{
			ListSessions: &pb.ListSessionsRequest{},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetSessionList(); resp != nil {
		return resp.Sessions, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// SyncSessions returns all sessions with metadata (message counts).
func (c *Client) SyncSessions(ctx context.Context) ([]*pb.SessionSummary, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_Sync{
			Sync: &pb.SyncRequest{},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetSyncResponse(); resp != nil {
		return resp.Sessions, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// SessionState contains session details with stream entries.
type SessionState struct {
	Session      *pb.Session
	Entries      []*pb.StreamEntry
	HasMore      bool
	LastStreamID string
	InProgress   *pb.InProgressState
}

// GetSession returns a session with its stream entries.
func (c *Client) GetSession(ctx context.Context, sessionID string) (*SessionState, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_OpenSession{
			OpenSession: &pb.OpenSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetSessionState(); resp != nil {
		state := &SessionState{
			Session:    resp.Session,
			Entries:    resp.GetEntries(),
			HasMore:    resp.HasMore,
			InProgress: resp.GetInProgress(),
		}
		if resp.LastStreamId != nil {
			state.LastStreamID = *resp.LastStreamId
		}

		return state, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// DeleteSession deletes a session.
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
		return fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive response: %w", err)
	}

	if msg.GetSessionDeleted() != nil {
		return nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// StreamEntryHandler is called for each stream entry received during tailing.
type StreamEntryHandler func(entry *pb.StreamEntryResponse) error

// TailEvents opens a session and streams events in real-time.
func (c *Client) TailEvents(ctx context.Context, sessionID string, onEntry StreamEntryHandler) error {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	// Open session to start receiving events
	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_OpenSession{
			OpenSession: &pb.OpenSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	// First message should be SessionStateResponse
	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive initial response: %w", err)
	}

	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	// Stream entries
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := stream.Receive()
		if err != nil {
			return err
		}

		if entry := msg.GetStreamEntry(); entry != nil && onEntry != nil {
			if err := onEntry(entry); err != nil {
				return err
			}
		}
	}
}

// Stream returns a raw bidirectional stream for advanced usage.
func (c *Client) Stream(ctx context.Context) *connect.BidiStreamForClient[pb.ClientMessage, pb.ServerMessage] {
	return c.client.Connect(ctx)
}

// ListSnapshots returns all snapshots for a session.
func (c *Client) ListSnapshots(ctx context.Context, sessionID string) ([]*pb.Snapshot, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_ListSnapshots{
			ListSnapshots: &pb.ListSnapshotsRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetSnapshotList(); resp != nil {
		return resp.Snapshots, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// PauseSession pauses a session.
func (c *Client) PauseSession(ctx context.Context, sessionID string) (*pb.Session, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_PauseSession{
			PauseSession: &pb.PauseSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetSessionUpdated(); resp != nil {
		return resp.Session, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// ResumeSession resumes a paused session.
func (c *Client) ResumeSession(ctx context.Context, sessionID string) (*pb.Session, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_ResumeSession{
			ResumeSession: &pb.ResumeSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetSessionUpdated(); resp != nil {
		return resp.Session, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// ListModels returns available models for the specified SDK type.
func (c *Client) ListModels(ctx context.Context, sdkType pb.SdkType, copilotBackend *pb.CopilotBackend) ([]*pb.ModelInfo, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	req := &pb.ListModelsRequest{
		SdkType:        sdkType,
		CopilotBackend: copilotBackend,
	}

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_ListModels{
			ListModels: req,
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}

	if resp := msg.GetModels(); resp != nil {
		return resp.Models, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

func (c *Client) CodexAuthStart(ctx context.Context) (*pb.CodexAuthStartedResponse, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_CodexAuthStart{
			CodexAuthStart: &pb.CodexAuthStartRequest{},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}
	if resp := msg.GetCodexAuthStarted(); resp != nil {
		return resp, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}
	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

func (c *Client) CodexAuthStatus(ctx context.Context) (*pb.CodexAuthStatusResponse, error) {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_CodexAuthStatus{
			CodexAuthStatus: &pb.CodexAuthStatusRequest{},
		},
	}); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("receive response: %w", err)
	}
	if resp := msg.GetCodexAuthStatus(); resp != nil {
		return resp, nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return nil, fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}
	return nil, fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

func (c *Client) CodexAuthLogout(ctx context.Context) error {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_CodexAuthLogout{
			CodexAuthLogout: &pb.CodexAuthLogoutRequest{},
		},
	}); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive response: %w", err)
	}
	if msg.GetCodexAuthLoggedOut() != nil {
		return nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}
	return fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}

// RestoreSnapshot restores a session to a snapshot.
func (c *Client) RestoreSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	stream := c.client.Connect(ctx)
	defer func() { _ = stream.CloseRequest() }()

	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_RestoreSnapshot{
			RestoreSnapshot: &pb.RestoreSnapshotRequest{
				SessionId:  sessionID,
				SnapshotId: snapshotID,
			},
		},
	}); err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive response: %w", err)
	}

	if msg.GetSnapshotRestored() != nil {
		return nil
	}
	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}

	return fmt.Errorf("unexpected response type: %T", msg.GetMessage())
}
