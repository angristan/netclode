package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	codexOAuthBaseURL     = "https://auth.openai.com"
	codexOAuthFlowTTL     = 15 * time.Minute
	codexOAuthHTTPTimeout = 15 * time.Second
)

type codexAuthPendingState struct {
	id                      string
	deviceAuthID            string
	userCode                string
	verificationURI         string
	verificationURIComplete string
	intervalSeconds         int32
	expiresAt               time.Time
}

type codexDeviceCodeResponse struct {
	DeviceAuthID string      `json:"device_auth_id"`
	UserCode     string      `json:"user_code"`
	Interval     json.Number `json:"interval"`
}

type codexCodeExchange struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

type codexTokenExchangeResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IdToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
}

func (m *Manager) StartCodexAuth(ctx context.Context) (*pb.CodexAuthStartedResponse, error) {
	now := time.Now().UTC()
	if len(m.config.CodexOAuthEncryptionKey) != 32 {
		return nil, fmt.Errorf("codex oauth is not configured: CODEX_OAUTH_ENCRYPTION_KEY_B64 must decode to 32 bytes")
	}

	m.codexAuthMu.Lock()
	if p := m.codexAuthPending; p != nil && now.Before(p.expiresAt) {
		resp := &pb.CodexAuthStartedResponse{
			VerificationUri:         p.verificationURI,
			VerificationUriComplete: strPtr(p.verificationURIComplete),
			UserCode:                p.userCode,
			IntervalSeconds:         p.intervalSeconds,
			ExpiresAt:               timestamppb.New(p.expiresAt),
		}
		m.codexAuthMu.Unlock()
		return resp, nil
	}
	m.codexAuthMu.Unlock()

	deviceCode, err := requestCodexDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

	intervalSeconds, _ := deviceCode.Interval.Int64()
	if intervalSeconds <= 0 {
		intervalSeconds = 5
	}

	pending := &codexAuthPendingState{
		id:                      uuid.NewString(),
		deviceAuthID:            deviceCode.DeviceAuthID,
		userCode:                deviceCode.UserCode,
		verificationURI:         codexOAuthBaseURL + "/codex/device",
		verificationURIComplete: "",
		intervalSeconds:         int32(intervalSeconds),
		expiresAt:               now.Add(codexOAuthFlowTTL),
	}

	m.codexAuthMu.Lock()
	m.codexAuthPending = pending
	m.codexAuthLastError = ""
	m.codexAuthLastErrorAt = time.Time{}
	m.codexAuthMu.Unlock()

	go m.finishCodexAuthFlow(pending)

	return &pb.CodexAuthStartedResponse{
		VerificationUri:         pending.verificationURI,
		VerificationUriComplete: strPtr(pending.verificationURIComplete),
		UserCode:                pending.userCode,
		IntervalSeconds:         pending.intervalSeconds,
		ExpiresAt:               timestamppb.New(pending.expiresAt),
	}, nil
}

func (m *Manager) finishCodexAuthFlow(pending *codexAuthPendingState) {
	ctx, cancel := context.WithDeadline(context.Background(), pending.expiresAt)
	defer cancel()

	exchange, err := pollCodexAuthorization(ctx, pending.deviceAuthID, pending.userCode, time.Duration(pending.intervalSeconds)*time.Second)
	if err != nil {
		m.failCodexAuthFlow(pending.id, err.Error())
		return
	}
	if !m.isCodexAuthFlowCurrent(pending.id) {
		return
	}

	tokens, err := exchangeCodexAuthCode(ctx, exchange)
	if err != nil {
		m.failCodexAuthFlow(pending.id, err.Error())
		return
	}
	if !m.isCodexAuthFlowCurrent(pending.id) {
		return
	}

	now := time.Now().UTC()
	data := &storage.CodexOAuthSessionData{
		AccessToken:  tokens.AccessToken,
		IdToken:      tokens.IdToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    inferCodexTokenExpiry(tokens.AccessToken, tokens.IdToken, tokens.ExpiresIn, now),
		UpdatedAt:    now,
	}
	if err := m.saveCodexOAuth(context.Background(), data); err != nil {
		m.failCodexAuthFlow(pending.id, err.Error())
		return
	}

	m.codexAuthMu.Lock()
	if m.codexAuthPending != nil && m.codexAuthPending.id == pending.id {
		m.codexAuthPending = nil
		m.codexAuthLastError = ""
		m.codexAuthLastErrorAt = time.Time{}
	}
	m.codexAuthMu.Unlock()
	slog.Info("Codex OAuth authentication completed")
}

