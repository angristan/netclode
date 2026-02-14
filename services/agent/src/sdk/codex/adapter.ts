/**
 * OpenAI Codex SDK Adapter
 *
 * Uses @openai/codex-sdk to communicate with OpenAI's Codex agent.
 *
 * ## Authentication
 *
 * The Codex SDK supports two authentication modes:
 *
 * 1. **API Key Mode** (default)
 *    - Uses OPENAI_API_KEY environment variable
 *    - Standard OpenAI API authentication
 *
 * 2. **ChatGPT OAuth Mode**
 *    - Uses OAuth tokens from ChatGPT login
 *    - Tokens written to ~/.codex/auth.json
 *    - Allows using ChatGPT subscription for Codex
 */

import { Codex, type Thread, type ModelReasoningEffort } from "@openai/codex-sdk";
import type { SDKAdapter, SDKConfig, PromptConfig, PromptEvent } from "../types.js";
import {
  createTranslatorState,
  resetTranslatorState,
  translateEvent,
  storeUsage,
  createResultEvent,
  type TranslatorState,
  type CodexEvent,
} from "./translator.js";
import { getSdkSessionId, registerSession } from "../../services/session.js";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import * as os from "node:os";
import { WORKSPACE_DIR } from "../../constants.js";
import { buildSystemPromptText } from "../../utils/system-prompt.js";

// Codex session ID mapping (Netclode session ID -> Codex thread ID)
const codexThreadMap = new Map<string, string>();
const NETCLODE_PLACEHOLDER_PREFIX = "NETCLODE_PLACEHOLDER_";

type CodexAuthMode = "api" | "oauth" | "unknown";

function hasCodexApiSuffix(model?: string): boolean {
  if (!model) return false;
  return /:api(?::(low|medium|high|minimal|xhigh))?$/.test(model);
}

function hasCodexOAuthSuffix(model?: string): boolean {
  if (!model) return false;
  return /:oauth(?::(low|medium|high|minimal|xhigh))?$/.test(model);
}

function isUsableApiKey(apiKey?: string): boolean {
  if (!apiKey) return false;
  return !apiKey.startsWith(NETCLODE_PLACEHOLDER_PREFIX);
}

export function resolveCodexAuthMode(config: SDKConfig): CodexAuthMode {
  if (hasCodexApiSuffix(config.model)) {
    return "api";
  }
  if (hasCodexOAuthSuffix(config.model)) {
    return "oauth";
  }

  // Without an explicit suffix, prefer OAuth if both token types are available.
  if (config.codexAccessToken && config.codexIdToken) {
    return "oauth";
  }
  if (isUsableApiKey(config.openaiApiKey)) {
    return "api";
  }
  return "unknown";
}
export class CodexAdapter implements SDKAdapter {
  private config: SDKConfig | null = null;
  private codex: Codex | null = null;
  private thread: Thread | null = null;
  private interruptSignal = false;
  private abortController: AbortController | null = null;
  private translatorState: TranslatorState = createTranslatorState();

  // Cleaned model name (without :api/:oauth/:effort suffixes)
  private cleanedModel: string | undefined = undefined;

  // Reasoning effort level (low, medium, high, minimal, xhigh)
  private reasoningEffort: string | undefined = undefined;

