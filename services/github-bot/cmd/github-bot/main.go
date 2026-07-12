package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	slogtrace "github.com/DataDog/dd-trace-go/contrib/log/slog/v2"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/profiler"

	"github.com/angristan/netclode/services/github-bot/internal/config"
	"github.com/angristan/netclode/services/github-bot/internal/controlplane"
	"github.com/angristan/netclode/services/github-bot/internal/ghclient"
	"github.com/angristan/netclode/services/github-bot/internal/metrics"
	"github.com/angristan/netclode/services/github-bot/internal/server"
	"github.com/angristan/netclode/services/github-bot/internal/store"
	"github.com/angristan/netclode/services/github-bot/internal/webhook"
	"github.com/angristan/netclode/services/github-bot/internal/workflow"
)

func main() {
	// Start Datadog tracer (reads DD_SERVICE, DD_ENV, DD_VERSION from env)
	tracer.Start(tracer.WithRuntimeMetrics())
	defer tracer.Stop()

	// Start continuous profiler
	if err := profiler.Start(
		profiler.WithProfileTypes(
			profiler.CPUProfile,
			profiler.HeapProfile,
			profiler.GoroutineProfile,
		),
	); err != nil {
		slog.Warn("Failed to start profiler", "error", err)
	}
	defer profiler.Stop()

	// Configure structured logging with Datadog trace correlation
	slog.SetDefault(slog.New(slogtrace.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Initialize DogStatsD metrics client
	if err := metrics.Init(); err != nil {
		slog.Warn("Failed to init metrics client", "error", err)
	}
	defer metrics.Close()

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
