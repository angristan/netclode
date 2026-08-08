package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/angristan/netclode/services/control-plane/internal/config"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	keySessionsAll = "sessions:all"
)

func snapshotsSetKey(sessionID string) string {
	return "session:" + sessionID + ":snapshots"
}

func snapshotKey(sessionID, snapshotID string) string {
	return "session:" + sessionID + ":snapshot:" + snapshotID
}

func sessionKey(id string) string {
	return "session:" + id
}

// StreamKey returns the Redis Stream key for the unified session stream.
// Exported for use by StreamSubscriber in the session package.
func StreamKey(id string) string {
	return "session:" + id + ":stream"
}

// RedisStorage implements Storage using Redis.
type RedisStorage struct {
	client *redis.Client
	config *config.Config
}

// NewRedisStorage creates a new Redis storage instance.
func NewRedisStorage(ctx context.Context, cfg *config.Config) (*RedisStorage, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}

	client := redis.NewClient(opts)

	// Add OpenTelemetry tracing and metrics hooks to all Redis commands
	if err := redisotel.InstrumentTracing(client); err != nil {
		slog.WarnContext(ctx, "Failed to instrument Redis tracing", "error", err)
	}
	if err := redisotel.InstrumentMetrics(client); err != nil {
		slog.WarnContext(ctx, "Failed to instrument Redis metrics", "error", err)
	}

	// Retry connection with backoff
	var lastErr error
	for i := range 10 {
		if err := client.Ping(ctx).Err(); err == nil {
			slog.InfoContext(ctx, "connected to Redis", "url", ParseRedisURL(cfg.RedisURL))
			return &RedisStorage{client: client, config: cfg}, nil
		} else {
			lastErr = err
			backoff := min(time.Duration(100*(i+1))*time.Millisecond, 3*time.Second)
			slog.WarnContext(ctx, "Redis connection failed, retrying", "attempt", i+1, "error", err, "backoff", backoff)
			time.Sleep(backoff)
		}
	}

	return nil, fmt.Errorf("failed to connect to Redis after 10 attempts: %w", lastErr)
}

// Close closes the Redis connection.
func (r *RedisStorage) Close() error {
	return r.client.Close()
}

// ============================================================================
// Session Storage
// ============================================================================

// SaveSession saves a session to Redis.
func (r *RedisStorage) SaveSession(ctx context.Context, s *pb.Session) error {
	pipe := r.client.TxPipeline()

	pipe.SAdd(ctx, keySessionsAll, s.Id)
	pipe.HSet(ctx, sessionKey(s.Id),
		"name", s.Name,
		"owner", s.Owner,
		"status", s.Status.String(),
		"createdAt", s.CreatedAt.AsTime().Format(time.RFC3339),
		"lastActiveAt", s.LastActiveAt.AsTime().Format(time.RFC3339),
	)
	if len(s.Repos) > 0 {
		if reposJSON, err := json.Marshal(s.Repos); err == nil {
			pipe.HSet(ctx, sessionKey(s.Id), "repos", string(reposJSON))
		} else {
			slog.WarnContext(ctx, "Failed to encode repos for session", "sessionID", s.Id, "error", err)
		}
	}
	if s.RepoAccess != nil {
		pipe.HSet(ctx, sessionKey(s.Id), "repoAccess", s.RepoAccess.String())
	}
	if s.SdkType != nil {
		pipe.HSet(ctx, sessionKey(s.Id), "sdkType", s.SdkType.String())
	}
	if s.Model != nil {
		pipe.HSet(ctx, sessionKey(s.Id), "model", *s.Model)
	}

	_, err := pipe.Exec(ctx)
	return err
}

