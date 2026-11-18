import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  assertAgentModelsMapped,
  decodeProviderBinding,
  isGatewayLimitStatus,
  ProviderBindingError,
  ProviderModelUnmappedError,
  resolveProviderModel,
  sdkCredentialFromBinding,
  stripProviderCost,
} from "../src/provider-binding.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import type { EmittedMessage } from "../src/executor.js";

// PRD #4320 modules 5 and 7. The key is assembled at runtime: no provider-token-shaped literal in source.
const KEY = ["gw", "key", "do-not-scan", "5555", "abcdefgh"].join("-");
const NOT_LOOPBACK_ENV = {} as NodeJS.ProcessEnv;
const LOOPBACK_ENV = { UZI_PROVIDER_ALLOW_LOOPBACK: "true" } as NodeJS.ProcessEnv;

function binding(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    provider_id: "p-1",
    revision: 3,
    engine: "claude",
    protocol: "anthropic_messages",
    base_url: "https://gw.example.test/anthropic",
    auth_style: "bearer",
    credential: KEY,
    model_map: { opus: { model: "corp-opus", context_window: 200_000 }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } },
    ...over,
  };
}

function decode(provider: unknown, extra: Record<string, unknown> = {}, env = NOT_LOOPBACK_ENV): ClaimProviderBinding | undefined {
  return decodeProviderBinding({ forge_pat: "pat", anthropic_oauth_token: "", provider, ...extra }, env);
}

function refuses(provider: unknown, rule: RegExp, extra: Record<string, unknown> = {}, env = NOT_LOOPBACK_ENV): void {
  assert.throws(() => decode(provider, extra, env), (e: unknown) => e instanceof ProviderBindingError && rule.test(e.rule), `expected a ${rule} refusal`);
}

