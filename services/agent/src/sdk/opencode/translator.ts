/**
 * OpenCode event translator
 *
 * Translates OpenCode SSE events to our common PromptEvent format.
 * Extracted for testability.
 */

import type { JsonObject } from "@bufbuild/protobuf";
import type { PromptEvent } from "../types.js";
import { normalizeToolName, normalizeToolInput } from "../utils/index.js";

/**
 * OpenCode SSE event structure
 */
export interface OpenCodeEvent {
  type: string;
  properties?: Record<string, unknown>;
}

/**
 * State tracked during event translation
 */
export interface TranslatorState {
  assistantMessageIds: Set<string>;
  toolStartTimes: Map<string, number>;
  toolStartEmitted: Set<string>;
  currentTextPartId: string | null;
  currentTextMessageId: string | null;
  textMessageIdCounter: number;
  lastUsage: { inputTokens: number; outputTokens: number } | null;
}

/**
 * Create initial translator state
 */
export function createTranslatorState(): TranslatorState {
  return {
    assistantMessageIds: new Set(),
    toolStartTimes: new Map(),
    toolStartEmitted: new Set(),
    currentTextPartId: null,
    currentTextMessageId: null,
    textMessageIdCounter: 0,
    lastUsage: null,
  };
}

/**
 * Reset translator state for a new prompt
 */
export function resetTranslatorState(state: TranslatorState): void {
  state.assistantMessageIds.clear();
  state.toolStartTimes.clear();
  state.toolStartEmitted.clear();
  state.currentTextPartId = null;
  state.currentTextMessageId = null;
  state.lastUsage = null;
}

/**
 * Translate a message.part.updated event
 */
export function translateMessagePartUpdated(
  part: Record<string, unknown>,
  delta: string | undefined,
  state: TranslatorState
): PromptEvent | null {
  // Only process parts that belong to assistant messages
  const messageId = part.messageID as string | undefined;
  if (messageId && !state.assistantMessageIds.has(messageId)) {
    return null;
  }

  switch (part.type) {
    case "tool":
      return translateToolPart(part, state);
    case "reasoning":
      return translateReasoningPart(part, delta);
    case "text":
    case "step-start":
    case "step-finish":
    default:
      return null;
  }
}

/**
 * Translate a text part
 */
function translateTextPart(
  part: Record<string, unknown>,
  delta: string | undefined,
  state: TranslatorState
): PromptEvent | null {
  // Only use delta content, not part.text (which is accumulated text)
  if (!delta) return null;

  // Each text part gets its own message ID
  const partId = part.id as string;
  if (partId !== state.currentTextPartId) {
    state.currentTextPartId = partId;
    state.currentTextMessageId = `msg_${Date.now()}_${++state.textMessageIdCounter}`;
  }

  return {
    type: "textDelta",
    content: delta,
    partial: true,
    messageId: state.currentTextMessageId || undefined,
  };
}

/**
 * Translate a reasoning part
 */
function translateReasoningPart(
  part: Record<string, unknown>,
  delta: string | undefined
): PromptEvent {
  return {
    type: "thinking",
    thinkingId: (part.id as string) || `thinking_${Date.now()}`,
    content: delta || (part.text as string) || "",
    partial: !!delta,
  };
}

/**
 * Translate a tool part
 */
function translateToolPart(
  part: Record<string, unknown>,
  state: TranslatorState
): PromptEvent | null {
  const partState = part.state as Record<string, unknown> | undefined;
  if (!partState) return null;

  const status = partState.status as string;
  const toolName = normalizeToolName(part.tool as string);
  const callId = part.callID as string;

  // Get input from state.input or parse from state.raw (JSON string)
  let input = partState.input as Record<string, unknown> | undefined;
  if (!input && partState.raw) {
    try {
      input = JSON.parse(partState.raw as string);
    } catch {
      // raw might be incomplete during streaming
    }
  }

  const normalizedInput = normalizeToolInput(input);

  switch (status) {
    case "pending":
      // Just track the start time, don't emit yet
      if (!state.toolStartTimes.has(callId)) {
        state.toolStartTimes.set(callId, Date.now());
      }
      return null;

    case "running":
      // Input is now complete, emit toolStart if we haven't already
      if (!state.toolStartEmitted.has(callId)) {
        state.toolStartEmitted.add(callId);
        if (!state.toolStartTimes.has(callId)) {
          state.toolStartTimes.set(callId, Date.now());
        }
        return {
          type: "toolStart",
          tool: toolName,
          toolUseId: callId,
          input: normalizedInput as JsonObject | undefined,
        };
      }
      return null;

    case "completed":
      return translateToolCompleted(toolName, callId, partState, normalizedInput, state);

    case "error":
      return translateToolError(toolName, callId, partState, normalizedInput, state);

    default:
      return null;
  }
}

/**
 * Handle tool completed status
 */
