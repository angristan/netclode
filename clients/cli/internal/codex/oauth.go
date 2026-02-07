package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	AuthBase    = "https://auth.openai.com"
	RedirectURI = "https://auth.openai.com/deviceauth/callback"
)

type DeviceCode struct {
	DeviceAuthID    string `json:"device_auth_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string
	Interval        json.Number `json:"interval"`
}

type CodeExchange struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
}

// RequestDeviceCode requests a device code from OpenAI auth
func RequestDeviceCode() (*DeviceCode, error) {
	body, _ := json.Marshal(map[string]string{"client_id": ClientID})
	resp, err := http.Post(
		AuthBase+"/api/accounts/deviceauth/usercode",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to request device code: %d", resp.StatusCode)
	}

	var dc DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, err
	}
	dc.VerificationURL = AuthBase + "/codex/device"
	if dc.Interval == "" {
		dc.Interval = "5"
	}
	return &dc, nil
}

// PollForAuthorization polls until user authorizes or timeout
func PollForAuthorization(dc *DeviceCode, timeout time.Duration) (*CodeExchange, error) {
	deadline := time.Now().Add(timeout)

	interval, _ := dc.Interval.Int64()
	if interval == 0 {
		interval = 5
	}

	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval) * time.Second)

		body, _ := json.Marshal(map[string]string{
			"device_auth_id": dc.DeviceAuthID,
			"user_code":      dc.UserCode,
		})
		resp, err := http.Post(
			AuthBase+"/api/accounts/deviceauth/token",
			"application/json",
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}

		// 403/404 = still waiting
		if resp.StatusCode == 403 || resp.StatusCode == 404 {
			_ = resp.Body.Close()
			continue
		}

		if resp.StatusCode != 200 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("poll failed: %d", resp.StatusCode)
		}

		var ce CodeExchange
		err = json.NewDecoder(resp.Body).Decode(&ce)
		_ = resp.Body.Close()
		return &ce, err
	}

	return nil, fmt.Errorf("device code expired (timeout)")
}

// ExchangeCodeForTokens exchanges authorization code for tokens
func ExchangeCodeForTokens(ce *CodeExchange) (*Tokens, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {ce.AuthorizationCode},
		"redirect_uri":  {RedirectURI},
		"client_id":     {ClientID},
		"code_verifier": {ce.CodeVerifier},
	}

	resp, err := http.Post(
		AuthBase+"/oauth/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange failed: %d", resp.StatusCode)
	}

	var t Tokens
	return &t, json.NewDecoder(resp.Body).Decode(&t)
}

// RefreshTokens refreshes the access token using refresh token
func RefreshTokens(refreshToken string) (*Tokens, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	}

	resp, err := http.Post(
		AuthBase+"/oauth/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token refresh failed: %d", resp.StatusCode)
	}

	var t Tokens
	return &t, json.NewDecoder(resp.Body).Decode(&t)
}
