package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/config"
	"github.com/angristan/netclode/services/control-plane/internal/github"
	"github.com/angristan/netclode/services/control-plane/internal/k8s"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	sandboxReadyTimeout = 120 * time.Second
)

// SessionUpdateCallback is called when a session is updated (e.g., auto-paused).
// This allows the API layer to broadcast updates to connected clients.
type SessionUpdateCallback func(session *pb.Session)

// AgentSessionConfig contains typed configuration for an agent session.
type AgentSessionConfig struct {
	SessionID          string
	OpenAIAPIKey       string // For Codex API mode
	MistralAPIKey      string // For OpenCode Mistral models
	GitHubToken        string // For git credentials (from GitHub App)
	GitHubCopilotToken string // For Copilot SDK
	Repos              []string
	RepoAccess         *pb.RepoAccess
	SdkType            *pb.SdkType
	Model              string
	CopilotBackend     *pb.CopilotBackend
	CodexAccessToken   string // For Codex OAuth mode
	CodexIdToken       string // For Codex OAuth mode
	ReasoningEffort    string // For Codex reasoning effort (low, medium, high)
	OllamaURL          string // For local Ollama inference
	OpenCodeAPIKey     string // For OpenCode Zen models
	ZaiAPIKey          string // For Z.AI GLM-4.7 models
}

// AgentConnection represents a connected agent that can receive commands.
type AgentConnection interface {
	ExecutePrompt(text string) error
	Interrupt() error
	GenerateTitle(requestID, prompt string) error
	GetGitStatus(requestID string) error
	GetGitDiff(requestID string, file *string) error
	SendTerminalInput(data string) error
	ResizeTerminal(cols, rows int) error
	UpdateGitCredentials(token string, repoAccess pb.RepoAccess) error
	UpdateCodexAuth(accessToken, idToken string, expiresAt *timestamppb.Timestamp) error
}

// WarmAgentConnection extends AgentConnection with session assignment capability.
type WarmAgentConnection interface {
	AgentConnection
	AssignSession(sessionID string, config *AgentSessionConfig) error
}

// Manager handles session lifecycle and agent communication.
type Manager struct {
	storage      storage.Storage
	k8s          k8s.Runtime
	config       *config.Config
	githubClient *github.Client

	sessions           map[string]*SessionState
	agents             map[string]AgentConnection     // sessionID -> agent connection
	warmAgents         map[string]WarmAgentConnection // podName -> warm agent connection (waiting for session)
	pendingAssignments map[string]string              // podName -> sessionID (for when agent registers late)
	mu                 sync.RWMutex

	// onSessionUpdated is called when a session is updated internally (e.g., auto-pause).
	onSessionUpdated SessionUpdateCallback

	// Backend-managed Codex OAuth device flow state.
	codexAuthMu          sync.Mutex
	codexAuthPending     *codexAuthPendingState
	codexAuthLastError   string
	codexAuthLastErrorAt time.Time
}

// NewManager creates a new session manager.
// githubClient can be nil if GitHub App is not configured.
func NewManager(store storage.Storage, k8sRuntime k8s.Runtime, cfg *config.Config, githubClient *github.Client) *Manager {
	return &Manager{
		storage:            store,
		k8s:                k8sRuntime,
		config:             cfg,
		githubClient:       githubClient,
		sessions:           make(map[string]*SessionState),
		agents:             make(map[string]AgentConnection),
		warmAgents:         make(map[string]WarmAgentConnection),
		pendingAssignments: make(map[string]string),
	}
}

// createRepoToken generates a GitHub token scoped to the specific repos with the given access level.
// Returns empty string if GitHub App is not configured.
func (m *Manager) createRepoToken(ctx context.Context, repos []string, access *pb.RepoAccess) string {
	if m.githubClient == nil {
		slog.Warn("GitHub App not configured, cannot create repo-scoped token")
		return ""
	}
	if len(repos) == 0 {
		return ""
	}

	// Determine access level for GitHub API (default to read)
	ghAccess := github.RepoAccessRead
	if access != nil && *access == pb.RepoAccess_REPO_ACCESS_WRITE {
		ghAccess = github.RepoAccessWrite
	}

	token, err := m.githubClient.CreateRepoToken(ctx, repos, ghAccess)
	if err != nil {
		slog.Error("Failed to create repo-scoped token", "repos", repos, "access", access, "error", err)
		return ""
	}

	return token.Token
}

func normalizeRepos(repos []string) []string {
	if len(repos) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(repos))
	seen := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		norm := github.NormalizeRepoURL(repo)
		if _, exists := seen[norm]; exists {
			continue
		}
		seen[norm] = struct{}{}
		normalized = append(normalized, norm)
	}

	return normalized
}

// SetOnSessionUpdated sets a callback that is called when a session is updated internally.
// This is used for broadcasting auto-pause events to connected clients.
func (m *Manager) SetOnSessionUpdated(cb SessionUpdateCallback) {
	m.onSessionUpdated = cb
}

// Initialize loads sessions from storage and reconciles with K8s state.
func (m *Manager) Initialize(ctx context.Context) error {
	slog.Info("Initializing session manager")

	// Load all sessions from storage
	sessions, err := m.storage.GetAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}

	slog.Info("Loaded sessions from storage", "count", len(sessions))

	// Load into memory with channel-based state
	for _, s := range sessions {
		state := NewSessionState(s)
		// Load persisted restore snapshot ID if any
		if restoreID, err := m.storage.GetRestoreSnapshotID(ctx, s.Id); err == nil && restoreID != "" {
			state.RestoreSnapshotID = restoreID
			slog.Info("Loaded restore snapshot ID", "sessionID", s.Id, "snapshotID", restoreID)
		}
		m.sessions[s.Id] = state
	}

	// Reconcile with K8s
	sandboxes, err := m.k8s.ListSandboxes(ctx)
	if err != nil {
		slog.Warn("Failed to list sandboxes, skipping reconciliation", "error", err)
		return nil
	}

	sandboxMap := make(map[string]k8s.SandboxInfo)
	for _, sb := range sandboxes {
		sandboxMap[sb.SessionID] = sb
	}

	// Update session statuses based on K8s state
	for id, state := range m.sessions {
		sb, exists := sandboxMap[id]
		if !exists {
			// No sandbox exists - clean up Tailscale service and mark as paused
			if state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING || state.Session.Status == pb.SessionStatus_SESSION_STATUS_CREATING {
				slog.Info("Session has no sandbox, marking as paused", "sessionID", id)
				state.Session.Status = pb.SessionStatus_SESSION_STATUS_PAUSED
				_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_PAUSED)
			}
			// Delete orphaned Tailscale service (if any)
			if err := m.k8s.DeleteSandboxService(ctx, id); err != nil {
				slog.Warn("Failed to delete orphaned sandbox service", "sessionID", id, "error", err)
			}
		} else if sb.Ready {
			state.ServiceFQDN = sb.ServiceFQDN
			// If session was running, mark as interrupted (agent was processing when we lost connection)
			// If session was creating, mark as ready (sandbox is ready to accept prompts)
			if state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING {
				slog.Info("Session was running on restart, marking as interrupted", "sessionID", id)
				state.Session.Status = pb.SessionStatus_SESSION_STATUS_INTERRUPTED
				_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_INTERRUPTED)
			} else if state.Session.Status == pb.SessionStatus_SESSION_STATUS_CREATING {
				state.Session.Status = pb.SessionStatus_SESSION_STATUS_READY
				_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_READY)
			}
		} else {
			// Sandbox exists but is not ready - mark as paused since we can't communicate with it
			if state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING || state.Session.Status == pb.SessionStatus_SESSION_STATUS_READY {
				slog.Info("Session sandbox exists but is not ready, marking as paused", "sessionID", id)
				state.Session.Status = pb.SessionStatus_SESSION_STATUS_PAUSED
				state.ServiceFQDN = "" // Clear any stale FQDN
				_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_PAUSED)
			}
		}
	}

	slog.Info("Session manager initialized", "sessions", len(m.sessions), "sandboxes", len(sandboxes))

	// Clean up orphaned Tailscale services (services without sessions in storage)
	if tsServices, err := m.k8s.ListTailscaleServices(ctx); err != nil {
		slog.Warn("Failed to list Tailscale services, skipping orphan cleanup", "error", err)
	} else {
		for _, sessionID := range tsServices {
			if _, exists := m.sessions[sessionID]; !exists {
				slog.Info("Deleting orphaned Tailscale service", "sessionID", sessionID)
				if err := m.k8s.DeleteSandboxService(ctx, sessionID); err != nil {
					slog.Warn("Failed to delete orphaned Tailscale service", "sessionID", sessionID, "error", err)
				}
			}
		}
	}

	// Enforce active session limit on startup
	m.enforceActiveLimit(ctx)

	return nil
}

// Close closes all session states.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, state := range m.sessions {
		state.Close()
	}
}

// generateID creates a new session ID.
func generateID() string {
	return uuid.NewString()[:12]
}

// Create creates a new session.
// If resources is provided, validates against host limits (max 50%) and bypasses warm pool.
func (m *Manager) Create(ctx context.Context, name string, repos []string, repoAccess *pb.RepoAccess, sdkType *pb.SdkType, model *string, copilotBackend *pb.CopilotBackend, tailnetAccess *bool, resources *pb.SandboxResources) (*pb.Session, error) {
	// Validate custom resources if provided
	if resources != nil {
		if err := m.validateResources(resources); err != nil {
			return nil, err
		}
	}

	// Codex OAuth sessions require backend-managed global OAuth credentials.
	if sdkType != nil && *sdkType == pb.SdkType_SDK_TYPE_CODEX && model != nil {
		authMode, _ := parseCodexAuthModeAndEffort(*model)
		if authMode == "oauth" {
			oauthData, err := m.getCodexOAuth(ctx)
			if err != nil {
				return nil, fmt.Errorf("get codex oauth data: %w", err)
			}
			if oauthData == nil || oauthData.AccessToken == "" || oauthData.IdToken == "" || oauthData.RefreshToken == "" {
				return nil, fmt.Errorf("codex oauth is not configured")
			}
		}
	}

	// Ensure we have a slot for a new active session
	m.ensureActiveSlot(ctx, "")

	id := generateID()
	now := timestamppb.Now()

	if name == "" {
		name = "Session " + id[:6]
	}

	session := &pb.Session{
		Id:             id,
		Name:           name,
		Status:         pb.SessionStatus_SESSION_STATUS_CREATING,
		Repos:          repos,
		RepoAccess:     repoAccess,
		CreatedAt:      now,
		LastActiveAt:   now,
		SdkType:        sdkType,
		Model:          model,
		CopilotBackend: copilotBackend,
	}

	// Save to storage
	if err := m.storage.SaveSession(ctx, session); err != nil {
		return nil, fmt.Errorf("save session: %w", err)
	}

	// Add to memory with channel-based state
	state := NewSessionState(session)
	m.mu.Lock()
	m.sessions[id] = state
	m.mu.Unlock()

	// Determine tailnet setting (default: disabled)
	tailnetEnabled := false
	if tailnetAccess != nil {
		tailnetEnabled = *tailnetAccess
	}

	// Start sandbox creation in background
	go m.createSandbox(context.Background(), id, repos, repoAccess, tailnetEnabled, resources)

	return session, nil
}

// validateResources checks that requested resources don't exceed host limits (50% of host).
func (m *Manager) validateResources(resources *pb.SandboxResources) error {
	maxCPUs := m.config.MaxSessionCPUs()
	maxMemoryMB := m.config.MaxSessionMemoryMB()

	if resources.Vcpus < 1 {
		return fmt.Errorf("vcpus must be at least 1")
	}
	if resources.Vcpus > int32(maxCPUs) {
		return fmt.Errorf("vcpus %d exceeds maximum allowed %d (50%% of host)", resources.Vcpus, maxCPUs)
	}

	if resources.MemoryMb < 512 {
		return fmt.Errorf("memory_mb must be at least 512")
	}
	if resources.MemoryMb > int32(maxMemoryMB) {
		return fmt.Errorf("memory_mb %d exceeds maximum allowed %d (50%% of host)", resources.MemoryMb, maxMemoryMB)
	}

	return nil
}

func (m *Manager) createSandbox(ctx context.Context, sessionID string, repos []string, repoAccess *pb.RepoAccess, tailnetEnabled bool, resources *pb.SandboxResources) {
	// If custom resources are requested, bypass warm pool and create directly
	if resources != nil {
		slog.Info("Creating sandbox with custom resources (bypassing warm pool)", "sessionID", sessionID, "vcpus", resources.Vcpus, "memoryMB", resources.MemoryMb)
		m.createSandboxDirect(ctx, sessionID, repos, repoAccess, tailnetEnabled, resources)
		return
	}

	if m.config.UseWarmPool {
		m.createSandboxViaClaim(ctx, sessionID, repos, repoAccess, tailnetEnabled)
	} else {
		m.createSandboxDirect(ctx, sessionID, repos, repoAccess, tailnetEnabled, nil)
	}
}

// createSandboxDirectOptions holds optional parameters for createSandboxDirect.
type createSandboxDirectOptions struct {
	restoreSnapshotID string
	resources         *pb.SandboxResources
}

