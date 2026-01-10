// Package protocol defines the event types exchanged between the control plane,
// agents, and clients (iOS, web).
package protocol

// AgentEventKind identifies the type of event emitted during agent execution.
type AgentEventKind string

const (
	EventKindToolStart    AgentEventKind = "tool_start"    // Agent started using a tool
	EventKindToolInput    AgentEventKind = "tool_input"    // Streaming tool input delta
	EventKindToolEnd      AgentEventKind = "tool_end"      // Tool execution completed
	EventKindFileChange   AgentEventKind = "file_change"   // File was created/edited/deleted
	EventKindCommandStart AgentEventKind = "command_start" // Shell command started
	EventKindCommandEnd   AgentEventKind = "command_end"   // Shell command completed
	EventKindThinking     AgentEventKind = "thinking"      // Agent thinking/reasoning
	EventKindPortExposed  AgentEventKind = "port_exposed"  // Port exposed for preview access
)

// AgentEvent is a polymorphic event type. Fields are optional based on Kind.
type AgentEvent struct {
	Kind         AgentEventKind         `json:"kind"`
	Timestamp    string                 `json:"timestamp"`
	Tool         string                 `json:"tool,omitempty"`
	ToolUseID    string                 `json:"toolUseId,omitempty"`
	Input        map[string]interface{} `json:"input,omitempty"`
	InputDelta   string                 `json:"inputDelta,omitempty"`
	Result       *string                `json:"result,omitempty"`
	Error        *string                `json:"error,omitempty"`
	Path         string                 `json:"path,omitempty"`
	Action       string                 `json:"action,omitempty"`
	Command      string                 `json:"command,omitempty"`
	Cwd          *string                `json:"cwd,omitempty"`
	ExitCode     *int                   `json:"exitCode,omitempty"`
	Output       *string                `json:"output,omitempty"`
	Content      string                 `json:"content,omitempty"`
	ThinkingID   string                 `json:"thinkingId,omitempty"` // Correlate streaming thinking updates
	Partial      bool                   `json:"partial,omitempty"`    // true for streaming thinking deltas
	Port         int                    `json:"port,omitempty"`
	Process      *string                `json:"process,omitempty"`
	PreviewURL   *string                `json:"previewUrl,omitempty"`
	LinesAdded   *int                   `json:"linesAdded,omitempty"`
	LinesRemoved *int                   `json:"linesRemoved,omitempty"`
}
