package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for the github-bot service.
type Config struct {
	Port                  string
	ControlPlaneURL       string
	ControlPlaneTokenPath string
	RedisURL              string
	GitHubAppID           int64
	GitHubPrivateKey      []byte // PEM-encoded
	InstallationID        int64
	WebhookSecret         string
	SdkType               string
	Model                 string
	SessionTimeout        time.Duration
	MaxConcurrent         int
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Port:                  envOrDefault("PORT", "8080"),
		ControlPlaneTokenPath: envOrDefault("CONTROL_PLANE_TOKEN_PATH", "/var/run/secrets/control-plane-auth/token"),
		SdkType:               envOrDefault("SDK_TYPE", "claude"),
		Model:                 envOrDefault("MODEL", "claude-opus-4-6"),
	}

	// Required
	cfg.ControlPlaneURL = os.Getenv("CONTROL_PLANE_URL")
	if cfg.ControlPlaneURL == "" {
		return nil, fmt.Errorf("CONTROL_PLANE_URL is required")
	}

	cfg.RedisURL = os.Getenv("REDIS_URL")
	if cfg.RedisURL == "" {
		return nil, fmt.Errorf("REDIS_URL is required")
	}

	appID := os.Getenv("GITHUB_APP_ID")
	if appID == "" {
		return nil, fmt.Errorf("GITHUB_APP_ID is required")
	}
	var err error
	cfg.GitHubAppID, err = strconv.ParseInt(appID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_ID must be a number: %w", err)
	}

	pk := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if pk == "" {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY is required")
	}
	cfg.GitHubPrivateKey = []byte(pk)

	instID := os.Getenv("GITHUB_INSTALLATION_ID")
	if instID == "" {
		return nil, fmt.Errorf("GITHUB_INSTALLATION_ID is required")
	}
	cfg.InstallationID, err = strconv.ParseInt(instID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GITHUB_INSTALLATION_ID must be a number: %w", err)
	}

	cfg.WebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	if cfg.WebhookSecret == "" {
		return nil, fmt.Errorf("GITHUB_WEBHOOK_SECRET is required")
	}

	// Optional with defaults
	timeoutStr := envOrDefault("SESSION_TIMEOUT", "10m")
	cfg.SessionTimeout, err = time.ParseDuration(timeoutStr)
	if err != nil {
		return nil, fmt.Errorf("SESSION_TIMEOUT invalid duration: %w", err)
	}

	maxConcStr := envOrDefault("MAX_CONCURRENT_GITHUB_SESSIONS", "5")
	cfg.MaxConcurrent, err = strconv.Atoi(maxConcStr)
	if err != nil {
		return nil, fmt.Errorf("MAX_CONCURRENT_GITHUB_SESSIONS must be a number: %w", err)
	}

	return cfg, nil
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
