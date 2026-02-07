package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
	"github.com/angristan/netclode/services/control-plane/internal/github"
	"github.com/angristan/netclode/services/control-plane/internal/session"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Validation errors for client requests
var (
	errSessionIDRequired = errors.New("sessionId is required")
	errTextRequired      = errors.New("text is required")
	errUnknownMessage    = errors.New("unknown message type")
)

// makeErrorResponse creates a unified error response.
func makeErrorResponse(sessionID, code, message string) *pb.ServerMessage {
	var sessID *string
	if sessionID != "" {
		sessID = &sessionID
	}
	return &pb.ServerMessage{
		Message: &pb.ServerMessage_Error{
			Error: &pb.ErrorResponse{
				Error: &pb.Error{
					Code:      code,
					Message:   message,
					SessionId: sessID,
				},
			},
		},
	}
}

// Ensure ConnectClientServiceHandler implements the interface
var _ netclodev1connect.ClientServiceHandler = (*ConnectClientServiceHandler)(nil)

// subscriptionInfo holds subscription state for a session.
type subscriptionInfo struct {
	sub    *session.StreamSubscriber
	cancel context.CancelFunc
}

// ConnectClientServiceHandler implements the Connect ClientService.
type ConnectClientServiceHandler struct {
	netclodev1connect.UnimplementedClientServiceHandler
	manager *session.Manager
	server  *Server
}

// NewConnectClientServiceHandler creates a new Connect client service handler.
func NewConnectClientServiceHandler(manager *session.Manager, server *Server) *ConnectClientServiceHandler {
	return &ConnectClientServiceHandler{
		manager: manager,
		server:  server,
	}
}

// ConnectConnection represents a Connect bidirectional stream connection.
type ConnectConnection struct {
	stream  *connect.BidiStream[pb.ClientMessage, pb.ServerMessage]
	manager *session.Manager
	server  *Server

	// Redis Streams-based subscriptions
	subscriptions map[string]*subscriptionInfo // sessionID -> subscription info
	subMu         sync.Mutex

	// Global messages channel (session create/delete events)
	globalMessages chan *pb.ServerMessage

	// For graceful shutdown
	done    chan struct{}
	closed  bool // true when connection is closed, protected by writeMu
	writeMu sync.Mutex
}

// Connect implements the bidirectional streaming RPC using Connect protocol.
func (h *ConnectClientServiceHandler) Connect(ctx context.Context, stream *connect.BidiStream[pb.ClientMessage, pb.ServerMessage]) error {
	conn := &ConnectConnection{
		stream:         stream,
		manager:        h.manager,
		server:         h.server,
		subscriptions:  make(map[string]*subscriptionInfo),
		globalMessages: make(chan *pb.ServerMessage, 64),
		done:           make(chan struct{}),
	}

	// Track connection
	h.server.connectConnections.Store(conn, struct{}{})
	h.server.connCount.Add(1)
	h.server.wg.Add(1)

	slog.Info("Connect connection opened", "activeConnections", h.server.connCount.Load())

	// Start goroutine to forward global messages
	go conn.forwardGlobalMessages()

	// Handle the connection
	err := conn.run(ctx)

	// Cleanup
	conn.close()
	h.server.connectConnections.Delete(conn)
	h.server.connCount.Add(-1)
	h.server.wg.Done()

	slog.Info("Connect connection closed", "activeConnections", h.server.connCount.Load())

	return err
}

// run handles the Connect connection lifecycle.
func (c *ConnectConnection) run(ctx context.Context) error {
	for {
		// Check for shutdown before blocking on Receive
		select {
		case <-c.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue to receive
		}

		// Receive blocks until a message arrives or the stream closes.
		// We check ctx.Done() after receive errors to handle cancellation.
		msg, err := c.stream.Receive()
		if err != nil {
			// Check for graceful close first
			if err == io.EOF {
				return nil
			}
			// Check if context was cancelled while we were blocked
			select {
			case <-c.done:
				return nil
			case <-ctx.Done():
				return nil
			default:
			}
			slog.Warn("Connect recv error", "error", err)
			return err
		}

		if err := c.handleMessage(ctx, msg); err != nil {
			slog.Warn("Connect handler error", "error", err)
			c.send(makeErrorResponse("", "HANDLER_ERROR", err.Error()))
		}
	}
}

