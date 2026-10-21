import assert from "node:assert/strict";
import { it } from "node:test";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { Worker } from "../src/worker.js";
import { loadConfig } from "../src/config.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { GitCache } from "../src/git.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import { decodeCodeSnapshotCleanup } from "../src/code-cross-check-contract.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { Outbox } from "../src/outbox.js";
import type { StatsCollector } from "../src/stats.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// The boot sweep deletes snapshot refs through rmTreePinned (src/rmtree.ts), which refuses off Linux (#1912).
const LINUX_ONLY = process.platform !== "linux"
  ? "needs the Linux-only descriptor-pinned tree removal (src/rmtree.ts rmTreePinned refuses off Linux)"
  : false;

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

// Fenced boot sweep (PRD #2170 U2). Every ref below is pinned at lead claim generation 7. A ref is
// deleted only on positive server proof that matches the snapshot's exact lead id, lead claim
// generation and head; each retained control names the one fence that must hold.
type Evidence = Record<string, unknown> | "throw" | "http-500" | "old-api" | "malformed";
const PIN = 7;
const swept = ["terminal", "foreign", "never-claimed", "decided-no-child", "terminal-child-no-local", "rowless-terminal", "rowless-foreign"];
const retained = ["live", "rowless-live", "pending-no-reader", "running-child-no-local", "open", "throw", "http-500", "old-api",
  "malformed", "moved", "child-generation", "child-id", "lead-generation", "head", "lead-id", "local-child-server-none",
  "decided-pending-outcome", "missing-metadata", "malformed-metadata", "rowless-lead-id", "rowless-child-terminal", "rowless-child-foreign"];

