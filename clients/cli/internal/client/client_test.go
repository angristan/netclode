package client

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mockClientServiceHandler implements the ClientServiceHandler for testing.
type mockClientServiceHandler struct {
	netclodev1connect.UnimplementedClientServiceHandler

	sessions     []*pb.Session
	summaries    []*pb.SessionSummary
	sessionState *pb.SessionStateResponse
	err          error
}

func (m *mockClientServiceHandler) Connect(ctx context.Context, stream *connect.BidiStream[pb.ClientMessage, pb.ServerMessage]) error {
	for {
		msg, err := stream.Receive()
		if err != nil {
			return err
		}

		var resp *pb.ServerMessage

		switch msg.Message.(type) {
		case *pb.ClientMessage_ListSessions:
			if m.err != nil {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_Error{
						Error: &pb.ErrorResponse{
							Error: &pb.Error{
								Code:    "TEST_ERROR",
								Message: m.err.Error(),
							},
						},
					},
				}
			} else {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_SessionList{
						SessionList: &pb.SessionListResponse{
							Sessions: m.sessions,
						},
					},
				}
			}

		case *pb.ClientMessage_Sync:
			if m.err != nil {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_Error{
						Error: &pb.ErrorResponse{
							Error: &pb.Error{
								Code:    "TEST_ERROR",
								Message: m.err.Error(),
							},
						},
					},
				}
			} else {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_SyncResponse{
						SyncResponse: &pb.SyncResponse{
							Sessions: m.summaries,
						},
					},
				}
			}

		case *pb.ClientMessage_OpenSession:
			if m.err != nil {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_Error{
						Error: &pb.ErrorResponse{
							Error: &pb.Error{
								Code:    "TEST_ERROR",
								Message: m.err.Error(),
							},
						},
					},
				}
			} else {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_SessionState{
						SessionState: m.sessionState,
					},
				}
			}

		case *pb.ClientMessage_DeleteSession:
			if m.err != nil {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_Error{
						Error: &pb.ErrorResponse{
							Error: &pb.Error{
								Code:    "TEST_ERROR",
								Message: m.err.Error(),
							},
						},
					},
				}
			} else {
				resp = &pb.ServerMessage{
					Message: &pb.ServerMessage_SessionDeleted{
						SessionDeleted: &pb.SessionDeletedResponse{
							SessionId: "test-session",
						},
					},
				}
			}
		}

		if resp != nil {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

// testClient creates a client with h2c support for testing.
type testClient struct {
	*Client
}

func newTestClient(url string) *testClient {
	// Create HTTP client with HTTP/2 cleartext (h2c) support
	httpClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}

	return &testClient{
		Client: &Client{
			baseURL: url,
			client:  netclodev1connect.NewClientServiceClient(httpClient, url),
		},
	}
}

// setupTestServer creates an h2c-enabled test server for Connect protocol testing.
func setupTestServer(t *testing.T, handler *mockClientServiceHandler) (string, func()) {
	mux := http.NewServeMux()
	path, h := netclodev1connect.NewClientServiceHandler(handler)
	mux.Handle(path, h)

	// Support HTTP/2 without TLS (required for bidirectional streams)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}

	server := &http.Server{Handler: mux, Protocols: protocols}
	go func() { _ = server.Serve(listener) }()

	url := "http://" + listener.Addr().String()
	cleanup := func() {
		_ = server.Close()
		_ = listener.Close()
	}

	return url, cleanup
}

func TestListSessions(t *testing.T) {
	handler := &mockClientServiceHandler{
		sessions: []*pb.Session{
			{
				Id:     "sess-1",
				Name:   "Test Session 1",
				Status: pb.SessionStatus_SESSION_STATUS_READY,
			},
			{
				Id:     "sess-2",
				Name:   "Test Session 2",
				Status: pb.SessionStatus_SESSION_STATUS_PAUSED,
			},
		},
	}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	sessions, err := client.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}

	if sessions[0].Id != "sess-1" {
		t.Errorf("expected session ID 'sess-1', got '%s'", sessions[0].Id)
	}

	if sessions[1].Status != pb.SessionStatus_SESSION_STATUS_PAUSED {
		t.Errorf("expected session 2 status PAUSED, got %v", sessions[1].Status)
	}
}

