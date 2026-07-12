package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/elazarl/goproxy"
)

func TestValidateProxyAuthResponse(t *testing.T) {
	// Test that the response structure matches what we expect
	response := validateProxyAuthResponse{
		Allowed:     true,
		SecretKey:   "anthropic",
		Placeholder: "NETCLODE_PLACEHOLDER_anthropic",
		SessionID:   "test-session-123",
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	var decoded validateProxyAuthResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if decoded.Allowed != response.Allowed {
		t.Errorf("Allowed = %v, want %v", decoded.Allowed, response.Allowed)
	}
	if decoded.SecretKey != response.SecretKey {
		t.Errorf("SecretKey = %v, want %v", decoded.SecretKey, response.SecretKey)
	}
	if decoded.Placeholder != response.Placeholder {
		t.Errorf("Placeholder = %v, want %v", decoded.Placeholder, response.Placeholder)
	}
	if decoded.SessionID != response.SessionID {
		t.Errorf("SessionID = %v, want %v", decoded.SessionID, response.SessionID)
	}
}

func TestValidateWithControlPlane(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("proxy-workload-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Create a mock control-plane server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/validate-proxy-auth" {
			t.Errorf("Unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer proxy-workload-token" {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req validateProxyAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Mock response based on input
		resp := validateProxyAuthResponse{
			Allowed:     req.TargetHost == "api.anthropic.com",
			SecretKey:   "anthropic",
			Placeholder: "NETCLODE_PLACEHOLDER_anthropic",
			SessionID:   "test-session",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := &Proxy{
		config: Config{
			ControlPlaneURL:       server.URL,
			ControlPlaneTokenPath: tokenPath,
		},
		httpClient: http.DefaultClient,
	}

	// Test allowed host
	result, err := p.validateWithControlPlane("test-token", "api.anthropic.com")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Error("Expected allowed=true for api.anthropic.com")
	}

	// Test disallowed host
	result, err = p.validateWithControlPlane("test-token", "evil.com")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if result.Allowed {
		t.Error("Expected allowed=false for evil.com")
	}
}

func TestNoInjectionOverHTTP(t *testing.T) {
	// Mock control-plane that always allows api.anthropic.com.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req validateProxyAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := validateProxyAuthResponse{
			Allowed:     req.TargetHost == "api.anthropic.com",
			SecretKey:   "anthropic",
			Placeholder: "NETCLODE_PLACEHOLDER_anthropic",
			SessionID:   "test-session",
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := &Proxy{
		config: Config{
			ControlPlaneURL: server.URL,
			Secrets: map[string]string{
				"anthropic": "REAL_SECRET",
			},
		},
		httpClient: http.DefaultClient,
		logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}

	req := httptest.NewRequest(http.MethodGet, "http://api.anthropic.com/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer NETCLODE_PLACEHOLDER_anthropic")
	ctx := &goproxy.ProxyCtx{
		Req:      req,
		UserData: "Bearer test-token",
	}

	outReq, _ := p.handleRequest(req, ctx)
	if got := outReq.Header.Get("Authorization"); got != "Bearer NETCLODE_PLACEHOLDER_anthropic" {
		t.Fatalf("unexpected injection over HTTP: %q", got)
	}
}

func TestHostHeaderMismatchBlocksInjection(t *testing.T) {
	p := &Proxy{
		config: Config{
			Secrets: map[string]string{
				"anthropic": "REAL_SECRET",
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}

	req := httptest.NewRequest(http.MethodGet, "https://evil.example/v1/foo", nil)
	req.Host = "api.anthropic.com"
	req.Header.Set("Authorization", "Bearer NETCLODE_PLACEHOLDER_anthropic")
	ctx := &goproxy.ProxyCtx{
		Req:      req,
		UserData: "Bearer test-token",
	}

	_, resp := p.handleRequest(req, ctx)
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden response on host mismatch, got %+v", resp)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer NETCLODE_PLACEHOLDER_anthropic" {
		t.Fatalf("unexpected injection on host mismatch: %q", got)
	}
}

func TestConnectHostMismatchBlocksInjection(t *testing.T) {
	p := &Proxy{
		config: Config{
			Secrets: map[string]string{
				"anthropic": "REAL_SECRET",
			},
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	}

	req := httptest.NewRequest(http.MethodGet, "https://evil.example/v1/foo", nil)
	req.Header.Set("Authorization", "Bearer NETCLODE_PLACEHOLDER_anthropic")
	ctx := &goproxy.ProxyCtx{
		Req: req,
		UserData: connectMeta{
			proxyAuth:   "Bearer test-token",
			connectHost: "api.anthropic.com",
		},
	}

	_, resp := p.handleRequest(req, ctx)
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden response on connect host mismatch, got %+v", resp)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer NETCLODE_PLACEHOLDER_anthropic" {
		t.Fatalf("unexpected injection on connect host mismatch: %q", got)
	}
}
