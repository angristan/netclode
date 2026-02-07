package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	storedTokensFilename = "codex-auth.json"
)

// StoredTokens represents locally cached Codex OAuth tokens.
type StoredTokens struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	IDToken      string     `json:"id_token"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func storedTokensPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "netclode", storedTokensFilename), nil
}

func LoadStoredTokens() (*StoredTokens, error) {
	path, err := storedTokensPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var tokens StoredTokens
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func SaveStoredTokens(tokens *StoredTokens) error {
	if tokens == nil {
		return fmt.Errorf("tokens are required")
	}
	path, err := storedTokensPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func DeleteStoredTokens() error {
	path, err := storedTokensPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func SaveOAuthTokens(tokens *Tokens) (*StoredTokens, error) {
	if tokens == nil {
		return nil, fmt.Errorf("tokens are required")
	}
	now := time.Now().UTC()
	stored := &StoredTokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		ExpiresAt:    inferTokenExpiry(tokens.AccessToken, tokens.IDToken, tokens.ExpiresIn, now),
		UpdatedAt:    now,
	}
	if err := SaveStoredTokens(stored); err != nil {
		return nil, err
	}
	return stored, nil
}

func EnsureFreshStoredTokens(skew time.Duration) (*StoredTokens, error) {
	tokens, err := LoadStoredTokens()
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		return nil, nil
	}

	now := time.Now().UTC()
	if tokens.ExpiresAt == nil {
		tokens.ExpiresAt = inferTokenExpiry(tokens.AccessToken, tokens.IDToken, 0, now)
	}

	if tokens.ExpiresAt != nil && now.Add(skew).Before(tokens.ExpiresAt.UTC()) {
		return tokens, nil
	}

	if tokens.RefreshToken == "" {
		return nil, fmt.Errorf("stored OAuth token is expired and has no refresh token")
	}

	refreshed, err := RefreshTokens(tokens.RefreshToken)
	if err != nil {
		return nil, err
	}
	if refreshed.AccessToken != "" {
		tokens.AccessToken = refreshed.AccessToken
	}
	if refreshed.IDToken != "" {
		tokens.IDToken = refreshed.IDToken
	}
	if refreshed.RefreshToken != "" {
		tokens.RefreshToken = refreshed.RefreshToken
	}
	tokens.ExpiresAt = inferTokenExpiry(tokens.AccessToken, tokens.IDToken, refreshed.ExpiresIn, now)
	tokens.UpdatedAt = now

	if err := SaveStoredTokens(tokens); err != nil {
		return nil, err
	}
	return tokens, nil
}

func inferTokenExpiry(accessToken, idToken string, expiresIn int64, now time.Time) *time.Time {
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
