package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/config"
	"github.com/angristan/netclode/services/control-plane/internal/k8s"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// mockRuntime implements k8s.Runtime for testing
type mockRuntime struct {
	mu               sync.Mutex
	sandboxes        map[string]*k8s.SandboxStatusInfo
	deletedSandboxes []string
	deletedClaims    []string
	deletedServices  []string
	deletedSecrets   []string
	createdSandboxes []string
	createdClaims    []string
	createdServices  []string
	exposedPorts     map[string]map[int]bool
	labeledSandboxes map[string]string // sandboxName -> sessionID
	readyCallbacks   map[string][]k8s.SandboxReadyCallback
	previewHostname  string
	previewHostErr   error
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{
		sandboxes:        make(map[string]*k8s.SandboxStatusInfo),
		exposedPorts:     make(map[string]map[int]bool),
		labeledSandboxes: make(map[string]string),
		readyCallbacks:   make(map[string][]k8s.SandboxReadyCallback),
	}
}

func (m *mockRuntime) CreateSandbox(ctx context.Context, sessionID string, env map[string]string, resources *k8s.SandboxResourceConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createdSandboxes = append(m.createdSandboxes, sessionID)
	m.sandboxes[sessionID] = &k8s.SandboxStatusInfo{Exists: true, Ready: true, ServiceFQDN: "test.local"}
	return nil
}

func (m *mockRuntime) WaitForReady(ctx context.Context, sessionID string, timeout time.Duration) (string, error) {
	return "test.local", nil
}

func (m *mockRuntime) WatchSandboxReady(sessionID string, callback k8s.SandboxReadyCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readyCallbacks[sessionID] = append(m.readyCallbacks[sessionID], callback)
}

func (m *mockRuntime) GetStatus(ctx context.Context, sessionID string) (*k8s.SandboxStatusInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if status, ok := m.sandboxes[sessionID]; ok {
		return status, nil
	}
	return &k8s.SandboxStatusInfo{Exists: false}, nil
}

func (m *mockRuntime) DeleteSandbox(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedSandboxes = append(m.deletedSandboxes, sessionID)
	delete(m.sandboxes, sessionID)
	return nil
}

func (m *mockRuntime) DeletePVC(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockRuntime) DeleteSecret(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedSecrets = append(m.deletedSecrets, sessionID)
	return nil
}

func (m *mockRuntime) ListSandboxes(ctx context.Context) ([]k8s.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []k8s.SandboxInfo
	for id, status := range m.sandboxes {
		result = append(result, k8s.SandboxInfo{
			SessionID:   id,
			Ready:       status.Ready,
			ServiceFQDN: status.ServiceFQDN,
		})
	}
	return result, nil
}

func (m *mockRuntime) CreateSandboxClaim(ctx context.Context, sessionID string, templateName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createdClaims = append(m.createdClaims, sessionID)
	return nil
}

func (m *mockRuntime) DeleteSandboxClaim(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedClaims = append(m.deletedClaims, sessionID)
	return nil
}

func (m *mockRuntime) CreateSandboxService(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createdServices = append(m.createdServices, sessionID)
	return nil
}

func (m *mockRuntime) DeleteSandboxService(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedServices = append(m.deletedServices, sessionID)
	return nil
}

func (m *mockRuntime) ListTailscaleServices(ctx context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockRuntime) ExposePort(ctx context.Context, sessionID string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ports, exists := m.exposedPorts[sessionID]
	if !exists {
		ports = make(map[int]bool)
		m.exposedPorts[sessionID] = ports
	}
	ports[port] = true
	return nil
}

func (m *mockRuntime) UnexposePort(ctx context.Context, sessionID string, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ports, exists := m.exposedPorts[sessionID]; exists {
		delete(ports, port)
	}
	return nil
}

func (m *mockRuntime) GetSandboxPreviewHostname(ctx context.Context, sessionID string) (string, error) {
	if m.previewHostname != "" {
		return m.previewHostname, m.previewHostErr
	}
	return "sandbox-" + sessionID, m.previewHostErr
}

func (m *mockRuntime) CreateOrLabelPoolSandbox(ctx context.Context, sessionID string, fromPool bool, env map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createdSandboxes = append(m.createdSandboxes, sessionID)
	m.sandboxes[sessionID] = &k8s.SandboxStatusInfo{Exists: true, Ready: true, ServiceFQDN: "test.local"}
	return nil
}

func (m *mockRuntime) GetWarmPool(ctx context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockRuntime) EvictWarmPool(ctx context.Context) error {
	return nil
}

func (m *mockRuntime) WaitForClaimBound(ctx context.Context, sessionID string, timeout time.Duration) (string, error) {
	return "sandbox-" + sessionID, nil
}

func (m *mockRuntime) GetSandboxByName(ctx context.Context, name string) (*k8s.Sandbox, error) {
	return nil, nil
}

func (m *mockRuntime) GetSessionIDByPodName(ctx context.Context, podName string) (string, error) {
	return "", nil
}

func (m *mockRuntime) LabelSandbox(ctx context.Context, sandboxName string, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.labeledSandboxes[sandboxName] = sessionID
	return nil
}

