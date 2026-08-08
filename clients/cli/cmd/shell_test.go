package cmd

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// shellTestHandler is a mock Connect server for shell tests.
// It queues responses that are sent back for each received message.
type shellTestHandler struct {
	netclodev1connect.UnimplementedClientServiceHandler

	mu        sync.Mutex
	responses []*pb.ServerMessage
}

func (h *shellTestHandler) queueResponse(msgs ...*pb.ServerMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.responses = append(h.responses, msgs...)
}

func (h *shellTestHandler) Connect(ctx context.Context, stream *connect.BidiStream[pb.ClientMessage, pb.ServerMessage]) error {
	// Wait for first message (CreateSession, OpenSession, etc.)
	_, err := stream.Receive()
	if err != nil {
		return err
	}

	// Send all queued responses
	h.mu.Lock()
	responses := make([]*pb.ServerMessage, len(h.responses))
	copy(responses, h.responses)
	h.mu.Unlock()

	for _, resp := range responses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}

	// Keep the stream open until context is cancelled (needed for receive loops)
	<-ctx.Done()
	return nil
}

func setupShellTestServer(t *testing.T, handler *shellTestHandler) (string, func()) {
	t.Helper()
	mux := http.NewServeMux()
	path, h := netclodev1connect.NewClientServiceHandler(handler)
	mux.Handle(path, h)

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

// triggerStream creates a Connect stream and sends a dummy message to trigger the handler.
func triggerStream(ctx context.Context, url string) *connect.BidiStreamForClient[pb.ClientMessage, pb.ServerMessage] {
	stream := newH2CClient(url).Connect(ctx)
	_ = stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_CreateSession{CreateSession: &pb.CreateSessionRequest{}},
	})
	return stream
}

func newH2CClient(url string) netclodev1connect.ClientServiceClient {
	httpClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}
	return netclodev1connect.NewClientServiceClient(httpClient, url)
}

func TestReceiveSessionState(t *testing.T) {
	t.Run("returns session state directly", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(&pb.ServerMessage{
			Message: &pb.ServerMessage_SessionState{
				SessionState: &pb.SessionStateResponse{
					Session: &pb.Session{
						Id:     "sess-1",
						Status: pb.SessionStatus_SESSION_STATUS_READY,
					},
				},
			},
		})

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := newH2CClient(url).Connect(ctx)
		// Send OpenSession to trigger the handler
		_ = stream.Send(&pb.ClientMessage{
			Message: &pb.ClientMessage_OpenSession{
				OpenSession: &pb.OpenSessionRequest{SessionId: "sess-1"},
			},
		})

		state, err := receiveSessionState(stream)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Session.Id != "sess-1" {
			t.Errorf("expected session ID 'sess-1', got '%s'", state.Session.Id)
		}
	})

	t.Run("skips stream entries before session state", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(
			&pb.ServerMessage{
				Message: &pb.ServerMessage_StreamEntry{
					StreamEntry: &pb.StreamEntryResponse{
						Entry: &pb.StreamEntry{
							Id:        "1-0",
							Timestamp: timestamppb.Now(),
							Payload: &pb.StreamEntry_TerminalOutput{
								TerminalOutput: &pb.TerminalOutput{Data: "old output"},
							},
						},
					},
				},
			},
			&pb.ServerMessage{
				Message: &pb.ServerMessage_SessionState{
					SessionState: &pb.SessionStateResponse{
						Session: &pb.Session{
							Id:     "sess-2",
							Status: pb.SessionStatus_SESSION_STATUS_READY,
						},
					},
				},
			},
		)

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := newH2CClient(url).Connect(ctx)
		_ = stream.Send(&pb.ClientMessage{
			Message: &pb.ClientMessage_OpenSession{
				OpenSession: &pb.OpenSessionRequest{SessionId: "sess-2"},
			},
		})

		state, err := receiveSessionState(stream)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Session.Id != "sess-2" {
			t.Errorf("expected session ID 'sess-2', got '%s'", state.Session.Id)
		}
	})

	t.Run("returns error from server", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(&pb.ServerMessage{
			Message: &pb.ServerMessage_Error{
				Error: &pb.ErrorResponse{
					Error: &pb.Error{Code: "NOT_FOUND", Message: "session not found"},
				},
			},
		})

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := newH2CClient(url).Connect(ctx)
		_ = stream.Send(&pb.ClientMessage{
			Message: &pb.ClientMessage_OpenSession{
				OpenSession: &pb.OpenSessionRequest{SessionId: "nope"},
			},
		})

		_, err := receiveSessionState(stream)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "NOT_FOUND") {
			t.Errorf("expected 'NOT_FOUND' error, got '%s'", err.Error())
		}
	})
}

