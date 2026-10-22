import assert from "node:assert/strict";
import { it } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Executor } from "../src/executor.js";
import type { BoundaryStep } from "../src/harness.js";
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

for (const expiry of ["returned updated", "queued readback"] as const) {
  it(`3304 post-import holder soft stop: ${expiry}`, async t => {
    const clock = virtualBoundaryClock();
    t.mock.method(Date, "now", clock.now);
    const ready = deferred(), acquired = deferred(), held = deferred(), stopped = deferred(), retry = deferred();
    const bare = git.barePathFor(fx.originPath);
    const locks = (git as unknown as { locks: Map<string, Promise<void>> }).locks;
    const tips: string[] = [], publications: Array<{ lastPublishedTip?: string; checkpointFloor?: string }> = [];
    const outcomes: Array<{ key: string; step?: unknown }> = [];
    const fetch = git.fetchAgentBranch.bind(git);
    const internal = git as unknown as { withLock<T>(key: string, action: () => Promise<T>, signal?: AbortSignal): Promise<T> };
    const withLock = internal.withLock.bind(git);
    let active = false, imported = false, queued = false, returned = false, released = false;
    let head = "", runId = "", branch = "";
    let failure: unknown, rig: ReturnType<typeof codexRig> | undefined;
    let holderChain: Promise<void> | undefined;
    t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      const result = await fetch(...args);
      if (active && !imported) {
        assert.equal(result.kind, "updated");
        imported = true;
        ready.resolve();
        await acquired.promise;
        if (expiry === "returned updated") clock.advance(10_000);
      }
      return result;
    });
    t.mock.method(internal, "withLock", <T>(key: string, action: () => Promise<T>, signal?: AbortSignal): Promise<T> => {
      const result = withLock(key, action, signal);
      if (key === bare && holderChain && active && imported && !queued && !released) {
        queued = true;
        assert.notEqual(locks.get(bare), holderChain, "ownership snapshot joins the real held chain");
        let settled = false;
        void result.then(() => { settled = true; }, () => { settled = true; });
        queueMicrotask(() => {
          if (expiry === "queued readback") clock.advance(10_000);
          // Observe the waiter, not its implementation scope. An unfixed
          // waiter remains queued after soft expiry until the original hard stop.
          queueMicrotask(() => { if (!settled) clock.advance(20_000); });
        });
      }
      return result;
    });
    t.mock.method(client, "publishCheckpoint", async (_id: string, tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
      tips.push(tip); await drain(pack);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/3304" } };
    });
    const executor: Executor = {
      get safety() { return rig?.safety; },
      run: async ctx => {
        rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]), now: clock.now, armDeadline: clock.armDeadline });
        runId = ctx.runId; branch = ctx.branch;
        commitInTree(ctx.worktreePath, "post-import.txt", "same retained H\n");
        head = (await git.branchTip(ctx.worktreePath, branch))!;
        active = true;
        try { await ctx.checkpoint!({ reap: true, sink: "milestone_checkpoint", progress: { completed: ["3304"], in_progress: [] } }); }
        catch (error) { failure = error; }
        returned = !released;
        stopped.resolve();
        await retry.promise;
        active = false;
        if (!failure) await ctx.checkpoint!({ reap: false });
        return { branch };
      },
    };
    const { github } = fakeGitHub();
    client.protocolFeatures = ["run_checkpoint_durability"];
    const subject = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
      github, checkpointIntervalMs: 1, now: clock.now, setTickTimer: () => () => {},
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm,
        afterPinnedPublish: state => publications.push(state), afterUnpinnedPublish: state => publications.push(state) },
    });
    const reporter = subject as unknown as {
      reportPublishOutcome(flight: unknown, key: string, text: string, fields?: Record<string, unknown>): void;
    };
    const report = reporter.reportPublishOutcome.bind(subject);
    t.mock.method(reporter, "reportPublishOutcome", (...args: Parameters<typeof report>) => {
      outcomes.push({ key: args[1], step: args[3]?.step });
      return report(...args);
    });
    const execution = subject.execute(gitlabClaim(330401, { repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" } }));
    let holder: Promise<void> | undefined, follower: Promise<void> | undefined;
    let overtook = false;
    try {
      await waitForTestEvent(ready.promise, "successful real import");
      holder = git.withBareLock(bare, async () => { acquired.resolve(); await held.promise; });
      holderChain = locks.get(bare);
      await waitForTestEvent(stopped.promise, "checkpoint settles after controlled deadline");
      active = false;
      follower = git.withBareLock(bare, async () => { overtook = !released; });
      assert.equal(failure instanceof Error ? (failure as Error & { diagnostic?: string }).diagnostic ?? failure.message : failure,
        undefined, "checkpoint must soft-stop without a boundary failure");
      assert.equal(returned, true);
      assert.equal(rig!.registry.isPoisoned(), false);
      assert.equal(queued, expiry === "queued readback");
      assert.deepEqual(outcomes, [{ key: "soft_deadline", step: expiry === "queued readback"
        ? "checkpoint_lock_wait" : "checkpoint_import_readback" }], "soft expiry reports its exact active phase");
      assert.deepEqual(tips, []);
      assert.equal(publications.length, 0);
      assert.ok(!api.states.some(s => s.body.checkpoint_contains_latest === true));
      assert.ok(api.states.some(s => s.body.milestones_completed?.includes("3304")));
    } finally {
      released = true; held.resolve(); acquired.resolve();
      await holder; await follower; await locks.get(bare);
      retry.resolve(); await execution;
    }
    const texts = api.messages(runId).filter(m => m.kind === "status").map(m => String(m.payload.text));
    const feed = texts.findIndex(text => text.includes("retained locally"));
    const stop = texts.findIndex(text => text.includes("soft deadline"));
    assert.ok(feed >= 0 && stop > feed, `retained-head feed precedes stop: ${JSON.stringify(texts)}`);
    assert.equal(overtook, false);
    assert.deepEqual(tips, [head]);
    assert.equal(publications.length, 1);
    assert.equal(publications[0]!.lastPublishedTip, head);
    assert.equal(publications[0]!.checkpointFloor, head);
    assert.equal((await git.committedTrackingOwnership(bare, branch, runId, head)).kind, "owned");
  });
}