func (m *mockRuntime) ListSandboxClaims(ctx context.Context) ([]k8s.SandboxClaimInfo, error) {
	return nil, nil
}

func (m *mockRuntime) CreateVolumeSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	return nil
}

func (m *mockRuntime) WaitForSnapshotReady(ctx context.Context, sessionID, snapshotID string, timeout time.Duration) error {
	return nil
}

func (m *mockRuntime) DeleteVolumeSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	return nil
}

func (m *mockRuntime) ListVolumeSnapshots(ctx context.Context, sessionID string) ([]k8s.VolumeSnapshotInfo, error) {
	return nil, nil
}

func (m *mockRuntime) RestoreFromSnapshot(ctx context.Context, sessionID, snapshotID string) (string, error) {
	return "old-pvc-name", nil
}

func (m *mockRuntime) DeletePVCByName(ctx context.Context, name string) error {
	return nil
}

func (m *mockRuntime) GetPVCName(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

func (m *mockRuntime) CreatePVCFromSnapshot(ctx context.Context, sessionID, snapshotID string) (string, error) {
	return "agent-home-sess-" + sessionID, nil
}

func (m *mockRuntime) WaitForRestoreJob(ctx context.Context, sessionID, snapshotID string, timeout time.Duration) error {
	return nil
}

func (m *mockRuntime) EnsureSessionAnchor(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockRuntime) DeleteSessionAnchor(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockRuntime) AddSessionAnchorToPVC(ctx context.Context, sessionID, pvcName string) error {
	return nil
}

func (m *mockRuntime) ConfigureNetwork(ctx context.Context, sessionID string, networkEnabled bool) error {
	return nil
}

func (m *mockRuntime) ConfigureTailnetAccess(ctx context.Context, sessionID string, tailnetEnabled bool) error {
	return nil
}

func (m *mockRuntime) DeleteNetworkRestriction(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockRuntime) VerifyAgentToken(ctx context.Context, token string, audiences []string) (string, error) {
	// Mock implementation - return a fake pod name for testing
	return "mock-pod-" + token[:8], nil
}

func (m *mockRuntime) GetSessionIDByPodIP(ctx context.Context, podIP string) (string, error) {
	// Mock implementation - return a fake session ID for testing
	return "mock-session-" + podIP, nil
}

func (m *mockRuntime) Close() {
}

// mockStorage implements storage.Storage for testing
type mockStorage struct {
	mu          sync.Mutex
	sessions    map[string]*pb.Session
	streams     map[string][]storage.StreamEntryWithID
	oauth       map[string]*storage.CodexOAuthSessionData
	oauthGlobal *storage.CodexOAuthSessionData
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		sessions: make(map[string]*pb.Session),
		streams:  make(map[string][]storage.StreamEntryWithID),
		oauth:    make(map[string]*storage.CodexOAuthSessionData),
	}
}

func (m *mockStorage) SaveSession(ctx context.Context, s *pb.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.Id] = s
	return nil
}

func (m *mockStorage) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		return s, nil
	}
	return nil, nil
}

func (m *mockStorage) GetAllSessions(ctx context.Context) ([]*pb.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*pb.Session
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result, nil
}

func (m *mockStorage) UpdateSessionStatus(ctx context.Context, id string, status pb.SessionStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.Status = status
	}
	return nil
}

func (m *mockStorage) UpdateSessionField(ctx context.Context, id, field, value string) error {
	return nil
}

func (m *mockStorage) SaveSessionCodexOAuth(ctx context.Context, sessionID string, data *storage.CodexOAuthSessionData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if data == nil {
		delete(m.oauth, sessionID)
		return nil
	}
	c := *data
	m.oauth[sessionID] = &c
	return nil
}

func (m *mockStorage) GetSessionCodexOAuth(ctx context.Context, sessionID string) (*storage.CodexOAuthSessionData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data := m.oauth[sessionID]
	if data == nil {
		return nil, nil
	}
	c := *data
	return &c, nil
}

func (m *mockStorage) DeleteSessionCodexOAuth(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.oauth, sessionID)
	return nil
}

func (m *mockStorage) SaveCodexOAuth(ctx context.Context, data *storage.CodexOAuthSessionData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if data == nil {
		m.oauthGlobal = nil
		return nil
	}
	c := *data
	m.oauthGlobal = &c
	return nil
}

func (m *mockStorage) GetCodexOAuth(ctx context.Context) (*storage.CodexOAuthSessionData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.oauthGlobal == nil {
		return nil, nil
	}
	c := *m.oauthGlobal
	return &c, nil
}

func (m *mockStorage) DeleteCodexOAuth(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.oauthGlobal = nil
	return nil
}

func (m *mockStorage) DeleteSession(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.oauth, id)
	return nil
}

// Unified Stream methods (replaces messages, events, and notifications)
func (m *mockStorage) AppendStreamEntry(ctx context.Context, sessionID string, entry *storage.StreamEntry) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stream := m.streams[sessionID]
	id := strconv.Itoa(len(stream)+1) + "-0"
	entryCopy := *entry
	m.streams[sessionID] = append(stream, storage.StreamEntryWithID{
		ID:    id,
		Entry: &entryCopy,
	})
	return id, nil
}

