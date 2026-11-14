import { it } from "node:test";
import assert from "node:assert/strict";
import { RecoveryCoordinator } from "../src/recovery.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { hibernationWriters } from "../src/hibernation-inventory.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// PRD #4208: recovery passes write capture records and bundles after the run that spawned them has
// left runActive, so they are registered hibernation writers (Recovery.trackPass).
const tick = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

function coordinator(t: { after: (fn: () => void) => void; name: string }) {
  const fx = makeFixture({}, { testName: t.name });
  t.after(() => fx.cleanup());
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const client = new WorkerClient("http://127.0.0.1:1", "tok", "0.1.0-test", nullLogger());
  return new RecoveryCoordinator({ client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: "tok" });
}

it("a terminal recovery pass in flight when the latch lands is waited for by settle", async (t) => {
  const c = coordinator(t as never);
  t.after(() => hibernationWriters.unlatch());
  let release!: () => void;
  const tail = new Promise<void>((res) => { release = res; });
  const pass = c.resumeTerminalEvidence("run-hib-1", tail, 1);
  assert.deepEqual(hibernationWriters.busyClasses(), ["recovery_pass"]);
  hibernationWriters.latch();
  let settled = false;
  const settle = hibernationWriters.settle(5000).then(() => { settled = true; });
  await tick(80);
  assert.equal(settled, false, "settle must wait for the in-flight recovery pass");
  release();
  await pass;
  await settle;
  assert.equal(settled, true);
  assert.equal(hibernationWriters.inFlight(), 0);
});

it("a new recovery pass is refused while the writer latch is held", async (t) => {
  const c = coordinator(t as never);
  t.after(() => hibernationWriters.unlatch());
  hibernationWriters.latch();
  // A pass that ran would await this never-settling tail and hang; a refused one returns at once.
  const never = new Promise<void>(() => {});
  await c.resumeTerminalEvidence("run-hib-2", never, 1);
  await c.resumePending();
  assert.equal(hibernationWriters.inFlight(), 0, "a refused pass is not counted");
  hibernationWriters.unlatch();
  await c.resumeTerminalEvidence("run-hib-2", Promise.resolve(), 1);
  assert.equal(hibernationWriters.inFlight(), 0);
});
