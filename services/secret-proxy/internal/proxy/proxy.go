// Package proxy implements a MITM proxy that injects secrets into HTTP headers.
//
// The proxy validates each request with the control-plane:
// 1. Extracts Proxy-Authorization header (K8s ServiceAccount token)
// 2. Calls control-plane to validate token and check if target host is allowed
// 3. If allowed, replaces the placeholder in request headers with the real secret
//
// This prevents secret exfiltration because:
// 1. Real secrets never enter the sandbox (they're stored in control-plane)
// 2. Control-plane enforces per-session allowlists based on SDK type
// 3. Replacement only happens in headers, not request bodies
package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	httptrace "github.com/DataDog/dd-trace-go/contrib/net/http/v2"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/ext"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/angristan/netclode/services/secret-proxy/internal/metrics"
	"github.com/elazarl/goproxy"
)

// Config holds the proxy configuration.
type Config struct {
	// ListenAddr is the address to listen on (e.g., ":8080").
	ListenAddr string

	// ControlPlaneURL is the URL of the control-plane for auth validation.
	ControlPlaneURL string

	// ControlPlaneTokenPath contains the projected identity token for this workload.
	ControlPlaneTokenPath string

	// Secrets maps secret keys to their actual values.
	// e.g., {"anthropic": "sk-ant-...", "openai": "sk-..."}
	Secrets map[string]string

	// CA is the TLS certificate used for MITM.
	CA tls.Certificate

	// Verbose enables verbose logging.
	Verbose bool
}

// validateProxyAuthRequest is the request to control-plane.
type validateProxyAuthRequest struct {
	Token      string `json:"token"`
	TargetHost string `json:"target_host"`
}

