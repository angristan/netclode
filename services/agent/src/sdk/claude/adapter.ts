/**
 * Claude Agent SDK Adapter
 *
 * Wraps the @anthropic-ai/claude-agent-sdk integration
 */

import { query } from "@anthropic-ai/claude-agent-sdk";
import type { SDKAdapter, SDKConfig, PromptConfig, PromptEvent } from "../types.js";
import { getSdkSessionId, registerSession } from "../../services/session.js";
import { buildSystemPromptText, type SystemPromptConfig } from "../../utils/system-prompt.js";
import {
  createTranslatorState,
  resetTranslatorState,
  translateMessage,
  type TranslatorState,
  type ClaudeMessage,
} from "./translator.js";
import { WORKSPACE_DIR } from "../../constants.js";

/**
 * Build the system prompt in Claude Agent SDK preset format
 */
function buildSystemPrompt(config: SystemPromptConfig): {
  type: "preset";
  preset: "claude_code";
  append: string;
} {
  return {
    type: "preset",
    preset: "claude_code",
    append: buildSystemPromptText(config),
  };
}

export class ClaudeSDKAdapter implements SDKAdapter {
  private config: SDKConfig | null = null;
  private interruptSignal = false;
  private abortController: AbortController | null = null;
  private translatorState: TranslatorState = createTranslatorState();

  async initialize(config: SDKConfig): Promise<void> {
    this.config = config;
    console.log("[claude-adapter] Initialized with workspace:", config.workspaceDir);
  }

  async *executePrompt(sessionId: string, text: string, promptConfig?: PromptConfig): AsyncGenerator<PromptEvent> {
    console.log(
      `[claude-adapter] ExecutePrompt (session=${sessionId}): "${text.slice(0, 100)}${text.length > 100 ? "..." : ""}"`
    );

    // Get repos from config for system prompt
    const currentGitRepos = promptConfig?.repos?.filter(Boolean) ?? [];

    // Reset translator state for new prompt
    resetTranslatorState(this.translatorState);

    // Look up the SDK session ID
    const sdkSessionId = sessionId ? getSdkSessionId(sessionId) : undefined;
    console.log(`[claude-adapter] SDK session lookup: ${sessionId} -> ${sdkSessionId || "(new session)"}`);

    // Clear any previous interrupt signal at start
    this.clearInterruptSignal();

    // Create an AbortController for this query
    this.abortController = new AbortController();

    try {
      const q = query({
        prompt: text,
        options: {
          cwd: WORKSPACE_DIR,
          permissionMode: "bypassPermissions",
          allowDangerouslySkipPermissions: true,
          model: this.config?.model || "claude-opus-4-5-20251101",
          persistSession: true,
          includePartialMessages: true,
          maxThinkingTokens: 10000,
          systemPrompt: buildSystemPrompt({ currentGitRepos }),
          settingSources: ["user", "project", "local"],
          abortController: this.abortController,
          ...(sdkSessionId && { resume: sdkSessionId }),
        },
      });

      for await (const message of q) {
        // Check for interrupt signal at each iteration
        if (this.isInterrupted()) {
          console.log("[claude-adapter] Interrupted by user");
          yield { type: "system", message: "interrupted" };
          return;
        }

        // Translate the message using the translator
        // Cast to access SDK-specific properties that aren't in the base type
        const msg = message as Record<string, unknown>;
        const claudeMessage: ClaudeMessage = {
          type: message.type as ClaudeMessage["type"],
          subtype: msg.subtype as string | undefined,
          session_id: msg.session_id as string | undefined,
          num_turns: msg.num_turns as number | undefined,
          parent_tool_use_id: msg.parent_tool_use_id as string | undefined,
          message: msg.message as ClaudeMessage["message"],
          event: msg.event as ClaudeMessage["event"],
        };

        const result = translateMessage(claudeMessage, this.translatorState);

        // Handle session registration
        if (result.sessionId && sessionId && !getSdkSessionId(sessionId)) {
          registerSession(sessionId, result.sessionId);
        }

        // Yield all translated events
        for (const event of result.events) {
          yield event;
        }
      }
    } catch (error) {
      console.error("[claude-adapter] Error during prompt:", error);
      yield { type: "error", message: String(error), retryable: false };
    }
  }

  setInterruptSignal(): void {
    this.interruptSignal = true;
    // Abort the query to cancel in-flight API calls and tool executions
    if (this.abortController) {
      this.abortController.abort();
      console.log("[claude-adapter] Interrupt signal set and query aborted");
    } else {
      console.log("[claude-adapter] Interrupt signal set");
    }
  }

  clearInterruptSignal(): void {
    this.interruptSignal = false;
    this.abortController = null;
  }

  isInterrupted(): boolean {
    return this.interruptSignal;
  }

  async shutdown(): Promise<void> {
    console.log("[claude-adapter] Shutdown");
    // Claude SDK doesn't need explicit cleanup
  }
}
