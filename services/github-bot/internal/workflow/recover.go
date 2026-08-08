package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/angristan/netclode/services/github-bot/internal/store"
)

// RecoverInFlight attempts to recover all in-flight sessions from a previous run.
// For each session: reconnect to control-plane, collect result, update the GitHub comment.
func RecoverInFlight(ctx context.Context, deps *Deps) {
	if deps.Store == nil {
		return
	}

	sessions, err := deps.Store.ListInFlight(ctx)
	if err != nil {
		slog.Error("Failed to list in-flight sessions for recovery", "error", err)
		return
	}

	if len(sessions) == 0 {
		slog.Info("No in-flight sessions to recover")
		return
	}

	slog.Info("Found in-flight sessions to recover", "count", len(sessions))

	for _, s := range sessions {
		recoverSession(ctx, deps, s)
	}
}

func recoverSession(ctx context.Context, deps *Deps, s store.InFlightSession) {
	logger := slog.With("deliveryID", s.DeliveryID, "sessionID", s.SessionID, "owner", s.Owner, "repo", s.Repo, "number", s.Number, "commentID", s.CommentID)
	logger.Info("Recovering in-flight session")

	// Always clear from Redis when done
	defer deps.Store.ClearInFlight(ctx, s.DeliveryID)

	// If we don't have a session ID, we can't recover — just update the comment
	if s.SessionID == "" {
		logger.Warn("No session ID, cannot recover — updating comment with error")
		if s.CommentID > 0 {
			_ = updateComment(ctx, deps.GH, s.Owner, s.Repo, s.CommentID, s.DeliveryID,
				"Netclode was restarted before the session could be created. Please try again.")
		}
		return
	}

	// Give the recovery a bounded timeout
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	result, err := deps.CP.RecoverSession(recoverCtx, s.SessionID)
	if err != nil {
		logger.Error("Failed to recover session", "error", err)
		if s.CommentID > 0 {
			body := fmt.Sprintf("Netclode was restarted during processing. Recovery failed: %s", err.Error())
			if result != nil && result.Response != "" {
				body = result.Response + "\n\n---\n*Note: Netclode was restarted. The above is a partial response.*"
			}
			_ = updateComment(ctx, deps.GH, s.Owner, s.Repo, s.CommentID, s.DeliveryID, body)
		}
		// Clean up the session
		go deleteSession(deps.CP, s.SessionID)
		return
	}

	// Post recovered result
	if result.Response == "" {
		result.Response = "*No response from agent (recovered after restart).*"
	}
	if s.CommentID > 0 {
		if err := updateComment(ctx, deps.GH, s.Owner, s.Repo, s.CommentID, s.DeliveryID, result.Response); err != nil {
			logger.Error("Failed to update comment with recovered response", "error", err)
		}
	}

	// Clean up session
	go deleteSession(deps.CP, s.SessionID)
	logger.Info("Session recovered successfully", "responseLen", len(result.Response))
}
