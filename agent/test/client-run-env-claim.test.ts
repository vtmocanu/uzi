import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { claimRunEnv } from "../src/run-kind.js";
import { makeClaim, recordingLogger } from "./helpers.js";

// PRD #4416: a malformed run_env / run_env_redact rejects the run-lane claim fail-closed (naming the
// field and rule, never a value); the chat claim decoder ignores both fields.
const VALUE = ["run", "env", "value", "do-not-scan", "1234"].join("-");

let body: Record<string, unknown>;
let client: WorkerClient;

beforeEach(() => {
  mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const pathname = new URL(String(input)).pathname;
    if (pathname.endsWith("/register")) return Response.json({ worker_id: "env-worker" });
    if (pathname.endsWith("/runs/claim")) return Response.json(body);
    throw new Error("unexpected request: " + pathname);
  });
  client = new WorkerClient("http://env.test", "test-worker-token", "test", recordingLogger().logger, { sleep: async () => {} });
});
afterEach(() => mock.restoreAll());

const live = (over: Record<string, unknown> = {}): Record<string, unknown> => ({
  var_id: "11111111-1111-4111-8111-111111111111", scope: "repo", rev: 1, name: "MY_TOKEN", value: VALUE,
  secret: true, description: "", ...over,
});

function setBody(kind: string, secrets: Record<string, unknown>): void {
  body = { ...makeClaim(), claim_generation: 3, kind, secrets: { forge_pat: "p", anthropic_oauth_token: "vendor-token-do-not-scan", ...secrets } } as unknown as Record<string, unknown>;
}

describe("run lane claim", () => {
  it("decodes run_env and run_env_redact", async () => {
    setBody("issue", { run_env: [live()], run_env_redact: [{ ...live({ var_id: "22222222-2222-4222-8222-222222222222" }), secret: undefined, description: undefined }] });
    const claim = await client.claimRun();
    const secrets = claim!.secrets;
    assert.equal(secrets.run_env?.[0]?.name, "MY_TOKEN");
    assert.equal(secrets.run_env_redact?.length, 1);
  });

  it("leaves a claim without variables untouched", async () => {
    setBody("issue", {});
    const claim = await client.claimRun();
    assert.equal(Object.hasOwn(claim!.secrets, "run_env"), false);
    assert.equal(Object.hasOwn(claim!.secrets, "run_env_redact"), false);
  });

  for (const [label, secrets] of [
    ["a bad name", { run_env: [live({ name: "lower" })] }],
    ["a NUL in a value", { run_env: [live({ value: "ab\u0000cdefgh" })] }],
    ["a non-array run_env", { run_env: "nope" }],
    ["a short secret", { run_env: [live({ value: "short" })] }],
    ["a malformed redact entry", { run_env_redact: [{ name: "X" }] }],
  ] as const) {
    it(`rejects ${label} without echoing a value`, async () => {
      setBody("issue", secrets);
      await assert.rejects(client.claimRun(), (e: Error) => /run_env claim/.test(e.message) && !e.message.includes(VALUE));
    });
  }
});

describe("chat claim", () => {
  it("ignores run_env even when malformed", async () => {
    setBody("chat", { run_env: "nope", run_env_redact: 7 });
    const claim = await client.claimChat();
    assert.ok(claim);
  });
});

describe("claimRunEnv (lead-lane gate)", () => {
  const vars = [{ ...(live() as object), name: "A_VAR", value: "v" }] as never;
  const secrets = { run_env: vars };
  for (const kind of ["issue", "prompt", "self_improve", "ci_fix", "mr_rework", "task", undefined] as const) {
    it(`passes name and value only for ${kind ?? "a missing kind"}`, () => {
      assert.deepEqual(claimRunEnv({ kind, secrets }), [{ name: "A_VAR", value: "v" }]);
    });
  }
  for (const kind of ["chat", "judge", "job", "cross_check"] as const) {
    it(`gives ${kind} nothing`, () => {
      assert.equal(claimRunEnv({ kind, secrets }), undefined);
    });
  }
  it("gives a task-review run (review target set) nothing, and an empty list nothing", () => {
    assert.equal(claimRunEnv({ kind: "task", review_target_run_id: "r", secrets }), undefined);
    assert.equal(claimRunEnv({ kind: "issue", secrets: { run_env: [] } }), undefined);
    assert.equal(claimRunEnv({ kind: "issue", secrets: {} }), undefined);
  });
});
