package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/angristan/netclode/clients/cli/internal/client"
	"github.com/angristan/netclode/clients/cli/internal/output"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage sessions",
	Long:  "List, inspect, and manage Netclode sessions.",
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all sessions",
	RunE:  runSessionsList,
}

var sessionsGetCmd = &cobra.Command{
	Use:   "get <session-id>",
	Short: "Get session details",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsGet,
}

var sessionsDeleteCmd = &cobra.Command{
	Use:   "delete <session-id>",
	Short: "Delete a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsDelete,
}

var sessionsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new session",
	RunE:  runSessionsCreate,
}

var sessionsPauseCmd = &cobra.Command{
	Use:   "pause <session-id>",
	Short: "Pause a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsPause,
}

var sessionsResumeCmd = &cobra.Command{
	Use:   "resume <session-id>",
	Short: "Resume a paused session",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsResume,
}

var (
	createName    string
	createRepos   []string
	createSdkType string
	createModel   string

	createTailnet  bool
	createVCPUs    int32
	createMemoryMB int32

	listSortBy string
	listDesc   bool
)

func init() {
	rootCmd.AddCommand(sessionsCmd)
	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsGetCmd)
	sessionsCmd.AddCommand(sessionsDeleteCmd)
	sessionsCmd.AddCommand(sessionsCreateCmd)
	sessionsCmd.AddCommand(sessionsPauseCmd)
	sessionsCmd.AddCommand(sessionsResumeCmd)

	sessionsCreateCmd.Flags().StringVar(&createName, "name", "", "Session name")
	sessionsCreateCmd.Flags().StringArrayVar(&createRepos, "repo", nil, "GitHub repository (owner/repo). Can be repeated")
	sessionsCreateCmd.Flags().StringVar(&createSdkType, "sdk", "claude", "SDK type (claude, opencode, copilot, or codex)")
	sessionsCreateCmd.Flags().StringVar(&createModel, "model", "", "Model ID for OpenCode (e.g., anthropic/claude-sonnet-4-0)")
	sessionsCreateCmd.Flags().BoolVar(&createTailnet, "tailnet", false, "Enable Tailnet access (allow 100.64.0.0/10)")
	sessionsCreateCmd.Flags().Int32Var(&createVCPUs, "vcpus", 0, "Custom vCPUs for VM (bypasses warm pool, max 50% of host)")
	sessionsCreateCmd.Flags().Int32Var(&createMemoryMB, "memory-mb", 0, "Custom memory in MB for VM (bypasses warm pool, max 50% of host)")

	sessionsListCmd.Flags().StringVar(&listSortBy, "sort", "created", "Sort by field: created, active, name, status")
	sessionsListCmd.Flags().BoolVar(&listDesc, "desc", true, "Sort descending (newest first)")
}

func runSessionsList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())

	sessions, err := c.SyncSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	// Sort sessions
	sortSessions(sessions, listSortBy, listDesc)

	if isJSONOutput() {
		return output.JSON(sessions)
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	printSessionsTable(sessions)
	return nil
}

func sortSessions(sessions []*pb.SessionSummary, sortBy string, desc bool) {
	sort.Slice(sessions, func(i, j int) bool {
		var less bool
		switch sortBy {
		case "created":
			ti := sessions[i].Session.CreatedAt.AsTime()
			tj := sessions[j].Session.CreatedAt.AsTime()
			less = ti.Before(tj)
		case "active":
			ti := sessions[i].Session.LastActiveAt.AsTime()
			tj := sessions[j].Session.LastActiveAt.AsTime()
			less = ti.Before(tj)
		case "name":
			less = sessions[i].Session.Name < sessions[j].Session.Name
		case "status":
			less = sessions[i].Session.Status < sessions[j].Session.Status
		default:
			// Default to created time
			ti := sessions[i].Session.CreatedAt.AsTime()
			tj := sessions[j].Session.CreatedAt.AsTime()
			less = ti.Before(tj)
		}
		if desc {
			return !less
		}
		return less
	})
}

