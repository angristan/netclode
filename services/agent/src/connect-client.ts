/**
 * Connect client for bidirectional communication with control plane.
 *
 * The agent connects TO the control plane (not the other way around),
 * establishing a bidirectional stream for all communication.
 */

import { writeFileSync, readFileSync, existsSync } from "fs";
import { createClient } from "@connectrpc/connect";
import { createGrpcTransport } from "@connectrpc/connect-node";
import { create } from "@bufbuild/protobuf";
import { timestampNow } from "@bufbuild/protobuf/wkt";

import {
  AgentService,
  AgentMessageSchema,
  AgentStreamResponseSchema,
  AgentTitleResponseSchema,
  AgentGitStatusResponseSchema,
  AgentGitDiffResponseSchema,
  AgentTerminalOutputSchema,
  AgentRegisterSchema,
  type AgentMessage,
  type AgentRegister,
  type ControlPlaneMessage,
  type AgentStreamResponse,
} from "../gen/netclode/v1/agent_pb.js";
import {
  AgentEventKind,
  AgentEventSchema,
  ToolStartPayloadSchema,
  ToolInputPayloadSchema,
  ToolOutputPayloadSchema,
  ToolEndPayloadSchema,
  ThinkingPayloadSchema,
  RepoClonePayloadSchema,
  RepoCloneStage,
} from "../gen/netclode/v1/events_pb.js";
import {
  GitFileStatus,
  GitFileChangeSchema,
  CopilotBackend as ProtoCopilotBackend,
  type SessionConfig,
} from "../gen/netclode/v1/common_pb.js";

// Import modular services
import { handleTerminalInput, resizeTerminal, setTerminalOutputCallback } from "./services/terminal.js";
import { generateTitle } from "./services/title.js";
import { getGitStatus, getGitDiff, configureGitCredentials, getRepoPath, getRepoPrefix, type GitFileChange } from "./git.js";

// Import SDK abstraction layer
import {
  createSDKAdapter,
  type SDKAdapter,
  type PromptEvent,
  type SdkType,
  type CopilotBackend,
} from "./sdk/index.js";
import { SdkType as ProtoSdkType } from "../gen/netclode/v1/common_pb.js";
import { initializeSessionRepos } from "./services/session.js";
import { WORKSPACE_DIR } from "./constants.js";

// Track if a prompt is currently running (to prevent concurrent prompts)
let isPromptRunning = false;

// Current SDK adapter for the session
let currentAdapter: SDKAdapter | null = null;

/**
 * Convert proto SdkType enum to internal SdkType string
 */
function parseSdkTypeFromProto(protoSdkType: ProtoSdkType | undefined): SdkType {
  switch (protoSdkType) {
    case ProtoSdkType.OPENCODE:
      return "opencode";
    case ProtoSdkType.COPILOT:
      return "copilot";
    case ProtoSdkType.CODEX:
      return "codex";
    case ProtoSdkType.CLAUDE:
    case ProtoSdkType.UNSPECIFIED:
    default:
      return "claude";
  }
}

/**
 * Convert proto CopilotBackend enum to internal CopilotBackend string
 */
function parseCopilotBackendFromProto(protoBackend: ProtoCopilotBackend | undefined): CopilotBackend | undefined {
  switch (protoBackend) {
    case ProtoCopilotBackend.GITHUB:
      return "github";
    case ProtoCopilotBackend.ANTHROPIC:
      return "anthropic";
    case ProtoCopilotBackend.UNSPECIFIED:
    default:
      return undefined; // Let adapter auto-detect
  }
}

/**
 * Convert local repo clone stage to protobuf enum
 */
function convertRepoCloneStage(stage: "cloning" | "done" | "error"): RepoCloneStage {
  switch (stage) {
    case "cloning": return RepoCloneStage.CLONING;
    case "done": return RepoCloneStage.DONE;
    case "error": return RepoCloneStage.ERROR;
    default: return RepoCloneStage.UNSPECIFIED;
  }
}

/**
 * Convert local git status to protobuf enum
 */
function convertGitStatus(status: GitFileChange["status"]): GitFileStatus {
  switch (status) {
    case "modified": return GitFileStatus.MODIFIED;
    case "added": return GitFileStatus.ADDED;
    case "deleted": return GitFileStatus.DELETED;
    case "renamed": return GitFileStatus.RENAMED;
    case "untracked": return GitFileStatus.UNTRACKED;
    case "copied": return GitFileStatus.COPIED;
    case "ignored": return GitFileStatus.IGNORED;
    case "unmerged": return GitFileStatus.UNMERGED;
    default: return GitFileStatus.UNSPECIFIED;
  }
}

