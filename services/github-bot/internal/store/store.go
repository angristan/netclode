package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
)

const (
	dedupPrefix    = "ghbot:delivery:"
	dedupTTL       = 24 * time.Hour
	inflightPrefix = "ghbot:inflight:"
	inflightTTL    = 30 * time.Minute // Sessions older than this are considered stale
)

// InFlightSession tracks state needed to recover an in-progress workflow.
type InFlightSession struct {
	DeliveryID string `json:"delivery_id"`
	SessionID  string `json:"session_id"`
	CommentID  int64  `json:"comment_id"`
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"` // PR or issue number
}

// Store provides Redis-backed deduplication and in-flight session tracking.
type Store struct {
	rdb *redis.Client
}

// New creates a new Redis-backed store.
func New(redisURL string) (*Store, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	if err := redisotel.InstrumentTracing(client); err != nil {
		slog.Warn("Failed to instrument Redis tracing", "error", err)
	}
	if err := redisotel.InstrumentMetrics(client); err != nil {
		slog.Warn("Failed to instrument Redis metrics", "error", err)
	}

	return &Store{
		rdb: client,
	}, nil
}

// IsDuplicate checks if a delivery ID has already been processed.
// Returns true if it's a duplicate (already seen).
func (s *Store) IsDuplicate(ctx context.Context, deliveryID string) bool {
	key := dedupPrefix + deliveryID
	set, err := s.rdb.SetNX(ctx, key, "1", dedupTTL).Result()
	if err != nil {
		slog.Warn("Redis SETNX error, allowing processing", "error", err, "deliveryID", deliveryID)
		return false
	}
	return !set
}

// TrackInFlight stores an in-flight session so it can be recovered after restart.
func (s *Store) TrackInFlight(ctx context.Context, session InFlightSession) error {
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	key := inflightPrefix + session.DeliveryID
	return s.rdb.Set(ctx, key, data, inflightTTL).Err()
}

// ClearInFlight removes an in-flight session (workflow completed successfully).
func (s *Store) ClearInFlight(ctx context.Context, deliveryID string) {
	key := inflightPrefix + deliveryID
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		slog.Warn("Failed to clear in-flight session", "error", err, "deliveryID", deliveryID)
	}
}

// ListInFlight returns all in-flight sessions (for recovery on startup).
func (s *Store) ListInFlight(ctx context.Context) ([]InFlightSession, error) {
	keys, err := s.rdb.Keys(ctx, inflightPrefix+"*").Result()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	var sessions []InFlightSession
	for _, key := range keys {
		data, err := s.rdb.Get(ctx, key).Result()
		if err != nil {
			slog.Warn("Failed to read in-flight session", "error", err, "key", key)
			continue
		}
		var session InFlightSession
		if err := json.Unmarshal([]byte(data), &session); err != nil {
			slog.Warn("Failed to parse in-flight session", "error", err, "key", key)
			s.rdb.Del(ctx, key)
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

// Close closes the Redis connection.
func (s *Store) Close() error {
	return s.rdb.Close()
}
