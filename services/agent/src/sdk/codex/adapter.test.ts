import { describe, expect, it } from "vitest";
import { resolveCodexAuthMode } from "./adapter.js";
import type { SDKConfig } from "../types.js";

function makeConfig(overrides: Partial<SDKConfig> = {}): SDKConfig {
  return {
    sdkType: "codex",
    workspaceDir: "/tmp/workspace",
    anthropicApiKey: "",
    ...overrides,
  };
}

describe("resolveCodexAuthMode", () => {
  it("prefers explicit oauth suffix", () => {
    const mode = resolveCodexAuthMode(
      makeConfig({
        model: "gpt-5-codex:oauth:high",
        openaiApiKey: "sk-real",
      })
    );
    expect(mode).toBe("oauth");
  });

  it("prefers explicit api suffix", () => {
    const mode = resolveCodexAuthMode(
      makeConfig({
        model: "gpt-5-codex:api:medium",
        codexAccessToken: "oauth-access",
        codexIdToken: "oauth-id",
      })
    );
    expect(mode).toBe("api");
  });

  it("prefers oauth tokens when suffix is omitted", () => {
    const mode = resolveCodexAuthMode(
      makeConfig({
        model: "gpt-5-codex",
        openaiApiKey: "sk-real",
        codexAccessToken: "oauth-access",
        codexIdToken: "oauth-id",
      })
    );
    expect(mode).toBe("oauth");
  });

  it("ignores Netclode placeholder api key", () => {
    const mode = resolveCodexAuthMode(
      makeConfig({
        model: "gpt-5-codex",
        openaiApiKey: "NETCLODE_PLACEHOLDER_openai",
      })
    );
    expect(mode).toBe("unknown");
  });
});
