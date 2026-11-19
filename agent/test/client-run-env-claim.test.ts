import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { makeClaim, recordingLogger } from "./helpers.js";

// PRD #4416 M2: claimRun decodes the repository variables fail-closed (a breach makes the claim
// unusable), delivers the decoded set on secrets, and the chat claim ignores the field.
const FIXTURE = JSON.parse(readFileSync(new URL("../../fixtures/claims/run-env-claim.json", import.meta.url), "utf8")) as Record<string, unknown>;

let body: Record<string, unknown>;
let client: WorkerClient;

beforeEach(() => {
  mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const pathname = new URL(String(input)).pathname;
    if (pathname.endsWith("/register")) return Response.json({ worker_id: "run-env-worker" });
    if (pathname.endsWith("/runs/claim")) return Response.json(body);
    throw new Error("unexpected request: " + pathname);
  });
  client = new WorkerClient("http://runenv.test", "test-worker-token", "test", recordingLogger().logger, { sleep: async () => {} });
});
afterEach(() => mock.restoreAll());

function setBody(secrets: Record<string, unknown>): void {
  body = { ...makeClaim(), claim_generation: 3, kind: "issue", secrets } as unknown as Record<string, unknown>;
}

describe("run_env on the claim", () => {
  it("delivers the decoded set", async () => {
    setBody({ forge_pat: "p", ...FIXTURE });
    const claim = await client.claimRun();
    const s = claim!.secrets as { run_env?: Array<{ name: string }>; run_env_redact?: unknown[] };
    assert.deepEqual(s.run_env?.map((v) => v.name), ["SERVICE_URL", "SERVICE_PASSPHRASE"]);
    assert.equal(s.run_env_redact?.length, 2);
  });

  it("leaves a claim without variables untouched", async () => {
    setBody({ forge_pat: "p" });
    const claim = await client.claimRun();
    assert.ok(!("run_env" in (claim!.secrets as object)));
    assert.ok(!("run_env_redact" in (claim!.secrets as object)));
  });

  it("fails closed on a malformed entry without echoing the value", async () => {
    const bad = JSON.parse(JSON.stringify(FIXTURE)) as { run_env: Array<Record<string, unknown>> };
    bad.run_env[1]!["value"] = "short";
    setBody({ forge_pat: "p", ...bad });
    await assert.rejects(client.claimRun(), (e: Error) => e.name === "RunEnvClaimError" && !e.message.includes("short\""));
  });

  it("fails closed on a non-array run_env", async () => {
    setBody({ forge_pat: "p", run_env: { name: "X" } });
    await assert.rejects(client.claimRun(), (e: Error) => e.name === "RunEnvClaimError");
  });

  it("the chat claim ignores the field", async () => {
    setBody({ forge_pat: "p", run_env: "not-even-an-array" });
    const claim = await client.claimChat();
    assert.ok(claim);
  });
});