func (m *mockStorage) GetStreamEntries(ctx context.Context, sessionID string, afterID string, limit int) ([]storage.StreamEntryWithID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterStreamEntriesLocked(sessionID, afterID, limit, nil), nil
}

func (m *mockStorage) GetStreamEntriesByTypes(ctx context.Context, sessionID string, afterID string, limit int, types []string) ([]storage.StreamEntryWithID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterStreamEntriesLocked(sessionID, afterID, limit, types), nil
}

func (m *mockStorage) GetLastStreamID(ctx context.Context, sessionID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stream := m.streams[sessionID]
	if len(stream) == 0 {
		return "0-0", nil
	}
	return stream[len(stream)-1].ID, nil
}

func (m *mockStorage) filterStreamEntriesLocked(sessionID, afterID string, limit int, types []string) []storage.StreamEntryWithID {
	stream := m.streams[sessionID]
	if len(stream) == 0 {
		return nil
	}

	typeFilter := map[string]bool{}
	for _, t := range types {
		typeFilter[t] = true
	}

	afterSeq := 0
	if afterID != "" && afterID != "0" {
		afterSeq = parseStreamSeq(afterID)
	}

	result := make([]storage.StreamEntryWithID, 0, len(stream))
	for _, e := range stream {
		if parseStreamSeq(e.ID) <= afterSeq {
			continue
		}
		if len(typeFilter) > 0 && !typeFilter[e.Entry.Type] {
			continue
		}
		result = append(result, e)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

func parseStreamSeq(id string) int {
	parts := strings.SplitN(id, "-", 2)
	if len(parts) == 0 {
		return 0
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0
	}
	return n
}

func (m *mockStorage) TruncateStreamAfter(ctx context.Context, sessionID string, afterID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	afterSeq := parseStreamSeq(afterID)
	stream := m.streams[sessionID]
	filtered := stream[:0]
	for _, e := range stream {
		if parseStreamSeq(e.ID) <= afterSeq {
			filtered = append(filtered, e)
		}
	}
	m.streams[sessionID] = filtered
	return nil
}

func (m *mockStorage) GetMessageCount(ctx context.Context, sessionID string) (int, error) {
	return 0, nil
}

func (m *mockStorage) IncrementMessageCount(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockStorage) SetMessageCount(ctx context.Context, sessionID string, count int) error {
	return nil
}

func (m *mockStorage) GetRedisClient() *redis.Client {
	return nil
}

func (m *mockStorage) Close() error {
	return nil
}

// Snapshot methods
func (m *mockStorage) SaveSnapshot(ctx context.Context, s *pb.Snapshot) error {
	return nil
}

func (m *mockStorage) GetSnapshot(ctx context.Context, sessionID, snapshotID string) (*pb.Snapshot, error) {
	return nil, nil
}

func (m *mockStorage) ListSnapshots(ctx context.Context, sessionID string) ([]*pb.Snapshot, error) {
	return nil, nil
}

func (m *mockStorage) DeleteSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	return nil
}

func (m *mockStorage) DeleteAllSnapshots(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockStorage) SetRestoreSnapshotID(ctx context.Context, sessionID, snapshotID string) error {
	return nil
}

func (m *mockStorage) GetRestoreSnapshotID(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

func (m *mockStorage) ClearRestoreSnapshotID(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockStorage) SetOldPVCName(ctx context.Context, sessionID, pvcName string) error {
	return nil
}

func (m *mockStorage) GetOldPVCName(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

func (m *mockStorage) ClearOldPVCName(ctx context.Context, sessionID string) error {
	return nil
}

func (m *mockStorage) SetPVCName(ctx context.Context, sessionID, pvcName string) error {
	return nil
}

func (m *mockStorage) GetPVCName(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

// Helper to create a test manager with mock dependencies
func newTestManager(maxActiveSessions int) (*Manager, *mockRuntime, *mockStorage) {
	runtime := newMockRuntime()
	store := newMockStorage()
	cfg := &config.Config{
		MaxActiveSessions: maxActiveSessions,
		UseWarmPool:       true,
	}

	manager := NewManager(store, runtime, cfg, nil)
	return manager, runtime, store
}

// Helper to add a session directly to manager state
func addSession(m *Manager, id string, status pb.SessionStatus, lastActiveAt time.Time) {
	session := &pb.Session{
		Id:           id,
		Name:         "Session " + id,
		Status:       status,
		CreatedAt:    timestamppb.New(lastActiveAt),
		LastActiveAt: timestamppb.New(lastActiveAt),
	}
	m.sessions[id] = NewSessionState(session)
}

func TestEnsureActiveSlot_NoActionWhenUnderLimit(t *testing.T) {
	manager, runtime, _ := newTestManager(3)

	// Add 2 running sessions (under limit of 3)
	now := time.Now()
	addSession(manager, "sess-1", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-2*time.Hour))
	addSession(manager, "sess-2", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-1*time.Hour))

	// Should not pause anything
	pausedID := manager.ensureActiveSlot(context.Background(), "")

	if pausedID != "" {
		t.Errorf("Expected no session to be paused, got %s", pausedID)
	}

	if len(runtime.deletedSandboxes) > 0 {
		t.Errorf("Expected no sandboxes deleted, got %v", runtime.deletedSandboxes)
	}
}

func TestEnsureActiveSlot_PausesOneWhenAtLimit(t *testing.T) {
	manager, _, _ := newTestManager(2)

	// Add 2 running sessions (at limit)
	now := time.Now()
	addSession(manager, "sess-1", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-2*time.Hour)) // oldest
	addSession(manager, "sess-2", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-1*time.Hour))

	// Should pause the oldest one
	pausedID := manager.ensureActiveSlot(context.Background(), "")

	if pausedID != "sess-1" {
		t.Errorf("Expected sess-1 to be paused, got %s", pausedID)
	}

	// Verify session status changed
	if manager.sessions["sess-1"].Session.Status != pb.SessionStatus_SESSION_STATUS_PAUSED {
		t.Errorf("Expected sess-1 status to be paused, got %s", manager.sessions["sess-1"].Session.Status)
	}
}