// createSandboxDirect creates a sandbox directly (legacy mode or custom resources mode).
// If opts.restoreSnapshotID is provided, the PVC is restored from that snapshot BEFORE creating the sandbox.
// If opts.resources is provided, the sandbox is created with custom VM resources.
func (m *Manager) createSandboxDirect(ctx context.Context, sessionID string, repos []string, repoAccess *pb.RepoAccess, tailnetEnabled bool, resources *pb.SandboxResources, restoreSnapshotID ...string) {
	var snapID string
	if len(restoreSnapshotID) > 0 {
		snapID = restoreSnapshotID[0]
	}
	env := map[string]string{
		"SESSION_ID":        sessionID,
		"ANTHROPIC_API_KEY": m.config.AnthropicAPIKey,
	}

	// If restoring from snapshot, create the PVC first and wait for restore to complete
	// BEFORE creating the sandbox. This ensures the restore job finishes before the pod mounts.
	if snapID != "" {
		slog.Info("Creating PVC from snapshot before sandbox", "sessionID", sessionID, "snapshotID", snapID)

		// Create standalone PVC from snapshot
		pvcName, err := m.k8s.CreatePVCFromSnapshot(ctx, sessionID, snapID)
		if err != nil {
			slog.Error("Failed to create PVC from snapshot", "sessionID", sessionID, "error", err)
			m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
			m.emitSessionError(ctx, sessionID, fmt.Sprintf("failed to create PVC from snapshot: %v", err))
			return
		}

		// Wait for JuiceFS restore job to complete BEFORE creating sandbox.
		// We must wait because the pod will fail with I/O errors if it tries to access
		// the filesystem before the restore completes.
		slog.Info("Waiting for snapshot restore job", "sessionID", sessionID, "snapshotID", snapID)
		if err := m.k8s.WaitForRestoreJob(ctx, sessionID, snapID, 5*time.Minute); err != nil {
			slog.Error("Snapshot restore job failed", "sessionID", sessionID, "error", err)
			// Cleanup: delete the PVC we created
			if delErr := m.k8s.DeletePVC(ctx, sessionID); delErr != nil {
				slog.Error("Failed to cleanup PVC after restore failure", "sessionID", sessionID, "error", delErr)
			}
			m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
			m.emitSessionError(ctx, sessionID, fmt.Sprintf("snapshot restore failed: %v", err))
			return
		}
		slog.Info("Snapshot restore completed, creating sandbox with existing PVC", "sessionID", sessionID, "pvc", pvcName)

		// Ensure the session anchor ConfigMap exists and owns the new PVC.
		// This prevents the PVC from being garbage-collected if the sandbox fails or is paused.
		if err := m.k8s.EnsureSessionAnchor(ctx, sessionID); err != nil {
			slog.Warn("Failed to create session anchor", "sessionID", sessionID, "error", err)
		} else if err := m.k8s.AddSessionAnchorToPVC(ctx, sessionID, pvcName); err != nil {
			slog.Warn("Failed to add session anchor to PVC", "sessionID", sessionID, "pvc", pvcName, "error", err)
		}

		// Delete the old orphaned PVC in background (non-blocking)
		// Only if it's different from the new PVC (avoids deleting the newly created one)
		go func(sessionID, newPVCName string) {
			bgCtx := context.Background()
			if oldPVCName, err := m.storage.GetOldPVCName(bgCtx, sessionID); err == nil && oldPVCName != "" {
				if oldPVCName == newPVCName {
					slog.Info("Skipping old PVC deletion (same as new PVC)", "sessionID", sessionID, "pvc", oldPVCName)
				} else {
					if err := m.k8s.DeletePVCByName(bgCtx, oldPVCName); err != nil {
						slog.Warn("Failed to delete old PVC after restore", "sessionID", sessionID, "pvc", oldPVCName, "error", err)
					} else {
						slog.Info("Deleted old PVC after restore", "sessionID", sessionID, "pvc", oldPVCName)
					}
				}
				_ = m.storage.ClearOldPVCName(bgCtx, sessionID)
			}
		}(sessionID, pvcName)

		// Pass the existing PVC name so sandbox uses it instead of creating a new one
		env[k8s.ExistingPVCEnvKey] = pvcName
	} else {
		// Not restoring from snapshot - check if we have an existing PVC (resume after pause)
		if existingPVC, err := m.storage.GetPVCName(ctx, sessionID); err == nil && existingPVC != "" {
			slog.Info("Resuming with existing PVC", "sessionID", sessionID, "pvc", existingPVC)
			env[k8s.ExistingPVCEnvKey] = existingPVC
		}
	}

	// Pass GitHub Copilot token if configured
	if m.config.GitHubCopilotToken != "" {
		env["GITHUB_COPILOT_TOKEN"] = m.config.GitHubCopilotToken
	}

	// Setup GitHub repo access if configured
	normalizedRepos := normalizeRepos(repos)
	if len(normalizedRepos) > 0 {
		if reposJSON, err := json.Marshal(normalizedRepos); err == nil {
			env["GIT_REPOS"] = string(reposJSON)
		} else {
			slog.Warn("Failed to encode repo list for sandbox env", "sessionID", sessionID, "error", err)
		}

		// Generate repo-scoped token via GitHub App for git credentials
		if token := m.createRepoToken(ctx, normalizedRepos, repoAccess); token != "" {
			env["GITHUB_TOKEN"] = token
		}
	}

	// Convert proto resources to k8s config
	var k8sResources *k8s.SandboxResourceConfig
	if resources != nil {
		k8sResources = &k8s.SandboxResourceConfig{
			VCPUs:    resources.Vcpus,
			MemoryMB: resources.MemoryMb,
		}
	}

	// Create sandbox
	if err := m.k8s.CreateSandbox(ctx, sessionID, env, k8sResources); err != nil {
		slog.Error("Failed to create sandbox", "sessionID", sessionID, "error", err)
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, err.Error())
		return
	}

	// Wait for ready using informer-based watching
	fqdn, err := m.k8s.WaitForReady(ctx, sessionID, sandboxReadyTimeout)
	if err != nil {
		slog.Error("Sandbox failed to become ready", "sessionID", sessionID, "error", err)
		// Cleanup: delete the sandbox to avoid resource leak
		if delErr := m.k8s.DeleteSandbox(ctx, sessionID); delErr != nil {
			slog.Error("Failed to cleanup sandbox after timeout", "sessionID", sessionID, "error", delErr)
		} else {
			slog.Info("Cleaned up sandbox after timeout", "sessionID", sessionID)
		}
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, err.Error())
		return
	}

	// Apply network policies
	// Always enable internet access (required for LLM API calls)
	if err := m.k8s.ConfigureNetwork(ctx, sessionID, true); err != nil {
		slog.Error("Failed to apply internet access policy", "sessionID", sessionID, "error", err)
		// Non-fatal: continue with sandbox creation, but log the error
	} else {
		slog.Info("Applied internet access policy", "sessionID", sessionID)
	}
	if tailnetEnabled {
		if err := m.k8s.ConfigureTailnetAccess(ctx, sessionID, true); err != nil {
			slog.Error("Failed to apply tailnet access policy", "sessionID", sessionID, "error", err)
			// Non-fatal: continue with sandbox creation, but log the error
		} else {
			slog.Info("Applied tailnet access policy", "sessionID", sessionID)
		}
	}

	// Create Tailscale-exposed service for preview URLs
	if err := m.k8s.CreateSandboxService(ctx, sessionID); err != nil {
		slog.Warn("Failed to create sandbox service", "sessionID", sessionID, "error", err)
		// Non-fatal: sandbox still works, just no preview URLs
	}

	// Restore any previously exposed ports (for resume after pause)
	m.restoreExposedPorts(ctx, sessionID)

	// Store PVC name for resume after pause
	if pvcName, err := m.k8s.GetPVCName(ctx, sessionID); err == nil {
		if err := m.storage.SetPVCName(ctx, sessionID, pvcName); err != nil {
			slog.Warn("Failed to store PVC name", "sessionID", sessionID, "pvc", pvcName, "error", err)
		} else {
			slog.Info("Stored PVC name", "sessionID", sessionID, "pvc", pvcName)
		}

		// Ensure the session anchor ConfigMap exists and owns the PVC.
		// This prevents the PVC from being garbage-collected when the Sandbox is deleted (during pause).
		if err := m.k8s.EnsureSessionAnchor(ctx, sessionID); err != nil {
			slog.Warn("Failed to create session anchor", "sessionID", sessionID, "error", err)
		} else if err := m.k8s.AddSessionAnchorToPVC(ctx, sessionID, pvcName); err != nil {
			slog.Warn("Failed to add session anchor to PVC", "sessionID", sessionID, "pvc", pvcName, "error", err)
		}
	}

	// Update state and check for pending prompt
	var pendingPrompt string
	m.mu.Lock()
	if state, ok := m.sessions[sessionID]; ok {
		state.ServiceFQDN = fqdn
		pendingPrompt = state.PendingPrompt
		state.PendingPrompt = "" // Clear it
		// Only set RUNNING if there's a pending prompt, otherwise READY
		if pendingPrompt != "" {
			state.Session.Status = pb.SessionStatus_SESSION_STATUS_RUNNING
		} else {
			state.Session.Status = pb.SessionStatus_SESSION_STATUS_READY
		}
	}
	m.mu.Unlock()

	newStatus := pb.SessionStatus_SESSION_STATUS_READY
	if pendingPrompt != "" {
		newStatus = pb.SessionStatus_SESSION_STATUS_RUNNING
	}
	if err := m.storage.UpdateSessionStatus(ctx, sessionID, newStatus); err != nil {
		slog.Error("Failed to update session status", "sessionID", sessionID, "error", err)
	}

	// Emit update
	if session := m.getSession(sessionID); session != nil {
		m.emitSessionUpdated(ctx, session)
	}

	// Process pending prompt if any
	if pendingPrompt != "" {
		slog.Info("Processing pending prompt", "sessionID", sessionID)
		if err := m.SendPrompt(ctx, sessionID, pendingPrompt); err != nil {
			slog.Error("Failed to send pending prompt", "sessionID", sessionID, "error", err)
		}
	}

	slog.Info("Session sandbox ready", "sessionID", sessionID, "fqdn", fqdn, "status", newStatus)
}

// createSandboxViaClaim uses SandboxClaim for warm pool allocation
func (m *Manager) createSandboxViaClaim(ctx context.Context, sessionID string, repos []string, repoAccess *pb.RepoAccess, tailnetEnabled bool) {
	// Always use the same template to leverage the warm pool
	// Network restrictions are applied via ConfigureNetwork() after claiming
	templateName := m.config.SandboxTemplate

	// Create SandboxClaim to request from warm pool
	if err := m.k8s.CreateSandboxClaim(ctx, sessionID, templateName); err != nil {
		slog.Error("Failed to create sandbox claim", "sessionID", sessionID, "error", err)
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, err.Error())
		return
	}

	// Wait for claim to be bound
	sandboxName, err := m.k8s.WaitForClaimBound(ctx, sessionID, sandboxReadyTimeout)
	if err != nil {
		slog.Error("Claim failed to bind", "sessionID", sessionID, "error", err)
		// Cleanup: delete the SandboxClaim
		if delErr := m.k8s.DeleteSandboxClaim(ctx, sessionID); delErr != nil {
			slog.Error("Failed to cleanup sandbox claim after timeout", "sessionID", sessionID, "error", delErr)
		} else {
			slog.Info("Cleaned up sandbox claim after timeout", "sessionID", sessionID)
		}
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, err.Error())
		return
	}

	slog.Info("Claim bound to sandbox", "sessionID", sessionID, "sandbox", sandboxName)

	// Try to push session to warm agent immediately (instant session start).
	// The agent connects with its original pod name, but the sandbox gets renamed when claimed.
	// Get the original pod name from the sandbox annotation.
	originalPodName := sandboxName // fallback
	if sandbox, err := m.k8s.GetSandboxByName(ctx, sandboxName); err == nil && sandbox != nil {
		if podName := sandbox.GetOriginalPodName(); podName != "" {
			originalPodName = podName
			slog.Info("Found original pod name from sandbox", "sessionID", sessionID, "originalPodName", originalPodName, "sandbox", sandboxName)
		}
	}

	m.AssignSessionToWarmAgent(originalPodName, sessionID)

	// Label the sandbox so the informer can track it
	if err := m.k8s.LabelSandbox(ctx, sandboxName, sessionID); err != nil {
		slog.Error("Failed to label sandbox", "sessionID", sessionID, "sandbox", sandboxName, "error", err)
		// Cleanup: delete the claim and sandbox - without label we can't manage it
		if delErr := m.k8s.DeleteSandboxClaim(ctx, sessionID); delErr != nil {
			slog.Error("Failed to cleanup sandbox claim", "sessionID", sessionID, "error", delErr)
		}
		if delErr := m.k8s.DeleteSandbox(ctx, sessionID); delErr != nil {
			slog.Error("Failed to cleanup sandbox", "sessionID", sessionID, "error", delErr)
		}
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, "failed to label sandbox")
		return
	}

	// Get sandbox to retrieve serviceFQDN
	sandbox, err := m.k8s.GetSandboxByName(ctx, sandboxName)
	if err != nil {
		slog.Error("Failed to get bound sandbox", "sessionID", sessionID, "sandbox", sandboxName, "error", err)
		// Cleanup: delete the claim and sandbox
		if delErr := m.k8s.DeleteSandboxClaim(ctx, sessionID); delErr != nil {
			slog.Error("Failed to cleanup sandbox claim", "sessionID", sessionID, "error", delErr)
		}
		if delErr := m.k8s.DeleteSandbox(ctx, sessionID); delErr != nil {
			slog.Error("Failed to cleanup sandbox", "sessionID", sessionID, "error", delErr)
		}
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, err.Error())
		return
	}

	fqdn := sandbox.Status.ServiceFQDN

	// If fqdn is empty but sandbox is ready, construct it manually.
	// The warm pool controller doesn't populate serviceFQDN in sandbox status.
	if fqdn == "" && sandbox.IsReady() {
		fqdn = fmt.Sprintf("%s.%s.svc.cluster.local", sandboxName, m.config.K8sNamespace)
		slog.Info("Constructed serviceFQDN", "sessionID", sessionID, "fqdn", fqdn)
	}

	// Warm pool sandboxes should already be Ready, but verify
	if !sandbox.IsReady() {
		// Wait for sandbox to become ready (shouldn't happen with warm pool)
		slog.Warn("Bound sandbox not ready yet, waiting", "sessionID", sessionID, "sandbox", sandboxName)
		fqdn, err = m.k8s.WaitForReady(ctx, sessionID, sandboxReadyTimeout)
		if err != nil {
			slog.Error("Sandbox not ready", "sessionID", sessionID, "error", err)
			// Cleanup: delete the claim and sandbox to avoid resource leak
			if delErr := m.k8s.DeleteSandboxClaim(ctx, sessionID); delErr != nil {
				slog.Error("Failed to cleanup sandbox claim after timeout", "sessionID", sessionID, "error", delErr)
			}
			if delErr := m.k8s.DeleteSandbox(ctx, sessionID); delErr != nil {
				slog.Error("Failed to cleanup sandbox after timeout", "sessionID", sessionID, "error", delErr)
			} else {
				slog.Info("Cleaned up sandbox after timeout", "sessionID", sessionID)
			}
			m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
			m.emitSessionError(ctx, sessionID, err.Error())
			return
		}
	}

	// Apply network policies AFTER sandbox is ready
	// Always enable internet access (required for LLM API calls)
	if err := m.k8s.ConfigureNetwork(ctx, sessionID, true); err != nil {
		slog.Error("Failed to apply internet access policy", "sessionID", sessionID, "error", err)
		// Non-fatal: continue with sandbox creation, but log the error
	} else {
		slog.Info("Applied internet access policy", "sessionID", sessionID)
	}
	if tailnetEnabled {
		if err := m.k8s.ConfigureTailnetAccess(ctx, sessionID, true); err != nil {
			slog.Error("Failed to apply tailnet access policy", "sessionID", sessionID, "error", err)
			// Non-fatal: continue with sandbox creation, but log the error
		} else {
			slog.Info("Applied tailnet access policy", "sessionID", sessionID)
		}
	}

	// Create Tailscale-exposed service for preview URLs
	if err := m.k8s.CreateSandboxService(ctx, sessionID); err != nil {
		slog.Warn("Failed to create sandbox service", "sessionID", sessionID, "error", err)
		// Non-fatal: sandbox still works, just no preview URLs
	}

	// Restore any previously exposed ports (for resume after pause)
	m.restoreExposedPorts(ctx, sessionID)

	// Store PVC name for resume after pause
	// Use GetPVCName which handles warm pool correctly (reads pod-name annotation)
	if pvcName, err := m.k8s.GetPVCName(ctx, sessionID); err == nil {
		if err := m.storage.SetPVCName(ctx, sessionID, pvcName); err != nil {
			slog.Warn("Failed to store PVC name", "sessionID", sessionID, "pvc", pvcName, "error", err)
		} else {
			slog.Info("Stored PVC name", "sessionID", sessionID, "pvc", pvcName)
		}

		// Ensure the session anchor ConfigMap exists and owns the PVC.
		// This prevents the PVC from being garbage-collected when the Sandbox is deleted (during pause).
		if err := m.k8s.EnsureSessionAnchor(ctx, sessionID); err != nil {
			slog.Warn("Failed to create session anchor", "sessionID", sessionID, "error", err)
		} else if err := m.k8s.AddSessionAnchorToPVC(ctx, sessionID, pvcName); err != nil {
			slog.Warn("Failed to add session anchor to PVC", "sessionID", sessionID, "pvc", pvcName, "error", err)
		}
	}

	// Update state and check for pending prompt
	var pendingPrompt string
	m.mu.Lock()
	if state, ok := m.sessions[sessionID]; ok {
		state.ServiceFQDN = fqdn
		pendingPrompt = state.PendingPrompt
		state.PendingPrompt = "" // Clear it
		// Only set RUNNING if there's a pending prompt, otherwise READY
		if pendingPrompt != "" {
			state.Session.Status = pb.SessionStatus_SESSION_STATUS_RUNNING
		} else {
			state.Session.Status = pb.SessionStatus_SESSION_STATUS_READY
		}
	}
	m.mu.Unlock()

	newStatus := pb.SessionStatus_SESSION_STATUS_READY
	if pendingPrompt != "" {
		newStatus = pb.SessionStatus_SESSION_STATUS_RUNNING
	}
	if err := m.storage.UpdateSessionStatus(ctx, sessionID, newStatus); err != nil {
		slog.Error("Failed to update session status", "sessionID", sessionID, "error", err)
	}

	// Emit update
	if session := m.getSession(sessionID); session != nil {
		m.emitSessionUpdated(ctx, session)
	}

	// Process pending prompt if any
	if pendingPrompt != "" {
		slog.Info("Processing pending prompt (warm pool)", "sessionID", sessionID)
		if err := m.SendPrompt(ctx, sessionID, pendingPrompt); err != nil {
			slog.Error("Failed to send pending prompt", "sessionID", sessionID, "error", err)
		}
	}

	slog.Info("Session sandbox ready (warm pool)", "sessionID", sessionID, "fqdn", fqdn, "status", newStatus)
}

