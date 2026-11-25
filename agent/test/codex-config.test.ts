import { assertNoTtlOverrides } from "./codex-no-ttl-overrides.js";
import { parse } from "smol-toml";
import { assertNoNativeServers } from "./codex-no-native-servers.js";
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
  buildCodexProviderConfigToml,
  CodexUnexpectedSystemConfigError,
  type CodexConfigOptions,
} from "../src/codex/config.js";

describe("buildCodexProviderConfigToml: trusted provider binding", () => {
  const binding = {
    provider_id: 'openai"]\n[features]\nhooks = true',
    revision: 3,
    engine: "codex" as const,
    protocol: "openai_responses" as const,
    base_url: "https://gateway.example/v1",
    auth_style: "bearer" as const,
    credential: "fixture-provider-credential",
    model_map: {
      default: { model: "gateway/default", context_window: 32768 },
      "gpt-6-astra": { model: "gateway/astra", context_window: 65536 },
      "gpt-6-sol": { model: "gateway/sol" },
    },
  };
  const opts = { binding, projectPath: '/work/re"po' };

  it("emits managed-auth HTTP transport with mapped metadata and no credential", () => {
    const config = parse(buildCodexProviderConfigToml(opts));
    assert.equal(config.model, "gateway/default");
    assert.equal(config.model_context_window, 32768);
    assert.match(config.model_provider as string, /^uzi-provider-[a-f0-9]{64}$/);
    const providers = config.model_providers as Record<string, unknown>;
    assert.deepEqual(JSON.parse(JSON.stringify(providers)), {
      [config.model_provider as string]: {
        name: config.model_provider, base_url: binding.base_url,
        wire_api: "responses", supports_websockets: false, requires_openai_auth: true,
        request_max_retries: 0, stream_max_retries: 0,
      },
    });
    assert.doesNotMatch(buildCodexProviderConfigToml(opts), /fixture-provider-credential|env_key/);
    assert.equal(parse(buildCodexProviderConfigToml({ ...opts, model: "gpt-6-astra" })).model_context_window, 65536);
    const fallback = parse(buildCodexProviderConfigToml({ ...opts, model: "gpt-6-sol" }));
    assert.equal(fallback.model, "gateway/sol");
    assert.equal(Object.hasOwn(fallback, "model_context_window"), false);
  });

  it("preserves native suppression and explicit untrusted project isolation", () => {
    const config = parse(buildCodexProviderConfigToml(opts));
    const baseline = parse(buildCodexProductionConfigToml({
      model: "gateway/default", projectPath: opts.projectPath, authMode: "api_key",
    }));
    for (const key of ["features", "projects", "analytics", "feedback", "agents",
      "project_doc_max_bytes", "check_for_update_on_startup", "web_search"]) {
      assert.deepEqual(config[key], baseline[key], key);
    }
    assertNoNativeServers(config);
    const features = parse(buildCodexProviderConfigToml({ ...opts, codeModeHost: true })).features;
    assert.deepEqual({ ...(features as object) }, { ...(config.features as object), code_mode_host: true });
  });

  it("refuses arbitrary identity, config, environment and header options", () => {
    for (const key of ["providerName", "provider", "baseUrl", "envKey", "env", "headers", "toml", "authMode"]) {
      assert.throws(() => buildCodexProviderConfigToml({ ...opts, [key]: "injected" } as never), /unsupported option/);
      assert.throws(() => buildCodexProviderConfigToml({
        ...opts, binding: { ...binding, [key]: "injected" },
      } as never), /unsupported option/);
    }
  });

  it("revalidates binding shape and refuses model/config injection", () => {
    for (const patch of [
      { engine: "claude" }, { auth_style: "x_api_key" }, { protocol: "chat_completions" },
      { base_url: "https://user:password@gateway.example/v1" },
      { model_map: { default: { model: 'bad"\nhooks=true' } } },
      { model_map: { default: { model: "ok", context_window: 1 } } },
      { model_map: { default: { model: "ok", headers: {} } } },
    ]) {
      assert.throws(() => buildCodexProviderConfigToml({ ...opts, binding: { ...binding, ...patch } } as never));
    }
    assert.throws(() => buildCodexProviderConfigToml({ ...opts, model: "gateway/arbitrary" } as never), /unmapped/);
  });

  it("keeps vendor endpoint and identity controls closed", () => {
    for (const authMode of ["api_key", "subscription"] as const) {
      for (const key of ["binding", "providerName", "provider", "baseUrl", "envKey", "headers", "toml"]) {
        assert.throws(() => buildCodexProductionConfigToml({
          model: "gpt-6-astra", projectPath: "/work/repo", authMode, [key]: binding,
        } as never), /unsupported option/);
      }
    }
  });
});

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
    assert.doesNotMatch(toml, /\bmcp_servers\b/, "no native MCP server is configured");
    assert.doesNotMatch(toml, /^\s*remote_models\s*=/m, "the removed compatibility key is omitted");
    for (const key of [
      "goals", "apps", "plugins", "shell_tool", "view_image", "sleep_tool", "apply_patch_freeform",
      "shell_snapshot", "shell_snapshot_v2",
      "code_mode", "code_mode_only", "code_mode_host", "code_mode_prewarm",
      "unified_exec", "hooks", "multi_agent", "multi_agent_v2",
      "enable_request_compression",
    ]) {
      const features = toml.split("[features]\n")[1]!.split("\n[")[0]!;
      assert.match(features, new RegExp(`^${key} = false$`, "m"), `[features].${key} must be false`);
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

  it("renders a hostile-but-valid custom model id as ONE escaped TOML string, injecting no key (PRD #1551)", () => {
    // A custom worker-root id the API validator allows can still contain TOML-significant
    // characters (`"`, `\`, `=`). toml()=JSON.stringify must escape them into a single
    // quoted string so the value cannot terminate the string and add its own key/line.
    const model = 'gpt-6"astra\\x=evil';
    const toml = buildCodexProductionConfigToml({
      model,
      projectPath: "/work/repo",
      authMode: "api_key",
    });
    assert.match(toml, /^model = "gpt-6\\"astra\\\\x=evil"$/m);
    const modelLines = toml.split("\n").filter((l) => l.startsWith("model = "));
    assert.equal(modelLines.length, 1, "the custom id produces exactly one model line");
    assert.doesNotMatch(toml, /^evil = /m, "the id cannot break out and add a key");
  });

  it("keeps the code-mode host DISABLED by default (no codeModeHost opt)", () => {
    const toml = buildCodexProductionConfigToml({
      model: "gpt-6-astra",
      projectPath: "/work/repo",
      authMode: "api_key",
    });
    assert.match(toml, /^code_mode_host = false$/m);
  });

  it("enables ONLY the code-mode host when codeModeHost is true", () => {
    const toml = buildCodexProductionConfigToml({
      model: "gpt-6-astra",
      projectPath: "/work/repo",
      authMode: "api_key",
      codeModeHost: true,
    });
    assert.match(toml, /^code_mode_host = true$/m);
    assert.match(toml, /^code_mode = false$/m);
    assert.match(toml, /^code_mode_only = false$/m);
    assert.match(toml, /^code_mode_prewarm = false$/m);
  });

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

  it("keeps the code-mode host DISABLED by default (no codeModeHost opt)", () => {
    const toml = buildCodexLoopbackTestConfigToml(opts, "http://127.0.0.1:43123/v1");
    assert.match(toml, /^code_mode_host = false$/m);
  });

  it("enables ONLY the code-mode host when codeModeHost is true", () => {
    const toml = buildCodexLoopbackTestConfigToml(
      { ...opts, codeModeHost: true },
      "http://127.0.0.1:43123/v1",
    );
    assert.match(toml, /^code_mode_host = true$/m);
    assert.match(toml, /^code_mode = false$/m);
    assert.match(toml, /^code_mode_only = false$/m);
    assert.match(toml, /^code_mode_prewarm = false$/m);
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

describe("no native MCP servers are configured", () => {
  for (const authMode of ["api_key", "subscription"] as const) {
    for (const codeModeHost of [false, true]) {
      for (const builder of ["production", "loopback"] as const) {
        it(`${builder}/${authMode}/host=${codeModeHost} semantic no-server config`, () => {
          const opts = { model: "gpt-6-astra", projectPath: "/work/repo", authMode, codeModeHost };
          const config = parse(builder === "production" ? buildCodexProductionConfigToml(opts)
            : buildCodexLoopbackTestConfigToml(opts, "http://127.0.0.1:43123/v1"));
          assertNoNativeServers(config);
          assert.equal(config.model, opts.model);
          assertNoTtlOverrides(opts, "production/loopback options");
          assertNoTtlOverrides(config, `${builder}/${authMode}/host=${codeModeHost} emitted TOML`);
          assert.equal((config.features as Record<string, unknown>).goals, false);
        });
      }
    }
  }
  it("custom provider semantic no-server config", () => {
    const config = parse(buildCodexConfigToml(OPTS));
    assert.equal(config.model, OPTS.model);
    assertNoTtlOverrides(OPTS, "generic options");
    assertNoTtlOverrides(config, "generic emitted TOML");
    assertNoNativeServers(config);
    assert.equal((config.features as Record<string, unknown>).goals, false);
  });
  it("rejects missing, null, scalar and array config roots", () => {
    for (const config of [undefined, null, false, 0, "config", [], [{}]]) {
      assert.throws(() => assertNoNativeServers(config), /outgoing thread config/);
    }
    assertNoNativeServers({ enabled: false, count: 0, optional: null, nested: ["value", {}] });
  });
  it("rejects nested, dotted and quoted outgoing server keys without inspecting values", () => {
    for (const config of [
      { mcp_servers: {} }, { "mcp_servers.fixture.command": "unused" },
      { '"mcp_servers".fixture': {} }, { features: { mcp_servers: {} } },
      { "'mcp_servers'.fixture": {} },
    ]) assert.throws(() => assertNoNativeServers(config), /no native MCP servers/);
    assertNoNativeServers({ model: "mcp_servers", projects: { "/work/repo": { trust_level: "untrusted" } } });
  });
});


describe("no TTL overrides: structured construction invariant", () => {
  it("rejects direct, nested, dotted and quoted TTL fields regardless of value", () => {
    for (const field of [
      { prompt_cache_ttl: null }, { prompt_cache_retention: "12h" },
      { config: { prompt_cache_options: { ttl: "12h" } } },
      { config: { prompt_cache_breakpoint: { ttl: false } } },
      { "config.prompt_cache_options.ttl": "12h" },
      { config: { '"prompt_cache_breakpoint"."ttl"': "12h" } },
      { "'prompt_cache_ttl'": undefined },
      { config: [{ "prompt_cache_retention": 0 }] },
      parse('[config.prompt_cache_options]\nttl = "12h"'),
      parse('"prompt_cache_breakpoint.ttl" = "12h"'),
    ]) assert.throws(() => assertNoTtlOverrides(field, "helper control"), /helper control: no TTL overrides/);
  });

  it("ignores prompt string values and permits cache identity without a TTL", () => {
    assertNoTtlOverrides({
      model: "gpt-6-astra", prompt_cache_key: "identity",
      input: [{ type: "text", text: 'prompt_cache_ttl = "12h"; prompt_cache_options.ttl; prompt_cache_retention' }],
      developerInstructions: "prompt_cache_breakpoint.ttl",
      config: { projects: { "/work/repo": { trust_level: "untrusted" } }, prompt_cache_options: {} },
    }, "harmless prompt strings");
    for (const value of [undefined, null, "prompt_cache_ttl", []]) {
      assert.throws(() => assertNoTtlOverrides(value, "invalid root"), /structured construction/);
    }
  });

  it("production options reject unsupported TTL injection without widening the whitelist", () => {
    for (const injection of [
      { prompt_cache_ttl: "12h" }, { prompt_cache_retention: "12h" },
      { prompt_cache_options: { ttl: "12h" } }, { prompt_cache_breakpoint: { ttl: "12h" } },
    ]) {
      assert.throws(() => buildCodexProductionConfigToml({
        model: "gpt-6-astra", projectPath: "/work/repo", authMode: "api_key", ...injection,
      } as never), /unsupported option/);
    }
  });
});