/**
 * Convert prompt event to AgentMessage
 */
function promptEventToAgentMessage(event: PromptEvent): AgentMessage {
  const timestamp = timestampNow();
  let response: AgentStreamResponse["response"];

  switch (event.type) {
    case "system":
      response = {
        case: "systemMessage",
        value: { $typeName: "netclode.v1.AgentSystemMessage", message: event.message },
      };
      break;

    case "textDelta":
      response = {
        case: "textDelta",
        value: {
          $typeName: "netclode.v1.AgentTextDelta",
          content: event.content,
          partial: event.partial,
          messageId: event.messageId || "",
        },
      };
      break;

    case "toolStart":
      response = {
        case: "event",
        value: create(AgentEventSchema, {
          kind: AgentEventKind.TOOL_START,
          correlationId: event.toolUseId,
          payload: {
            case: "toolStart",
            value: create(ToolStartPayloadSchema, {
              tool: event.tool,
              ...(event.parentToolUseId && { parentToolUseId: event.parentToolUseId }),
            }),
          },
        }),
      };
      break;

    case "toolInput":
      // Streaming input delta
      response = {
        case: "event",
        value: create(AgentEventSchema, {
          kind: AgentEventKind.TOOL_INPUT,
          correlationId: event.toolUseId,
          payload: {
            case: "toolInput",
            value: create(ToolInputPayloadSchema, {
              delta: event.inputDelta,
            }),
          },
        }),
      };
      break;

    case "toolInputComplete":
      // Complete input (partial=false will be set at control-plane level)
      response = {
        case: "event",
        value: create(AgentEventSchema, {
          kind: AgentEventKind.TOOL_INPUT,
          correlationId: event.toolUseId,
          payload: {
            case: "toolInput",
            value: create(ToolInputPayloadSchema, {
              input: event.input,
            }),
          },
        }),
      };
      break;

    case "toolEnd":
      response = {
        case: "event",
        value: create(AgentEventSchema, {
          kind: AgentEventKind.TOOL_END,
          correlationId: event.toolUseId,
          payload: {
            case: "toolEnd",
            value: create(ToolEndPayloadSchema, {
              success: !event.error,
              error: event.error,
              ...(event.durationMs !== undefined && { durationMs: BigInt(event.durationMs) }),
              ...(event.result && { result: event.result }),
            }),
          },
        }),
      };
      break;

    case "thinking":
      response = {
        case: "event",
        value: create(AgentEventSchema, {
          kind: AgentEventKind.THINKING,
          correlationId: event.thinkingId,
          payload: {
            case: "thinking",
            value: create(ThinkingPayloadSchema, {
              content: event.content,
              partial: event.partial,
            }),
          },
        }),
      };
      break;

    case "repoClone":
      response = {
        case: "event",
        value: create(AgentEventSchema, {
          kind: AgentEventKind.REPO_CLONE,
          correlationId: event.repo || "",
          payload: {
            case: "repoClone",
            value: create(RepoClonePayloadSchema, {
              stage: convertRepoCloneStage(event.stage),
              repo: event.repo,
              message: event.message,
            }),
          },
        }),
      };
      break;

    case "result":
      response = {
        case: "result",
        value: {
          $typeName: "netclode.v1.AgentResult",
          inputTokens: event.inputTokens,
          outputTokens: event.outputTokens,
          totalTurns: event.totalTurns,
        },
      };
      break;

    case "error":
      response = {
        case: "error",
        value: { $typeName: "netclode.v1.AgentError", message: event.message, retryable: event.retryable },
      };
      break;

    default:
      response = { case: undefined, value: undefined };
  }

  return create(AgentMessageSchema, {
    message: {
      case: "promptResponse",
      value: create(AgentStreamResponseSchema, { response }),
    },
  });
}

/**
 * Agent connection state
 */
interface AgentConnection {
  sessionConfig: SessionConfig | null;
  send: (msg: AgentMessage) => void;
}

let connection: AgentConnection | null = null;