describe("decodeProviderBinding (PRD #4320 module 5)", () => {
  it("a vendor claim (no provider key) decodes to undefined; an empty oauth token alone is fine", () => {
    assert.equal(decodeProviderBinding({ forge_pat: "p", anthropic_oauth_token: "tok" }), undefined);
    assert.equal(decodeProviderBinding({ forge_pat: "p", anthropic_oauth_token: "" }), undefined);
    assert.equal(decodeProviderBinding(undefined), undefined);
  });

  it("accepts a well-formed binding, with and without a (present, empty or absent) oauth token, and rebuilds it without unknown fields", () => {
    const got = decode(binding({ smuggled: "x" }))!;
    assert.equal(got.base_url, "https://gw.example.test/anthropic");
    assert.equal(got.credential, KEY);
    assert.ok(!("smuggled" in got));
    assert.equal(decodeProviderBinding({ forge_pat: "p", provider: binding() })!.provider_id, "p-1");
  });

  it("refuses a provider next to an OAuth token, or next to a Codex block", () => {
    refuses(binding(), /anthropic_oauth_token/, { anthropic_oauth_token: ["oauth", "tok", "do-not-scan"].join("-") });
    refuses(binding(), /codex/, { codex: { auth_mode: "api_key" } });
  });

  it("refuses non-https base URLs, userinfo and unparseable URLs", () => {
    refuses(binding({ base_url: "http://gw.example.test/anthropic" }), /https/);
    refuses(binding({ base_url: "ftp://gw.example.test" }), /https/);
    refuses(binding({ base_url: "https://user:pw@gw.example.test/a" }), /userinfo/);
    refuses(binding({ base_url: "https://user@gw.example.test/a" }), /userinfo/);
    refuses(binding({ base_url: "not a url" }), /parse/);
    refuses(binding({ base_url: "" }), /base_url/);
  });

  it("http is accepted only for a loopback host AND the worker's own UZI_PROVIDER_ALLOW_LOOPBACK=true", () => {
    for (const host of ["127.0.0.1:4010", "localhost:4010", "[::1]:4010"]) {
      refuses(binding({ base_url: `http://${host}/v1x` }), /https/, {}, NOT_LOOPBACK_ENV);
      assert.equal(decode(binding({ base_url: `http://${host}/v1x` }), {}, LOOPBACK_ENV)!.base_url, `http://${host}/v1x`);
    }
    refuses(binding({ base_url: "http://gw.example.test/x" }), /https/, {}, LOOPBACK_ENV);
    refuses(binding({ base_url: "http://127.0.0.1.evil.test/x" }), /https/, {}, LOOPBACK_ENV);
    refuses(binding({ base_url: "http://127.0.0.1:4010/x" }), /https/, {}, { UZI_PROVIDER_ALLOW_LOOPBACK: "1" } as NodeJS.ProcessEnv);
  });

  it("refuses an oversized base URL", () => {
    refuses(binding({ base_url: `https://gw.example.test/${"a".repeat(2048)}` }), /oversized/);
  });

  it("refuses an unknown engine, protocol or auth style, and a non-positive revision", () => {
    refuses(binding({ engine: "codex" }), /engine/);
    refuses(binding({ protocol: "openai_responses" }), /protocol/);
    refuses(binding({ auth_style: "basic" }), /auth_style/);
    refuses(binding({ revision: 0 }), /revision/);
    refuses(binding({ provider_id: "" }), /provider_id/);
  });

  it("refuses a model map without haiku, with an unknown alias, or with a non-object entry", () => {
    refuses(binding({ model_map: { opus: { model: "o" }, sonnet: { model: "s" } } }), /haiku/);
    refuses(binding({ model_map: { haiku: { model: "h" }, ultra: { model: "u" } } }), /unknown alias/);
    refuses(binding({ model_map: { haiku: "h" } }), /not an object/);
    refuses(binding({ model_map: null }), /model_map/);
  });

  it("refuses model values outside [A-Za-z0-9._:/@-]{1,256}", () => {
    for (const bad of ["", "has space", "semi;colon", "new\nline", "x".repeat(257), "back`tick", "über"]) {
      refuses(binding({ model_map: { haiku: { model: bad } } }), /model is outside/);
    }
    assert.equal(decode(binding({ model_map: { haiku: { model: "a/b:c@d.e_f-g" } } }))!.model_map.haiku.model, "a/b:c@d.e_f-g");
    assert.equal(decode(binding({ model_map: { haiku: { model: "x".repeat(256) } } }))!.model_map.haiku.model.length, 256);
  });

  it("refuses a credential shorter than 16, longer than 4096, or not printable ASCII", () => {
    refuses(binding({ credential: "x".repeat(15) }), /credential/);
    refuses(binding({ credential: "x".repeat(4097) }), /credential/);
    refuses(binding({ credential: `${KEY} with space` }), /credential/);
    refuses(binding({ credential: `${KEY}\n` }), /credential/);
    refuses(binding({ credential: `${KEY}é` }), /credential/);
    refuses(binding({ credential: 42 }), /credential/);
    assert.equal(decode(binding({ credential: "x".repeat(16) }))!.credential.length, 16);
    assert.equal(decode(binding({ credential: "x".repeat(4096) }))!.credential.length, 4096);
  });

  it("refuses a context_window outside 8192..10000000 or non-integer; accepts the bounds", () => {
    for (const bad of [8191, 10_000_001, 0, -1, 1.5, "200000", Number.NaN, null]) {
      refuses(binding({ model_map: { haiku: { model: "h", context_window: bad } } }), /context_window/);
    }
    assert.equal(decode(binding({ model_map: { haiku: { model: "h", context_window: 8192 } } }))!.model_map.haiku.context_window, 8192);
    assert.equal(decode(binding({ model_map: { haiku: { model: "h", context_window: 10_000_000 } } }))!.model_map.haiku.context_window, 10_000_000);
  });

  it("the refusal never carries the credential or the URL", () => {
    try {
      decode(binding({ base_url: `http://user:${KEY}@gw.example.test/x`, credential: KEY }));
      assert.fail("expected a refusal");
    } catch (e) {
      assert.ok(e instanceof ProviderBindingError);
      assert.ok(!String((e as Error).message).includes(KEY));
      assert.ok(!String((e as Error).message).includes("gw.example.test"));
    }
  });

  it("sdkCredentialFromBinding maps the wire shape onto the SdkCredential", () => {
    const cred = sdkCredentialFromBinding(decode(binding({ auth_style: "x_api_key" }))!);
    assert.equal(cred.kind, "provider");
    if (cred.kind !== "provider") return;
    assert.equal(cred.baseUrl, "https://gw.example.test/anthropic");
    assert.equal(cred.authStyle, "x_api_key");
    assert.equal(cred.key, KEY);
    assert.equal(cred.modelMap.haiku.model, "corp-haiku");
  });
});