it("3304 milestone with no new work still observes post-import soft expiry", async t => {
  const clock = virtualBoundaryClock();
  t.mock.method(Date, "now", clock.now);
  const ownership = git.committedTrackingOwnership.bind(git);
  let expire = false, published = 0, observed = false;
  let rig: ReturnType<typeof codexRig> | undefined;
  t.mock.method(git, "committedTrackingOwnership", (...args: Parameters<typeof git.committedTrackingOwnership>) => {
    const scope = (git as unknown as { boundaryProcesses: { getStore(): { softSignal?: AbortSignal } | undefined } }).boundaryProcesses.getStore();
    if (expire && scope?.softSignal) { expire = false; clock.advance(10_000); }
    return ownership(...args);
  });
  t.mock.method(client, "publishCheckpoint", async (_id: string, _tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
    published++; await drain(pack); return { ok: true, body: { published: true } };
  });
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]), now: clock.now, armDeadline: clock.armDeadline });
      commitInTree(ctx.worktreePath, "no-new-work.txt", "published once\n");
      clock.advance(1); // Open the initial time gate before establishing lastPublishedTip.
      await ctx.checkpoint!({ reap: false });
      assert.equal(published, 1);
      expire = true;
      await ctx.checkpoint!({ reap: true, sink: "milestone_checkpoint", progress: { completed: ["idle"], in_progress: [] } });
      assert.equal(expire, false);
      assert.equal(rig.registry.isPoisoned(), false);
      assert.equal(published, 1);
      observed = true;
      return { branch: ctx.branch };
    },
  };
  const claim = gitlabClaim(330402);
  await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
    checkpointIntervalMs: 1, now: clock.now, setTickTimer: () => () => {},
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  }).execute(claim);
  assert.equal(observed, true);
  assert.ok(api.messages(claim.run_id).some(m => String(m.payload.text).includes("checkpoint publish skipped: soft deadline")));
});