func TestWaitForReady(t *testing.T) {
	t.Run("returns on immediate ready", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(&pb.ServerMessage{
			Message: &pb.ServerMessage_SessionCreated{
				SessionCreated: &pb.SessionCreatedResponse{
					Session: &pb.Session{
						Id:     "sess-fast",
						Status: pb.SessionStatus_SESSION_STATUS_READY,
					},
				},
			},
		})

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := triggerStream(ctx, url)

		id, err := waitForReady(ctx, stream)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "sess-fast" {
			t.Errorf("expected 'sess-fast', got '%s'", id)
		}
	})

	t.Run("waits for ready via StreamEntry", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(
			&pb.ServerMessage{
				Message: &pb.ServerMessage_SessionCreated{
					SessionCreated: &pb.SessionCreatedResponse{
						Session: &pb.Session{
							Id:     "sess-warm",
							Status: pb.SessionStatus_SESSION_STATUS_CREATING,
						},
					},
				},
			},
			&pb.ServerMessage{
				Message: &pb.ServerMessage_StreamEntry{
					StreamEntry: &pb.StreamEntryResponse{
						Entry: &pb.StreamEntry{
							Id:        "1-0",
							Timestamp: timestamppb.Now(),
							Payload: &pb.StreamEntry_SessionUpdate{
								SessionUpdate: &pb.Session{
									Id:     "sess-warm",
									Status: pb.SessionStatus_SESSION_STATUS_READY,
								},
							},
						},
					},
				},
			},
		)

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := triggerStream(ctx, url)

		id, err := waitForReady(ctx, stream)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "sess-warm" {
			t.Errorf("expected 'sess-warm', got '%s'", id)
		}
	})

	t.Run("ignores SessionUpdated for other sessions", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(
			&pb.ServerMessage{
				Message: &pb.ServerMessage_SessionCreated{
					SessionCreated: &pb.SessionCreatedResponse{
						Session: &pb.Session{
							Id:     "sess-ours",
							Status: pb.SessionStatus_SESSION_STATUS_CREATING,
						},
					},
				},
			},
			// Auto-pause broadcast for a different session
			&pb.ServerMessage{
				Message: &pb.ServerMessage_SessionUpdated{
					SessionUpdated: &pb.SessionUpdatedResponse{
						Session: &pb.Session{
							Id:     "sess-other",
							Status: pb.SessionStatus_SESSION_STATUS_PAUSED,
						},
					},
				},
			},
			// Our session becomes ready
			&pb.ServerMessage{
				Message: &pb.ServerMessage_StreamEntry{
					StreamEntry: &pb.StreamEntryResponse{
						Entry: &pb.StreamEntry{
							Id:        "2-0",
							Timestamp: timestamppb.Now(),
							Payload: &pb.StreamEntry_SessionUpdate{
								SessionUpdate: &pb.Session{
									Id:     "sess-ours",
									Status: pb.SessionStatus_SESSION_STATUS_READY,
								},
							},
						},
					},
				},
			},
		)

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := triggerStream(ctx, url)

		id, err := waitForReady(ctx, stream)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "sess-ours" {
			t.Errorf("expected 'sess-ours', got '%s'", id)
		}
	})

	t.Run("returns error on session failure", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(
			&pb.ServerMessage{
				Message: &pb.ServerMessage_SessionCreated{
					SessionCreated: &pb.SessionCreatedResponse{
						Session: &pb.Session{
							Id:     "sess-fail",
							Status: pb.SessionStatus_SESSION_STATUS_CREATING,
						},
					},
				},
			},
			&pb.ServerMessage{
				Message: &pb.ServerMessage_StreamEntry{
					StreamEntry: &pb.StreamEntryResponse{
						Entry: &pb.StreamEntry{
							Id:        "1-0",
							Timestamp: timestamppb.Now(),
							Payload: &pb.StreamEntry_SessionUpdate{
								SessionUpdate: &pb.Session{
									Id:     "sess-fail",
									Status: pb.SessionStatus_SESSION_STATUS_ERROR,
								},
							},
						},
					},
				},
			},
		)

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := triggerStream(ctx, url)

		_, err := waitForReady(ctx, stream)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "failed to start") {
			t.Errorf("expected 'failed to start', got '%s'", err.Error())
		}
	})

	t.Run("returns server error", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(&pb.ServerMessage{
			Message: &pb.ServerMessage_Error{
				Error: &pb.ErrorResponse{
					Error: &pb.Error{Code: "QUOTA_EXCEEDED", Message: "too many sessions"},
				},
			},
		})

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx := t.Context()

		stream := triggerStream(ctx, url)

		_, err := waitForReady(ctx, stream)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "QUOTA_EXCEEDED") {
			t.Errorf("expected 'QUOTA_EXCEEDED', got '%s'", err.Error())
		}
	})
}

