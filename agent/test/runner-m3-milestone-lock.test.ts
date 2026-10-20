import assert from "node:assert/strict";
import { it } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import { CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { api, client, deferred, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, drain, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";
import { waitForTestEvent } from "./helpers.js";

installHarness();
installRecoveryRootCleanup();

it("m3 milestone import lock soft stop forfeits its callback and the next nonreaped checkpoint publishes original H1", async t => {
  const clock = virtualBoundaryClock();
  t.mock.method(Date, "now", clock.now);
  const ready = deferred(), acquired = deferred(), held = deferred(), stopped = deferred(), retry = deferred();
  const bare = git.barePathFor(fx.originPath);
  const locks = (git as unknown as { locks: Map<string, Promise<void>> }).locks;
  const softArms: number[] = [], steps: string[] = [], tips: string[] = [];
  const publications: Array<{ lastPublishedTip?: string; checkpointFloor?: string }> = [];
  const imports: Array<Awaited<ReturnType<typeof git.fetchAgentBranch>>> = [];
  const errors: unknown[] = [];
  let holderChain: Promise<void> | undefined;
  let importMode: string | undefined;
  let enqueued = false, released = false, returnedWhileHeld = false, completed = false;
  let first = true, active = false, h1 = "", branch = "", source = "", runId = "";
  let rig: ReturnType<typeof codexRig> | undefined;
  const fetch = git.fetchAgentBranch.bind(git);
  t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
    if (!active) return fetch(...args);
    if (!first) {
      const result = await fetch(...args);
      imports.push(result);
      return result;
    }
    first = false;
    importMode = args[4].checkpointImportMode;
    ready.resolve();
    await acquired.promise; // The predecessor is acquired outside this import's ALS scope.
    const step = args[4].checkpointStep;
    try {
      return await fetch(args[0], args[1], args[2], args[3], {
        ...args[4],
        checkpointStep: name => {
          step?.(name);
          steps.push(name);
          if (name === "checkpoint_import_lock_wait") {
            // withLock installs its chain synchronously after the hook returns.
            queueMicrotask(() => {
              enqueued = locks.get(bare) !== holderChain;
              clock.advance(10_000);
            });
          }
        },
      });
    } catch (error) { errors.push(error); throw error; }
  });
  t.mock.method(client, "publishCheckpoint", async (_id: string, tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
    tips.push(tip);
    await drain(pack);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/m3-lock" } };
  });
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({
        lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        now: clock.now, armDeadline: clock.armDeadline,
      });
      source = ctx.worktreePath; branch = ctx.branch; runId = ctx.runId;
      commitInTree(source, "m3-lock.txt", "original waiting milestone\n");
      h1 = (await git.branchTip(source, branch))!;
      active = true;
      await ctx.checkpoint!({ reap: true, sink: "milestone_checkpoint", progress: { completed: ["m3"], in_progress: [] } });
      returnedWhileHeld = !released;
      stopped.resolve();
      await retry.promise;
      await ctx.checkpoint!({ reap: false });
      active = false;
      completed = true;
      return { branch };
    },
  };
  const { github } = fakeGitHub();
  const forge = fakeGitlab();
  client.protocolFeatures = ["run_checkpoint_durability"];
  const claim = gitlabClaim(312380, {
    repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
  });
  const startedAt = clock.now();
  const execution = runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, {
    github, checkpointIntervalMs: 1, now: clock.now, setTickTimer: () => () => {},
    checkpointTestHooks: {
      absoluteNow: clock.now,
      armSoftDeadline: (fire, ms) => { softArms.push(ms); return clock.arm(fire, ms); },
      afterPinnedPublish: state => publications.push(structuredClone(state)),
      afterUnpinnedPublish: state => publications.push(structuredClone(state)),
    },
  }).execute(claim);
  const read = (...args: string[]) => execFileSync("git", ["-C", bare, ...args], { encoding: "utf8" }).trim();
  const snapshot = () => ({
    refs: read("for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/", "refs/uzi-runner/", "refs/uzi-owed/"),
    config: fs.readFileSync(path.join(bare, "config"), "utf8"),
    receipts: fs.existsSync(path.join(bare, "uzi-owed"))
      ? fs.readdirSync(path.join(bare, "uzi-owed")).sort().map(name => [name, fs.readFileSync(path.join(bare, "uzi-owed", name), "utf8")])
      : [],
  });
  let holder: Promise<void> | undefined;
  let before: ReturnType<typeof snapshot> | undefined, atStop: typeof before, afterRelease: typeof before;
  let sourceAtStop = "", tipsAtStop: string[] = [], publicationsAtStop: unknown[] = [];
  let statesAtStop: typeof api.states = [];
  try {
    await waitForTestEvent(ready.promise, "real milestone import reached after preliminary reads");
    holder = git.withBareLock(bare, async () => { acquired.resolve(); await held.promise; });
    holderChain = locks.get(bare);
    before = snapshot();
    await waitForTestEvent(stopped.promise, "milestone returns while predecessor remains held");
    atStop = snapshot();
    sourceAtStop = execFileSync("git", ["-C", source, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    tipsAtStop = [...tips]; publicationsAtStop = [...publications]; statesAtStop = structuredClone(api.states);
    released = true; held.resolve();
    await holder;
    await locks.get(bare); // Drain the forfeited slot before inspecting for late mutations.
    afterRelease = snapshot();
  } finally {
    released = true; held.resolve(); acquired.resolve(); retry.resolve();
    const settled = await Promise.allSettled([holder, execution]);
    await Promise.resolve().then(() => {
      for (const result of settled) if (result.status === "rejected") throw result.reason;
    });
  }
  assert.ok(holderChain, "the external predecessor installed the real bare lock chain");
  assert.equal(importMode, "milestone");
  assert.equal(enqueued, true, "the actual import waiter joined the held bare chain");
  assert.equal(returnedWhileHeld, true);
  assert.equal(completed, true);
  assert.equal(clock.now() - startedAt, 10_000);
  assert.deepEqual(softArms, [10_000]);
  assert.deepEqual(clock.budgets.filter(b => b.sink === "milestone_checkpoint").map(b => b.ms), [30_000]);
  assert.equal(errors.length, 1);
  assert.ok(errors[0] instanceof CheckpointSoftDeadlineError);
  assert.deepEqual(steps, ["checkpoint_import_lock_wait"], "transport, poststage and mutation callbacks never run, including after release");
  assert.deepEqual(atStop, before, "no stage, tracking, ownership or receipt mutation at soft stop");
  assert.deepEqual(afterRelease, before, "the cancelled import never mutates late");
  assert.equal(sourceAtStop, h1);
  assert.deepEqual(tipsAtStop, []);
  assert.deepEqual(publicationsAtStop, []);
  assert.ok(!statesAtStop.some(s => s.body.checkpoint_contains_latest === true), "no false remote durability report");
  assert.equal(imports.length, 1, "the next nonreaped checkpoint performs a real import");
  assert.equal(imports[0]!.kind, "updated");
  if (imports[0]!.kind === "updated") assert.equal(imports[0]!.candidateSha, h1);
  assert.deepEqual(tips, [h1], "the retry packs the exact retained source head");
  assert.equal(publications.length, 1, "runner records confirmed remote durability only after publication");
  assert.equal(publications[0]!.lastPublishedTip, h1);
  assert.equal(publications[0]!.checkpointFloor, h1);
  assert.equal(await git.trackingTip(bare, branch), h1);
  assert.equal((await git.committedTrackingOwnership(bare, branch, runId, h1)).kind, "owned");
});
