import { afterEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import { CodexCredentialDeferredError } from "../src/codex/codex-executor.js";
import { StubExecutor } from "../src/executor.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { RunRunner } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, installHarness, runner, simulateCommittedWork } from "./runner-harness.js";

// Issue #1742 M2 rework: hardening of the finalize-pending record (canonical names, generation
// validation, register-list dedupe and cap, adopt keeps `since`, link / dir-fsync failure seams),
// the runner's retirement sites that the first cut missed, and the crash cuts at quiescence and
// fetch-back.
installHarness();

describe("issue #1742 finalize durability at the filesystem boundary", () => {
  for (const location of ["root", "run"] as const) {
    it(`refuses a failed ${location} directory fsync through the real helper`, async () => {
      const root = tmpRoot();
      const { logger, lines } = recordingLogger();
      const outbox = makeOutbox(root, { log: logger });
      await outbox.init();
      const realOpen = fsp.open.bind(fsp);
      const failingDir = location === "root" ? root : path.join(root, RUN_A);
      mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
        const handle = await realOpen(...args);
        if (args[0] === failingDir) {
          mock.method(handle, "sync", async () => {
            throw Object.assign(new Error("injected directory I/O failure"), { code: "EIO" });
          });
        }
        return handle;
      });
      const result = await outbox.journalFinalize(RUN_A, 3);
      assert.equal(result.written, false);
      assert.equal(msgs(lines).includes("finalize record durable"), false);
      assert.deepEqual(outbox.listPendingFinalizes(), []);
      const files = await fsp.readdir(path.join(root, RUN_A));
      assert.equal(files.some((name) => name.endsWith(".tmp")), false, "the temporary hard link is cleaned up");
      if (location === "run") {
        assert.ok(files.includes("finalize-3.json"), "the installed destination survives the fsync failure");
      }
    });
  }

  for (const invalid of ["bad_mac", "wrong_identity", "symlink"] as const) {
    it(`refuses to adopt an existing ${invalid} finalize record`, async () => {
      const root = tmpRoot();
      const { logger, lines } = recordingLogger();
      const outbox = makeOutbox(root, { log: logger });
      await outbox.init();
      await outbox.journalFinalize(RUN_B, 4);
      const donor = path.join(root, RUN_B, "finalize-4.json");
      const dir = path.join(root, RUN_A);
      const target = path.join(dir, "finalize-3.json");
      await fsp.mkdir(dir);
      if (invalid === "symlink") await fsp.symlink(donor, target);
      else {
        const body = await fsp.readFile(donor, "utf8");
        await fsp.writeFile(target, invalid === "bad_mac" ? body.replace(RUN_B, RUN_A) : body);
      }
      const before = await fsp.readFile(target, "utf8");
      const durableBefore = msgs(lines).filter((m) => m === "finalize record durable").length;
      const result = await outbox.journalFinalize(RUN_A, 3);
      assert.equal(result.written, false);
      assert.equal(result.reason, "existing_finalize_invalid");
      assert.equal(msgs(lines).filter((m) => m === "finalize record durable").length, durableBefore);
      assert.equal(outbox.listPendingFinalizes().some((p) => p.run_id === RUN_A), false);
      assert.equal(await fsp.readFile(target, "utf8"), before, "never replaces the existing record");
      if (invalid === "symlink") assert.equal((await fsp.lstat(target)).isSymbolicLink(), true);
    });
  }
});

const roots: string[] = [];
afterEach(async () => {
  mock.restoreAll();
  for (const root of roots.splice(0)) await fsp.rm(root, { recursive: true, force: true });
});

function tmpRoot(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "issue-1742-hardening-"));
  roots.push(dir);
  return path.join(dir, "outbox");
}

