/**
 * SDK abstraction layer types
 *
 * This module defines the interface for SDK adapters, allowing the agent
 * to use different AI SDKs (Claude Agent SDK, OpenCode SDK, etc.)
 */

import type { JsonObject } from "@bufbuild/protobuf";

export type SdkType = "claude" | "opencode" | "copilot" | "codex";

/**
 * Backend for Copilot SDK sessions
 */
export type CopilotBackend = "github" | "anthropic";

/**
 * Configuration for SDK initialization
 */
export interface SDKConfig {
  sdkType: SdkType;
  workspaceDir: string;
  anthropicApiKey: string;
  openaiApiKey?: string; // OpenAI API key (for Codex API mode)
  mistralApiKey?: string; // Mistral API key (for OpenCode SDK)
  githubCopilotToken?: string; // GitHub PAT with Copilot scope (for Copilot SDK auth)
  model?: string; // e.g., "anthropic/claude-sonnet-4-0" for OpenCode, "codex-mini-latest:api" for Codex
  copilotBackend?: CopilotBackend; // For Copilot SDK: "github" or "anthropic"
  // Codex SDK OAuth tokens (for ChatGPT auth mode)
  codexAccessToken?: string;
  codexIdToken?: string;
  codexRefreshToken?: string;
  // Codex reasoning effort (low, medium, high, minimal, xhigh)
  reasoningEffort?: string;
  // Ollama URL for local inference (e.g., "http://ollama.netclode.svc.cluster.local:11434")
  ollamaUrl?: string;
  // OpenCode Zen API key (for paid models; empty/"public" = free tier only)
  openCodeApiKey?: string;
  zaiApiKey?: string;
  githubCopilotOAuthAccessToken?: string;
  githubCopilotOAuthRefreshToken?: string;
  githubCopilotOAuthTokenExpires?: string;
}

/**
 * Configuration passed to executePrompt
 */
export interface PromptConfig {
  repos?: string[];
  githubToken?: string;
}

/**
 * Event types emitted during prompt execution
 */
export type PromptEvent =
  | { type: "system"; message: string }
  | { type: "textDelta"; content: string; partial: boolean; messageId?: string }
  | { type: "toolStart"; tool: string; toolUseId: string; parentToolUseId?: string; input?: JsonObject }
  | { type: "toolInput"; toolUseId: string; inputDelta: string; parentToolUseId?: string }
  | { type: "toolInputComplete"; toolUseId: string; parentToolUseId?: string; input: JsonObject }
  | { type: "toolEnd"; tool: string; toolUseId: string; result?: string; error?: string; parentToolUseId?: string; durationMs?: number }
  | { type: "thinking"; thinkingId: string; content: string; partial: boolean }
  | { type: "repoClone"; stage: "cloning" | "done" | "error"; repo: string; message: string }
  | { type: "result"; inputTokens: number; outputTokens: number; totalTurns: number }
  | { type: "error"; message: string; retryable: boolean };

/**
 * SDK Adapter interface
 * All SDK implementations must implement this interface
 */
export interface SDKAdapter {
  /**
   * Initialize the SDK adapter
   * Called once when the adapter is created
   */
  initialize(config: SDKConfig): Promise<void>;

  /**
   * Execute a prompt and yield events
   * @param sessionId - Netclode session ID (for session mapping)
   * @param text - The prompt text
   * @param config - Additional configuration (repos, github token)
   */
  executePrompt(sessionId: string, text: string, config?: PromptConfig): AsyncGenerator<PromptEvent>;

  /**
   * Set the interrupt signal to stop prompt execution
   */
  setInterruptSignal(): void;

  /**
   * Clear the interrupt signal
   */
  clearInterruptSignal(): void;

  /**
   * Check if interrupt was requested
   */
  isInterrupted(): boolean;

  /**
   * Shutdown the SDK adapter
   * Called when the agent is shutting down
   */
  shutdown(): Promise<void>;
}
