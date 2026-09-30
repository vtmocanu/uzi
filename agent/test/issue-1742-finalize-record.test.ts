import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { isFinalizeBoundResult, type RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";

// Issue #1742 M2: the worker-local finalize-pending record (outbox), its register hand-off, and the
// runner write and retirement sites.
installHarness();

const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fsp.rm(root, { recursive: true, force: true });
});

function tmpRoot(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "issue-1742-finalize-"));
  roots.push(dir);
  return path.join(dir, "outbox");
}

function makeOutbox(root: string, opts: { rawWrite?: RawWriteSeam; log?: ReturnType<typeof nullLogger> } = {}): Outbox {
  return new Outbox({
    root,
    log: opts.log ?? nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
    ...(opts.rawWrite ? { rawWrite: opts.rawWrite } : {}),
  });
}

async function dirFiles(root: string, runId: string, re: RegExp): Promise<string[]> {
  const names = await fsp.readdir(path.join(root, runId)).catch((err: NodeJS.ErrnoException) => {
    if (err.code === "ENOENT") return [];
    throw err;
  });
  return names.filter((n) => re.test(n)).sort();
}
const finalizeFiles = (root: string, runId: string) => dirFiles(root, runId, /^finalize-[0-9]+[.]json$/);

const RUN_A = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN_B = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";

interface Line { level: string; msg: string; [k: string]: unknown }
const msgs = (lines: unknown[]) => (lines as Line[]).map((l) => l.msg);

async function registerWith(
  outbox: Outbox,
  register: (snapshot: ActiveSnapshot | undefined) => Promise<unknown>,
  withRegistry = true,
): Promise<void> {
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
  const client = {
    register: async (_n: string, _t?: string, _m?: number, _c?: string[], _p?: string[], snapshot?: ActiveSnapshot) =>
      register(snapshot),
  } as unknown as WorkerClient;
  const worker = new Worker(
    { workerName: "finalize-probe", workerTemplate: "base", maxConcurrentRuns: 1, pollIntervalMs: 1 } as Config,
    client,
    {} as RunRunner,
    {} as ChatRunner,
    {} as JudgeRunner,
    {} as ReviewRunner,
    nullLogger(),
    () => ({ ok: true, missing: [] }),
    outbox,
    new Map(),
    withRegistry ? registry : undefined,
  );
  await (worker as unknown as { registerWithRetry(signal: AbortSignal): Promise<void> })
    .registerWithRetry(new AbortController().signal);
}