// GetSession retrieves a session from Redis.
func (r *RedisStorage) GetSession(ctx context.Context, id string) (*pb.Session, error) {
	data, err := r.client.HGetAll(ctx, sessionKey(id)).Result()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}

	session := &pb.Session{
		Id:     id,
		Name:   data["name"],
		Owner:  data["owner"],
		Status: parseSessionStatus(data["status"]),
	}
	if t, err := time.Parse(time.RFC3339, data["createdAt"]); err == nil {
		session.CreatedAt = timestamppb.New(t)
	}
	if t, err := time.Parse(time.RFC3339, data["lastActiveAt"]); err == nil {
		session.LastActiveAt = timestamppb.New(t)
	}
	if reposJSON, ok := data["repos"]; ok && reposJSON != "" {
		var repos []string
		if err := json.Unmarshal([]byte(reposJSON), &repos); err == nil {
			session.Repos = repos
		} else {
			slog.WarnContext(ctx, "Failed to decode repos for session", "sessionID", id, "error", err)
		}
	}
	if repoAccessStr, ok := data["repoAccess"]; ok && repoAccessStr != "" {
		repoAccess := parseRepoAccess(repoAccessStr)
		if repoAccess != pb.RepoAccess_REPO_ACCESS_UNSPECIFIED {
			session.RepoAccess = &repoAccess
		}
	}
	if sdkTypeStr, ok := data["sdkType"]; ok && sdkTypeStr != "" {
		sdkType := parseSdkType(sdkTypeStr)
		session.SdkType = &sdkType
	}
	if model, ok := data["model"]; ok && model != "" {
		session.Model = &model
	}

	return session, nil
}

// parseSessionStatus converts a string status to pb.SessionStatus enum.
func parseSessionStatus(s string) pb.SessionStatus {
	switch s {
	case "SESSION_STATUS_CREATING", "creating":
		return pb.SessionStatus_SESSION_STATUS_CREATING
	case "SESSION_STATUS_RESUMING", "resuming":
		return pb.SessionStatus_SESSION_STATUS_RESUMING
	case "SESSION_STATUS_READY", "ready":
		return pb.SessionStatus_SESSION_STATUS_READY
	case "SESSION_STATUS_RUNNING", "running":
		return pb.SessionStatus_SESSION_STATUS_RUNNING
	case "SESSION_STATUS_PAUSED", "paused":
		return pb.SessionStatus_SESSION_STATUS_PAUSED
	case "SESSION_STATUS_ERROR", "error":
		return pb.SessionStatus_SESSION_STATUS_ERROR
	case "SESSION_STATUS_INTERRUPTED", "interrupted":
		return pb.SessionStatus_SESSION_STATUS_INTERRUPTED
	default:
		return pb.SessionStatus_SESSION_STATUS_UNSPECIFIED
	}
}

// parseSdkType converts a string to pb.SdkType enum.
func parseSdkType(s string) pb.SdkType {
	switch s {
	case "SDK_TYPE_CLAUDE":
		return pb.SdkType_SDK_TYPE_CLAUDE
	case "SDK_TYPE_OPENCODE":
		return pb.SdkType_SDK_TYPE_OPENCODE
	default:
		return pb.SdkType_SDK_TYPE_UNSPECIFIED
	}
}

func parseRepoAccess(s string) pb.RepoAccess {
	switch strings.ToLower(s) {
	case "repo_access_read", "read":
		return pb.RepoAccess_REPO_ACCESS_READ
	case "repo_access_write", "write":
		return pb.RepoAccess_REPO_ACCESS_WRITE
	default:
		return pb.RepoAccess_REPO_ACCESS_UNSPECIFIED
	}
}

