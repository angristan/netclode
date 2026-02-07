package cmd

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/angristan/netclode/clients/cli/internal/client"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
	"github.com/spf13/cobra"
)

var portCmd = &cobra.Command{
	Use:   "port",
	Short: "Manage exposed ports",
}

var portExposeCmd = &cobra.Command{
	Use:   "expose <session-id> <port>",
	Short: "Expose a port for a session via Tailscale",
	Args:  cobra.ExactArgs(2),
	RunE:  runPortExpose,
}

var portUnexposeCmd = &cobra.Command{
	Use:   "unexpose <session-id> <port>",
	Short: "Remove an exposed port for a session",
	Args:  cobra.ExactArgs(2),
	RunE:  runPortUnexpose,
}

func init() {
	portCmd.AddCommand(portExposeCmd)
	portCmd.AddCommand(portUnexposeCmd)
	rootCmd.AddCommand(portCmd)
}

func runPortExpose(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	port, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("invalid port: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := client.New(getServerURL())

	stream := c.Stream(ctx)
	defer func() { _ = stream.CloseRequest() }()

	// Open session first
	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_OpenSession{
			OpenSession: &pb.OpenSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return fmt.Errorf("open session: %w", err)
	}

	// Wait for session state
	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive session state: %w", err)
	}
	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}
	if msg.GetSessionState() == nil {
		return fmt.Errorf("expected session state, got %T", msg.GetMessage())
	}

	// Send expose port request
	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_ExposePort{
			ExposePort: &pb.ExposePortRequest{
				SessionId: sessionID,
				Port:      int32(port),
			},
		},
	}); err != nil {
		return fmt.Errorf("send expose port: %w", err)
	}

	// Wait for response
	for {
		msg, err := stream.Receive()
		if err != nil {
			return fmt.Errorf("receive: %w", err)
		}

		if resp := msg.GetPortExposed(); resp != nil {
			fmt.Printf("Port %d exposed\n", port)
			fmt.Printf("Preview URL: %s\n", resp.PreviewUrl)
			return nil
		}

		if errResp := msg.GetError(); errResp != nil {
			return fmt.Errorf("error: %s: %s", errResp.Error.Code, errResp.Error.Message)
		}
	}
}

func runPortUnexpose(cmd *cobra.Command, args []string) error {
	sessionID := args[0]
	port, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("invalid port: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := client.New(getServerURL())

	stream := c.Stream(ctx)
	defer func() { _ = stream.CloseRequest() }()

	// Open session first
	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_OpenSession{
			OpenSession: &pb.OpenSessionRequest{
				SessionId: sessionID,
			},
		},
	}); err != nil {
		return fmt.Errorf("open session: %w", err)
	}

	// Wait for session state
	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("receive session state: %w", err)
	}
	if errResp := msg.GetError(); errResp != nil {
		return fmt.Errorf("%s: %s", errResp.Error.Code, errResp.Error.Message)
	}
	if msg.GetSessionState() == nil {
		return fmt.Errorf("expected session state, got %T", msg.GetMessage())
	}

	// Send unexpose port request
	if err := stream.Send(&pb.ClientMessage{
		Message: &pb.ClientMessage_UnexposePort{
			UnexposePort: &pb.UnexposePortRequest{
				SessionId: sessionID,
				Port:      int32(port),
			},
		},
	}); err != nil {
		return fmt.Errorf("send unexpose port: %w", err)
	}

	// Wait for response
	for {
		msg, err := stream.Receive()
		if err != nil {
			return fmt.Errorf("receive: %w", err)
		}

		if resp := msg.GetPortUnexposed(); resp != nil {
			fmt.Printf("Port %d unexposed\n", port)
			return nil
		}

		if errResp := msg.GetError(); errResp != nil {
			return fmt.Errorf("error: %s: %s", errResp.Error.Code, errResp.Error.Message)
		}
	}
}
