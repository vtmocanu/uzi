import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
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
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { buildRunLaneReconcile } from "../src/codex/codex-executor.js";
import {
  ExecutionRegistry,
  newLocalExecutionEpoch,
  type RegisteredRoot,
} from "../src/codex/registry.js";
import { selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  deferred,
  fakeGitlab,
  git,
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
  // PRD #1392 M1/M2 (fact 9): record the `releaseEvidence` class each release stamped, so the
  // completion path's publication/NULL mapping is observable ("publication" for a pushed branch,
  // undefined for a no-code completion).
  releaseCalls: Array<{ runId: string; generation?: number; evidence?: string }> = [];
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
  async releaseRecoveryCustody(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    this.releaseCalls.push({ runId, generation, evidence: releaseEvidence });
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
  /** The evidence class each release stamped, in call order (PRD #1392 M1/M2, fact 9). */
  releasedEvidence(): Array<string | undefined> {
    return this.releaseCalls.map((c) => c.evidence);
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
      // PRD #1392 M1/M2 (fact 9): a no-code completion carries NO branch, so it is NOT a
      // publication — the release stamps NO evidence class (the api stores NULL), never mis-stamps.
      assert.deepEqual(
        client.releasedEvidence(),
        [undefined],
        "a report-only completion stamps NO release_evidence (NULL)",
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

// PRD #1392 M1/M2 (fact 9) — the completion release stamps its evidence CLASS from the reported
// body: a full publication (a completed report carrying a pushed `branch`) → "publication"; a
// no-code completion (report_only / not_code / scope-capped-empty, no branch) → NO evidence (the
// api stores NULL). The report-only case above pins the NULL half; this block pins the publication
// half through the REAL push+MR completion path (StubExecutor commits a real file and returns its
// branch, so the runner pushes it and reports { status:"completed", branch }).
describe("RunRunner — completion release_evidence publication mapping (PRD #1392 M1/M2, fact 9)", () => {
  it("a full publication (pushed branch + MR) stamps release_evidence: \"publication\"", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, client, root } = injectedCoordinator();
    try {
      const claim = gitlabClaim(4801, { claim_generation: 22 });
      await runnerWith(
        () => ({ executor: new StubExecutor(nullLogger()) }),
        gitlab,
        undefined,
        nullLogger(),
        { recovery: coord },
      ).execute(claim);
      assert.ok(hasStatus(claim.run_id, "completed"), "the run completed with a pushed branch + MR");
      // The completion released the exact generation, stamped "publication" (a pushed branch was
      // reported) — never the fresh-forge "forge_no_output" and never NULL.
      assert.deepEqual(client.releasedGenerations(), [22], "the exact generation was released");
      assert.deepEqual(
        client.releasedEvidence(),
        ["publication"],
        "a pushed-branch completion stamps release_evidence: publication",
      );
    } finally {
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

// PRD #1349 M2 (D4.5) / #1531 & #1539 — shared Codex-reap fixtures. Lifted to file scope so the
// #1539 recovery-cancellation describe below reuses the SAME real Codex boundary + event log.
const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok-XXXXXXXX",
  capability: "run-cap-XXXXXXXX",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
};
const RELEASE_TOK = "codex-release-tok-XXXXXXXX";
const REFRESH_TOK = "codex-refresh-tok-XXXXXXXX";

function bindingOf(block: Record<string, unknown>): CodexBinding {
  const sel = selectCodexBinding({ codex: block });
  if (sel.kind !== "codex") throw new Error("expected a codex selection");
  return sel.binding;
}

/** A registry-ownable provider root whose reap/dispose are idempotent no-ops. */
function trackedRoot(): RegisteredRoot {
  return {
    kind: "provider",
    reap: async () => ({ ok: true }),
    dispose: async () => {},
  };
}

function registerRoot(reg: ExecutionRegistry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  if (reserved.kind === "reserved") reg.registerRoot(reserved.reservation, root);
}

/** A REAL Codex safety (via createCodexExecutionSafety over a real registry + a tracked provider
 *  root + the real run-lane reconcile) whose per-sink reconcile pushes "reconcile" on each call,
 *  then throws when `decideThrow()` says so — modelling either the api's 409 once a terminal
 *  state has been recorded, or a genuinely contended reconcile even while active. */
function codexReapSafety(events: string[], decideThrow: () => boolean): CodexExecutionSafety {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  registerRoot(registry, trackedRoot());
  const binding = bindingOf(SUBSCRIPTION);
  const reconcileClient = {
    releaseCodex: async () => {
      events.push("reconcile");
      if (decideThrow()) throw new Error("codex reconcile refused (409): run is terminal");
      return { access_token: RELEASE_TOK };
    },
    refreshCodex: async () => {
      events.push("reconcile");
      if (decideThrow()) throw new Error("codex reconcile refused (409): run is terminal");
      return { access_token: REFRESH_TOK, generation: 7, outcome: "advanced" };
    },
  };
  const reconcile = buildRunLaneReconcile("run-1531-codex", reconcileClient as never, binding, () => {});
  return createCodexExecutionSafety(
    registry,
    async () => {
      throw new Error("boundary-action spawn is not used by the reap-only path");
    },
    reconcile,
    undefined,
    async (request) => {
      const [command, ...args] = request.argv;
      if (!command) throw new Error("empty test process argv");
      const child = spawn(command, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
      const terminal = new Promise<{ code: number }>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code: code ?? (signal ? 128 : 1) }));
      });
      return {
        root: {
          kind: "boundary_action",
          reap: async () => { await terminal; return { ok: true }; },
          dispose: async () => { if (child.exitCode === null) child.kill("SIGKILL"); await terminal.catch(() => undefined); },
        },
        stdin: child.stdin,
        stdout: child.stdout,
        stderr: child.stderr,
        waitChild: async () => terminal,
      };
    },
  );
}

/** The injected recovery client that additionally records "reserve"/"release" into the SHARED
 *  ordered event log, so the settle's disposition is ordered relative to reconcile/failed. */
class EventRecoveryClient extends FakeRecoveryClient {
  constructor(private readonly events: string[]) {
    super();
  }
  override async reserveRecoveryCapture(
    runId: string,
    req: RecoveryReserveRequest,
  ): Promise<RecoveryReserveResponse> {
    this.events.push("reserve");
    return super.reserveRecoveryCapture(runId, req);
  }
  override async releaseRecoveryCustody(
    runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    this.events.push("release");
    return super.releaseRecoveryCustody(runId, generation, releaseEvidence);
  }
}

interface CodexReapFixture {
  coord: RecoveryCoordinator;
  client: EventRecoveryClient;
  git: FakeRecoveryGit;
  safety: CodexExecutionSafety;
  root: string;
}

function fixture(events: string[], decideThrow: () => boolean): CodexReapFixture {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1531-codex-"));
  const client = new EventRecoveryClient(events);
  const git = new FakeRecoveryGit();
  const coord = new RecoveryCoordinator({
    client,
    git,
    log: nullLogger(),
    recoveryRoot: root,
    workerToken: "m2-worker-join-token-0123456789",
    now: () => 1_700_000_000_000,
  });
  const safety = codexReapSafety(events, decideThrow);
  return { coord, client, git, safety, root };
}

/** A Codex-shaped executor: carries `.safety`, NO killAgentTree, (optionally) commits, then
 *  throws a generic error — the first-turn Codex failure that reaches reportGenericFailure. */
function codexFailFactory(homeRoot: string, safety: CodexExecutionSafety, commit: boolean): ExecutorFactory {
  return (runId) => {
    const runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        safety,
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(runHome, { recursive: true });
          if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the codex failure\n");
          throw new Error("codex agent failed hard");
        },
      },
    };
  };
}

