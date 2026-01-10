import { useState, useRef, useEffect } from "react";
import {
  Box,
  Paper,
  Text,
  Textarea,
  ActionIcon,
  Group,
  Stack,
  ScrollArea,
  Badge,
  Code,
  Loader,
  Anchor,
  useMantineColorScheme,
} from "@mantine/core";
import type { AgentEvent } from "@netclode/protocol";
import { StreamingMarkdown } from "./StreamingMarkdown";

export interface ChatMessage {
  role: "user" | "assistant";
  content: string;
}

function EventDetails({ event }: { event: AgentEvent }) {
  switch (event.kind) {
    case "tool_start":
      return (
        <Stack gap="xs" p="xs">
          <Group gap="xs">
            <Text size="xs" c="dimmed">Input:</Text>
          </Group>
          <Code block style={{ fontSize: 11, maxHeight: 200, overflow: "auto" }}>
            {JSON.stringify(event.input, null, 2)}
          </Code>
        </Stack>
      );
    case "tool_input":
      return (
        <Stack gap="xs" p="xs">
          <Text size="xs" c="dimmed">Streaming input...</Text>
          <Code block style={{ fontSize: 11, maxHeight: 100, overflow: "auto", opacity: 0.7 }}>
            {event.inputDelta}
          </Code>
        </Stack>
      );
    case "tool_end":
      return (
        <Stack gap="xs" p="xs">
          {event.result && (
            <>
              <Text size="xs" c="dimmed">Result:</Text>
              <TerminalOutput content={event.result} maxHeight={200} />
            </>
          )}
          {event.error && (
            <>
              <Text size="xs" c="red">Error:</Text>
              <Code block color="red" style={{ fontSize: 11 }}>
                {event.error}
              </Code>
            </>
          )}
        </Stack>
      );
    case "file_change":
      return (
        <Stack gap="xs" p="xs">
          <Group gap="xs" wrap="nowrap">
            <Badge
              size="xs"
              color={event.action === "create" ? "green" : event.action === "delete" ? "red" : "blue"}
              variant="light"
            >
              {event.action}
            </Badge>
            <Code style={{ fontSize: 11, flex: 1 }}>{event.path}</Code>
            {(event.linesAdded !== undefined || event.linesRemoved !== undefined) && (
              <Group gap={4}>
                {event.linesAdded !== undefined && event.linesAdded > 0 && (
                  <Text size="xs" c="green" fw={500}>+{event.linesAdded}</Text>
                )}
                {event.linesRemoved !== undefined && event.linesRemoved > 0 && (
                  <Text size="xs" c="red" fw={500}>-{event.linesRemoved}</Text>
                )}
              </Group>
            )}
          </Group>
          {event.diff && (
            <DiffView diff={event.diff} />
          )}
        </Stack>
      );
    case "command_start":
      return (
        <Stack gap="xs" p="xs">
          <TerminalOutput content={`$ ${event.command}`} maxHeight={60} />
          {event.cwd && (
            <Text size="xs" c="dimmed">in {event.cwd}</Text>
          )}
        </Stack>
      );
    case "command_end":
      return (
        <Stack gap="xs" p="xs">
          <Group gap="xs">
            <Badge
              size="xs"
              color={event.exitCode === 0 ? "green" : "red"}
              variant="light"
            >
              exit {event.exitCode}
            </Badge>
          </Group>
          {event.output && (
            <TerminalOutput content={event.output} maxHeight={200} />
          )}
        </Stack>
      );
    case "thinking":
      return (
        <Box p="xs">
          <Group gap="xs" mb={4}>
            <Text size="xs" fw={500} c="grape">Thinking</Text>
            {event.partial && (
              <Loader size={10} color="grape" />
            )}
          </Group>
          <Text 
            size="xs" 
            c="dimmed" 
            style={{ 
              whiteSpace: "pre-wrap", 
              fontStyle: "italic",
              maxHeight: 200,
              overflow: "auto",
            }}
          >
            {event.content}
          </Text>
        </Box>
      );
    case "port_exposed":
      return (
        <Stack gap="xs" p="xs">
          <Group gap="xs">
            <Badge size="xs" color="cyan" variant="light">Port {event.port}</Badge>
            {event.process && <Text size="xs" c="dimmed">{event.process}</Text>}
          </Group>
          {event.previewUrl && (
            <Anchor href={event.previewUrl} target="_blank" size="xs">
              Open preview
            </Anchor>
          )}
        </Stack>
      );
    default:
      return null;
  }
}

