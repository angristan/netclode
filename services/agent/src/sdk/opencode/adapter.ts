/**
 * OpenCode SDK Adapter
 *
 * Spawns opencode serve and communicates via REST API + SSE events
 */

import { spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs/promises";
import * as path from "node:path";
import type { SDKAdapter, SDKConfig, PromptConfig, PromptEvent } from "../types.js";
import {
  createTranslatorState,
  resetTranslatorState,
  translateEvent,
  type TranslatorState,
} from "./translator.js";
import { getSdkSessionId, registerSession } from "../../services/session.js";
import { WORKSPACE_DIR } from "../../constants.js";
import { buildSystemPromptText } from "../../utils/system-prompt.js";
const OPENCODE_PORT = 4096;
const OPENCODE_HOST = "127.0.0.1";

interface OpenCodeServer {
  url: string;
  process: ChildProcess;
}

export class OpenCodeAdapter implements SDKAdapter {
  private config: SDKConfig | null = null;
  private server: OpenCodeServer | null = null;
  private interruptSignal = false;
  private ollamaUrl: string | null = null;
  private translatorState: TranslatorState = createTranslatorState();

  async initialize(config: SDKConfig): Promise<void> {
    this.config = config;
    this.ollamaUrl = config.ollamaUrl || null;
    console.log("[opencode-adapter] Initializing with model:", config.model, "ollamaUrl:", this.ollamaUrl);

    await this.startServer();
  }

  private async writeOpencodeAuthFile(): Promise<void> {
    const refreshToken = this.config?.githubCopilotOAuthRefreshToken;
    if (!refreshToken) return;

    const authDir = "/agent/.local/share/opencode";
    const authFile = path.join(authDir, "auth.json");

    // Write placeholders instead of real tokens so that the secret-proxy can inject
    // the real OAuth access token into outbound request headers. The placeholder value
    // matches the one registered in the control-plane allowlist for SDK_TYPE_OPENCODE
    // (secretKey: "github_copilot_oauth", placeholder: "NETCLODE_PLACEHOLDER_github_copilot_oauth").
    // When opencode sends "Authorization: Bearer NETCLODE_PLACEHOLDER_github_copilot_oauth" to
    // api.githubcopilot.com, the secret-proxy replaces the placeholder with the real OAuth token.
    const authContent = {
      "github-copilot": {
        type: "oauth",
        refresh: "NETCLODE_PLACEHOLDER_github_copilot_oauth",
        access: "NETCLODE_PLACEHOLDER_github_copilot_oauth",
        expires: 0,
      },
    };

    try {
      await fs.mkdir(authDir, { recursive: true });
      await fs.writeFile(authFile, JSON.stringify(authContent, null, 2), { encoding: "utf-8", mode: 0o600 });
      console.log("[opencode-adapter] Wrote opencode auth.json for GitHub Copilot (OAuth, placeholder tokens)");
    } catch (error) {
      console.error("[opencode-adapter] Failed to write opencode auth.json:", error);
    }
  }

  private async startServer(): Promise<void> {
    if (this.server) {
      console.log("[opencode-adapter] Server already running at", this.server.url);
      return;
    }

    await this.writeOpencodeAuthFile();

    const startTime = Date.now();
    console.log("[opencode-adapter] Starting opencode serve...");

    const args = ["serve", `--hostname=${OPENCODE_HOST}`, `--port=${OPENCODE_PORT}`];

    const model = this.config?.model || "anthropic/claude-sonnet-4-0";
    const thinkingLevel = this.config?.reasoningEffort;
    const [providerId, modelName] = model.includes("/") ? model.split("/", 2) : ["anthropic", model];
    const thinkingBudget = thinkingLevel === "max" ? 32000 : thinkingLevel === "high" ? 16000 : 0;

    const isZenModel = providerId === "opencode";
    const isCopilotModel = providerId === "github-copilot";

    let providerConfig: Record<string, unknown> = {};

    if (thinkingBudget > 0) {
      providerConfig[providerId] = {
        models: {
          [modelName]: {
            options: {
              thinking: {
                type: "enabled",
                budgetTokens: thinkingBudget,
              },
            },
          },
        },
      };
    }

    if (providerId === "ollama" && this.ollamaUrl) {
      console.log("[opencode-adapter] Configuring Ollama provider with URL:", this.ollamaUrl);
      // Ollama requires @ai-sdk/openai-compatible with /v1 endpoint
      const ollamaBaseUrl = this.ollamaUrl.endsWith("/v1")
        ? this.ollamaUrl
        : this.ollamaUrl.replace(/\/$/, "") + "/v1";
      providerConfig["ollama"] = {
        npm: "@ai-sdk/openai-compatible",
        name: "Ollama",
        options: {
          baseURL: ollamaBaseUrl,
        },
        models: {
          [modelName]: {
            name: modelName,
            tools: true,
            // reasoning: true enables thinking mode for compatible models
            reasoning: true,
          },
        },
      };
    }

    const opencodeConfig = {
      model,
      logLevel: "INFO",
      // Reference our custom instructions file (written at executePrompt time)
      instructions: [".netclode-instructions.md"],
      permission: {
        edit: "allow",
        bash: "allow",
        webfetch: "allow",
        mcp: "allow",
      },
      ...(Object.keys(providerConfig).length > 0 && { provider: providerConfig }),
    };

    const proc = spawn("opencode", args, {
      env: {
        ...process.env,
        OPENCODE_CONFIG_CONTENT: JSON.stringify(opencodeConfig),
        ANTHROPIC_API_KEY: this.config?.anthropicApiKey || process.env.ANTHROPIC_API_KEY,
        ...(this.config?.openaiApiKey && { OPENAI_API_KEY: this.config.openaiApiKey }),
        ...(this.config?.mistralApiKey && { MISTRAL_API_KEY: this.config.mistralApiKey }),
        // OpenCode Zen API key: use configured key, or "public" for free tier if Zen model
        ...(this.config?.openCodeApiKey
          ? { OPENCODE_API_KEY: this.config.openCodeApiKey }
          : isZenModel && { OPENCODE_API_KEY: "public" }),
        // Z.AI API key for GLM-4.7 models (models.dev uses ZHIPU_API_KEY)
        ...(this.config?.zaiApiKey && { ZHIPU_API_KEY: this.config.zaiApiKey }),
        OPENCODE_DISABLE_DEFAULT_PLUGINS: "true",
        // Disable models fetch except for providers that discover models dynamically
        ...(!isZenModel && !isCopilotModel && { OPENCODE_DISABLE_MODELS_FETCH: "true" }),
      },
      stdio: ["pipe", "pipe", "pipe"],
      cwd: WORKSPACE_DIR,
    });

    const url = await new Promise<string>((resolve, reject) => {
      const timeout = setTimeout(() => {
        proc.kill();
        reject(new Error("Timeout waiting for opencode server to start"));
      }, 60000);

      let stdout = "";
      let stderr = "";
      let resolved = false;

      const doResolve = (url: string) => {
        if (resolved) return;
        resolved = true;
        clearInterval(pollInterval);
        clearTimeout(timeout);
        resolve(url);
      };

      proc.stdout?.on("data", (chunk) => {
        stdout += chunk.toString();
        console.log("[opencode-adapter] stdout:", chunk.toString().trim());
      });

      proc.stderr?.on("data", (chunk) => {
        stderr += chunk.toString();
        console.error("[opencode-adapter] stderr:", chunk.toString().trim());
      });

      proc.on("exit", (code) => {
        if (resolved) return;
        clearInterval(pollInterval);
        clearTimeout(timeout);
        reject(new Error(`opencode serve exited with code ${code}\nstdout: ${stdout}\nstderr: ${stderr}`));
      });

      proc.on("error", (error) => {
        if (resolved) return;
        clearInterval(pollInterval);
        clearTimeout(timeout);
        reject(error);
      });

      let pollCount = 0;
      const pollInterval = setInterval(async () => {
        if (resolved) return;
        pollCount++;
        const elapsed = Date.now() - startTime;
        if (pollCount === 1 || pollCount % 50 === 0) {
          console.log(`[opencode-adapter] Health check attempt ${pollCount}, elapsed ${elapsed}ms`);
        }
        try {
          const res = await fetch(`http://${OPENCODE_HOST}:${OPENCODE_PORT}/session`, {
            method: "GET",
            signal: AbortSignal.timeout(1000),
          });
          if (res.ok) {
            console.log(`[opencode-adapter] Server responded to health check after ${pollCount} attempts, ${elapsed}ms`);
            doResolve(`http://${OPENCODE_HOST}:${OPENCODE_PORT}`);
          }
        } catch {
          // Server not ready yet
        }
      }, 200);
    });

    this.server = { url, process: proc };
    console.log("[opencode-adapter] Server started at:", url);
  }

  /**
   * Write custom instructions file to workspace
   * OpenCode reads this via the `instructions` config field
   */
  private async writeInstructionsFile(currentGitRepos: string[]): Promise<void> {
    const systemPromptText = buildSystemPromptText({ currentGitRepos });
    const instructionsPath = path.join(WORKSPACE_DIR, ".netclode-instructions.md");

    try {
      await fs.writeFile(instructionsPath, systemPromptText, "utf-8");
      console.log("[opencode-adapter] Wrote .netclode-instructions.md with system prompt");
    } catch (error) {
      console.error("[opencode-adapter] Failed to write instructions file:", error);
    }
  }

  private async abortSession(sessionId: string | undefined): Promise<void> {
    if (!this.server || !sessionId) return;

    try {
      const response = await fetch(`${this.server.url}/session/${sessionId}/abort`, {
        method: "POST",
        headers: { "x-opencode-directory": WORKSPACE_DIR },
      });

      if (response.ok) {
        console.log(`[opencode-adapter] Session ${sessionId} aborted`);
      } else {
        console.warn(`[opencode-adapter] Failed to abort session: ${response.statusText}`);
      }
    } catch (error) {
      console.error("[opencode-adapter] Error aborting session:", error);
    }
  }

  async *executePrompt(sessionId: string, text: string, promptConfig?: PromptConfig): AsyncGenerator<PromptEvent> {
    if (!this.server) {
      throw new Error("OpenCode server not initialized");
    }

    console.log(
      `[opencode-adapter] ExecutePrompt (session=${sessionId}): "${text.slice(0, 100)}${text.length > 100 ? "..." : ""}"`
    );

    // Write instructions file with system prompt (includes repo info when available)
    const currentGitRepos = promptConfig?.repos?.filter(Boolean) ?? [];
    await this.writeInstructionsFile(currentGitRepos);

    // Get or create OpenCode session (persisted mapping survives pod restarts)
    let ocSessionId = getSdkSessionId(sessionId);

    if (!ocSessionId) {
      const createResponse = await fetch(`${this.server.url}/session`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-opencode-directory": WORKSPACE_DIR,
        },
        body: JSON.stringify({}),
      });

      if (!createResponse.ok) {
        const errorText = await createResponse.text();
        throw new Error(`Failed to create OpenCode session: ${createResponse.statusText} - ${errorText}`);
      }

      const sessionData = (await createResponse.json()) as { id: string };
      ocSessionId = sessionData.id;
      registerSession(sessionId, ocSessionId);
      console.log(`[opencode-adapter] Created OpenCode session: ${ocSessionId}`);
    } else {
      console.log(`[opencode-adapter] Reusing OpenCode session: ${ocSessionId}`);
    }

    // Clear interrupt signal and reset translator state
    this.clearInterruptSignal();

    const eventQueue: PromptEvent[] = [];
    let resolveNextEvent: ((value: PromptEvent | null) => void) | null = null;
    let completed = false;

    const eventUrl = `${this.server.url}/event?directory=${encodeURIComponent(WORKSPACE_DIR)}`;

    let resolveConnected!: () => void;
    let rejectConnected!: (err: Error) => void;
    let sseConnectionEstablished = false;
    const sseConnected = new Promise<void>((res, rej) => {
      resolveConnected = res;
      rejectConnected = rej;
    });

    const processEvents = async () => {
      try {
        const eventResponse = await fetch(eventUrl, {
          headers: { Accept: "text/event-stream" },
        });

        if (!eventResponse.ok || !eventResponse.body) {
          const err = new Error(`Failed to subscribe to events: ${eventResponse.statusText}`);
          rejectConnected(err);
          throw err;
        }

        sseConnectionEstablished = true;
        resolveConnected();

        const reader = eventResponse.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (!this.interruptSignal && !completed) {
          const { done, value } = await reader.read();
          if (done) break;

          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split("\n");
          buffer = lines.pop() || "";

          for (const line of lines) {
            if (line.startsWith("data: ")) {
              const data = line.slice(6);
              if (data === "[DONE]") {
                completed = true;
                break;
              }
              try {
                const event = JSON.parse(data);
                const promptEvent = translateEvent(event, this.translatorState);
                if (promptEvent) {
                  if (resolveNextEvent) {
                    resolveNextEvent(promptEvent);
                    resolveNextEvent = null;
                  } else {
                    eventQueue.push(promptEvent);
                  }

                  if (promptEvent.type === "result" || promptEvent.type === "error") {
                    completed = true;
                  }
                }
              } catch (e) {
                console.error("[opencode-adapter] Failed to parse event:", e, "data:", data);
              }
            }
          }
        }

        reader.releaseLock();
      } catch (error) {
        console.error("[opencode-adapter] Event stream error:", error);
        if (!sseConnectionEstablished) {
          rejectConnected(error instanceof Error ? error : new Error(String(error)));
        }
        const errorEvent: PromptEvent = {
          type: "error",
          message: `Event stream error: ${error instanceof Error ? error.message : String(error)}`,
          retryable: false,
        };
        if (resolveNextEvent) {
          resolveNextEvent(errorEvent);
          resolveNextEvent = null;
        } else {
          eventQueue.push(errorEvent);
        }
      } finally {
        completed = true;
        if (resolveNextEvent) {
          resolveNextEvent(null);
        }
      }
    };

    const eventProcessor = processEvents();

    try {
      await sseConnected;
    } catch (error) {
      yield {
        type: "error",
        message: `Failed to connect to OpenCode event stream: ${error instanceof Error ? error.message : String(error)}`,
        retryable: false,
      };
      return;
    }
    console.log("[opencode-adapter] SSE connection established, sending prompt");

    try {
      const promptResponse = await fetch(`${this.server.url}/session/${ocSessionId}/prompt_async`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "x-opencode-directory": WORKSPACE_DIR,
        },
        body: JSON.stringify({
          parts: [{ type: "text", text }],
        }),
      });

      if (!promptResponse.ok) {
        const errorText = await promptResponse.text();
        throw new Error(`Failed to send prompt: ${promptResponse.statusText} - ${errorText}`);
      }
    } catch (error) {
      yield {
        type: "error",
        message: `Failed to send prompt: ${error instanceof Error ? error.message : String(error)}`,
        retryable: false,
      };
      return;
    }

    while (!completed || eventQueue.length > 0) {
      if (this.interruptSignal) {
        await this.abortSession(ocSessionId);
        yield { type: "system", message: "interrupted" };
        return;
      }

      if (eventQueue.length > 0) {
        yield eventQueue.shift()!;
      } else if (!completed) {
        const event = await new Promise<PromptEvent | null>((resolve) => {
          resolveNextEvent = resolve;
        });
        if (event) {
          yield event;
        }
      }
    }

    await eventProcessor;
  }

  setInterruptSignal(): void {
    this.interruptSignal = true;
    console.log("[opencode-adapter] Interrupt signal set");
  }

  clearInterruptSignal(): void {
    this.interruptSignal = false;
    resetTranslatorState(this.translatorState);
  }

  isInterrupted(): boolean {
    return this.interruptSignal;
  }

  async shutdown(): Promise<void> {
    console.log("[opencode-adapter] Shutting down...");

    if (this.server) {
      this.server.process.kill();
      this.server = null;
    }

    resetTranslatorState(this.translatorState);
  }
}
