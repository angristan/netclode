import { useParams, useLocation } from "wouter";
import { useState, useEffect, useCallback, useRef } from "react";
import {
  AppShell,
  Group,
  Button,
  Text,
  Badge,
  Box,
  Grid,
} from "@mantine/core";
import {
  useWebSocket,
  useWebSocketMessages,
} from "../contexts/WebSocketContext";
import { ChatPanel, ChatMessage } from "../components/ChatPanel";
import { Terminal } from "../components/Terminal";
import { ThemeToggle } from "../components/ThemeToggle";
import type { ServerMessage, Session, AgentEvent } from "@netclode/protocol";

export function WorkspacePage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const [, navigate] = useLocation();
  const [isProcessing, setIsProcessing] = useState(false);
  const lastOpenedIdRef = useRef<string | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [events, setEvents] = useState<AgentEvent[]>([]);
  const { send, connected } = useWebSocket();
  // Track notification cursor for reconnection resilience
  const lastNotificationIdRef = useRef<string | undefined>(undefined);

  // Helper to update notification cursor
  const updateCursor = useCallback((msgId: string | undefined) => {
    if (msgId) {
      lastNotificationIdRef.current = msgId;
    }
  }, []);

  const handleMessage = useCallback(
    (msg: ServerMessage) => {
      switch (msg.type) {
        case "session.updated":
          if (msg.session.id === id) {
            setSession(msg.session);
            updateCursor(msg.id);
          }
          break;
        case "session.state":
          if (msg.session.id === id) {
            setSession(msg.session);
            setMessages(
              msg.messages.map((m) => ({ role: m.role, content: m.content }))
            );
            setEvents(msg.events.map((e) => e.event));
            // Store the notification cursor for reconnection
            if (msg.lastNotificationId) {
              lastNotificationIdRef.current = msg.lastNotificationId;
            }
          }
          break;
        case "agent.message":
          if (msg.sessionId === id) {
            updateCursor(msg.id);
            if (msg.partial) {
              setMessages((prev) => {
                const last = prev[prev.length - 1];
                if (last?.role === "assistant") {
                  return [
                    ...prev.slice(0, -1),
                    { ...last, content: last.content + msg.content },
                  ];
                }
                return [...prev, { role: "assistant", content: msg.content }];
              });
            } else {
              setMessages((prev) => [
                ...prev,
                { role: "assistant", content: msg.content },
              ]);
            }
          }
          break;
        case "agent.event":
          if (msg.sessionId === id) {
            updateCursor(msg.id);
            const event = msg.event;
            
            // Handle streaming thinking events - accumulate content by thinkingId
            if (event.kind === "thinking" && event.thinkingId && event.partial) {
              setEvents((prev) => {
                const existingIndex = prev.findIndex(
                  (e) => e.kind === "thinking" && e.thinkingId === event.thinkingId
                );
                if (existingIndex >= 0) {
                  // Append to existing thinking event
                  const updated = [...prev];
                  const existing = updated[existingIndex];
                  if (existing.kind === "thinking") {
                    updated[existingIndex] = {
                      ...existing,
                      content: existing.content + event.content,
                    };
                  }
                  return updated;
                } else {
                  // New thinking event
                  return [...prev, event];
                }
              });
            } else {
              // Non-thinking events or complete thinking blocks
              setEvents((prev) => [...prev, event]);
            }
          }
          break;
        case "agent.done":
          if (msg.sessionId === id) {
            updateCursor(msg.id);
            setIsProcessing(false);
          }
          break;
        case "agent.error":
          if (msg.sessionId === id) {
            updateCursor(msg.id);
            setMessages((prev) => [
              ...prev,
              { role: "assistant", content: `Error: ${msg.error}` },
            ]);
            setIsProcessing(false);
          }
          break;
        case "user.message":
          // User message from another client - add if not duplicate
          if (msg.sessionId === id) {
            updateCursor(msg.id);
            setMessages((prev) => {
              // Skip if last message is the same (sent by this client)
              const last = prev[prev.length - 1];
              if (last?.role === "user" && last.content === msg.content) {
                return prev;
              }
              return [...prev, { role: "user", content: msg.content }];
            });
          }
          break;
      }
    },
    [id, updateCursor]
  );

  useWebSocketMessages(handleMessage);

  // Track previous connection state for reconnection detection
  const prevConnectedRef = useRef(false);

  useEffect(() => {
    const wasDisconnected = !prevConnectedRef.current;
    const justConnected = connected && wasDisconnected;

    if (connected && id) {
      const isNewSession = lastOpenedIdRef.current !== id;
      const isReconnection = justConnected && lastOpenedIdRef.current === id;

      if (isNewSession) {
        // Opening a different session - clear state and start fresh
        lastOpenedIdRef.current = id;
        lastNotificationIdRef.current = undefined;
        setMessages([]);
        setEvents([]);
        send({ type: "session.open", id });
        send({ type: "session.resume", id });
      } else if (isReconnection && lastNotificationIdRef.current) {
        // Reconnecting to same session - use cursor to resume from where we left off
        send({
          type: "session.open",
          id,
          lastNotificationId: lastNotificationIdRef.current,
        });
        send({ type: "session.resume", id });
      }
    }
    prevConnectedRef.current = connected;
  }, [connected, id, send]);

  const handleSendPrompt = (text: string) => {
    if (!id) return;
    setMessages((prev) => [...prev, { role: "user", content: text }]);
    setIsProcessing(true);
    setEvents([]);
    send({ type: "prompt", sessionId: id, text });
  };

  const handleInterrupt = () => {
    if (!id) return;
    send({ type: "prompt.interrupt", sessionId: id });
  };

  const handleTerminalInput = (data: string) => {
    if (!id) return;
    send({ type: "terminal.input", sessionId: id, data });
  };

  const getStatusText = () => {
    if (!connected) return "Disconnected";
    if (!session) return "Connecting";
    if (isProcessing) return "Processing";
    if (session.status === "creating") return "Starting sandbox...";
    if (session.status === "ready") return "Ready";
    if (session.status === "running") return "Running";
    if (session.status === "error") return "Error";
    return session.status;
  };

  const getStatusColor = () => {
    if (!connected) return "gray";
    if (!session) return "blue";
    if (isProcessing) return "yellow";
    if (session.status === "running" || session.status === "ready") return "green";
    if (session.status === "error") return "red";
    if (session.status === "creating") return "blue";
    return "gray";
  };

  return (
    <AppShell header={{ height: 60 }} padding={0}>
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between">
          <Group>
            <Button variant="subtle" onClick={() => navigate("/")}>
              ←
            </Button>
            <Box>
              <Text fw={500}>{session?.name || "Session"}</Text>
              <Text size="xs" c="dimmed">
                {id?.slice(0, 8)}
              </Text>
            </Box>
          </Group>
          <Group>
            <Badge color={getStatusColor()} variant="light">
              {getStatusText()}
            </Badge>
            <ThemeToggle />
          </Group>
        </Group>
      </AppShell.Header>

      <AppShell.Main h="calc(100vh - 60px)">
        <Grid h="100%" gutter={0}>
          <Grid.Col span={6} h="100%">
            <Box h="100%" style={{ borderRight: "1px solid var(--mantine-color-default-border)" }}>
              <ChatPanel
                messages={messages}
                events={events}
                onSend={handleSendPrompt}
                onInterrupt={handleInterrupt}
                disabled={!connected || (session?.status !== "running" && session?.status !== "ready")}
                isProcessing={isProcessing}
              />
            </Box>
          </Grid.Col>
          <Grid.Col span={6} h="100%">
            <Terminal onInput={handleTerminalInput} />
          </Grid.Col>
        </Grid>
      </AppShell.Main>
    </AppShell>
  );
}
