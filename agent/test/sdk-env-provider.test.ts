import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { buildSdkEnv, PROTECTED_ENV_KEYS, PROVIDER_FALLBACK_CONTEXT_WINDOW, type SdkCredential } from "../src/sdk-env.js";
import { COMMAND_ENV_PROTECTED_KEYS } from "../src/codex/codex-executor.js";

// PRD #4320 module 6: buildSdkEnv in provider mode. The gateway key is assembled at runtime: no
// provider-token-shaped literal lives in source.
const GATEWAY_KEY = ["gw", "key", "do-not-scan", "4444", "abcdef"].join("-");
const GATEWAY_URL = "https://gw.example.test/anthropic";
const FAKE_OAUTH = "dummy-oauth-token-do-not-scan-0000";
const AMBIENT_KEY = "dummy-anthropic-api-key-do-not-scan-3333";
const HOME_DIR = "/data/agent-home";

type ProviderCred = Extract<SdkCredential, { kind: "provider" }>;

function providerCred(over: Partial<ProviderCred> = {}): ProviderCred {
  return {
    kind: "provider",
    baseUrl: GATEWAY_URL,
    authStyle: "bearer",
    key: GATEWAY_KEY,
    modelMap: { opus: { model: "corp-opus" }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } },
    ...over,
  };
}

/** The keys whose value is not undefined: what the child process actually sees. */
function defined(e: Record<string, string | undefined>): Record<string, string> {
  return Object.fromEntries(Object.entries(e).filter(([, v]) => v !== undefined)) as Record<string, string>;
}

