package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/angristan/netclode/clients/cli/internal/client"
	"github.com/angristan/netclode/clients/cli/internal/output"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/spf13/cobra"
)

var (
	eventsLimit int
	eventsKind  string
)

var eventsCmd = &cobra.Command{
	Use:   "events <session-id>",
	Short: "Show events for a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runEvents,
}

var eventsTailCmd = &cobra.Command{
	Use:   "tail <session-id>",
	Short: "Stream events in real-time",
	Args:  cobra.ExactArgs(1),
	RunE:  runEventsTail,
}

func init() {
	rootCmd.AddCommand(eventsCmd)
	eventsCmd.AddCommand(eventsTailCmd)
	eventsCmd.Flags().IntVarP(&eventsLimit, "limit", "n", 50, "Limit number of events (0 = all)")
	eventsCmd.Flags().StringVar(&eventsKind, "kind", "", "Filter by event kind (TOOL_START, FILE_CHANGE, etc.)")
}

// EventInfo contains extracted event data from stream entries.
type EventInfo struct {
	Event     *pb.AgentEvent
	Timestamp string
	StreamID  string
}

func runEvents(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	c := client.New(getServerURL())
	sessionID := args[0]

	state, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Extract events from entries (AgentEvents, excluding MESSAGE kind)
	var events []EventInfo
	for _, e := range state.Entries {
		if e.Partial {
			continue // Skip streaming deltas
		}
		if event := e.GetEvent(); event != nil {
			// Skip MESSAGE events (they're shown in messages command)
			if event.Kind == pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE {
				continue
			}
			events = append(events, EventInfo{
				Event:     event,
				Timestamp: e.Timestamp.AsTime().Format("15:04:05"),
				StreamID:  e.Id,
			})
		}
	}

	// Filter by kind if specified
	if eventsKind != "" {
		filtered := make([]EventInfo, 0)
		targetKind := normalizeEventKind(eventsKind)
		for _, ei := range events {
			if ei.Event != nil && formatEventKind(ei.Event.Kind) == targetKind {
				filtered = append(filtered, ei)
			}
		}
		events = filtered
	}

	// Apply limit (from the end)
	if eventsLimit > 0 && len(events) > eventsLimit {
		events = events[len(events)-eventsLimit:]
	}

	if isJSONOutput() {
		return output.JSON(events)
	}

	if len(events) == 0 {
		fmt.Println("No events found.")
		return nil
	}

	printEventsTable(events)
	return nil
}

func normalizeEventKind(kind string) string {
	// Normalize user input to match our formatted output
	k := strings.ToLower(kind)
	k = strings.TrimPrefix(k, "agent_event_kind_")
	return k
}

func formatToolDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("(%dms)", ms)
	} else if ms < 60000 {
		return fmt.Sprintf("(%.1fs)", float64(ms)/1000)
	}
	return fmt.Sprintf("(%.1fm)", float64(ms)/60000)
}

func formatEventKind(kind pb.AgentEventKind) string {
	// Convert "AGENT_EVENT_KIND_TOOL_START" -> "tool_start"
	s := kind.String()
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "agent_event_kind_")
	return s
}

func printEventsTable(events []EventInfo) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = output.HeaderColor.Fprintf(w, "TIME\tKIND\tTOOL/PATH\tDETAILS\n")

	for _, ei := range events {
		e := ei.Event
		if e == nil {
			continue
		}

		timestamp := ei.Timestamp
		kind := formatEventKind(e.Kind)
		toolOrPath := getToolOrPath(e)
		details := getEventDetails(e)

		kindColor := output.EventKindColor(kind)

		_, _ = output.TimeColor.Fprintf(w, "%s\t", timestamp)
		_, _ = kindColor.Fprintf(w, "%s\t", kind)
		_, _ = fmt.Fprintf(w, "%s\t", output.Truncate(toolOrPath, 30))
		_, _ = fmt.Fprintf(w, "%s\n", output.Truncate(details, 50))
	}

	_ = w.Flush()
}

func getToolOrPath(e *pb.AgentEvent) string {
	if ts := e.GetToolStart(); ts != nil {
		return ts.Tool
	}
	if te := e.GetToolEnd(); te != nil {
		// Tool name is in correlation_id for tool events
		return e.CorrelationId
	}
	if ti := e.GetToolInput(); ti != nil {
		return e.CorrelationId
	}
	if to := e.GetToolOutput(); to != nil {
		return e.CorrelationId
	}
	return "-"
}