func TestEnsureActiveSlot_PausesMultipleWhenOverLimit(t *testing.T) {
	manager, _, _ := newTestManager(2)

	// Add 5 running sessions (way over limit of 2)
	now := time.Now()
	addSession(manager, "sess-1", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-5*time.Hour)) // oldest
	addSession(manager, "sess-2", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-4*time.Hour))
	addSession(manager, "sess-3", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-3*time.Hour))
	addSession(manager, "sess-4", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-2*time.Hour))
	addSession(manager, "sess-5", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-1*time.Hour)) // newest

	// Should pause enough to get under limit (pause 4, leaving 1 active + 1 for new)
	manager.ensureActiveSlot(context.Background(), "")

	// Count active sessions
	activeCount := 0
	for _, state := range manager.sessions {
		if isActiveStatus(state.Session.Status) {
			activeCount++
		}
	}

	// Should have less than MaxActiveSessions (2) active
	if activeCount >= 2 {
		t.Errorf("Expected less than 2 active sessions, got %d", activeCount)
	}

	// Verify oldest sessions were paused first
	if manager.sessions["sess-1"].Session.Status != pb.SessionStatus_SESSION_STATUS_PAUSED {
		t.Errorf("Expected sess-1 (oldest) to be paused")
	}
}

func TestEnsureActiveSlot_ExcludesSpecifiedSession(t *testing.T) {
	manager, _, _ := newTestManager(2)

	// Add 3 running sessions (over limit)
	now := time.Now()
	addSession(manager, "sess-1", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-3*time.Hour)) // oldest
	addSession(manager, "sess-2", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-2*time.Hour))
	addSession(manager, "sess-3", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-1*time.Hour))

	// Exclude sess-1 from being paused AND from count (simulates resuming a zombie session)
	pausedID := manager.ensureActiveSlot(context.Background(), "sess-1")

	// Should pause sess-2 (oldest among non-excluded)
	if pausedID != "sess-2" {
		t.Errorf("Expected sess-2 to be paused (sess-1 excluded), got %s", pausedID)
	}
}

func TestEnsureActiveSlot_NoActionWhenExcludedSessionAtLimit(t *testing.T) {
	manager, _, _ := newTestManager(2)

	// Add 2 running sessions (at limit)
	now := time.Now()
	addSession(manager, "sess-1", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-2*time.Hour))
	addSession(manager, "sess-2", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-1*time.Hour))

	// Exclude sess-1 from count - simulates resuming a zombie session that's already counted as active
	// Since we exclude sess-1, count is 1, which is under limit of 2
	pausedID := manager.ensureActiveSlot(context.Background(), "sess-1")

	// Should NOT pause anything - the excluded session is already using the slot
	if pausedID != "" {
		t.Errorf("Expected no session to be paused when excluded session is at limit, got %s", pausedID)
	}
}

func TestEnsureActiveSlot_NoLimitWhenZero(t *testing.T) {
	manager, runtime, _ := newTestManager(0) // 0 = unlimited

	// Add many running sessions
	now := time.Now()
	for i := 0; i < 10; i++ {
		addSession(manager, "sess-"+string(rune('A'+i)), pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(time.Duration(-i)*time.Hour))
	}

	// Should not pause anything
	pausedID := manager.ensureActiveSlot(context.Background(), "")

	if pausedID != "" {
		t.Errorf("Expected no session to be paused with limit=0, got %s", pausedID)
	}

	if len(runtime.deletedSandboxes) > 0 {
		t.Errorf("Expected no sandboxes deleted with limit=0, got %v", runtime.deletedSandboxes)
	}
}

func TestEnforceActiveLimit_OnStartup(t *testing.T) {
	manager, _, _ := newTestManager(2)

	// Simulate startup state with 4 running sessions
	now := time.Now()
	addSession(manager, "sess-1", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-4*time.Hour))
	addSession(manager, "sess-2", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-3*time.Hour))
	addSession(manager, "sess-3", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-2*time.Hour))
	addSession(manager, "sess-4", pb.SessionStatus_SESSION_STATUS_RUNNING, now.Add(-1*time.Hour))

	// Enforce limit on startup
	manager.enforceActiveLimit(context.Background())

	// Count active sessions
	activeCount := 0
	for _, state := range manager.sessions {
		if isActiveStatus(state.Session.Status) {
			activeCount++
		}
	}

	// Should be at or under the limit
	if activeCount > 2 {
		t.Errorf("Expected at most 2 active sessions, got %d", activeCount)
	}
}

