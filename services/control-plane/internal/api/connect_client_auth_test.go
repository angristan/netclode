package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/angristan/netclode/services/control-plane/gen/netclode/v1"
)

func TestBotClientMessageAllowlist(t *testing.T) {
	bot := &ConnectConnection{role: ClientRoleBot}
	if err := bot.authorizeMessage(context.Background(), &pb.ClientMessage{
		Message: &pb.ClientMessage_CreateSession{CreateSession: &pb.CreateSessionRequest{}},
	}); err != nil {
		t.Fatalf("bot create rejected: %v", err)
	}

	for _, msg := range []*pb.ClientMessage{
		{Message: &pb.ClientMessage_DeleteAllSessions{DeleteAllSessions: &pb.DeleteAllSessionsRequest{}}},
		{Message: &pb.ClientMessage_TerminalInput{TerminalInput: &pb.TerminalInputRequest{SessionId: "other", Data: "whoami"}}},
		{Message: &pb.ClientMessage_ListSessions{ListSessions: &pb.ListSessionsRequest{}}},
	} {
		err := bot.authorizeMessage(context.Background(), msg)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("bot message %T returned %v, want permission denied", msg.Message, err)
		}
	}
}

func TestAdminClientAllowsAdministrativeMessages(t *testing.T) {
	admin := &ConnectConnection{role: ClientRoleAdmin}
	err := admin.authorizeMessage(context.Background(), &pb.ClientMessage{
		Message: &pb.ClientMessage_DeleteAllSessions{DeleteAllSessions: &pb.DeleteAllSessionsRequest{}},
	})
	if err != nil {
		t.Fatalf("admin message rejected: %v", err)
	}
}