describe("resolveProviderModel (PRD #4320 module 7)", () => {
  const map = decode(binding({ model_map: { opus: { model: "corp-opus" }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } } }))!.model_map;

  it("allows a mapped alias or exactly a mapped target, unchanged", () => {
    assert.equal(resolveProviderModel("opus", map), "opus");
    assert.equal(resolveProviderModel("haiku", map), "haiku");
    assert.equal(resolveProviderModel("corp-sonnet", map), "corp-sonnet");
  });

  it("an empty or unset model is the sonnet alias", () => {
    assert.equal(resolveProviderModel(undefined, map), "sonnet");
    assert.equal(resolveProviderModel("", map), "sonnet");
    assert.equal(resolveProviderModel("   ", map), "sonnet");
  });

  it("refuses with a typed error naming the model, and never rewrites", () => {
    for (const bad of ["fable", "claude-opus-4-1", "opus[1m]", "Opus", "corp-sonnet ", "corp-haiku-2"]) {
      assert.throws(
        () => resolveProviderModel(bad, map),
        (e: unknown) => e instanceof ProviderModelUnmappedError && e.reason === "provider_model_unmapped" && e.message.includes(bad.trim()),
        bad,
      );
    }
  });

  it("an unset model on a map with no sonnet is refused naming the sonnet alias", () => {
    const noSonnet = { haiku: { model: "h" } };
    assert.throws(() => resolveProviderModel(undefined, noSonnet), (e: unknown) => e instanceof ProviderModelUnmappedError && e.model === "sonnet");
  });

  it("assertAgentModelsMapped checks every explicit agent model and ignores agents that inherit", () => {
    assertAgentModelsMapped({ a: {}, b: { model: "haiku" }, c: { model: "corp-opus" } }, map);
    assert.throws(() => assertAgentModelsMapped({ a: {}, b: { model: "claude-sonnet-4-5" } }, map), ProviderModelUnmappedError);
    assert.throws(() => assertAgentModelsMapped({ a: { model: "fable" } }, map), /fable/);
    // "inherit" is the SDK's parent-model sentinel: the lead is already resolved, so it is never refused.
    // Checked against a map WITHOUT sonnet, so an "inherit" mistaken for the sonnet default would throw.
    assertAgentModelsMapped({ a: { model: "inherit" } }, { haiku: { model: "corp-haiku" } });
    assert.equal(resolveProviderModel("inherit", map), resolveProviderModel(undefined, map));
  });
});

describe("stripProviderCost and isGatewayLimitStatus (PRD #4320 P7, D11)", () => {
  it("removes total_cost_usd and every modelUsage costUSD, keeping tokens", () => {
    const em: EmittedMessage = {
      kind: "status",
      agent: "lead",
      payload: {
        event: "result", total_cost_usd: 0.42, num_turns: 3,
        modelUsage: { "corp-sonnet": { inputTokens: 10, outputTokens: 5, costUSD: 0.4 }, "corp-haiku": { inputTokens: 1, costUSD: 0.02 } },
      },
    };
    const out = stripProviderCost(em);
    assert.ok(!("total_cost_usd" in out.payload));
    assert.deepStrictEqual(out.payload.modelUsage, { "corp-sonnet": { inputTokens: 10, outputTokens: 5 }, "corp-haiku": { inputTokens: 1 } });
    assert.equal(out.payload.num_turns, 3);
    assert.ok("total_cost_usd" in em.payload, "the input is not mutated");
  });

  it("returns a message with no cost untouched (same object)", () => {
    const em: EmittedMessage = { kind: "status", agent: "lead", payload: { text: "hi" } };
    assert.equal(stripProviderCost(em), em);
  });

  it("only 429 and 529 are gateway limits", () => {
    assert.equal(isGatewayLimitStatus(429), true);
    assert.equal(isGatewayLimitStatus(529), true);
    for (const s of [400, 401, 403, 500, 502, 503, 408, null, undefined]) assert.equal(isGatewayLimitStatus(s), false, String(s));
  });
});
