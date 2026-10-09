import assert from "node:assert/strict";
import { it } from "node:test";
import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { Worker } from "../src/worker.js";
import { loadConfig } from "../src/config.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { GitCache } from "../src/git.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { Outbox } from "../src/outbox.js";
import type { StatsCollector } from "../src/stats.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

for (const failure of ["none", "snapshot", "cleanup"] as const) {
  it(`Worker.run awaits CODE boot cleanup before all claim lanes: ${failure}`, async () => {
    const events: string[] = [];
    const abort = new AbortController();
    let release!: () => void;
    let entered!: () => void;
    const gate = new Promise<void>(r => { release = r; });
    const ready = new Promise<void>(r => { entered = r; });
    const client = {
      register: async () => { events.push("register"); return { worker_id: "worker" }; },
      hasFeature: () => false,
      heartbeat: async () => { events.push("heartbeat"); },
      claimRun: async () => { events.push("run"); return null; },
      claimChat: async () => { events.push("chat"); return null; },
      claimCrossCheck: async () => { events.push("cross"); abort.abort(); return null; },
    } as unknown as WorkerClient;
    const runner = {
      snapshotBootRecoveries: async () => [],
      resumePendingRecoveries: async () => {},
      settlePendingPredecessors: async () => {},
      protectRecoveryTerminalDeps: (deps: unknown) => { events.push("resolve-terminals"); return deps; },
    } as unknown as RunRunner;
    const config = loadConfig({ UZI_API_URL: "http://example.com", UZI_WORKER_TOKEN: "worker-token",
      WORKER_CROSS_CHECK_SLOTS: "1" });
    config.pollIntervalMs = config.chatPollMs = config.heartbeatIntervalMs = 2;
    const checker = {
      execute: async () => {},
      snapshotBootCodeSnapshots: async () => {
        events.push("snapshot");
        if (failure === "snapshot") throw new Error("discovery failed");
        return [];
      },
      cleanupBootCodeSnapshots: async (candidates: unknown[]) => {
        assert.deepEqual(candidates, []);
        events.push("cleanup-start"); entered();
        await gate;
        events.push("cleanup-end");
        if (failure === "cleanup") throw new Error("cleanup failed");
      },
    };
    const outbox = {
      isDisabled: () => false, uncleanRuns: () => [], runsWithPending: () => [],
      listPendingTerminals: () => [], listPendingFinalizes: () => [],
    } as unknown as Outbox;
    const worker = new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner,
      {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }),
      outbox, undefined, undefined, undefined, undefined, undefined, undefined,
      () => ({ collect: () => ({}) }) as unknown as StatsCollector,
      undefined, undefined, undefined, undefined, checker);
    const done = worker.run(abort.signal);
    try {
      await ready;
      assert.ok(events.indexOf("snapshot") < events.indexOf("register"));
      assert.ok(events.indexOf("heartbeat") < events.indexOf("cleanup-start"));
      assert.ok(events.indexOf("resolve-terminals") > events.indexOf("register"));
      assert.ok(events.indexOf("resolve-terminals") < events.indexOf("cleanup-start"));
      assert.ok(!events.some(e => ["run", "chat", "cross"].includes(e)));
    } finally { release(); await done; }
    for (const lane of ["run", "chat", "cross"])
      assert.ok(events.indexOf(lane) > events.indexOf("cleanup-end"));
  });
}

for (const mode of ["deadline", "shutdown"] as const) {
  it(`CODE boot discovery ${mode} settles behind a held lock without releasing serialization`, async (t) => {
    const fx = makeFixture({ "source.txt": "code\n" });
    const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const abort = new AbortController();
    let release!: () => void;
    let acquired!: () => void;
    const ready = new Promise<void>(resolve => { acquired = resolve; });
    const held = new Promise<void>(resolve => { release = resolve; });
    const events: string[] = [];
    let fallback: NodeJS.Timeout | undefined;
    let shutdown: NodeJS.Timeout | undefined;
    let holder: Promise<void> | undefined;
    try {
      const bare = await git.ensureClone(fx.originPath, "");
      const head = execFileSync("git", ["-C", bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      await git.pinCodeSnapshot(bare, "lead", head, head, true, 1);
      execFileSync("git", ["-C", bare, "update-ref", "refs/uzi-cross-check/unrelated", head]);
      const refs = () => execFileSync("git", ["-C", bare, "show-ref"], { encoding: "utf8" });
      const before = refs();
      holder = git.withBareLock(bare, async () => { acquired(); await held; events.push("released"); });
      await ready;
      // The fallback completes an unbounded implementation, yielding an assertion failure rather than a test timeout.
      fallback = setTimeout(release, 250);
      const discover = git.discoverCodeSnapshots.bind(git);
      t.mock.method(git, "discoverCodeSnapshots", async (signal?: AbortSignal) => {
        events.push("discover");
        if (mode === "shutdown") shutdown = setTimeout(() => abort.abort(), 10);
        return discover(signal, mode === "deadline" ? 10 : 30_000);
      });
      const deletion = t.mock.method(git, "deleteCodeSnapshot", async () => { events.push("deleted"); });
      let candidates: unknown;
      const client = {
        register: async () => { events.push("register"); return { worker_id: "worker" }; },
        hasFeature: () => false, heartbeat: async () => {},
        claimRun: async () => { abort.abort(); return null; },
        claimChat: async () => null, claimCrossCheck: async () => null,
      } as unknown as WorkerClient;
      const checker = new CrossCheckRunner(client, git, nullLogger(), { model: { run: async () => "" } });
      const cleanup = checker.cleanupBootCodeSnapshots.bind(checker);
      t.mock.method(checker, "cleanupBootCodeSnapshots", async (value: Parameters<typeof cleanup>[0], signal: AbortSignal) => {
        candidates = value;
        await cleanup(value, signal);
      });
      const runner = { snapshotBootRecoveries: async () => [], resumePendingRecoveries: async () => {},
        settlePendingPredecessors: async () => {}, protectRecoveryTerminalDeps: (deps: unknown) => deps,
      } as unknown as RunRunner;
      const config = loadConfig({ UZI_API_URL: "http://example.com", UZI_WORKER_TOKEN: "worker-token" });
      config.pollIntervalMs = config.chatPollMs = config.heartbeatIntervalMs = 2;
      const outbox = { isDisabled: () => false, uncleanRuns: () => [], runsWithPending: () => [],
        listPendingTerminals: () => [], listPendingFinalizes: () => [], } as unknown as Outbox;
      const worker = new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner,
        {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }), outbox,
        undefined, undefined, undefined, undefined, undefined, undefined,
        () => ({ collect: () => ({}) }) as unknown as StatsCollector,
        undefined, undefined, undefined, undefined, checker);
      await worker.run(abort.signal);
      assert.ok(!events.includes("released"), "worker must settle discovery before the held lock releases");
      if (mode === "deadline") {
        assert.ok(events.includes("register"), "deadline must allow registration to continue");
        assert.deepEqual(candidates, [], "cancelled discovery cannot authorize cleanup");
      }
      clearTimeout(fallback);
      let followerRan = false;
      const follower = git.withBareLock(bare, async () => { followerRan = true; });
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(followerRan, false, "cancelled waiter cannot let a follower overtake the holder");
      release();
      await holder;
      await follower;
      assert.equal(followerRan, true);
      assert.equal(deletion.mock.callCount(), 0, "no deferred continuation may delete refs");
      assert.equal(refs(), before, "discovery cannot delete, rebind or mutate unrelated refs");
    } finally {
      clearTimeout(fallback); clearTimeout(shutdown); release?.(); await holder; fx.cleanup();
    }
  });
}