// GetAllSessions retrieves all sessions from Redis.
func (r *RedisStorage) GetAllSessions(ctx context.Context) ([]*pb.Session, error) {
	ids, err := r.client.SMembers(ctx, keySessionsAll).Result()
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return []*pb.Session{}, nil
	}

	// Pipeline to get all session data
	pipe := r.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, sessionKey(id))
	}
	_, err = pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}

	sessions := make([]*pb.Session, 0, len(ids))
	for i, id := range ids {
		data, err := cmds[i].Result()
		if err != nil || len(data) == 0 {
			continue
		}

		session := &pb.Session{
			Id:     id,
			Name:   data["name"],
			Status: parseSessionStatus(data["status"]),
		}
		if t, err := time.Parse(time.RFC3339, data["createdAt"]); err == nil {
			session.CreatedAt = timestamppb.New(t)
		}
		if t, err := time.Parse(time.RFC3339, data["lastActiveAt"]); err == nil {
			session.LastActiveAt = timestamppb.New(t)
		}
		if reposJSON, ok := data["repos"]; ok && reposJSON != "" {
			var repos []string
			if err := json.Unmarshal([]byte(reposJSON), &repos); err == nil {
				session.Repos = repos
			} else {
				slog.WarnContext(ctx, "Failed to decode repos for session", "sessionID", id, "error", err)
			}
		}
		if repoAccessStr, ok := data["repoAccess"]; ok && repoAccessStr != "" {
			repoAccess := parseRepoAccess(repoAccessStr)
			if repoAccess != pb.RepoAccess_REPO_ACCESS_UNSPECIFIED {
				session.RepoAccess = &repoAccess
			}
		}
		sessions = append(sessions, session)
	}

	return sessions, nil
}

// UpdateSessionStatus updates the status of a session.
func (r *RedisStorage) UpdateSessionStatus(ctx context.Context, id string, status pb.SessionStatus) error {
	return r.client.HSet(ctx, sessionKey(id), "status", status.String()).Err()
}

// UpdateSessionField updates a single field of a session.
func (r *RedisStorage) UpdateSessionField(ctx context.Context, id, field, value string) error {
	return r.client.HSet(ctx, sessionKey(id), field, value).Err()
}

// SetRestoreSnapshotID stores the snapshot ID to restore from (persisted for crash recovery).
func (r *RedisStorage) SetRestoreSnapshotID(ctx context.Context, sessionID, snapshotID string) error {
	return r.client.HSet(ctx, sessionKey(sessionID), "restoreSnapshotID", snapshotID).Err()
}

// GetRestoreSnapshotID retrieves the pending restore snapshot ID.
func (r *RedisStorage) GetRestoreSnapshotID(ctx context.Context, sessionID string) (string, error) {
	return r.client.HGet(ctx, sessionKey(sessionID), "restoreSnapshotID").Result()
}

// ClearRestoreSnapshotID removes the restore snapshot ID after use.
func (r *RedisStorage) ClearRestoreSnapshotID(ctx context.Context, sessionID string) error {
	return r.client.HDel(ctx, sessionKey(sessionID), "restoreSnapshotID").Err()
}

// SetOldPVCName stores the old PVC name to delete after restore completes.
func (r *RedisStorage) SetOldPVCName(ctx context.Context, sessionID, pvcName string) error {
	return r.client.HSet(ctx, sessionKey(sessionID), "oldPVCName", pvcName).Err()
}

// GetOldPVCName retrieves the old PVC name to delete.
func (r *RedisStorage) GetOldPVCName(ctx context.Context, sessionID string) (string, error) {
	return r.client.HGet(ctx, sessionKey(sessionID), "oldPVCName").Result()
}

// ClearOldPVCName removes the old PVC name after cleanup.
func (r *RedisStorage) ClearOldPVCName(ctx context.Context, sessionID string) error {
	return r.client.HDel(ctx, sessionKey(sessionID), "oldPVCName").Err()
}

// SetPVCName stores the current PVC name for the session (used for resume after pause).
func (r *RedisStorage) SetPVCName(ctx context.Context, sessionID, pvcName string) error {
	return r.client.HSet(ctx, sessionKey(sessionID), "pvcName", pvcName).Err()
}

// GetPVCName retrieves the current PVC name for the session.
func (r *RedisStorage) GetPVCName(ctx context.Context, sessionID string) (string, error) {
	return r.client.HGet(ctx, sessionKey(sessionID), "pvcName").Result()
}