// handleMessage dispatches a client message to the appropriate handler.
func (c *ConnectConnection) handleMessage(ctx context.Context, msg *pb.ClientMessage) error {
	switch m := msg.Message.(type) {
	case *pb.ClientMessage_CreateSession:
		return c.handleSessionCreate(ctx, m.CreateSession)
	case *pb.ClientMessage_ListSessions:
		return c.handleSessionList(ctx)
	case *pb.ClientMessage_OpenSession:
		return c.handleSessionOpen(ctx, m.OpenSession)
	case *pb.ClientMessage_ResumeSession:
		return c.handleSessionResume(ctx, m.ResumeSession.SessionId)
	case *pb.ClientMessage_PauseSession:
		return c.handleSessionPause(ctx, m.PauseSession.SessionId)
	case *pb.ClientMessage_DeleteSession:
		return c.handleSessionDelete(ctx, m.DeleteSession.SessionId)
	case *pb.ClientMessage_DeleteAllSessions:
		return c.handleSessionDeleteAll(ctx)
	case *pb.ClientMessage_SendPrompt:
		return c.handlePrompt(ctx, m.SendPrompt.SessionId, m.SendPrompt.Text)
	case *pb.ClientMessage_InterruptPrompt:
		return c.handlePromptInterrupt(ctx, m.InterruptPrompt.SessionId)
	case *pb.ClientMessage_TerminalInput:
		return c.handleTerminalInput(ctx, m.TerminalInput.SessionId, m.TerminalInput.Data)
	case *pb.ClientMessage_TerminalResize:
		return c.handleTerminalResize(ctx, m.TerminalResize)
	case *pb.ClientMessage_Sync:
		return c.handleSync(ctx)
	case *pb.ClientMessage_ExposePort:
		return c.handlePortExpose(ctx, m.ExposePort.SessionId, int(m.ExposePort.Port))
	case *pb.ClientMessage_ListGithubRepos:
		return c.handleGitHubReposList(ctx)
	case *pb.ClientMessage_GitStatus:
		return c.handleGitStatus(ctx, m.GitStatus.SessionId)
	case *pb.ClientMessage_GitDiff:
		return c.handleGitDiff(ctx, m.GitDiff)
	case *pb.ClientMessage_ListModels:
		return c.handleListModels(ctx, m.ListModels)
	case *pb.ClientMessage_GetCopilotStatus:
		return c.handleGetCopilotStatus(ctx)
	case *pb.ClientMessage_ListSnapshots:
		return c.handleListSnapshots(ctx, m.ListSnapshots)
	case *pb.ClientMessage_RestoreSnapshot:
		return c.handleRestoreSnapshot(ctx, m.RestoreSnapshot)
	case *pb.ClientMessage_UpdateRepoAccess:
		return c.handleUpdateRepoAccess(ctx, m.UpdateRepoAccess)
	case *pb.ClientMessage_GetResourceLimits:
		return c.handleGetResourceLimits(ctx, m.GetResourceLimits)
	case *pb.ClientMessage_CodexAuthStart:
		return c.handleCodexAuthStart(ctx, m.CodexAuthStart)
	case *pb.ClientMessage_CodexAuthStatus:
		return c.handleCodexAuthStatus(ctx, m.CodexAuthStatus)
	case *pb.ClientMessage_CodexAuthLogout:
		return c.handleCodexAuthLogout(ctx, m.CodexAuthLogout)
	default:
		return connect.NewError(connect.CodeInvalidArgument, errUnknownMessage)
	}
}

// send sends a message to the client.
func (c *ConnectConnection) send(msg *pb.ServerMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return fmt.Errorf("connection closed")
	}
	return c.stream.Send(msg)
}

// close closes the connection and cancels all subscriptions.
func (c *ConnectConnection) close() {
	// Mark as closed first to prevent new sends
	c.writeMu.Lock()
	c.closed = true
	c.writeMu.Unlock()

	select {
	case <-c.done:
		return
	default:
		close(c.done)
	}

	c.subMu.Lock()
	for _, info := range c.subscriptions {
		info.cancel()
	}
	c.subscriptions = make(map[string]*subscriptionInfo)
	c.subMu.Unlock()
}