function getRepoConfigs(): Array<{ repo: string; dir: string; prefix: string }> {
  const repos = connection?.sessionConfig?.repos ?? [];
  const totalRepos = repos.length;
  return repos.map((repo) => {
    const prefix = getRepoPrefix(repo, totalRepos);
    const dir = getRepoPath(repo, totalRepos, WORKSPACE_DIR);
    return { repo, dir, prefix };
  });
}

/**
 * Connect to the control plane and handle bidirectional communication.
 * In warm pool mode (no sessionId), authenticates using Kubernetes ServiceAccount token.
 */
export async function connectToControlPlane(
  controlPlaneUrl: string,
  sessionId: string | undefined
): Promise<void> {
  console.log(`[agent] Connecting to control plane at ${controlPlaneUrl}`);

  const transport = createGrpcTransport({
    baseUrl: controlPlaneUrl,
  });

  const client = createClient(AgentService, transport);

  // Create async generator for outgoing messages
  const outgoingMessages: AgentMessage[] = [];
  let resolveNext: ((value: AgentMessage) => void) | null = null;

  const sendMessage = (msg: AgentMessage) => {
    if (resolveNext) {
      resolveNext(msg);
      resolveNext = null;
    } else {
      outgoingMessages.push(msg);
    }
  };

  // Read Kubernetes ServiceAccount token for authentication (required for all modes)
  const k8sTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token";
  let k8sToken: string | undefined;

  if (existsSync(k8sTokenPath)) {
    try {
      k8sToken = readFileSync(k8sTokenPath, "utf-8").trim();
      console.log("[agent] Read Kubernetes ServiceAccount token for authentication");
    } catch (err) {
      console.error("[agent] Failed to read Kubernetes token:", err);
    }
  } else {
    console.warn("[agent] Kubernetes token not found at", k8sTokenPath);
  }

  if (!k8sToken) {
    throw new Error("Kubernetes ServiceAccount token required for authentication");
  }

  // Build registration message
  // - Direct mode: include sessionId + k8sToken
  // - Warm pool mode: only k8sToken (session assigned later via gRPC push)
  const registerValue: AgentRegister = create(AgentRegisterSchema, {
    k8sToken,
    version: "1.0.0",
    ...(sessionId && { sessionId }),
  });

  async function* messageGenerator(): AsyncIterable<AgentMessage> {
    // First, send registration (with sessionId for direct mode, podName for warm pool mode)
    yield create(AgentMessageSchema, {
      message: {
        case: "register",
        value: registerValue,
      },
    });

    // Then yield any queued or future messages
    while (true) {
      if (outgoingMessages.length > 0) {
        yield outgoingMessages.shift()!;
      } else {
        yield await new Promise<AgentMessage>((resolve) => {
          resolveNext = resolve;
        });
      }
    }
  }

  // Set up terminal output callback to forward to CP
  setTerminalOutputCallback((data: string) => {
    if (connection) {
      connection.send(
        create(AgentMessageSchema, {
          message: {
            case: "terminalOutput",
            value: create(AgentTerminalOutputSchema, { data }),
          },
        })
      );
    }
  });

  // Track current session ID - may be assigned later in warm pool mode
  let currentSessionId = sessionId;

  try {
    // Start bidirectional stream
    const stream = client.connect(messageGenerator());

    connection = {
      sessionConfig: null,
      send: sendMessage,
    };

    // Handle incoming messages from control plane
    for await (const msg of stream) {
      // sessionAssigned updates currentSessionId (warm pool mode)
      if (msg.message.case === "sessionAssigned") {
        currentSessionId = msg.message.value.sessionId;
        console.log(`[agent] Session assigned via push: ${currentSessionId}`);
      }
      await handleControlPlaneMessage(currentSessionId || "", msg, sendMessage);
    }
  } finally {
    // Clear terminal output callback
    setTerminalOutputCallback(null);
    connection = null;

    // Shutdown SDK adapters
    if (currentAdapter) {
      try {
        await currentAdapter.shutdown();
      } catch (err) {
        console.error("[agent] Error shutting down SDK adapter:", err);
      }
      currentAdapter = null;
    }

    console.log("[agent] Disconnected from control plane");
  }
}

/**
 * Handle a message from the control plane.
 */