function makeOutbox(
  root: string,
  opts: { rawWrite?: RawWriteSeam; log?: ReturnType<typeof nullLogger>; now?: () => number } = {},
): Outbox {
  return new Outbox({
    root,
    log: opts.log ?? nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
    ...(opts.rawWrite ? { rawWrite: opts.rawWrite } : {}),
    ...(opts.now ? { now: opts.now } : {}),
  });
}

async function finalizeFiles(root: string, runId: string): Promise<string[]> {
  const names = await fsp.readdir(path.join(root, runId)).catch((err: NodeJS.ErrnoException) => {
    if (err.code === "ENOENT") return [];
    throw err;
  });
  return names.filter((n) => /^finalize-.*[.]json$/.test(n)).sort();
}

const RUN_A = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN_B = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const runIdOf = (i: number) => `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`;

interface Line { level: string; msg: string; [k: string]: unknown }
const msgs = (lines: unknown[]) => (lines as Line[]).map((l) => l.msg);

async function registerWith(
  outbox: Outbox,
  register: (snapshot: ActiveSnapshot | undefined) => Promise<unknown>,
): Promise<void> {
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
  const wc = {
    register: async (_n: string, _t?: string, _m?: number, _c?: string[], _p?: string[], snapshot?: ActiveSnapshot) =>
      register(snapshot),
  } as unknown as WorkerClient;
  const worker = new Worker(
    { workerName: "finalize-probe", workerTemplate: "base", maxConcurrentRuns: 1, pollIntervalMs: 1 } as Config,
    wc,
    {} as RunRunner,
    {} as ChatRunner,
    {} as JudgeRunner,
    {} as ReviewRunner,
    nullLogger(),
    () => ({ ok: true, missing: [] }),
    outbox,
    new Map(),
    registry,
  );
  await (worker as unknown as { registerWithRetry(signal: AbortSignal): Promise<void> })
    .registerWithRetry(new AbortController().signal);
}