// forwardGlobalMessages reads from the global messages channel and sends to stream.
func (c *ConnectConnection) forwardGlobalMessages() {
	for {
		select {
		case msg, ok := <-c.globalMessages:
			if !ok {
				return
			}
			if err := c.send(msg); err != nil {
				slog.Debug("Failed to forward global message", "error", err)
				return
			}
		case <-c.done:
			return
		}
	}
}

// subscribe adds a subscription for a session.
func (c *ConnectConnection) subscribe(_ context.Context, sessionID string, lastNotificationID string) error {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	if old, ok := c.subscriptions[sessionID]; ok {
		old.cancel()
		delete(c.subscriptions, sessionID)
	}

	subCtx, cancel := context.WithCancel(context.Background())

	sub, err := c.manager.Subscribe(subCtx, sessionID, lastNotificationID)
	if err != nil {
		cancel()
		return err
	}

	c.subscriptions[sessionID] = &subscriptionInfo{
		sub:    sub,
		cancel: cancel,
	}

	go c.forwardMessages(sessionID, sub)

	return nil
}

// forwardMessages reads from the StreamSubscriber and sends to stream.
func (c *ConnectConnection) forwardMessages(sessionID string, sub *session.StreamSubscriber) {
	for {
		select {
		case msg, ok := <-sub.Messages():
			if !ok {
				return
			}
			// Messages are already *pb.ServerMessage from subscriber
			if err := c.send(msg); err != nil {
				slog.Debug("Failed to forward message", "sessionID", sessionID, "error", err)
				return
			}
		case <-c.done:
			return
		}
	}
}

// unsubscribe removes a subscription for a session.
func (c *ConnectConnection) unsubscribe(sessionID string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()

	if info, ok := c.subscriptions[sessionID]; ok {
		info.cancel()
		delete(c.subscriptions, sessionID)
	}
}

// Handler implementations

func (c *ConnectConnection) handleSessionCreate(ctx context.Context, req *pb.CreateSessionRequest) error {
	var repos []string
	var repoAccessPtr *pb.RepoAccess
	var sdkTypePtr *pb.SdkType
	var modelPtr *string
	var copilotBackendPtr *pb.CopilotBackend
	var tailnetAccessPtr *bool
	var resourcesPtr *pb.SandboxResources

	if len(req.Repos) > 0 {
		seen := make(map[string]struct{}, len(req.Repos))
		for _, repo := range req.Repos {
			repo = strings.TrimSpace(repo)
			if repo == "" {
				continue
			}
			normalized := github.NormalizeRepoURL(repo)
			if _, exists := seen[normalized]; exists {
				continue
			}
			seen[normalized] = struct{}{}
			repos = append(repos, normalized)
		}
	}
	if req.RepoAccess != nil {
		repoAccessPtr = req.RepoAccess
	}
	if len(repos) == 0 {
		repoAccessPtr = nil
	}
	if req.SdkType != nil {
		sdkTypePtr = req.SdkType
	}
	if req.Model != nil {
		modelPtr = req.Model
	}
	if req.CopilotBackend != nil {
		copilotBackendPtr = req.CopilotBackend
	}
	if req.NetworkConfig != nil {
		tailnetAccessPtr = &req.NetworkConfig.TailnetAccess
	}
	if req.Resources != nil {
		resourcesPtr = req.Resources
	}
	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	if req.CodexOauthTokens != nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("codex_oauth_tokens are no longer accepted; use backend codex auth flow"))
	}

	if sdkTypePtr != nil && *sdkTypePtr == pb.SdkType_SDK_TYPE_CODEX {
		if modelPtr == nil || codexModelAuthMode(*modelPtr) == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("codex model must include auth suffix (:oauth or :api), e.g. gpt-5.2-codex:oauth:high"))
		}
	}

	sess, err := c.manager.Create(ctx, name, repos, repoAccessPtr, sdkTypePtr, modelPtr, copilotBackendPtr, tailnetAccessPtr, resourcesPtr)
	if err != nil {
		return err
	}

	if err := c.subscribe(ctx, sess.Id, "$"); err != nil {
		slog.Warn("Failed to subscribe to new session", "sessionID", sess.Id, "error", err)
	}

	msg := &pb.ServerMessage{
		Message: &pb.ServerMessage_SessionCreated{
			SessionCreated: &pb.SessionCreatedResponse{Session: sess},
		},
	}

	if err := c.send(msg); err != nil {
		return err
	}

	// Broadcast to other clients
	c.server.BroadcastToAllConnect(msg, c)

	// Send initial prompt if provided
	if req.InitialPrompt != nil && *req.InitialPrompt != "" {
		slog.Info("Sending initial prompt", "sessionID", sess.Id)
		if err := c.manager.SendPrompt(ctx, sess.Id, *req.InitialPrompt); err != nil {
			slog.Warn("Failed to send initial prompt", "sessionID", sess.Id, "error", err)
		}
	}

	return nil
}

