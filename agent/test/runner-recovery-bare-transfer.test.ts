import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import {
  RecoveryCoordinator,
  type RecoveryArchiveClient,
  type RecoveryBundleProducer,
} from "../src/recovery.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryHold,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
} from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  client,
  deferred,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// PRD #1349 M2 (D4.5) — settleRecoveryGeneration must TRANSFER the run's exact clone-only
// restore-point head into the TRUSTED worker bare (fetchAgentBranch → verify → trackingTip) BEFORE
// bundle production reads it, then pin+capture THAT verified-present SHA. Exercised with the REAL
// GitCache as the recovery bundle producer (a FAKE producer cannot surface the clone-only-vs-bare
// split these tests hinge on): the runner's own `this.git` and the coordinator's producer are the
// SAME live GitCache, so a settle that skips the transfer feeds produceRecoveryBundle a commit
// absent from the bare and trips its trusted-bare guard (bundle_failed / no upload / lost clone) —
// exactly what the assertions below RED-detect (see the mutation control in the run instructions).

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const WORKER_TOKEN = "bare-xfer-worker-join-token-0123456789";

/** Commit `content` into `file` under the runner clone (a clone-only head — no bare ref advances). */
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

/** Read from a git dir (e.g. the trusted bare), trimming the trailing newline. */
function gitRead(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: GIT_ENV,
    encoding: "utf8",
    stdio: "pipe",
  }).trim();
}

// ── the fake archive client (copied from runner-recovery-generation.test.ts) ─────────

class FakeRecoveryClient implements RecoveryArchiveClient {
  reserveCalls: RecoveryReserveRequest[] = [];
  uploadCalls: Array<{ captureId: string; manifest: RecoveryUploadManifest }> = [];
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
}

/** A coordinator over `root`, injected with the REAL GitCache as its bundle producer (the whole
 *  point — the coordinator's producer is the same live `git` the runner uses). */
function coordOver(root: string, fakeClient: FakeRecoveryClient): RecoveryCoordinator {
  const producer: RecoveryBundleProducer = git; // GitCache satisfies RecoveryBundleProducer
  return new RecoveryCoordinator({
    client: fakeClient,
    git: producer,
    log: nullLogger(),
    recoveryRoot: root,
    workerToken: WORKER_TOKEN,
    now: () => 1_700_000_000_000,
  });
}

function makeCoord(): { coord: RecoveryCoordinator; fakeClient: FakeRecoveryClient; root: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-"));
  const fakeClient = new FakeRecoveryClient();
  return { coord: coordOver(root, fakeClient), fakeClient, root };
}

/** Resolves once `signal` aborts. */
function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

/** Commit a clone-only head, then throw a PLAIN error — an early live-worker failure that unwinds
 *  through reportGenericFailure → reapThenSettleRecoveryGeneration → settleRecoveryGeneration. */
function commitThenFailFactory(homeRoot: string): ExecutorFactory {
  return (runId) => {
    const runHome = path.join(homeRoot, runId);
    return {
      homeDir: runHome,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.mkdirSync(runHome, { recursive: true });
          commitInTree(ctx.worktreePath, "WORK.txt", "committed clone-only work\n");
          throw new Error("agent failed hard");
        },
      },
    };
  };
}

const hasStatus = (runId: string, status: string): boolean =>
  api.states.some((s) => s.runId === runId && s.body.status === status);

