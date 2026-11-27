import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { RUN_ENV_CAPABILITY } from "../src/run-env-claim.js";
import { makeClaim, recordingLogger } from "./helpers.js";

// PRD 4416 M2: the run lane decodes secrets.run_env / run_env_redact fail-closed; the chat lane ignores them.
const fixture = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/claims/run-env-claim.json", import.meta.url), "utf8"),
) as { run_env: unknown[]; run_env_redact: unknown[] };

let body: Record<string, unknown>;
let client: WorkerClient;

beforeEach(() => {
  mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const pathname = new URL(String(input)).pathname;
    if (pathname.endsWith("/register")) return Response.json({ worker_id: "run-env-worker" });
    if (pathname.endsWith("/runs/claim")) return Response.json(body);
    throw new Error("unexpected request: " + pathname);
  });
  client = new WorkerClient("http://run-env.test", "test-worker-token", "test", recordingLogger().logger, { sleep: async () => {} });
});
afterEach(() => mock.restoreAll());

function setBody(secrets: Record<string, unknown>): void {
  body = { ...makeClaim(), claim_generation: 3, kind: "issue", secrets: { forge_pat: "p", ...secrets } } as unknown as Record<string, unknown>;
}

type Secrets = { run_env?: unknown[]; run_env_redact?: unknown[] };

describe("run lane run_env decode", () => {
  it("keeps a well-formed run_env and run_env_redact", async () => {
    await client.register("run-env-worker", undefined, undefined, undefined, [RUN_ENV_CAPABILITY]);
    setBody({ run_env: fixture.run_env, run_env_redact: fixture.run_env_redact });
    const secrets = (await client.claimRun())!.secrets as Secrets;
    assert.equal(secrets.run_env?.length, 2);
    assert.equal(secrets.run_env_redact?.length, 1);
  });

  it("leaves a claim without variables untouched", async () => {
    await client.register("run-env-worker", undefined, undefined, undefined, [RUN_ENV_CAPABILITY]);
    setBody({});
    const secrets = (await client.claimRun())!.secrets as Secrets;
    assert.ok(!("run_env" in secrets));
    assert.ok(!("run_env_redact" in secrets));
  });

  it("rejects a malformed list without echoing a value", async () => {
    await client.register("run-env-worker", undefined, undefined, undefined, [RUN_ENV_CAPABILITY]);
    const bad = { ...(fixture.run_env[1] as Record<string, unknown>), name: "lower_case", value: "dummy-value-do-not-scan-1234" };
    setBody({ run_env: [bad] });
    await assert.rejects(client.claimRun(), (e: Error) => /run_env/.test(e.message) && !e.message.includes("dummy-value-do-not-scan-1234"));
    setBody({ run_env: "not-a-list" });
    await assert.rejects(client.claimRun(), /run_env/);
  });

  it("the chat lane ignores the field", async () => {
    await client.register("run-env-worker", undefined, undefined, undefined, [RUN_ENV_CAPABILITY]);
    setBody({ run_env: "not-a-list" });
    assert.ok(await client.claimChat());
  });
});