func (m *Manager) updateSessionStatus(ctx context.Context, sessionID string, status pb.SessionStatus) {
	m.mu.Lock()
	if state, ok := m.sessions[sessionID]; ok {
		state.Session.Status = status
	}
	m.mu.Unlock()

	_ = m.storage.UpdateSessionStatus(ctx, sessionID, status)

	if session := m.getSession(sessionID); session != nil {
		// Emit to session-specific subscribers
		m.emitSessionUpdated(ctx, session)

		// Also broadcast globally so clients on the sessions list page see the update
		if m.onSessionUpdated != nil {
			m.onSessionUpdated(session)
		}
	}
}

func (m *Manager) updateSessionName(ctx context.Context, sessionID, name string) {
	m.mu.Lock()
	if state, ok := m.sessions[sessionID]; ok {
		state.Session.Name = name
	}
	m.mu.Unlock()

	_ = m.storage.UpdateSessionField(ctx, sessionID, "name", name)

	if session := m.getSession(sessionID); session != nil {
		// Emit to session-specific subscribers
		m.emitSessionUpdated(ctx, session)

		// Also broadcast globally so clients on the sessions list page see the update
		if m.onSessionUpdated != nil {
			m.onSessionUpdated(session)
		}
	}
}

// updateLastActiveAt updates the last active timestamp for a session.
func (m *Manager) updateLastActiveAt(ctx context.Context, sessionID string) {
	now := timestamppb.Now()

	m.mu.Lock()
	if state, ok := m.sessions[sessionID]; ok {
		state.Session.LastActiveAt = now
	}
	m.mu.Unlock()

	_ = m.storage.UpdateSessionField(ctx, sessionID, "lastActiveAt", now.AsTime().UTC().Format(time.RFC3339))
}

// waitForSandbox waits for an existing sandbox to become ready (used when sandbox already exists).
func (m *Manager) waitForSandbox(ctx context.Context, sessionID string) {
	fqdn, err := m.k8s.WaitForReady(ctx, sessionID, sandboxReadyTimeout)
	if err != nil {
		slog.Error("Sandbox failed to become ready", "sessionID", sessionID, "error", err)
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_ERROR)
		m.emitSessionError(ctx, sessionID, err.Error())
		return
	}

	// Update state and check for pending prompt
	var pendingPrompt string
	m.mu.Lock()
	if state, ok := m.sessions[sessionID]; ok {
		state.ServiceFQDN = fqdn
		state.Session.Status = pb.SessionStatus_SESSION_STATUS_READY
		pendingPrompt = state.PendingPrompt
		state.PendingPrompt = "" // Clear it
	}
	m.mu.Unlock()

	if err := m.storage.UpdateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_READY); err != nil {
		slog.Error("Failed to update session status", "sessionID", sessionID, "error", err)
	}

	// Emit update
	if session := m.getSession(sessionID); session != nil {
		m.emitSessionUpdated(ctx, session)
	}

	// Process pending prompt if any
	if pendingPrompt != "" {
		slog.Info("Processing pending prompt (waitForSandbox)", "sessionID", sessionID)
		if err := m.SendPrompt(ctx, sessionID, pendingPrompt); err != nil {
			slog.Error("Failed to send pending prompt", "sessionID", sessionID, "error", err)
		}
	}

	slog.Info("Sandbox ready", "sessionID", sessionID, "fqdn", fqdn)
}

// Resume resumes a paused session.
func (m *Manager) Resume(ctx context.Context, id string) (*pb.Session, error) {
	state := m.getOrLoadState(ctx, id)
	if state == nil {
		return nil, fmt.Errorf("session %s not found", id)
	}

	m.mu.Lock()

	// If session is already CREATING or RESUMING, a sandbox creation is already in progress.
	// Don't start another one - just return and let the caller queue the prompt.
	if state.Session.Status == pb.SessionStatus_SESSION_STATUS_CREATING ||
		state.Session.Status == pb.SessionStatus_SESSION_STATUS_RESUMING {
		slog.Info("Session already creating/resuming, skipping Resume", "sessionID", id, "status", state.Session.Status.String())
		m.mu.Unlock()
		return state.Session, nil
	}
	m.mu.Unlock()

	// Check if sandbox exists and is ready BEFORE calling ensureActiveSlot.
	// If sandbox already exists, we don't need to make room for it.
	status, err := m.k8s.GetStatus(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get sandbox status: %w", err)
	}

	if status.Exists && status.Ready && status.ServiceFQDN != "" {
		// Sandbox already running and ready to accept prompts - no need to ensure slot
		m.mu.Lock()
		state.ServiceFQDN = status.ServiceFQDN
		// Preserve RUNNING status if already set (e.g., during warm pool creation)
		// Only change to READY if we were in a different state (PAUSED, RESUMING, etc.)
		wasAlreadyActive := state.Session.Status == pb.SessionStatus_SESSION_STATUS_READY ||
			state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING
		if !wasAlreadyActive {
			state.Session.Status = pb.SessionStatus_SESSION_STATUS_READY
		}
		m.mu.Unlock()

		// Only update storage if status actually changed
		if !wasAlreadyActive {
			_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_READY)
		}
		return state.Session, nil
	}

	// Sandbox doesn't exist or isn't ready - ensure we have a slot before creating/waiting
	m.ensureActiveSlot(ctx, id)

	if status.Exists {
		// Sandbox exists but not ready yet - just wait for it
		m.mu.Lock()
		state.Session.Status = pb.SessionStatus_SESSION_STATUS_RESUMING
		m.mu.Unlock()

		_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_RESUMING)

		// Wait for existing sandbox to become ready
		go m.waitForSandbox(context.Background(), id)

		return state.Session, nil
	}

	// No sandbox exists - create new one (resuming from paused state)
	m.mu.Lock()
	state.Session.Status = pb.SessionStatus_SESSION_STATUS_RESUMING
	restoreSnapshotID := state.RestoreSnapshotID
	state.RestoreSnapshotID = "" // Clear it in memory
	m.mu.Unlock()

	_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_RESUMING)

	// Always use direct sandbox creation for resume (bypasses warm pool).
	// This ensures we use the session's existing PVC instead of getting a fresh warm pool PVC.
	// createSandboxDirect will automatically use the stored PVC name if available.
	// Note: Resume always uses default tailnetEnabled=false since we don't store network config
	// Note: Resume doesn't support custom resources - uses nil to get default resources
	if restoreSnapshotID != "" {
		slog.Info("Resuming with snapshot restore", "sessionID", id, "snapshotID", restoreSnapshotID)
		_ = m.storage.ClearRestoreSnapshotID(ctx, id) // Clear from storage
		go m.createSandboxDirect(context.Background(), id, state.Session.Repos, state.Session.RepoAccess, false, nil, restoreSnapshotID)
	} else {
		slog.Info("Resuming session", "sessionID", id)
		go m.createSandboxDirect(context.Background(), id, state.Session.Repos, state.Session.RepoAccess, false, nil)
	}

	return state.Session, nil
}

// Pause pauses a running session.
func (m *Manager) Pause(ctx context.Context, id string) (*pb.Session, error) {
	state := m.getOrLoadState(ctx, id)
	if state == nil {
		return nil, fmt.Errorf("session %s not found", id)
	}

	// Delete claim if using warm pool
	if m.config.UseWarmPool {
		if err := m.k8s.DeleteSandboxClaim(ctx, id); err != nil {
			slog.Warn("Failed to delete sandbox claim", "sessionID", id, "error", err)
		}
	}

	// Delete Tailscale service
	if err := m.k8s.DeleteSandboxService(ctx, id); err != nil {
		slog.Warn("Failed to delete sandbox service", "sessionID", id, "error", err)
	}

	// Delete network restriction policy if any
	if err := m.k8s.DeleteNetworkRestriction(ctx, id); err != nil {
		slog.Warn("Failed to delete network restriction policy", "sessionID", id, "error", err)
	}

	// Delete sandbox - PVC survives because it has the session anchor ConfigMap as a second owner
	if err := m.k8s.DeleteSandbox(ctx, id); err != nil {
		slog.Warn("Failed to delete sandbox", "sessionID", id, "error", err)
	}

	// Delete secret (only in direct mode - warm pool uses shared secret)
	if !m.config.UseWarmPool {
		if err := m.k8s.DeleteSecret(ctx, id); err != nil {
			slog.Warn("Failed to delete secret", "sessionID", id, "error", err)
		}
	}

	// Update state
	m.mu.Lock()
	state.ServiceFQDN = ""
	state.Session.Status = pb.SessionStatus_SESSION_STATUS_PAUSED
	m.mu.Unlock()

	_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_PAUSED)

	return state.Session, nil
}