func TestSyncSessions(t *testing.T) {
	msgCount := int32(5)
	handler := &mockClientServiceHandler{
		summaries: []*pb.SessionSummary{
			{
				Session: &pb.Session{
					Id:     "sess-1",
					Name:   "Test Session",
					Status: pb.SessionStatus_SESSION_STATUS_RUNNING,
				},
				MessageCount: &msgCount,
			},
		},
	}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	summaries, err := client.SyncSessions(context.Background())
	if err != nil {
		t.Fatalf("SyncSessions failed: %v", err)
	}

	if len(summaries) != 1 {
		t.Errorf("expected 1 summary, got %d", len(summaries))
	}

	if *summaries[0].MessageCount != 5 {
		t.Errorf("expected message count 5, got %d", *summaries[0].MessageCount)
	}
}

func TestGetSession(t *testing.T) {
	lastStreamID := "stream-123"
	handler := &mockClientServiceHandler{
		sessionState: &pb.SessionStateResponse{
			Session: &pb.Session{
				Id:     "sess-1",
				Name:   "Test Session",
				Status: pb.SessionStatus_SESSION_STATUS_READY,
			},
			Entries: []*pb.StreamEntry{
				{
					Id:        "1-0",
					Timestamp: timestamppb.Now(),
					Payload: &pb.StreamEntry_Event{
						Event: &pb.AgentEvent{
							Kind: pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_START,
						},
					},
				},
			},
			HasMore:      true,
			LastStreamId: &lastStreamID,
		},
	}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	state, err := client.GetSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if state.Session.Id != "sess-1" {
		t.Errorf("expected session ID 'sess-1', got '%s'", state.Session.Id)
	}

	if len(state.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(state.Entries))
	}

	if !state.HasMore {
		t.Error("expected HasMore to be true")
	}

	if state.LastStreamID != "stream-123" {
		t.Errorf("expected LastStreamID 'stream-123', got '%s'", state.LastStreamID)
	}
}

func TestDeleteSession(t *testing.T) {
	handler := &mockClientServiceHandler{}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	err := client.DeleteSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
}

func TestClientErrorHandling(t *testing.T) {
	handler := &mockClientServiceHandler{
		err: &testError{msg: "session not found"},
	}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	_, err := client.ListSessions(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !contains(err.Error(), "TEST_ERROR") {
		t.Errorf("expected error to contain 'TEST_ERROR', got '%s'", err.Error())
	}
}

func TestNewClient(t *testing.T) {
	client := New("http://localhost:3000")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.baseURL != "http://localhost:3000" {
		t.Errorf("expected baseURL 'http://localhost:3000', got '%s'", client.baseURL)
	}
}

func TestListSessionsEmpty(t *testing.T) {
	handler := &mockClientServiceHandler{
		sessions: []*pb.Session{},
	}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	sessions, err := client.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}

	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestGetSessionWithMinimalData(t *testing.T) {
	handler := &mockClientServiceHandler{
		sessionState: &pb.SessionStateResponse{
			Session: &pb.Session{
				Id:     "sess-minimal",
				Name:   "Minimal",
				Status: pb.SessionStatus_SESSION_STATUS_CREATING,
			},
			Entries: nil,
			HasMore: false,
		},
	}

	url, cleanup := setupTestServer(t, handler)
	defer cleanup()

	client := newTestClient(url)
	state, err := client.GetSession(context.Background(), "sess-minimal")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if state.Session.Status != pb.SessionStatus_SESSION_STATUS_CREATING {
		t.Errorf("expected CREATING status, got %v", state.Session.Status)
	}

	if len(state.Entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(state.Entries))
	}

	if state.HasMore {
		t.Error("expected HasMore to be false")
	}
}

// Helper types and functions

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Benchmark tests

func BenchmarkListSessions(b *testing.B) {
	sessions := make([]*pb.Session, 100)
	for i := range 100 {
		sessions[i] = &pb.Session{
			Id:           "sess-" + string(rune('0'+i%10)),
			Name:         "Test Session",
			Status:       pb.SessionStatus_SESSION_STATUS_READY,
			CreatedAt:    timestamppb.Now(),
			LastActiveAt: timestamppb.Now(),
		}
	}

	handler := &mockClientServiceHandler{sessions: sessions}

	mux := http.NewServeMux()
	path, h := netclodev1connect.NewClientServiceHandler(handler)
	mux.Handle(path, h)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	server := &http.Server{Handler: mux, Protocols: protocols}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	defer func() { _ = listener.Close() }()

	client := newTestClient("http://" + listener.Addr().String())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.ListSessions(context.Background())
	}
}