// mockWarmAgentConnection implements WarmAgentConnection for testing
type mockWarmAgentConnection struct {
	assignedSessionID string
	assignedConfig    *AgentSessionConfig
	assignCalled      bool
	assignError       error
	executeCalled     bool
	executeText       string
	executeErr        error
	codexAuthCalled   bool
	codexAccessToken  string
	codexIdToken      string
	codexExpiresAt    *timestamppb.Timestamp
	codexAuthErr      error
}

func (m *mockWarmAgentConnection) ExecutePrompt(text string) error {
	m.executeCalled = true
	m.executeText = text
	return m.executeErr
}

func (m *mockWarmAgentConnection) Interrupt() error {
	return nil
}

func (m *mockWarmAgentConnection) GenerateTitle(requestID, prompt string) error {
	return nil
}

func (m *mockWarmAgentConnection) GetGitStatus(requestID string) error {
	return nil
}

func (m *mockWarmAgentConnection) GetGitDiff(requestID string, file *string) error {
	return nil
}

func (m *mockWarmAgentConnection) SendTerminalInput(data string) error {
	return nil
}

func (m *mockWarmAgentConnection) ResizeTerminal(cols, rows int) error {
	return nil
}

func (m *mockWarmAgentConnection) UpdateGitCredentials(token string, repoAccess pb.RepoAccess) error {
	return nil
}

func (m *mockWarmAgentConnection) UpdateCodexAuth(accessToken, idToken string, expiresAt *timestamppb.Timestamp) error {
	m.codexAuthCalled = true
	m.codexAccessToken = accessToken
	m.codexIdToken = idToken
	m.codexExpiresAt = expiresAt
	return m.codexAuthErr
}

func (m *mockWarmAgentConnection) AssignSession(sessionID string, config *AgentSessionConfig) error {
	m.assignCalled = true
	m.assignedSessionID = sessionID
	m.assignedConfig = config
	return m.assignError
}

func TestRegisterWarmAgentConnection(t *testing.T) {
	manager, _, _ := newTestManager(3)

	conn := &mockWarmAgentConnection{}
	manager.RegisterWarmAgentConnection("pod-1", conn)

	// Verify the connection was registered
	if got := manager.GetWarmAgentConnection("pod-1"); got != conn {
		t.Errorf("Expected warm agent to be registered, got %v", got)
	}
}

func TestUnregisterWarmAgentConnection(t *testing.T) {
	manager, _, _ := newTestManager(3)

	conn := &mockWarmAgentConnection{}
	manager.RegisterWarmAgentConnection("pod-1", conn)
	manager.UnregisterWarmAgentConnection("pod-1")

	// Verify the connection was removed
	if got := manager.GetWarmAgentConnection("pod-1"); got != nil {
		t.Errorf("Expected warm agent to be unregistered, got %v", got)
	}
}

func TestAssignSessionToWarmAgent_Success(t *testing.T) {
	manager, _, _ := newTestManager(3)

	// Create a session first
	now := time.Now()
	addSession(manager, "sess-123", pb.SessionStatus_SESSION_STATUS_CREATING, now)

	// Register a warm agent
	conn := &mockWarmAgentConnection{}
	manager.RegisterWarmAgentConnection("pod-1", conn)

	// Assign the session
	result := manager.AssignSessionToWarmAgent("pod-1", "sess-123")

	if !result {
		t.Error("Expected AssignSessionToWarmAgent to return true")
	}

	if !conn.assignCalled {
		t.Error("Expected AssignSession to be called on the connection")
	}

	if conn.assignedSessionID != "sess-123" {
		t.Errorf("Expected assigned session ID to be 'sess-123', got %s", conn.assignedSessionID)
	}

	// Verify the connection was moved from warmAgents to agents
	if got := manager.GetWarmAgentConnection("pod-1"); got != nil {
		t.Error("Expected warm agent to be removed from warmAgents map")
	}

	if got := manager.GetAgentConnection("sess-123"); got != conn {
		t.Errorf("Expected agent to be in agents map, got %v", got)
	}
}

func TestAssignSessionToWarmAgent_NotFound(t *testing.T) {
	manager, _, _ := newTestManager(3)

	// Try to assign to a non-existent warm agent
	result := manager.AssignSessionToWarmAgent("nonexistent-pod", "sess-123")

	if result {
		t.Error("Expected AssignSessionToWarmAgent to return false for non-existent pod")
	}
}

func TestAssignSessionToWarmAgent_SessionNotFound(t *testing.T) {
	manager, _, _ := newTestManager(3)

	// Register a warm agent but don't create the session
	conn := &mockWarmAgentConnection{}
	manager.RegisterWarmAgentConnection("pod-1", conn)

	// Try to assign a non-existent session
	result := manager.AssignSessionToWarmAgent("pod-1", "nonexistent-session")

	// Should fail because GetSessionConfig will fail
	if result {
		t.Error("Expected AssignSessionToWarmAgent to return false for non-existent session")
	}

	// The warm agent should have been removed from warmAgents but not added to agents
	// since the assignment failed
	if got := manager.GetAgentConnection("nonexistent-session"); got != nil {
		t.Error("Expected agent to NOT be in agents map after failed assignment")
	}
}

