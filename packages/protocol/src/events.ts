// Agent events emitted during execution
export type AgentEventKind =
  | "tool_start"
  | "tool_input"
  | "tool_end"
  | "file_change"
  | "command_start"
  | "command_end"
  | "thinking"
  | "port_exposed";

export interface BaseAgentEvent {
  kind: AgentEventKind;
  timestamp: string;
}

export interface ToolStartEvent extends BaseAgentEvent {
  kind: "tool_start";
  tool: string;
  toolUseId: string;
  input: Record<string, unknown>;
}

export interface ToolInputEvent extends BaseAgentEvent {
  kind: "tool_input";
  toolUseId: string;
  inputDelta: string; // Partial JSON being streamed
}

export interface ToolEndEvent extends BaseAgentEvent {
  kind: "tool_end";
  tool: string;
  toolUseId: string;
  result?: string;
  error?: string;
}

export interface FileChangeEvent extends BaseAgentEvent {
  kind: "file_change";
  path: string;
  action: "create" | "edit" | "delete";
  linesAdded?: number;
  linesRemoved?: number;
  diff?: string; // Unified diff content (for future use)
}

export interface CommandStartEvent extends BaseAgentEvent {
  kind: "command_start";
  command: string;
  cwd?: string;
}

export interface CommandEndEvent extends BaseAgentEvent {
  kind: "command_end";
  command: string;
  exitCode: number;
  output?: string;
}

export interface ThinkingEvent extends BaseAgentEvent {
  kind: "thinking";
  thinkingId: string; // Correlate streaming updates for same thinking block
  content: string;
  partial: boolean; // true = streaming delta, false = complete block
}

export interface PortExposedEvent extends BaseAgentEvent {
  kind: "port_exposed";
  port: number;
  process?: string;
  previewUrl?: string;
}

export type AgentEvent =
  | ToolStartEvent
  | ToolInputEvent
  | ToolEndEvent
  | FileChangeEvent
  | CommandStartEvent
  | CommandEndEvent
  | ThinkingEvent
  | PortExposedEvent;