// Delete deletes a session and all its resources.
func (m *Manager) Delete(ctx context.Context, id string) error {
	// Close the session state (stops broadcast loop)
	m.mu.Lock()
	if state, ok := m.sessions[id]; ok {
		state.Close()
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	// Delete K8s VolumeSnapshots first (before PVC deletion)
	if snapshots, err := m.k8s.ListVolumeSnapshots(ctx, id); err != nil {
		slog.Warn("Failed to list VolumeSnapshots during session delete", "sessionID", id, "error", err)
	} else {
		for _, snap := range snapshots {
			if err := m.k8s.DeleteVolumeSnapshot(ctx, id, snap.SnapshotID); err != nil {
				slog.Warn("Failed to delete VolumeSnapshot during session delete",
					"sessionID", id, "snapshotID", snap.SnapshotID, "error", err)
			}
		}
	}

	// Delete K8s resources
	_ = m.k8s.DeleteSandboxClaim(ctx, id)
	_ = m.k8s.DeleteSandboxService(ctx, id)
	_ = m.k8s.DeleteNetworkRestriction(ctx, id)
	_ = m.k8s.DeleteSandbox(ctx, id)
	_ = m.k8s.DeleteSecret(ctx, id)
	_ = m.k8s.DeleteSessionAnchor(ctx, id)
	_ = m.k8s.DeletePVC(ctx, id)

	// Delete from storage
	if err := m.storage.DeleteSession(ctx, id); err != nil {
		return fmt.Errorf("delete session from storage: %w", err)
	}

	slog.Info("Session deleted", "sessionID", id)
	return nil
}

// DeleteAll deletes all sessions and their resources.
// Returns the list of deleted session IDs.
func (m *Manager) DeleteAll(ctx context.Context) ([]string, error) {
	// Get all session IDs
	m.mu.Lock()
	sessionIDs := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	m.mu.Unlock()

	var deletedIDs []string
	var lastErr error

	for _, id := range sessionIDs {
		if err := m.Delete(ctx, id); err != nil {
			slog.Error("Failed to delete session during DeleteAll", "sessionID", id, "error", err)
			lastErr = err
			continue
		}
		deletedIDs = append(deletedIDs, id)
	}

	slog.Info("DeleteAll completed", "deleted", len(deletedIDs), "total", len(sessionIDs))

	if lastErr != nil {
		return deletedIDs, fmt.Errorf("some sessions failed to delete: %w", lastErr)
	}

	return deletedIDs, nil
}

// ExposePort exposes a port for a session via Tailscale and persists the event.
func (m *Manager) ExposePort(ctx context.Context, sessionID string, port int) (string, error) {
	if err := m.k8s.ExposePort(ctx, sessionID, port); err != nil {
		return "", err
	}

	previewHost, err := m.k8s.GetSandboxPreviewHostname(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to resolve sandbox preview hostname, using fallback", "sessionID", sessionID, "error", err)
		previewHost = fmt.Sprintf("sandbox-%s", sessionID)
	}
	previewURL := fmt.Sprintf("http://%s:%d", previewHost, port)
	port32 := int32(port)

	// Create and persist the port_exposed event
	event := &pb.AgentEvent{
		Kind:          pb.AgentEventKind_AGENT_EVENT_KIND_PORT_EXPOSED,
		CorrelationId: fmt.Sprintf("port-%d", port),
		Payload: &pb.AgentEvent_PortExposed{
			PortExposed: &pb.PortExposedPayload{
				Port:       port32,
				PreviewUrl: &previewURL,
			},
		},
	}

	// Emit to all connected clients (this also persists to the stream)
	// Port exposed events are final (not partial streaming)
	m.emitAgentEvent(ctx, sessionID, event, false)

	return previewURL, nil
}

// restoreExposedPorts re-exposes ports that were previously exposed for a session.
// This is called during resume to restore port exposure after sandbox recreation.
// It reads persisted port_exposed events from the stream and re-applies them to K8s.
func (m *Manager) restoreExposedPorts(ctx context.Context, sessionID string) {
	// Read all event entries from the stream
	entries, err := m.storage.GetStreamEntriesByTypes(ctx, sessionID, "0", 0, []string{storage.StreamEntryTypeEvent})
	if err != nil {
		slog.Warn("Failed to read stream entries for port restoration", "sessionID", sessionID, "error", err)
		return
	}

	// Collect unique ports that were exposed
	exposedPorts := make(map[int32]bool)
	for _, e := range entries {
		if e.Entry.Partial {
			continue // Skip partial (streaming) entries
		}
		var event pb.AgentEvent
		if err := json.Unmarshal(e.Entry.Payload, &event); err != nil {
			continue
		}
		if event.Kind == pb.AgentEventKind_AGENT_EVENT_KIND_PORT_EXPOSED {
			if portPayload := event.GetPortExposed(); portPayload != nil {
				exposedPorts[portPayload.Port] = true
			}
		}
	}

	// Re-expose each port (only updates K8s, doesn't re-emit events)
	for port := range exposedPorts {
		if err := m.k8s.ExposePort(ctx, sessionID, int(port)); err != nil {
			slog.Warn("Failed to restore exposed port", "sessionID", sessionID, "port", port, "error", err)
		} else {
			slog.Info("Restored exposed port", "sessionID", sessionID, "port", port)
		}
	}
}

// List returns all sessions, reconciling with K8s state.
func (m *Manager) List(ctx context.Context) ([]pb.Session, error) {
	// Reconcile with K8s
	sandboxes, err := m.k8s.ListSandboxes(ctx)
	if err != nil {
		slog.Warn("Failed to list sandboxes", "error", err)
	}

	sandboxMap := make(map[string]k8s.SandboxInfo)
	for _, sb := range sandboxes {
		sandboxMap[sb.SessionID] = sb
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	sessions := make([]pb.Session, 0, len(m.sessions))
	for id, state := range m.sessions {
		sb, exists := sandboxMap[id]
		if !exists && (state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING || state.Session.Status == pb.SessionStatus_SESSION_STATUS_CREATING) {
			state.Session.Status = pb.SessionStatus_SESSION_STATUS_PAUSED
			_ = m.storage.UpdateSessionStatus(ctx, id, pb.SessionStatus_SESSION_STATUS_PAUSED)
		} else if exists && sb.Ready {
			state.ServiceFQDN = sb.ServiceFQDN
		}
		sessions = append(sessions, *state.Session)
	}

	return sessions, nil
}

// Get returns a session by ID.
func (m *Manager) Get(ctx context.Context, id string) (*pb.Session, error) {
	state := m.getOrLoadState(ctx, id)
	if state == nil {
		return nil, fmt.Errorf("session %s not found", id)
	}

	return state.Session, nil
}

// GetWithHistory returns a session with its stream entries and in-progress state.
// Returns: session, entries, hasMore, lastStreamID, inProgress, error
func (m *Manager) GetWithHistory(ctx context.Context, id string, afterStreamID string, limit int) (*pb.Session, []storage.StreamEntryWithID, bool, string, *pb.InProgressState, error) {
	session, err := m.Get(ctx, id)
	if err != nil {
		return nil, nil, false, "", nil, err
	}

	// Get all entries and filter for history (non-partial only)
	allEntries, err := m.storage.GetStreamEntries(ctx, id, afterStreamID, limit)
	if err != nil {
		return nil, nil, false, "", nil, fmt.Errorf("get stream entries: %w", err)
	}

	// Filter to only include non-partial (final) entries for history
	entries := make([]storage.StreamEntryWithID, 0, len(allEntries))
	for _, e := range allEntries {
		if !e.Entry.Partial {
			entries = append(entries, e)
		}
	}

	// Get the latest stream ID for cursor-based subscription
	lastStreamID, err := m.storage.GetLastStreamID(ctx, id)
	if err != nil {
		slog.Warn("Failed to get last stream ID, using $", "sessionID", id, "error", err)
		lastStreamID = "$" // Default to "only new" if error
	}

	// If session is RUNNING, include in-progress state
	var inProgress *pb.InProgressState
	if session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING {
		m.mu.RLock()
		state, ok := m.sessions[id]
		if ok {
			inProgress = &pb.InProgressState{
				Messages: make(map[string]string),
				Thinking: make(map[string]string),
				Tools:    make(map[string]*pb.InProgressTool),
			}
			// Add current message content if any
			if state.CurrentMessageID != "" {
				content := state.ContentBuilder.String()
				if content != "" {
					inProgress.Messages[state.CurrentMessageID] = content
				}
			}
			// TODO: Add thinking and tool accumulators when state.go is updated
		}
		m.mu.RUnlock()
	}

	// TODO: implement proper pagination with hasMore
	hasMore := false

	return session, entries, hasMore, lastStreamID, inProgress, nil
}

// GetAllWithMeta returns all sessions with metadata.
func (m *Manager) GetAllWithMeta(ctx context.Context) ([]pb.SessionSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]pb.SessionSummary, 0, len(m.sessions))
	for id, state := range m.sessions {
		meta := pb.SessionSummary{Session: state.Session}

		count, err := m.storage.GetMessageCount(ctx, id)
		if err == nil {
			count32 := int32(count)
			meta.MessageCount = &count32
		}

		// Get last stream ID as a cursor reference
		lastStreamID, err := m.storage.GetLastStreamID(ctx, id)
		if err == nil && lastStreamID != "" && lastStreamID != "$" {
			meta.LastStreamId = &lastStreamID
		}

		result = append(result, meta)
	}

	return result, nil
}

func (m *Manager) getSession(id string) *pb.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if state, ok := m.sessions[id]; ok {
		return state.Session
	}
	return nil
}

func (m *Manager) getState(id string) *SessionState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.sessions[id]
}

// getOrLoadState retrieves session state from memory or loads from storage if not found.
// This handles the case where a session was created on a different pod during rollout.
func (m *Manager) getOrLoadState(ctx context.Context, id string) *SessionState {
	// First try memory
	m.mu.RLock()
	state, ok := m.sessions[id]
	m.mu.RUnlock()

	if ok {
		return state
	}

	// Not in memory, try loading from storage
	session, err := m.storage.GetSession(ctx, id)
	if err != nil {
		slog.Debug("Session not found in storage", "sessionID", id, "error", err)
		return nil
	}
	if session == nil {
		slog.Debug("Session not found in storage", "sessionID", id)
		return nil
	}

	// Load into memory
	state = NewSessionState(session)

	// Load persisted restore snapshot ID if any
	if restoreID, err := m.storage.GetRestoreSnapshotID(ctx, id); err == nil && restoreID != "" {
		state.RestoreSnapshotID = restoreID
	}

	// Check K8s for sandbox status to set ServiceFQDN
	if status, err := m.k8s.GetStatus(ctx, id); err == nil && status.Exists && status.Ready {
		state.ServiceFQDN = status.ServiceFQDN
	}

	m.mu.Lock()
	// Double-check it wasn't added while we were loading
	if existing, ok := m.sessions[id]; ok {
		m.mu.Unlock()
		return existing
	}
	m.sessions[id] = state
	m.mu.Unlock()

	slog.Info("Loaded session from storage into memory", "sessionID", id)
	return state
}

// isActiveStatus returns true if the status represents an active session (using resources).
func isActiveStatus(status pb.SessionStatus) bool {
	return status == pb.SessionStatus_SESSION_STATUS_CREATING ||
		status == pb.SessionStatus_SESSION_STATUS_RESUMING ||
		status == pb.SessionStatus_SESSION_STATUS_READY ||
		status == pb.SessionStatus_SESSION_STATUS_RUNNING
}

// countActiveSessionsLocked returns the number of active sessions.
// Excludes the session with excludeID if provided.
// Must be called with m.mu held (read or write).
func (m *Manager) countActiveSessionsLocked(excludeID string) int {
	count := 0
	for id, state := range m.sessions {
		if id == excludeID {
			continue
		}
		if isActiveStatus(state.Session.Status) {
			count++
		}
	}
	return count
}

// findOldestActiveSession finds the oldest active session by LastActiveAt.
// Excludes the session with excludeID. Must be called with m.mu held.
func (m *Manager) findOldestActiveSessionLocked(excludeID string) *SessionState {
	var oldest *SessionState
	var oldestTime time.Time

	for id, state := range m.sessions {
		if id == excludeID {
			continue
		}
		if !isActiveStatus(state.Session.Status) {
			continue
		}

		if state.Session.LastActiveAt == nil {
			continue
		}
		lastActive := state.Session.LastActiveAt.AsTime()

		if oldest == nil || lastActive.Before(oldestTime) {
			oldest = state
			oldestTime = lastActive
		}
	}

	return oldest
}

// ensureActiveSlot ensures there's room for a new active session.
// If at the limit, it pauses oldest active sessions until there's room (excluding excludeID).
// Returns the last paused session ID (if any) or empty string.
func (m *Manager) ensureActiveSlot(ctx context.Context, excludeID string) string {
	maxActive := m.config.MaxActiveSessions
	if maxActive <= 0 {
		return ""
	}

	var lastPausedID string

	for {
		m.mu.Lock()
		activeCount := m.countActiveSessionsLocked(excludeID)

		if activeCount < maxActive {
			m.mu.Unlock()
			return lastPausedID
		}

		// Find oldest active session to pause
		oldest := m.findOldestActiveSessionLocked(excludeID)
		if oldest == nil {
			m.mu.Unlock()
			return lastPausedID
		}

		oldestID := oldest.Session.Id
		m.mu.Unlock()

		// Pause the oldest session
		slog.Info("Auto-pausing session to make room", "sessionID", oldestID, "activeCount", activeCount, "maxActive", maxActive)
		pausedSession, err := m.Pause(ctx, oldestID)
		if err != nil {
			slog.Error("Failed to auto-pause session", "sessionID", oldestID, "error", err)
			return lastPausedID
		}

		// Notify callback so API can broadcast to clients
		if m.onSessionUpdated != nil && pausedSession != nil {
			m.onSessionUpdated(pausedSession)
		}

		lastPausedID = oldestID
	}
}

// enforceActiveLimit pauses sessions until the active count is within MaxActiveSessions.
// This is called on startup to clean up excess sessions.
func (m *Manager) enforceActiveLimit(ctx context.Context) {
	maxActive := m.config.MaxActiveSessions
	if maxActive <= 0 {
		return
	}

	for {
		m.mu.Lock()
		activeCount := m.countActiveSessionsLocked("")

		if activeCount <= maxActive {
			m.mu.Unlock()
			return
		}

		// Find oldest active session to pause
		oldest := m.findOldestActiveSessionLocked("")
		if oldest == nil {
			m.mu.Unlock()
			return
		}

		oldestID := oldest.Session.Id
		m.mu.Unlock()

		slog.Info("Enforcing active limit on startup", "sessionID", oldestID, "activeCount", activeCount, "maxActive", maxActive)
		if _, err := m.Pause(ctx, oldestID); err != nil {
			slog.Error("Failed to pause session during limit enforcement", "sessionID", oldestID, "error", err)
			return
		}
	}
}

// Subscribe creates a StreamSubscriber for a session.
// lastNotificationID specifies where to start reading:
//   - "$" = only new notifications
//   - "0" = from the beginning of the stream
//   - "<stream-id>" = from after the given ID (exclusive)
//
// The subscriber reads from Redis Streams and is closed when ctx is cancelled.
func (m *Manager) Subscribe(ctx context.Context, id string, lastNotificationID string) (*StreamSubscriber, error) {
	state := m.getState(id)
	if state == nil {
		return nil, fmt.Errorf("session %s not found", id)
	}

	client := m.storage.GetRedisClient()
	sub := NewStreamSubscriber(id, lastNotificationID, client)
	go sub.Run(ctx)

	return sub, nil
}

// ListGitHubRepos returns repositories accessible to the GitHub App installation.
func (m *Manager) ListGitHubRepos(ctx context.Context) ([]GitHubRepo, error) {
	if m.githubClient == nil {
		slog.Warn("ListGitHubRepos called but GitHub App is not configured")
		return nil, nil
	}

	repos, err := m.githubClient.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing repos: %w", err)
	}

	result := make([]GitHubRepo, len(repos))
	for i, r := range repos {
		result[i] = GitHubRepo{
			Name:        r.Name,
			FullName:    r.FullName,
			Private:     r.Private,
			Description: r.Description,
		}
	}

	return result, nil
}

// GitHubRepo represents a GitHub repository (kept for API compatibility).
type GitHubRepo struct {
	Name        string
	FullName    string
	Private     bool
	Description string
}

// GetSessionIDByPodName finds the session ID for a pod name (used by warm pool agents).
func (m *Manager) GetSessionIDByPodName(ctx context.Context, podName string) (string, error) {
	return m.k8s.GetSessionIDByPodName(ctx, podName)
}

