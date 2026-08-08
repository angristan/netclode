// auth-proxy is a tiny HTTP proxy that adds ServiceAccount token authentication.
//
// It sits between the SDK HTTP client and the external secret-proxy:
//
//	SDK → auth-proxy (localhost:8080) → secret-proxy (external)
//
// The auth-proxy:
// 1. Reads the K8s ServiceAccount token from a mounted file
// 2. Adds Proxy-Authorization: Bearer <token> to each request
// 3. Forwards to the external secret-proxy
//
// This proxy has NO secrets - it only adds authentication.
// The actual secrets are in the external secret-proxy (outside the microVM).
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	tokenPath     = getEnv("TOKEN_PATH", "/var/run/secrets/proxy-auth/token")
	upstreamProxy = getEnv("UPSTREAM_PROXY", "http://secret-proxy.netclode.svc.cluster.local:8080")
	listenAddr    = getEnv("LISTEN_ADDR", ":8080")
)

func main() {
	// Verify token is readable at startup
	if _, err := readToken(); err != nil {
		log.Printf("Warning: initial token read failed: %v", err)
	}

	// Create dialer for upstream connections
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	handler := &proxyHandler{
		upstream: upstreamProxy,
		dialer:   dialer,
	}

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("auth-proxy starting on %s, forwarding to %s", listenAddr, upstreamProxy)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// readToken reads the SA token fresh from disk on every call.
// The projected volume is a tmpfs, so this is effectively a memory read.
func readToken() (string, error) {
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func getToken() string {
	token, err := readToken()
	if err != nil {
		log.Printf("Warning: failed to read token: %v", err)
		return ""
	}
	return token
}

type proxyHandler struct {
	upstream string
	dialer   *net.Dialer
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Handle CONNECT method (HTTPS tunneling)
	if r.Method == http.MethodConnect {
		h.handleConnect(w, r)
		return
	}

	// Handle regular HTTP requests
	h.handleHTTP(w, r)
}

func (h *proxyHandler) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Create request to upstream proxy
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Copy headers
	for key, values := range r.Header {
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}

	// Add Proxy-Authorization
	token := getToken()
	if token != "" {
		upstreamReq.Header.Set("Proxy-Authorization", "Bearer "+token)
	}

	// Send via upstream proxy
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(h.upstream)),
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	_, _ = io.Copy(w, resp.Body)
}

func (h *proxyHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	// Add Proxy-Authorization
	token := getToken()

	// Connect to upstream proxy
	upstreamConn, err := h.dialer.DialContext(r.Context(), "tcp", strings.TrimPrefix(h.upstream, "http://"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Send CONNECT request with auth header
	connectReq := "CONNECT " + r.Host + " HTTP/1.1\r\n"
	connectReq += "Host: " + r.Host + "\r\n"
	if token != "" {
		connectReq += "Proxy-Authorization: Bearer " + token + "\r\n"
	}
	connectReq += "\r\n"

	if _, err := upstreamConn.Write([]byte(connectReq)); err != nil {
		_ = upstreamConn.Close()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Read the CONNECT response using a proper HTTP parser.
	// A raw Read() could under-read (partial header) or over-read (consume
	// early TLS bytes from the tunnel), both of which corrupt the tunnel and
	// cause HPE_INVALID_VERSION on the client side.
	br := bufio.NewReader(upstreamConn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = upstreamConn.Close()
		http.Error(w, "reading upstream CONNECT response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK {
		_ = upstreamConn.Close()
		http.Error(w, fmt.Sprintf("upstream proxy rejected CONNECT: %s", resp.Status), http.StatusBadGateway)
		return
	}

	// Hijack the client connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstreamConn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		_ = upstreamConn.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send 200 OK to client
	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// If the buffered reader consumed bytes beyond the HTTP response
	// (e.g. the start of the TLS handshake), forward them to the client
	// before starting the bidirectional copy.
	if br.Buffered() > 0 {
		buffered, _ := br.Peek(br.Buffered())
		if len(buffered) > 0 {
			_, _ = clientConn.Write(buffered)
		}
		// Discard so the bufio.Reader is empty
		_, _ = br.Discard(len(buffered))
	}

	// Bidirectional copy using the raw connection
	go func() {
		_, _ = io.Copy(upstreamConn, clientConn)
		_ = upstreamConn.Close()
	}()
	go func() {
		_, _ = io.Copy(clientConn, upstreamConn)
		_ = clientConn.Close()
	}()
}

func mustParseURL(rawurl string) *url.URL {
	u, err := url.Parse(rawurl)
	if err != nil {
		panic(err)
	}
	return u
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
