import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
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
      await client.register("provider-worker");
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "", provider: provider() });
      const decoded = await claim();
      assert.equal((decoded!.secrets as Secrets).provider?.credential, KEY);
    });

    it("leaves a vendor claim (no provider key) untouched", async () => {
      await client.register("provider-worker");
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "vendor-token-do-not-scan" });
      const decoded = await claim();
      assert.ok(!("provider" in (decoded!.secrets as object)));
    });

    it("refuses BOTH bindings: a provider next to a non-empty OAuth token", async () => {
      await client.register("provider-worker");
      setBody(kind, { forge_pat: "p", anthropic_oauth_token: "vendor-token-do-not-scan", provider: provider() });
      await assert.rejects(claim(), (e: Error) => /malformed provider binding/.test(e.message) && !e.message.includes(KEY));
    });

    for (const [name, over] of [
      ["a plain-http base URL", { base_url: "http://gw.example.test/a" }],
      ["userinfo in the base URL", { base_url: "https://u:p@gw.example.test/a" }],
      ["an unknown engine", { engine: "codex" }],
      ["a model map without haiku", { model_map: { sonnet: { model: "s" } } }],
      ["a short credential", { credential: "short" }],
      ["an out-of-range context window", { model_map: { haiku: { model: "h", context_window: 100 } } }],
    ] as const) {
      it(`refuses ${name}`, async () => {
        await client.register("provider-worker");
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
    await client.register("provider-worker");
    delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    setBody("issue", { forge_pat: "p", anthropic_oauth_token: "", provider: provider({ base_url: "http://127.0.0.1:4010/anthropic" }) });
    await assert.rejects(client.claimRun(), /malformed provider binding/);
    process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
    const decoded = await client.claimRun();
    assert.equal((decoded!.secrets as Secrets).provider?.base_url, "http://127.0.0.1:4010/anthropic");
  });
});
