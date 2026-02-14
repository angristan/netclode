package storage

import (
	"context"
	"encoding/json"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/redis/go-redis/v9"
)

// StreamEntry represents an entry in the unified session stream.
// This is the storage-layer representation; it gets converted to pb.StreamEntry for the API.
type StreamEntry struct {
	Type      string          `json:"type"`      // "event", "terminal_output", "session_update", "error"
	Partial   bool            `json:"partial"`   // true = streaming delta, false = final
	Payload   json.RawMessage `json:"payload"`   // Type-specific data (AgentEvent, TerminalOutput, Session, Error)
	Timestamp string          `json:"timestamp"` // ISO8601 timestamp
}

// StreamEntryWithID wraps a stream entry with its Redis Stream ID.
type StreamEntryWithID struct {
	ID    string
	Entry *StreamEntry
}

// CodexOAuthSessionData is encrypted and stored per session for Codex OAuth mode.
type CodexOAuthSessionData struct {
	AccessToken  string     `json:"access_token"`
	IdToken      string     `json:"id_token"`
	RefreshToken string     `json:"refresh_token"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Storage defines the interface for session persistence.
type Storage interface {
	// Sessions
	SaveSession(ctx context.Context, s *pb.Session) error
	GetSession(ctx context.Context, id string) (*pb.Session, error)
	GetAllSessions(ctx context.Context) ([]*pb.Session, error)
	UpdateSessionStatus(ctx context.Context, id string, status pb.SessionStatus) error
	UpdateSessionField(ctx context.Context, id, field, value string) error
	SaveSessionCodexOAuth(ctx context.Context, sessionID string, data *CodexOAuthSessionData) error
	GetSessionCodexOAuth(ctx context.Context, sessionID string) (*CodexOAuthSessionData, error)
	DeleteSessionCodexOAuth(ctx context.Context, sessionID string) error
	SaveCodexOAuth(ctx context.Context, data *CodexOAuthSessionData) error
	GetCodexOAuth(ctx context.Context) (*CodexOAuthSessionData, error)
	DeleteCodexOAuth(ctx context.Context) error
	DeleteSession(ctx context.Context, id string) error

	// Unified Stream (replaces messages, events, and notifications)
	// All session data flows through a single Redis Stream per session.
	AppendStreamEntry(ctx context.Context, sessionID string, entry *StreamEntry) (string, error)
	GetStreamEntries(ctx context.Context, sessionID string, afterID string, limit int) ([]StreamEntryWithID, error)
	GetStreamEntriesByTypes(ctx context.Context, sessionID string, afterID string, limit int, types []string) ([]StreamEntryWithID, error)
	GetLastStreamID(ctx context.Context, sessionID string) (string, error)
	TruncateStreamAfter(ctx context.Context, sessionID string, afterID string) error

	// Message count (for session summary)
	GetMessageCount(ctx context.Context, sessionID string) (int, error)
	IncrementMessageCount(ctx context.Context, sessionID string) error
	SetMessageCount(ctx context.Context, sessionID string, count int) error

	// Snapshots
	SaveSnapshot(ctx context.Context, snapshot *pb.Snapshot) error
	GetSnapshot(ctx context.Context, sessionID, snapshotID string) (*pb.Snapshot, error)
	ListSnapshots(ctx context.Context, sessionID string) ([]*pb.Snapshot, error)
	DeleteSnapshot(ctx context.Context, sessionID, snapshotID string) error
	DeleteAllSnapshots(ctx context.Context, sessionID string) error

	// Restore snapshot ID (persisted for crash recovery)
	SetRestoreSnapshotID(ctx context.Context, sessionID, snapshotID string) error
	GetRestoreSnapshotID(ctx context.Context, sessionID string) (string, error)
	ClearRestoreSnapshotID(ctx context.Context, sessionID string) error

	// Old PVC name to delete after restore (cleanup orphaned PVCs)
	SetOldPVCName(ctx context.Context, sessionID, pvcName string) error
	GetOldPVCName(ctx context.Context, sessionID string) (string, error)
	ClearOldPVCName(ctx context.Context, sessionID string) error

	// Current PVC name (for resume after pause)
	SetPVCName(ctx context.Context, sessionID, pvcName string) error
	GetPVCName(ctx context.Context, sessionID string) (string, error)

	// Redis client access (for StreamSubscriber)
	GetRedisClient() *redis.Client

	// Lifecycle
	Close() error
}

// Stream entry types for storage layer
const (
	StreamEntryTypeEvent          = "event"           // AgentEvent (messages, thinking, tools, etc.)
	StreamEntryTypeTerminalOutput = "terminal_output" // Terminal data
	StreamEntryTypeSessionUpdate  = "session_update"  // Session status changes
	StreamEntryTypeError          = "error"           // Error notifications
)
