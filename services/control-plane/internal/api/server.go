package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	httptrace "github.com/DataDog/dd-trace-go/contrib/net/http/v2"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/gen/netclode/v1/netclodev1connect"
	"github.com/angristan/netclode/services/control-plane/internal/session"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const (
	// ShutdownTimeout is the maximum time to wait for connections to drain
	ShutdownTimeout = 30 * time.Second
)

// Server is the HTTP server with Connect protocol and graceful shutdown support.
type Server struct {
	manager     *session.Manager
	httpServers []*http.Server

	// Connect connection tracking
	connectConnections sync.Map // map[*ConnectConnection]struct{}

	connCount  atomic.Int64
	shutdownCh chan struct{}
	wg         sync.WaitGroup
}

// ListenAddresses defines the isolated control-plane listener addresses.
type ListenAddresses struct {
	Client   string
	Agent    string
	Internal string
	Bot      string
}

type workloadTokenVerifier interface {
	VerifyWorkloadToken(ctx context.Context, token, audience, serviceAccount string) (string, error)
}

// NewServer creates a new server.
func NewServer(manager *session.Manager) *Server {
	s := &Server{
		manager:    manager,
		shutdownCh: make(chan struct{}),
	}

	// Set up callback for auto-pause broadcasts
	manager.SetOnSessionUpdated(func(session *pb.Session) {
		// Session is already *pb.Session, create pb.ServerMessage directly
		pbMsg := &pb.ServerMessage{
			Message: &pb.ServerMessage_SessionUpdated{
				SessionUpdated: &pb.SessionUpdatedResponse{Session: session},
			},
		}
		s.BroadcastToAllConnect(pbMsg, nil)
	})

	return s
}

// BroadcastToAllConnect sends a message to all connected Connect clients except the sender.
func (s *Server) BroadcastToAllConnect(msg *pb.ServerMessage, exclude *ConnectConnection) {
	s.connectConnections.Range(func(key, value any) bool {
		if conn, ok := key.(*ConnectConnection); ok && conn != exclude {
			// Workload clients receive only direct responses and owned session streams.
			if conn.role != ClientRoleAdmin {
				return true
			}
			// Non-blocking send to avoid blocking broadcast
			select {
			case conn.globalMessages <- msg:
			default:
				slog.Debug("Skipping global message for slow Connect client")
			}
		}
		return true
	})
}