// DeleteSession removes a session and all related data from Redis.
func (r *RedisStorage) DeleteSession(ctx context.Context, id string) error {
	// First delete all snapshots
	if err := r.DeleteAllSnapshots(ctx, id); err != nil {
		slog.WarnContext(ctx, "failed to delete snapshots during session delete", "sessionID", id, "error", err)
	}

	pipe := r.client.TxPipeline()
	pipe.SRem(ctx, keySessionsAll, id)
	pipe.Del(ctx, sessionKey(id))
	pipe.Del(ctx, StreamKey(id)) // Delete unified stream
	_, err := pipe.Exec(ctx)
	return err
}

// ============================================================================
// Unified Stream Storage
// ============================================================================

// AppendStreamEntry appends an entry to the session's unified stream.
// Returns the Redis Stream ID of the appended entry.
func (r *RedisStorage) AppendStreamEntry(ctx context.Context, sessionID string, entry *StreamEntry) (string, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}

	id, err := r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey(sessionID),
		Values: map[string]any{
			"data": string(data),
		},
	}).Result()

	return id, err
}

// GetStreamEntries retrieves entries from the session's unified stream.
// If afterID is empty or "0", returns from the beginning.
// If afterID is "$", returns empty (caller wants only new entries).
// If limit is 0, returns all matching entries.
func (r *RedisStorage) GetStreamEntries(ctx context.Context, sessionID string, afterID string, limit int) ([]StreamEntryWithID, error) {
	return r.GetStreamEntriesByTypes(ctx, sessionID, afterID, limit, nil)
}

// GetStreamEntriesByTypes retrieves entries filtered by type.
// If types is nil or empty, returns all entries.
func (r *RedisStorage) GetStreamEntriesByTypes(ctx context.Context, sessionID string, afterID string, limit int, types []string) ([]StreamEntryWithID, error) {
	streamKey := StreamKey(sessionID)

	startID := afterID
	if startID == "" {
		startID = "0"
	} else if startID != "$" && startID != "0" {
		// Exclude the given ID by using exclusive range
		startID = "(" + startID
	}

	// If "$" just return empty - caller wants only new entries
	if afterID == "$" {
		return []StreamEntryWithID{}, nil
	}

	messages, err := r.client.XRange(ctx, streamKey, startID, "+").Result()
	if err != nil {
		return nil, err
	}

	// Build type filter set if provided
	typeFilter := make(map[string]bool)
	if len(types) > 0 {
		for _, t := range types {
			typeFilter[t] = true
		}
	}

	entries := make([]StreamEntryWithID, 0, len(messages))
	for _, msg := range messages {
		dataStr, ok := msg.Values["data"].(string)
		if !ok {
			continue
		}

		var entry StreamEntry
		if err := json.Unmarshal([]byte(dataStr), &entry); err != nil {
			continue
		}

		// Apply type filter if provided
		if len(typeFilter) > 0 && !typeFilter[entry.Type] {
			continue
		}

		entries = append(entries, StreamEntryWithID{
			ID:    msg.ID,
			Entry: &entry,
		})

		// Apply limit after filtering
		if limit > 0 && len(entries) >= limit {
			break
		}
	}

	return entries, nil
}

// GetLastStreamID returns the ID of the last entry in the session's stream.
// Returns "$" if the stream is empty (meaning "only new entries").
func (r *RedisStorage) GetLastStreamID(ctx context.Context, sessionID string) (string, error) {
	streamKey := StreamKey(sessionID)

	// Use XREVRANGE to get just the last entry
	messages, err := r.client.XRevRangeN(ctx, streamKey, "+", "-", 1).Result()
	if err != nil {
		if err == redis.Nil {
			return "$", nil
		}
		return "", err
	}

	if len(messages) == 0 {
		return "$", nil
	}

	return messages[0].ID, nil
}