let saved: Record<string, string | undefined>;
beforeEach(() => {
  saved = {
    ANTHROPIC_API_KEY: process.env.ANTHROPIC_API_KEY,
    ANTHROPIC_AUTH_TOKEN: process.env.ANTHROPIC_AUTH_TOKEN,
    UZI_RUNNER_PATH: process.env.UZI_RUNNER_PATH,
    UZI_CLI_CA_FILE: process.env.UZI_CLI_CA_FILE,
  };
  delete process.env.UZI_RUNNER_PATH;
  delete process.env.UZI_CLI_CA_FILE;
  process.env.ANTHROPIC_API_KEY = AMBIENT_KEY;
  process.env.ANTHROPIC_AUTH_TOKEN = AMBIENT_KEY;
});
afterEach(() => {
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("buildSdkEnv provider mode (PRD #4320)", () => {
  it("bearer: sets the base URL, ANTHROPIC_AUTH_TOKEN, the mapped models, the small/fast slot and the traffic switch, and pins everything else", () => {
    const e = buildSdkEnv(providerCred(), HOME_DIR);
    assert.equal(e.ANTHROPIC_BASE_URL, GATEWAY_URL);
    assert.equal(e.ANTHROPIC_AUTH_TOKEN, GATEWAY_KEY);
    assert.equal(e.ANTHROPIC_API_KEY, undefined);
    assert.equal(e.CLAUDE_CODE_OAUTH_TOKEN, undefined);
    assert.equal(e.ANTHROPIC_DEFAULT_OPUS_MODEL, "corp-opus");
    assert.equal(e.ANTHROPIC_DEFAULT_SONNET_MODEL, "corp-sonnet");
    assert.equal(e.ANTHROPIC_DEFAULT_HAIKU_MODEL, "corp-haiku");
    assert.equal(e.ANTHROPIC_DEFAULT_FABLE_MODEL, undefined, "an unmapped alias is pinned off");
    assert.equal(e.ANTHROPIC_SMALL_FAST_MODEL, "corp-haiku");
    assert.equal(e.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, "1");
    assert.equal(e.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "200000", "no context_window set: the conservative 200K fallback applies");
    for (const k of ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_DEFAULT_FABLE_MODEL"]) assert.ok(k in e, `${k} is pinned`);
  });

  it("x_api_key: sets ANTHROPIC_API_KEY and pins ANTHROPIC_AUTH_TOKEN", () => {
    const e = buildSdkEnv(providerCred({ authStyle: "x_api_key" }), HOME_DIR);
    assert.equal(e.ANTHROPIC_API_KEY, GATEWAY_KEY);
    assert.equal(e.ANTHROPIC_AUTH_TOKEN, undefined);
    assert.equal(e.CLAUDE_CODE_OAUTH_TOKEN, undefined);
  });

  it("keeps provider routing with run-owned TMPDIR and a provisioned PATH", () => {
    const leaf = "/data/runner/.tmp/run-provider-test";
    const e = buildSdkEnv(providerCred(), HOME_DIR, { PATH: "/provisioned/bin" }, undefined, undefined, undefined, leaf);
    assert.equal(e.TMPDIR, leaf);
    assert.equal(e.PATH, "/provisioned/bin");
    assert.equal(e.ANTHROPIC_BASE_URL, GATEWAY_URL);
    assert.equal(e.ANTHROPIC_AUTH_TOKEN, GATEWAY_KEY);
    assert.equal(e.CLAUDE_CODE_OAUTH_TOKEN, undefined);
  });

  for (const authStyle of ["bearer", "x_api_key"] as const) {
    it(`${authStyle}: the defined key set is exactly the provider keys plus the base keys, and the key appears once`, () => {
      const e = buildSdkEnv(providerCred({
        authStyle,
        modelMap: { opus: { model: "o", context_window: 200_000 }, sonnet: { model: "s" }, haiku: { model: "h" }, fable: { model: "f" } },
      }), HOME_DIR);
      const expected = new Set([
        "HOME", "CLAUDE_CONFIG_DIR", "PATH", "AGENT_BROWSER_ARGS", "AGENT_BROWSER_EXECUTABLE_PATH", "CHROME_PATH",
        "ANTHROPIC_BASE_URL", authStyle === "bearer" ? "ANTHROPIC_AUTH_TOKEN" : "ANTHROPIC_API_KEY",
        "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
        "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_MAX_CONTEXT_TOKENS",
      ]);
      if (process.env.UZI_RUNNER_TMPDIR || process.env.TMPDIR) expected.add("TMPDIR");
      if (process.env.UZI_SQLC_DIR) expected.add("UZI_SQLC_DIR");
      assert.deepStrictEqual(new Set(Object.keys(defined(e))), expected);
      assert.equal(Object.values(defined(e)).filter((v) => v === GATEWAY_KEY).length, 1, "the key is in exactly one variable");
      assert.ok(!Object.values(defined(e)).includes(AMBIENT_KEY), "the worker's ambient ANTHROPIC_* never leaks");
    });
  }

  it("CLAUDE_CODE_MAX_CONTEXT_TOKENS is the smallest context_window among ALL mapped models (process-wide key)", () => {
    const e = buildSdkEnv(providerCred({
      modelMap: { opus: { model: "o", context_window: 1_000_000 }, sonnet: { model: "s", context_window: 128_000 }, haiku: { model: "h", context_window: 32_768 } },
    }), HOME_DIR);
    assert.equal(e.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "32768", "the haiku slot's window counts even though it is not the lead");
    const one = buildSdkEnv(providerCred({ modelMap: { sonnet: { model: "s" }, haiku: { model: "h", context_window: 8192 } } }), HOME_DIR);
    assert.equal(one.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "8192", "no 100k floor");
    assert.equal(one.CLAUDE_CODE_AUTO_COMPACT_WINDOW, undefined, "the floored key is never used");
  });

  it("no configured window falls back to 200000; windows above it are never raised or capped to it", () => {
    assert.equal(PROVIDER_FALLBACK_CONTEXT_WINDOW, 200_000);
    const above = buildSdkEnv(providerCred({
      modelMap: { opus: { model: "o", context_window: 1_000_000 }, sonnet: { model: "s", context_window: 400_000 }, haiku: { model: "h", context_window: 300_000 } },
    }), HOME_DIR);
    assert.equal(above.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "300000", "an explicit window above the fallback wins");
    const partial = buildSdkEnv(providerCred({ modelMap: { sonnet: { model: "s", context_window: 400_000 }, haiku: { model: "h" } } }), HOME_DIR);
    assert.equal(partial.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "400000", "a partial map uses the smallest explicit window (documented limitation)");
  });

  it("a provisioned tool env can never set or clear a provider-mode key", () => {
    const hostile: Record<string, string> = {
      ANTHROPIC_BASE_URL: "https://evil.example.test", ANTHROPIC_AUTH_TOKEN: "x", ANTHROPIC_API_KEY: "y", CLAUDE_CODE_OAUTH_TOKEN: "z",
      ANTHROPIC_DEFAULT_OPUS_MODEL: "claude-opus", ANTHROPIC_DEFAULT_SONNET_MODEL: "claude-sonnet", ANTHROPIC_DEFAULT_HAIKU_MODEL: "claude-haiku",
      ANTHROPIC_DEFAULT_FABLE_MODEL: "claude-fable", ANTHROPIC_SMALL_FAST_MODEL: "claude-haiku", CLAUDE_CODE_SUBAGENT_MODEL: "claude-opus",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "0", CLAUDE_CODE_MAX_CONTEXT_TOKENS: "9", CLAUDE_CODE_AUTO_COMPACT_WINDOW: "9",
    };
    const e = buildSdkEnv(providerCred(), HOME_DIR, hostile);
    assert.equal(e.ANTHROPIC_BASE_URL, GATEWAY_URL);
    assert.equal(e.ANTHROPIC_AUTH_TOKEN, GATEWAY_KEY);
    assert.equal(e.ANTHROPIC_API_KEY, undefined);
    assert.equal(e.CLAUDE_CODE_OAUTH_TOKEN, undefined);
    assert.equal(e.ANTHROPIC_DEFAULT_FABLE_MODEL, undefined);
    assert.equal(e.ANTHROPIC_DEFAULT_OPUS_MODEL, "corp-opus");
    assert.equal(e.CLAUDE_CODE_SUBAGENT_MODEL, undefined);
    assert.equal(e.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, "1");
    assert.equal(e.CLAUDE_CODE_MAX_CONTEXT_TOKENS, "200000", "the provisioned \"9\" never wins over the fallback");
    assert.equal(e.CLAUDE_CODE_AUTO_COMPACT_WINDOW, undefined);
    // OAuth mode: the same protected keys hold.
    const oauth = buildSdkEnv({ kind: "oauth", token: FAKE_OAUTH }, HOME_DIR, hostile);
    assert.equal(oauth.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
    assert.ok(!("ANTHROPIC_BASE_URL" in oauth) && !("CLAUDE_CODE_SUBAGENT_MODEL" in oauth) && !("CLAUDE_CODE_MAX_CONTEXT_TOKENS" in oauth));
  });

  it("OAuth mode carries no provider key at all", () => {
    const e = buildSdkEnv({ kind: "oauth", token: FAKE_OAUTH }, HOME_DIR);
    for (const k of ["ANTHROPIC_BASE_URL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_MAX_CONTEXT_TOKENS"]) {
      assert.ok(!(k in e), `${k} must be absent in OAuth mode`);
    }
    assert.equal(e.CLAUDE_CODE_OAUTH_TOKEN, FAKE_OAUTH);
  });

  it("the sdk-env protected set and the Codex command-env mirror agree (the documented differences are pinned)", () => {
    for (const k of PROTECTED_ENV_KEYS) {
      // sdk-only: the CLI's CA bundle and its config directory (PRD #4416); the Codex command env has no CLI
      if (k === "NODE_EXTRA_CA_CERTS" || k === "CLAUDE_CONFIG_DIR") continue;
      assert.ok(COMMAND_ENV_PROTECTED_KEYS.has(k), `${k} is protected in sdk-env but missing from the Codex mirror`);
    }
    const codexOnly = [...COMMAND_ENV_PROTECTED_KEYS].filter((k) => !PROTECTED_ENV_KEYS.has(k)).sort();
    assert.deepStrictEqual(codexOnly, [
      "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST", "GOCACHE", "GOMODCACHE", "LANG", "NPM_CONFIG_CACHE", "PATH", "TMPDIR", "npm_config_cache",
    ], "the Codex mirror's extras are exactly the command-boundary literals");
    for (const k of [
      "ANTHROPIC_BASE_URL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL",
      "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_CODE_AUTO_COMPACT_WINDOW",
    ]) {
      assert.ok(PROTECTED_ENV_KEYS.has(k) && COMMAND_ENV_PROTECTED_KEYS.has(k), `${k} is protected in both sets`);
    }
  });
});