// ListenAndServe starts isolated HTTP/Connect listeners for each trust domain.
func (s *Server) ListenAndServe(ctx context.Context, addresses ListenAddresses) error {
	clientMux := http.NewServeMux()
	clientMux.HandleFunc("GET /health", s.handleHealth)
	clientHandler := NewConnectClientServiceHandler(s.manager, s, ClientRoleAdmin)
	clientPath, clientHandlerFunc := netclodev1connect.NewClientServiceHandler(clientHandler)
	clientMux.Handle(clientPath, clientHandlerFunc)

	agentMux := http.NewServeMux()
	agentMux.HandleFunc("GET /health", s.handleHealth)
	agentHandler := NewConnectAgentServiceHandler(s.manager, s)
	agentPath, agentHandlerFunc := netclodev1connect.NewAgentServiceHandler(agentHandler)
	agentMux.Handle(agentPath, agentHandlerFunc)

	internalMux := http.NewServeMux()
	internalMux.HandleFunc("GET /health", s.handleHealth)
	internalMux.Handle("POST /internal/validate-proxy-auth", requireWorkloadAuth(
		s.manager, "netclode-internal", "secret-proxy", http.HandlerFunc(s.handleValidateProxyAuth),
	))

	botMux := http.NewServeMux()
	botMux.HandleFunc("GET /health", s.handleHealth)
	botHandler := NewConnectClientServiceHandler(s.manager, s, ClientRoleBot)
	botPath, botHandlerFunc := netclodev1connect.NewClientServiceHandler(botHandler)
	botMux.Handle(botPath, requireWorkloadAuth(s.manager, "netclode-client", "github-bot", botHandlerFunc))

	s.httpServers = []*http.Server{
		newHTTPServer(addresses.Client, clientMux),
		newHTTPServer(addresses.Agent, agentMux),
		newHTTPServer(addresses.Internal, internalMux),
		newHTTPServer(addresses.Bot, botMux),
	}

	errCh := make(chan error, len(s.httpServers))
	for _, httpServer := range s.httpServers {
		httpServer := httpServer
		slog.Info("Starting isolated h2c server", "addr", httpServer.Addr)
		go func() {
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("h2c server %s: %w", httpServer.Addr, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		return s.gracefulShutdown()
	case err := <-errCh:
		_ = s.gracefulShutdown()
		return err
	}
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	tracedHandler := httptrace.WrapHandler(handler, "control-plane", "http.request")
	return &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(tracedHandler, &http2.Server{}),
	}
}

func requireWorkloadAuth(verifier workloadTokenVerifier, audience, serviceAccount string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		if _, err := verifier.VerifyWorkloadToken(r.Context(), token, audience, serviceAccount); err != nil {
			slog.WarnContext(r.Context(), "Workload authentication failed", "serviceAccount", serviceAccount, "error", err)
			http.Error(w, "invalid workload identity", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(value string) (string, bool) {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.Contains(token, " ") {
		return "", false
	}
	return token, true
}

// gracefulShutdown performs graceful shutdown with connection draining.
func (s *Server) gracefulShutdown() error {
	slog.Info("Starting graceful shutdown", "activeConnections", s.connCount.Load())

	// Signal all connections to start closing
	select {
	case <-s.shutdownCh:
		// Already closed
	default:
		close(s.shutdownCh)
	}

	// Create a context with timeout for the entire shutdown process
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	// Wait for all connections to close (with timeout)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("All connections closed gracefully")
	case <-ctx.Done():
		slog.Warn("Timeout waiting for connections, forcing close",
			"remainingConnections", s.connCount.Load())
		// Force close remaining Connect connections
		s.connectConnections.Range(func(key, value any) bool {
			if conn, ok := key.(*ConnectConnection); ok {
				conn.close()
			}
			return true
		})
	}

	var firstErr error
	for _, httpServer := range s.httpServers {
		if err := httpServer.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// validateProxyAuthRequest is the request body for proxy auth validation.
type validateProxyAuthRequest struct {
	Token      string `json:"token"`
	TargetHost string `json:"target_host"`
}

// validateProxyAuthResponse is the response for proxy auth validation.
type validateProxyAuthResponse struct {
	Allowed     bool   `json:"allowed"`
	SecretKey   string `json:"secret_key,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

// handleValidateProxyAuth validates proxy authentication requests from the secret-proxy.
// POST /internal/validate-proxy-auth
// Body: {"token": "<k8s-sa-token>", "target_host": "api.anthropic.com"}
// Returns: {"allowed": true, "secret_key": "anthropic", "placeholder": "NETCLODE_PLACEHOLDER_anthropic"}
func (s *Server) handleValidateProxyAuth(w http.ResponseWriter, r *http.Request) {
	var req validateProxyAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(validateProxyAuthResponse{Error: "invalid request body"})
		return
	}

	if req.Token == "" || req.TargetHost == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(validateProxyAuthResponse{Error: "token and target_host required"})
		return
	}

	result, err := s.manager.ValidateProxyAuth(r.Context(), req.Token, req.TargetHost)
	if err != nil {
		slog.Warn("Proxy auth validation failed",
			"targetHost", req.TargetHost,
			"error", err,
			"tokenLen", len(req.Token),
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(validateProxyAuthResponse{Allowed: false, Error: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(validateProxyAuthResponse{
		Allowed:     result.Allowed,
		SecretKey:   result.SecretKey,
		Placeholder: result.Placeholder,
		SessionID:   result.SessionID,
	})
}

// Shutdown initiates graceful shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.gracefulShutdown()
}

// ActiveConnections returns the number of active Connect connections.
func (s *Server) ActiveConnections() int64 {
	return s.connCount.Load()
}