it("real Git CODE boot sweep deletes only on fenced positive proof and retains everything else", { skip: LINUX_ONLY }, async () => {
  const fx = makeFixture({ "source.txt": "code\n" });
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  try {
    const bare = await git.ensureClone(fx.originPath, "");
    const head = execFileSync("git", ["-C", bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    const otherHead = "c".repeat(40);
    const evidence = new Map<string, Evidence>();
    const leads = new Map<string, string>();
    const metadataFile = (lead: string) => path.join(bare, "uzi-code-snapshots", `code-snapshot-${lead}.json`);
    // Every control starts as a CLOSED reader plus a decided, terminal-checker row.
    const row = (lead: string, child: string, over: Record<string, unknown> = {}) => ({
      protocol: "code_snapshot_cleanup_v1", lead_run_id: lead, lead_status: "completed", owned_by_worker: true,
      row_present: true, lead_claim_generation: PIN, head_commit: head, outcome: "completed",
      child_present: true, checker_run_id: child, checker_claim_generation: 2, checker_status: "completed", ...over });
    const noRow = (lead: string, over: Record<string, unknown> = {}) => ({
      protocol: "code_snapshot_cleanup_v1", lead_run_id: lead, lead_status: "completed", owned_by_worker: true,
      row_present: false, lead_claim_generation: 0, head_commit: null, outcome: null,
      child_present: false, checker_run_id: null, checker_claim_generation: 0, checker_status: null, ...over });
    const noChildRef = new Set(["never-claimed", "decided-no-child", "terminal-child-no-local", "rowless-terminal", "rowless-foreign",
      "rowless-live", "rowless-lead-id", "pending-no-reader", "running-child-no-local", "decided-pending-outcome"]);
    for (const control of [...swept, ...retained]) {
      const lead = randomUUID(), child = randomUUID();
      leads.set(control, lead);
      await git.pinCodeSnapshot(bare, lead, head, head, true, PIN);
      if (!noChildRef.has(control)) {
        const checkout = await git.runnerCloneAtCommit(bare, head, child, lead, 2);
        await git.removeRunnerClone(checkout);
        if (control !== "open") await git.closeCodeSnapshotReader(bare, lead, checkout,
          { childId: child, childGeneration: 2, terminalStatus: "completed", cleanupConfirmed: true });
      }
      const foreign = { lead_status: "running", owned_by_worker: false };
      const live = { lead_status: "running", owned_by_worker: true };
      const ev: Record<string, Evidence> = {
        terminal: row(lead, child),
        foreign: row(lead, child, foreign),
        "never-claimed": row(lead, child, { checker_claim_generation: 0, checker_status: "queued" }),
        "decided-no-child": row(lead, child, { child_present: false, checker_run_id: null, checker_claim_generation: 0, checker_status: null }),
        "terminal-child-no-local": row(lead, child, { checker_status: "cancelled" }),
        "rowless-terminal": noRow(lead),
        "rowless-foreign": noRow(lead, foreign),
        "rowless-lead-id": noRow(lead, { lead_run_id: randomUUID() }),
        "rowless-child-terminal": noRow(lead),
        "rowless-child-foreign": noRow(lead, foreign),
        live: row(lead, child, live),
        "rowless-live": noRow(lead, live),
        "pending-no-reader": row(lead, child, { outcome: "pending", checker_claim_generation: 0, checker_status: "queued" }),
        "running-child-no-local": row(lead, child, { checker_claim_generation: 2, checker_status: "running" }),
        open: row(lead, child),
        throw: "throw", "http-500": "http-500", "old-api": "old-api", malformed: "malformed",
        moved: row(lead, child),
        "child-generation": row(lead, child, { checker_claim_generation: 3 }),
        "child-id": row(lead, randomUUID()),
        "lead-generation": row(lead, child, { lead_claim_generation: PIN + 1 }),
        head: row(lead, child, { head_commit: otherHead }),
        "lead-id": row(lead, child, { lead_run_id: randomUUID() }),
        "local-child-server-none": row(lead, child, { child_present: false, checker_run_id: null, checker_claim_generation: 0, checker_status: null }),
        "decided-pending-outcome": row(lead, child, { outcome: "pending" }),
        "missing-metadata": row(lead, child),
        "malformed-metadata": row(lead, child),
      };
      evidence.set(lead, ev[control]!);
    }
    execFileSync("git", ["-C", bare, "update-ref", "refs/uzi-cross-check/unrelated", head]);
    const asked: string[] = [];
    const client = { getCodeSnapshotCleanup: async (lead: string) => {
      asked.push(lead);
      const ev = evidence.get(lead);
      if (ev === "throw") throw new Error("unavailable");
      if (ev === "http-500") throw new RequestError("GET", "/ownership", 500, "boom");
      if (ev === "old-api") return { status: "completed" };
      if (ev === "malformed") return { ...(row(lead, randomUUID()) as object), extra: true };
      return ev;
    } } as unknown as WorkerClient;
    const checker = new CrossCheckRunner(client, git, nullLogger(), {
      model: { run: async () => { assert.fail("boot cannot invoke a model"); } },
    });
    // Production decodes in the client; mirror that here so mock evidence cannot skip strict validation.
    const raw = client.getCodeSnapshotCleanup.bind(client);
    client.getCodeSnapshotCleanup = async (lead: string, signal?: AbortSignal) => decodeCodeSnapshotCleanup(await raw(lead, signal));
    // Corrupt metadata before discovery: it must be excluded and its ref retained.
    fs.rmSync(metadataFile(leads.get("missing-metadata")!));
    fs.writeFileSync(metadataFile(leads.get("malformed-metadata")!), "{not json");
    const candidates = await checker.snapshotBootCodeSnapshots();
    execFileSync("git", ["-C", bare, "update-ref", `refs/uzi-cross-check/${leads.get("moved")}`,
      execFileSync("git", ["-C", bare, "commit-tree", head + "^{tree}"], {
        input: "moved\n", encoding: "utf8", env: { ...process.env, GIT_AUTHOR_NAME: "test",
          GIT_AUTHOR_EMAIL: "test@example.test", GIT_COMMITTER_NAME: "test", GIT_COMMITTER_EMAIL: "test@example.test" },
      }).trim()]);
    await checker.cleanupBootCodeSnapshots(candidates, new AbortController().signal);
    for (const control of swept)
      await assert.rejects(git.codeSnapshotTip(bare, leads.get(control)!), (): boolean => true, `${control} must be swept`);
    for (const control of retained) {
      const tip = execFileSync("git", ["-C", bare, "rev-parse", `refs/uzi-cross-check/${leads.get(control)!}`], { encoding: "utf8" }).trim();
      assert.equal(tip === head, control !== "moved", `${control} must be retained`);
    }
    assert.equal(await git.codeSnapshotTip(bare, "unrelated"), head);
    for (const quiet of ["open", "missing-metadata", "malformed-metadata"])
      assert.ok(!asked.includes(leads.get(quiet)!), `${quiet} must not even reach the server`);
    const before = asked.length;
    await checker.cleanupBootCodeSnapshots([], new AbortController().signal);
    assert.equal(asked.length, before, "empty boot makes zero API calls");
  } finally { fx.cleanup(); }
});

it("boot sweep keeps the ref when the server's lead claim generation differs from the pin, even if every other field matches", { skip: LINUX_ONLY }, async () => {
  const fx = makeFixture({ "source.txt": "code\n" });
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  try {
    const bare = await git.ensureClone(fx.originPath, "");
    const head = execFileSync("git", ["-C", bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    const lead = randomUUID(), child = randomUUID();
    await git.pinCodeSnapshot(bare, lead, head, head, true, 4);
    const checkout = await git.runnerCloneAtCommit(bare, head, child, lead, 2);
    await git.removeRunnerClone(checkout);
    await git.closeCodeSnapshotReader(bare, lead, checkout, { childId: child, childGeneration: 2, terminalStatus: "completed", cleanupConfirmed: true });
    let generation = 5;
    const client = { getCodeSnapshotCleanup: async () => ({ protocol: "code_snapshot_cleanup_v1", lead_run_id: lead,
      lead_status: "completed", owned_by_worker: true, row_present: true, lead_claim_generation: generation,
      head_commit: head, outcome: "completed", child_present: true, checker_run_id: child,
      checker_claim_generation: 2, checker_status: "completed" }) } as unknown as WorkerClient;
    const checker = new CrossCheckRunner(client, git, nullLogger(), { model: { run: async () => "" } });
    const sweep = async () => checker.cleanupBootCodeSnapshots(await checker.snapshotBootCodeSnapshots(), new AbortController().signal);
    await sweep();
    assert.equal(await git.codeSnapshotTip(bare, lead), head, "a server generation that is not the pin generation keeps the ref");
    generation = 4;
    await sweep();
    await assert.rejects(git.codeSnapshotTip(bare, lead), (): boolean => true, "the matching generation sweeps it");
  } finally { fx.cleanup(); }
});