func (c *ConnectConnection) handleSessionList(ctx context.Context) error {
	sessions, err := c.manager.List(ctx)
	if err != nil {
		return err
	}

	// Sessions are already pb.Session, just need to convert to pointers
	pbSessions := make([]*pb.Session, len(sessions))
	for i := range sessions {
		pbSessions[i] = &sessions[i]
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_SessionList{
			SessionList: &pb.SessionListResponse{Sessions: pbSessions},
		},
	})
}

func (c *ConnectConnection) handleSessionOpen(ctx context.Context, req *pb.OpenSessionRequest) error {
	afterStreamID := ""
	if req.AfterStreamId != nil {
		afterStreamID = *req.AfterStreamId
	}
	limit := 0 // no limit by default
	if req.Limit != nil {
		limit = int(*req.Limit)
	}

	sess, entries, hasMore, lastStreamID, inProgress, err := c.manager.GetWithHistory(ctx, req.SessionId, afterStreamID, limit)
	if err != nil {
		return err
	}

	// Subscribe to real-time updates starting from current position
	if err := c.subscribe(ctx, req.SessionId, lastStreamID); err != nil {
		slog.Warn("Failed to subscribe to opened session", "sessionID", req.SessionId, "error", err)
	}

	// Convert storage entries to proto StreamEntry
	pbEntries := make([]*pb.StreamEntry, len(entries))
	for i, e := range entries {
		pbEntries[i] = storageEntryToProto(e)
	}

	resp := &pb.SessionStateResponse{
		Session:      sess,
		Entries:      pbEntries,
		HasMore:      hasMore,
		LastStreamId: &lastStreamID,
		InProgress:   inProgress,
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_SessionState{SessionState: resp},
	})
}

func (c *ConnectConnection) handleSessionResume(ctx context.Context, id string) error {
	sess, err := c.manager.Resume(ctx, id)
	if err != nil {
		return err
	}

	if err := c.subscribe(ctx, id, "$"); err != nil {
		slog.Warn("Failed to subscribe to resumed session", "sessionID", id, "error", err)
	}

	msg := &pb.ServerMessage{
		Message: &pb.ServerMessage_SessionUpdated{
			SessionUpdated: &pb.SessionUpdatedResponse{Session: sess},
		},
	}

	if err := c.send(msg); err != nil {
		return err
	}

	c.server.BroadcastToAllConnect(msg, c)
	return nil
}

func (c *ConnectConnection) handleSessionPause(ctx context.Context, id string) error {
	sess, err := c.manager.Pause(ctx, id)
	if err != nil {
		return err
	}

	c.unsubscribe(id)

	msg := &pb.ServerMessage{
		Message: &pb.ServerMessage_SessionUpdated{
			SessionUpdated: &pb.SessionUpdatedResponse{Session: sess},
		},
	}

	if err := c.send(msg); err != nil {
		return err
	}

	c.server.BroadcastToAllConnect(msg, c)
	return nil
}

func (c *ConnectConnection) handleSessionDelete(ctx context.Context, id string) error {
	c.unsubscribe(id)

	if err := c.manager.Delete(ctx, id); err != nil {
		return err
	}

	msg := &pb.ServerMessage{
		Message: &pb.ServerMessage_SessionDeleted{
			SessionDeleted: &pb.SessionDeletedResponse{SessionId: id},
		},
	}

	if err := c.send(msg); err != nil {
		return err
	}

	c.server.BroadcastToAllConnect(msg, c)
	return nil
}

func (c *ConnectConnection) handleSessionDeleteAll(ctx context.Context) error {
	deletedIDs, err := c.manager.DeleteAll(ctx)
	if err != nil {
		slog.Warn("Some sessions failed to delete", "error", err)
	}

	msg := &pb.ServerMessage{
		Message: &pb.ServerMessage_SessionsDeletedAll{
			SessionsDeletedAll: &pb.SessionsDeletedAllResponse{DeletedIds: deletedIDs},
		},
	}

	if err := c.send(msg); err != nil {
		return err
	}

	c.server.BroadcastToAllConnect(msg, c)
	return nil
}