async function handleControlPlaneMessage(
  sessionId: string,
  msg: ControlPlaneMessage,
  send: (msg: AgentMessage) => void
): Promise<void> {
  switch (msg.message.case) {
    case "registered":
      if (msg.message.value.success) {
        console.log("[agent] Registered with control plane");
        // Create ready file AFTER registration - this ensures the pod is only
        // marked ready (and claimable by warm pool) after the gRPC connection is established
        try {
          writeFileSync("/tmp/agent-ready", "ready");
          console.log("[agent] Ready file created");
        } catch (e) {
          console.warn("[agent] Could not create ready file:", e);
        }
        if (connection && msg.message.value.config) {
          connection.sessionConfig = msg.message.value.config;

          // Initialize SDK adapter based on session config
          const config = msg.message.value.config;
          const sdkType = parseSdkTypeFromProto(config.sdkType);
          const copilotBackend = parseCopilotBackendFromProto(config.copilotBackend);
          console.log(`[agent] Initializing SDK adapter: ${sdkType}, model: ${config.model || "(default)"}, copilotBackend: ${copilotBackend || "(auto)"}`);

          try {
            currentAdapter = await createSDKAdapter({
              sdkType,
              workspaceDir: WORKSPACE_DIR,
              anthropicApiKey: process.env.ANTHROPIC_API_KEY || "",
              githubCopilotToken: process.env.GITHUB_COPILOT_TOKEN || "",
              model: config.model,
              copilotBackend,
              openaiApiKey: process.env.OPENAI_API_KEY || "",
              codexAccessToken: config.codexAccessToken,
              codexIdToken: config.codexIdToken,
              codexRefreshToken: config.codexRefreshToken,
              reasoningEffort: config.reasoningEffort,
              mistralApiKey: process.env.MISTRAL_API_KEY || "",
              ollamaUrl: config.ollamaUrl,
              openCodeApiKey: process.env.OPENCODE_API_KEY || "",
              zaiApiKey: process.env.ZAI_API_KEY || "",
              githubCopilotOAuthAccessToken: config.githubCopilotOauthAccessToken,
              githubCopilotOAuthRefreshToken: config.githubCopilotOauthRefreshToken,
              githubCopilotOAuthTokenExpires: config.githubCopilotOauthTokenExpires,
            });
          } catch (err) {
            console.error("[agent] Failed to initialize SDK adapter:", err);
            throw new Error(`SDK initialization failed: ${err instanceof Error ? err.message : String(err)}`);
          }
        }
      } else {
        console.error("[agent] Registration failed:", msg.message.value.error);
        throw new Error(`Registration failed: ${msg.message.value.error}`);
      }
      break;

    case "executePrompt":
      if (isPromptRunning) {
        console.warn("[agent] Prompt already running, ignoring new prompt");
        break;
      }
      // Don't await - run concurrently so interrupt/terminal messages can be processed
      isPromptRunning = true;
      handleExecutePrompt(sessionId, msg.message.value.text, send)
        .catch((err) => {
          console.error("[agent] Prompt execution error (async):", err);
        })
        .finally(() => {
          isPromptRunning = false;
        });
      break;

    case "interrupt":
      console.log("[agent] Interrupt requested");
      if (currentAdapter) {
        currentAdapter.setInterruptSignal();
      }
      break;

    case "generateTitle":
      await handleGenerateTitle(msg.message.value.requestId, msg.message.value.prompt, send);
      break;

    case "getGitStatus":
      await handleGetGitStatus(msg.message.value.requestId, send);
      break;

    case "getGitDiff":
      await handleGetGitDiff(msg.message.value.requestId, msg.message.value.file, send);
      break;

    case "terminalInput":
      handleTerminalInputMessage(msg.message.value);
      break;

    case "updateGitCredentials":
      await handleUpdateGitCredentials(msg.message.value);
      break;

    case "sessionAssigned":
      // Warm pool mode: session was assigned to us
      await handleSessionAssigned(sessionId, msg.message.value, send);
      break;

    default:
      console.warn("[agent] Unknown control plane message:", msg.message.case);
  }
}

/**
 * Handle update git credentials request
 */
async function handleUpdateGitCredentials(credentials: {
  githubToken: string;
  repoAccess: number;
}): Promise<void> {
  console.log("[agent] Updating git credentials, new access level:", credentials.repoAccess);
  try {
    await configureGitCredentials(credentials.githubToken);
    console.log("[agent] Git credentials updated successfully");
  } catch (error) {
    console.error("[agent] Failed to update git credentials:", error);
  }
}

/**
 * Handle session assigned (warm pool mode) - initialize SDK with pushed config
 */
