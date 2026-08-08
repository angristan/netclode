// Package github provides GitHub App authentication and token generation.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// RepoAccess specifies the level of access for repository operations.
type RepoAccess string

const (
	// RepoAccessRead allows read-only access (clone, fetch).
	RepoAccessRead RepoAccess = "read"
	// RepoAccessWrite allows read-write access (clone, push).
	RepoAccessWrite RepoAccess = "write"
)

// Client handles GitHub App authentication and token generation.
type Client struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	httpClient     *http.Client
}

// NewClient creates a new GitHub client.
func NewClient(appID, installationID int64, privateKeyPEM string) (*Client, error) {
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	return &Client{
		appID:          appID,
		installationID: installationID,
		privateKey:     privateKey,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: otelhttp.NewTransport(nil),
		},
	}, nil
}

// parsePrivateKey parses a PEM-encoded RSA private key.
// Supports both PKCS#1 and PKCS#8 formats.
func parsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	// Try PKCS#1 first (GitHub's default format)
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	// Try PKCS#8
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}

	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}

	return rsaKey, nil
}

// createJWT creates a JWT for GitHub App authentication.
func (c *Client) createJWT() (string, error) {
	now := time.Now()

	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", c.appID),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(c.privateKey)
}

// InstallationToken represents a GitHub installation access token.
type InstallationToken struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// tokenRequest is the request body for creating an installation access token.
type tokenRequest struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// CreateRepoToken creates an installation token scoped to specific repositories.
// The token is scoped to only the specified repos with the given access level.
func (c *Client) CreateRepoToken(ctx context.Context, repos []string, access RepoAccess) (*InstallationToken, error) {
	jwt, err := c.createJWT()
	if err != nil {
		return nil, fmt.Errorf("create JWT: %w", err)
	}

	// Build request body with scoped permissions
	reqBody := tokenRequest{
		Permissions: map[string]string{
			"contents": string(access),
			"metadata": "read",
		},
	}

	// Add write permissions if needed
	if access == RepoAccessWrite {
		reqBody.Permissions["pull_requests"] = "write"
	}

	// Scope token to the specific repositories
	var repoNames []string
	for _, repo := range repos {
		repoName := extractRepoName(repo)
		if repoName == "" {
			continue
		}
		repoNames = append(repoNames, repoName)
	}
	if len(repoNames) > 0 {
		reqBody.Repositories = repoNames
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", c.installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		slog.Error("GitHub API error creating token", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("GitHub API error: %s - %s", resp.Status, string(body))
	}

	var token InstallationToken
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	slog.Info("Created GitHub repo-scoped token",
		"repos", repos,
		"access", access,
		"expiresAt", token.ExpiresAt,
	)

	return &token, nil
}

// Repo represents a GitHub repository.
type Repo struct {
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Private     bool   `json:"private"`
	Description string `json:"description"`
}

// listReposResponse is the GitHub API response for listing installation repositories.
type listReposResponse struct {
	TotalCount   int       `json:"total_count"`
	Repositories []apiRepo `json:"repositories"`
}

type apiRepo struct {
	Name        string  `json:"name"`
	FullName    string  `json:"full_name"`
	Private     bool    `json:"private"`
	Description *string `json:"description"`
}

// ListRepos returns all repositories accessible to the GitHub App installation.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	// First get an installation token
	jwt, err := c.createJWT()
	if err != nil {
		return nil, fmt.Errorf("create JWT: %w", err)
	}

	tokenURL := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", c.installationID)
	tokenReq, err := http.NewRequestWithContext(ctx, "POST", tokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create token request: %w", err)
	}
	tokenReq.Header.Set("Authorization", "Bearer "+jwt)
	tokenReq.Header.Set("Accept", "application/vnd.github+json")
	tokenReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	tokenResp, err := c.httpClient.Do(tokenReq)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(tokenResp.Body)
		return nil, fmt.Errorf("GitHub API error getting token: %s - %s", tokenResp.Status, string(body))
	}

	var tokenData InstallationToken
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	// Now list repositories using the installation token
	var allRepos []Repo
	page := 1
	perPage := 100

	for {
		url := fmt.Sprintf("https://api.github.com/installation/repositories?per_page=%d&page=%d", perPage, page)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("create list request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+tokenData.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("GitHub API error listing repos: %s - %s", resp.Status, string(body))
		}

		var listResp listReposResponse
		if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("parse list response: %w", err)
		}
		resp.Body.Close()

		for _, repo := range listResp.Repositories {
			r := Repo{
				Name:     repo.Name,
				FullName: repo.FullName,
				Private:  repo.Private,
			}
			if repo.Description != nil {
				r.Description = *repo.Description
			}
			allRepos = append(allRepos, r)
		}

		if len(allRepos) >= listResp.TotalCount || len(listResp.Repositories) < perPage {
			break
		}
		page++

		// Safety limit
		if page > 10 {
			break
		}
	}

	slog.Info("Listed GitHub installation repositories", "count", len(allRepos))
	return allRepos, nil
}

// extractRepoName extracts the repository name from various formats.
// Supports: "owner/repo", "https://github.com/owner/repo", "https://github.com/owner/repo.git"
func extractRepoName(input string) string {
	input = strings.TrimSuffix(input, ".git")

	if after, ok := strings.CutPrefix(input, "https://github.com/"); ok {
		input = after
	} else if after, ok := strings.CutPrefix(input, "http://github.com/"); ok {
		input = after
	}

	// Now we should have "owner/repo" - return just the repo name
	parts := strings.Split(input, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}

	return ""
}

// NormalizeRepoURL converts various repo formats to a proper HTTPS URL.
// Supports: "owner/repo", "github.com/owner/repo", "https://github.com/owner/repo"
func NormalizeRepoURL(input string) string {
	if strings.HasPrefix(input, "https://") || strings.HasPrefix(input, "http://") {
		return input
	}

	if strings.HasPrefix(input, "github.com/") {
		return "https://" + input + ".git"
	}

	if strings.Contains(input, "/") {
		return "https://github.com/" + input + ".git"
	}

	return input
}
