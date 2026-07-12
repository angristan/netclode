package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	slogtrace "github.com/DataDog/dd-trace-go/contrib/log/slog/v2"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/profiler"

	"github.com/angristan/netclode/services/control-plane/internal/api"
	"github.com/angristan/netclode/services/control-plane/internal/config"
	"github.com/angristan/netclode/services/control-plane/internal/github"
	"github.com/angristan/netclode/services/control-plane/internal/k8s"
	"github.com/angristan/netclode/services/control-plane/internal/metrics"
	"github.com/angristan/netclode/services/control-plane/internal/session"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
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
	logger := slog.New(slogtrace.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Initialize DogStatsD metrics client
	if err := metrics.Init(); err != nil {
		slog.Warn("Failed to init metrics client", "error", err)
	}
	defer metrics.Close()

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration
	cfg := config.Load()
	slog.Info("Configuration loaded",
		"clientPort", cfg.ClientPort,
		"agentPort", cfg.AgentPort,
		"internalPort", cfg.InternalPort,
		"botPort", cfg.BotPort,
		"namespace", cfg.K8sNamespace,
		"agentImage", cfg.AgentImage,
		"redisURL", storage.ParseRedisURL(cfg.RedisURL),
		"idleTimeout", cfg.IdleTimeout,
	)

	// Initialize Redis storage
	store, err := storage.NewRedisStorage(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init redis: %w", err)
	}
	defer func() {
		slog.Info("Closing Redis connection")
		store.Close()
	}()

	// Initialize Kubernetes runtime with informers
	k8sRuntime, err := k8s.NewRuntime(cfg)
	if err != nil {
		return fmt.Errorf("init k8s: %w", err)
	}
	defer func() {
		slog.Info("Stopping K8s informer")
		k8sRuntime.Close()
	}()

	// Initialize GitHub client (nil if not configured)
	var githubClient *github.Client
	if cfg.HasGitHubApp() {
		var err error
		githubClient, err = github.NewClient(cfg.GitHubAppID, cfg.GitHubInstallationID, cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("Failed to create GitHub client", "error", err)
		} else {
			slog.Info("GitHub App client initialized")
		}
	}

	// Create session manager
	manager := session.NewManager(store, k8sRuntime, cfg, githubClient)
	defer func() {
		slog.Info("Closing session manager")
		manager.Close()
	}()

	// Initialize manager (load sessions, reconcile with K8s)
	if err := manager.Initialize(ctx); err != nil {
		return fmt.Errorf("init manager: %w", err)
	}

	// Create HTTP/Connect server
	server := api.NewServer(manager)

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigCh
		slog.Info("Shutdown signal received", "signal", sig.String())
		cancel()
	}()

	// Start the isolated client, agent, internal, and bot listeners.
	addresses := api.ListenAddresses{
		Client:   fmt.Sprintf(":%d", cfg.ClientPort),
		Agent:    fmt.Sprintf(":%d", cfg.AgentPort),
		Internal: fmt.Sprintf(":%d", cfg.InternalPort),
		Bot:      fmt.Sprintf(":%d", cfg.BotPort),
	}
	if err := server.ListenAndServe(ctx, addresses); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	slog.Info("Server stopped gracefully")
	return nil
}
