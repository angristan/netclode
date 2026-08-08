package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/angristan/netclode/services/github-bot/internal/config"
	"github.com/angristan/netclode/services/github-bot/internal/controlplane"
	"github.com/angristan/netclode/services/github-bot/internal/ghclient"
	"github.com/angristan/netclode/services/github-bot/internal/observability"
	"github.com/angristan/netclode/services/github-bot/internal/server"
	"github.com/angristan/netclode/services/github-bot/internal/store"
	"github.com/angristan/netclode/services/github-bot/internal/webhook"
	"github.com/angristan/netclode/services/github-bot/internal/workflow"
)

func main() {
	// Configure structured logging with trace correlation
	slog.SetDefault(slog.New(observability.NewLogHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Initialize OpenTelemetry traces and metrics
	shutdown, err := observability.Setup(context.Background(), "github-bot")
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

	cfg, err := config.Load()
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// Initialize GitHub client
	gh, err := ghclient.New(cfg.GitHubAppID, cfg.InstallationID, cfg.GitHubPrivateKey)
	if err != nil {
		slog.Error("Failed to create GitHub client", "error", err)
		os.Exit(1)
	}

	// Initialize control-plane client
	cp := controlplane.New(cfg.ControlPlaneURL, cfg.ControlPlaneTokenPath)

	// Initialize Redis store (dedup + in-flight tracking)
	st, err := store.New(cfg.RedisURL)
	if err != nil {
		slog.Error("Failed to create store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Build shared deps
	deps := &workflow.Deps{
		GH:      gh,
		CP:      cp,
		Store:   st,
		SdkType: controlplane.ParseSdkType(cfg.SdkType),
		Model:   cfg.Model,
	}

	// Create webhook handler
	handler := webhook.NewHandler(cfg.WebhookSecret, st, deps, cfg.MaxConcurrent, cfg.SessionTimeout)

	// Create HTTP server
	mux := server.New(handler)
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server
	go func() {
		slog.Info("Starting github-bot", "port", cfg.Port, "controlPlane", cfg.ControlPlaneURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server error", "error", err)
			os.Exit(1)
		}
	}()

	// Recover in-flight sessions in the background so the health endpoint
	// is reachable immediately and liveness probes don't kill the pod.
	go workflow.RecoverInFlight(context.Background(), deps)

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	slog.Info("Shutting down...")

	// Stop accepting new requests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("Server shutdown error", "error", err)
	}

	// Wait for in-flight workflows
	slog.Info("Waiting for in-flight workflows...")
	handler.Wait()

	slog.Info("Shutdown complete")
}
