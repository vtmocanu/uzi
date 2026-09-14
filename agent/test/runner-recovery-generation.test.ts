import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import type { BoundaryPermit, BoundaryRequest, CodexExecutionSafety } from "../src/harness.js";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer } from "../src/recovery.js";
import { type RecoveryBundleResult } from "../src/git.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryHold,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
} from "../src/protocol.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  deferred,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// PRD #1349 M2 — the RUNNER wiring of generation-exact custody disposition, asserted through a
// REAL RecoveryCoordinator injected via RunnerOptions.recovery (a fake archive client + fake
// bundle producer). Each test drives a park / early-terminal path and asserts the concrete
// release/reserve/upload calls + the exact claim generation — the actual safety behavior, not
// just "no error". The FakeGit.alreadyPublished flag models the fresh-forge comparison outcome:
// true = H already on the forge (provably empty → RELEASE), false = unpublished committed work
// (→ CAPTURE). The runner's OWN git (real GitCache) still builds the recovery checkpoint on the
// recovery_wait/shutdown paths; only the disposition's produce/fetch is the fake.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string, content: string): string {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
  return execFileSync("git", ["-C", treePath, "rev-parse", "HEAD"], {
    env: GIT_ENV,
    encoding: "utf8",
  }).trim();
}

// ── fakes injected into the real coordinator ─────────────────────────────────────

class FakeRecoveryClient implements RecoveryArchiveClient {
  reserveCalls: RecoveryReserveRequest[] = [];
  uploadCalls: Array<{ captureId: string; manifest: RecoveryUploadManifest }> = [];
  releaseCalls: Array<{ runId: string; generation?: number }> = [];
  listCalls: string[] = [];
  holds: RecoveryHold[] = [];
  uploadShouldThrow = false;

  async reserveRecoveryCapture(_runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push(req);
    return { capture_id: "srv-capture-1", state: "preparing" };
  }
  async getRecoveryCaptureStatus(_runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(
    _runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
  ): Promise<RecoveryCaptureStatusResponse> {
    for await (const _ of bundle) {
      /* drain */
    }
    this.uploadCalls.push({ captureId, manifest });
    if (this.uploadShouldThrow) throw new Error("upload rejected");
    return { capture_id: captureId, state: "available", manifest_bound: true };
  }
  async releaseRecoveryCustody(runId: string, generation?: number): Promise<RecoveryReleaseResponse> {
    this.releaseCalls.push({ runId, generation });
    return { run_id: runId, released: true, holds_released: 1 };
  }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    this.listCalls.push(runId);
    return { run_id: runId, holds: this.holds };
  }
  /** The exact generations that were released, in call order. */
  releasedGenerations(): Array<number | undefined> {
    return this.releaseCalls.map((c) => c.generation);
  }
}

class FakeRecoveryGit implements RecoveryBundleProducer {
  fetchCalls = 0;
  produceCalls = 0;
  alreadyPublished = false;
  bytes = Buffer.from("m2-fake-bundle-bytes\x00ÿ");

  async fetchDefaultTip(): Promise<string> {
    this.fetchCalls++;
    return "3".repeat(40);
  }
  async produceRecoveryBundle(
    _barePath: string,
    opts: { sourceSha: string; outPath: string; forgeTip?: string; maxBytes?: number },
  ): Promise<RecoveryBundleResult> {
    this.produceCalls++;
    if (this.alreadyPublished) {
      return {
        bundlePath: "",
        byteSize: 0,
        checksum: "",
        chunkCount: 0,
        prerequisiteShas: [],
        sourceSha: opts.sourceSha,
        selfContained: false,
        alreadyPublished: true,
      };
    }
    await fsp.writeFile(opts.outPath, this.bytes);
    return {
      bundlePath: opts.outPath,
      byteSize: this.bytes.length,
      checksum: createHash("sha256").update(this.bytes).digest("hex"),
      chunkCount: 1,
      prerequisiteShas: [opts.forgeTip ?? "base"],
      sourceSha: opts.sourceSha,
      selfContained: false,
      alreadyPublished: false,
    };
  }
}

