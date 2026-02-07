package session

import (
	"strings"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/storage"
)

// SessionState holds the in-memory state for a session.
// Real-time updates are handled via Redis Streams, not in-memory channels.
type SessionState struct {
	Session       *pb.Session
	ServiceFQDN   string // DNS name of the agent service when running (legacy, kept for k8s)
	PendingPrompt string // Prompt queued before agent connected

	// Agent streaming state
	CurrentMessageID        string
	CurrentMessageStartTime time.Time // When the first content for current message arrived
	ContentBuilder          strings.Builder
	TitleGenerated          bool
	OriginalPrompt          string
	ThinkingBuffers         map[string]string
	ThinkingStartTimes      map[string]time.Time // When each thinking block started (for correct timestamp ordering)
	ToolStartTimes          map[string]time.Time // When each tool started (for correct timestamp ordering)

	// Restore state - when set, next sandbox creation restores from this snapshot
	RestoreSnapshotID string

	// Session-scoped Codex OAuth tokens (persisted encrypted in Redis).
	CodexOAuth *storage.CodexOAuthSessionData
}

// NewSessionState creates a new session state.
func NewSessionState(session *pb.Session) *SessionState {
	return &SessionState{
		Session:            session,
		ThinkingBuffers:    make(map[string]string),
		ThinkingStartTimes: make(map[string]time.Time),
		ToolStartTimes:     make(map[string]time.Time),
	}
}

// Close is a no-op now that we use Redis Streams for real-time.
// Kept for backwards compatibility with existing code.
func (s *SessionState) Close() {
	// No-op - StreamSubscribers are closed by their context
}
