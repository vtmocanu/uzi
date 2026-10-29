import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { HARNESS_EFFORTS } from "../src/harness.js";
import { makeClaim, recordingLogger } from "./helpers.js";

// PRD #3247 claim guard: a claim's config.default_effort outside the closed effort set is dropped
// at decode time, on the run lane (issue/judge/job), the cross-check lane and the chat lane.

let body: Record<string, unknown>;
let marker: string | undefined;
let client: WorkerClient;
let lines: unknown[];

beforeEach(() => {
  marker = undefined;
  mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const pathname = new URL(String(input)).pathname;
    if (pathname.endsWith("/register")) return Response.json({ worker_id: "effort-worker" });
    if (pathname.endsWith("/runs/claim")) {
      return Response.json(body, marker ? { headers: { "X-Uzi-Claim-Kind": marker } } : undefined);
    }
    throw new Error("unexpected request: " + pathname);
  });
  const rec = recordingLogger();
  lines = rec.lines;
  client = new WorkerClient("http://effort.test", "test-worker-token", "test", rec.logger, { sleep: async () => {} });
});
afterEach(() => mock.restoreAll());

const LANES: ReadonlyArray<readonly [string, string, () => Promise<{ config?: unknown } | null>]> = [
  ["issue claim", "issue", () => client.claimRun()],
  ["judge claim", "judge", () => client.claimRun()],
  ["job claim", "job", () => client.claimRun()],
  ["cross-check lane claim", "cross_check", () => client.claimCrossCheck()],
  ["chat claim", "chat", () => client.claimChat()],
];

const cfg = (c: { config?: unknown } | null): Record<string, unknown> => c!.config as Record<string, unknown>;

function setBody(kind: string, config: Record<string, unknown> | undefined): void {
  body = { ...makeClaim(), claim_generation: 3, kind } as unknown as Record<string, unknown>;
  if (config !== undefined) body.config = config;
  if (kind === "cross_check") marker = "cross_check";
}

for (const [label, kind, claim] of LANES) {
  describe(label, () => {
    for (const bad of ["runtime", "ultra", "", 42, null, ["high"]] as const) {
      it(`drops default_effort ${JSON.stringify(bad)} without echoing it`, async () => {
        await client.register("effort-worker");
        setBody(kind, { default_model: "m", default_effort: bad });
        const decoded = await claim();
        assert.ok(decoded);
        assert.equal(Object.hasOwn(cfg(decoded), "default_effort"), false);
        assert.equal(cfg(decoded).default_model, "m", "sibling config survives");
        const warns = (lines as Array<{ level: string; msg: string }>).filter((l) => l.level === "warn"
          && l.msg.includes("default_effort"));
        assert.equal(warns.length, 1);
        if (typeof bad === "string" && bad !== "") assert.ok(!JSON.stringify(lines).includes(bad));
      });
    }
    for (const ok of HARNESS_EFFORTS) {
      it(`keeps default_effort ${ok}`, async () => {
        await client.register("effort-worker");
        setBody(kind, { default_effort: ok });
        const decoded = await claim();
        assert.equal(cfg(decoded).default_effort, ok);
        assert.equal((lines as Array<{ level: string }>).some((l) => l.level === "warn"), false);
      });
    }
    it("leaves an absent default_effort absent", async () => {
      await client.register("effort-worker");
      setBody(kind, { default_model: "m" });
      const decoded = await claim();
      assert.equal(Object.hasOwn(cfg(decoded), "default_effort"), false);
      setBody(kind, undefined);
      assert.equal((await claim())!.config, undefined);
      assert.equal((lines as Array<{ level: string }>).some((l) => l.level === "warn"), false);
    });
  });
}