interface Injected {
  coord: RecoveryCoordinator;
  client: FakeRecoveryClient;
  git: FakeRecoveryGit;
  root: string;
}

function injectedCoordinator(): Injected {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-recov-"));
  const client = new FakeRecoveryClient();
  const git = new FakeRecoveryGit();
  const coord = new RecoveryCoordinator({
    client,
    git,
    log: nullLogger(),
    recoveryRoot: root,
    workerToken: "m2-worker-join-token-0123456789",
    now: () => 1_700_000_000_000,
  });
  return { coord, client, git, root };
}

/** An executor that (optionally) commits work in the runner clone, then throws a generic
 *  error — the shape of an early live-worker failure that reaches executeClaim's generic
 *  terminal catch (reports `failed`, then dispositions its exact generation hold). */
function failFactory(homeRoot: string, commit: boolean): ExecutorFactory {
  return (runId) => {
    const runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(runHome, { recursive: true });
          if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the failure\n");
          throw new Error("agent failed hard");
        },
      },
    };
  };
}

/** The recovery_wait factory: materialize the resume dirs, optionally do work in the clone,
 *  then throw TransientRecoveryError (the persistently-empty-turn shape). */
function recoveryWaitFactory(
  homeRoot: string,
  iid: number,
  work: (worktreePath: string) => void,
): ExecutorFactory {
  const pluginDir = skillsPluginDir(worktreeDirFor(iid));
  return (runId) => {
    const runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(pluginDir, { recursive: true });
          fs.writeFileSync(path.join(pluginDir, "marker"), "x");
          fs.mkdirSync(runHome, { recursive: true });
          fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
          work(ctx.worktreePath);
          throw new TransientRecoveryError();
        },
      },
    };
  };
}

/** Resolves once `signal` aborts. */
function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

/** A shutdown factory: commit work, materialize the resume dirs, signal started, wait for
 *  the worker-shutdown abort, then throw. */
function shutdownFactory(
  homeRoot: string,
  iid: number,
): { factory: ExecutorFactory; started: Promise<void>; sha: () => string } {
  const pluginDir = skillsPluginDir(worktreeDirFor(iid));
  const start = deferred();
  let sha = "";
  const factory: ExecutorFactory = (runId) => {
    const runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          sha = commitInTree(ctx.worktreePath, "WORK.txt", "work before shutdown\n");
          fs.mkdirSync(pluginDir, { recursive: true });
          fs.writeFileSync(path.join(pluginDir, "marker"), "x");
          fs.mkdirSync(runHome, { recursive: true });
          fs.writeFileSync(path.join(runHome, "session"), "transcript", "utf8");
          start.resolve();
          await waitAbort(ctx.signal!);
          throw new Error("aborted mid-run");
        },
      },
    };
  };
  return { factory, started: start.promise, sha: () => sha };
}

const hasStatus = (runId: string, status: string): boolean =>
  api.states.some((s) => s.runId === runId && s.body.status === status);