// Terminal-style output component
function TerminalOutput({ content, maxHeight = 200 }: { content: string; maxHeight?: number }) {
  const { colorScheme } = useMantineColorScheme();
  const isDark = colorScheme === "dark";

  return (
    <Box
      style={{
        backgroundColor: isDark ? "#1a1b26" : "#f4f4f5",
        borderRadius: 6,
        padding: "8px 10px",
        maxHeight,
        overflow: "auto",
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
        fontSize: 11,
        lineHeight: 1.5,
        whiteSpace: "pre-wrap",
        wordBreak: "break-all",
        color: isDark ? "#a9b1d6" : "#3f3f46",
      }}
    >
      {content}
    </Box>
  );
}

// Diff view component for file changes
function DiffView({ diff }: { diff: string }) {
  const { colorScheme } = useMantineColorScheme();
  const isDark = colorScheme === "dark";
  const lines = diff.split("\n");

  return (
    <Box
      style={{
        backgroundColor: isDark ? "#1a1b26" : "#fafafa",
        borderRadius: 6,
        overflow: "auto",
        maxHeight: 300,
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
        fontSize: 11,
        lineHeight: 1.6,
      }}
    >
      {lines.map((line, i) => {
        let bg = "transparent";
        let color = isDark ? "#a9b1d6" : "#3f3f46";

        if (line.startsWith("+") && !line.startsWith("+++")) {
          bg = isDark ? "rgba(46, 160, 67, 0.15)" : "rgba(46, 160, 67, 0.1)";
          color = isDark ? "#7ee787" : "#1a7f37";
        } else if (line.startsWith("-") && !line.startsWith("---")) {
          bg = isDark ? "rgba(248, 81, 73, 0.15)" : "rgba(248, 81, 73, 0.1)";
          color = isDark ? "#f85149" : "#cf222e";
        } else if (line.startsWith("@@")) {
          color = isDark ? "#a5d6ff" : "#0969da";
        }

        return (
          <Box
            key={i}
            style={{
              backgroundColor: bg,
              padding: "0 8px",
              color,
              whiteSpace: "pre",
            }}
          >
            {line || " "}
          </Box>
        );
      })}
    </Box>
  );
}

// Inline tool event component (Claude Code style) - always shows details
function InlineToolEvent({ event }: { event: AgentEvent }) {
  const { colorScheme } = useMantineColorScheme();
  const isDark = colorScheme === "dark";

  // Skip tool_input events in the main view (too noisy)
  if (event.kind === "tool_input") return null;

  const getIcon = (): string => {
    switch (event.kind) {
      case "tool_start": return "⏳";
      case "tool_end": return event.error ? "✗" : "✓";
      case "file_change": return "📄";
      case "command_start": return "▶";
      case "command_end": return event.exitCode === 0 ? "✓" : "✗";
      case "thinking": return "💭";
      case "port_exposed": return "🌐";
      default: return "•";
    }
  };

  const getLabel = (): string => {
    switch (event.kind) {
      case "tool_start": return `Using ${event.tool}`;
      case "tool_end": return `${event.tool}${event.error ? " failed" : ""}`;
      case "file_change": return `${event.action} ${event.path.split("/").pop()}`;
      case "command_start": return `Running command`;
      case "command_end": return `Command ${event.exitCode === 0 ? "completed" : "failed"}`;
      case "thinking": return event.partial ? "Thinking..." : "Thought";
      case "port_exposed": return `Port ${event.port} opened`;
      default: return String((event as { kind: string }).kind);
    }
  };

  const isInProgress = event.kind === "tool_start" || event.kind === "command_start" || 
                       (event.kind === "thinking" && event.partial);
  const isError = (event.kind === "tool_end" && event.error) ||
                  (event.kind === "command_end" && event.exitCode !== 0);

  // Determine if this event has details to show
  const hasDetails = event.kind === "tool_start" ||
                     event.kind === "tool_end" ||
                     event.kind === "command_start" ||
                     event.kind === "command_end" ||
                     event.kind === "file_change" ||
                     event.kind === "thinking" ||
                     event.kind === "port_exposed";

  return (
    <Box
      style={{
        marginLeft: 32,
        borderLeft: `2px solid ${isDark ? "#3b3b4f" : "#e0e0e0"}`,
        paddingLeft: 12,
      }}
    >
      <Group gap="xs" wrap="nowrap">
        <Text size="sm" c={isError ? "red" : isInProgress ? "yellow" : "green"}>
          {getIcon()}
        </Text>
        <Text size="xs" fw={500} c={isError ? "red" : "dimmed"}>
          {getLabel()}
        </Text>
        {isInProgress && <Loader size={10} />}
      </Group>
      {hasDetails && (
        <Box mt="xs">
          <EventDetails event={event} />
        </Box>
      )}
    </Box>
  );
}