async function handleSessionAssigned(
  _sessionId: string | undefined,
  assigned: { sessionId: string; config?: SessionConfig },
  _send: (msg: AgentMessage) => void
): Promise<void> {
  console.log("[agent] Session assigned via push:", assigned.sessionId);

  if (!assigned.config) {
    console.error("[agent] SessionAssigned missing config");
    return;
  }

  const config = assigned.config;

  // Update connection config
  if (connection) {
    connection.sessionConfig = config;
  }

  // Initialize SDK adapter based on session config
  const sdkType = parseSdkTypeFromProto(config.sdkType);
  const copilotBackend = parseCopilotBackendFromProto(config.copilotBackend);
  console.log(`[agent] Initializing SDK adapter (warm pool): ${sdkType}, model: ${config.model || "(default)"}, copilotBackend: ${copilotBackend || "(auto)"}`);

  try {
    currentAdapter = await createSDKAdapter({
      sdkType,
      workspaceDir: WORKSPACE_DIR,
      anthropicApiKey: process.env.ANTHROPIC_API_KEY || "",
      githubCopilotToken: process.env.GITHUB_COPILOT_TOKEN || "",
      model: config.model,
      copilotBackend,
      openaiApiKey: process.env.OPENAI_API_KEY || "",
      codexAccessToken: config.codexAccessToken,
      codexIdToken: config.codexIdToken,
      codexRefreshToken: config.codexRefreshToken,
      reasoningEffort: config.reasoningEffort,
      mistralApiKey: process.env.MISTRAL_API_KEY || "",
      ollamaUrl: config.ollamaUrl,
      openCodeApiKey: process.env.OPENCODE_API_KEY || "",
      zaiApiKey: process.env.ZAI_API_KEY || "",
      githubCopilotOAuthAccessToken: config.githubCopilotOauthAccessToken,
      githubCopilotOAuthRefreshToken: config.githubCopilotOauthRefreshToken,
      githubCopilotOAuthTokenExpires: config.githubCopilotOauthTokenExpires,
    });
    console.log("[agent] SDK adapter initialized (warm pool mode)");
  } catch (err) {
    console.error("[agent] Failed to initialize SDK adapter (warm pool):", err);
    throw new Error(`SDK initialization failed: ${err instanceof Error ? err.message : String(err)}`);
  }
}

/**
 * Handle execute prompt request
 */
async function handleExecutePrompt(
  sessionId: string,
  text: string,
  send: (msg: AgentMessage) => void
): Promise<void> {
  const config = connection?.sessionConfig;

  if (!currentAdapter) {
    console.error("[agent] No SDK adapter initialized");
    send(
      create(AgentMessageSchema, {
        message: {
          case: "promptResponse",
          value: create(AgentStreamResponseSchema, {
            response: {
              case: "error",
              value: {
                $typeName: "netclode.v1.AgentError",
                message: "SDK adapter not initialized",
                retryable: false,
              },
            },
          }),
        },
      })
    );
    return;
  }

  try {
    // Initialize session repos if needed (SDK-agnostic)
    if (config?.repos && config.repos.length > 0) {
      for await (const event of initializeSessionRepos(sessionId, config.repos, config.githubToken)) {
        send(promptEventToAgentMessage(event));
      }
    }

    for await (const event of currentAdapter.executePrompt(
      sessionId,
      text,
      config ? { repos: config.repos, githubToken: config.githubToken } : undefined
    )) {
      send(promptEventToAgentMessage(event));
      
      // If toolStart has input, also send toolInputComplete event
      // This is needed because the proto toolStart doesn't carry input - it comes via toolInput
      if (event.type === "toolStart" && event.input && Object.keys(event.input).length > 0) {
        send(promptEventToAgentMessage({
          type: "toolInputComplete",
          toolUseId: event.toolUseId,
          parentToolUseId: event.parentToolUseId,
          input: event.input,
        }));
      }
    }
  } catch (error) {
    console.error("[agent] Prompt execution error:", error);
    send(
      create(AgentMessageSchema, {
        message: {
          case: "promptResponse",
          value: create(AgentStreamResponseSchema, {
            response: {
              case: "error",
              value: {
                $typeName: "netclode.v1.AgentError",
                message: error instanceof Error ? error.message : String(error),
                retryable: false,
              },
            },
          }),
        },
      })
    );
  }
}