describe("RunRunner — early failed/cancelled exact-generation disposition (PRD #1349 M2, D4.5)", () => {
  it("empty early failure RELEASES the exact generation before cleanup (no work → release)", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-emptyfail-"));
    try {
      git.alreadyPublished = true; // fresh-forge proof: H already published → provably empty
      const claim = gitlabClaim(4101, { claim_generation: 7 });
      await runnerWith(failFactory(homeRoot, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.deepEqual(
        client.releasedGenerations(),
        [7],
        "the exact claim generation (7) was released, and only it",
      );
      assert.equal(client.reserveCalls.length, 0, "an empty hold reserves nothing");
      assert.equal(client.uploadCalls.length, 0, "an empty hold uploads nothing");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("DISCRIMINATING opposite: commit-then-fail CAPTURES and NEVER releases", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-commitfail-"));
    try {
      git.alreadyPublished = false; // unpublished committed output exists
      const claim = gitlabClaim(4102, { claim_generation: 7 });
      await runnerWith(failFactory(homeRoot, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"));
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
      assert.equal(client.reserveCalls.length, 1, "committed work is reserved for capture");
      assert.equal(client.reserveCalls[0]!.generation, 7, "the reserve binds the exact generation");
      assert.equal(client.uploadCalls.length, 1, "the generation-bound bundle is uploaded");
      assert.equal(git.fetchCalls, 1, "settlement did a fresh forge-tip fetch (settlement-time ancestry)");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("upload failure RETAINS: reserves + attempts upload, never releases, marks needs_action", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-uploadfail-"));
    try {
      git.alreadyPublished = false;
      client.uploadShouldThrow = true;
      const claim = gitlabClaim(4103, { claim_generation: 12 });
      await runnerWith(failFactory(homeRoot, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.equal(client.releaseCalls.length, 0, "an upload failure NEVER releases (no fabricated release)");
      assert.equal(client.reserveCalls.length, 1);
      assert.equal(client.uploadCalls.length, 1, "the upload was attempted");
      const recs = await coord.inspect(claim.run_id);
      assert.equal(recs.length, 1);
      assert.equal(recs[0]!.state, "needs_action", "the local source + open hold are retained");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("after clone: inventories prior holds and default reseed RETAINS the predecessor (releases only the current generation)", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-inventory-"));
    try {
      // A prior generation's hold is open on this run (an abrupt predecessor). The current
      // run reseeded from default (no predecessor source), so it can only settle ITS OWN hold.
      client.holds = [{ hold_id: "hold-old", generation: 3, has_available_capture: false }];
      git.alreadyPublished = true; // the current generation is provably empty
      const claim = gitlabClaim(4104, { claim_generation: 4 });
      await runnerWith(failFactory(homeRoot, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.deepEqual(client.listCalls, [claim.run_id], "the post-clone generation-exact inventory ran once");
      assert.deepEqual(
        client.releasedGenerations(),
        [4],
        "released ONLY the current generation (4); the inventoried predecessor (3) is RETAINED",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

describe("RunRunner — recovery_wait park exact-generation disposition (PRD #1349 M2, D4)", () => {
  it("committed local checkpoint (unpublished) → CAPTURE, never release", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-rw-commit-"));
    try {
      git.alreadyPublished = false;
      const iid = 4201;
      const factory = recoveryWaitFactory(homeRoot, iid, (wt) => {
        commitInTree(wt, "WORK.txt", "recovered committed work\n");
      });
      const claim = gitlabClaim(iid, { claim_generation: 5 });
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "recovery_wait"), "the run parked recovery_wait");
      assert.equal(client.releaseCalls.length, 0, "work-bearing recovery_wait never releases");
      assert.equal(client.reserveCalls.length, 1, "the committed restore point is captured");
      assert.equal(client.reserveCalls[0]!.generation, 5);
      assert.equal(client.uploadCalls.length, 1);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("uncommitted work → WIP restore commit → CAPTURE", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-rw-wip-"));
    try {
      git.alreadyPublished = false;
      const iid = 4202;
      const factory = recoveryWaitFactory(homeRoot, iid, (wt) => {
        // A dirty, never-committed edit → commitWipMarker captures it into the restore point.
        fs.writeFileSync(path.join(wt, "WIP.txt"), "in-progress, uncommitted\n");
      });
      const claim = gitlabClaim(iid, { claim_generation: 8 });
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "recovery_wait"));
      assert.equal(client.releaseCalls.length, 0, "the WIP restore commit is work → never released");
      assert.equal(client.reserveCalls.length, 1, "the WIP restore commit is captured");
      assert.equal(client.reserveCalls[0]!.generation, 8);
      assert.equal(client.uploadCalls.length, 1);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("clean tree (no work) recovery_wait → RELEASE the exact generation (published/empty)", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-rw-clean-"));
    try {
      git.alreadyPublished = true; // the already-committed base tip is on the forge → empty
      const iid = 4203;
      const factory = recoveryWaitFactory(homeRoot, iid, () => {
        /* clean tree */
      });
      const claim = gitlabClaim(iid, { claim_generation: 6 });
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "recovery_wait"));
      assert.deepEqual(client.releasedGenerations(), [6], "a verified-empty recovery_wait releases the exact hold");
      assert.equal(client.uploadCalls.length, 0, "nothing to archive");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

describe("RunRunner — graceful shutdown retains within the k8s grace (PRD #1349 M2, D4.6)", () => {
  it("pins the generation and RETAINS (no release/reserve/upload) — settlement deferred", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-shutdown-"));
    try {
      const iid = 4301;
      const { factory, started, sha } = shutdownFactory(homeRoot, iid);
      const claim = gitlabClaim(iid, { claim_generation: 9 });
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord });
      const p = runner.execute(claim);
      await started;
      runner.shutdown();
      await p;
      // A grace-bounded shutdown MUST NOT run a credentialed fresh-forge capture (it could
      // exceed the k8s termination grace). It pins the generation and retains custody.
      assert.equal(client.releaseCalls.length, 0, "a bounded shutdown never releases");
      assert.equal(client.reserveCalls.length, 0, "no capture upload within the grace");
      assert.equal(client.uploadCalls.length, 0);
      const recs = await coord.inspect(claim.run_id);
      assert.equal(recs.length, 1, "the generation was pinned");
      assert.equal(recs[0]!.state, "pinned", "the source is RETAINED (pinned) for later settlement");
      assert.equal(recs[0]!.generation, 9, "the pinned record carries the exact generation");
      // The shutdown pin ADVANCED the source past the early-pin base to the committed head,
      // proving the shutdown-branch pin ran (not merely the after-clone evidence pin).
      assert.equal(recs[0]!.sourceSha, sha(), "the shutdown pin recorded the committed restore point");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

// PRD #1349 M2 (F3) — a no-code `completed` code-publishing run (report_only / not_code /
// scope-capped-empty) never reaches the finalization pin, so its EARLY generation-evidence pin
// would leak and the next restart's resumePending would escalate it to a FALSE needs_action. The
// completed terminal drive must release this generation's hold (and remove the local record) even
// when the finalization recoveryRecord is absent.
describe("RunRunner — no-code completion settles the early-evidence hold (PRD #1349 M2, F3)", () => {
  it("a report-only COMPLETED run releases its exact generation and removes the leaked early-evidence record", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-reportonly-"));
    try {
      const claim = gitlabClaim(4401, { claim_generation: 15 });
      const factory: ExecutorFactory = (runId) => {
        const runHome = path.join(homeRoot, runId);
        return {
          homeDir: runHome,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.mkdirSync(runHome, { recursive: true });
              // No commit, no checkpoint: a genuine no-code deliverable (report_only completes).
              return { branch: ctx.branch, reportOnly: true, summary: "findings only, nothing to land" };
            },
          },
        };
      };
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "completed"), "the run completed report-only");
      assert.deepEqual(
        client.releasedGenerations(),
        [15],
        "a no-code completion releases the exact generation (the early-evidence hold)",
      );
      assert.equal(client.reserveCalls.length, 0, "a no-code completion captures nothing");
      assert.deepEqual(
        await coord.inspect(claim.run_id),
        [],
        "the early-evidence record is removed — no leaked `pinned` record for the restart sweep to escalate",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

// PRD #1349 M2 (F4) — a usage-limit hit with wait_on_limit=false reports `failed` and returns
// parked=false, so the limit-park settle is skipped and the error never reaches the generic catch.
// A code-publishing run that hit the limit with opt-out must still be routed through the
// exact-generation disposition before its clone is torn down, so committed-since-checkpoint work is
// CAPTURED (never silently dropped) and a provably-empty opt-out RELEASES its exact hold.
describe("RunRunner — usage-limit opt-out routes through the coordinator (PRD #1349 M2, F4)", () => {
  function limitOptOutFactory(homeRoot: string, commit: boolean): ExecutorFactory {
    return (runId) => {
      const runHome = path.join(homeRoot, runId);
      return {
        homeDir: runHome,
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(runHome, { recursive: true });
            if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the limit\n");
            throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          },
        },
      };
    };
  }

  it("wait_on_limit=false with committed work CAPTURES (never silently drops) before teardown", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-optout-"));
    try {
      git.alreadyPublished = false; // committed-since-checkpoint work is unpublished
      const claim = gitlabClaim(4501, { claim_generation: 13, wait_on_limit: false });
      await runnerWith(limitOptOutFactory(homeRoot, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "an opt-out usage-limit hit reports failed");
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
      assert.equal(client.reserveCalls.length, 1, "the opt-out committed work is CAPTURED, not dropped");
      assert.equal(client.reserveCalls[0]!.generation, 13, "the capture binds the exact generation");
      assert.equal(client.uploadCalls.length, 1, "the generation-bound bundle is uploaded");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("DISCRIMINATING: wait_on_limit=false with NO work RELEASES the exact generation (provably empty)", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-optout-empty-"));
    try {
      git.alreadyPublished = true; // provably empty → H already on the forge
      const claim = gitlabClaim(4502, { claim_generation: 14, wait_on_limit: false });
      await runnerWith(limitOptOutFactory(homeRoot, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"));
      assert.deepEqual(client.releasedGenerations(), [14], "a provably-empty opt-out releases its exact hold");
      assert.equal(client.uploadCalls.length, 0, "nothing to archive on an empty opt-out");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

// PRD #1349 M2 (F4 regression) — the OPT-OUT is not the only non-parked limit outcome.
// handleLimitReached returns parked=false on THREE paths: the wait_on_limit=false opt-out (above),
// a park report that THREW, and a server ACK whose status is not `limit_wait` (the server declined
// the park and failed the run instead — RUN_LIMIT_MAX_WAITS exhausted, retry_not_before past
// RUN_LIMIT_MAX_PARK). The last two carry wait_on_limit=true, so a `} else if (!claim.wait_on_limit)`
// guard skipped them: their committed-since-checkpoint work was torn down with the clone and NO
// disposition ran. The fix routes ALL non-parked limit outcomes through the same exact-generation
// disposition. This block drives the server-declined path (ack != limit_wait via overrideStateStatus)
// with wait_on_limit=TRUE and asserts the disposition fires — it reserves nothing on the unfixed
// opt-out-only guard.
describe("RunRunner — a server-declined park still routes through the coordinator (PRD #1349 M2, F4 regression)", () => {
  function limitFactory(homeRoot: string, commit: boolean): ExecutorFactory {
    return (runId) => {
      const runHome = path.join(homeRoot, runId);
      return {
        homeDir: runHome,
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(runHome, { recursive: true });
            if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the limit\n");
            throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          },
        },
      };
    };
  }

  it("wait_on_limit=true but the server DECLINED the park: committed work CAPTURES before teardown", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-declined-commit-"));
    try {
      git.alreadyPublished = false; // committed-since-checkpoint work is unpublished
      const claim = gitlabClaim(4701, { claim_generation: 17, wait_on_limit: true });
      // 200 + "failed": the park request is acknowledged with a status that is NOT limit_wait, so
      // handleLimitReached returns parked=false while wait_on_limit stays true — the exact path the
      // opt-out-only guard dropped.
      api.overrideStateStatus(claim.run_id, "failed");
      await runnerWith(limitFactory(homeRoot, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
      assert.equal(
        client.reserveCalls.length,
        1,
        "a declined park with committed work is CAPTURED, not silently dropped (0 on the opt-out-only guard)",
      );
      assert.equal(client.reserveCalls[0]!.generation, 17, "the capture binds the exact generation");
      assert.equal(client.uploadCalls.length, 1, "the generation-bound bundle is uploaded");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("DISCRIMINATING: wait_on_limit=true declined park with NO work RELEASES the exact generation", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, git, root } = injectedCoordinator();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-declined-empty-"));
    try {
      git.alreadyPublished = true; // provably empty → H already on the forge
      const claim = gitlabClaim(4702, { claim_generation: 18, wait_on_limit: true });
      api.overrideStateStatus(claim.run_id, "failed");
      await runnerWith(limitFactory(homeRoot, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.deepEqual(client.releasedGenerations(), [18], "a provably-empty declined park releases its exact hold");
      assert.equal(client.uploadCalls.length, 0, "nothing to archive on an empty declined park");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

// PRD #1349 M2 (F2) — a credentialed settleRecoveryGeneration does a PAT-bearing fresh-forge
// fetch. On a CODEX run the provider root is disposed only in executeClaim's finally (killAgentTree
// is a no-op), so a bare settle in the generic-failure catch would race a still-alive Codex provider
// (PAT-in-/proc/environ exposure). The fix reaps the provider (Codex-aware, via withBoundary) BEFORE
// the credentialed fetch. Driven with a Codex-shaped executor (a `.safety` whose withBoundary models
// the quiesce+reap, NO killAgentTree).
describe("RunRunner — credentialed settle reaps the Codex provider FIRST (PRD #1349 M2, F2)", () => {
  /** A FakeRecoveryGit that records, at each credentialed fetchDefaultTip, whether the provider had
   *  already been reaped (`!providerAlive`). Shares the live-provider flag with the fake safety. */
  class ReapProbeGit extends FakeRecoveryGit {
    reapedAtFetch: boolean[] = [];
    constructor(private readonly state: { providerAlive: boolean }) {
      super();
    }
    override async fetchDefaultTip(): Promise<string> {
      this.reapedAtFetch.push(!this.state.providerAlive);
      return super.fetchDefaultTip();
    }
  }

  it("a Codex generic failure reaps the provider root BEFORE the credentialed settle fetch", async () => {
    const { gitlab } = fakeGitlab();
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-codexreap-"));
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m2-codexreap-home-"));
    try {
      const state = { providerAlive: false };
      const client = new FakeRecoveryClient();
      const git = new ReapProbeGit(state);
      git.alreadyPublished = false; // committed work → the credentialed fresh-forge fetch runs
      const coord = new RecoveryCoordinator({
        client,
        git,
        log: nullLogger(),
        recoveryRoot: root,
        workerToken: "m2-worker-join-token-0123456789",
        now: () => 1_700_000_000_000,
      });
      // A minimal Codex-shaped safety: withBoundary models the quiesce+reap of the provider root.
      const boundaries: string[] = [];
      const safety: CodexExecutionSafety = {
        kind: "codex",
        withBoundary: async <T>(
          req: BoundaryRequest,
          action: (permit: BoundaryPermit) => Promise<T>,
        ): Promise<T> => {
          boundaries.push(req.boundary);
          state.providerAlive = false; // withBoundary quiesced + reaped the provider root
          const ac = new AbortController();
          return action({ signal: ac.signal } as unknown as BoundaryPermit);
        },
        spawnBoundaryProcess: async () => {
          throw new Error("spawnBoundaryProcess is not used by this test");
        },
        dispose: async () => ({ kind: "disposed" }),
      };
      const claim = gitlabClaim(4601, { claim_generation: 21 });
      const factory: ExecutorFactory = (runId) => {
        const runHome = path.join(homeRoot, runId);
        return {
          homeDir: runHome,
          executor: {
            safety,
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.mkdirSync(runHome, { recursive: true });
              state.providerAlive = true; // the Codex provider subprocess is live
              commitInTree(ctx.worktreePath, "WORK.txt", "committed before the codex failure\n");
              throw new Error("codex agent failed hard");
            },
          },
        };
      };
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      // The credentialed settle DID run — committed work is captured, not dropped.
      assert.equal(git.fetchCalls, 1, "the credentialed fresh-forge fetch ran");
      assert.equal(client.reserveCalls.length, 1, "committed work was captured");
      // ...and it ran under a Codex reap boundary, AFTER the provider root was reaped (F2).
      assert.ok(boundaries.length >= 1, "the settle ran under a Codex reap boundary");
      assert.deepEqual(
        git.reapedAtFetch,
        [true],
        "the provider was reaped BEFORE the credentialed fetch (no PAT-in-/proc/environ race)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});