describe("issue #1742 finalize hardening: Outbox write seams", () => {
  it("a failing link() writes no durable line and leaves no record on disk or in the list", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const outbox = makeOutbox(root, { log: logger });
    await outbox.init();
    mock.method(fsp, "link", async () => {
      const err = new Error("injected link failure") as NodeJS.ErrnoException;
      err.code = "EACCES";
      throw err;
    });
    const res = await outbox.journalFinalize(RUN_A, 3);
    mock.restoreAll();
    assert.equal(res.written, false);
    assert.match(res.reason ?? "", /injected link failure/);
    assert.equal(msgs(lines).includes("finalize record durable"), false);
    assert.deepEqual(await finalizeFiles(root, RUN_A), [], "no record file");
    assert.equal((await fsp.readdir(path.join(root, RUN_A))).some((n) => n.endsWith(".tmp")), false, "no leftover temp");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), [], "nothing loadable after a restart");
  });

  it("a failing run-dir fsync writes no durable line and lists nothing in this process", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const outbox = makeOutbox(root, { log: logger });
    await outbox.init();
    const target = outbox as unknown as { fsyncDir(dir: string): Promise<void> };
    const realFsync = target.fsyncDir.bind(outbox);
    target.fsyncDir = async (dir: string) => {
      if (path.basename(dir) === RUN_A) throw new Error("injected dir fsync failure");
      await realFsync(dir);
    };
    const res = await outbox.journalFinalize(RUN_A, 3);
    assert.equal(res.written, false);
    assert.match(res.reason ?? "", /injected dir fsync failure/);
    assert.equal(msgs(lines).includes("finalize record durable"), false);
    assert.deepEqual(outbox.listPendingFinalizes(), [], "an unconfirmed-durable record is not listed in this process");
    // The link happened before the fsync failed, so the file IS on disk: the safe, intended behaviour.
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-3.json"], "the linked file is on disk after the failure");
    // A restarted Outbox loads it (and would offer it) until it is retired.
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), [{ run_id: RUN_A, claim_generation: 3 }]);
    await restarted.retireFinalize(RUN_A, 3);
    assert.deepEqual(await finalizeFiles(root, RUN_A), [], "retireFinalize removes the linked file");
    assert.deepEqual(restarted.listPendingFinalizes(), []);
  });

  it("an adopt after an unconfirmed install keeps the on-disk `since`, not the retry's clock", async () => {
    const root = tmpRoot();
    let clock = 1_000;
    const outbox = makeOutbox(root, { now: () => clock });
    await outbox.init();
    const target = outbox as unknown as { fsyncDir(dir: string): Promise<void> };
    const realFsync = target.fsyncDir.bind(outbox);
    target.fsyncDir = async (dir: string) => {
      if (path.basename(dir) === RUN_A) throw new Error("injected dir fsync failure");
      await realFsync(dir);
    };
    assert.equal((await outbox.journalFinalize(RUN_A, 3)).written, false); // linked, then fsync failed: since=1000 on disk
    target.fsyncDir = realFsync;
    clock = 5_000;
    assert.equal((await outbox.journalFinalize(RUN_B, 1)).written, true); // since=5000
    clock = 9_000;
    assert.equal((await outbox.journalFinalize(RUN_A, 3)).written, true); // adopts the file written at 1000
    // Ordering is by `since`: A (1000, kept from disk) sorts before B (5000). A fresh now() would be 9000.
    assert.deepEqual(outbox.listPendingFinalizes().map((e) => e.run_id), [RUN_A, RUN_B]);
  });

  it("rejects a non-integer, negative or unsafe claim generation and writes nothing", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    for (const gen of [-1, 1.5, Number.NaN, Number.POSITIVE_INFINITY, 2 ** 60]) {
      const res = await outbox.journalFinalize(RUN_A, gen);
      assert.equal(res.written, false, `gen ${gen}`);
      assert.equal(res.reason, "invalid_claim_generation", `gen ${gen}`);
    }
    assert.equal(fs.existsSync(path.join(root, RUN_A)), false, "no run dir was even created");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("loads only canonical file names: finalize-03.json is left on disk and not loaded", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 3);
    await fsp.rename(path.join(root, RUN_A, "finalize-3.json"), path.join(root, RUN_A, "finalize-03.json"));
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), []);
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-03.json"]);
  });
});