describe("issue #1742 finalize record: Outbox", () => {
  it("logs `finalize record durable` only after the record is linked and the directory is fsynced", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const events: string[] = [];
    let durableSeenAt: string | undefined;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "finalize") {
        assert.equal(msgs(lines).includes("finalize record durable"), false, "no durable line before the write");
        events.push(`write:${path.basename(ctx.path)}`);
      }
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite, log: logger });
    await outbox.init();
    // Observe the directory fsync through the private method the install path calls.
    const target = outbox as unknown as { fsyncDir(dir: string): Promise<void> };
    const realFsync = target.fsyncDir.bind(outbox);
    target.fsyncDir = async (dir: string) => {
      await realFsync(dir);
      events.push(`fsyncDir:${path.basename(dir)}`);
      if (msgs(lines).includes("finalize record durable")) durableSeenAt = `fsyncDir:${path.basename(dir)}`;
    };
    const res = await outbox.journalFinalize(RUN_A, 3);
    assert.deepEqual(res, { written: true });
    const durable = (lines as Line[]).find((l) => l.msg === "finalize record durable");
    assert.ok(durable, "the durable line was logged");
    assert.equal(durable.level, "info");
    assert.equal(durable.run_id, RUN_A);
    assert.equal(durable.claim_generation, 3);
    assert.equal(durableSeenAt, undefined, "the durable line was not logged during any directory fsync");
    assert.ok(events.includes(`fsyncDir:${RUN_A}`), "the run directory was fsynced");
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"]);
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN_A, claim_generation: 3 }]);
  });

  it("logs `finalize record not written` and never the durable line when the write fails", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "finalize" && ctx.path.endsWith(".tmp")) throw new Error("injected write failure");
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite, log: logger });
    await outbox.init();
    const res = await outbox.journalFinalize(RUN_A, 3);
    assert.equal(res.written, false);
    assert.match(res.reason ?? "", /injected write failure/);
    assert.equal(msgs(lines).includes("finalize record durable"), false);
    const warn = (lines as Line[]).find((l) => l.msg === "finalize record not written");
    assert.ok(warn, "the not-written line was logged");
    assert.equal(warn.level, "warn");
    assert.equal(warn.run_id, RUN_A);
    assert.deepEqual(await finalizeFiles(root, RUN_A), [], "no record file and no leftover temp");
    assert.equal((await fsp.readdir(path.join(root, RUN_A))).some((n) => n.endsWith(".tmp")), false);
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("reports not-written (no throw) when the outbox failed closed", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const outbox = makeOutbox(root, { log: logger });
    // never init(): no key, so the store is unusable
    const res = await outbox.journalFinalize(RUN_A, 3);
    assert.deepEqual(res, { written: false, reason: "outbox_disabled" });
    assert.ok(msgs(lines).includes("finalize record not written"));
    assert.equal(msgs(lines).includes("finalize record durable"), false);
  });

  it("is idempotent: a second write for the same generation adopts the first record", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    assert.equal((await outbox.journalFinalize(RUN_A, 3)).written, true);
    assert.equal((await outbox.journalFinalize(RUN_A, 3)).written, true);
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"]);
    assert.equal(outbox.listPendingFinalizes().length, 1);
  });

  it("body carries only run_id, claim_generation, since and a MAC", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    const parsed = JSON.parse(await fsp.readFile(path.join(root, RUN_A, "finalize-3.json"), "utf8")) as Record<string, unknown>;
    assert.deepEqual(Object.keys(parsed).sort(), ["claim_generation", "mac", "run_id", "since"]);
  });

  it("reloads an authenticated record after restart; retiring it removes the file", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), [{ run_id: RUN_A, claim_generation: 3 }]);
    await restarted.retireFinalize(RUN_A, 3);
    assert.deepEqual(restarted.listPendingFinalizes(), []);
    assert.deepEqual(await finalizeFiles(root, RUN_A), []);
  });

  it("does not load a record with a bad MAC, and leaves it on disk with a warning", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    const file = path.join(root, RUN_A, "finalize-3.json");
    const parsed = JSON.parse(await fsp.readFile(file, "utf8")) as Record<string, unknown>;
    parsed.mac = "0".repeat(64);
    await fsp.writeFile(file, JSON.stringify(parsed), { mode: 0o600 });
    const { logger, lines } = recordingLogger();
    const restarted = makeOutbox(root, { log: logger });
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), []);
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"], "left on disk");
    assert.ok((lines as Line[]).some((l) => l.level === "warn" && String(l.msg).includes("finalize record ignored")));
  });

  it("does not load a record misfiled under another generation", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    await fsp.copyFile(path.join(root, RUN_A, "finalize-3.json"), path.join(root, RUN_A, "finalize-4.json"));
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), [{ run_id: RUN_A, claim_generation: 3 }]);
  });

  it("does not list a run that also holds a pending terminal journal", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    await outbox.journalFinalize(RUN_B, 1);
    await outbox.journalTerminal(RUN_A, 3, "running", 0, { status: "completed" });
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN_B, claim_generation: 1 }]);
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), [{ run_id: RUN_B, claim_generation: 1 }]);
  });

  it("retention never reclaims a run dir holding a finalize record", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    const far = Date.now() + 30 * 86_400_000;
    await outbox.sweepRetention(far);
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"]);
    await outbox.retireFinalize(RUN_A, 3);
    await outbox.sweepRetention(far);
    assert.equal(fs.existsSync(path.join(root, RUN_A)), false, "reclaimed once the record is retired");
  });
});