func TestMultipleWarmAgents(t *testing.T) {
	manager, _, _ := newTestManager(3)

	// Register multiple warm agents
	conn1 := &mockWarmAgentConnection{}
	conn2 := &mockWarmAgentConnection{}
	conn3 := &mockWarmAgentConnection{}

	manager.RegisterWarmAgentConnection("pod-1", conn1)
	manager.RegisterWarmAgentConnection("pod-2", conn2)
	manager.RegisterWarmAgentConnection("pod-3", conn3)

	// Verify all are registered
	if manager.GetWarmAgentConnection("pod-1") != conn1 {
		t.Error("pod-1 not registered")
	}
	if manager.GetWarmAgentConnection("pod-2") != conn2 {
		t.Error("pod-2 not registered")
	}
	if manager.GetWarmAgentConnection("pod-3") != conn3 {
		t.Error("pod-3 not registered")
	}

	// Create sessions and assign
	now := time.Now()
	addSession(manager, "sess-a", pb.SessionStatus_SESSION_STATUS_CREATING, now)
	addSession(manager, "sess-b", pb.SessionStatus_SESSION_STATUS_CREATING, now)

	manager.AssignSessionToWarmAgent("pod-1", "sess-a")
	manager.AssignSessionToWarmAgent("pod-2", "sess-b")

	// pod-1 and pod-2 should be assigned, pod-3 still waiting
	if manager.GetWarmAgentConnection("pod-1") != nil {
		t.Error("pod-1 should not be in warmAgents after assignment")
	}
	if manager.GetWarmAgentConnection("pod-2") != nil {
		t.Error("pod-2 should not be in warmAgents after assignment")
	}
	if manager.GetWarmAgentConnection("pod-3") != conn3 {
		t.Error("pod-3 should still be in warmAgents")
	}

	if manager.GetAgentConnection("sess-a") != conn1 {
		t.Error("sess-a should be assigned to conn1")
	}
	if manager.GetAgentConnection("sess-b") != conn2 {
		t.Error("sess-b should be assigned to conn2")
	}
}