it("real Git CODE boot sweep deletes terminal/foreign CLOSED refs and retains uncertain observations", async () => {
  const fx = makeFixture({ "source.txt": "code\n" });
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  try {
    const bare = await git.ensureClone(fx.originPath, "");
    const head = execFileSync("git", ["-C", bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    const observations = new Map<string, Record<string, unknown>>();
    const leads = new Map<string, string>();
    for (const control of ["terminal", "foreign", "live", "pending", "open", "unknown", "moved", "old-api", "no-child", "generation"]) {
      const lead = randomUUID(), child = randomUUID();
      leads.set(control, lead);
      await git.pinCodeSnapshot(bare, lead, head, head, true, 7);
      if (control !== "no-child") {
        const checkout = await git.runnerCloneAtCommit(bare, head, child, lead, 2);
        await git.removeRunnerClone(checkout);
        if (control !== "open") await git.closeCodeSnapshotReader(bare, lead, checkout,
          { childId: child, childGeneration: 2, terminalStatus: "completed", cleanupConfirmed: true });
      }
      observations.set(lead, { protocol: "code_snapshot_cleanup_v1", lead_run_id: lead, head_commit: head,
        outcome: control === "pending" ? "pending" : "completed",
        lead_status: control === "foreign" || control === "live" ? "running" : "completed",
        owned_by_worker: control !== "foreign", checker_run_id: child,
        checker_claim_generation: control === "generation" ? 3 : 2, checker_status: "completed" });
    }
    execFileSync("git", ["-C", bare, "update-ref", "refs/uzi-cross-check/unrelated", head]);
    let calls = 0;
    const client = { getCodeSnapshotCleanup: async (lead: string) => {
      calls++;
      if (lead === leads.get("unknown")) throw new Error("unknown");
      if (lead === leads.get("old-api")) return { status: "completed" };
      return observations.get(lead);
    } } as unknown as WorkerClient;
    const checker = new CrossCheckRunner(client, git, nullLogger(), {
      model: { run: async () => { assert.fail("boot cannot invoke a model"); } },
    });
    const candidates = await checker.snapshotBootCodeSnapshots();
    execFileSync("git", ["-C", bare, "update-ref", `refs/uzi-cross-check/${leads.get("moved")}`,
      execFileSync("git", ["-C", bare, "commit-tree", head + "^{tree}"], {
        input: "moved\n", encoding: "utf8", env: { ...process.env, GIT_AUTHOR_NAME: "test",
          GIT_AUTHOR_EMAIL: "test@example.test", GIT_COMMITTER_NAME: "test", GIT_COMMITTER_EMAIL: "test@example.test" },
      }).trim()]);
    await checker.cleanupBootCodeSnapshots(candidates, new AbortController().signal);
    for (const control of ["terminal", "foreign"])
      await assert.rejects(git.codeSnapshotTip(bare, leads.get(control)!));
    for (const control of ["live", "pending", "open", "unknown", "old-api", "no-child", "generation"])
      assert.equal(await git.codeSnapshotTip(bare, leads.get(control)!), head, control);
    assert.notEqual(await git.codeSnapshotTip(bare, leads.get("moved")!), head);
    assert.equal(await git.codeSnapshotTip(bare, "unrelated"), head);
    const before = calls;
    await checker.cleanupBootCodeSnapshots([], new AbortController().signal);
    assert.equal(calls, before, "empty boot makes zero API calls");
  } finally { fx.cleanup(); }
});