func (c *ConnectConnection) handlePrompt(ctx context.Context, sessionID, text string) error {
	if sessionID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errSessionIDRequired)
	}
	if text == "" {
		return connect.NewError(connect.CodeInvalidArgument, errTextRequired)
	}

	return c.manager.SendPrompt(ctx, sessionID, text)
}

func (c *ConnectConnection) handlePromptInterrupt(ctx context.Context, sessionID string) error {
	return c.manager.Interrupt(ctx, sessionID)
}

func (c *ConnectConnection) handleTerminalInput(ctx context.Context, sessionID, data string) error {
	if sessionID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errSessionIDRequired)
	}
	if data == "" {
		return nil // Empty input is a no-op
	}

	return c.manager.SendTerminalInput(ctx, sessionID, data)
}

var errInvalidTerminalDimensions = errors.New("cols and rows must be positive integers")

func (c *ConnectConnection) handleTerminalResize(ctx context.Context, req *pb.TerminalResizeRequest) error {
	if req.SessionId == "" {
		return connect.NewError(connect.CodeInvalidArgument, errSessionIDRequired)
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		return connect.NewError(connect.CodeInvalidArgument, errInvalidTerminalDimensions)
	}

	return c.manager.ResizeTerminal(ctx, req.SessionId, int(req.Cols), int(req.Rows))
}

func (c *ConnectConnection) handleSync(ctx context.Context) error {
	sessions, err := c.manager.GetAllWithMeta(ctx)
	if err != nil {
		return err
	}

	// Sessions are already pb.SessionSummary, just need to convert to pointers
	pbSessions := make([]*pb.SessionSummary, len(sessions))
	for i := range sessions {
		pbSessions[i] = &sessions[i]
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_SyncResponse{
			SyncResponse: &pb.SyncResponse{
				Sessions:   pbSessions,
				ServerTime: timestamppb.Now(),
			},
		},
	})
}

func (c *ConnectConnection) handlePortExpose(ctx context.Context, sessionID string, port int) error {
	if sessionID == "" {
		return c.send(makeErrorResponse(sessionID, "PORT_ERROR", "sessionId is required"))
	}
	if port < 1 || port > 65535 {
		return c.send(makeErrorResponse(sessionID, "PORT_ERROR", "port must be between 1 and 65535"))
	}

	previewURL, err := c.manager.ExposePort(ctx, sessionID, port)
	if err != nil {
		return c.send(makeErrorResponse(sessionID, "PORT_ERROR", err.Error()))
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_PortExposed{
			PortExposed: &pb.PortExposedResponse{
				SessionId:  sessionID,
				Port:       int32(port),
				PreviewUrl: previewURL,
			},
		},
	})
}

func (c *ConnectConnection) handleGitHubReposList(ctx context.Context) error {
	repos, err := c.manager.ListGitHubRepos(ctx)
	if err != nil {
		return c.send(makeErrorResponse("", "GITHUB_ERROR", "Failed to list GitHub repositories: "+err.Error()))
	}

	pbRepos := make([]*pb.GitHubRepo, len(repos))
	for i, r := range repos {
		pbRepos[i] = &pb.GitHubRepo{
			Name:     r.Name,
			FullName: r.FullName,
			Private:  r.Private,
		}
		if r.Description != "" {
			pbRepos[i].Description = &r.Description
		}
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_GithubRepos{
			GithubRepos: &pb.GitHubReposResponse{Repos: pbRepos},
		},
	})
}

func (c *ConnectConnection) handleGitStatus(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return c.send(makeErrorResponse(sessionID, "GIT_ERROR", "sessionId is required"))
	}

	files, err := c.manager.GetGitStatus(ctx, sessionID)
	if err != nil {
		return c.send(makeErrorResponse(sessionID, "GIT_ERROR", err.Error()))
	}

	// Files are already pb.GitFileChange, convert to pointers
	pbFiles := make([]*pb.GitFileChange, len(files))
	for i := range files {
		pbFiles[i] = &files[i]
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_GitStatus{
			GitStatus: &pb.GitStatusResponse{SessionId: sessionID, Files: pbFiles},
		},
	})
}