it("3304 acquisition diagnostics distinguish queued locks from reads in each checkpoint phase", async t => {
  const clock = virtualBoundaryClock();
  const bare = git.barePathFor(fx.originPath);
  const branch = "feature/acquisition-diagnostics";
  const origin = (...args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args], { encoding: "utf8" }).trim();
  origin("checkout", "-b", branch);
  commitInTree(fx.originPath, "diagnostics.txt", "published floor\n");
  origin("checkout", "main");
  const transitions: string[] = [], acquiredPhases: string[] = [], readPhases: string[] = [], adoptionResults: unknown[] = [];
  let tracing = false, active: BoundaryStep | undefined, owningPhase: BoundaryStep | undefined;
  let rig: ReturnType<typeof codexRig> | undefined;
  const record = (step: (name: BoundaryStep) => void) => (name: BoundaryStep) => {
    active = name; transitions.push(name); step(name);
  };
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]), now: clock.now, armDeadline: clock.armDeadline });
      clock.advance(1);
      // Rewrite the published commit so the checkpoint adopts a real bridge.
      fs.writeFileSync(path.join(ctx.worktreePath, "diagnostics.txt"), "rewritten floor\n");
      const command = (...args: string[]) => execFileSync("git", ["-C", ctx.worktreePath, ...args], { encoding: "utf8" }).trim();
      command("add", "diagnostics.txt");
      command("-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
        "-c", "commit.gpgsign=false", "commit", "--amend", "--no-edit");
      tracing = true;
      try {
        await ctx.checkpoint!({ reap: true, sink: "milestone_checkpoint", progress: { completed: ["diagnostics"], in_progress: [] } });
      } finally { tracing = false; }
      return { branch: ctx.branch };
    },
  };
  t.mock.method(client, "publishCheckpoint", async (_id: string, _tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
    await drain(pack); return { ok: true, body: { published: true } };
  });
  const subject = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
    checkpointIntervalMs: 1, now: clock.now, setTickTimer: () => () => {},
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  });
  const runnerInternal = subject as unknown as {
    checkedTrackingSnapshot(flight: never, barePath: string, branch: string, sha?: string,
      step?: (name: BoundaryStep) => void, phase?: BoundaryStep): Promise<string | null>;
  };
  const snapshot = runnerInternal.checkedTrackingSnapshot.bind(subject);
  t.mock.method(runnerInternal, "checkedTrackingSnapshot", async (...args: Parameters<typeof snapshot>) => {
    if (!tracing || !args[4]) return snapshot(...args);
    owningPhase = args[5];
    active = owningPhase;
    assert.ok(owningPhase, "the runner supplies the owning phase");
    try { return await snapshot(args[0], args[1], args[2], args[3], record(args[4]), args[5]); }
    finally { owningPhase = undefined; }
  });
  const update = git.updateTrackingRef.bind(git);
  t.mock.method(git, "updateTrackingRef", async (...args: Parameters<typeof update>) => {
    if (!tracing) return update(...args);
    assert.ok(args[3].checkpointStep, "bridge adoption supplies acquisition tracing");
    owningPhase = active = "checkpoint_bridge";
    try {
      const result = await update(args[0], args[1], args[2], { ...args[3], checkpointStep: record(args[3].checkpointStep) });
      adoptionResults.push(result.kind === "updated" ? result.kind : result);
      return result;
    } catch (error) { adoptionResults.push(String(error)); throw error; }
    finally { owningPhase = undefined; }
  });
  const gitInternal = git as unknown as {
    withLock<T>(key: string, action: () => Promise<T>, signal?: AbortSignal): Promise<T>;
    observeTrackingUnderLock(barePath: string, branch: string): Promise<unknown>;
  };
  const withLock = gitInternal.withLock.bind(git);
  t.mock.method(gitInternal, "withLock", async <T>(key: string, action: () => Promise<T>, signal?: AbortSignal): Promise<T> => {
    if (!tracing || !owningPhase || key !== bare) return withLock(key, action, signal);
    const phase = owningPhase;
    assert.equal(active, "checkpoint_lock_wait", "label changes immediately before queueing");
    const release = deferred();
    // One controlled predecessor per acquisition; it cannot block beyond this hook.
    const holder = withLock(key, async () => { await release.promise; });
    const waiter = withLock(key, async () => {
      const result = action();
      assert.equal(active, phase, "phase resumes inside acquisition before the read settles");
      acquiredPhases.push(phase);
      return result;
    }, signal);
    try { assert.equal(active, "checkpoint_lock_wait", "queued waiter still reports waiting"); }
    finally { release.resolve(); }
    const [, result] = await Promise.all([holder, waiter]);
    return result;
  });
  const observe = gitInternal.observeTrackingUnderLock.bind(git);
  t.mock.method(gitInternal, "observeTrackingUnderLock", async (...args: Parameters<typeof observe>) => {
    if (tracing && owningPhase) {
      assert.equal(active, owningPhase, "Git reads report their owning phase");
      readPhases.push(owningPhase);
    }
    return observe(...args);
  });
  client.protocolFeatures = ["run_checkpoint_durability"];
  await subject.execute(gitlabClaim(330403, { kind: "task", issue_iid: null, branch, open_mr: false }));
  const phases = [
    "checkpoint_import_readback", "checkpoint_post_import", "checkpoint_bridge",
    "checkpoint_bridge", "checkpoint_bridge", "checkpoint_post_bridge", "checkpoint_overlay",
  ];
  assert.deepEqual(adoptionResults, ["updated"]);
  assert.deepEqual(acquiredPhases, phases, "import readback, fetched tip, bridge observation/adoption/readback, post-bridge, overlay context");
  assert.deepEqual(transitions, phases.flatMap(phase => ["checkpoint_lock_wait", phase]));
  assert.deepEqual(readPhases, [
    "checkpoint_import_readback", "checkpoint_post_import",
    "checkpoint_bridge", "checkpoint_bridge", "checkpoint_bridge", "checkpoint_bridge",
    "checkpoint_post_bridge", "checkpoint_overlay",
  ], "adoption also observes tracking while checking conflicting refs under the same lock");
});
