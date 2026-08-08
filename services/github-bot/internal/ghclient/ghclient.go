package ghclient

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v68/github"
)

// Client wraps the GitHub API for the bot's needs.
type Client struct {
	gh *github.Client
}

// New creates a new GitHub client authenticated as the GitHub App installation.
func New(appID, installationID int64, privateKey []byte) (*Client, error) {
	transport, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create github app transport: %w", err)
	}

	return &Client{
		gh: github.NewClient(&http.Client{Transport: transport}),
	}, nil
}

// PostComment creates a comment on an issue or pull request.
// Returns the comment ID for later editing.
func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) (int64, error) {
	comment, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{
		Body: &body,
	})
	if err != nil {
		return 0, fmt.Errorf("create comment: %w", err)
	}
	return comment.GetID(), nil
}

// EditComment updates an existing comment.
func (c *Client) EditComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	_, _, err := c.gh.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{
		Body: &body,
	})
	if err != nil {
		return fmt.Errorf("edit comment: %w", err)
	}
	return nil
}

// GetPRDiff fetches the raw diff for a pull request.
func (c *Client) GetPRDiff(ctx context.Context, owner, repo string, number int) (string, error) {
	diff, _, err := c.gh.PullRequests.GetRaw(ctx, owner, repo, number, github.RawOptions{
		Type: github.Diff,
	})
	if err != nil {
		return "", fmt.Errorf("get PR diff: %w", err)
	}
	return diff, nil
}

// GetPR fetches pull request metadata.
func (c *Client) GetPR(ctx context.Context, owner, repo string, number int) (*github.PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get PR: %w", err)
	}
	return pr, nil
}

// GetIssue fetches issue metadata.
func (c *Client) GetIssue(ctx context.Context, owner, repo string, number int) (*github.Issue, error) {
	issue, _, err := c.gh.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get issue: %w", err)
	}
	return issue, nil
}

// ListIssueComments fetches comments on an issue/PR (last N).
func (c *Client) ListIssueComments(ctx context.Context, owner, repo string, number int, count int) ([]*github.IssueComment, error) {
	// List all comments, we'll take the last N
	opts := &github.IssueListCommentsOptions{
		Sort:      github.Ptr("created"),
		Direction: github.Ptr("desc"),
		ListOptions: github.ListOptions{
			PerPage: count,
		},
	}
	comments, _, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	// Reverse to chronological order
	for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
		comments[i], comments[j] = comments[j], comments[i]
	}
	return comments, nil
}

// GetUserPermission checks the permission level of a user on a repository.
// Returns "admin", "write", "read", or "none".
func (c *Client) GetUserPermission(ctx context.Context, owner, repo, username string) (string, error) {
	perm, _, err := c.gh.Repositories.GetPermissionLevel(ctx, owner, repo, username)
	if err != nil {
		return "none", fmt.Errorf("get permission level: %w", err)
	}
	return perm.GetPermission(), nil
}

// HasWriteAccess checks if a user has write or admin access to a repository.
func (c *Client) HasWriteAccess(ctx context.Context, owner, repo, username string) (bool, error) {
	perm, err := c.GetUserPermission(ctx, owner, repo, username)
	if err != nil {
		return false, err
	}
	return perm == "admin" || perm == "write", nil
}

// DownloadPRDiffTruncated fetches a PR diff, truncated to maxBytes.
func (c *Client) DownloadPRDiffTruncated(ctx context.Context, owner, repo string, number int, maxBytes int) (string, bool, error) {
	diff, err := c.GetPRDiff(ctx, owner, repo, number)
	if err != nil {
		return "", false, err
	}
	if len(diff) > maxBytes {
		// Truncate at a newline boundary
		truncated := diff[:maxBytes]
		if idx := strings.LastIndex(truncated, "\n"); idx > 0 {
			truncated = truncated[:idx]
		}
		slog.Info("Truncated PR diff", "originalBytes", len(diff), "truncatedBytes", len(truncated))
		return truncated, true, nil
	}
	return diff, false, nil
}

// ReactToComment adds a reaction to a comment.
func (c *Client) ReactToComment(ctx context.Context, owner, repo string, commentID int64, reaction string) error {
	_, _, err := c.gh.Reactions.CreateIssueCommentReaction(ctx, owner, repo, commentID, reaction)
	if err != nil {
		// Non-fatal, just log
		slog.Warn("Failed to add reaction", "error", err, "commentID", commentID, "reaction", reaction)
		return nil
	}
	return nil
}