// PRD #1349 M2 (D4.5) / #1531 — a FIRST-TURN Codex failure via reportGenericFailure must reap the
// Codex provider WHILE the run is still actively-claimed (BEFORE the terminal `failed` report),
// then run the non-status-gated custody settle AFTER the report. The Codex per-sink credential
// reconcile inside withBoundary (refreshCodex/releaseCodex) is authorized by the api ONLY while
// the run is actively-claimed (codexActivelyClaimedStatuses); once the status is terminal it is
// refused (409), the blocked reconcile throws CodexBoundaryError, and the hold would leak as
// source_only. Unlike the F2 test above (a PERMISSIVE fake withBoundary that just flips a flag),
// this drives a REAL Codex boundary — a real ExecutionRegistry + tracked provider RegisteredRoot +
// the real buildRunLaneReconcile over a subscription binding — whose reconcile FAILS once a
// terminal state has been recorded (mirroring the 409), and asserts the split order
// `reconcile < failed < settle`. The mutation control: move the reap back AFTER the failed report
// and the 409-shaped reconcile blocks it → no release/no capture (cases 1 & 2 redden).
describe("RunRunner — first-turn Codex failure reaps BEFORE the terminal report (PRD #1349 M2 D4.5 / #1531)", () => {
  it("clean tree: reap (reconcile) runs BEFORE the failed report, then the exact-generation release settles after it", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4611, { claim_generation: 21 });
    // The 409 model: the reconcile throws ONCE a terminal `failed` has been recorded for the run.
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1531-clean-home-"));
    try {
      git.alreadyPublished = true; // provably empty → RELEASE the exact hold
      api.onState(claim.run_id, (body) => {
        if (body.status === "failed") events.push("failed");
      });
      await runnerWith(codexFailFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      // The reap's per-sink reconcile ran while still actively-claimed — BEFORE the `failed`
      // report — so it was authorized (no 409); the settle then released AFTER the report.
      assert.ok(events.indexOf("reconcile") >= 0, "the reap boundary ran the per-sink reconcile");
      assert.ok(events.indexOf("failed") >= 0, "the run reported failed");
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("failed"),
        `reap-reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("release"),
        `the settle release must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.deepEqual(
        client.releasedGenerations(),
        [21],
        "the release named the EXACT claim generation (21)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("committed tree: reap precedes the failed report, then the generation-bound CAPTURE settles after it (never releases)", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4612, { claim_generation: 21 });
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1531-commit-home-"));
    try {
      git.alreadyPublished = false; // unpublished committed work → CAPTURE
      api.onState(claim.run_id, (body) => {
        if (body.status === "failed") events.push("failed");
      });
      await runnerWith(codexFailFactory(homeRoot, safety, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"));
      assert.ok(
        events.indexOf("reconcile") >= 0 && events.indexOf("failed") >= 0,
        `both reconcile and failed recorded; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("failed"),
        `reap-reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("reserve"),
        `the capture reserve must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.equal(client.reserveCalls.length, 1, "committed work is captured");
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("a genuinely contended reconcile (alwaysBlock) RETAINS the hold: no release/reserve, but the terminal failure is still reported", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4613, { claim_generation: 21 });
    // alwaysBlock: the reconcile throws even while the run is still `running`, so the reap fails and
    // the hold is retained (source_only) — but the run must still report its terminal failure.
    const { coord, client, git, safety, root } = fixture(events, () => true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1531-retain-home-"));
    try {
      git.alreadyPublished = true;
      api.onState(claim.run_id, (body) => {
        if (body.status === "failed") events.push("failed");
      });
      await runnerWith(codexFailFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.equal(client.releaseCalls.length, 0, "a blocked reap retains the hold — never releases");
      assert.equal(client.reserveCalls.length, 0, "a blocked reap retains the hold — never captures");
      assert.ok(
        hasStatus(claim.run_id, "failed"),
        "the terminal failure is still reported (the hold is left open, not the report)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

// PRD #1349 M2 (D4.5) / #1539 — a recovery CANCELLATION (a steering-cancel that terminates
// handleRecoveryExhausted's hold loop) must reap the Codex provider WHILE the run is still
// actively-claimed (BEFORE the terminal `failed`/cancelled report), then run the non-status-gated
// custody settle AFTER the report. Same 409 hazard as #1531's reportGenericFailure site: the Codex
// per-sink reconcile inside withBoundary is authorized ONLY while actively-claimed; once the report
// makes the run terminal it is refused, and a settle-then-report ordering would block the reap and
// leak the exact-generation `source_only` hold. This reuses the SAME real Codex boundary + ordered
// event log as #1531, drives the executor to a TransientRecoveryError (so handleRecoveryExhausted
// owns it), fails recovery capture BEFORE its publish boundary (so capture runs no reconcile and the
// loop keeps looping until the cancel lands), and asserts the split order `reconcile < failed <
// settle`. MUTATION control: restore the old `reapThenSettleRecoveryGeneration` AFTER the report and
// drop the pre-report reap → the 409-shaped reconcile blocks it → cases (a)/(b) redden.
describe("RunRunner — recovery cancellation reaps BEFORE the terminal report (PRD #1349 M2 D4.5 / #1539)", () => {
  /** A Codex-shaped executor (carries `.safety`, NO killAgentTree) that (optionally) commits, then
   *  throws a TransientRecoveryError — the recovery-hold entry that handleRecoveryExhausted owns. */
  function codexTransientFactory(homeRoot: string, safety: CodexExecutionSafety, commit: boolean): ExecutorFactory {
    return (runId) => {
      const runHome = path.join(homeRoot, runId);
      return {
        homeDir: runHome,
        executor: {
          safety,
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(runHome, { recursive: true });
            if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the codex cancel\n");
            throw new TransientRecoveryError();
          },
        },
      };
    };
  }

  /** Fail recovery capture at the fetch-back seam (BEFORE captureRecoveryRestorePoint's publish
   *  boundary, so it never runs a reconcile), keeping the hold loop looping until the cancel lands.
   *  The settle transfer AFTER the failed report needs the SAME real-git seam to SUCCEED, so it is
   *  re-allowed the instant the terminal report lands (flipped inside the combined onState hook).
   *  Returns a { flip } setter the hook calls and a `captureFailed` deferred that resolves once the
   *  loop has reached (and failed) a capture — the cue to inject the cancel. */
  function failCaptureUntilReport(): { armAllow: () => void; captureFailed: ReturnType<typeof deferred> } {
    let allowFetch = false;
    const realFetch = git.fetchAgentBranch.bind(git);
    const captureFailed = deferred();
    git.fetchAgentBranch = async (...args: Parameters<typeof realFetch>) => {
      if (allowFetch) return realFetch(...args);
      captureFailed.resolve();
      throw new Error("injected recovery-capture fetch failure (before publish)");
    };
    return { armAllow: () => { allowFetch = true; }, captureFailed };
  }

  /** CAVEAT 1: api.onState keeps ONE hook per run. This single combined hook records `failed` into
   *  the ordered event log, re-allows the settle fetch, and maps failed→cancelled (Service.SetState:
   *  the cancel input only stamps stop_kind; the worker's failed report performs CancelRunByWorker). */
  function armCancelMapping(runId: string, events: string[], armAllow: () => void): void {
    api.onState(runId, (body) => {
      if (body.status === "failed") {
        events.push("failed");
        armAllow();
      }
      const mapped = body.status === "failed" ? "cancelled" : body.status;
      api.setOwnershipStatus(runId, mapped);
      api.overrideStateStatus(runId, mapped);
    });
  }

  it("clean tree: reap (reconcile) runs BEFORE the cancel report, then the exact-generation release settles after it", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4711, { claim_generation: 31 });
    // The 409 model: the reconcile throws ONCE a terminal failed state has been recorded for the run.
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git: fakeGit, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-clean-home-"));
    try {
      fakeGit.alreadyPublished = true; // provably empty → RELEASE the exact hold
      const { armAllow, captureFailed } = failCaptureUntilReport();
      armCancelMapping(claim.run_id, events, armAllow);
      const runner = runnerWith(codexTransientFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
        recoveryRetryMs: 5,
      });
      const execution = runner.execute(claim);
      await Promise.race([
        captureFailed.promise,
        execution.then(() => { throw new Error("execution exited before reaching the cancel branch"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;

      assert.ok(
        api.states.some((s) => s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel is reported as a terminal failed(run cancelled)",
      );
      // The reap's per-sink reconcile ran while still actively-claimed — BEFORE the report — so it
      // was authorized (no 409); the settle then released AFTER the report.
      assert.ok(events.indexOf("reconcile") >= 0, "the reap boundary ran the per-sink reconcile");
      assert.equal(
        events.filter((e) => e === "reconcile").length,
        1,
        `capture must NOT reach its publish boundary (which would add a reconcile); got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.lastIndexOf("reconcile") < events.indexOf("failed"),
        `every reap reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("release"),
        `the settle release must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.deepEqual(client.releasedGenerations(), [31], "the release named the EXACT claim generation (31)");
      assert.equal(client.reserveCalls.length, 0, "a provably-empty cancel reserves nothing");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("committed tree: reap precedes the cancel report, then the generation-bound CAPTURE settles after it (never releases)", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4712, { claim_generation: 31 });
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git: fakeGit, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-commit-home-"));
    try {
      fakeGit.alreadyPublished = false; // unpublished committed work → CAPTURE
      const { armAllow, captureFailed } = failCaptureUntilReport();
      armCancelMapping(claim.run_id, events, armAllow);
      const runner = runnerWith(codexTransientFactory(homeRoot, safety, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
        recoveryRetryMs: 5,
      });
      const execution = runner.execute(claim);
      await Promise.race([
        captureFailed.promise,
        execution.then(() => { throw new Error("execution exited before reaching the cancel branch"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;

      assert.ok(hasStatus(claim.run_id, "failed"), "the cancel is reported as a terminal failed");
      assert.equal(
        events.filter((e) => e === "reconcile").length,
        1,
        `capture must NOT reach its publish boundary; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.lastIndexOf("reconcile") < events.indexOf("failed"),
        `every reap reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("reserve"),
        `the capture reserve must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.equal(client.reserveCalls.length, 1, "committed work is captured");
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("a blocked reap (contended reconcile) RETAINS the hold: no release/reserve, but the cancel is still reported", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4713, { claim_generation: 31 });
    // alwaysBlock: the reconcile throws even while the run is still `running`, so the pre-report reap
    // fails and the hold is retained (source_only) — but the cancel must still be reported.
    const { coord, client, git: fakeGit, safety, root } = fixture(events, () => true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-retain-home-"));
    try {
      fakeGit.alreadyPublished = true;
      const { armAllow, captureFailed } = failCaptureUntilReport();
      armCancelMapping(claim.run_id, events, armAllow);
      const runner = runnerWith(codexTransientFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
        recoveryRetryMs: 5,
      });
      const execution = runner.execute(claim);
      await Promise.race([
        captureFailed.promise,
        execution.then(() => { throw new Error("execution exited before reaching the cancel branch"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;

      assert.equal(client.releaseCalls.length, 0, "a blocked reap retains the hold — never releases");
      assert.equal(client.reserveCalls.length, 0, "a blocked reap retains the hold — never captures");
      assert.ok(
        api.states.some((s) => s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel is still reported (the hold is left open, not the report)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  // #1539 M1 COVERAGE GAP — the OTHER settle site in handleRecoveryExhausted: the terminal-ownership
  // EARLY-RETURN branch (`if (terminal.has(status)) { … if (cancelReap) settle }`). It fires when the
  // cancel report returns a STATUSLESS ack (so the ack-terminal branch is skipped), and the NEXT loop
  // turn reads a terminal `cancelled` ownership. That read runs the deferred custody settle the
  // pre-report reap earned. The status-bearing-ack cases above settle on the ack itself and never
  // reach this line. MUTATION control: deleting that `if (cancelReap) settle` line → no release here.
  it("statusless cancel ack + terminal ownership read: the early-return branch settles the exact-generation release", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4714, { claim_generation: 41 });
    // The 409 model: the reconcile throws ONCE a terminal failed state has been recorded for the run.
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git: fakeGit, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-statusless-home-"));
    try {
      fakeGit.alreadyPublished = true; // provably empty → RELEASE the exact hold
      const { armAllow, captureFailed } = failCaptureUntilReport();
      // CAVEAT: ONE combined hook (fake-api keeps one per run). On the `failed` report it records the
      // event, re-allows the settle fetch, arms a STATUSLESS ack for THIS report (so the ack-terminal
      // branch is skipped), and sets a terminal `cancelled` ownership so the NEXT loop turn reaches
      // the early-return settle branch. It does NOT overrideStateStatus to a terminal value — that
      // would settle on the ack instead and never exercise this line.
      api.onState(claim.run_id, (body) => {
        if (body.status === "failed") {
          events.push("failed");
          armAllow();
          api.omitStateAckStatus(claim.run_id);
          api.setOwnershipStatus(claim.run_id, "cancelled");
        }
      });
      const runner = runnerWith(codexTransientFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
        recoveryRetryMs: 5,
      });
      const execution = runner.execute(claim);
      await Promise.race([
        captureFailed.promise,
        execution.then(() => { throw new Error("execution exited before reaching the cancel branch"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;

      assert.ok(
        api.states.some((s) => s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel is reported as a terminal failed(run cancelled)",
      );
      assert.ok(events.indexOf("reconcile") >= 0, "the reap boundary ran the per-sink reconcile");
      assert.ok(
        events.lastIndexOf("reconcile") < events.indexOf("failed"),
        `every reap reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("release"),
        `the settle release must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.deepEqual(
        client.releasedGenerations(),
        [41],
        "the terminal-ownership early-return branch released the EXACT claim generation (41)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

// PRD #1349 M2 (F2/F4) / #1539 — the usage-LIMIT arm of executeClaim's catch must reap the Codex
// provider WHILE the run is still actively-claimed (BEFORE handleLimitReached reports anything),
// then settle AFTER the report on the non-parked branch. Same 409 hazard as #1531/#1539's other
// terminal sites: the Codex per-sink reconcile inside withBoundary is authorized ONLY while the run
// is actively-claimed; once handleLimitReached reports `failed` (or the server COERCES a park to
// `failed`) the reconcile is refused, and a settle-then-report ordering would block the reap and
// leak the exact-generation `source_only` hold. This reuses the SAME real Codex boundary + ordered
// event log, drives the executor to a LimitReachedError, and asserts the split order across all
// three non-parked outcomes (opt-out, server-declined park) plus the parked regression. MUTATION
// control: restore the terminal-first `reapThenSettleRecoveryGeneration(..., "terminal")` in the
// else and drop the pre-report reap → the 409-shaped reconcile blocks it → (a)-(c) redden.
describe("RunRunner — the usage-limit arm reaps BEFORE the terminal report (PRD #1349 M2 F2/F4 / #1539)", () => {
  /** A Codex-shaped executor (carries `.safety`, NO killAgentTree) that (optionally) commits, then
   *  throws a LimitReachedError — the usage-limit death handleLimitReached owns. */
  function codexLimitFactory(homeRoot: string, safety: CodexExecutionSafety, commit: boolean): ExecutorFactory {
    return (runId) => {
      const runHome = path.join(homeRoot, runId);
      return {
        homeDir: runHome,
        executor: {
          safety,
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(runHome, { recursive: true });
            if (commit) commitInTree(ctx.worktreePath, "WORK.txt", "committed before the limit\n");
            throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          },
        },
      };
    };
  }

  /** ONE hook per run (fake-api keeps one): record the terminal-ish report boundary into the event
   *  log. The opt-out reports `failed`; a wait_on_limit park reports `limit_wait` (the server may
   *  then coerce its ACK to `failed`). Both are pushed so a case asserts against its own boundary. */
  function armLimitEvents(runId: string, events: string[]): void {
    api.onState(runId, (body) => {
      if (body.status === "failed") events.push("failed");
      if (body.status === "limit_wait") events.push("limit_wait");
    });
  }

  it("(a) wait_on_limit=false, clean tree: reap (reconcile) precedes the failed report, then the exact-generation release settles after it", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4801, { claim_generation: 51, wait_on_limit: false });
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-limit-optout-clean-"));
    try {
      git.alreadyPublished = true; // provably empty → RELEASE the exact hold
      armLimitEvents(claim.run_id, events);
      await runnerWith(codexLimitFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "an opt-out usage-limit hit reports failed");
      assert.ok(events.indexOf("reconcile") >= 0, "the reap boundary ran the per-sink reconcile");
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("failed"),
        `reap-reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("release"),
        `the settle release must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.deepEqual(client.releasedGenerations(), [51], "the release named the EXACT claim generation (51)");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("(b) wait_on_limit=false, committed work: reap precedes the failed report, then the generation-bound CAPTURE settles after it (never releases)", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4802, { claim_generation: 52, wait_on_limit: false });
    const decideThrow = (): boolean =>
      api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed");
    const { coord, client, git, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-limit-optout-commit-"));
    try {
      git.alreadyPublished = false; // unpublished committed work → CAPTURE
      armLimitEvents(claim.run_id, events);
      await runnerWith(codexLimitFactory(homeRoot, safety, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"));
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("failed"),
        `reap-reconcile must precede the failed report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("failed") < events.indexOf("reserve"),
        `the capture reserve must run after the failed report; got ${JSON.stringify(events)}`,
      );
      assert.equal(client.reserveCalls.length, 1, "committed work is captured");
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("(c) wait_on_limit=true but the server DECLINED the park (ack coerced to failed), clean tree: reap precedes the report, then the exact-generation release settles after it", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4803, { claim_generation: 53, wait_on_limit: true });
    // The server coerces the limit_wait ACK to `failed`; the reported BODY stays limit_wait, so the
    // 409 model keys on that recorded report — the reap (before it) is authorized, a reap after it
    // (the mutation) is refused.
    const decideThrow = (): boolean =>
      api.states.some(
        (s) => s.runId === claim.run_id && (s.body.status === "limit_wait" || s.body.status === "failed"),
      );
    const { coord, client, git, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-limit-declined-clean-"));
    try {
      git.alreadyPublished = true; // provably empty → RELEASE the exact hold
      armLimitEvents(claim.run_id, events);
      // 200 + "failed": the park request is acknowledged with a status that is NOT limit_wait, so
      // handleLimitReached returns parked=false while wait_on_limit stays true (the server-coerced fail).
      api.overrideStateStatus(claim.run_id, "failed");
      await runnerWith(codexLimitFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "limit_wait"), "the worker reported a limit_wait park (the server declined it)");
      assert.ok(events.indexOf("reconcile") >= 0, "the reap boundary ran the per-sink reconcile");
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("limit_wait"),
        `reap-reconcile must precede the (declined) park report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("limit_wait") < events.indexOf("release"),
        `the settle release must run after the report; got ${JSON.stringify(events)}`,
      );
      assert.deepEqual(client.releasedGenerations(), [53], "the release named the EXACT claim generation (53)");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("(c') wait_on_limit=true declined park, committed work: reap precedes the report, then the generation-bound CAPTURE settles after it", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4804, { claim_generation: 54, wait_on_limit: true });
    const decideThrow = (): boolean =>
      api.states.some(
        (s) => s.runId === claim.run_id && (s.body.status === "limit_wait" || s.body.status === "failed"),
      );
    const { coord, client, git, safety, root } = fixture(events, decideThrow);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-limit-declined-commit-"));
    try {
      git.alreadyPublished = false; // unpublished committed work → CAPTURE
      armLimitEvents(claim.run_id, events);
      api.overrideStateStatus(claim.run_id, "failed");
      await runnerWith(codexLimitFactory(homeRoot, safety, true), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(
        events.indexOf("reconcile") < events.indexOf("limit_wait"),
        `reap-reconcile must precede the (declined) park report; got ${JSON.stringify(events)}`,
      );
      assert.ok(
        events.indexOf("limit_wait") < events.indexOf("reserve"),
        `the capture reserve must run after the report; got ${JSON.stringify(events)}`,
      );
      assert.equal(client.reserveCalls.length, 1, "committed work is captured");
      assert.equal(client.releaseCalls.length, 0, "committed work is NEVER released");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("(d) a blocked pre-report reap (contended reconcile), wait_on_limit=false: no release/reserve, but the failure is still reported", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4805, { claim_generation: 55, wait_on_limit: false });
    // alwaysBlock: the reconcile throws even while the run is still `running`, so the pre-report reap
    // fails and the hold is retained (source_only) — but the run must still report its failure, and
    // there is NO post-report retry (a blocked reap poisons the registry stickily).
    const { coord, client, git, safety, root } = fixture(events, () => true);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-limit-blocked-"));
    try {
      git.alreadyPublished = true;
      armLimitEvents(claim.run_id, events);
      await runnerWith(codexLimitFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.equal(client.releaseCalls.length, 0, "a blocked reap retains the hold — never releases");
      assert.equal(client.reserveCalls.length, 0, "a blocked reap retains the hold — never captures");
      assert.ok(
        hasStatus(claim.run_id, "failed"),
        "the terminal failure is still reported (the hold is left open, not the report)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("(e) parked regression: wait_on_limit=true, server ACKs the park — the run parks and the park-path settle still runs", async () => {
    const { gitlab } = fakeGitlab();
    const events: string[] = [];
    const claim = gitlabClaim(4806, { claim_generation: 56, wait_on_limit: true });
    // A genuine park: the park-boundary reconcile (and the settle after it) must SUCCEED, so the
    // reconcile is never blocked — limit_wait is actively-claimed, so its reconcile is authorized.
    const { coord, client, git, safety, root } = fixture(events, () => false);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1539-limit-park-"));
    try {
      git.alreadyPublished = true; // provably empty → the park settle RELEASES the exact hold
      armLimitEvents(claim.run_id, events);
      await runnerWith(codexLimitFactory(homeRoot, safety, false), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "limit_wait"), "the run parked (limit_wait)");
      // The pre-report reap ran a reconcile AND the park boundary ran its own — two in all. Assert
      // only the robust facts: at least the two reconciles, and the park-path settle released.
      assert.equal(
        events.filter((e) => e === "reconcile").length,
        2,
        `both the pre-report reap and the park boundary reconcile; got ${JSON.stringify(events)}`,
      );
      assert.deepEqual(client.releasedGenerations(), [56], "the park-path settle released the EXACT claim generation (56)");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});