// TruncateStreamAfter deletes all entries after the given stream ID.
// If afterID is empty, all entries are deleted.
func (r *RedisStorage) TruncateStreamAfter(ctx context.Context, sessionID string, afterID string) error {
	streamKey := StreamKey(sessionID)

	if afterID == "" {
		// Delete entire stream
		return r.client.Del(ctx, streamKey).Err()
	}

	// Get all entry IDs after the given ID
	messages, err := r.client.XRange(ctx, streamKey, "("+afterID, "+").Result()
	if err != nil {
		return err
	}

	if len(messages) == 0 {
		return nil
	}

	// Delete each entry after the snapshot point
	ids := make([]string, len(messages))
	for i, msg := range messages {
		ids[i] = msg.ID
	}

	return r.client.XDel(ctx, streamKey, ids...).Err()
}

// GetStreamDepth returns the number of entries in the session's Redis stream.
// This is useful for monitoring stream backlog depth.
func (r *RedisStorage) GetStreamDepth(ctx context.Context, sessionID string) (int64, error) {
	return r.client.XLen(ctx, StreamKey(sessionID)).Result()
}

// GetMessageCount returns the message count for a session.
// This reads from a counter stored in the session hash for efficiency.
func (r *RedisStorage) GetMessageCount(ctx context.Context, sessionID string) (int, error) {
	count, err := r.client.HGet(ctx, sessionKey(sessionID), "messageCount").Int()
	if err == redis.Nil {
		return 0, nil
	}
	return count, err
}

// IncrementMessageCount atomically increments the message counter for a session.
func (r *RedisStorage) IncrementMessageCount(ctx context.Context, sessionID string) error {
	return r.client.HIncrBy(ctx, sessionKey(sessionID), "messageCount", 1).Err()
}

// SetMessageCount sets the message counter to a specific value (used for snapshot restore).
func (r *RedisStorage) SetMessageCount(ctx context.Context, sessionID string, count int) error {
	return r.client.HSet(ctx, sessionKey(sessionID), "messageCount", count).Err()
}

// ============================================================================
// Snapshot Storage
// ============================================================================

// SaveSnapshot saves a snapshot to Redis.
func (r *RedisStorage) SaveSnapshot(ctx context.Context, snapshot *pb.Snapshot) error {
	pipe := r.client.TxPipeline()

	// Add to sorted set with creation time as score
	pipe.ZAdd(ctx, snapshotsSetKey(snapshot.SessionId), redis.Z{
		Score:  float64(snapshot.CreatedAt.AsTime().Unix()),
		Member: snapshot.Id,
	})

	// Store snapshot data (StreamId instead of EventStreamId)
	pipe.HSet(ctx, snapshotKey(snapshot.SessionId, snapshot.Id),
		"id", snapshot.Id,
		"sessionId", snapshot.SessionId,
		"name", snapshot.Name,
		"createdAt", snapshot.CreatedAt.AsTime().Format(time.RFC3339),
		"sizeBytes", snapshot.SizeBytes,
		"turnNumber", snapshot.TurnNumber,
		"messageCount", snapshot.MessageCount,
		"streamId", snapshot.StreamId,
	)

	_, err := pipe.Exec(ctx)
	return err
}

// GetSnapshot retrieves a snapshot by ID.
func (r *RedisStorage) GetSnapshot(ctx context.Context, sessionID, snapshotID string) (*pb.Snapshot, error) {
	data, err := r.client.HGetAll(ctx, snapshotKey(sessionID, snapshotID)).Result()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("snapshot not found: %s", snapshotID)
	}

	snapshot := &pb.Snapshot{
		Id:        data["id"],
		SessionId: data["sessionId"],
		Name:      data["name"],
	}

	if t, err := time.Parse(time.RFC3339, data["createdAt"]); err == nil {
		snapshot.CreatedAt = timestamppb.New(t)
	}

	if sizeStr, ok := data["sizeBytes"]; ok {
		var size int64
		fmt.Sscanf(sizeStr, "%d", &size)
		snapshot.SizeBytes = size
	}

	if turnStr, ok := data["turnNumber"]; ok {
		var turn int32
		fmt.Sscanf(turnStr, "%d", &turn)
		snapshot.TurnNumber = turn
	}

	if msgCountStr, ok := data["messageCount"]; ok {
		var count int32
		fmt.Sscanf(msgCountStr, "%d", &count)
		snapshot.MessageCount = count
	}

	if streamId, ok := data["streamId"]; ok {
		snapshot.StreamId = streamId
	}

	return snapshot, nil
}

