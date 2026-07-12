// Package config handles configuration loading for the secret proxy.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config holds the application configuration.
type Config struct {
	// ListenAddr is the address to listen on.
	ListenAddr string

	// ControlPlaneURL is the URL of the control-plane for auth validation.
	ControlPlaneURL string
	// ControlPlaneTokenPath contains the projected identity token for this workload.
	ControlPlaneTokenPath string

	// CAPath is the path to the CA certificate file.
	CAPath string

	// CAKeyPath is the path to the CA private key file.
	CAKeyPath string

	// SecretsPath is the path to the secrets JSON file.
	// Format: {"secretKey": "secretValue", ...}
	SecretsPath string

	// Verbose enables verbose logging.
	Verbose bool
}

// Load loads configuration from environment variables.
func Load() Config {
	return Config{
		ListenAddr:            getEnv("LISTEN_ADDR", ":8080"),
		ControlPlaneURL:       getEnv("CONTROL_PLANE_URL", "http://control-plane-internal.netclode.svc.cluster.local:3002"),
		ControlPlaneTokenPath: getEnv("CONTROL_PLANE_TOKEN_PATH", "/var/run/secrets/control-plane-auth/token"),
		CAPath:                getEnv("CA_CERT_PATH", "/etc/secret-proxy/ca.crt"),
		CAKeyPath:             getEnv("CA_KEY_PATH", "/etc/secret-proxy/ca.key"),
		SecretsPath:           getEnv("SECRETS_PATH", "/etc/secret-proxy/secrets.json"),
		Verbose:               os.Getenv("VERBOSE") == "true",
	}
}

// LoadSecrets reads secrets from a JSON file.
// Format: {"secretKey": "secretValue", ...}
// Example: {"anthropic": "sk-ant-...", "openai": "sk-...", "mistral": "..."}
func LoadSecrets(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secrets file: %w", err)
	}

	var secrets map[string]string
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("parse secrets JSON: %w", err)
	}

	return secrets, nil
}

func getEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
