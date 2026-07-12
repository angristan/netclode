package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/config"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func setupTestRedis(t *testing.T) (*miniredis.Miniredis, *RedisStorage) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	cfg := &config.Config{
		RedisURL: "redis://" + mr.Addr(),
	}

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	storage := &RedisStorage{
		client: client,
		config: cfg,
	}

	return mr, storage
}

func TestSaveSessionReposAndAccess(t *testing.T) {
	mr, storage := setupTestRedis(t)
	defer mr.Close()
	defer storage.Close()

	ctx := context.Background()
	repoAccess := pb.RepoAccess_REPO_ACCESS_WRITE
	now := timestamppb.Now()

	session := &pb.Session{
		Id:           "test-session-repos",
		Name:         "Repo Session",
		Owner:        "github-bot",
		Status:       pb.SessionStatus_SESSION_STATUS_READY,
		Repos:        []string{"https://github.com/owner/repo.git", "https://github.com/owner/another.git"},
		RepoAccess:   &repoAccess,
		CreatedAt:    now,
		LastActiveAt: now,
	}

	if err := storage.SaveSession(ctx, session); err != nil {
		t.Fatalf("failed to save session: %v", err)
	}

	loaded, err := storage.GetSession(ctx, session.Id)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if loaded == nil {
		t.Fatalf("expected session, got nil")
	}

	if len(loaded.Repos) != len(session.Repos) {
		t.Fatalf("expected %d repos, got %d", len(session.Repos), len(loaded.Repos))
	}
	for i, repo := range session.Repos {
		if loaded.Repos[i] != repo {
			t.Fatalf("expected repo %q, got %q", repo, loaded.Repos[i])
		}
	}
	if loaded.RepoAccess == nil || *loaded.RepoAccess != repoAccess {
		t.Fatalf("expected repo access %v, got %v", repoAccess, loaded.RepoAccess)
	}
	if loaded.Owner != session.Owner {
		t.Fatalf("expected owner %q, got %q", session.Owner, loaded.Owner)
	}
}

