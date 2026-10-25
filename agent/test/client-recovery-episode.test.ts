import assert from "node:assert/strict";
import { afterEach, beforeEach, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { GitCache, type RecoveryJournalEntry } from "../src/git.js";
import { recoveryProgress } from "../src/recovery-progress.js";
import { makeClaim, nullLogger } from "./helpers.js";

let claim: Record<string, unknown>;
let client: WorkerClient;
let paths: string[];

beforeEach(() => {
  claim = { ...makeClaim(), claim_generation: 5 };
  paths = [];
  mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
    const pathname = new URL(String(input)).pathname;
    paths.push(pathname);
    if (pathname.endsWith("/register")) return Response.json({ worker_id: "episode-worker" });
    if (pathname.endsWith("/runs/claim")) return Response.json(claim);
    throw new Error("unexpected request: " + pathname);
  });
  client = new WorkerClient("http://episode.test", "test-worker-token", "test", nullLogger(), {
    sleep: async () => {},
  });
});
afterEach(() => mock.restoreAll());

it("claimRun preserves missing, zero, positive and max-safe recovery episodes through JSON decoding", async () => {
  await client.register("episode-worker");
  const legacy = await client.claimRun();
  assert.ok(legacy);
  assert.equal(Object.hasOwn(legacy, "worker_recovery_episode"), false);
  for (const episode of [0, 7, Number.MAX_SAFE_INTEGER]) {
    claim.worker_recovery_episode = episode;
    const decoded = await client.claimRun();
    assert.ok(decoded);
    assert.equal(decoded.worker_recovery_episode, episode);
    assert.equal((await client.claimChat())?.worker_recovery_episode, episode);
    assert.equal(decoded.claim_generation, 5);
    assert.equal(decoded.run_id, claim.run_id);
  }
  assert.equal(paths.filter(p => p.endsWith("/runs/claim")).length, 7);
});

for (const [label, episode] of [
  ["negative", -1], ["fractional", 0.5], ["unsafe", Number.MAX_SAFE_INTEGER + 1],
  ["string", "8"], ["null", null], ["true", true], ["false", false],
  ["array", [8]], ["object", { episode: 8 }],
] as const) {
  it(`claimRun silently drops ${label} episode and cannot renew exhausted progress`, async () => {
    claim.worker_recovery_episode = episode;
    await client.register("episode-worker");
    const decoded = await client.claimRun();
    assert.ok(decoded, "malformed optional episode must not reject the claim");
    assert.equal(Object.hasOwn(decoded, "worker_recovery_episode"), false);
    const chat = await client.claimChat();
    assert.ok(chat);
    assert.equal(Object.hasOwn(chat, "worker_recovery_episode"), false);
    assert.equal(decoded.claim_generation, 5);
    assert.equal(decoded.run_id, claim.run_id);
    assert.ok(paths.some(p => p.endsWith("/runs/claim")));

    // Keep the production reservation/parser boundary while replacing only persistent IO.
    const source = { runId: "episode-run", clonePath: "/tmp/episode-run" };
    let journal: RecoveryJournalEntry = {
      ...source, recovery: {
        version: 1, source, attempts: 3, startedAt: 1_000_000, deadline: 1_300_000,
        backoffMs: 4000, stage: "blocked", blocker: "budget_exhausted",
        claimGeneration: 4, episode: 7,
      },
    };
    const before = structuredClone(journal);
    mock.method(Date, "now", () => 1_400_000);
    const cache = new GitCache("/tmp/episode-cache", nullLogger());
    const seam = cache as unknown as {
      withLock: (bare: string, action: () => Promise<unknown>) => Promise<unknown>;
      checkedRecovery: () => Promise<RecoveryJournalEntry>;
      writeRecovery: (bare: string, branch: string, value: RecoveryJournalEntry) => Promise<void>;
      runGit: () => Promise<string>;
    };
    seam.withLock = async (_bare, action) => action();
    seam.checkedRecovery = async () => {
      recoveryProgress(journal.recovery);
      return structuredClone(journal);
    };
    seam.writeRecovery = async (_bare, _branch, value) => { journal = structuredClone(value); };
    seam.runGit = async () => "";
    await assert.rejects(cache.reserveRecoveryIteration(
      "/tmp/bare", "branch", "key", source, 1000,
      decoded.claim_generation, decoded.worker_recovery_episode,
    ), /blocked/);
    assert.deepEqual(journal, before);
  });
}
