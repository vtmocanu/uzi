import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory, type RunRunner } from "../src/runner.js";
import { RecoveryCoordinator, type RecoveryBundleProducer } from "../src/recovery.js";
import { nullLogger } from "./helpers.js";
import { fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

installHarness();
resetResidueQuarantineAfterEach();

// issue #2263 — the RUNNER side of the archived-pin contract, through the real settleRecoveryGeneration
// with the real GitCache as the runner's git. An archive upload never deletes the pin from the runner
// (the recovery coordinator does, once the run is not executing); only the already-published outcome
// deletes it here, and only with the pinned head as the expected SHA.

// Inherited repository-location variables (a git hook exports them) override `-C` and would point
// these fixture commands, update-ref included, at the caller's repository; drop them.
const GIT_ENV: NodeJS.ProcessEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
for (const k of ["GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
  "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_PREFIX"]) delete GIT_ENV[k];
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const WORKER_TOKEN = "issue-2263-runner-worker-token-0123456789";
const rd = (cwd: string, ...args: string[]) =>
  execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
const present = (bare: string, ref: string) => { try { return rd(bare, "rev-parse", "--verify", ref); } catch { return undefined; } };

function commitInTree(treePath: string, file: string, content: string): string {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], { env: GIT_ENV, stdio: "pipe" });
  return rd(treePath, "rev-parse", "HEAD");
}
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

interface Spy { calls: unknown[][]; restore: () => void }
function spyOnDelete(): Spy {
  const original = git.deleteRecoveryPin.bind(git);
  const calls: unknown[][] = [];
  git.deleteRecoveryPin = (async (...args: Parameters<typeof original>) => { calls.push(args); return original(...args); }) as typeof git.deleteRecoveryPin;
  return { calls, restore: () => { git.deleteRecoveryPin = original; } };
}

function setup(producer: RecoveryBundleProducer, runnerRef: { current?: RunRunner }) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "issue-2263-runner-"));
  fs.chmodSync(root, 0o700);
  const client = new FakeRecoveryClient();
  const log = nullLogger();
  const retained: string[] = [];
  log.warn = (message, fields) => { if (message === "recovery: uploaded record pin retained") retained.push((fields as { reason: string }).reason); };
  log.info = (message, fields) => { if (message === "recovery: uploaded record pin retained") retained.push((fields as { reason: string }).reason); };
  const coord = new RecoveryCoordinator({
    client, git: producer, log, recoveryRoot: root, workerToken: WORKER_TOKEN, now: () => 1_700_000_000_000,
    // The real wiring: a run is executing while the runner still holds it.
    isExecuting: (runId) => runnerRef.current?.isExecuting(runId) ?? false,
  });
  return { root, client, coord, retained };
}

describe("RunRunner — archived recovery pin ownership (issue #2263)", () => {
  it("an archive upload never deletes the pin from the runner; the coordinator keeps it while executing and removes it afterwards", async () => {
    const { gitlab } = fakeGitlab();
    const runnerRef: { current?: RunRunner } = {};
    // No forge credentials for the default-tip fetch: the real bundle is then self-contained, so the archive proves the head.
    const originalFetchTip = git.fetchDefaultTip.bind(git);
    git.fetchDefaultTip = async () => { throw new Error("no forge credentials"); };
    const { root, client, coord, retained } = setup(git, runnerRef);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "issue-2263-runner-home-"));
    const spy = spyOnDelete();
    try {
      const iid = 6301;
      const claim = gitlabClaim(iid, { claim_generation: 7 });
      runnerRef.current = runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), { recovery: coord });
      await runnerRef.current.execute(claim);
      assert.equal(client.uploadCalls.length, 1, "the bundle was archived");
      assert.equal(client.releaseCalls.length, 0);
      const bare = git.barePathFor(fx.originPath);
      const pinRef = `refs/uzi-recovery-pin/${claim.run_id}/7`;
      assert.deepEqual(spy.calls, [], "the runner did not delete the pin after an archive upload");
      assert.equal(present(bare, pinRef), rd(bare, "rev-parse", `refs/uzi-runner/agent/issue-${iid}`), "the pin survives the settle");
      assert.deepEqual(retained, ["executing"], "the coordinator refused because the run was still executing");
      // The run is over: a boot/live pass owns the deletion.
      await coord.resumePending();
      assert.equal(present(bare, pinRef), undefined, "the coordinator removes the pin once the run is not executing");
      assert.deepEqual(spy.calls, [], "and still not through the runner");
    } finally {
      spy.restore();
      git.fetchDefaultTip = originalFetchTip;
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  function publishedProducer(onProduce?: (sourceSha: string) => void): RecoveryBundleProducer {
    return {
      fetchDefaultTip: async () => "3".repeat(40),
      resolveRecoveryBareDir: git.resolveRecoveryBareDir.bind(git),
      produceRecoveryBundle: async (_bare, opts) => {
        onProduce?.(opts.sourceSha);
        return { bundlePath: "", byteSize: 0, checksum: "", chunkCount: 0, prerequisiteShas: [], sourceSha: opts.sourceSha,
          selfContained: false, alreadyPublished: true };
      },
    };
  }

  it("the already-published outcome deletes the pin with the pinned head as the expected SHA", async () => {
    const { gitlab } = fakeGitlab();
    const runnerRef: { current?: RunRunner } = {};
    const { root, client, coord } = setup(publishedProducer(), runnerRef);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "issue-2263-runner-home-"));
    const spy = spyOnDelete();
    try {
      const iid = 6302;
      const claim = gitlabClaim(iid, { claim_generation: 8 });
      runnerRef.current = runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), { recovery: coord });
      await runnerRef.current.execute(claim);
      const bare = git.barePathFor(fx.originPath);
      const head = rd(bare, "rev-parse", `refs/uzi-runner/agent/issue-${iid}`);
      assert.equal(client.uploadCalls.length, 0, "nothing archived: the head is already published");
      assert.equal(spy.calls.length, 1);
      assert.deepEqual(spy.calls[0], [bare, claim.run_id, 8, head], "4-argument compare-and-delete naming the pinned head");
      assert.equal(present(bare, `refs/uzi-recovery-pin/${claim.run_id}/8`), undefined);
    } finally {
      spy.restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("the already-published delete retains a pin whose ref has moved", async () => {
    const { gitlab } = fakeGitlab();
    const runnerRef: { current?: RunRunner } = {};
    const iid = 6303;
    const claim = gitlabClaim(iid, { claim_generation: 9 });
    let moved: string | undefined;
    const bare = () => git.barePathFor(fx.originPath);
    const { root, coord } = setup(publishedProducer((sourceSha) => {
      // Between the pin and the runner's delete, the pin is re-pointed at the head's parent.
      moved = rd(bare(), "rev-parse", sourceSha + "^");
      rd(bare(), "update-ref", `refs/uzi-recovery-pin/${claim.run_id}/9`, moved);
    }), runnerRef);
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "issue-2263-runner-home-"));
    const spy = spyOnDelete();
    try {
      runnerRef.current = runnerWith(commitThenFailFactory(homeRoot), gitlab, undefined, nullLogger(), { recovery: coord });
      await runnerRef.current.execute(claim);
      assert.ok(moved, "the producer ran and moved the pin");
      assert.equal(spy.calls.length, 1, "the runner attempted the delete");
      assert.equal(present(bare(), `refs/uzi-recovery-pin/${claim.run_id}/9`), moved, "the moved pin is left as found");
    } finally {
      spy.restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});