// GetSessionConfig returns session configuration for the agent.
// This is used by agents to get session-specific config when using warm pools.
func (m *Manager) GetSessionConfig(ctx context.Context, sessionID string) (*AgentSessionConfig, error) {
	state := m.getOrLoadState(ctx, sessionID)
	if state == nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	config := &AgentSessionConfig{
		SessionID:          sessionID,
		OpenAIAPIKey:       m.config.OpenAIAPIKey,
		MistralAPIKey:      m.config.MistralAPIKey,
		GitHubCopilotToken: m.config.GitHubCopilotToken,
		SdkType:            state.Session.SdkType,
		CopilotBackend:     state.Session.CopilotBackend,
		OllamaURL:          m.config.OllamaURL,
		OpenCodeAPIKey:     m.config.OpenCodeAPIKey,
		ZaiAPIKey:          m.config.ZaiAPIKey,
	}

	if state.Session.Model != nil {
		model := *state.Session.Model
		config.Model = model

		// For Anthropic direct models (ID ends with :anthropic), force Anthropic backend
		if strings.HasSuffix(model, ":anthropic") {
			anthropicBackend := pb.CopilotBackend_COPILOT_BACKEND_ANTHROPIC
			config.CopilotBackend = &anthropicBackend
		}

		// For OpenCode SDK, parse model format: provider/model:thinking (e.g., anthropic/claude-sonnet-4-5:high)
		// Thinking levels: high, max (for Claude extended thinking)
		if state.Session.SdkType != nil && *state.Session.SdkType == pb.SdkType_SDK_TYPE_OPENCODE {
			parts := strings.Split(model, ":")
			if len(parts) == 2 {
				thinkingLevel := parts[1]
				if thinkingLevel == "high" || thinkingLevel == "max" {
					config.ReasoningEffort = thinkingLevel
					// Strip the thinking suffix from model ID for the agent
					config.Model = parts[0]
				}
			}
		}

		// For Codex SDK, parse model format: base:auth:effort (e.g., gpt-5-codex:oauth:high)
		// Also supports legacy format: base:auth (e.g., gpt-5-codex:oauth)
		if state.Session.SdkType != nil && *state.Session.SdkType == pb.SdkType_SDK_TYPE_CODEX {
			authMode, reasoningEffort := parseCodexAuthModeAndEffort(model)
			if authMode == "" {
				return nil, fmt.Errorf("invalid codex model for session %s: missing auth suffix (:oauth or :api)", sessionID)
			}
			if reasoningEffort != "" {
				config.ReasoningEffort = reasoningEffort
			}

			// Set credentials based on auth mode.
			// OAuth mode is session-scoped and loaded from encrypted storage, never from global env.
			if authMode == "api" {
				config.OpenAIAPIKey = m.config.OpenAIAPIKey
			} else if authMode == "oauth" {
				// Ensure OAuth sessions do not carry API-key credentials.
				config.OpenAIAPIKey = ""
				oauthData, err := m.getSessionCodexOAuth(ctx, sessionID)
				if err != nil {
					return nil, fmt.Errorf("get session codex oauth data: %w", err)
				}
				if oauthData == nil || oauthData.AccessToken == "" || oauthData.IdToken == "" {
					return nil, fmt.Errorf("codex oauth tokens not configured for session %s", sessionID)
				}
				config.CodexAccessToken = oauthData.AccessToken
				config.CodexIdToken = oauthData.IdToken
			}
		}
	}

	// Setup GitHub repo access if configured
	normalizedRepos := normalizeRepos(state.Session.Repos)
	if len(normalizedRepos) > 0 {
		config.Repos = normalizedRepos
		config.RepoAccess = state.Session.RepoAccess

		// Generate repo-scoped token via GitHub App for git credentials
		if token := m.createRepoToken(ctx, normalizedRepos, state.Session.RepoAccess); token != "" {
			config.GitHubToken = token
		}
	}

	return config, nil
}

// UpdateRepoAccess changes the repository access level for a session and sends new credentials to the agent.
func (m *Manager) UpdateRepoAccess(ctx context.Context, sessionID string, newAccess pb.RepoAccess) error {
	slog.Info("UpdateRepoAccess called", "sessionID", sessionID, "newAccess", newAccess)

	m.mu.Lock()
	state, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", sessionID)
	}

	// Need repos to generate scoped token
	repos := normalizeRepos(state.Session.Repos)

	// Update the session's repo access
	state.Session.RepoAccess = &newAccess
	m.mu.Unlock()

	// Generate new repo-scoped token for this access level
	newToken := m.createRepoToken(ctx, repos, &newAccess)
	if newToken == "" && len(repos) > 0 {
		slog.Warn("Failed to create GitHub token (GitHub App not configured?)", "sessionID", sessionID, "access", newAccess)
	}

	// Persist to Redis
	if err := m.storage.SaveSession(ctx, state.Session); err != nil {
		slog.Warn("Failed to persist session after repo access update", "sessionID", sessionID, "error", err)
	}

	// Send new credentials to the agent if connected
	m.mu.RLock()
	agent, agentConnected := m.agents[sessionID]
	m.mu.RUnlock()

	if agentConnected && newToken != "" {
		if err := agent.UpdateGitCredentials(newToken, newAccess); err != nil {
			slog.Warn("Failed to send updated credentials to agent", "sessionID", sessionID, "error", err)
			return fmt.Errorf("failed to update agent credentials: %w", err)
		}
		slog.Info("Sent updated git credentials to agent", "sessionID", sessionID, "access", newAccess)
	}

	// Emit session update
	m.emitSessionUpdated(ctx, state.Session)

	return nil
}

// publishStreamEntry publishes an entry to the unified session stream.
// All StreamSubscribers reading from this session's stream will receive it.
func (m *Manager) publishStreamEntry(ctx context.Context, sessionID string, entry *storage.StreamEntry) {
	if _, err := m.storage.AppendStreamEntry(ctx, sessionID, entry); err != nil {
		slog.Warn("Failed to publish stream entry", "sessionID", sessionID, "type", entry.Type, "error", err)
	}
}

// protoJsonOpts configures protojson to emit readable enum names instead of numbers.
var protoJsonOpts = protojson.MarshalOptions{
	UseEnumNumbers: false,
}

// emitSessionUpdated broadcasts a session update to all subscribers.
func (m *Manager) emitSessionUpdated(ctx context.Context, session *pb.Session) {
	payload, _ := protoJsonOpts.Marshal(session)
	m.publishStreamEntry(ctx, session.Id, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeSessionUpdate,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// emitSessionError broadcasts a session error to all subscribers.
func (m *Manager) emitSessionError(ctx context.Context, sessionID, errMsg string) {
	payload, _ := protoJsonOpts.Marshal(&pb.Error{
		Code:      "SESSION_ERROR",
		Message:   errMsg,
		SessionId: &sessionID,
	})
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeError,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// emitAgentEvent broadcasts an agent event to all subscribers.
// The partial flag indicates if this is a streaming delta (true) or final event (false).
// The optional timestamp parameter allows specifying when the event logically started (for correct ordering).
func (m *Manager) emitAgentEvent(ctx context.Context, sessionID string, event *pb.AgentEvent, partial bool, timestamp ...time.Time) {
	ts := time.Now()
	if len(timestamp) > 0 && !timestamp[0].IsZero() {
		ts = timestamp[0]
	}
	payload, _ := protoJsonOpts.Marshal(event)
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeEvent,
		Partial:   partial,
		Payload:   payload,
		Timestamp: ts.UTC().Format(time.RFC3339),
	})
}

// emitAgentMessage broadcasts an agent message to all subscribers.
// Uses AgentEvent with MESSAGE kind. partial=true for streaming deltas.
func (m *Manager) emitAgentMessage(ctx context.Context, sessionID, messageID, content string, partial bool) {
	event := &pb.AgentEvent{
		Kind:          pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE,
		CorrelationId: messageID,
		Payload: &pb.AgentEvent_Message{
			Message: &pb.MessagePayload{
				Role:    pb.MessageRole_MESSAGE_ROLE_ASSISTANT,
				Content: content,
			},
		},
	}
	payload, _ := protoJsonOpts.Marshal(event)
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeEvent,
		Partial:   partial,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// emitAgentDone broadcasts agent completion to all subscribers.
// This is emitted as a SESSION_UPDATE with the current session state.
func (m *Manager) emitAgentDone(ctx context.Context, sessionID string) {
	m.mu.RLock()
	state, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		slog.Warn("emitAgentDone: session not found", "sessionID", sessionID)
		return
	}

	payload, _ := protoJsonOpts.Marshal(state.Session)
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeSessionUpdate,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// emitAgentError broadcasts an agent error to all subscribers.
func (m *Manager) emitAgentError(ctx context.Context, sessionID, errMsg string) {
	payload, _ := protoJsonOpts.Marshal(&pb.Error{
		Code:      "AGENT_ERROR",
		Message:   errMsg,
		SessionId: &sessionID,
	})
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeError,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// emitUserMessage broadcasts a user message to all subscribers and increments message counter.
func (m *Manager) emitUserMessage(ctx context.Context, sessionID, text string) {
	messageID := uuid.New().String()
	event := &pb.AgentEvent{
		Kind:          pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE,
		CorrelationId: messageID,
		Payload: &pb.AgentEvent_Message{
			Message: &pb.MessagePayload{
				Role:    pb.MessageRole_MESSAGE_ROLE_USER,
				Content: text,
			},
		},
	}
	payload, _ := protoJsonOpts.Marshal(event)
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeEvent,
		Partial:   false, // User messages are always complete
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	// Increment message counter
	if err := m.storage.IncrementMessageCount(ctx, sessionID); err != nil {
		slog.Warn("Failed to increment message count", "sessionID", sessionID, "error", err)
	}
}

// EmitEvent broadcasts an event from a sandbox to all connected clients.
// This is used by the internal API to receive events from entrypoint scripts.
func (m *Manager) EmitEvent(ctx context.Context, sessionID string, event *pb.AgentEvent) error {
	// Verify session exists
	m.mu.RLock()
	_, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}

	// Emit the event (internal events are final, not streaming deltas)
	m.emitAgentEvent(ctx, sessionID, event, false)
	slog.Debug("Emitted internal event", "sessionID", sessionID, "kind", event.Kind)
	return nil
}

// emitTerminalOutput broadcasts terminal output to all connected clients.
func (m *Manager) emitTerminalOutput(ctx context.Context, sessionID, data string) {
	payload, _ := protoJsonOpts.Marshal(&pb.TerminalOutput{
		Data: data,
	})
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      storage.StreamEntryTypeTerminalOutput,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// SendTerminalInput sends input to the agent terminal.
func (m *Manager) SendTerminalInput(ctx context.Context, sessionID, data string) error {
	m.mu.RLock()
	agent, ok := m.agents[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no agent connected for session %s", sessionID)
	}

	return agent.SendTerminalInput(data)
}

// ResizeTerminal resizes the agent terminal.
func (m *Manager) ResizeTerminal(ctx context.Context, sessionID string, cols, rows int) error {
	m.mu.RLock()
	agent, ok := m.agents[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no agent connected for session %s", sessionID)
	}

	return agent.ResizeTerminal(cols, rows)
}

// RegisterAgentConnection registers an agent connection for a session.
func (m *Manager) RegisterAgentConnection(sessionID string, conn AgentConnection) {
	m.mu.Lock()
	m.agents[sessionID] = conn

	// Check for pending prompt and session status
	var pendingPrompt string
	var wasInterrupted bool
	var isRunning bool
	if state, ok := m.sessions[sessionID]; ok {
		// Session is ready to handle prompts if RUNNING or READY (sandbox available)
		isRunning = (state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING ||
			state.Session.Status == pb.SessionStatus_SESSION_STATUS_READY)
		// Grab pending prompt if session is ready to handle it
		// During restore, session is RESUMING until restore job completes
		if isRunning && state.PendingPrompt != "" {
			pendingPrompt = state.PendingPrompt
			state.PendingPrompt = "" // Clear it
		} else if state.PendingPrompt != "" {
			slog.Warn("Pending prompt exists but session not ready", "sessionID", sessionID, "status", state.Session.Status.String(), "pendingPrompt", state.PendingPrompt[:min(50, len(state.PendingPrompt))])
		}
		wasInterrupted = (state.Session.Status == pb.SessionStatus_SESSION_STATUS_INTERRUPTED)
	} else {
		slog.Warn("Session not found in RegisterAgentConnection", "sessionID", sessionID)
	}
	m.mu.Unlock()

	slog.Info("Agent connection registered", "sessionID", sessionID, "wasInterrupted", wasInterrupted, "isRunning", isRunning, "hasPendingPrompt", pendingPrompt != "")

	ctx := context.Background()

	// If session was interrupted, emit reconnect event (status stays interrupted until user sends prompt)
	if wasInterrupted {
		event := &pb.AgentEvent{
			Kind: pb.AgentEventKind_AGENT_EVENT_KIND_AGENT_RECONNECTED,
		}
		m.emitAgentEvent(ctx, sessionID, event, false)
		slog.Info("Emitted agent_reconnected event", "sessionID", sessionID)
	}

	// Send pending prompt if any (for prompts queued before agent connected during session creation)
	if pendingPrompt != "" {
		slog.Info("Sending pending prompt to agent", "sessionID", sessionID)
		if err := m.SendPrompt(ctx, sessionID, pendingPrompt); err != nil {
			slog.Error("Failed to send pending prompt", "sessionID", sessionID, "error", err)
		}
	}
}

// UnregisterAgentConnection unregisters an agent connection.
// If the session was running, it will be marked as interrupted.
func (m *Manager) UnregisterAgentConnection(sessionID string) {
	m.mu.Lock()
	wasRunning := false
	if state, ok := m.sessions[sessionID]; ok {
		wasRunning = (state.Session.Status == pb.SessionStatus_SESSION_STATUS_RUNNING)
	}
	delete(m.agents, sessionID)
	m.mu.Unlock()

	slog.Info("Agent connection unregistered", "sessionID", sessionID, "wasRunning", wasRunning)

	// If session was running, mark as interrupted and emit event
	if wasRunning {
		ctx := context.Background()
		m.updateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_INTERRUPTED)

		event := &pb.AgentEvent{
			Kind: pb.AgentEventKind_AGENT_EVENT_KIND_AGENT_DISCONNECTED,
		}
		m.emitAgentEvent(ctx, sessionID, event, false)

		slog.Info("Session marked as interrupted due to agent disconnect", "sessionID", sessionID)
	}
}

// GetAgentConnection returns the agent connection for a session (or nil if not connected).
func (m *Manager) GetAgentConnection(sessionID string) AgentConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agents[sessionID]
}

// RegisterWarmAgentConnection registers a warm pool agent waiting for session assignment.
func (m *Manager) RegisterWarmAgentConnection(podName string, conn WarmAgentConnection) {
	m.mu.Lock()
	m.warmAgents[podName] = conn
	sessionID, hasPending := m.pendingAssignments[podName]
	if hasPending {
		delete(m.pendingAssignments, podName)
	}
	m.mu.Unlock()

	slog.Info("Warm agent registered", "podName", podName)

	if hasPending {
		m.AssignSessionToWarmAgent(podName, sessionID)
	}
}

// UnregisterWarmAgentConnection unregisters a warm pool agent connection.
func (m *Manager) UnregisterWarmAgentConnection(podName string) {
	m.mu.Lock()
	delete(m.warmAgents, podName)
	delete(m.pendingAssignments, podName)
	m.mu.Unlock()

	slog.Info("Warm agent connection unregistered", "podName", podName)
}

// GetWarmAgentConnection returns a warm agent connection by pod name (or nil if not found).
func (m *Manager) GetWarmAgentConnection(podName string) WarmAgentConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.warmAgents[podName]
}

// AssignSessionToWarmAgent assigns a session to a warm pool agent and moves it to the active agents map.
// Returns true if the agent was found and assigned, false otherwise.
func (m *Manager) AssignSessionToWarmAgent(podName, sessionID string) bool {
	m.mu.Lock()
	warmAgent, ok := m.warmAgents[podName]
	if !ok {
		m.pendingAssignments[podName] = sessionID
		m.mu.Unlock()
		slog.Info("Queued pending session assignment", "podName", podName, "sessionID", sessionID)
		return false
	}

	// Move from warm to active
	delete(m.warmAgents, podName)
	m.agents[sessionID] = warmAgent
	m.mu.Unlock()

	slog.Info("Warm agent assigned to session", "podName", podName, "sessionID", sessionID)

	// Get session config and push to agent
	ctx := context.Background()
	config, err := m.GetSessionConfig(ctx, sessionID)
	if err != nil {
		slog.Error("Failed to get session config for warm agent", "sessionID", sessionID, "error", err)
		// Remove from active agents since we couldn't get config
		m.mu.Lock()
		delete(m.agents, sessionID)
		m.mu.Unlock()
		return false
	}

	if err := warmAgent.AssignSession(sessionID, config); err != nil {
		slog.Error("Failed to push session to warm agent", "podName", podName, "sessionID", sessionID, "error", err)
		// Remove from active agents since assignment failed
		m.mu.Lock()
		delete(m.agents, sessionID)
		m.mu.Unlock()
		return false
	}

	return true
}

