package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/angristan/netclode/services/secret-proxy/internal/certs"
	"github.com/angristan/netclode/services/secret-proxy/internal/config"
	"github.com/angristan/netclode/services/secret-proxy/internal/observability"
	"github.com/angristan/netclode/services/secret-proxy/internal/proxy"
)

func main() {
	// Configure structured logging with trace correlation
	logLevel := slog.LevelInfo
	if os.Getenv("VERBOSE") == "true" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(observability.NewLogHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	// Initialize OpenTelemetry traces and metrics
	shutdown, err := observability.Setup(context.Background(), "secret-proxy")
	if err != nil {
		slog.Warn("Failed to set up OpenTelemetry", "error", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			slog.Warn("OpenTelemetry shutdown failed", "error", err)
		}
	}()

	if err := run(logger); err != nil {
		logger.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// Load configuration
	cfg := config.Load()
	logger.Info("Configuration loaded",
		"listenAddr", cfg.ListenAddr,
		"controlPlaneURL", cfg.ControlPlaneURL,
		"caPath", cfg.CAPath,
		"secretsPath", cfg.SecretsPath,
		"verbose", cfg.Verbose,
	)

	// Load secrets from file (not env var - prevents /proc/*/environ exposure)
	secrets, err := config.LoadSecrets(cfg.SecretsPath)
	if err != nil {
		return err
	}
	logger.Info("Secrets loaded", "count", len(secrets))

	// Load or generate CA certificate
	ca, err := certs.LoadOrGenerateCA(cfg.CAPath, cfg.CAKeyPath)
	if err != nil {
		return err
	}
	logger.Info("CA certificate loaded")

	// Create and start proxy
	p := proxy.New(proxy.Config{
		ListenAddr:            cfg.ListenAddr,
		ControlPlaneURL:       cfg.ControlPlaneURL,
		ControlPlaneTokenPath: cfg.ControlPlaneTokenPath,
		Secrets:               secrets,
		CA:                    ca,
		Verbose:               cfg.Verbose,
	}, logger)

	return p.ListenAndServe()
}