/**
 * Handle generate title request
 */
async function handleGenerateTitle(
  requestId: string,
  prompt: string,
  send: (msg: AgentMessage) => void
): Promise<void> {
  try {
    const title = await generateTitle(prompt);
    send(
      create(AgentMessageSchema, {
        message: {
          case: "titleResponse",
          value: create(AgentTitleResponseSchema, { requestId, title }),
        },
      })
    );
  } catch (error) {
    console.error("[agent] Title generation error:", error);
    send(
      create(AgentMessageSchema, {
        message: {
          case: "titleResponse",
          value: create(AgentTitleResponseSchema, { requestId, title: "" }),
        },
      })
    );
  }
}

/**
 * Handle get git status request
 */
async function handleGetGitStatus(
  requestId: string,
  send: (msg: AgentMessage) => void
): Promise<void> {
  try {
    const repoConfigs = getRepoConfigs();
    const files: Array<GitFileChange & { repo: string }> = [];

    if (repoConfigs.length === 0) {
      const rootFiles = await getGitStatus(WORKSPACE_DIR);
      files.push(...rootFiles.map((file) => ({ ...file, repo: "" })));
    } else {
      for (const { repo, dir, prefix } of repoConfigs) {
        const repoFiles = await getGitStatus(dir);
        files.push(
          ...repoFiles.map((file) => ({
            ...file,
            path: `${prefix}/${file.path}`,
            repo,
          }))
        );
      }
    }
    send(
      create(AgentMessageSchema, {
        message: {
          case: "gitStatusResponse",
          value: create(AgentGitStatusResponseSchema, {
            requestId,
            files: files.map((f) =>
              create(GitFileChangeSchema, {
                path: f.path,
                status: convertGitStatus(f.status),
                staged: f.staged,
                linesAdded: f.linesAdded,
                linesRemoved: f.linesRemoved,
                repo: f.repo,
              })
            ),
          }),
        },
      })
    );
  } catch (error) {
    console.error("[agent] Git status error:", error);
    send(
      create(AgentMessageSchema, {
        message: {
          case: "gitStatusResponse",
          value: create(AgentGitStatusResponseSchema, { requestId, files: [] }),
        },
      })
    );
  }
}

/**
 * Handle get git diff request
 */
async function handleGetGitDiff(
  requestId: string,
  file: string | undefined,
  send: (msg: AgentMessage) => void
): Promise<void> {
  try {
    const repoConfigs = getRepoConfigs();
    let diff = "";

    if (repoConfigs.length === 0) {
      diff = await getGitDiff(WORKSPACE_DIR, file);
    } else if (file) {
      const parts = file.split("/");
      const prefix = parts[0];
      let target = repoConfigs.find((repoConfig) => repoConfig.prefix === prefix);
      let relativeFile = parts.slice(1).join("/");

      if (!target && repoConfigs.length === 1) {
        target = repoConfigs[0];
        relativeFile = file;
      }

      if (target) {
        const fileArg = relativeFile.length > 0 ? relativeFile : undefined;
        diff = await getGitDiff(target.dir, fileArg, target.prefix);
      }
    } else {
      const diffs: string[] = [];
      for (const repoConfig of repoConfigs) {
        const repoDiff = await getGitDiff(repoConfig.dir, undefined, repoConfig.prefix);
        if (repoDiff) {
          diffs.push(repoDiff.trimEnd());
        }
      }
      diff = diffs.join("\n");
    }
    send(
      create(AgentMessageSchema, {
        message: {
          case: "gitDiffResponse",
          value: create(AgentGitDiffResponseSchema, { requestId, diff }),
        },
      })
    );
  } catch (error) {
    console.error("[agent] Git diff error:", error);
    send(
      create(AgentMessageSchema, {
        message: {
          case: "gitDiffResponse",
          value: create(AgentGitDiffResponseSchema, { requestId, diff: "" }),
        },
      })
    );
  }
}

/**
 * Handle terminal input from control plane
 */
function handleTerminalInputMessage(input: {
  input: { case: "data"; value: string } | { case: "resize"; value: { cols: number; rows: number } } | { case: undefined; value?: undefined };
}): void {
  switch (input.input.case) {
    case "data":
      handleTerminalInput(input.input.value);
      break;
    case "resize":
      resizeTerminal(input.input.value.cols, input.input.value.rows);
      break;
  }
}
