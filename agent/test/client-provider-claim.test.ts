import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { CLAUDE_PROVIDER_CAPABILITY } from "../src/provider-binding.js";
import { makeClaim, recordingLogger } from "./helpers.js";

// PRD #4320 module 5: the claim decoder is fail-closed on a provider binding, on the run lane
// (issue/judge/job) and the chat lane. A binding next to an OAuth token, or any malformed field,
// makes the claim throw (the claim loop's malformed-claim path); a vendor claim is untouched.
// The gateway key is assembled at runtime (no provider-token-shaped literal in source).
const KEY = ["gw", "key", "do-not-scan", "8888", "abcdefgh"].join("-");

let body: Record<string, unknown>;
let client: WorkerClient;

beforeEach(() => {
  mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const pathname = new URL(String(input)).pathname;
    if (pathname.endsWith("/register")) return Response.json({ worker_id: "provider-worker" });
    if (pathname.endsWith("/runs/claim")) return Response.json(body);
    throw new Error("unexpected request: " + pathname);
  });
  client = new WorkerClient("http://provider.test", "test-worker-token", "test", recordingLogger().logger, { sleep: async () => {} });
});
afterEach(() => mock.restoreAll());

const provider = (over: Record<string, unknown> = {}): Record<string, unknown> => ({
  provider_id: "p-1", revision: 4, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/anthropic",
  auth_style: "bearer", credential: KEY, model_map: { sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } }, ...over,
});

type Secrets = { provider?: { credential: string; base_url: string } };
const LANES: ReadonlyArray<readonly [string, string, () => Promise<{ secrets?: unknown } | null>]> = [
  ["issue claim", "issue", () => client.claimRun()],
  ["job claim", "job", () => client.claimRun()],
  ["chat claim", "chat", () => client.claimChat()],
];

function setBody(kind: string, secrets: Record<string, unknown>): void {
  body = { ...makeClaim(), claim_generation: 3, kind, secrets } as unknown as Record<string, unknown>;
}

for (const [label, kind, claim] of LANES) {
  describe(label, () => {
    it("decodes a well-formed provider binding with an empty oauth token", async () => {
      await client.register("provider-worker", undefined, undefined, undefined, [CLAUDE_PROVIDER_CAPABILITY]);
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "", provider: provider() });
      const decoded = await claim();
      assert.equal((decoded!.secrets as Secrets).provider?.credential, KEY);
    });

    it("refuses a provider claim when the worker did not advertise claude_provider_v1", async () => {
      await client.register("provider-worker", undefined, undefined, undefined, ["cross_check_v1"]);
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "", provider: provider() });
      await assert.rejects(claim(), (e: Error) => /malformed provider binding on claim: capability not advertised/.test(e.message) && !e.message.includes(KEY));
      // A vendor claim is unaffected by the missing capability.
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "vendor-token-do-not-scan" });
      assert.ok(await claim());
    });

    it("leaves a vendor claim (no provider key) untouched", async () => {
      await client.register("provider-worker", undefined, undefined, undefined, [CLAUDE_PROVIDER_CAPABILITY]);
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "vendor-token-do-not-scan" });
      const decoded = await claim();
      assert.ok(!("provider" in (decoded!.secrets as object)));
    });

    it("refuses BOTH bindings: a provider next to a non-empty OAuth token", async () => {
      await client.register("provider-worker", undefined, undefined, undefined, [CLAUDE_PROVIDER_CAPABILITY]);
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "vendor-token-do-not-scan", provider: provider() });
      await assert.rejects(claim(), (e: Error) => /malformed provider binding/.test(e.message) && !e.message.includes(KEY));
    });

    for (const [name, over] of [
      ["a plain-http base URL", { base_url: "http://gw.example.test/a" }],
      ["userinfo in the base URL", { base_url: "https://u:p@gw.example.test/a" }],
      ["an unknown engine", { engine: "other" }],
      ["a model map without haiku", { model_map: { sonnet: { model: "s" } } }],
      ["a short credential", { credential: "short" }],
      ["an out-of-range context window", { model_map: { haiku: { model: "h", context_window: 100 } } }],
    ] as const) {
      it(`refuses ${name}`, async () => {
        await client.register("provider-worker", undefined, undefined, undefined, [CLAUDE_PROVIDER_CAPABILITY]);
        setBody(kind, { forge_pat: "p", anthropic_oauth_token: "", provider: provider(over) });
        await assert.rejects(claim(), /malformed provider binding/);
      });
    }
  });
}

