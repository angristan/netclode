import { beforeEach, describe, expect, it, vi } from "vitest";
import type { SDKConfig } from "../types.js";

const runStreamedMock = vi.fn();

vi.mock("@openai/codex-sdk", () => {
  class MockCodex {
    startThread() {
      return {
        id: "thread-1",
        runStreamed: runStreamedMock,
      };
    }

    resumeThread() {
      return {
        id: "thread-1",
        runStreamed: runStreamedMock,
      };
    }
  }

  return { Codex: MockCodex };
});

import { CodexAdapter } from "./adapter.js";

function makeConfig(overrides: Partial<SDKConfig> = {}): SDKConfig {
  return {
    sdkType: "codex",
    workspaceDir: "/tmp/workspace",
    anthropicApiKey: "",
    model: "gpt-5-codex:oauth:high",
    codexAccessToken: "access-token",
    codexIdToken: "eyJhbGciOiJub25lIn0.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC0xIn19.",
    ...overrides,
  };
}

describe("CodexAdapter interrupt behavior", () => {
  beforeEach(() => {
    runStreamedMock.mockReset();
    process.env.CODEX_HOME = `/tmp/netclode-codex-test-${Date.now()}-${Math.random()}`;
  });

  it("passes AbortSignal to runStreamed and yields interrupt error on abort", async () => {
    runStreamedMock.mockImplementation(
      (_input: string, options?: { signal?: AbortSignal }) =>
        new Promise((_resolve, reject) => {
          if (!options?.signal) {
            reject(new Error("missing signal"));
            return;
          }

          const abort = () => {
            const err = new Error("operation aborted");
            err.name = "AbortError";
            reject(err);
          };

          if (options.signal.aborted) {
            abort();
            return;
          }

          options.signal.addEventListener("abort", abort, { once: true });
        })
    );

    const adapter = new CodexAdapter();
    await adapter.initialize(makeConfig());

    const iterator = adapter.executePrompt("sess-1", "hello")[Symbol.asyncIterator]();
    const firstEventPromise = iterator.next();

    await vi.waitFor(() => {
      expect(runStreamedMock).toHaveBeenCalledTimes(1);
    });
    adapter.setInterruptSignal();

    const firstEvent = await firstEventPromise;

    expect(runStreamedMock).toHaveBeenCalledTimes(1);
    expect(runStreamedMock.mock.calls[0][1]?.signal).toBeInstanceOf(AbortSignal);
    expect(firstEvent.done).toBe(false);
    expect(firstEvent.value).toEqual({
      type: "error",
      message: "Prompt interrupted",
      retryable: true,
    });
  });
});
