package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/angristan/netclode/services/control-plane/internal/storage"
)

const (
	codexOAuthClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthTokenEndpoint = "https://auth.openai.com/oauth/token"
	codexRefreshLeadTime    = 12 * time.Hour
	codexRefreshURLOverride = "CODEX_REFRESH_TOKEN_URL_OVERRIDE"
)

type codexRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IdToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (m *Manager) prepareCodexOAuthForPrompt(ctx context.Context, sessionID string) (*storage.CodexOAuthSessionData, error) {
	data, err := m.getCodexOAuth(ctx)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("missing codex oauth data")
	}
	if data.AccessToken == "" || data.IdToken == "" {
		return nil, fmt.Errorf("incomplete codex oauth data")
	}

	now := time.Now().UTC()
	if data.ExpiresAt == nil {
		if inferred := inferCodexTokenExpiry(data.AccessToken, data.IdToken, 0, now); inferred != nil {
			data.ExpiresAt = inferred
			data.UpdatedAt = now
			if err := m.saveCodexOAuth(ctx, data); err != nil {
				return nil, fmt.Errorf("save inferred token expiry: %w", err)
			}
		}
	}

	if shouldRefreshCodexOAuth(data, now) {
		if data.RefreshToken == "" {
			return nil, fmt.Errorf("oauth refresh required but refresh token is missing")
		}

		slog.Info("Refreshing Codex OAuth tokens", "sessionID", sessionID)
		refreshed, err := refreshCodexOAuth(ctx, data.RefreshToken)
		if err != nil {
			slog.Warn("Codex OAuth refresh failed", "sessionID", sessionID, "error", err)
			return nil, err
		}
		if refreshed.AccessToken != "" {
			data.AccessToken = refreshed.AccessToken
		}
		if refreshed.IdToken != "" {
			data.IdToken = refreshed.IdToken
		}
		if refreshed.RefreshToken != "" {
			data.RefreshToken = refreshed.RefreshToken
		}
		if data.AccessToken == "" || data.IdToken == "" {
			return nil, fmt.Errorf("refresh response missing required access/id token")
		}

		data.ExpiresAt = inferCodexTokenExpiry(data.AccessToken, data.IdToken, refreshed.ExpiresIn, now)
		data.UpdatedAt = now
		if err := m.saveCodexOAuth(ctx, data); err != nil {
			return nil, fmt.Errorf("save refreshed oauth data: %w", err)
		}
		slog.Info("Codex OAuth refresh succeeded", "sessionID", sessionID)
	}

	return data, nil
}

func shouldRefreshCodexOAuth(data *storage.CodexOAuthSessionData, now time.Time) bool {
	if data == nil || data.ExpiresAt == nil {
		return true
	}
	return now.Add(codexRefreshLeadTime).After(data.ExpiresAt.UTC())
}

func refreshCodexOAuth(ctx context.Context, refreshToken string) (*codexRefreshResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {codexOAuthClientID},
		"refresh_token": {refreshToken},
	}

	endpoint := os.Getenv(codexRefreshURLOverride)
	// Backward-compatible alias for older tests/config.
	if endpoint == "" {
		endpoint = os.Getenv("CODEX_OAUTH_TOKEN_URL_OVERRIDE")
	}
	if endpoint == "" {
		endpoint = codexOAuthTokenEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh token failed with status %d", resp.StatusCode)
	}

	var refreshed codexRefreshResponse
	if err := json.Unmarshal(body, &refreshed); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	return &refreshed, nil
}

func inferCodexTokenExpiry(accessToken, idToken string, expiresIn int64, now time.Time) *time.Time {
	if expiresIn > 0 {
		t := now.Add(time.Duration(expiresIn) * time.Second).UTC()
		return &t
	}
	if t := parseJWTExp(accessToken); t != nil {
		return t
	}
	if t := parseJWTExp(idToken); t != nil {
		return t
	}
	return nil
}

func parseJWTExp(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return nil
	}
	t := time.Unix(claims.Exp, 0).UTC()
	return &t
}