func (m *Manager) isCodexAuthFlowCurrent(flowID string) bool {
	m.codexAuthMu.Lock()
	defer m.codexAuthMu.Unlock()
	return m.codexAuthPending != nil && m.codexAuthPending.id == flowID
}

func (m *Manager) failCodexAuthFlow(flowID, message string) {
	m.codexAuthMu.Lock()
	defer m.codexAuthMu.Unlock()
	if m.codexAuthPending == nil || m.codexAuthPending.id != flowID {
		return
	}
	m.codexAuthPending = nil
	m.codexAuthLastError = message
	m.codexAuthLastErrorAt = time.Now().UTC()
	slog.Warn("Codex OAuth authentication failed", "error", message)
}

func (m *Manager) GetCodexAuthStatus(ctx context.Context) (*pb.CodexAuthStatusResponse, error) {
	now := time.Now().UTC()

	m.codexAuthMu.Lock()
	if p := m.codexAuthPending; p != nil {
		if now.Before(p.expiresAt) {
			resp := &pb.CodexAuthStatusResponse{
				State:     pb.CodexAuthState_CODEX_AUTH_STATE_PENDING,
				ExpiresAt: timestamppb.New(p.expiresAt),
			}
			m.codexAuthMu.Unlock()
			return resp, nil
		}
		m.codexAuthPending = nil
		m.codexAuthLastError = "authorization timed out"
		m.codexAuthLastErrorAt = now
	}
	lastErr := m.codexAuthLastError
	m.codexAuthMu.Unlock()

	data, err := m.getCodexOAuth(ctx)
	if err != nil {
		return nil, err
	}
	if data != nil && data.AccessToken != "" && data.IdToken != "" && data.RefreshToken != "" {
		resp := &pb.CodexAuthStatusResponse{
			State: pb.CodexAuthState_CODEX_AUTH_STATE_READY,
		}
		if data.ExpiresAt != nil {
			resp.ExpiresAt = timestamppb.New(*data.ExpiresAt)
		}
		if accountID := codexAccountIDFromIDToken(data.IdToken); accountID != "" {
			resp.AccountId = &accountID
		}
		return resp, nil
	}

	if lastErr != "" {
		return &pb.CodexAuthStatusResponse{
			State: pb.CodexAuthState_CODEX_AUTH_STATE_ERROR,
			Error: &lastErr,
		}, nil
	}

	return &pb.CodexAuthStatusResponse{
		State: pb.CodexAuthState_CODEX_AUTH_STATE_UNAUTHENTICATED,
	}, nil
}

func (m *Manager) LogoutCodexAuth(ctx context.Context) error {
	m.codexAuthMu.Lock()
	m.codexAuthPending = nil
	m.codexAuthLastError = ""
	m.codexAuthLastErrorAt = time.Time{}
	m.codexAuthMu.Unlock()
	return m.storage.DeleteCodexOAuth(ctx)
}

func requestCodexDeviceCode(ctx context.Context) (*codexDeviceCodeResponse, error) {
	payload, _ := json.Marshal(map[string]string{"client_id": codexOAuthClientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthBaseURL+"/api/accounts/deviceauth/usercode", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: codexOAuthHTTPTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("request device code failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var out codexDeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func pollCodexAuthorization(ctx context.Context, deviceAuthID, userCode string, interval time.Duration) (*codexCodeExchange, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}

	client := &http.Client{Timeout: codexOAuthHTTPTimeout}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		payload, _ := json.Marshal(map[string]string{
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthBaseURL+"/api/accounts/deviceauth/token", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			return nil, fmt.Errorf("device authorization failed: status=%d body=%s", resp.StatusCode, string(body))
		}
		var out codexCodeExchange
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return &out, err
	}
}

func exchangeCodexAuthCode(ctx context.Context, exchange *codexCodeExchange) (*codexTokenExchangeResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {exchange.AuthorizationCode},
		"redirect_uri":  {"https://auth.openai.com/deviceauth/callback"},
		"client_id":     {codexOAuthClientID},
		"code_verifier": {exchange.CodeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthBaseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: codexOAuthHTTPTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("token exchange failed: status=%d body=%s", resp.StatusCode, string(body))
	}

	var out codexTokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" || out.IdToken == "" || out.RefreshToken == "" {
		return nil, fmt.Errorf("token exchange response missing required tokens")
	}
	return &out, nil
}

func codexAccountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}