describe("issue #1742 finalize record: register hand-off", () => {
  it("retires exactly the offered set after an accepted register; a later record is kept", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    let sent: ActiveSnapshot | undefined;
    await registerWith(outbox, async (snapshot) => {
      sent = snapshot;
      // A live flight writes a record while the register is in flight: not part of the offered set.
      await outbox.journalFinalize(RUN_B, 5);
      return {};
    });
    assert.deepEqual(sent?.finalize_resume, [{ run_id: RUN_A, claim_generation: 3 }]);
    assert.deepEqual(await finalizeFiles(root, RUN_A), []);
    assert.deepEqual(await finalizeFiles(root, RUN_B), ["finalize-5.json"]);
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN_B, claim_generation: 5 }]);
  });

  it("retires nothing when the register fails, and re-sends the same offered set", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    let calls = 0;
    const seen: Array<ActiveSnapshot | undefined> = [];
    await registerWith(outbox, async (snapshot) => {
      calls++;
      seen.push(snapshot);
      if (calls === 1) throw new RequestError("POST", "/api/workers/register", 500, "boom");
      assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"], "the failed register retired nothing");
      return {};
    });
    assert.equal(calls, 2);
    assert.deepEqual(seen[0]?.finalize_resume, seen[1]?.finalize_resume);
    assert.equal(seen[0]?.snapshot_epoch, seen[1]?.snapshot_epoch, "the snapshot is built once");
    assert.deepEqual(await finalizeFiles(root, RUN_A), []);
  });

  it("retires nothing on a permanent auth rejection", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    await assert.rejects(
      registerWith(outbox, async () => {
        throw new RequestError("POST", "/api/workers/register", 401, "no");
      }),
    );
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"]);
  });

  it("sends no snapshot and retires nothing without a registry", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    let sent: ActiveSnapshot | undefined = { snapshot_epoch: 99, active: [], pending_overflow: false };
    await registerWith(outbox, async (snapshot) => { sent = snapshot; return {}; }, false);
    assert.equal(sent, undefined);
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"], "an unoffered record is never retired");
  });

  it("omits finalize_resume when nothing is pending, and never carries it on a heartbeat or claim build", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    let sent: ActiveSnapshot | undefined = { snapshot_epoch: 99, active: [], pending_overflow: false };
    await registerWith(outbox, async (snapshot) => { sent = snapshot; return {}; });
    assert.equal(sent, undefined, "no terminals and no finalizes: no register snapshot");

    await outbox.journalFinalize(RUN_A, 3);
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
    registry.add(RUN_B, 2);
    assert.equal("finalize_resume" in registry.build(), false, "build() (heartbeat/claim) never carries it");
    assert.equal("finalize_resume" in registry.buildRegisterSnapshot(), false, "an empty offered set omits the key");
    const withList = registry.buildRegisterSnapshot(outbox.listPendingFinalizes());
    assert.deepEqual(withList.finalize_resume, [{ run_id: RUN_A, claim_generation: 3 }]);
    assert.equal("finalize_resume" in registry.build(), false);
  });
});

describe("issue #1742 finalize record: isFinalizeBoundResult", () => {
  const base: ExecutorResult = { text: "", sessionId: "s" } as unknown as ExecutorResult;
  it("is true for an ordinary result", () => {
    assert.equal(isFinalizeBoundResult(base), true);
  });
  for (const [field, value] of [
    ["pausedAt", { completedCount: 1 }],
    ["completionHeld", { reason: "no_progress" }],
    ["walled", { reason: "wall" }],
    ["switchReleased", true],
  ] as const) {
    it(`is false when ${field} is set`, () => {
      assert.equal(isFinalizeBoundResult({ ...base, [field]: value }), false);
    });
  }
});