  async initialize(config: SDKConfig): Promise<void> {
    this.config = config;

    // Strip :api/:oauth and :effort suffixes from model
    // Format: model:auth:effort (e.g., gpt-5-codex:oauth:high)
    this.cleanedModel = config.model?.replace(/:(api|oauth)(:(low|medium|high|minimal|xhigh))?$/, "");
    this.reasoningEffort = config.reasoningEffort;

    // Determine auth mode from model suffix or available credentials.
    const authMode = resolveCodexAuthMode(config);
    const isApiMode = authMode === "api";
    const isOAuthMode = authMode === "oauth";
    const apiKey = isUsableApiKey(config.openaiApiKey) ? config.openaiApiKey : undefined;

    console.log("[codex-adapter] Initializing");
    console.log("[codex-adapter] Model:", this.cleanedModel || "default");
    console.log("[codex-adapter] Auth mode:", isApiMode ? "API key" : isOAuthMode ? "OAuth" : "unknown");
    console.log("[codex-adapter] Reasoning effort:", this.reasoningEffort || "default");

    // Build clean env object without undefined values
    const buildEnv = (overrides: Record<string, string | undefined> = {}): Record<string, string> => {
      const env: Record<string, string> = {};
      for (const [key, value] of Object.entries(process.env)) {
        if (value !== undefined) {
          env[key] = value;
        }
      }
      for (const [key, value] of Object.entries(overrides)) {
        if (value !== undefined) {
          env[key] = value;
        } else {
          delete env[key];
        }
      }
      return env;
    };

    // Determine which credentials to use based on auth mode
    if (isOAuthMode && config.codexAccessToken && config.codexIdToken) {
      // OAuth mode: write tokens to ~/.codex/auth.json
      // The Codex CLI binary reads credentials from this location
      await this.writeCodexAuth(config.codexAccessToken, config.codexIdToken);
      console.log("[codex-adapter] Using OAuth authentication (ChatGPT subscription)");

      this.codex = new Codex({
        // For OAuth, don't pass apiKey - let it use auth.json
        // Remove any API-key env vars to force OAuth.
        env: buildEnv({ OPENAI_API_KEY: undefined, CODEX_API_KEY: undefined }),
      });
    } else if (isApiMode && apiKey) {
      // API key mode: use OPENAI_API_KEY
      console.log("[codex-adapter] Using API key authentication");

      this.codex = new Codex({
        apiKey,
        env: buildEnv({ OPENAI_API_KEY: apiKey }),
      });
    } else {
      // Fallback: avoid placeholder API keys and rely on existing Codex auth state.
      console.log("[codex-adapter] No explicit auth credentials, using existing Codex auth state");

      this.codex = new Codex({
        env: buildEnv({ OPENAI_API_KEY: undefined, CODEX_API_KEY: undefined }),
      });
    }

    console.log("[codex-adapter] Client created");
  }

  /**
   * Write global AGENTS.md to ~/.codex/ with system prompt
   * Codex reads this file for global instructions
   */
  private async writeGlobalAgentsMd(currentGitRepos: string[]): Promise<void> {
    const systemPromptText = buildSystemPromptText({ currentGitRepos });
    const codexHome = process.env.CODEX_HOME || path.join(os.homedir(), ".codex");

    try {
      await fs.mkdir(codexHome, { recursive: true });
      const agentsMdPath = path.join(codexHome, "AGENTS.md");
      await fs.writeFile(agentsMdPath, systemPromptText, "utf-8");
      console.log("[codex-adapter] Wrote global AGENTS.md to", agentsMdPath);
    } catch (error) {
      console.error("[codex-adapter] Failed to write global AGENTS.md:", error);
    }
  }

  /**
   * Write OAuth tokens to Codex auth file
   * The Codex CLI reads from ~/.codex/auth.json
   */
  private async writeCodexAuth(accessToken: string, idToken: string): Promise<void> {
    const codexHome = process.env.CODEX_HOME || path.join(os.homedir(), ".codex");
    await fs.mkdir(codexHome, { recursive: true });
    const accountId = this.extractAccountIdFromIdToken(idToken);

    const authData = {
      auth_mode: "chatgptAuthTokens",
      OPENAI_API_KEY: null,
      tokens: {
        access_token: accessToken,
        id_token: idToken,
        refresh_token: "",
        account_id: accountId,
      },
      last_refresh: new Date().toISOString(),
    };

    const authPath = path.join(codexHome, "auth.json");
    await fs.writeFile(authPath, JSON.stringify(authData, null, 2), { mode: 0o600 });
    console.log("[codex-adapter] OAuth tokens written to", authPath);
  }

  private extractAccountIdFromIdToken(idToken: string): string | undefined {
    const parts = idToken.split(".");
    if (parts.length !== 3) {
      return undefined;
    }
    try {
      const payload = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf-8"));
      const authClaims = payload?.["https://api.openai.com/auth"];
      if (!authClaims || typeof authClaims !== "object") {
        return undefined;
      }
      return typeof authClaims.chatgpt_account_id === "string" ? authClaims.chatgpt_account_id : undefined;
    } catch {
      return undefined;
    }
  }

  async updateCodexAuth(accessToken: string, idToken: string, _expiresAt?: Date): Promise<void> {
    await this.writeCodexAuth(accessToken, idToken);
    if (this.config) {
      this.config.codexAccessToken = accessToken;
      this.config.codexIdToken = idToken;
    }
    console.log("[codex-adapter] Updated OAuth tokens");
  }