describe("issue #1742 finalize hardening: the offered list", () => {
  it("dedupes by run keeping the highest generation, before and after a restart", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    for (const gen of [2, 5, 3]) await outbox.journalFinalize(RUN_A, gen);
    await outbox.journalFinalize(RUN_B, 1);
    const want = [{ run_id: RUN_A, claim_generation: 5 }, { run_id: RUN_B, claim_generation: 1 }];
    assert.deepEqual(outbox.listPendingFinalizes(), want);
    const restarted = makeOutbox(root);
    await restarted.init();
    assert.deepEqual(restarted.listPendingFinalizes(), want);
  });

  it("caps the list at 256, oldest `since` first, and logs how many were omitted", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    let clock = 0;
    const outbox = makeOutbox(root, { log: logger, now: () => (clock += 10) });
    await outbox.init();
    const total = 260;
    for (let i = 0; i < total; i++) await outbox.journalFinalize(runIdOf(i), 1);
    const offered = outbox.listPendingFinalizes();
    assert.equal(offered.length, 256);
    assert.deepEqual(offered.map((e) => e.run_id), Array.from({ length: 256 }, (_, i) => runIdOf(i)), "the oldest 256, in order");
    const warn = (lines as Line[]).find((l) => l.msg.includes("over the register cap"));
    assert.ok(warn, "the omission was logged");
    assert.equal(warn.omitted, 4);
    assert.deepEqual(outbox.listPendingFinalizes(), offered, "the selection is deterministic");
  });

  it("breaks a `since` tie by run_id", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root, { now: () => 7 });
    await outbox.init();
    await outbox.journalFinalize(RUN_B, 1);
    await outbox.journalFinalize(RUN_A, 1);
    assert.deepEqual(outbox.listPendingFinalizes().map((e) => e.run_id), [RUN_A, RUN_B]);
  });

  it("an accepted register retires the offered records and the lower generations of the same run only", async () => {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    await outbox.journalFinalize(RUN_A, 2);
    await outbox.journalFinalize(RUN_A, 5);
    let sent: ActiveSnapshot | undefined;
    await registerWith(outbox, async (snapshot) => {
      sent = snapshot;
      await outbox.journalFinalize(RUN_A, 9); // a live flight's later generation: not offered, kept
      return {};
    });
    assert.deepEqual(sent?.finalize_resume, [{ run_id: RUN_A, claim_generation: 5 }]);
    assert.equal(sent?.pending_overflow, false, "a finalize-only snapshot leases nothing");
    assert.deepEqual(await finalizeFiles(root, RUN_A), ["finalize-9.json"]);
  });

  it("logs no `register finalize snapshot` line when nothing is offered", async () => {
    const root = tmpRoot();
    const { logger, lines } = recordingLogger();
    const outbox = makeOutbox(root, { log: logger });
    await outbox.init();
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
    const wc = { register: async () => ({}) } as unknown as WorkerClient;
    const worker = new Worker(
      { workerName: "w", workerTemplate: "base", maxConcurrentRuns: 1, pollIntervalMs: 1 } as Config,
      wc, {} as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, logger,
      () => ({ ok: true, missing: [] }), outbox, new Map(), registry,
    );
    await (worker as unknown as { registerWithRetry(signal: AbortSignal): Promise<void> })
      .registerWithRetry(new AbortController().signal);
    assert.equal(msgs(lines).includes("register finalize snapshot"), false);
  });
});

