import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { anthropicGateway, BLOCKED_BASE_URL, decodeAnthropicGateway, setAnthropicGatewayFromWire } from "../src/anthropic-gateway.js";
import { buildSdkEnv } from "../src/sdk-env.js";

const KEY = "gw-fake-key-for-tests";
const HOME = "/tmp/uzi-agent-home";

afterEach(() => setAnthropicGatewayFromWire(null));

describe("anthropic gateway", () => {
  it("decodes the register/heartbeat wire; null is off, blocked or malformed is blocked", () => {
    assert.deepStrictEqual(
      decodeAnthropicGateway({ base_url: "https://gw.example.com/anthropic", auth_header: "x-api-key", models: { opus: "bedrock/anthropic.claude:0" } }),
      { baseUrl: "https://gw.example.com/anthropic", authHeader: "x-api-key", models: { opus: "bedrock/anthropic.claude:0" } },
    );
    assert.deepStrictEqual(decodeAnthropicGateway({ base_url: "https://gw.example.com", auth_header: "authorization", models: null })?.models, {});
    assert.strictEqual(decodeAnthropicGateway(null), undefined);
    assert.strictEqual(decodeAnthropicGateway(undefined), undefined);
    for (const bad of [
      "x", {}, { blocked: true },
      { base_url: "http://gw.example.com", auth_header: "authorization" },
      { base_url: "https://u:p@gw.example.com", auth_header: "authorization" },
      { base_url: "https://gw.example.com", auth_header: "cookie" },
      { base_url: "https://gw.example.com", auth_header: "authorization", models: { gpt: "x" } },
      { base_url: "https://gw.example.com", auth_header: "authorization", models: { opus: "has space" } },
      { base_url: "https://gw.example.com", auth_header: "authorization", models: { opus: "gw\u00a0x" } },
    ]) {
      const got = decodeAnthropicGateway(bad);
      assert.ok(got?.blocked === true && got.baseUrl === BLOCKED_BASE_URL, JSON.stringify(bad));
    }
  });

  it("a blocked gateway points the CLI at an unresolvable host with no OAuth variable", () => {
    setAnthropicGatewayFromWire({ blocked: true });
    const env = buildSdkEnv(KEY, HOME);
    assert.strictEqual(env.ANTHROPIC_BASE_URL, BLOCKED_BASE_URL);
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, undefined);
  });

  it("routes the CLI through a Bearer gateway with the alias map, and a tool env cannot redirect it", () => {
    setAnthropicGatewayFromWire({ base_url: "https://gw.example.com", auth_header: "authorization", models: { opus: "gw-opus", haiku: "gw-haiku" } });
    const env = buildSdkEnv(KEY, HOME, {
      ANTHROPIC_BASE_URL: "https://evil.example.com",
      ANTHROPIC_DEFAULT_OPUS_MODEL: "evil-model",
      ANTHROPIC_DEFAULT_SONNET_MODEL: "evil-model",
    });
    assert.strictEqual(env.ANTHROPIC_BASE_URL, "https://gw.example.com");
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, KEY);
    assert.strictEqual(env.ANTHROPIC_API_KEY, undefined);
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, undefined, "the gateway key must not ride the OAuth variable");
    assert.strictEqual(env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, "1");
    assert.strictEqual(env.ANTHROPIC_DEFAULT_OPUS_MODEL, "gw-opus");
    assert.strictEqual(env.ANTHROPIC_DEFAULT_HAIKU_MODEL, "gw-haiku");
    assert.ok(!("ANTHROPIC_DEFAULT_SONNET_MODEL" in env), "an unmapped alias stays unset, and the tool env cannot set it");
  });

  it("sends the key as x-api-key when the gateway asks for it", () => {
    setAnthropicGatewayFromWire({ base_url: "https://gw.example.com", auth_header: "x-api-key", models: {} });
    const env = buildSdkEnv(KEY, HOME);
    assert.strictEqual(env.ANTHROPIC_API_KEY, KEY);
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, undefined);
  });

  it("an absent field keeps the gateway (an older api pod mid-rollout); only null clears it", () => {
    setAnthropicGatewayFromWire({ base_url: "https://gw.example.com", auth_header: "authorization" });
    setAnthropicGatewayFromWire(undefined);
    assert.strictEqual(anthropicGateway()?.baseUrl, "https://gw.example.com");
  });

  it("a null wire clears the gateway, so the CLI talks to Anthropic directly again", () => {
    setAnthropicGatewayFromWire({ base_url: "https://gw.example.com", auth_header: "authorization" });
    assert.ok(anthropicGateway());
    setAnthropicGatewayFromWire(null);
    const env = buildSdkEnv(KEY, HOME, { ANTHROPIC_BASE_URL: "https://evil.example.com" });
    assert.ok(!("ANTHROPIC_BASE_URL" in env));
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, undefined);
    assert.strictEqual(env.CLAUDE_CODE_OAUTH_TOKEN, KEY);
  });
});
