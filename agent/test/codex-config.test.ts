import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  assertNoUnexpectedSystemConfig,
  buildCodexConfigToml,
  buildCodexLoopbackTestConfigToml,
  buildCodexProductionConfigToml,
  CodexUnexpectedSystemConfigError,
  type CodexConfigOptions,
} from "../src/codex/config.js";

// PRD #1156 (M3a) — the stock, native-DISABLED config builder + system-config guard.

const OPTS: CodexConfigOptions = {
  model: "gpt-5-codex",
  provider: { name: "uzi-codex", baseUrl: "http://127.0.0.1:8080/v1", envKey: "CODEX_PROVIDER_KEY", wireApi: "responses" },
  projectPath: "/work/repo",
};

describe("buildCodexConfigToml: stock native-disabled config", () => {
  const toml = buildCodexConfigToml(OPTS);

  it("provisions a stock, doc-free, update-free app-server", () => {
    assert.match(toml, /^model = "gpt-5-codex"$/m);
    assert.match(toml, /^model_provider = "uzi-codex"$/m);
    assert.match(toml, /^project_doc_max_bytes = 0$/m);
    assert.match(toml, /^check_for_update_on_startup = false$/m);
    assert.match(toml, /^web_search = "disabled"$/m);
  });

  it("provisions the canonical project EXPLICITLY untrusted", () => {
    assert.match(toml, /^\[projects\."\/work\/repo"\]$/m);
    assert.match(toml, /^trust_level = "untrusted"$/m);
    assert.doesNotMatch(toml, /trust_level = "trusted"/);
  });

  it("disables analytics, feedback and agents", () => {
    assert.match(toml, /^\[analytics\]\nenabled = false$/m);
    assert.match(toml, /^\[feedback\]\nenabled = false$/m);
    assert.match(toml, /^\[agents\]\nenabled = false$/m);
  });

  it("pins every [features] native knob to its disabled value", () => {
    for (const key of [
      "apps", "plugins", "shell_tool", "view_image", "sleep_tool", "apply_patch_freeform",
      "shell_snapshot", "shell_snapshot_v2",
      "code_mode", "code_mode_only", "code_mode_host", "code_mode_prewarm",
      "remote_models", "unified_exec", "hooks", "multi_agent", "multi_agent_v2",
      "enable_request_compression",
    ]) {
      assert.match(toml, new RegExp(`^${key} = false$`, "m"), `${key} must be false`);
    }
  });

  it("carries NO hook-trust bypass and NEVER arms hooks", () => {
    assert.doesNotMatch(toml, /bypass_hook_trust/);
    assert.doesNotMatch(toml, /dangerously-bypass-hook-trust/);
    assert.doesNotMatch(toml, /hooks = true/);
    assert.match(toml, /^hooks = false$/m);
  });

  it("emits a retry-free, websocket-free provider block", () => {
    assert.match(toml, /^\[model_providers\."uzi-codex"\]$/m);
    assert.match(toml, /^name = "uzi-codex"$/m);
    assert.match(toml, /^base_url = "http:\/\/127\.0\.0\.1:8080\/v1"$/m);
    assert.match(toml, /^env_key = "CODEX_PROVIDER_KEY"$/m);
    assert.match(toml, /^wire_api = "responses"$/m);
    assert.match(toml, /^supports_websockets = false$/m);
    assert.match(toml, /^requires_openai_auth = false$/m);
    assert.match(toml, /^request_max_retries = 0$/m);
    assert.match(toml, /^stream_max_retries = 0$/m);
  });

  it("rejects a provider name that is not a bare identifier (no TOML-key injection)", () => {
    assert.throws(() => buildCodexConfigToml({ ...OPTS, provider: { ...OPTS.provider, name: 'evil"]\ninjected = true' } }), /provider name/);
  });

  it("escapes special characters in the project path", () => {
    const t = buildCodexConfigToml({ ...OPTS, projectPath: '/work/re"po' });
    assert.match(t, /^\[projects\."\/work\/re\\"po"\]$/m);
  });
});

describe("assertNoUnexpectedSystemConfig: fail-closed on a present /etc/codex", () => {
  let present: string;
  before(() => { present = mkdtempSync(join(tmpdir(), "codex-etc-")); });
  after(() => { rmSync(present, { recursive: true, force: true }); });

  it("throws when the injected system-config path EXISTS", () => {
    assert.throws(() => assertNoUnexpectedSystemConfig(present), CodexUnexpectedSystemConfigError);
    assert.throws(() => assertNoUnexpectedSystemConfig(present), /Unexpected system Codex configuration/);
  });

  it("passes (no throw) when the path is ABSENT — the shipped image state", () => {
    assert.doesNotThrow(() => assertNoUnexpectedSystemConfig(join(present, "does-not-exist")));
  });
});

describe("buildCodexProductionConfigToml: fixed managed-auth provider", () => {
  for (const authMode of ["api_key", "subscription"] as const) {
    it(`${authMode} uses pinned Codex's built-in OpenAI auth routing`, () => {
      const toml = buildCodexProductionConfigToml({
        model: "gpt-6-astra",
        projectPath: "/work/repo",
        authMode,
      });

      assert.match(toml, /^model_provider = "openai"$/m);
      assert.match(toml, /^project_doc_max_bytes = 0$/m);
      assert.match(toml, /^\[projects\."\/work\/repo"\]\ntrust_level = "untrusted"$/m);
      assert.doesNotMatch(toml, /^\[model_providers\./m, "production does not shadow the built-in provider");
      assert.doesNotMatch(toml, /base_url|env_key|requires_openai_auth/, "routing/auth remain pinned upstream");
    });
  }

  it("rejects endpoint/provider injection even from untyped runtime input", () => {
    assert.throws(
      () => buildCodexProductionConfigToml({
        model: "gpt-6-astra",
        projectPath: "/work/repo",
        authMode: "api_key",
        baseUrl: "http://127.0.0.1:9/v1",
      } as never),
      /unsupported option/,
    );
  });

  it("rejects missing and unsupported auth modes without an earlier unknown-option failure", () => {
    assert.throws(
      () => buildCodexProductionConfigToml({ model: "gpt-6-astra", projectPath: "/work/repo" } as never),
      /explicit supported auth mode/,
    );
    assert.throws(
      () => buildCodexProductionConfigToml({ model: "gpt-6-astra", projectPath: "/work/repo", authMode: "oauth" } as never),
      /explicit supported auth mode/,
    );
  });
});

describe("buildCodexLoopbackTestConfigToml: authenticated packaged fake", () => {
  const opts = {
    model: "gpt-6-astra",
    projectPath: "/work/repo",
    authMode: "subscription" as const,
  };

  it("uses pinned Codex's authenticated custom-provider test shape with WebSockets off", () => {
    const toml = buildCodexLoopbackTestConfigToml(opts, "http://127.0.0.1:43123/v1");
    assert.match(toml, /^model_provider = "uzi-m3b-openai"$/m);
    assert.match(toml, /^\[model_providers\."uzi-m3b-openai"\]$/m);
    assert.match(toml, /^name = "OpenAI"$/m);
    assert.match(toml, /^base_url = "http:\/\/127\.0\.0\.1:43123\/v1"$/m);
    assert.match(toml, /^supports_websockets = false$/m);
    assert.match(toml, /^requires_openai_auth = true$/m);
    assert.doesNotMatch(toml, /env_key/);
  });

  it("rejects every non-literal-loopback redirect and credential-bearing URL", () => {
    for (const value of [
      "https://127.0.0.1:43123/v1",
      "http://localhost:43123/v1",
      "http://127.0.0.1/v1",
      "http://127.0.0.1:43123/other",
      "http://user:secret@127.0.0.1:43123/v1",
      "http://127.0.0.1:43123/v1?target=external",
      "http://0x7f000001:43123/v1",
      "http://2130706433:43123/v1",
      "http://127.0.0.1:80/v1",
      "https://api.openai.com/v1",
    ]) {
      assert.throws(
        () => buildCodexLoopbackTestConfigToml(opts, value),
        /loopback test base URL/,
        value,
      );
    }
  });
});