func TestCreate_CodexOAuthRequiresGlobalCredential(t *testing.T) {
	manager, _, store := newTestManager(3)
	manager.config.UseWarmPool = false

	sdkType := pb.SdkType_SDK_TYPE_CODEX
	model := "gpt-5-codex:oauth:high"
	_, err := manager.Create(context.Background(), "OAuth Session", nil, nil, &sdkType, &model, nil, nil, nil)
	if err == nil {
		t.Fatal("expected Create to fail when global codex oauth is not configured")
	}

	now := time.Now().UTC()
	expiresAt := now.Add(30 * time.Minute)
	if err := store.SaveCodexOAuth(context.Background(), &storage.CodexOAuthSessionData{
		AccessToken:  "access-token",
		IdToken:      "id-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    &expiresAt,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("SaveCodexOAuth failed: %v", err)
	}

	sess, err := manager.Create(context.Background(), "OAuth Session", nil, nil, &sdkType, &model, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if sess == nil || sess.Id == "" {
		t.Fatalf("unexpected session returned: %+v", sess)
	}
}

func TestGetSessionConfig_CodexOAuthOmitsRefreshToken(t *testing.T) {
	manager, _, store := newTestManager(3)
	manager.config.OpenAIAPIKey = "should-not-be-sent-in-oauth-mode"
	sessionID := "sess-oauth-config"
	now := time.Now().UTC()
	model := "gpt-5-codex:oauth:high"
	sdkType := pb.SdkType_SDK_TYPE_CODEX

	session := &pb.Session{
		Id:           sessionID,
		Name:         "OAuth Session",
		Status:       pb.SessionStatus_SESSION_STATUS_READY,
		CreatedAt:    timestamppb.New(now),
		LastActiveAt: timestamppb.New(now),
		SdkType:      &sdkType,
		Model:        &model,
	}
	manager.sessions[sessionID] = NewSessionState(session)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}
	expiresAt := now.Add(20 * time.Minute)
	if err := store.SaveCodexOAuth(context.Background(), &storage.CodexOAuthSessionData{
		AccessToken:  "oauth-access",
		IdToken:      "oauth-id",
		RefreshToken: "oauth-refresh",
		ExpiresAt:    &expiresAt,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("SaveCodexOAuth failed: %v", err)
	}

	cfg, err := manager.GetSessionConfig(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetSessionConfig failed: %v", err)
	}
	if cfg.CodexAccessToken != "oauth-access" || cfg.CodexIdToken != "oauth-id" {
		t.Fatalf("unexpected codex oauth tokens in config: %+v", cfg)
	}
	if cfg.OpenAIAPIKey != "" {
		t.Fatalf("expected OpenAI API key to be omitted in oauth mode, got %q", cfg.OpenAIAPIKey)
	}
}

func TestGetSessionConfig_CodexModelMissingAuthSuffixFails(t *testing.T) {
	manager, _, store := newTestManager(3)
	sessionID := "sess-codex-no-auth-suffix"
	now := time.Now().UTC()
	model := "gpt-5-codex"
	sdkType := pb.SdkType_SDK_TYPE_CODEX

	session := &pb.Session{
		Id:           sessionID,
		Name:         "Codex Session",
		Status:       pb.SessionStatus_SESSION_STATUS_READY,
		CreatedAt:    timestamppb.New(now),
		LastActiveAt: timestamppb.New(now),
		SdkType:      &sdkType,
		Model:        &model,
	}
	manager.sessions[sessionID] = NewSessionState(session)
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	_, err := manager.GetSessionConfig(context.Background(), sessionID)
	if err == nil {
		t.Fatal("expected GetSessionConfig to fail for codex model without auth suffix")
	}
	if !strings.Contains(err.Error(), "missing auth suffix") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendPrompt_CodexOAuthRefreshesAndPushesUpdatedTokens(t *testing.T) {
	manager, _, store := newTestManager(3)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","id_token":"new-id","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", server.URL)

	// Ensure mock storage can persist oauth data with keyless mock implementation.
	manager.config.CodexOAuthEncryptionKey = []byte("0123456789abcdef0123456789abcdef")

	sessionID := "sess-sendprompt-oauth"
	now := time.Now().UTC()
	sdkType := pb.SdkType_SDK_TYPE_CODEX
	model := "gpt-5-codex:oauth:high"
	session := &pb.Session{
		Id:           sessionID,
		Name:         "OAuth Prompt",
		Status:       pb.SessionStatus_SESSION_STATUS_READY,
		CreatedAt:    timestamppb.New(now),
		LastActiveAt: timestamppb.New(now),
		SdkType:      &sdkType,
		Model:        &model,
	}
	state := NewSessionState(session)
	manager.sessions[sessionID] = state
	_ = store.SaveSession(context.Background(), session)

	expiredAt := now.Add(1 * time.Minute)
	_ = store.SaveCodexOAuth(context.Background(), &storage.CodexOAuthSessionData{
		AccessToken:  "old-access",
		IdToken:      "old-id",
		RefreshToken: "old-refresh",
		ExpiresAt:    &expiredAt,
		UpdatedAt:    now,
	})

	conn := &mockWarmAgentConnection{}
	manager.RegisterAgentConnection(sessionID, conn)
	defer manager.UnregisterAgentConnection(sessionID)

	if err := manager.SendPrompt(context.Background(), sessionID, "hello"); err != nil {
		t.Fatalf("SendPrompt failed: %v", err)
	}

	if !conn.codexAuthCalled {
		t.Fatal("expected UpdateCodexAuth to be called")
	}
	if conn.codexAccessToken != "new-access" || conn.codexIdToken != "new-id" {
		t.Fatalf("unexpected updated codex tokens: access=%q id=%q", conn.codexAccessToken, conn.codexIdToken)
	}

	updated, err := store.GetCodexOAuth(context.Background())
	if err != nil {
		t.Fatalf("GetCodexOAuth failed: %v", err)
	}
	if updated == nil || updated.RefreshToken != "new-refresh" {
		b, _ := json.Marshal(updated)
		t.Fatalf("expected refreshed oauth data in storage, got %s", b)
	}
	if !conn.executeCalled {
		t.Fatal("expected ExecutePrompt to be called")
	}
}

func TestStartCodexAuth_RequiresEncryptionKey(t *testing.T) {
	manager, _, _ := newTestManager(3)
	manager.config.CodexOAuthEncryptionKey = nil

	_, err := manager.StartCodexAuth(context.Background())
	if err == nil {
		t.Fatal("expected StartCodexAuth to fail without encryption key")
	}
	if !strings.Contains(err.Error(), "CODEX_OAUTH_ENCRYPTION_KEY_B64") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetAllowedSecretForHost_CodexHasNoProxySecret(t *testing.T) {
	manager, _, _ := newTestManager(3)

	secretKey, placeholder := manager.getAllowedSecretForHost(pb.SdkType_SDK_TYPE_CODEX, "api.openai.com")
	if secretKey != "" || placeholder != "" {
		t.Fatalf("expected no proxy secret mapping for codex, got secretKey=%q placeholder=%q", secretKey, placeholder)
	}
}

func TestHandleAgentResponse_SystemInterruptedSetsReady(t *testing.T) {
	manager, _, _ := newTestManager(3)
	now := time.Now().UTC()
	addSession(manager, "sess-interrupt", pb.SessionStatus_SESSION_STATUS_RUNNING, now)

	state := manager.sessions["sess-interrupt"]
	state.CurrentMessageID = "msg_123"
	state.ContentBuilder.WriteString("partial content")
	state.OriginalPrompt = "long running prompt"

	err := manager.HandleAgentResponse(context.Background(), "sess-interrupt", &pb.AgentStreamResponse{
		Response: &pb.AgentStreamResponse_SystemMessage{
			SystemMessage: &pb.AgentSystemMessage{Message: "interrupted"},
		},
	})
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	if got := manager.sessions["sess-interrupt"].Session.Status; got != pb.SessionStatus_SESSION_STATUS_READY {
		t.Fatalf("expected status READY after interrupt, got %s", got)
	}
	if got := state.CurrentMessageID; got != "" {
		t.Fatalf("expected CurrentMessageID to be reset, got %q", got)
	}
	if got := state.ContentBuilder.String(); got != "" {
		t.Fatalf("expected ContentBuilder to be reset, got %q", got)
	}
	if got := state.OriginalPrompt; got != "" {
		t.Fatalf("expected OriginalPrompt to be reset, got %q", got)
	}
}

func TestHandleAgentResponse_SystemMessageNonInterruptNoStatusChange(t *testing.T) {
	manager, _, _ := newTestManager(3)
	now := time.Now().UTC()
	addSession(manager, "sess-system", pb.SessionStatus_SESSION_STATUS_RUNNING, now)

	err := manager.HandleAgentResponse(context.Background(), "sess-system", &pb.AgentStreamResponse{
		Response: &pb.AgentStreamResponse_SystemMessage{
			SystemMessage: &pb.AgentSystemMessage{Message: "ready"},
		},
	})
	if err != nil {
		t.Fatalf("HandleAgentResponse failed: %v", err)
	}

	if got := manager.sessions["sess-system"].Session.Status; got != pb.SessionStatus_SESSION_STATUS_RUNNING {
		t.Fatalf("expected status to stay RUNNING for non-interrupt system message, got %s", got)
	}
}

func TestExposePort_UsesResolvedPreviewHostname(t *testing.T) {
	manager, runtime, _ := newTestManager(3)
	runtime.previewHostname = "sandbox-test123.dolly-grouse.ts.net"

	previewURL, err := manager.ExposePort(context.Background(), "test123", 1234)
	if err != nil {
		t.Fatalf("ExposePort failed: %v", err)
	}

	expected := "http://sandbox-test123.dolly-grouse.ts.net:1234"
	if previewURL != expected {
		t.Fatalf("expected preview URL %q, got %q", expected, previewURL)
	}
}

func TestExposePort_FallsBackToShortHostnameOnLookupError(t *testing.T) {
	manager, runtime, _ := newTestManager(3)
	runtime.previewHostErr = errors.New("lookup failed")

	previewURL, err := manager.ExposePort(context.Background(), "test123", 1234)
	if err != nil {
		t.Fatalf("ExposePort failed: %v", err)
	}

	expected := "http://sandbox-test123:1234"
	if previewURL != expected {
		t.Fatalf("expected preview URL %q, got %q", expected, previewURL)
	}
}

func TestUnexposePort_RemovesPortAndPersistsEvent(t *testing.T) {
	manager, runtime, _ := newTestManager(3)

	if _, err := manager.ExposePort(context.Background(), "test123", 1234); err != nil {
		t.Fatalf("ExposePort failed: %v", err)
	}
	if err := manager.UnexposePort(context.Background(), "test123", 1234); err != nil {
		t.Fatalf("UnexposePort failed: %v", err)
	}

	runtime.mu.Lock()
	_, stillExposed := runtime.exposedPorts["test123"][1234]
	runtime.mu.Unlock()
	if stillExposed {
		t.Fatalf("expected port 1234 to be removed from runtime state")
	}

	entries, err := manager.storage.GetStreamEntriesByTypes(context.Background(), "test123", "0", 0, []string{storage.StreamEntryTypeEvent})
	if err != nil {
		t.Fatalf("GetStreamEntriesByTypes failed: %v", err)
	}

	foundUnexposed := false
	for _, e := range entries {
		var event pb.AgentEvent
		if err := protojson.Unmarshal(e.Entry.Payload, &event); err != nil {
			continue
		}
		if event.Kind == pb.AgentEventKind_AGENT_EVENT_KIND_PORT_UNEXPOSED {
			if payload := event.GetPortUnexposed(); payload != nil && payload.Port == 1234 {
				foundUnexposed = true
				break
			}
		}
	}

	if !foundUnexposed {
		t.Fatalf("expected persisted port_unexposed event for port 1234")
	}
}

func TestRestoreExposedPorts_AppliesLatestExposeState(t *testing.T) {
	manager, runtime, _ := newTestManager(3)

	if _, err := manager.ExposePort(context.Background(), "test123", 1234); err != nil {
		t.Fatalf("ExposePort(1234) failed: %v", err)
	}
	if _, err := manager.ExposePort(context.Background(), "test123", 5678); err != nil {
		t.Fatalf("ExposePort(5678) failed: %v", err)
	}
	if err := manager.UnexposePort(context.Background(), "test123", 1234); err != nil {
		t.Fatalf("UnexposePort(1234) failed: %v", err)
	}

	// Simulate fresh runtime state before resume restoration.
	runtime.mu.Lock()
	runtime.exposedPorts["test123"] = make(map[int]bool)
	runtime.mu.Unlock()

	manager.restoreExposedPorts(context.Background(), "test123")

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.exposedPorts["test123"][5678] {
		t.Fatalf("expected port 5678 to be restored")
	}
	if runtime.exposedPorts["test123"][1234] {
		t.Fatalf("expected port 1234 to remain unexposed after restoration")
	}
}