function translateToolCompleted(
  toolName: string,
  callId: string,
  partState: Record<string, unknown>,
  normalizedInput: Record<string, unknown> | undefined,
  state: TranslatorState
): PromptEvent {
  // If we never emitted toolStart, emit it now
  if (!state.toolStartEmitted.has(callId)) {
    state.toolStartEmitted.add(callId);
    if (!state.toolStartTimes.has(callId)) {
      state.toolStartTimes.set(callId, Date.now());
    }
    return {
      type: "toolStart",
      tool: toolName,
      toolUseId: callId,
      input: normalizedInput as JsonObject | undefined,
    };
  }

  const startTime = state.toolStartTimes.get(callId);
  state.toolStartTimes.delete(callId);
  state.toolStartEmitted.delete(callId);
  const durationMs = startTime ? Date.now() - startTime : undefined;

  return {
    type: "toolEnd",
    tool: toolName,
    toolUseId: callId,
    result: partState.output as string | undefined,
    ...(durationMs !== undefined && { durationMs }),
  };
}

/**
 * Handle tool error status
 */
function translateToolError(
  toolName: string,
  callId: string,
  partState: Record<string, unknown>,
  normalizedInput: Record<string, unknown> | undefined,
  state: TranslatorState
): PromptEvent {
  // If we never emitted toolStart, emit it now
  if (!state.toolStartEmitted.has(callId)) {
    state.toolStartEmitted.add(callId);
    if (!state.toolStartTimes.has(callId)) {
      state.toolStartTimes.set(callId, Date.now());
    }
    return {
      type: "toolStart",
      tool: toolName,
      toolUseId: callId,
      input: normalizedInput as JsonObject | undefined,
    };
  }

  const startTime = state.toolStartTimes.get(callId);
  state.toolStartTimes.delete(callId);
  state.toolStartEmitted.delete(callId);
  const durationMs = startTime ? Date.now() - startTime : undefined;

  return {
    type: "toolEnd",
    tool: toolName,
    toolUseId: callId,
    error: partState.error as string | undefined,
    ...(durationMs !== undefined && { durationMs }),
  };
}

/**
 * Translate a message.updated event
 */
export function translateMessageUpdated(
  info: Record<string, unknown>,
  state: TranslatorState
): PromptEvent | null {
  // Track assistant message IDs so we can filter message.part.updated events
  if (info.role === "assistant" && info.id) {
    state.assistantMessageIds.add(info.id as string);
  }

  // Track usage data for later (emitted with session.idle)
  if (info.role === "assistant" && info.time) {
    const time = info.time as Record<string, unknown>;
    if (time.completed) {
      const tokens = info.tokens as Record<string, number> | undefined;
      if (tokens) {
        state.lastUsage = {
          inputTokens: (state.lastUsage?.inputTokens || 0) + (tokens.input || 0),
          outputTokens: (state.lastUsage?.outputTokens || 0) + (tokens.output || 0),
        };
      }
    }
  }

  // Check for errors
  if (info.error) {
    const error = info.error as Record<string, unknown>;
    const errorData = error.data as Record<string, string> | undefined;
    return {
      type: "error",
      message: errorData?.message || "Unknown error",
      retryable: false,
    };
  }

  return null;
}

/**
 * Translate a session.idle event
 */
export function translateSessionIdle(state: TranslatorState): PromptEvent {
  return {
    type: "result",
    inputTokens: state.lastUsage?.inputTokens || 0,
    outputTokens: state.lastUsage?.outputTokens || 0,
    totalTurns: 1,
  };
}

/**
 * Main event translator function
 */
export function translateEvent(
  event: OpenCodeEvent,
  state: TranslatorState
): PromptEvent | null {
  const props = event.properties || {};

  switch (event.type) {
    case "message.part.delta": {
      // OpenCode streams text deltas as separate events:
      // { type: "message.part.delta", properties: { messageID, partID, field, delta } }
      // Only handle text field deltas for assistant messages.
      const messageId = props.messageID as string | undefined;
      const field = props.field as string | undefined;
      const delta = props.delta as string | undefined;
      const partId = props.partID as string | undefined;

      if (field !== "text" || !delta || !messageId) return null;
      if (!state.assistantMessageIds.has(messageId)) return null;

      // Track part ID for message ID continuity
      if (partId !== state.currentTextPartId) {
        state.currentTextPartId = partId || null;
        state.currentTextMessageId = `msg_${Date.now()}_${++state.textMessageIdCounter}`;
      }

      return {
        type: "textDelta",
        content: delta,
        partial: true,
        messageId: state.currentTextMessageId || undefined,
      };
    }

    case "message.part.updated": {
      // Used for tool state transitions (pending → running → completed/error).
      // Text content arrives via message.part.delta instead.
      const part = props.part as Record<string, unknown> | undefined;
      if (!part) return null;
      // Only pass through for tool parts — text parts have no delta here
      if (part.type !== "tool") return null;
      return translateMessagePartUpdated(part, undefined, state);
    }

    case "message.updated": {
      const info = props.info as Record<string, unknown> | undefined;
      if (!info) return null;
      return translateMessageUpdated(info, state);
    }

    case "session.idle":
      return translateSessionIdle(state);

    default:
      return null;
  }
}