  async *executePrompt(sessionId: string, text: string, promptConfig?: PromptConfig): AsyncGenerator<PromptEvent> {
    if (!this.codex) {
      throw new Error("Codex client not initialized");
    }

    // Reset translator state for new prompt
    resetTranslatorState(this.translatorState);

    console.log(
      `[codex-adapter] ExecutePrompt (session=${sessionId}): "${text.slice(0, 100)}${text.length > 100 ? "..." : ""}"`
    );

    // Write global AGENTS.md with system prompt (includes repo info when available)
    const currentGitRepos = promptConfig?.repos?.filter(Boolean) ?? [];
    await this.writeGlobalAgentsMd(currentGitRepos);

    // Clear interrupt signal
    this.clearInterruptSignal();
    this.abortController = new AbortController();

    // Get or create Codex thread (persisted mapping survives pod restarts)
    const existingThreadId = getSdkSessionId(sessionId);

    try {
      if (existingThreadId) {
        console.log(`[codex-adapter] Resuming Codex thread: ${existingThreadId}`);
        this.thread = this.codex.resumeThread(existingThreadId, {
          workingDirectory: WORKSPACE_DIR,
          sandboxMode: "danger-full-access",
          approvalPolicy: "never",
          model: this.cleanedModel,
          modelReasoningEffort: this.reasoningEffort as ModelReasoningEffort,
          skipGitRepoCheck: true, // We handle git setup ourselves
        });
      } else {
        console.log(`[codex-adapter] Creating new Codex thread`);
        this.thread = this.codex.startThread({
          workingDirectory: WORKSPACE_DIR,
          sandboxMode: "danger-full-access",
          approvalPolicy: "never",
          model: this.cleanedModel,
          modelReasoningEffort: this.reasoningEffort as ModelReasoningEffort,
          skipGitRepoCheck: true, // We handle git setup ourselves
        });
      }
    } catch (error) {
      console.error("[codex-adapter] Failed to create/resume thread:", error);
      yield {
        type: "error",
        message: `Failed to create thread: ${error instanceof Error ? error.message : String(error)}`,
        retryable: true,
      };
      return;
    }

    try {
      // Run the prompt with streaming
      const { events } = await this.thread.runStreamed(text, {
        signal: this.abortController.signal,
      });

      for await (const event of events) {
        if (this.interruptSignal) {
          console.log("[codex-adapter] Interrupted by user");
          yield { type: "error", message: "Prompt interrupted", retryable: true };
          return;
        }

        // Capture thread ID from first event and persist the mapping
        if (event.type === "thread.started" && this.thread.id) {
          registerSession(sessionId, this.thread.id);
        }

        // Track usage from turn.completed
        if (event.type === "turn.completed") {
          storeUsage(event.usage, this.translatorState);
        }

        // Translate and yield events using the translator
        const codexEvent: CodexEvent = {
          type: event.type,
          item: "item" in event ? event.item : undefined,
          error: "error" in event ? event.error : undefined,
          message: "message" in event ? event.message : undefined,
          usage: "usage" in event ? event.usage : undefined,
        };
        const promptEvents = translateEvent(codexEvent, this.translatorState);
        for (const pe of promptEvents) {
          yield pe;
        }
      }

      // Emit final result
      yield createResultEvent(this.translatorState);
    } catch (error) {
      if (this.interruptSignal || this.isAbortError(error)) {
        console.log("[codex-adapter] Prompt interrupted");
        yield { type: "error", message: "Prompt interrupted", retryable: true };
        return;
      }
      console.error("[codex-adapter] Error during prompt execution:", error);
      yield {
        type: "error",
        message: `Prompt execution error: ${error instanceof Error ? error.message : String(error)}`,
        retryable: false,
      };
    } finally {
      this.abortController = null;
    }
  }

  setInterruptSignal(): void {
    this.interruptSignal = true;
    if (this.abortController) {
      this.abortController.abort();
      console.log("[codex-adapter] Interrupt signal set and run aborted");
    } else {
      console.log("[codex-adapter] Interrupt signal set");
    }
  }

  clearInterruptSignal(): void {
    this.interruptSignal = false;
    this.abortController = null;
    resetTranslatorState(this.translatorState);
  }

  isInterrupted(): boolean {
    return this.interruptSignal;
  }

  async shutdown(): Promise<void> {
    console.log("[codex-adapter] Shutting down...");
    if (this.abortController) {
      this.abortController.abort();
      this.abortController = null;
    }
    this.thread = null;
    this.codex = null;
    resetTranslatorState(this.translatorState);
  }

  private isAbortError(error: unknown): boolean {
    if (!(error instanceof Error)) {
      return false;
    }

    if (error.name === "AbortError") {
      return true;
    }

    const message = error.message.toLowerCase();
    return message.includes("aborted") || message.includes("aborterror");
  }
}