interface ChatPanelProps {
  messages: ChatMessage[];
  events: AgentEvent[];
  onSend: (text: string) => void;
  onInterrupt?: () => void;
  disabled?: boolean;
  isProcessing?: boolean;
}

export function ChatPanel({
  messages,
  events,
  onSend,
  onInterrupt,
  disabled,
  isProcessing,
}: ChatPanelProps) {
  const [input, setInput] = useState("");
  const viewport = useRef<HTMLDivElement>(null);
  const { colorScheme } = useMantineColorScheme();
  const isDark = colorScheme === "dark";

  const userMsgBg = isDark ? "blue.9" : "blue.0";
  const assistantMsgBg = isDark ? "orange.9" : "orange.0";

  useEffect(() => {
    viewport.current?.scrollTo({ top: viewport.current.scrollHeight, behavior: "smooth" });
  }, [messages, events]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (input.trim() && !disabled && !isProcessing) {
      onSend(input.trim());
      setInput("");
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      handleSubmit(e);
    }
  };

  return (
    <Box h="100%" style={{ display: "flex", flexDirection: "column" }}>
      <ScrollArea flex={1} viewportRef={viewport} p="md">
        {messages.length === 0 && !isProcessing && (
          <Paper p="xl" ta="center" withBorder>
            <Text size="xl" mb="xs">💬</Text>
            <Text c="dimmed">Ask Claude anything...</Text>
          </Paper>
        )}
        <Stack gap="md">
          {messages.map((msg, i) => {
            const isLastAssistant = msg.role === "assistant" && i === messages.length - 1;
            const isStreaming = isLastAssistant && isProcessing;

            return (
              <Group key={i} align="flex-start" gap="sm" wrap="nowrap">
                <Text size="lg">{msg.role === "user" ? "👤" : "🧠"}</Text>
                <Box style={{ flex: 1, minWidth: 0 }}>
                  <Text size="xs" fw={500} mb={4}>
                    {msg.role === "user" ? "You" : "Claude"}
                  </Text>
                  <Paper
                    p="sm"
                    bg={msg.role === "user" ? userMsgBg : assistantMsgBg}
                    radius="md"
                  >
                    {msg.role === "user" ? (
                      <Text size="sm" style={{ whiteSpace: "pre-wrap" }}>
                        {msg.content}
                      </Text>
                    ) : (
                      <StreamingMarkdown content={msg.content} isStreaming={isStreaming} />
                    )}
                  </Paper>
                </Box>
              </Group>
            );
          })}
          {/* Tool events shown inline during/after processing */}
          {events.length > 0 && (
            <Stack gap="xs">
              {events.map((event, i) => (
                <InlineToolEvent key={i} event={event} />
              ))}
            </Stack>
          )}
          {isProcessing && events.length === 0 && (
            <Group align="flex-start" gap="sm">
              <Text size="lg">🧠</Text>
              <Loader size="sm" type="dots" />
            </Group>
          )}
        </Stack>
      </ScrollArea>
      <Box p="md" style={{ borderTop: "1px solid var(--mantine-color-default-border)" }}>
        <form onSubmit={handleSubmit}>
          <Group gap="sm" align="flex-end">
            <Textarea
              flex={1}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={handleKeyDown}
              placeholder={disabled ? "Session not ready..." : "Ask Claude..."}
              disabled={disabled}
              minRows={1}
              maxRows={4}
              autosize
            />
            {isProcessing ? (
              <ActionIcon
                size="lg"
                variant="filled"
                color="red"
                onClick={onInterrupt}
                title="Stop"
              >
                ■
              </ActionIcon>
            ) : (
              <ActionIcon
                size="lg"
                variant="filled"
                type="submit"
                disabled={disabled || !input.trim()}
                title="Send"
              >
                ↑
              </ActionIcon>
            )}
          </Group>
        </form>
      </Box>
    </Box>
  );
}
