package config

import (
	"encoding/base64"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port               int
	AnthropicAPIKey    string
	OpenAIAPIKey       string // OpenAI API key (for Codex SDK)
	MistralAPIKey      string // Mistral API key (for OpenCode SDK)
	GitHubCopilotToken string // GitHub PAT with Copilot scope (for Copilot SDK only)

	// GitHub Copilot OAuth tokens (for OpenCode provider; obtained via device-flow)
	GitHubCopilotOAuthAccessToken  string
	GitHubCopilotOAuthRefreshToken string
	GitHubCopilotOAuthTokenExpires string // Unix timestamp string, "0" = no expiry
	K8sNamespace                   string
	AgentImage                     string
	SandboxTemplate                string
	DefaultCPUs                    int
	DefaultMemoryMB                int
	RedisURL                       string
	UseWarmPool                    bool
	MaxActiveSessions              int
	IdleTimeout                    time.Duration // Auto-pause sessions after this duration of inactivity (0 = disabled)

	// Host resource limits for per-session resource validation
	HostCPUs     int // Total CPUs on the host (for 50% limit validation)
	HostMemoryMB int // Total memory in MB on the host (for 50% limit validation)

	// Overcommit ratios for K8s scheduling (requests = actual / ratio)
	// Higher ratio = more overcommit. 1 = no overcommit, 4 = 4x overcommit
	CPUOvercommitRatio    int
	MemoryOvercommitRatio int

	// Codex OAuth tokens (for ChatGPT auth mode)
	CodexAccessToken  string
	CodexIdToken      string
	CodexRefreshToken string

	// GitHub App integration (for repo-scoped tokens)
	GitHubAppID          int64
	GitHubAppPrivateKey  string // PEM-encoded private key
	GitHubInstallationID int64

	// Ollama local inference (optional)
	OllamaURL string // URL for Ollama API (e.g., "http://ollama.netclode.svc.cluster.local:11434")

	// OpenCode Zen (optional)
	OpenCodeAPIKey string // OpenCode Zen API key (if empty, only free models available)

	// Z.AI (optional)
	ZaiAPIKey string // Z.AI API key (for GLM-4.7 models via Anthropic-compatible endpoint)
}

func Load() *Config {
	return &Config{
		Port:               getEnvInt("PORT", 3000),
		AnthropicAPIKey:    getEnv("ANTHROPIC_API_KEY", ""),
		OpenAIAPIKey:       getEnv("OPENAI_API_KEY", ""),
		MistralAPIKey:      getEnv("MISTRAL_API_KEY", ""),
		GitHubCopilotToken: getEnv("GITHUB_COPILOT_TOKEN", ""),

		GitHubCopilotOAuthAccessToken:  getEnv("GITHUB_COPILOT_OAUTH_ACCESS_TOKEN", ""),
		GitHubCopilotOAuthRefreshToken: getEnv("GITHUB_COPILOT_OAUTH_REFRESH_TOKEN", ""),
		GitHubCopilotOAuthTokenExpires: getEnv("GITHUB_COPILOT_OAUTH_TOKEN_EXPIRES", "0"),
		K8sNamespace:                   getEnv("K8S_NAMESPACE", "netclode"),
		AgentImage:                     getEnv("AGENT_IMAGE", "ghcr.io/angristan/netclode-agent:latest"),
		SandboxTemplate:                getEnv("SANDBOX_TEMPLATE", "netclode-agent"),
		DefaultCPUs:                    getEnvInt("DEFAULT_CPUS", 4),
		DefaultMemoryMB:                getEnvInt("DEFAULT_MEMORY_MB", 4096),
		RedisURL:                       getEnv("REDIS_URL", "redis://redis-sessions.netclode.svc.cluster.local:6379"),
		UseWarmPool:                    getEnvBool("WARM_POOL_ENABLED", true),
		MaxActiveSessions:              getEnvInt("MAX_ACTIVE_SESSIONS", 5),
		IdleTimeout:                    time.Duration(getEnvInt("IDLE_TIMEOUT_MINUTES", 0)) * time.Minute, // 0 = disabled
		HostCPUs:                       getEnvInt("HOST_CPUS", 16),                                        // Default assumes 16-core host
		HostMemoryMB:                   getEnvInt("HOST_MEMORY_MB", 32768),                                // Default assumes 32GB host
		CPUOvercommitRatio:             getEnvInt("CPU_OVERCOMMIT_RATIO", 1),                              // 1 = no overcommit
		MemoryOvercommitRatio:          getEnvInt("MEMORY_OVERCOMMIT_RATIO", 1),                           // 1 = no overcommit

		// Codex OAuth tokens
		CodexAccessToken:  getEnv("CODEX_ACCESS_TOKEN", ""),
		CodexIdToken:      getEnv("CODEX_ID_TOKEN", ""),
		CodexRefreshToken: getEnv("CODEX_REFRESH_TOKEN", ""),

		// GitHub App integration
		GitHubAppID:          getEnvInt64("GITHUB_APP_ID", 0),
		GitHubAppPrivateKey:  getGitHubPrivateKey(),
		GitHubInstallationID: getEnvInt64("GITHUB_INSTALLATION_ID", 0),

		// Ollama local inference
		OllamaURL: getEnv("OLLAMA_URL", ""),

		// OpenCode Zen
		OpenCodeAPIKey: getEnv("OPENCODE_API_KEY", ""),

		// Z.AI
		ZaiAPIKey: getEnv("ZAI_API_KEY", ""),
	}
}

// getGitHubPrivateKey returns the GitHub App private key.
// It first checks GITHUB_APP_PRIVATE_KEY_B64 (base64-encoded),
// then falls back to GITHUB_APP_PRIVATE_KEY (raw PEM).
func getGitHubPrivateKey() string {
	// Try base64-encoded version first (for .env files where multiline is tricky)
	if b64 := os.Getenv("GITHUB_APP_PRIVATE_KEY_B64"); b64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err == nil {
			return string(decoded)
		}
	}
	// Fall back to raw PEM
	return os.Getenv("GITHUB_APP_PRIVATE_KEY")
}

// HasGitHubApp returns true if GitHub App is configured.
func (c *Config) HasGitHubApp() bool {
	return c.GitHubAppID > 0 && c.GitHubAppPrivateKey != "" && c.GitHubInstallationID > 0
}

// MaxSessionCPUs returns the maximum allowed vCPUs per session (50% of host).
func (c *Config) MaxSessionCPUs() int {
	return c.HostCPUs / 2
}

// MaxSessionMemoryMB returns the maximum allowed memory per session in MB (25% of host, rounded up to power of 2 GB).
func (c *Config) MaxSessionMemoryMB() int {
	raw := c.HostMemoryMB / 4
	// Round up to nearest power of 2 in GB (1024, 2048, 4096, 8192, 16384, ...)
	gbOptions := []int{1024, 2048, 4096, 8192, 16384, 32768, 65536}
	for _, gb := range gbOptions {
		if gb >= raw {
			return gb
		}
	}
	return raw // Fallback if larger than 64GB
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.ParseInt(value, 10, 64); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1"
	}
	return defaultValue
}