describe("loopback is accepted only with the worker's own UZI_PROVIDER_ALLOW_LOOPBACK=true", () => {
  const prior = process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
  afterEach(() => {
    if (prior === undefined) delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    else process.env.UZI_PROVIDER_ALLOW_LOOPBACK = prior;
  });

  it("refuses http loopback by default and accepts it with the dev flag", async () => {
    await client.register("provider-worker", undefined, undefined, undefined, [CLAUDE_PROVIDER_CAPABILITY]);
    delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    setBody("issue", { forge_pat: "p", anthropic_oauth_token: "", provider: provider({ base_url: "http://127.0.0.1:4010/anthropic" }) });
    await assert.rejects(client.claimRun(), /malformed provider binding/);
    process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
    const decoded = await client.claimRun();
    assert.equal((decoded!.secrets as Secrets).provider?.base_url, "http://127.0.0.1:4010/anthropic");
  });
});

// PRD #4323: a Codex provider claim is accepted only when this registration advertised
// codex_provider_v1 with the Codex harness and runtime v2 (the trio the api admits it on).
const codexProvider = (): Record<string, unknown> => provider({ engine: "codex", protocol: "openai_responses",
  model_map: { default: { model: "gateway-default" } } });
const CODEX_CAPS = ["codex_provider_v1", "codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"];

describe("a failed re-register fails closed", () => {
  it("refuses a Codex provider claim after a re-register that throws following a successful one", async () => {
    await client.register("provider-worker", undefined, undefined, undefined, CODEX_CAPS);
    mock.restoreAll();
    mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
      const pathname = new URL(String(input)).pathname;
      if (pathname.endsWith("/register")) return new Response("bad request", { status: 400 });
      if (pathname.endsWith("/runs/claim")) return Response.json(body);
      throw new Error("unexpected request: " + pathname);
    });
    await assert.rejects(client.register("provider-worker", undefined, undefined, undefined, CODEX_CAPS));
    setBody("issue", { provider: codexProvider() });
    await assert.rejects(client.claimRun(), (e: unknown) => e instanceof Error &&
      "reason" in e && e.reason === "provider_lane_unsupported");
  });
});

for (const [label, kind, claim] of LANES) {
  it(`${label} accepts a Codex provider when codex_provider_v1 and the Codex runtime capabilities are advertised`, async () => {
    await client.register("provider-worker", undefined, undefined, undefined, CODEX_CAPS);
    setBody(kind, { provider: codexProvider() });
    const decoded = await claim();
    assert.equal((decoded!.secrets as { provider?: { engine: string } }).provider?.engine, "codex");
  });

  for (const [control, caps] of [
    ["a legacy worker with no provider capabilities", []],
    ["an A1-only worker (claude_provider_v1 only)", [CLAUDE_PROVIDER_CAPABILITY]],
    ["a Codex-disabled worker (codex_provider_v1 without the harness)", [CLAUDE_PROVIDER_CAPABILITY, "codex_provider_v1"]],
    ["a worker missing codex_runtime_v2", ["codex_provider_v1", "codex_harness_v1"]],
    ["a worker missing the Codex harness (provider and runtime v2 only)", ["codex_provider_v1", "codex_runtime_v2"]],
    ["a worker with the Codex runtime but not codex_provider_v1", ["codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"]],
  ] as const) {
    it(`${label} refuses a Codex provider on ${control}`, async () => {
      await client.register("provider-worker", undefined, undefined, undefined, [...caps]);
      setBody(kind, { provider: codexProvider() });
      await assert.rejects(claim(), (e: unknown) => e instanceof Error &&
        "reason" in e && e.reason === "provider_lane_unsupported" && !e.message.includes(KEY));
    });
  }
}