// VerifyAgentToken validates a Kubernetes ServiceAccount token and returns the verified pod name.
// This is used to authenticate agents connecting to the control plane, preventing impersonation attacks.
// Uses default K8s audiences (for standard agent-to-control-plane authentication).
func (m *Manager) VerifyAgentToken(ctx context.Context, token string) (string, error) {
	return m.k8s.VerifyAgentToken(ctx, token, nil) // nil = default K8s audiences
}

// ProxyAuthResult contains the result of proxy authentication validation.
type ProxyAuthResult struct {
	Allowed     bool   // Whether the request is allowed
	SecretKey   string // Which secret to inject (e.g., "anthropic", "openai")
	SessionID   string // The session ID for logging
	Placeholder string // The placeholder value to look for (e.g., "NETCLODE_PLACEHOLDER_anthropic")
}

// ValidateProxyAuth validates a proxy authentication request.
// It validates the Kubernetes ServiceAccount token and checks if the target host is allowed.
// The token is cryptographically verified via TokenReview API.
func (m *Manager) ValidateProxyAuth(ctx context.Context, token, targetHost string) (*ProxyAuthResult, error) {
	// Verify the ServiceAccount token with "secret-proxy" audience
	// The proxy-auth projected token uses this specific audience
	podName, err := m.k8s.VerifyAgentToken(ctx, token, []string{"secret-proxy"})
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}

	// Look up session by pod name
	sessionID, err := m.k8s.GetSessionIDByPodName(ctx, podName)
	if err != nil {
		return nil, fmt.Errorf("session not found for pod %s: %w", podName, err)
	}

	// Get session state to determine SDK type
	state := m.getState(sessionID)
	if state == nil {
		// Try loading from storage
		session, err := m.storage.GetSession(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("session %s not found: %w", sessionID, err)
		}
		state = &SessionState{Session: session}
	}

	// Get allowed hosts based on SDK type
	sdkType := pb.SdkType_SDK_TYPE_CLAUDE // default
	if state.Session.SdkType != nil {
		sdkType = *state.Session.SdkType
	}

	// Check if target host is allowed and get the secret key
	secretKey, placeholder := m.getAllowedSecretForHost(sdkType, targetHost)
	if secretKey == "" {
		return &ProxyAuthResult{
			Allowed:   false,
			SessionID: sessionID,
		}, nil
	}

	return &ProxyAuthResult{
		Allowed:     true,
		SecretKey:   secretKey,
		SessionID:   sessionID,
		Placeholder: placeholder,
	}, nil
}

// getAllowedSecretForHost returns the secret key and placeholder for a given SDK type and host.
// Returns empty strings if the host is not allowed.
func (m *Manager) getAllowedSecretForHost(sdkType pb.SdkType, host string) (secretKey, placeholder string) {
	// Define allowed hosts per SDK type
	// Returns (secretKey, placeholder) where secretKey maps to the actual secret
	type hostMapping struct {
		hosts       []string
		secretKey   string
		placeholder string
	}

	var allowedMappings []hostMapping

	switch sdkType {
	case pb.SdkType_SDK_TYPE_CLAUDE:
		allowedMappings = []hostMapping{
			{hosts: []string{"api.anthropic.com"}, secretKey: "anthropic", placeholder: "NETCLODE_PLACEHOLDER_anthropic"},
		}

	case pb.SdkType_SDK_TYPE_OPENCODE:
		allowedMappings = []hostMapping{
			{hosts: []string{"api.anthropic.com"}, secretKey: "anthropic", placeholder: "NETCLODE_PLACEHOLDER_anthropic"},
			{hosts: []string{"api.openai.com"}, secretKey: "openai", placeholder: "NETCLODE_PLACEHOLDER_openai"},
			{hosts: []string{"api.mistral.ai"}, secretKey: "mistral", placeholder: "NETCLODE_PLACEHOLDER_mistral"},
			{hosts: []string{"openrouter.ai", "api.openrouter.ai"}, secretKey: "opencode", placeholder: "NETCLODE_PLACEHOLDER_opencode"},
			{hosts: []string{"api.opencode.ai"}, secretKey: "opencode", placeholder: "NETCLODE_PLACEHOLDER_opencode"},
			{hosts: []string{"open.bigmodel.cn"}, secretKey: "zai", placeholder: "NETCLODE_PLACEHOLDER_zai"},
		}

	case pb.SdkType_SDK_TYPE_COPILOT:
		allowedMappings = []hostMapping{
			{hosts: []string{"api.github.com", "copilot-proxy.githubusercontent.com", "api.individual.githubcopilot.com"}, secretKey: "github_copilot", placeholder: "NETCLODE_PLACEHOLDER_github_copilot"},
			{hosts: []string{"api.anthropic.com"}, secretKey: "anthropic", placeholder: "NETCLODE_PLACEHOLDER_anthropic"},
		}

	case pb.SdkType_SDK_TYPE_CODEX:
		// Codex authentication is now provided directly by backend-issued API/OAuth
		// tokens to the agent. No placeholder replacement is configured for Codex.
		allowedMappings = nil

	default:
		// Default to Claude behavior
		allowedMappings = []hostMapping{
			{hosts: []string{"api.anthropic.com"}, secretKey: "anthropic", placeholder: "NETCLODE_PLACEHOLDER_anthropic"},
		}
	}

	// Check if the target host matches any allowed host
	for _, mapping := range allowedMappings {
		for _, allowedHost := range mapping.hosts {
			if host == allowedHost {
				return mapping.secretKey, mapping.placeholder
			}
			// Support wildcard subdomains (e.g., *.githubusercontent.com)
			if strings.HasPrefix(allowedHost, "*.") {
				suffix := allowedHost[1:] // Remove the *
				if strings.HasSuffix(host, suffix) {
					return mapping.secretKey, mapping.placeholder
				}
			}
		}
	}

	return "", ""
}

func isCodexReasoningEffort(value string) bool {
	switch value {
	case "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

// parseCodexAuthModeAndEffort extracts auth mode and reasoning effort from model suffix.
// Supported formats:
// - base:api
// - base:oauth
// - base:api:high
// - base:oauth:low
func parseCodexAuthModeAndEffort(model string) (authMode, reasoningEffort string) {
	parts := strings.Split(model, ":")
	if len(parts) < 2 {
		return "", ""
	}

	authMode = parts[len(parts)-1]
	if isCodexReasoningEffort(authMode) {
		reasoningEffort = authMode
		if len(parts) >= 3 {
			authMode = parts[len(parts)-2]
		} else {
			authMode = ""
		}
	}

	if authMode != "api" && authMode != "oauth" {
		authMode = ""
	}
	return authMode, reasoningEffort
}

func cloneCodexOAuthData(data *storage.CodexOAuthSessionData) *storage.CodexOAuthSessionData {
	if data == nil {
		return nil
	}
	c := *data
	if data.ExpiresAt != nil {
		expiresAt := *data.ExpiresAt
		c.ExpiresAt = &expiresAt
	}
	return &c
}

func (m *Manager) getCodexOAuth(ctx context.Context) (*storage.CodexOAuthSessionData, error) {
	data, err := m.storage.GetCodexOAuth(ctx)
	if err != nil || data == nil {
		return data, err
	}
	return cloneCodexOAuthData(data), nil
}

func (m *Manager) saveCodexOAuth(ctx context.Context, data *storage.CodexOAuthSessionData) error {
	if err := m.storage.SaveCodexOAuth(ctx, data); err != nil {
		return err
	}
	return nil
}

// Legacy wrappers kept while tests/branches are still converging.
func (m *Manager) getSessionCodexOAuth(ctx context.Context, _ string) (*storage.CodexOAuthSessionData, error) {
	return m.getCodexOAuth(ctx)
}

func (m *Manager) saveSessionCodexOAuth(ctx context.Context, _ string, data *storage.CodexOAuthSessionData) error {
	return m.saveCodexOAuth(ctx, data)
}

// ListModels returns available models for the specified SDK type.
// For Copilot SDK, returns a combined list of GitHub Copilot and Anthropic (BYOK) models.
// For Codex SDK, returns OpenAI models with "gpt-codex" family.
func (m *Manager) ListModels(sdkType pb.SdkType, copilotBackend *pb.CopilotBackend) []*pb.ModelInfo {
	switch sdkType {
	case pb.SdkType_SDK_TYPE_CLAUDE:
		return m.fetchModelsFromModelsDev("anthropic")
	case pb.SdkType_SDK_TYPE_OPENCODE:
		return m.fetchOpenCodeModels()
	case pb.SdkType_SDK_TYPE_COPILOT:
		// Return combined list of GitHub Copilot + Anthropic (BYOK) models
		return m.fetchCopilotModels()
	case pb.SdkType_SDK_TYPE_CODEX:
		// Return OpenAI models with "gpt-codex" family
		return m.fetchCodexModels()
	default:
		return m.fetchModelsFromModelsDev("anthropic")
	}
}

// modelsDevResponse represents the response from models.dev API
type modelsDevResponse map[string]modelsDevProvider

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Family      string            `json:"family"`
	Attachment  bool              `json:"attachment"`
	Reasoning   bool              `json:"reasoning"`
	ToolCall    bool              `json:"tool_call"`
	Modalities  modelsDevModality `json:"modalities"`
	Cost        modelsDevCost     `json:"cost"`
	Limit       modelsDevLimit    `json:"limit"`
	ReleaseDate string            `json:"release_date"`
}

type modelsDevModality struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type modelsDevCost struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// fetchModelsFromModelsDev fetches models from models.dev API for a specific provider
func (m *Manager) fetchModelsFromModelsDev(provider string) []*pb.ModelInfo {
	models := m.doFetchModelsFromModelsDev(provider)
	if models == nil {
		return getModelsFallback(provider)
	}
	return models
}

func (m *Manager) doFetchModelsFromModelsDev(provider string) []*pb.ModelInfo {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		slog.Error("Failed to fetch from models.dev", "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("models.dev API error", "status", resp.StatusCode)
		return nil
	}

	var data modelsDevResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Error("Failed to decode models.dev response", "error", err)
		return nil
	}

	providerData, ok := data[provider]
	if !ok {
		slog.Error("Provider not found in models.dev", "provider", provider)
		return nil
	}

	models := make([]*pb.ModelInfo, 0, len(providerData.Models))
	for _, model := range providerData.Models {
		capabilities := []string{"chat", "code"}
		for _, input := range model.Modalities.Input {
			if input == "image" {
				capabilities = append(capabilities, "vision")
				break
			}
		}
		if model.Reasoning {
			capabilities = append(capabilities, "reasoning")
		}

		models = append(models, &pb.ModelInfo{
			Id:           model.ID,
			Name:         model.Name,
			Provider:     strPtr(providerData.Name),
			Capabilities: capabilities,
		})
	}

	slog.Info("Fetched models from models.dev", "provider", provider, "count", len(models))
	return models
}

// fetchOpenCodeModels fetches models for OpenCode SDK based on configured API keys
// Returns models with provider-prefixed IDs (e.g., "anthropic/claude-sonnet-4-0", "mistral/mistral-large-2512")
func (m *Manager) fetchOpenCodeModels() []*pb.ModelInfo {
	slog.Info("fetchOpenCodeModels called",
		"hasAnthropicKey", m.config.AnthropicAPIKey != "",
		"hasOpenAIKey", m.config.OpenAIAPIKey != "",
		"hasMistralKey", m.config.MistralAPIKey != "",
		"hasOpenCodeKey", m.config.OpenCodeAPIKey != "",
		"hasZaiKey", m.config.ZaiAPIKey != "")

	var models []*pb.ModelInfo

	// Add OpenCode Zen models (always available - free models if no key, all models if key is set)
	zenModels := m.fetchZenModels()
	slog.Info("Fetched OpenCode Zen models", "count", len(zenModels))
	models = append(models, zenModels...)

	// Add Anthropic models if ANTHROPIC_API_KEY is set
	if m.config.AnthropicAPIKey != "" {
		anthropicModels := m.fetchOpenCodeModelsForProvider("anthropic")
		slog.Info("Fetched OpenCode Anthropic models", "count", len(anthropicModels))
		models = append(models, anthropicModels...)
	}

	// Add OpenAI models if OPENAI_API_KEY is set
	if m.config.OpenAIAPIKey != "" {
		openaiModels := m.fetchOpenCodeModelsForProvider("openai")
		slog.Info("Fetched OpenCode OpenAI models", "count", len(openaiModels))
		models = append(models, openaiModels...)
	}

	// Add Mistral models if MISTRAL_API_KEY is set
	if m.config.MistralAPIKey != "" {
		mistralModels := m.fetchOpenCodeModelsForProvider("mistral")
		slog.Info("Fetched OpenCode Mistral models", "count", len(mistralModels))
		models = append(models, mistralModels...)
	}

	// Add Ollama models if OLLAMA_URL is set
	if m.config.OllamaURL != "" {
		ollamaModels := m.fetchOllamaModels()
		slog.Info("Fetched OpenCode Ollama models", "count", len(ollamaModels))
		models = append(models, ollamaModels...)
	}

	// Add Z.AI models if ZAI_API_KEY is set
	if m.config.ZaiAPIKey != "" {
		zaiModels := m.fetchOpenCodeModelsForProvider("zai")
		slog.Info("Fetched OpenCode Z.AI models", "count", len(zaiModels))
		models = append(models, zaiModels...)
	}

	if len(models) == 0 {
		slog.Warn("No OpenCode credentials configured (need ANTHROPIC_API_KEY, OPENAI_API_KEY, MISTRAL_API_KEY, OPENCODE_API_KEY, ZAI_API_KEY, or OLLAMA_URL)")
	}

	slog.Info("fetchOpenCodeModels returning", "totalModels", len(models))
	return models
}

