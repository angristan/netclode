package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/angristan/netclode/clients/cli/internal/client"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/spf13/cobra"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage authentication",
	Long:  "Authenticate with various SDK providers.",
}

var authCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Authenticate with ChatGPT for Codex SDK",
	Long: `Authenticate with ChatGPT using OAuth device code flow.

This command will:
1. Display a verification URL and code
2. Wait for you to authorize in your browser
3. Complete authentication on the backend`,
	RunE: runAuthCodex,
}

var authCodexStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show backend Codex OAuth status",
	RunE:  runAuthCodexStatus,
}

var authCodexLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Delete backend Codex OAuth tokens",
	RunE:  runAuthCodexLogout,
}

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authCodexCmd)
	authCodexCmd.AddCommand(authCodexStatusCmd)
	authCodexCmd.AddCommand(authCodexLogoutCmd)
}

func runAuthCodex(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())

	fmt.Println("Codex Authentication (ChatGPT OAuth)")
	fmt.Println("=====================================")
	fmt.Println()

	fmt.Println("Requesting device code...")
	started, err := c.CodexAuthStart(ctx)
	if err != nil {
		return fmt.Errorf("failed to start backend codex auth: %w", err)
	}

	fmt.Println()
	fmt.Printf("Visit:  %s\n", started.VerificationUri)
	fmt.Printf("Code:   %s\n", started.UserCode)
	fmt.Println()
	fmt.Println("Waiting for backend authentication to complete...")

	interval := time.Duration(started.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(15 * time.Minute)
	if started.ExpiresAt != nil {
		deadline = started.ExpiresAt.AsTime()
	}

	for time.Now().Before(deadline) {
		status, err := c.CodexAuthStatus(ctx)
		if err != nil {
			return fmt.Errorf("failed to check auth status: %w", err)
		}

		switch status.State {
		case pb.CodexAuthState_CODEX_AUTH_STATE_READY:
			fmt.Println()
			fmt.Println("Authentication successful!")
			if status.AccountId != nil && *status.AccountId != "" {
				fmt.Printf("Account: %s\n", *status.AccountId)
			}
			if status.ExpiresAt != nil {
				fmt.Printf("Token expires at: %s\n", status.ExpiresAt.AsTime().Format(time.RFC3339))
			}
			return nil
		case pb.CodexAuthState_CODEX_AUTH_STATE_ERROR:
			if status.Error != nil {
				return fmt.Errorf("authentication failed: %s", *status.Error)
			}
			return fmt.Errorf("authentication failed")
		}

		time.Sleep(interval)
	}

	return fmt.Errorf("authentication timed out")
}

func runAuthCodexStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())

	status, err := c.CodexAuthStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to read codex oauth status: %w", err)
	}

	switch status.State {
	case pb.CodexAuthState_CODEX_AUTH_STATE_READY:
		fmt.Println("Codex OAuth: authenticated")
		if status.AccountId != nil && *status.AccountId != "" {
			fmt.Printf("Account: %s\n", *status.AccountId)
		}
		if status.ExpiresAt != nil {
			fmt.Printf("Expires at: %s\n", status.ExpiresAt.AsTime().Format(time.RFC3339))
		}
	case pb.CodexAuthState_CODEX_AUTH_STATE_PENDING:
		fmt.Println("Codex OAuth: pending authorization")
		if status.ExpiresAt != nil {
			fmt.Printf("Pending expires at: %s\n", status.ExpiresAt.AsTime().Format(time.RFC3339))
		}
	case pb.CodexAuthState_CODEX_AUTH_STATE_ERROR:
		fmt.Println("Codex OAuth: error")
		if status.Error != nil && *status.Error != "" {
			fmt.Printf("Error: %s\n", *status.Error)
		}
	default:
		fmt.Println("Codex OAuth: not authenticated")
	}
	return nil
}

func runAuthCodexLogout(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())
	if err := c.CodexAuthLogout(ctx); err != nil {
		return fmt.Errorf("failed to delete backend codex oauth tokens: %w", err)
	}
	fmt.Println("Codex OAuth tokens removed from backend.")
	return nil
}