func (c *ConnectConnection) handleGitDiff(ctx context.Context, req *pb.GitDiffRequest) error {
	if req.SessionId == "" {
		return c.send(makeErrorResponse(req.SessionId, "GIT_ERROR", "sessionId is required"))
	}

	file := ""
	if req.File != nil {
		file = *req.File
	}

	diff, err := c.manager.GetGitDiff(ctx, req.SessionId, file)
	if err != nil {
		return c.send(makeErrorResponse(req.SessionId, "GIT_ERROR", err.Error()))
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_GitDiff{
			GitDiff: &pb.GitDiffResponse{SessionId: req.SessionId, Diff: diff},
		},
	})
}

// All conversion helpers have been removed since we now use proto types (pb.*) directly.
// The Manager now returns *pb.Session, pb.Message, pb.Event, etc.
// The StreamSubscriber now returns *pb.ServerMessage directly.

// handleListModels returns available models for the specified SDK type.
func (c *ConnectConnection) handleListModels(ctx context.Context, req *pb.ListModelsRequest) error {
	slog.Info("ListModels request", "sdkType", req.SdkType, "copilotBackend", req.CopilotBackend)
	models := c.manager.ListModels(req.SdkType, req.CopilotBackend)
	slog.Info("ListModels response", "sdkType", req.SdkType, "count", len(models))

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_Models{
			Models: &pb.ModelsResponse{
				Models:    models,
				RequestId: req.RequestId,
				SdkType:   &req.SdkType, // Echo back which SDK type these models are for
			},
		},
	})
}

func codexModelAuthMode(model string) string {
	parts := strings.Split(model, ":")
	if len(parts) < 2 {
		return ""
	}
	last := parts[len(parts)-1]
	if last == "minimal" || last == "low" || last == "medium" || last == "high" || last == "xhigh" {
		if len(parts) >= 3 {
			last = parts[len(parts)-2]
		} else {
			return ""
		}
	}
	if last == "api" || last == "oauth" {
		return last
	}
	return ""
}

// handleGetCopilotStatus returns GitHub Copilot authentication status and quota.
func (c *ConnectConnection) handleGetCopilotStatus(ctx context.Context) error {
	status := c.manager.GetCopilotStatus(ctx)

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_CopilotStatus{
			CopilotStatus: status,
		},
	})
}

// handleListSnapshots returns all snapshots for a session.
func (c *ConnectConnection) handleListSnapshots(ctx context.Context, req *pb.ListSnapshotsRequest) error {
	if req.SessionId == "" {
		return c.send(makeErrorResponse(req.SessionId, "SNAPSHOT_ERROR", "sessionId is required"))
	}

	snapshots, err := c.manager.ListSnapshots(ctx, req.SessionId)
	if err != nil {
		return c.send(makeErrorResponse(req.SessionId, "SNAPSHOT_ERROR", err.Error()))
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_SnapshotList{
			SnapshotList: &pb.SnapshotListResponse{
				SessionId: req.SessionId,
				Snapshots: snapshots,
				RequestId: req.RequestId,
			},
		},
	})
}

// handleRestoreSnapshot restores a session's workspace from a snapshot.
func (c *ConnectConnection) handleRestoreSnapshot(ctx context.Context, req *pb.RestoreSnapshotRequest) error {
	if req.SessionId == "" {
		return c.send(makeErrorResponse(req.SessionId, "SNAPSHOT_ERROR", "sessionId is required"))
	}
	if req.SnapshotId == "" {
		return c.send(makeErrorResponse(req.SessionId, "SNAPSHOT_ERROR", "snapshotId is required"))
	}

	messagesRestored, err := c.manager.RestoreSnapshot(ctx, req.SessionId, req.SnapshotId)
	if err != nil {
		return c.send(makeErrorResponse(req.SessionId, "SNAPSHOT_ERROR", err.Error()))
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_SnapshotRestored{
			SnapshotRestored: &pb.SnapshotRestoredResponse{
				SessionId:        req.SessionId,
				SnapshotId:       req.SnapshotId,
				MessagesRestored: messagesRestored,
				RequestId:        req.RequestId,
			},
		},
	})
}