describe("issue #1742 finalize record: runner write and retirement", () => {
  const RUNNER_OPTS = { outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 };

  it("a normal completion writes the record, then retires it with the terminal", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const outbox = makeOutbox(root, { log: logger });
    await outbox.init();
    const claim = gitlabClaim(1742, { claim_generation: 6 });
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS }).execute(claim);
    const durable = (lines as Line[]).filter((l) => l.msg === "finalize record durable");
    assert.equal(durable.length, 1, "the record was written once");
    assert.equal(durable[0]?.run_id, claim.run_id);
    assert.equal(durable[0]?.claim_generation, 6);
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    assert.deepEqual(await finalizeFiles(root, claim.run_id), [], "retired once the terminal journal installed");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("retires it after an unjournaled terminal send when the journal could not be installed", async () => {
    const root = tmpRoot();
    let failTerminal = false;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "terminal") {
        failTerminal = true;
        const err = new Error("no space") as NodeJS.ErrnoException;
        err.code = "ENOSPC";
        throw err;
      }
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite });
    await outbox.init();
    const claim = gitlabClaim(1745, { claim_generation: 2 });
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS }).execute(claim);
    assert.equal(failTerminal, true, "the terminal journal write was refused");
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    assert.deepEqual(await finalizeFiles(root, claim.run_id), []);
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("a crash before the record is durable leaves no record and no register snapshot", async () => {
    const root = tmpRoot();
    let enteredWrite!: () => void;
    const entered = new Promise<void>((resolve) => { enteredWrite = resolve; });
    let releaseWrite!: () => void;
    const released = new Promise<void>((resolve) => { releaseWrite = resolve; });
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "finalize") {
        enteredWrite();
        await released;
      }
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite });
    await outbox.init();
    const claim = gitlabClaim(1746, { claim_generation: 3 });
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    const execution = runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS }).execute(claim);
    try {
      await Promise.race([
        entered,
        new Promise<void>((_, reject) => setTimeout(() => reject(new Error("finalize write seam was not reached")), 5_000)),
      ]);
      assert.deepEqual(await finalizeFiles(root, claim.run_id), [], "no record file at the cut");
      const restarted = makeOutbox(root);
      await restarted.init();
      assert.deepEqual(restarted.listPendingFinalizes(), []);
      let sent: ActiveSnapshot | undefined = { snapshot_epoch: 99, active: [], pending_overflow: false };
      await registerWith(restarted, async (snapshot) => { sent = snapshot; return {}; });
      assert.equal(sent, undefined, "the residual window: nothing to offer");
    } finally {
      releaseWrite();
      await execution;
    }
  });

  for (const [field, value] of [
    ["pausedAt", { completedCount: 0 }],
    ["completionHeld", { reason: "no_progress" }],
    ["walled", { reason: "wall" }],
    ["switchReleased", true],
  ] as const) {
    it(`writes no record when the executor result sets ${field}`, async () => {
      const root = tmpRoot();
      const { logger, lines } = recordingLogger();
      const outbox = makeOutbox(root, { log: logger });
      await outbox.init();
      const claim = gitlabClaim(1747, { claim_generation: 2 });
      simulateCommittedWork();
      const { gitlab } = fakeGitlab();
      const stub = new StubExecutor(nullLogger());
      const executor: Executor = {
        run: async (ctx: RunContext) => ({ ...(await stub.run(ctx)), [field]: value }),
      };
      await runner(executor, gitlab, undefined, { outbox, ...RUNNER_OPTS }).execute(claim);
      assert.equal(msgs(lines).includes("finalize record durable"), false);
      assert.deepEqual(await finalizeFiles(root, claim.run_id), []);
      assert.deepEqual(outbox.listPendingFinalizes(), []);
    });
  }

  it("keeps the record when a graceful shutdown interrupts finalize", async () => {
    const root = tmpRoot();
    let rr: RunRunner | undefined;
    let shutdownFired = false;
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "terminal" && !shutdownFired) {
        shutdownFired = true;
        rr?.shutdown();
        throw new Error("aborted by worker shutdown during finalize");
      }
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite });
    await outbox.init();
    const claim = gitlabClaim(1748, { claim_generation: 4 });
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    rr = runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS });
    await rr.execute(claim);
    assert.equal(shutdownFired, true, "the shutdown fired inside finalize");
    assert.deepEqual(await finalizeFiles(root, claim.run_id), ["finalize-4.json"], "the shutdown branch leaves the record");
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: claim.run_id, claim_generation: 4 }]);
    assert.equal(
      api.states.some((s) => s.runId === claim.run_id && (s.body.status === "completed" || s.body.status === "failed")),
      false,
      "a shutdown reports no terminal state",
    );
  });
});