func TestAppendStreamEntry(t *testing.T) {
	mr, storage := setupTestRedis(t)
	defer mr.Close()
	defer storage.Close()

	ctx := context.Background()
	sessionID := "test-session-1"

	entry := &StreamEntry{
		Type:      StreamEntryTypeEvent,
		Payload:   json.RawMessage(`{"kind":"tool_start","tool":"bash"}`),
		Timestamp: time.Now().Format(time.RFC3339),
	}

	// Append entry
	id, err := storage.AppendStreamEntry(ctx, sessionID, entry)
	if err != nil {
		t.Fatalf("failed to append stream entry: %v", err)
	}

	if id == "" {
		t.Error("expected non-empty stream ID")
	}

	// Verify it was written to the stream
	streamKey := StreamKey(sessionID)
	entries, err := storage.client.XRange(ctx, streamKey, "-", "+").Result()
	if err != nil {
		t.Fatalf("failed to read stream: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].ID != id {
		t.Errorf("expected ID %s, got %s", id, entries[0].ID)
	}
}

func TestGetStreamEntries(t *testing.T) {
	mr, storage := setupTestRedis(t)
	defer mr.Close()
	defer storage.Close()

	ctx := context.Background()
	sessionID := "test-session-2"

	// Append 3 entries
	var ids []string
	for i := range 3 {
		entry := &StreamEntry{
			Type:      StreamEntryTypeEvent,
			Payload:   json.RawMessage(`{"kind":1}`), // MESSAGE kind
			Timestamp: time.Now().Format(time.RFC3339),
		}
		id, err := storage.AppendStreamEntry(ctx, sessionID, entry)
		if err != nil {
			t.Fatalf("failed to append entry %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Get all entries from beginning
	entries, err := storage.GetStreamEntries(ctx, sessionID, "0", 0)
	if err != nil {
		t.Fatalf("failed to get entries: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}

	// Get entries after first one
	entries, err = storage.GetStreamEntries(ctx, sessionID, ids[0], 0)
	if err != nil {
		t.Fatalf("failed to get entries after: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("expected 2 entries after first, got %d", len(entries))
	}

	// Verify IDs match
	if entries[0].ID != ids[1] {
		t.Errorf("expected first result ID %s, got %s", ids[1], entries[0].ID)
	}

	// Get entries with "$" (should return empty)
	entries, err = storage.GetStreamEntries(ctx, sessionID, "$", 0)
	if err != nil {
		t.Fatalf("failed to get entries with $: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("expected 0 entries with $, got %d", len(entries))
	}
}

func TestGetLastStreamID(t *testing.T) {
	mr, storage := setupTestRedis(t)
	defer mr.Close()
	defer storage.Close()

	ctx := context.Background()
	sessionID := "test-session-3"

	// Empty stream should return "$"
	id, err := storage.GetLastStreamID(ctx, sessionID)
	if err != nil {
		t.Fatalf("failed to get last stream ID: %v", err)
	}

	if id != "$" {
		t.Errorf("expected '$' for empty stream, got %s", id)
	}

	// Append some entries
	var lastID string
	for range 3 {
		entry := &StreamEntry{
			Type:      StreamEntryTypeEvent,
			Payload:   json.RawMessage(`{}`),
			Timestamp: time.Now().Format(time.RFC3339),
		}
		lastID, _ = storage.AppendStreamEntry(ctx, sessionID, entry)
	}

	// Should return the last ID
	id, err = storage.GetLastStreamID(ctx, sessionID)
	if err != nil {
		t.Fatalf("failed to get last stream ID: %v", err)
	}

	if id != lastID {
		t.Errorf("expected last ID %s, got %s", lastID, id)
	}
}

func TestGetStreamEntries_Limit(t *testing.T) {
	mr, storage := setupTestRedis(t)
	defer mr.Close()
	defer storage.Close()

	ctx := context.Background()
	sessionID := "test-session-4"

	// Append 10 entries
	for i := range 10 {
		entry := &StreamEntry{
			Type:      StreamEntryTypeEvent,
			Payload:   json.RawMessage(`{"kind":1}`), // MESSAGE kind
			Timestamp: time.Now().Format(time.RFC3339),
		}
		_, err := storage.AppendStreamEntry(ctx, sessionID, entry)
		if err != nil {
			t.Fatalf("failed to append entry %d: %v", i, err)
		}
	}

	// Get with limit of 5
	entries, err := storage.GetStreamEntries(ctx, sessionID, "0", 5)
	if err != nil {
		t.Fatalf("failed to get entries: %v", err)
	}

	if len(entries) != 5 {
		t.Errorf("expected 5 entries with limit, got %d", len(entries))
	}
}

func TestStreamKey(t *testing.T) {
	expected := "session:abc123:stream"
	actual := StreamKey("abc123")

	if actual != expected {
		t.Errorf("expected %s, got %s", expected, actual)
	}
}

func TestGetStreamEntriesByTypes(t *testing.T) {
	mr, storage := setupTestRedis(t)
	defer mr.Close()
	defer storage.Close()

	ctx := context.Background()
	sessionID := "test-session-5"

	// Append entries of different types
	entries := []struct {
		entryType string
		content   string
	}{
		{StreamEntryTypeEvent, `{"kind":1}`}, // MESSAGE kind
		{StreamEntryTypeEvent, `{"kind":3}`}, // TOOL_START kind
		{StreamEntryTypeEvent, `{"kind":1}`}, // MESSAGE kind
		{StreamEntryTypeTerminalOutput, `{"data":"output"}`},
		{StreamEntryTypeEvent, `{"kind":1}`}, // MESSAGE kind
	}

	for _, e := range entries {
		entry := &StreamEntry{
			Type:      e.entryType,
			Payload:   json.RawMessage(e.content),
			Timestamp: time.Now().Format(time.RFC3339),
		}
		_, err := storage.AppendStreamEntry(ctx, sessionID, entry)
		if err != nil {
			t.Fatalf("failed to append entry: %v", err)
		}
	}

	// Get only event entries
	eventEntries, err := storage.GetStreamEntriesByTypes(ctx, sessionID, "0", 0, []string{StreamEntryTypeEvent})
	if err != nil {
		t.Fatalf("failed to get event entries: %v", err)
	}

	if len(eventEntries) != 4 {
		t.Errorf("expected 4 event entries, got %d", len(eventEntries))
	}

	// Get terminal output only
	terminalEntries, err := storage.GetStreamEntriesByTypes(ctx, sessionID, "0", 0, []string{StreamEntryTypeTerminalOutput})
	if err != nil {
		t.Fatalf("failed to get terminal entries: %v", err)
	}

	if len(terminalEntries) != 1 {
		t.Errorf("expected 1 terminal entry, got %d", len(terminalEntries))
	}

	// Get multiple types
	mixedEntries, err := storage.GetStreamEntriesByTypes(ctx, sessionID, "0", 0, []string{StreamEntryTypeEvent, StreamEntryTypeTerminalOutput})
	if err != nil {
		t.Fatalf("failed to get mixed entries: %v", err)
	}

	if len(mixedEntries) != 5 {
		t.Errorf("expected 5 mixed entries, got %d", len(mixedEntries))
	}
}