// handleUpdateRepoAccess changes the repository access level for a session.
func (c *ConnectConnection) handleUpdateRepoAccess(ctx context.Context, req *pb.UpdateRepoAccessRequest) error {
	if req.SessionId == "" {
		return c.send(makeErrorResponse(req.SessionId, "REPO_ACCESS_ERROR", "sessionId is required"))
	}

	if err := c.manager.UpdateRepoAccess(ctx, req.SessionId, req.RepoAccess); err != nil {
		return c.send(makeErrorResponse(req.SessionId, "REPO_ACCESS_ERROR", err.Error()))
	}

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_RepoAccessUpdated{
			RepoAccessUpdated: &pb.RepoAccessUpdatedResponse{
				SessionId:  req.SessionId,
				RepoAccess: req.RepoAccess,
				RequestId:  req.RequestId,
			},
		},
	})
}

// handleGetResourceLimits returns the maximum allowed sandbox resources per session.
func (c *ConnectConnection) handleGetResourceLimits(ctx context.Context, req *pb.GetResourceLimitsRequest) error {
	limits := c.manager.GetResourceLimits()
	limits.RequestId = req.RequestId

	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_ResourceLimits{
			ResourceLimits: limits,
		},
	})
}

func (c *ConnectConnection) handleCodexAuthStart(ctx context.Context, req *pb.CodexAuthStartRequest) error {
	resp, err := c.manager.StartCodexAuth(ctx)
	if err != nil {
		return c.send(makeErrorResponse("", "CODEX_AUTH_ERROR", err.Error()))
	}
	resp.RequestId = req.RequestId
	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_CodexAuthStarted{
			CodexAuthStarted: resp,
		},
	})
}

func (c *ConnectConnection) handleCodexAuthStatus(ctx context.Context, req *pb.CodexAuthStatusRequest) error {
	resp, err := c.manager.GetCodexAuthStatus(ctx)
	if err != nil {
		return c.send(makeErrorResponse("", "CODEX_AUTH_ERROR", err.Error()))
	}
	resp.RequestId = req.RequestId
	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_CodexAuthStatus{
			CodexAuthStatus: resp,
		},
	})
}

func (c *ConnectConnection) handleCodexAuthLogout(ctx context.Context, req *pb.CodexAuthLogoutRequest) error {
	if err := c.manager.LogoutCodexAuth(ctx); err != nil {
		return c.send(makeErrorResponse("", "CODEX_AUTH_ERROR", err.Error()))
	}
	return c.send(&pb.ServerMessage{
		Message: &pb.ServerMessage_CodexAuthLoggedOut{
			CodexAuthLoggedOut: &pb.CodexAuthLoggedOutResponse{
				RequestId: req.RequestId,
			},
		},
	})
}

// storageEntryToProto converts a storage.StreamEntryWithID to a pb.StreamEntry.
// The new unified model uses oneof payload with: AgentEvent, TerminalOutput, Session, Error
func storageEntryToProto(e storage.StreamEntryWithID) *pb.StreamEntry {
	entry := &pb.StreamEntry{
		Id:      e.ID,
		Partial: e.Entry.Partial,
	}

	// Parse timestamp
	if t, err := time.Parse(time.RFC3339, e.Entry.Timestamp); err == nil {
		entry.Timestamp = timestamppb.New(t)
	}

	// Set payload based on type
	switch e.Entry.Type {
	case storage.StreamEntryTypeEvent:
		// AgentEvent includes all content: messages, thinking, tools, etc.
		var event pb.AgentEvent
		if err := protojson.Unmarshal(e.Entry.Payload, &event); err == nil {
			entry.Payload = &pb.StreamEntry_Event{Event: &event}
		}
	case storage.StreamEntryTypeTerminalOutput:
		var output pb.TerminalOutput
		if err := protojson.Unmarshal(e.Entry.Payload, &output); err == nil {
			entry.Payload = &pb.StreamEntry_TerminalOutput{TerminalOutput: &output}
		}
	case storage.StreamEntryTypeSessionUpdate:
		var sess pb.Session
		if err := protojson.Unmarshal(e.Entry.Payload, &sess); err == nil {
			entry.Payload = &pb.StreamEntry_SessionUpdate{SessionUpdate: &sess}
		}
	case storage.StreamEntryTypeError:
		var errProto pb.Error
		if err := protojson.Unmarshal(e.Entry.Payload, &errProto); err == nil {
			entry.Payload = &pb.StreamEntry_Error{Error: &errProto}
		}
	}

	return entry
}