describe("issue #1742 finalize hardening: runner retirement and crash cuts", () => {
  const RUNNER_OPTS = { outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 };

  async function outboxWithRoot(): Promise<{ root: string; outbox: Outbox }> {
    const root = tmpRoot();
    const outbox = makeOutbox(root);
    await outbox.init();
    return { root, outbox };
  }

  it("retires the record when a finalize-time report reveals a SERVER wall park", async () => {
    const root = tmpRoot();
    // No terminal journal can be installed, so the terminal send is the first thing that can end
    // the flight and the journal-install retirement site is never reached.
    const rawWrite: RawWriteSeam = async (write, ctx) => {
      if (ctx.kind === "terminal") {
        const err = new Error("no space") as NodeJS.ErrnoException;
        err.code = "ENOSPC";
        throw err;
      }
      await write();
    };
    const outbox = makeOutbox(root, { rawWrite });
    await outbox.init();
    const claim = gitlabClaim(1750, { claim_generation: 4 });
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    api.failStateWhen(claim.run_id, (b) => b.status === "completed", {
      httpStatus: 409, runStatus: "paused", disposition: "stale_claim", holdReason: "budget_exhausted",
    });
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS }).execute(claim);
    assert.ok(
      !api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"),
      "a server wall park reports no failure",
    );
    assert.deepEqual(await finalizeFiles(root, claim.run_id), [], "retired: the api parked this generation");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("retires the record when the completion hold parks the run during finalize", async () => {
    const { root, outbox } = await outboxWithRoot();
    const claim = gitlabClaim(1751, { claim_generation: 2, config: { completion_contract_version: 1, contract_revision: 1 } });
    api.setCompletionPermitResponse(false, { denyReason: "missing_milestones" });
    git.trackingTip = (async () => "1111111111111111111111111111111111111111") as typeof git.trackingTip;
    const { gitlab } = fakeGitlab();
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS, recoveryRetryMs: 1 }).execute(claim);
    assert.equal(api.completionHoldRequests.length, 1, "the run entered the completion hold");
    assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    assert.deepEqual(await finalizeFiles(root, claim.run_id), [], "retired: the api accepted the hold");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("retires the record when a vault_locked deferral at finalize parks the run", async () => {
    const { root, outbox } = await outboxWithRoot();
    client.protocolFeatures = ["recovery_cause_vault_locked"];
    const claim = gitlabClaim(1752, { claim_generation: 2 });
    simulateCommittedWork();
    const realFetch = git.fetchAgentBranch.bind(git);
    let deferred = false;
    git.fetchAgentBranch = (async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      if (!deferred) {
        deferred = true;
        throw new CodexCredentialDeferredError();
      }
      return realFetch(...args);
    }) as typeof git.fetchAgentBranch;
    const { gitlab } = fakeGitlab();
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS, recoveryRetryMs: 1 }).execute(claim);
    assert.equal(deferred, true, "the deferral fired at the finalize fetch-back");
    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.ok(statuses.includes("recovery_wait"), `the run parked: ${statuses.join(",")}`);
    assert.deepEqual(await finalizeFiles(root, claim.run_id), [], "retired: the api accepted the vault park");
    assert.deepEqual(outbox.listPendingFinalizes(), []);
  });

  it("a cut at finalize quiescence carries the record: a restart loads it and the register offers it", async () => {
    const { root, outbox } = await outboxWithRoot();
    const claim = gitlabClaim(1753, { claim_generation: 3 });
    simulateCommittedWork();
    let reached!: () => void;
    const at = new Promise<void>((resolve) => { reached = resolve; });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const { gitlab } = fakeGitlab();
    const execution = runner(new StubExecutor(nullLogger()), gitlab, undefined, {
      outbox,
      ...RUNNER_OPTS,
      quiesceRun: async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
        if (req.site === "finalize") {
          reached();
          await gate;
          throw new Error("cut: process died at finalize quiescence");
        }
        return { process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" } };
      },
    }).execute(claim);
    try {
      await Promise.race([at, new Promise<void>((_, rej) => setTimeout(() => rej(new Error("quiescence cut not reached")), 10_000))]);
      await assertRestartOffers(root, claim.run_id, 3);
    } finally {
      release();
      await execution;
    }
  });

  it("a cut at finalize fetch-back carries the record: a restart loads it and the register offers it", async () => {
    const { root, outbox } = await outboxWithRoot();
    const claim = gitlabClaim(1754, { claim_generation: 5 });
    simulateCommittedWork();
    let reached!: () => void;
    const at = new Promise<void>((resolve) => { reached = resolve; });
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    git.fetchAgentBranch = (async () => {
      reached();
      await gate;
      throw new Error("cut: process died at finalize fetch-back");
    }) as typeof git.fetchAgentBranch;
    const { gitlab } = fakeGitlab();
    const execution = runner(new StubExecutor(nullLogger()), gitlab, undefined, { outbox, ...RUNNER_OPTS }).execute(claim);
    try {
      await Promise.race([at, new Promise<void>((_, rej) => setTimeout(() => rej(new Error("fetch-back cut not reached")), 10_000))]);
      await assertRestartOffers(root, claim.run_id, 5);
    } finally {
      release();
      await execution;
    }
  });
});

/** Restart an Outbox over `root` mid-flight and prove the record loads and a register offers it. */
async function assertRestartOffers(root: string, runId: string, gen: number): Promise<void> {
  assert.deepEqual(await finalizeFiles(root, runId), [`finalize-${gen}.json`], "the record is on disk at the cut");
  const restarted = makeOutbox(root);
  await restarted.init();
  assert.deepEqual(restarted.listPendingFinalizes(), [{ run_id: runId, claim_generation: gen }]);
  let sent: ActiveSnapshot | undefined;
  await registerWith(restarted, async (snapshot) => { sent = snapshot; return {}; });
  assert.deepEqual(sent?.finalize_resume, [{ run_id: runId, claim_generation: gen }]);
}