// ListSnapshots retrieves all snapshots for a session, ordered by creation time (newest first).
func (r *RedisStorage) ListSnapshots(ctx context.Context, sessionID string) ([]*pb.Snapshot, error) {
	// Get all snapshot IDs from sorted set (newest first by score)
	ids, err := r.client.ZRevRange(ctx, snapshotsSetKey(sessionID), 0, -1).Result()
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return []*pb.Snapshot{}, nil
	}

	// Pipeline to get all snapshot data
	pipe := r.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, snapshotKey(sessionID, id))
	}
	_, err = pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}

	snapshots := make([]*pb.Snapshot, 0, len(ids))
	for _, cmd := range cmds {
		data, err := cmd.Result()
		if err != nil || len(data) == 0 {
			continue
		}

		snapshot := &pb.Snapshot{
			Id:        data["id"],
			SessionId: data["sessionId"],
			Name:      data["name"],
		}

		if t, err := time.Parse(time.RFC3339, data["createdAt"]); err == nil {
			snapshot.CreatedAt = timestamppb.New(t)
		}

		if sizeStr, ok := data["sizeBytes"]; ok {
			var size int64
			fmt.Sscanf(sizeStr, "%d", &size)
			snapshot.SizeBytes = size
		}

		if turnStr, ok := data["turnNumber"]; ok {
			var turn int32
			fmt.Sscanf(turnStr, "%d", &turn)
			snapshot.TurnNumber = turn
		}

		if msgCountStr, ok := data["messageCount"]; ok {
			var count int32
			fmt.Sscanf(msgCountStr, "%d", &count)
			snapshot.MessageCount = count
		}

		if streamId, ok := data["streamId"]; ok {
			snapshot.StreamId = streamId
		}

		snapshots = append(snapshots, snapshot)
	}

	return snapshots, nil
}

// DeleteSnapshot removes a snapshot from Redis.
func (r *RedisStorage) DeleteSnapshot(ctx context.Context, sessionID, snapshotID string) error {
	pipe := r.client.TxPipeline()
	pipe.ZRem(ctx, snapshotsSetKey(sessionID), snapshotID)
	pipe.Del(ctx, snapshotKey(sessionID, snapshotID))
	_, err := pipe.Exec(ctx)
	return err
}

// DeleteAllSnapshots removes all snapshots for a session.
func (r *RedisStorage) DeleteAllSnapshots(ctx context.Context, sessionID string) error {
	// Get all snapshot IDs first
	ids, err := r.client.ZRange(ctx, snapshotsSetKey(sessionID), 0, -1).Result()
	if err != nil {
		return err
	}

	if len(ids) == 0 {
		return nil
	}

	// Delete all snapshot data and the set
	pipe := r.client.TxPipeline()
	for _, id := range ids {
		pipe.Del(ctx, snapshotKey(sessionID, id))
	}
	pipe.Del(ctx, snapshotsSetKey(sessionID))

	_, err = pipe.Exec(ctx)
	return err
}

// GetRedisClient returns the underlying Redis client for direct access (used by StreamSubscriber).
func (r *RedisStorage) GetRedisClient() *redis.Client {
	return r.client
}

// ============================================================================
// Utilities
// ============================================================================

// ParseRedisURL extracts host from a Redis URL for logging purposes.
func ParseRedisURL(url string) string {
	// redis://[:password@]host:port
	url = strings.TrimPrefix(url, "redis://")
	if idx := strings.LastIndex(url, "@"); idx != -1 {
		url = url[idx+1:]
	}
	return url
}
