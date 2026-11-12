import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { anthropicGateway, decodeAnthropicGateway, setAnthropicGatewayFromWire } from "../src/anthropic-gateway.js";
import { buildSdkEnv } from "../src/sdk-env.js";

const KEY = "gw-fake-key-for-tests";
const HOME = "/tmp/uzi-agent-home";

afterEach(() => setAnthropicGatewayFromWire(null));

describe("anthropic gateway", () => {
  it("decodes the register/heartbeat wire and refuses anything malformed", () => {
    assert.deepStrictEqual(
      decodeAnthropicGateway({ base_url: "https://gw.example.com/anthropic", auth_header: "x-api-key", models: { opus: "gw-opus" } }),
      { baseUrl: "https://gw.example.com/anthropic", authHeader: "x-api-key", models: { opus: "gw-opus" } },
    );
    assert.deepStrictEqual(decodeAnthropicGateway({ base_url: "https://gw.example.com", auth_header: "authorization", models: null })?.models, {});
    for (const bad of [
      null, undefined, "x", {},
      { base_url: "http://gw.example.com", auth_header: "authorization" },
      { base_url: "https://u:p@gw.example.com", auth_header: "authorization" },
      { base_url: "https://gw.example.com", auth_header: "cookie" },
      { base_url: "https://gw.example.com", auth_header: "authorization", models: { gpt: "x" } },
      { base_url: "https://gw.example.com", auth_header: "authorization", models: { opus: "has space" } },
    ]) {
      assert.strictEqual(decodeAnthropicGateway(bad), undefined, JSON.stringify(bad));
    }
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

  it("a null wire clears the gateway, so the CLI talks to Anthropic directly again", () => {
    setAnthropicGatewayFromWire({ base_url: "https://gw.example.com", auth_header: "authorization" });
    assert.ok(anthropicGateway());
    setAnthropicGatewayFromWire(null);
    const env = buildSdkEnv(KEY, HOME, { ANTHROPIC_BASE_URL: "https://evil.example.com" });
    assert.ok(!("ANTHROPIC_BASE_URL" in env));
    assert.strictEqual(env.ANTHROPIC_AUTH_TOKEN, undefined);
  });
});