// fetchOpenCodeModelsForProvider fetches models for a specific provider with provider-prefixed IDs
func (m *Manager) fetchOpenCodeModelsForProvider(provider string) []*pb.ModelInfo {
	baseModels := m.fetchModelsFromModelsDev(provider)
	if baseModels == nil {
		return nil
	}

	// Thinking levels for Claude models that support reasoning
	thinkingLevels := []struct {
		suffix string
		label  string
	}{
		{"high", "High"},
		{"max", "Max"},
	}

	// Prefix model IDs with provider name for OpenCode SDK format
	models := make([]*pb.ModelInfo, 0, len(baseModels))
	for _, model := range baseModels {
		baseID := provider + "/" + model.Id

		// Check if model supports reasoning (extended thinking)
		supportsThinking := false
		for _, cap := range model.Capabilities {
			if cap == "reasoning" {
				supportsThinking = true
				break
			}
		}

		if supportsThinking && provider == "anthropic" {
			// For Claude models with reasoning, generate variants:
			// 1. Base model without thinking
			models = append(models, &pb.ModelInfo{
				Id:           baseID,
				Name:         model.Name,
				Provider:     model.Provider,
				Capabilities: model.Capabilities,
			})
			// 2. Variants with thinking levels (high, max)
			for _, level := range thinkingLevels {
				models = append(models, &pb.ModelInfo{
					Id:              baseID + ":" + level.suffix,
					Name:            model.Name,
					Provider:        model.Provider,
					Capabilities:    model.Capabilities,
					ReasoningEffort: strPtr(level.label),
				})
			}
		} else {
			// Non-thinking models: just add with provider prefix
			models = append(models, &pb.ModelInfo{
				Id:           baseID,
				Name:         model.Name,
				Provider:     model.Provider,
				Capabilities: model.Capabilities,
			})
		}
	}

	return models
}

// fetchZenModels fetches models from the OpenCode Zen provider
// If OPENCODE_API_KEY is not set, only returns free models (cost.input === 0)
func (m *Manager) fetchZenModels() []*pb.ModelInfo {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		slog.Error("Failed to fetch from models.dev for Zen", "error", err)
		return m.getZenModelsFallback()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("models.dev API error for Zen", "status", resp.StatusCode)
		return m.getZenModelsFallback()
	}

	var data modelsDevResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Error("Failed to decode models.dev response for Zen", "error", err)
		return m.getZenModelsFallback()
	}

	zenData, ok := data["opencode"]
	if !ok {
		slog.Warn("OpenCode provider not found in models.dev")
		return m.getZenModelsFallback()
	}

	hasAPIKey := m.config.OpenCodeAPIKey != ""
	var models []*pb.ModelInfo

	for _, model := range zenData.Models {
		// If no API key, only include free models (cost.input == 0 and cost.output == 0)
		isFree := model.Cost.Input == 0 && model.Cost.Output == 0
		if !hasAPIKey && !isFree {
			continue
		}

		capabilities := []string{"chat", "code"}
		for _, input := range model.Modalities.Input {
			if input == "image" {
				capabilities = append(capabilities, "vision")
				break
			}
		}
		if model.Reasoning {
			capabilities = append(capabilities, "reasoning")
		}

		// Zen models use "opencode/" prefix
		models = append(models, &pb.ModelInfo{
			Id:           "opencode/" + model.ID,
			Name:         model.Name,
			Provider:     strPtr("OpenCode Zen"),
			Capabilities: capabilities,
		})
	}

	slog.Info("Fetched Zen models", "total", len(models), "hasAPIKey", hasAPIKey)
	return models
}

// getZenModelsFallback returns fallback free models when API is unavailable
func (m *Manager) getZenModelsFallback() []*pb.ModelInfo {
	// Only return free models as fallback (always available without API key)
	return []*pb.ModelInfo{
		{Id: "opencode/gpt-5-nano", Name: "GPT-5 Nano", Provider: strPtr("OpenCode Zen"), Capabilities: []string{"chat", "vision", "code", "reasoning"}},
		{Id: "opencode/big-pickle", Name: "Big Pickle", Provider: strPtr("OpenCode Zen"), Capabilities: []string{"chat", "code", "reasoning"}},
		{Id: "opencode/glm-4.7-free", Name: "GLM-4.7 Free", Provider: strPtr("OpenCode Zen"), Capabilities: []string{"chat", "code", "reasoning"}},
		{Id: "opencode/minimax-m2.1-free", Name: "MiniMax M2.1 Free", Provider: strPtr("OpenCode Zen"), Capabilities: []string{"chat", "code", "reasoning"}},
	}
}

// ollamaTagsResponse represents the response from Ollama /api/tags endpoint
type ollamaTagsResponse struct {
	Models []ollamaModel `json:"models"`
}

type ollamaModel struct {
	Name       string       `json:"name"`
	Model      string       `json:"model"`
	Size       int64        `json:"size"`
	ModifiedAt string       `json:"modified_at"`
	Details    ollamaDetail `json:"details"`
}

type ollamaDetail struct {
	Family            string `json:"family"`
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
}

// fetchOllamaModels fetches models from the configured Ollama instance
func (m *Manager) fetchOllamaModels() []*pb.ModelInfo {
	if m.config.OllamaURL == "" {
		return nil
	}

	url := strings.TrimSuffix(m.config.OllamaURL, "/") + "/api/tags"
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		slog.Error("Failed to fetch from Ollama", "url", url, "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("Ollama API error", "status", resp.StatusCode)
		return nil
	}

	var data ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Error("Failed to decode Ollama response", "error", err)
		return nil
	}

	models := make([]*pb.ModelInfo, 0, len(data.Models))
	for _, model := range data.Models {
		// Extract display name (remove tag suffix for cleaner display)
		displayName := model.Name
		if model.Details.ParameterSize != "" {
			displayName = model.Name + " (" + model.Details.ParameterSize + ")"
		}

		// All Ollama models are downloaded (they come from /api/tags)
		downloaded := true

		models = append(models, &pb.ModelInfo{
			Id:           "ollama/" + model.Name,
			Name:         displayName,
			Provider:     strPtr("Ollama"),
			Capabilities: []string{"chat", "code"},
			Downloaded:   &downloaded,
			SizeBytes:    &model.Size,
		})
	}

	slog.Info("Fetched models from Ollama", "url", url, "count", len(models))
	return models
}

// fetchCopilotModels fetches combined GitHub Copilot + Anthropic models
func (m *Manager) fetchCopilotModels() []*pb.ModelInfo {
	// Check which credentials are available
	hasGitHubCopilot := m.config.GitHubCopilotToken != ""
	hasAnthropicKey := m.config.AnthropicAPIKey != ""

	// If neither credential is available, return empty list
	if !hasGitHubCopilot && !hasAnthropicKey {
		slog.Warn("No Copilot credentials configured (need GITHUB_COPILOT_TOKEN or ANTHROPIC_API_KEY)")
		return nil
	}

	// Fetch from models.dev - get both github-copilot and anthropic providers
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		slog.Error("Failed to fetch from models.dev", "error", err)
		return m.getCopilotModelsFallback()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("models.dev API error", "status", resp.StatusCode)
		return m.getCopilotModelsFallback()
	}

	var data modelsDevResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Error("Failed to decode models.dev response", "error", err)
		return m.getCopilotModelsFallback()
	}

	var models []*pb.ModelInfo

	// Add GitHub Copilot models
	if hasGitHubCopilot {
		if copilotData, ok := data["github-copilot"]; ok {
			for _, model := range copilotData.Models {
				capabilities := []string{"chat", "code"}
				for _, input := range model.Modalities.Input {
					if input == "image" {
						capabilities = append(capabilities, "vision")
						break
					}
				}
				if model.Reasoning {
					capabilities = append(capabilities, "reasoning")
				}

				models = append(models, &pb.ModelInfo{
					Id:           model.ID,
					Name:         model.Name,
					Provider:     strPtr("Copilot"),
					Capabilities: capabilities,
				})
			}
		}
	}

	// Add Anthropic models for BYOK
	if hasAnthropicKey {
		if anthropicData, ok := data["anthropic"]; ok {
			for _, model := range anthropicData.Models {
				capabilities := []string{"chat", "code"}
				for _, input := range model.Modalities.Input {
					if input == "image" {
						capabilities = append(capabilities, "vision")
						break
					}
				}
				if model.Reasoning {
					capabilities = append(capabilities, "reasoning")
				}

				models = append(models, &pb.ModelInfo{
					Id:           model.ID + ":anthropic",
					Name:         model.Name,
					Provider:     strPtr("Anthropic"),
					Capabilities: capabilities,
				})
			}
		}
	}

	slog.Info("Fetched Copilot models from models.dev", "count", len(models))
	return models
}

// getModelsFallback returns fallback models for a provider when models.dev is unavailable
func getModelsFallback(provider string) []*pb.ModelInfo {
	switch provider {
	case "anthropic":
		return []*pb.ModelInfo{
			{Id: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Provider: strPtr("Anthropic"), Capabilities: []string{"chat", "vision", "code", "reasoning"}},
			{Id: "claude-sonnet-4-0", Name: "Claude Sonnet 4", Provider: strPtr("Anthropic"), Capabilities: []string{"chat", "vision", "code"}},
		}
	case "opencode":
		return []*pb.ModelInfo{
			{Id: "anthropic/claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Provider: strPtr("Anthropic"), Capabilities: []string{"chat", "vision", "code", "reasoning"}},
			{Id: "anthropic/claude-sonnet-4-0", Name: "Claude Sonnet 4", Provider: strPtr("Anthropic"), Capabilities: []string{"chat", "vision", "code"}},
		}
	default:
		return nil
	}
}

// getCopilotModelsFallback returns fallback models when models.dev is unavailable
// Models are filtered based on available credentials
func (m *Manager) getCopilotModelsFallback() []*pb.ModelInfo {
	hasGitHubCopilot := m.config.GitHubCopilotToken != ""
	hasAnthropicKey := m.config.AnthropicAPIKey != ""

	var models []*pb.ModelInfo

	// GitHub Copilot models (require GITHUB_COPILOT_TOKEN)
	if hasGitHubCopilot {
		models = append(models,
			&pb.ModelInfo{Id: "claude-sonnet-4.5", Name: "Claude Sonnet 4.5", Provider: strPtr("Copilot"), Capabilities: []string{"chat", "vision", "code", "reasoning"}},
			&pb.ModelInfo{Id: "claude-sonnet-4", Name: "Claude Sonnet 4", Provider: strPtr("Copilot"), Capabilities: []string{"chat", "vision", "code"}},
			&pb.ModelInfo{Id: "gpt-4o", Name: "GPT-4o", Provider: strPtr("Copilot"), Capabilities: []string{"chat", "vision", "code"}},
			&pb.ModelInfo{Id: "gemini-2.5-pro", Name: "Gemini 2.5 Pro", Provider: strPtr("Copilot"), Capabilities: []string{"chat", "vision", "code"}},
		)
	}

	// Anthropic direct models (require ANTHROPIC_API_KEY)
	if hasAnthropicKey {
		models = append(models,
			&pb.ModelInfo{Id: "claude-sonnet-4-5:anthropic", Name: "Claude Sonnet 4.5", Provider: strPtr("Anthropic"), Capabilities: []string{"chat", "vision", "code", "reasoning"}},
			&pb.ModelInfo{Id: "claude-sonnet-4-0:anthropic", Name: "Claude Sonnet 4", Provider: strPtr("Anthropic"), Capabilities: []string{"chat", "vision", "code"}},
		)
	}

	return models
}

// fetchCodexModels fetches OpenAI Codex models (family: gpt-codex or gpt-codex-mini)
// and returns auth mode suffix variants based on backend-available credentials:
// - ":api" suffix when OPENAI_API_KEY is configured
// - ":oauth" suffix when backend Codex OAuth credentials are configured
// If both are configured, both sets are returned.
func (m *Manager) fetchCodexModels() []*pb.ModelInfo {
	hasAPIKey := m.config.OpenAIAPIKey != ""
	hasOAuth := false
	if data, err := m.storage.GetCodexOAuth(context.Background()); err != nil {
		slog.Warn("Failed to read global Codex OAuth credentials", "error", err)
	} else if data != nil && data.AccessToken != "" && data.IdToken != "" && data.RefreshToken != "" {
		hasOAuth = true
	}

	// If neither is configured, return empty
	if !hasAPIKey && !hasOAuth {
		return nil
	}

	// Fetch base models from models.dev
	baseModels := m.doFetchCodexModels()
	if baseModels == nil {
		baseModels = m.getCodexBaseModels()
	}

	// Reasoning effort levels to generate
	effortLevels := []struct {
		suffix string
		label  string
	}{
		{"low", "Low"},
		{"medium", "Med"},
		{"high", "High"},
		{"xhigh", "xHigh"},
	}

	// Create models with auth mode and reasoning effort suffixes
	// Format: model:auth:effort (e.g., gpt-5-codex:oauth:high)
	var models []*pb.ModelInfo
	for _, base := range baseModels {
		if hasAPIKey {
			for _, effort := range effortLevels {
				models = append(models, &pb.ModelInfo{
					Id:              fmt.Sprintf("%s:api:%s", base.Id, effort.suffix),
					Name:            base.Name,
					Provider:        strPtr("OpenAI"),
					Capabilities:    base.Capabilities,
					ReasoningEffort: strPtr(effort.label),
				})
			}
		}
		if hasOAuth {
			for _, effort := range effortLevels {
				models = append(models, &pb.ModelInfo{
					Id:              fmt.Sprintf("%s:oauth:%s", base.Id, effort.suffix),
					Name:            base.Name,
					Provider:        strPtr("ChatGPT"),
					Capabilities:    base.Capabilities,
					ReasoningEffort: strPtr(effort.label),
				})
			}
		}
	}

	slog.Info("Fetched Codex models", "count", len(models), "hasAPIKey", hasAPIKey, "hasOAuth", hasOAuth)
	return models
}

func (m *Manager) doFetchCodexModels() []*pb.ModelInfo {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		slog.Error("Failed to fetch from models.dev", "error", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("models.dev API error", "status", resp.StatusCode)
		return nil
	}

	var data modelsDevResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Error("Failed to decode models.dev response", "error", err)
		return nil
	}

	openaiData, ok := data["openai"]
	if !ok {
		slog.Error("OpenAI provider not found in models.dev")
		return nil
	}

	var models []*pb.ModelInfo
	for _, model := range openaiData.Models {
		// Filter to only include Codex models (family contains "gpt-codex")
		if !strings.Contains(model.Family, "gpt-codex") {
			continue
		}

		capabilities := []string{"chat", "code"}
		for _, input := range model.Modalities.Input {
			if input == "image" {
				capabilities = append(capabilities, "vision")
				break
			}
		}
		if model.Reasoning {
			capabilities = append(capabilities, "reasoning")
		}

		models = append(models, &pb.ModelInfo{
			Id:           model.ID,
			Name:         model.Name,
			Provider:     strPtr("OpenAI"),
			Capabilities: capabilities,
		})
	}

	slog.Info("Fetched Codex models from models.dev", "count", len(models))
	return models
}

// getCodexBaseModels returns fallback base Codex models when models.dev is unavailable
// These are base models without auth mode suffix - the suffix is added by fetchCodexModels
func (m *Manager) getCodexBaseModels() []*pb.ModelInfo {
	return []*pb.ModelInfo{
		{Id: "gpt-5.2-codex", Name: "GPT-5.2 Codex", Provider: strPtr("OpenAI"), Capabilities: []string{"chat", "code", "reasoning"}},
		{Id: "gpt-5.1-codex", Name: "GPT-5.1 Codex", Provider: strPtr("OpenAI"), Capabilities: []string{"chat", "code", "reasoning"}},
		{Id: "codex-mini-latest", Name: "Codex Mini", Provider: strPtr("OpenAI"), Capabilities: []string{"chat", "code"}},
	}
}