func TestShellReceiveLoop(t *testing.T) {
	t.Run("detects PTY exit marker and cancels", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(&pb.ServerMessage{
			Message: &pb.ServerMessage_StreamEntry{
				StreamEntry: &pb.StreamEntryResponse{
					Entry: &pb.StreamEntry{
						Id:        "1-0",
						Timestamp: timestamppb.Now(),
						Payload: &pb.StreamEntry_TerminalOutput{
							TerminalOutput: &pb.TerminalOutput{
								Data: "\r\n\x1b]9999;pty-exit;0\x07",
							},
						},
					},
				},
			},
		})

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stream := newH2CClient(url).Connect(ctx)
		// Send a dummy message to trigger the handler
		_ = stream.Send(&pb.ClientMessage{
			Message: &pb.ClientMessage_OpenSession{
				OpenSession: &pb.OpenSessionRequest{SessionId: "sess-1"},
			},
		})

		err := shellReceiveLoop(ctx, cancel, stream)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ctx.Err() == nil {
			t.Error("expected context to be cancelled after PTY exit")
		}
	})

	t.Run("returns server error", func(t *testing.T) {
		handler := &shellTestHandler{}
		handler.queueResponse(&pb.ServerMessage{
			Message: &pb.ServerMessage_Error{
				Error: &pb.ErrorResponse{
					Error: &pb.Error{Code: "AGENT_ERROR", Message: "agent disconnected"},
				},
			},
		})

		url, cleanup := setupShellTestServer(t, handler)
		defer cleanup()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stream := newH2CClient(url).Connect(ctx)
		_ = stream.Send(&pb.ClientMessage{
			Message: &pb.ClientMessage_OpenSession{
				OpenSession: &pb.OpenSessionRequest{SessionId: "sess-1"},
			},
		})

		err := shellReceiveLoop(ctx, cancel, stream)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "AGENT_ERROR") {
			t.Errorf("expected 'AGENT_ERROR', got '%s'", err.Error())
		}
	})
}

func TestPtyExitMarker(t *testing.T) {
	t.Run("matches agent output format", func(t *testing.T) {
		for _, code := range []string{"0", "1", "130", "255"} {
			output := "\r\n\x1b]9999;pty-exit;" + code + "\x07"
			if !strings.Contains(output, ptyExitMarker) {
				t.Errorf("ptyExitMarker not found for exit code %s", code)
			}
		}
	})
}