describe("RunRunner — settle transfers the clone-only head into the trusted bare (PRD #1349 M2, D4.5)", () => {
  it("1. non-cancellation early failure with a committed clone-only head CAPTURES via the bare transfer", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-fail-"));
    try {
      const iid = 5101;
      const claim = gitlabClaim(iid, { claim_generation: 7 });
      await runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.equal(fakeClient.reserveCalls.length, 1, "the committed clone-only head is reserved");
      assert.equal(fakeClient.reserveCalls[0]!.generation, claim.claim_generation, "the reserve binds the exact generation");
      assert.equal(fakeClient.uploadCalls.length, 1, "the generation-bound bundle is uploaded");
      assert.equal(fakeClient.releaseCalls.length, 0, "committed work is NEVER released");
      // The exact clone-only head was transferred into the trusted bare BEFORE bundle production.
      const bare = git.barePathFor(fx.originPath);
      assert.equal(
        gitRead(bare, "show", `refs/uzi-runner/agent/issue-${iid}:WORK.txt`),
        "committed clone-only work",
        "the committed head is present in the trusted bare tracking ref",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("2. ordinary owner-cancellation early exit (via reportGenericFailure) CAPTURES the committed clone-only head", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-cancel-"));
    try {
      const iid = 5102;
      const claim = gitlabClaim(iid, { claim_generation: 8 });
      // Mirror Service.SetState: the cancel input stamped stop_kind='cancelled', so the worker's
      // subsequent `failed` report maps to the authoritative `cancelled`.
      let stopRequested = false;
      api.onState(claim.run_id, (body) => {
        const status = body.status === "failed" && stopRequested ? "cancelled" : body.status;
        api.setOwnershipStatus(claim.run_id, status);
        api.overrideStateStatus(claim.run_id, status);
      });
      const start = deferred();
      const factory: ExecutorFactory = (runId) => {
        const runHome = path.join(homeRoot, runId);
        return {
          homeDir: runHome,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.mkdirSync(runHome, { recursive: true });
              commitInTree(ctx.worktreePath, "WORK.txt", "committed clone-only work\n");
              start.resolve();
              // The cancel input aborts ctx.signal; then throw a PLAIN error (NOT
              // TransientRecoveryError, which would enter the handleRecoveryExhausted loop and miss
              // the early-cancel path). This unwinds through reportGenericFailure.
              await waitAbort(ctx.signal!);
              throw new Error("aborted by owner cancel");
            },
          },
        };
      };
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord });
      const execution = runner.execute(claim);
      await start.promise;
      stopRequested = true;
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assert.equal(
        (await client.getRunOwnership(claim.run_id)).status,
        "cancelled",
        "the authoritative run status is cancelled",
      );
      assert.equal(fakeClient.reserveCalls.length, 1, "the committed clone-only head is reserved");
      assert.equal(fakeClient.reserveCalls[0]!.generation, claim.claim_generation, "the reserve binds the exact generation");
      assert.equal(fakeClient.uploadCalls.length, 1, "the generation-bound bundle is uploaded");
      assert.equal(fakeClient.releaseCalls.length, 0, "committed work is NEVER released");
      const bare = git.barePathFor(fx.originPath);
      assert.equal(
        gitRead(bare, "show", `refs/uzi-runner/agent/issue-${iid}:WORK.txt`),
        "committed clone-only work",
        "the committed head is present in the trusted bare tracking ref",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("3. transfer/verification failure PRESERVES the source clone and keeps the hold open", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-preserve-"));
    const originalFetch = git.fetchAgentBranch.bind(git);
    try {
      const iid = 5103;
      const claim = gitlabClaim(iid, { claim_generation: 9 });
      // The transfer's fetch-back into the trusted bare fails: no durable replacement exists there,
      // so the runner clone is the only recoverable source and must survive cleanup.
      git.fetchAgentBranch = async () => {
        throw new Error("injected fetch-back failure");
      };
      await runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.equal(fakeClient.reserveCalls.length, 0, "no bundle production on a failed transfer");
      assert.equal(fakeClient.uploadCalls.length, 0, "nothing uploaded on a failed transfer");
      assert.equal(fakeClient.releaseCalls.length, 0, "never a fabricated release");
      assert.equal(
        fs.existsSync(worktreeDirFor(iid)),
        true,
        "the runner clone (the only recoverable source) SURVIVES cleanup",
      );
    } finally {
      git.fetchAgentBranch = originalFetch;
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("4. capture-failure discriminator: an upload failure AFTER a verified transfer retains the covered bare ref (needs_action)", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-uploadfail-"));
    try {
      const iid = 5104;
      fakeClient.uploadShouldThrow = true;
      const claim = gitlabClaim(iid, { claim_generation: 11 });
      await runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.equal(fakeClient.reserveCalls.length, 1, "the verified transfer produced a bundle and reserved");
      assert.equal(fakeClient.uploadCalls.length, 1, "the upload was attempted");
      assert.equal(fakeClient.releaseCalls.length, 0, "an upload failure NEVER releases");
      const recs = await coord.inspect(claim.run_id);
      assert.equal(recs.length, 1);
      assert.equal(recs[0]!.state, "needs_action", "the open hold + local source are retained");
      // The covered bare tracking ref is the retained source, so the whole clone need not persist.
      const bare = git.barePathFor(fx.originPath);
      assert.equal(
        gitRead(bare, "show", `refs/uzi-runner/agent/issue-${iid}:WORK.txt`),
        "committed clone-only work",
        "the committed head remains present in the trusted bare tracking ref",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("5. a fresh coordinator re-uploads the journaled bundle on restart (agent-side real-bundle replay)", async () => {
    const { gitlab } = fakeGitlab();
    // First coordinator: the upload fails AFTER a verified transfer, leaving a journaled bundle on
    // disk (state needs_action, bundle bytes retained) under `root`.
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-restart-"));
    try {
      const iid = 5105;
      fakeClient.uploadShouldThrow = true;
      const claim = gitlabClaim(iid, { claim_generation: 12 });
      await runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.equal(fakeClient.uploadCalls.length, 1, "the first upload was attempted and failed");
      const before = await coord.inspect(claim.run_id);
      assert.equal(before[0]!.state, "needs_action", "the first attempt left a journaled bundle");

      // A FRESH coordinator over the SAME recoveryRoot + SAME worker token (so the MAC
      // authenticates), with a NEW client whose upload succeeds. resumePending re-uploads the
      // journaled bundle BYTES with no forge PAT — this validates the AGENT-side replay logic
      // against a fake server. The real server's authorization of a post-terminal reservation
      // against the open exact-generation hold is the separately-tested contract ReserveCaptureExact
      // in api/internal/store/queries/recovery.sql:276-300 (no runs.status gate; the original worker
      // services its open exact-generation hold).
      const freshClient = new FakeRecoveryClient();
      const newCoord = coordOver(root, freshClient);
      await newCoord.resumePending();
      assert.equal(freshClient.uploadCalls.length, 1, "the journaled bundle bytes were re-uploaded");
      const after = await newCoord.inspect(claim.run_id);
      assert.equal(after.length, 1);
      assert.equal(after[0]!.state, "uploaded", "the record did NOT degrade to incomplete_local_inputs");
      assert.notEqual(after[0]!.reason, "incomplete_local_inputs");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("6. positive-verify failure (fetch reports success, tracking ref does NOT cover HEAD) PRESERVES the source clone and keeps the hold open", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-verifyfail-"));
    const originalVerify = git.verifyRunnerTrackingCovers.bind(git);
    try {
      const iid = 5106;
      const claim = gitlabClaim(iid, { claim_generation: 13 });
      // Model a partial-fetch / race: fetchAgentBranch is REAL and succeeds, but the positive verify
      // reports the bare's tracking ref does NOT cover the run HEAD. Every other case in this file has
      // the fetch genuinely cover HEAD, so a verify that passes for real and a verify that is bypassed
      // outright are otherwise indistinguishable — this is the mutation guard for that gap.
      git.verifyRunnerTrackingCovers = async () => false;
      await runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.equal(fakeClient.reserveCalls.length, 0, "no bundle production on an unverified transfer");
      assert.equal(fakeClient.uploadCalls.length, 0, "nothing uploaded on an unverified transfer");
      assert.equal(fakeClient.releaseCalls.length, 0, "never a fabricated release");
      assert.equal(
        fs.existsSync(worktreeDirFor(iid)),
        true,
        "the runner clone (the only recoverable source) SURVIVES cleanup",
      );
    } finally {
      git.verifyRunnerTrackingCovers = originalVerify;
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("7. a dirty tree at settle time is WIP-committed by commitWipMarker before the transfer, and the marker reaches the trusted bare", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-dirty-"));
    try {
      const iid = 5107;
      const claim = gitlabClaim(iid, { claim_generation: 14 });
      const factory: ExecutorFactory = (runId) => {
        const runHome = path.join(homeRoot, runId);
        return {
          homeDir: runHome,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.mkdirSync(runHome, { recursive: true });
              // UNCOMMITTED work — no `git commit`, so worktreeStatus reports the tree dirty and the
              // helper takes the commitWipMarker branch every other case here leaves unexercised.
              fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "uncommitted wip content\n");
              throw new Error("agent failed hard");
            },
          },
        };
      };
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.equal(fakeClient.reserveCalls.length, 1, "the WIP-committed head is reserved");
      assert.equal(fakeClient.reserveCalls[0]!.generation, claim.claim_generation, "the reserve binds the exact generation");
      assert.equal(fakeClient.uploadCalls.length, 1, "the generation-bound bundle is uploaded");
      assert.equal(fakeClient.releaseCalls.length, 0, "committed work is NEVER released");
      const bare = git.barePathFor(fx.originPath);
      assert.equal(
        gitRead(bare, "show", `refs/uzi-runner/agent/issue-${iid}:WIP.txt`),
        "uncommitted wip content",
        "the WIP-marker-committed content is present in the trusted bare tracking ref",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("8. commitWipMarker returning false on a dirty tree PRESERVES the source clone and keeps the hold open", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-wipfail-"));
    const originalCommitWip = git.commitWipMarker.bind(git);
    try {
      const iid = 5108;
      const claim = gitlabClaim(iid, { claim_generation: 15 });
      // The helper REQUIRES commitWipMarker to succeed on a dirty tree (an ambiguous `false` there is
      // treated as a commit FAILURE, since the tree is already known dirty) — model that failure.
      git.commitWipMarker = async () => false;
      const factory: ExecutorFactory = (runId) => {
        const runHome = path.join(homeRoot, runId);
        return {
          homeDir: runHome,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.mkdirSync(runHome, { recursive: true });
              fs.writeFileSync(path.join(ctx.worktreePath, "WIP.txt"), "uncommitted wip content\n");
              throw new Error("agent failed hard");
            },
          },
        };
      };
      await runnerWith(factory, gitlab, undefined, nullLogger(), { recovery: coord }).execute(claim);
      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.equal(fakeClient.reserveCalls.length, 0, "no bundle production when the WIP commit failed");
      assert.equal(fakeClient.uploadCalls.length, 0, "nothing uploaded when the WIP commit failed");
      assert.equal(fakeClient.releaseCalls.length, 0, "never a fabricated release");
      assert.equal(
        fs.existsSync(worktreeDirFor(iid)),
        true,
        "the runner clone (the only recoverable source) SURVIVES cleanup",
      );
    } finally {
      git.commitWipMarker = originalCommitWip;
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("9. a concurrent run moving the shared tracking ref between verify and retrieval never pins the foreign head, and the verified head stays durable through a prune (issue #1507)", async () => {
    const { gitlab } = fakeGitlab();
    const { coord, fakeClient, root } = makeCoord();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-bare-xfer-toctou-"));
    const originalVerify = git.verifyRunnerTrackingCovers.bind(git);
    const originalProduce = git.produceRecoveryBundle.bind(git);
    try {
      const iid = 5109;
      const branch = `agent/issue-${iid}`;
      const trackingRef = `refs/uzi-runner/${branch}`;
      const claim = gitlabClaim(iid, { claim_generation: 21 });
      const bare = git.barePathFor(fx.originPath);
      let ourHead = "";
      let foreignSha = "";

      // After the REAL positive-verify passes, model a concurrent run on the same bare+branch:
      // capture THIS run's verified head, mint a FOREIGN root commit (ourHead is NOT its ancestor —
      // it reuses ourHead's tree but has no parent, so ourHead becomes unreachable once the ref
      // moves), and move the shared tracking ref to it. The UNFIXED reread would then pin foreignSha.
      git.verifyRunnerTrackingCovers = async (bp, wt, br) => {
        const verified = await originalVerify(bp, wt, br);
        if (verified && br === branch && !foreignSha) {
          ourHead = gitRead(bare, "rev-parse", trackingRef);
          const ourTree = gitRead(bare, "rev-parse", `${ourHead}^{tree}`);
          foreignSha = gitRead(
            bare,
            "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false",
            "commit-tree", ourTree, "-m", "foreign concurrent run",
          );
          gitRead(bare, "update-ref", trackingRef, foreignSha);
        }
        return verified;
      };

      // Model a maintenance/prune pass in the settle→produce window. Reflogs are off on a --bare
      // clone, so once the tracking ref points at the foreign root commit, OUR head is reachable
      // ONLY via the run+generation pin the fix plants — gc --prune=now removes it otherwise, and
      // the real produceRecoveryBundle's rev-parse then fails (fixed-but-no-pin RED).
      git.produceRecoveryBundle = async (bp, opts) => {
        gitRead(bare, "gc", "--prune=now");
        return originalProduce(bp, opts);
      };

      await runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), {
        recovery: coord,
      }).execute(claim);

      assert.ok(hasStatus(claim.run_id, "failed"), "the run reported failed");
      assert.ok(ourHead && foreignSha && ourHead !== foreignSha, "the injection captured two distinct heads");
      assert.equal(fakeClient.reserveCalls.length, 1, "the verified head is reserved");
      assert.equal(
        fakeClient.reserveCalls[0]!.source_sha,
        ourHead,
        "the pinned/reserved source is THIS run's verified head, never the concurrently-moved foreign head",
      );
      assert.notEqual(fakeClient.reserveCalls[0]!.source_sha, foreignSha, "the foreign head is never pinned");
      assert.equal(fakeClient.reserveCalls[0]!.generation, claim.claim_generation, "the reserve binds the exact generation");
      assert.equal(fakeClient.uploadCalls.length, 1, "the generation-bound bundle is uploaded (our head survived the prune)");
      assert.equal(fakeClient.releaseCalls.length, 0, "committed work is NEVER released");
      // The verified head is still a reachable commit in the bare after settle — the durable pin held
      // it through the prune, and the foreign move never displaced it as the pinned source.
      assert.equal(gitRead(bare, "cat-file", "-t", ourHead), "commit", "the verified head survives as a commit");
    } finally {
      git.verifyRunnerTrackingCovers = originalVerify;
      git.produceRecoveryBundle = originalProduce;
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});