func printSessionsTable(sessions []*pb.SessionSummary) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	// Header
	_, _ = output.HeaderColor.Fprintf(w, "ID\tNAME\tSTATUS\tREPOS\tMSGS\tCREATED\tACTIVE\n")

	for _, s := range sessions {
		sess := s.Session
		id := output.Truncate(sess.Id, 16)
		name := output.Truncate(sess.Name, 30)
		status := formatStatus(sess.Status.String())
		repo := "-"
		if len(sess.Repos) > 0 {
			repo = output.Truncate(formatReposSummary(sess.Repos), 20)
		}
		msgs := "-"
		if s.MessageCount != nil {
			msgs = fmt.Sprintf("%d", *s.MessageCount)
		}
		created := output.RelativeTime(sess.CreatedAt)
		active := output.RelativeTime(sess.LastActiveAt)

		statusColor := output.StatusColor(sess.Status.String())

		_, _ = output.IDColor.Fprintf(w, "%s\t", id)
		_, _ = output.NameColor.Fprintf(w, "%s\t", name)
		_, _ = statusColor.Fprintf(w, "%s\t", status)
		_, _ = fmt.Fprintf(w, "%s\t", repo)
		_, _ = fmt.Fprintf(w, "%s\t", msgs)
		_, _ = output.TimeColor.Fprintf(w, "%s\t", created)
		_, _ = output.TimeColor.Fprintf(w, "%s\n", active)
	}

	_ = w.Flush()
}

func formatStatus(status string) string {
	// Convert "SESSION_STATUS_READY" -> "ready"
	s := strings.ToLower(status)
	s = strings.TrimPrefix(s, "session_status_")
	return s
}

func formatReposSummary(repos []string) string {
	display := formatReposDisplay(repos)
	if len(display) == 0 {
		return "-"
	}
	if len(display) == 1 {
		return display[0]
	}
	return fmt.Sprintf("%s (+%d)", display[0], len(display)-1)
}

func formatReposDisplay(repos []string) []string {
	if len(repos) == 0 {
		return nil
	}
	display := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.TrimPrefix(repo, "https://github.com/")
		repo = strings.TrimPrefix(repo, "http://github.com/")
		repo = strings.TrimSuffix(repo, ".git")
		if repo != "" {
			display = append(display, repo)
		}
	}
	return display
}

func runSessionsGet(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())
	sessionID := args[0]

	state, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if isJSONOutput() {
		return output.JSON(map[string]interface{}{
			"session":    state.Session,
			"entryCount": len(state.Entries),
			"hasMore":    state.HasMore,
		})
	}

	printSessionDetails(state)
	return nil
}

func printSessionDetails(state *client.SessionState) {
	sess := state.Session

	fmt.Println()
	_, _ = output.HeaderColor.Println("Session Details")
	fmt.Println(strings.Repeat("-", 40))

	fmt.Printf("%-15s ", "ID:")
	_, _ = output.IDColor.Println(sess.Id)

	fmt.Printf("%-15s %s\n", "Name:", sess.Name)

	status := formatStatus(sess.Status.String())
	fmt.Printf("%-15s ", "Status:")
	_, _ = output.StatusColor(sess.Status.String()).Println(status)

	repo := "-"
	if len(sess.Repos) > 0 {
		repo = strings.Join(formatReposDisplay(sess.Repos), ", ")
	}
	fmt.Printf("%-15s %s\n", "Repos:", repo)

	fmt.Printf("%-15s %s (%s)\n", "Created:",
		output.FormatTimestamp(sess.CreatedAt),
		output.RelativeTime(sess.CreatedAt))

	fmt.Printf("%-15s %s (%s)\n", "Last Active:",
		output.FormatTimestamp(sess.LastActiveAt),
		output.RelativeTime(sess.LastActiveAt))

	fmt.Println()
	_, _ = output.HeaderColor.Println("Statistics")
	fmt.Println(strings.Repeat("-", 40))
	fmt.Printf("%-15s %d\n", "Entries:", len(state.Entries))
	if state.HasMore {
		_, _ = output.MutedColor.Println("(more entries available)")
	}
	fmt.Println()
}

func runSessionsDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())
	sessionID := args[0]

	if err := c.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	if isJSONOutput() {
		return output.JSON(map[string]string{
			"deleted": sessionID,
		})
	}

	_, _ = output.SuccessColor.Printf("Deleted session %s\n", sessionID)
	return nil
}

func runSessionsCreate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())

	// Parse SDK type
	var sdkType pb.SdkType
	switch strings.ToLower(createSdkType) {
	case "opencode":
		sdkType = pb.SdkType_SDK_TYPE_OPENCODE
	case "copilot":
		sdkType = pb.SdkType_SDK_TYPE_COPILOT
	case "codex":
		sdkType = pb.SdkType_SDK_TYPE_CODEX
	case "claude", "":
		sdkType = pb.SdkType_SDK_TYPE_CLAUDE
	default:
		return fmt.Errorf("invalid SDK type: %s (use 'claude', 'opencode', 'copilot', or 'codex')", createSdkType)
	}

	opts := client.CreateSessionOptions{
		Name:          createName,
		Repos:         createRepos,
		SdkType:       sdkType,
		Model:         createModel,
		TailnetAccess: createTailnet,
		VCPUs:         createVCPUs,
		MemoryMB:      createMemoryMB,
	}

	if sdkType == pb.SdkType_SDK_TYPE_CODEX {
		authMode := codexModelAuthMode(createModel)
		if authMode == "" {
			return fmt.Errorf("codex model must include auth suffix (:oauth or :api), e.g. gpt-5-codex:oauth:high")
		}
	}

	session, err := c.CreateSession(ctx, opts)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	if isJSONOutput() {
		return output.JSON(session)
	}

	_, _ = output.SuccessColor.Printf("Created session %s\n", session.Id)
	fmt.Printf("  Name:     %s\n", session.Name)
	fmt.Printf("  Status:   %s\n", formatStatus(session.Status.String()))
	if session.SdkType != nil {
		fmt.Printf("  SDK:      %s\n", formatSdkType(*session.SdkType))
	}
	if session.Model != nil {
		fmt.Printf("  Model:    %s\n", *session.Model)
	}

	return nil
}

func formatSdkType(sdkType pb.SdkType) string {
	switch sdkType {
	case pb.SdkType_SDK_TYPE_OPENCODE:
		return "opencode"
	case pb.SdkType_SDK_TYPE_COPILOT:
		return "copilot"
	case pb.SdkType_SDK_TYPE_CODEX:
		return "codex"
	case pb.SdkType_SDK_TYPE_CLAUDE:
		return "claude"
	default:
		return "unknown"
	}
}

func codexModelAuthMode(model string) string {
	parts := strings.Split(model, ":")
	if len(parts) < 2 {
		return ""
	}
	last := parts[len(parts)-1]
	switch last {
	case "minimal", "low", "medium", "high", "xhigh":
		if len(parts) < 3 {
			return ""
		}
		last = parts[len(parts)-2]
	}
	if last == "api" || last == "oauth" {
		return last
	}
	return ""
}

func runSessionsPause(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())
	sessionID := args[0]

	session, err := c.PauseSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("pause session: %w", err)
	}

	if isJSONOutput() {
		return output.JSON(session)
	}

	_, _ = output.SuccessColor.Printf("Paused session %s\n", sessionID)
	fmt.Printf("  Status: %s\n", formatStatus(session.Status.String()))
	return nil
}

func runSessionsResume(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())
	sessionID := args[0]

	session, err := c.ResumeSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("resume session: %w", err)
	}

	if isJSONOutput() {
		return output.JSON(session)
	}

	_, _ = output.SuccessColor.Printf("Resumed session %s\n", sessionID)
	fmt.Printf("  Status: %s\n", formatStatus(session.Status.String()))
	return nil
}