// ============================================================================
// Snapshot Operations
// ============================================================================

// CreateSnapshot creates a snapshot of the session's PVC using Kubernetes VolumeSnapshots.
func (m *Manager) CreateSnapshot(ctx context.Context, sessionID string, name string) (*pb.Snapshot, error) {
	state := m.getState(sessionID)
	if state == nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	// Get message count for the snapshot
	msgCount, _ := m.storage.GetMessageCount(ctx, sessionID)

	// Count turns by counting MESSAGE events (approximate)
	// We look at event entries to count how many complete messages exist
	entries, _ := m.storage.GetStreamEntriesByTypes(ctx, sessionID, "0", 0, []string{storage.StreamEntryTypeEvent})
	// Filter for MESSAGE kind events and count
	messageEventCount := 0
	for _, e := range entries {
		if e.Entry.Partial {
			continue // Skip partial (streaming) entries
		}
		var event pb.AgentEvent
		if err := json.Unmarshal(e.Entry.Payload, &event); err == nil {
			if event.Kind == pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE {
				messageEventCount++
			}
		}
	}
	turnNumber := messageEventCount / 2 // Rough approximation: each turn has user + assistant message

	// Get current stream ID for snapshot point
	streamID, _ := m.storage.GetLastStreamID(ctx, sessionID)

	// Generate snapshot ID
	snapshotID := uuid.NewString()[:12]

	// Create K8s VolumeSnapshot
	if err := m.k8s.CreateVolumeSnapshot(ctx, sessionID, snapshotID); err != nil {
		return nil, fmt.Errorf("failed to create volume snapshot: %w", err)
	}

	// Wait for snapshot to be ready (with timeout)
	if err := m.k8s.WaitForSnapshotReady(ctx, sessionID, snapshotID, 60*time.Second); err != nil {
		// Cleanup failed snapshot
		_ = m.k8s.DeleteVolumeSnapshot(ctx, sessionID, snapshotID)
		return nil, fmt.Errorf("snapshot not ready: %w", err)
	}

	// Create snapshot record
	snapshot := &pb.Snapshot{
		Id:           snapshotID,
		SessionId:    sessionID,
		Name:         name,
		CreatedAt:    timestamppb.Now(),
		SizeBytes:    0, // K8s VolumeSnapshots don't report size synchronously
		TurnNumber:   int32(turnNumber),
		MessageCount: int32(msgCount),
		StreamId:     streamID,
	}

	// Save to storage
	if err := m.storage.SaveSnapshot(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to save snapshot: %w", err)
	}

	slog.Info("Snapshot created", "sessionID", sessionID, "snapshotID", snapshotID, "name", name)
	return snapshot, nil
}

// RestoreSnapshot restores a session from a snapshot.
// This involves:
// 1. Setting session status to "restoring"
// 2. Deleting the sandbox/pod and old PVC
// 3. Creating new PVC from the VolumeSnapshot
// 4. Truncating messages/events in Redis
// 5. Session will be recreated when client reconnects
func (m *Manager) RestoreSnapshot(ctx context.Context, sessionID, snapshotID string) (int32, error) {
	state := m.getState(sessionID)
	if state == nil {
		return 0, fmt.Errorf("session %s not found", sessionID)
	}

	// Must be ready or interrupted (not running) to restore
	if state.Session.Status != pb.SessionStatus_SESSION_STATUS_READY &&
		state.Session.Status != pb.SessionStatus_SESSION_STATUS_INTERRUPTED {
		return 0, fmt.Errorf("cannot restore while session is %s", state.Session.Status.String())
	}

	// Get snapshot metadata
	snapshot, err := m.storage.GetSnapshot(ctx, sessionID, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("snapshot not found: %w", err)
	}

	slog.Info("Starting restore from snapshot", "sessionID", sessionID, "snapshotID", snapshotID)

	// Update session status to indicate restore in progress
	state.Session.Status = pb.SessionStatus_SESSION_STATUS_PAUSED
	_ = m.storage.UpdateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_PAUSED)

	// Disconnect the agent (if connected)
	m.mu.Lock()
	if _, ok := m.agents[sessionID]; ok {
		delete(m.agents, sessionID)
		// The agent connection will be closed when the pod is deleted
	}
	m.mu.Unlock()

	// Clean up K8s resources (sandbox, PVC) - returns old PVC name for cleanup after restore
	oldPVCName, err := m.k8s.RestoreFromSnapshot(ctx, sessionID, snapshotID)
	if err != nil {
		slog.Error("Restore cleanup failed", "sessionID", sessionID, "snapshotID", snapshotID, "error", err)
		state.Session.Status = pb.SessionStatus_SESSION_STATUS_READY
		_ = m.storage.UpdateSessionStatus(ctx, sessionID, pb.SessionStatus_SESSION_STATUS_READY)
		return 0, fmt.Errorf("restore failed: %w", err)
	}

	// Store snapshot ID for next sandbox creation (bypasses warm pool)
	state.RestoreSnapshotID = snapshotID
	if err := m.storage.SetRestoreSnapshotID(ctx, sessionID, snapshotID); err != nil {
		slog.Warn("Failed to persist restore snapshot ID", "sessionID", sessionID, "error", err)
	}

	// Store old PVC name so it can be deleted after restore completes
	if oldPVCName != "" {
		if err := m.storage.SetOldPVCName(ctx, sessionID, oldPVCName); err != nil {
			slog.Warn("Failed to persist old PVC name", "sessionID", sessionID, "pvc", oldPVCName, "error", err)
		}
	}

	// Truncate the unified stream to the snapshot point
	if err := m.storage.TruncateStreamAfter(ctx, sessionID, snapshot.StreamId); err != nil {
		slog.Warn("Failed to truncate stream", "sessionID", sessionID, "error", err)
	}

	// Reset message counter to snapshot value
	if err := m.storage.SetMessageCount(ctx, sessionID, int(snapshot.MessageCount)); err != nil {
		slog.Warn("Failed to reset message count", "sessionID", sessionID, "error", err)
	}

	// Delete snapshots newer than the restored one (destructive restore)
	m.deleteSnapshotsAfter(ctx, sessionID, snapshot)

	// Delete the K8s VolumeSnapshots that are newer than the restored one
	m.deleteK8sSnapshotsAfter(ctx, sessionID, snapshot)

	slog.Info("Snapshot restored", "sessionID", sessionID, "snapshotID", snapshotID, "messagesRestored", snapshot.MessageCount)

	// Resume immediately to create sandbox from snapshot
	// This runs async so we don't block the restore response
	go func() {
		if _, err := m.Resume(context.Background(), sessionID); err != nil {
			slog.Error("Failed to resume after restore", "sessionID", sessionID, "error", err)
		}
	}()

	return snapshot.MessageCount, nil
}

// ListSnapshots returns all snapshots for a session.
func (m *Manager) ListSnapshots(ctx context.Context, sessionID string) ([]*pb.Snapshot, error) {
	return m.storage.ListSnapshots(ctx, sessionID)
}

// AutoSnapshot creates an auto-snapshot after an agent turn completes.
// This runs asynchronously and doesn't block the main flow.
func (m *Manager) AutoSnapshot(ctx context.Context, sessionID string, turnNumber int, promptSummary string) {
	// Truncate prompt for name
	name := promptSummary
	if len(name) > 50 {
		name = name[:47] + "..."
	}
	name = fmt.Sprintf("Turn %d: %s", turnNumber, name)

	snapshot, err := m.CreateSnapshot(ctx, sessionID, name)
	if err != nil {
		slog.Warn("Auto-snapshot failed", "sessionID", sessionID, "error", err)
		return
	}

	// Enforce retention (keep last 10 snapshots)
	m.enforceSnapshotRetention(ctx, sessionID, 10)

	// Emit snapshot created notification to clients
	m.emitSnapshotCreated(ctx, sessionID, snapshot)

	slog.Info("Auto-snapshot created", "sessionID", sessionID, "snapshotID", snapshot.Id, "turn", turnNumber)
}

// enforceSnapshotRetention deletes old snapshots beyond the limit.
func (m *Manager) enforceSnapshotRetention(ctx context.Context, sessionID string, maxSnapshots int) {
	snapshots, err := m.storage.ListSnapshots(ctx, sessionID)
	if err != nil || len(snapshots) <= maxSnapshots {
		return
	}

	// Delete oldest snapshots beyond limit (list is already newest-first)
	for i := maxSnapshots; i < len(snapshots); i++ {
		if err := m.storage.DeleteSnapshot(ctx, sessionID, snapshots[i].Id); err != nil {
			slog.Warn("Failed to delete old snapshot", "sessionID", sessionID, "snapshotID", snapshots[i].Id, "error", err)
		} else {
			slog.Debug("Deleted old snapshot", "sessionID", sessionID, "snapshotID", snapshots[i].Id)
		}
	}
}

// deleteSnapshotsAfter deletes all snapshots created after the given snapshot (destructive restore).
func (m *Manager) deleteSnapshotsAfter(ctx context.Context, sessionID string, restoredSnapshot *pb.Snapshot) {
	snapshots, err := m.storage.ListSnapshots(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to list snapshots for cleanup", "sessionID", sessionID, "error", err)
		return
	}

	restoredTime := restoredSnapshot.CreatedAt.AsTime()
	deleted := 0

	// Delete snapshots newer than the restored one (list is newest-first)
	for _, s := range snapshots {
		if s.CreatedAt.AsTime().After(restoredTime) {
			if err := m.storage.DeleteSnapshot(ctx, sessionID, s.Id); err != nil {
				slog.Warn("Failed to delete newer snapshot", "sessionID", sessionID, "snapshotID", s.Id, "error", err)
			} else {
				deleted++
				slog.Debug("Deleted newer snapshot", "sessionID", sessionID, "snapshotID", s.Id)
			}
		}
	}

	if deleted > 0 {
		slog.Info("Deleted snapshots after restore point", "sessionID", sessionID, "count", deleted)
		// Emit updated snapshot list to clients
		m.emitSnapshotList(ctx, sessionID)
	}
}

// deleteK8sSnapshotsAfter deletes K8s VolumeSnapshots created after the restored snapshot.
func (m *Manager) deleteK8sSnapshotsAfter(ctx context.Context, sessionID string, restoredSnapshot *pb.Snapshot) {
	snapshots, err := m.storage.ListSnapshots(ctx, sessionID)
	if err != nil {
		slog.Warn("Failed to list snapshots for K8s cleanup", "sessionID", sessionID, "error", err)
		return
	}

	restoredTime := restoredSnapshot.CreatedAt.AsTime()

	for _, s := range snapshots {
		if s.CreatedAt.AsTime().After(restoredTime) {
			if err := m.k8s.DeleteVolumeSnapshot(ctx, sessionID, s.Id); err != nil {
				slog.Warn("Failed to delete K8s VolumeSnapshot", "sessionID", sessionID, "snapshotID", s.Id, "error", err)
			} else {
				slog.Debug("Deleted K8s VolumeSnapshot", "sessionID", sessionID, "snapshotID", s.Id)
			}
		}
	}
}

// emitSnapshotList broadcasts updated snapshot list to clients.
func (m *Manager) emitSnapshotList(ctx context.Context, sessionID string) {
	snapshots, err := m.storage.ListSnapshots(ctx, sessionID)
	if err != nil {
		return
	}
	payload, _ := protoJsonOpts.Marshal(&pb.SnapshotListResponse{
		SessionId: sessionID,
		Snapshots: snapshots,
	})
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      "snapshot_list",
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// emitSnapshotCreated broadcasts snapshot created event to clients.
func (m *Manager) emitSnapshotCreated(ctx context.Context, sessionID string, snapshot *pb.Snapshot) {
	payload, _ := protoJsonOpts.Marshal(&pb.SnapshotCreatedResponse{
		SessionId: sessionID,
		Snapshot:  snapshot,
	})
	m.publishStreamEntry(ctx, sessionID, &storage.StreamEntry{
		Type:      "snapshot_created",
		Payload:   payload,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// copilotUserResponse represents the GitHub Copilot internal API response.
type copilotUserResponse struct {
	QuotaResetDateUTC string                      `json:"quota_reset_date_utc"`
	QuotaSnapshots    map[string]copilotQuotaSnap `json:"quota_snapshots"`
}

type copilotQuotaSnap struct {
	Entitlement int32 `json:"entitlement"`
	Remaining   int32 `json:"remaining"`
	Unlimited   bool  `json:"unlimited"`
}

// GetCopilotStatus returns GitHub Copilot authentication status and quota.
func (m *Manager) GetCopilotStatus(ctx context.Context) *pb.CopilotStatusResponse {
	hasGitHubCopilotToken := m.config.GitHubCopilotToken != ""

	resp := &pb.CopilotStatusResponse{
		Auth: &pb.CopilotAuthStatus{
			IsAuthenticated: hasGitHubCopilotToken,
			AuthType:        strPtr("env"),
		},
	}

	if !hasGitHubCopilotToken {
		return resp
	}

	// Fetch quota from GitHub's internal API
	quota, err := m.fetchCopilotQuota(ctx)
	if err != nil {
		slog.Warn("failed to fetch Copilot quota", "error", err)
		return resp
	}
	resp.Quota = quota

	return resp
}

// fetchCopilotQuota calls the GitHub Copilot internal API to get quota info.
func (m *Manager) fetchCopilotQuota(ctx context.Context) (*pb.CopilotPremiumQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/copilot_internal/user", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.config.GitHubCopilotToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", httpResp.StatusCode)
	}

	var data copilotUserResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	premium, ok := data.QuotaSnapshots["premium_interactions"]
	if !ok {
		return nil, fmt.Errorf("premium_interactions not found in response")
	}

	// If unlimited, don't return quota (it's not meaningful)
	if premium.Unlimited {
		return nil, nil
	}

	used := premium.Entitlement - premium.Remaining
	quota := &pb.CopilotPremiumQuota{
		Used:      used,
		Limit:     premium.Entitlement,
		Remaining: premium.Remaining,
	}
	if data.QuotaResetDateUTC != "" {
		quota.ResetAt = &data.QuotaResetDateUTC
	}

	return quota, nil
}

// Helper for optional string pointers
func strPtr(s string) *string {
	return &s
}

// GetResourceLimits returns the maximum allowed sandbox resources per session.
func (m *Manager) GetResourceLimits() *pb.ResourceLimitsResponse {
	return &pb.ResourceLimitsResponse{
		MaxVcpus:        int32(m.config.MaxSessionCPUs()),
		MaxMemoryMb:     int32(m.config.MaxSessionMemoryMB()),
		DefaultVcpus:    int32(m.config.DefaultCPUs),
		DefaultMemoryMb: int32(m.config.DefaultMemoryMB),
	}
}