func getEventDetails(e *pb.AgentEvent) string {
	switch e.Kind {
	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_END:
		if te := e.GetToolEnd(); te != nil {
			var parts []string
			if te.DurationMs != nil && *te.DurationMs > 0 {
				parts = append(parts, formatToolDuration(*te.DurationMs))
			}
			if te.Error != nil {
				parts = append(parts, "error: "+*te.Error)
			} else if te.Success {
				parts = append(parts, "success")
			}
			return strings.Join(parts, " ")
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_START:
		if ts := e.GetToolStart(); ts != nil {
			return ts.Tool
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_INPUT:
		if ti := e.GetToolInput(); ti != nil {
			if ti.Input != nil {
				// Show input fields summary
				var parts []string
				for k, v := range ti.Input.AsMap() {
					parts = append(parts, fmt.Sprintf("%s=%v", k, output.Truncate(fmt.Sprintf("%v", v), 20)))
				}
				return strings.Join(parts, " ")
			}
			if ti.Delta != nil {
				return "(streaming...)"
			}
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_TOOL_OUTPUT:
		if to := e.GetToolOutput(); to != nil {
			if to.Output != nil {
				return output.Truncate(*to.Output, 50)
			}
			if to.Delta != nil {
				return "(streaming...)"
			}
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_THINKING:
		if thinking := e.GetThinking(); thinking != nil {
			return output.Truncate(thinking.Content, 50)
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_PORT_EXPOSED:
		if port := e.GetPortExposed(); port != nil {
			url := ""
			if port.PreviewUrl != nil {
				url = " " + *port.PreviewUrl
			}
			return fmt.Sprintf("port=%d%s", port.Port, url)
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_PORT_UNEXPOSED:
		if port := e.GetPortUnexposed(); port != nil {
			return fmt.Sprintf("port=%d removed", port.Port)
		}

	case pb.AgentEventKind_AGENT_EVENT_KIND_REPO_CLONE:
		if repo := e.GetRepoClone(); repo != nil {
			stage := repo.Stage.String()
			stage = strings.ToLower(strings.TrimPrefix(stage, "REPO_CLONE_STAGE_"))
			return stage + " " + repo.Message
		}
	}

	return ""
}

func runEventsTail(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	c := client.New(getServerURL())
	sessionID := args[0]

	fmt.Fprintf(os.Stderr, "Tailing events for session %s (Ctrl+C to stop)...\n\n", sessionID)

	err := c.TailEvents(ctx, sessionID, func(entryResp *pb.StreamEntryResponse) error {
		entry := entryResp.Entry
		if entry == nil {
			return nil
		}

		if isJSONOutput() {
			return output.JSONLine(entryResp)
		}

		// Skip partial (streaming) entries - only show final entries
		if entry.Partial {
			return nil
		}

		// Dispatch based on payload type
		if event := entry.GetEvent(); event != nil {
			// Handle AgentEvent (includes messages, thinking, tools, etc.)
			if event.Kind == pb.AgentEventKind_AGENT_EVENT_KIND_MESSAGE {
				if msg := event.GetMessage(); msg != nil {
					printStreamMessage(event)
				}
			} else {
				printStreamEvent(event, entry.Timestamp.AsTime().Format("15:04:05"))
			}
		} else if sess := entry.GetSessionUpdate(); sess != nil {
			_, _ = output.TimeColor.Print("[status] ")
			fmt.Printf("%s\n", sess.Status.String())
		}
		return nil
	})

	if err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}

func printStreamEvent(e *pb.AgentEvent, timestamp string) {
	if e == nil {
		return
	}

	kind := formatEventKind(e.Kind)
	toolOrPath := getToolOrPath(e)
	details := getEventDetails(e)

	kindColor := output.EventKindColor(kind)

	_, _ = output.TimeColor.Printf("[%s] ", timestamp)
	_, _ = kindColor.Printf("%-15s ", kind)
	fmt.Printf("%-20s ", output.Truncate(toolOrPath, 20))
	_, _ = output.MutedColor.Printf("%s\n", details)
}

func printStreamMessage(event *pb.AgentEvent) {
	msg := event.GetMessage()
	if msg == nil {
		return
	}
	// Convert "MESSAGE_ROLE_USER" -> "user"
	role := strings.ToLower(msg.Role.String())
	role = strings.TrimPrefix(role, "message_role_")

	_, _ = output.TimeColor.Print("[message] ")
	_, _ = output.IDColor.Printf("%s: ", role)
	// Show first line only for streaming view
	content := msg.Content
	if idx := strings.Index(content, "\n"); idx > 0 {
		content = content[:idx] + "..."
	}
	fmt.Println(output.Truncate(content, 60))
}