// validateProxyAuthResponse is the response from control-plane.
type validateProxyAuthResponse struct {
	Allowed     bool   `json:"allowed"`
	SecretKey   string `json:"secret_key,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Proxy is a MITM proxy that injects secrets into HTTP headers.
type Proxy struct {
	config     Config
	server     *goproxy.ProxyHttpServer
	logger     *slog.Logger
	httpClient *http.Client
}

// connectMeta carries data from the CONNECT request into MITM'd requests.
// This lets us enforce "CONNECT host must match request host".
type connectMeta struct {
	proxyAuth   string
	connectHost string
}

// canonicalHost normalizes a host for policy checks:
// lower-case, no port, no trailing dot, IPv6-safe.
func canonicalHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	host := raw
	if strings.HasPrefix(host, "[") {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		} else {
			host = strings.Trim(host, "[]")
		}
	} else if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// New creates a new secret injection proxy.
func New(cfg Config, logger *slog.Logger) *Proxy {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = cfg.Verbose

	// goproxy's default transport skips upstream TLS verification
	// (InsecureSkipVerify), which would hand real secrets to any spoofed
	// upstream. Override it with a verifying transport.
	proxy.Tr = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	// Set up custom CA for MITM
	goproxy.GoproxyCa = cfg.CA
	goproxy.OkConnect = &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: goproxy.TLSConfigFromCA(&cfg.CA)}
	goproxy.MitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: goproxy.TLSConfigFromCA(&cfg.CA)}
	goproxy.RejectConnect = &goproxy.ConnectAction{Action: goproxy.ConnectReject, TLSConfig: goproxy.TLSConfigFromCA(&cfg.CA)}

	p := &Proxy{
		config: cfg,
		server: proxy,
		logger: logger,
		httpClient: httptrace.WrapClient(&http.Client{
			Timeout: 10 * time.Second,
		}, httptrace.WithService("control-plane-validation")),
	}

	// Custom CONNECT handler to capture Proxy-Authorization header from CONNECT request
	// This is important because after MITM the request handler sees requests inside
	// the TLS tunnel, which don't have the original CONNECT headers.
	proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		// Preserve CONNECT target host and auth for use in handleRequest.
		meta := connectMeta{
			connectHost: canonicalHost(host),
		}
		// Extract Proxy-Authorization from CONNECT request
		if ctx.Req != nil {
			proxyAuth := ctx.Req.Header.Get("Proxy-Authorization")
			if proxyAuth != "" {
				meta.proxyAuth = proxyAuth
				logger.Debug("Captured Proxy-Authorization from CONNECT", "host", host)
			}
		}
		ctx.UserData = meta
		// Continue with MITM
		return goproxy.MitmConnect, host
	})

	// Add request handler for secret injection
	proxy.OnRequest().DoFunc(p.handleRequest)

	return p
}

// handleRequest processes each request and injects secrets where appropriate.
func (p *Proxy) handleRequest(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	span, spanCtx := tracer.StartSpanFromContext(req.Context(), "secret-proxy.handle",
		tracer.SpanType(ext.SpanTypeWeb),
		tracer.Tag("target.host", req.Host),
	)
	defer span.Finish()
	req = req.WithContext(spanCtx)

	metrics.Incr("proxy.requests", []string{"host:" + canonicalHost(req.Host)})

	rawHost := req.Host
	if rawHost == "" && req.URL != nil {
		rawHost = req.URL.Host
	}

	scheme := ""
	if req.URL != nil {
		scheme = strings.ToLower(req.URL.Scheme)
	}
	if scheme == "" && req.TLS != nil {
		scheme = "https"
	}
	if scheme != "https" {
		// Never inject secrets into plain HTTP requests.
		p.logger.DebugContext(spanCtx, "Non-HTTPS request, skipping secret injection",
			"host", rawHost,
			"scheme", scheme,
		)
		return req, nil
	}

	urlHost := ""
	if req.URL != nil {
		urlHost = canonicalHost(req.URL.Host)
	}
	reqHost := canonicalHost(req.Host)
	targetHost := urlHost
	if targetHost == "" {
		targetHost = reqHost
	}
	if targetHost == "" {
		p.logger.DebugContext(spanCtx, "Missing host, skipping secret injection")
		return req, nil
	}
	// Guard against Host header spoofing: it must match the actual target host.
	if urlHost != "" && reqHost != "" && urlHost != reqHost {
		p.logger.WarnContext(spanCtx, "Host header mismatch",
			"urlHost", urlHost,
			"reqHost", reqHost,
		)
		return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden, "host mismatch")
	}

	// Extract Proxy-Authorization header
	// First try ctx.UserData (captured from CONNECT request for HTTPS)
	// Fall back to request header (for plain HTTP proxying)
	var (
		proxyAuth   string
		connectHost string
	)
	if ctx.UserData != nil {
		switch meta := ctx.UserData.(type) {
		case connectMeta:
			proxyAuth = meta.proxyAuth
			connectHost = meta.connectHost
		case *connectMeta:
			if meta != nil {
				proxyAuth = meta.proxyAuth
				connectHost = meta.connectHost
			}
		case string:
			proxyAuth = meta
		}
	}
	// Enforce that HTTPS requests stay within the CONNECT target host.
	if connectHost != "" && connectHost != targetHost {
		p.logger.WarnContext(spanCtx, "CONNECT host mismatch",
			"connectHost", connectHost,
			"targetHost", targetHost,
		)
		return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden, "connect host mismatch")
	}
	if proxyAuth == "" {
		proxyAuth = req.Header.Get("Proxy-Authorization")
	}
	if proxyAuth == "" {
		// No auth header - pass through without injection
		p.logger.DebugContext(spanCtx, "No Proxy-Authorization header, passing through", "host", targetHost)
		return req, nil
	}

	// Remove the "Bearer " prefix
	token := strings.TrimPrefix(proxyAuth, "Bearer ")
	if token == proxyAuth {
		p.logger.DebugContext(spanCtx, "Invalid Proxy-Authorization format (expected Bearer)", "host", targetHost)
		return req, nil
	}

	// Remove Proxy-Authorization header before forwarding (it's for the proxy, not upstream)
	req.Header.Del("Proxy-Authorization")

	// Validate with control-plane using token
	authStart := time.Now()
	authResult, err := p.validateWithControlPlane(token, targetHost)
	metrics.Distribution("proxy.auth.duration_ms", float64(time.Since(authStart).Milliseconds()), []string{"host:" + targetHost})
	if err != nil {
		p.logger.ErrorContext(spanCtx, "Control-plane validation failed", "host", targetHost, "error", err)
		metrics.Incr("proxy.auth.errors", []string{"host:" + targetHost})
		// Block the request — passing through would leak the placeholder key upstream.
		return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusProxyAuthRequired,
			fmt.Sprintf("secret-proxy: validation failed (%v)", err))
	}

	if !authResult.Allowed {
		// Host is not in the allowlist — no secret to inject, pass through.
		// This is safe: placeholders only exist in headers sent to allowlisted API hosts
		// (e.g., Anthropic, OpenAI). Requests to non-allowlisted hosts (npm registry,
		// GitHub, etc.) don't carry placeholders and should reach the internet normally.
		p.logger.DebugContext(spanCtx, "Host not in allowlist, passing through without injection",
			"host", targetHost,
			"sessionID", authResult.SessionID,
		)
		return req, nil
	}

	// Get the actual secret value
	secretValue, ok := p.config.Secrets[authResult.SecretKey]
	if !ok {
		p.logger.ErrorContext(spanCtx, "Secret key not found in config",
			"secretKey", authResult.SecretKey,
			"host", targetHost,
			"sessionID", authResult.SessionID,
		)
		// Block — the placeholder is still in the headers and we can't inject the real secret.
		return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusProxyAuthRequired,
			fmt.Sprintf("secret-proxy: secret key %q not configured", authResult.SecretKey))
	}

	// Replace placeholder in headers ONLY (not in body - prevents reflection attacks)
	injected := false
	for name, values := range req.Header {
		for i, value := range values {
			if strings.Contains(value, authResult.Placeholder) {
				req.Header[name][i] = strings.Replace(value, authResult.Placeholder, secretValue, -1)
				injected = true
			}
		}
	}

	if injected {
		p.logger.InfoContext(spanCtx, "Injected secret into request",
			"host", targetHost,
			"secretKey", authResult.SecretKey,
			"sessionID", authResult.SessionID,
		)
		metrics.Incr("proxy.secrets.injected", []string{"secret_key:" + authResult.SecretKey, "host:" + targetHost})
	}

	return req, nil
}

// validateWithControlPlane calls the control-plane to validate the proxy auth request.
func (p *Proxy) validateWithControlPlane(token, targetHost string) (*validateProxyAuthResponse, error) {
	reqBody := validateProxyAuthRequest{
		Token:      token,
		TargetHost: targetHost,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimSuffix(p.config.ControlPlaneURL, "/") + "/internal/validate-proxy-auth"
	var workloadToken string
	if p.config.ControlPlaneTokenPath != "" {
		token, err := os.ReadFile(p.config.ControlPlaneTokenPath)
		if err != nil {
			return nil, fmt.Errorf("read control-plane token: %w", err)
		}
		workloadToken = strings.TrimSpace(string(token))
		if workloadToken == "" {
			return nil, fmt.Errorf("control-plane token is empty")
		}
	}

	// Retry up to 3 times with backoff for transient failures (K8s API timeouts, etc.).
	// Without retries, a single K8s API hiccup causes the placeholder key to leak to Anthropic.
	const maxRetries = 3
	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 500 * time.Millisecond
			p.logger.Debug("Retrying control-plane validation", "attempt", attempt+1, "backoff", backoff, "host", targetHost)
			time.Sleep(backoff)
		}

		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if workloadToken != "" {
			req.Header.Set("Authorization", "Bearer "+workloadToken)
		}

		resp, err := p.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue // Retry on network/timeout errors
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response: %w", err)
			continue
		}

		// 5xx = control-plane overloaded, retry
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
			continue
		}

		var result validateProxyAuthResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}

		// 401 = auth validation failed (bad token, session not found, etc.).
		// Return an error so the caller BLOCKS the request instead of passing through.
		// Passing through would leak the placeholder key to the upstream API.
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("auth validation failed: %s", result.Error)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, result.Error)
		}

		return &result, nil
	}

	return nil, lastErr
}

// ListenAndServe starts the proxy server.
func (p *Proxy) ListenAndServe() error {
	slog.Info("Starting secret proxy", "addr", p.config.ListenAddr)
	return http.ListenAndServe(p.config.ListenAddr, p.server)
}
