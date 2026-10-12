import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache } from "../src/git.js";
import type { Logger } from "../src/log.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import { nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  homeDir,
  installHarness,
  isAlive,
  runnerWith,
  waitDead,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

// issue #1786 M1 — a flight that learns its claim is stale (a /state stale_claim ack, a steering
// claim fence, or the first running report's terminal refusal) must keep its clone's bytes: the
// new claim owns the RUN, not this worker's unpublished source. Real GitCache, real Git; the
// executor fake writes an unfetched commit, a modified tracked binary (new bytes and mode) and
// an untracked file into the runner clone before the claim goes stale.

const DOCKER = "unix:///nonexistent/uzi-test-docker.sock";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BIN_V1 = Buffer.from([0x00, 0x01, 0x02, 0xff, 0xfe, 0x00, 0x10, 0x80]);
const BIN_V2 = Buffer.from([0xde, 0xad, 0x00, 0xbe, 0xef, 0x00, 0xff, 0x7f, 0x00]);
const UNTRACKED = "only on this worker\n";
const OPEN_HOLD = "11111111-0000-4000-8000-00000000b001";

function gitIn(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function cfgAll(bare: string, key: string): string[] {
  try {
    return gitIn(bare, "config", "--local", "--get-all", key).split("\n").filter((l) => l !== "");
  } catch {
    return [];
  }
}
function journalRaw(bare: string, branch: string): string {
  return cfgAll(bare, `uzi-recovery.${branch}.clone`).join("\n");
}
function ledgerStates(bare: string, branch: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const raw of cfgAll(bare, `uzi-attempts.${branch}.entry`)) {
    const e = JSON.parse(raw) as { attemptId: string; state: string };
    out.set(e.attemptId, e.state);
  }
  return out;
}
function markers(bare: string, runId: string): string[] {
  return cfgAll(bare, `uzi-stopped.${runId}.entry`);
}
function reportsFor(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status as string);
}
const attemptIdOf = (clonePath: string): string => path.basename(clonePath).split(".attempt-")[1]!;

type Trigger = "stale" | "fence";
type Flavor = "canonical" | "attempt";

interface Facts {
  clonePath: string;
  commit: string;
  baseHead: string;
}

/** Write the three kinds of worker-only state into `wt`. */
function writeWorkerOnlyState(wt: string, opts: { cleanTree?: boolean; seedOnly?: "clean" | "untracked" } = {}): Facts {
  const baseHead = gitIn(wt, "rev-parse", "HEAD");
  if (opts.seedOnly) {
    // A flight stopped before it committed anything: HEAD is still the seed base.
    if (opts.seedOnly === "untracked") fs.writeFileSync(path.join(wt, "untracked.txt"), UNTRACKED);
    return { clonePath: wt, commit: baseHead, baseHead };
  }
  fs.mkdirSync(path.join(wt, "assets"), { recursive: true });
  fs.writeFileSync(path.join(wt, "assets", "blob.bin"), BIN_V1);
  gitIn(wt, "add", "assets/blob.bin");
  gitIn(wt, ...IDENT, "commit", "-q", "-m", "unfetched work");
  const commit = gitIn(wt, "rev-parse", "HEAD");
  if (opts.cleanTree) return { clonePath: wt, commit, baseHead };
  fs.writeFileSync(path.join(wt, "assets", "blob.bin"), BIN_V2);
  fs.chmodSync(path.join(wt, "assets", "blob.bin"), 0o755);
  fs.writeFileSync(path.join(wt, "untracked.txt"), UNTRACKED);
  return { clonePath: wt, commit, baseHead };
}

/** Everything a recovering operator needs, rebuilt from a byte copy of the retained clone. */
function assertRecoverableFromExport(facts: Facts, label: string): void {
  assert.ok(fs.existsSync(facts.clonePath), `${label}: the clone is retained`);
  const exportDir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "uzi-stopped-export-"));
  try {
    const copy = path.join(exportDir, "copy");
    fs.cpSync(facts.clonePath, copy, { recursive: true, verbatimSymlinks: true });
    assert.equal(gitIn(copy, "rev-parse", "HEAD"), facts.commit, `${label}: the unfetched commit is the exported HEAD`);
    gitIn(copy, "cat-file", "-e", `${facts.commit}^{commit}`);
    const archive = path.join(exportDir, "head.tar");
    execFileSync("git", ["-C", copy, "archive", "-o", archive, "HEAD"], { env: GIT_ENV, stdio: "pipe" });
    const unpack = path.join(exportDir, "head");
    fs.mkdirSync(unpack);
    execFileSync("tar", ["-xf", archive, "-C", unpack], { stdio: "pipe" });
    assert.deepEqual(fs.readFileSync(path.join(unpack, "assets", "blob.bin")), BIN_V1, `${label}: the committed binary`);
    assert.deepEqual(fs.readFileSync(path.join(copy, "assets", "blob.bin")), BIN_V2, `${label}: the modified binary's bytes`);
    assert.equal(fs.statSync(path.join(copy, "assets", "blob.bin")).mode & 0o777, 0o755, `${label}: the modified binary's mode`);
    assert.equal(fs.readFileSync(path.join(copy, "untracked.txt"), "utf8"), UNTRACKED, `${label}: the untracked file`);
  } finally {
    fs.rmSync(exportDir, { recursive: true, force: true });
  }
}

interface RigOptions {
  guarded?: boolean;
  cleanTree?: boolean;
  /** No commit: the flight stops at the seed base head, clean or with one untracked file. */
  seedOnly?: "clean" | "untracked";
  recovery?: RecoveryCoordinator;
  log?: Logger;
  runnerOpts?: Record<string, unknown>;
  /** Runs after the worker-only state is written and before the claim goes stale. */
  beforeStop?: (ctx: RunContext, facts: Facts) => Promise<void>;
  /** Replaces the stopping executor for the Nth (0-based) claim. */
  executorFor?: (n: number) => Executor | undefined;
  /** The factory hands the runner a per-run HOME (created by the executor). */
  withHome?: boolean;
  /** An executor killAgentTree hook (the quiescence proof's literal reap). */
  killAgentTree?: () => void;
}

interface Rig {
  runner: RunRunner;
  claim: ReturnType<typeof gitlabClaim>;
  runId: string;
  bare: string;
  branch: string;
  /** Facts of every stopping flight, in claim order. */
  all: Facts[];
  facts: () => Facts;
  /** The count of this run's /state reports when the claim went stale. */
  statesAtStop: () => number;
  homePath: () => string;
  gitlabCalls: () => number;
  /** Execute a later claim of the same run. */
  next: (generation: number, over?: Record<string, unknown>) => Promise<void>;
}

/** The executor writes the worker-only state, then the claim goes stale through `trigger`. */
function stopRig(iid: number, flavor: Flavor, trigger: Trigger, o: RigOptions = {}): Rig {
  const { gitlab, calls } = fakeGitlab();
  const claim = gitlabClaim(iid, { claim_generation: 2, ...(o.guarded ? { inventory_guarded: true } : {}) });
  const all: Facts[] = [];
  let statesAtStop = -1;
  let n = 0;
  let runner!: RunRunner;
  const stopper: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      const facts = writeWorkerOnlyState(ctx.worktreePath, { cleanTree: o.cleanTree, ...(o.seedOnly ? { seedOnly: o.seedOnly } : {}) });
      all.push(facts);
      if (o.withHome) fs.mkdirSync(path.join(homeDir, ctx.runId), { recursive: true });
      await o.beforeStop?.(ctx, facts);
      if (trigger === "stale") {
        api.failStateWhen(ctx.runId, (b) => {
          if (b.status !== "awaiting_input") return false;
          statesAtStop = api.states.filter((s) => s.runId === ctx.runId).length;
          return true;
        }, { httpStatus: 409, runStatus: "running", disposition: "stale_claim" });
        await ctx.askUser!([{ question: "still mine?", header: "Q" }]);
        throw new Error("the stale ack must have stopped the flight");
      }
      statesAtStop = api.states.filter((s) => s.runId === ctx.runId).length;
      (runner as unknown as { activeRuns: Map<string, { steering: { claimFence: () => string | undefined } }> })
        .activeRuns.get(ctx.runId)!.steering.claimFence = () => "stale";
      throw new Error("executor stopped by the claim fence");
    },
    ...(o.killAgentTree ? { killAgentTree: o.killAgentTree } : {}),
  };
  runner = runnerWith(() => {
    const custom = o.executorFor?.(n++);
    return { executor: custom ?? stopper, ...(o.withHome ? { homeDir: path.join(homeDir, claim.run_id) } : {}) };
  }, gitlab, undefined, o.log ?? nullLogger(), {
    ...(flavor === "attempt" ? { dockerHost: DOCKER } : {}),
    ...(o.recovery ? { recovery: o.recovery } : {}),
    ...o.runnerOpts,
  });
  return {
    runner, claim, runId: claim.run_id,
    bare: git.barePathFor(fx.originPath), branch: `agent/issue-${iid}`,
    all, facts: () => all[0]!, statesAtStop: () => statesAtStop,
    homePath: () => path.join(homeDir, claim.run_id), gitlabCalls: () => calls.length,
    next: async (generation, over = {}) => { await runner.execute({ ...claim, claim_generation: generation, ...over }); },
  };
}

function guardedRecovery(holds: "open" | "released"): { coordinator: RecoveryCoordinator; client: FakeRecoveryClient } {
  const recoveryClient = new FakeRecoveryClient();
  client.protocolFeatures = ["recovery_park_cause", "recovery_release_exact_echo", "recovery_inventory_v1"];
  client.hasFeature = (name: string) => name === "recovery_inventory_v1";
  (recoveryClient as unknown as { hasFeature: (n: string) => boolean }).hasFeature = (n) => n === "recovery_inventory_v1";
  recoveryClient.holds = holds === "open"
    ? [{ hold_id: OPEN_HOLD, generation: 2, inventory_guarded: true, has_available_capture: false } as never]
    : [];
  const coordinator = new RecoveryCoordinator({
    client: recoveryClient, git, log: nullLogger(), recoveryRoot: git.recoveryRoot,
    workerToken: "stopped-source-worker-join-token-0123456789",
  });
  // The api acknowledged the guarded generation's FINAL over the bare's roots, which cover only
  // the committed head: the dirty tree beyond it is covered by nothing.
  coordinator.inventoryCleanupState = async () => "acknowledged";
  return { coordinator, client: recoveryClient };
}

const quiescentOutcome = (): QuiesceRunOutcome => ({
  process: { state: "quiescent", processes: [], killed: [], detail: "" },
  docker: { state: "not_wired", removed: [], detail: "" },
});

describe("RunRunner #1786 M1 — a flight that learns its claim is stale keeps its clone's bytes", () => {
  for (const flavor of ["canonical", "attempt"] as const) {
    for (const trigger of ["stale", "fence"] as const) {
      const iid0 = 17860 + (flavor === "attempt" ? 10 : 0) + (trigger === "fence" ? 1 : 0);
      it(`${flavor} clone, ${trigger === "stale" ? "stale_claim ack on a later /state report" : "steering claim fence"}: unfetched commit, modified binary and untracked file stay recoverable`, async () => {
        const rig = stopRig(iid0, flavor, trigger, { withHome: true });
        await rig.runner.execute(rig.claim);
        const facts = rig.facts();
        if (flavor === "canonical") assert.equal(facts.clonePath, worktreeDirFor(iid0));
        else assert.match(path.basename(facts.clonePath), /\.attempt-/);
        assertRecoverableFromExport(facts, `${flavor}/${trigger}`);
        assert.equal(fs.existsSync(rig.homePath()), false, "the per-run HOME is still removed");
      });

      it(`${flavor} clone, ${trigger}: the retention is recorded in the bare, and the stop reports and publishes nothing`, async () => {
        const iid = iid0 + 100;
        const published: string[] = [];
        const origPublish = client.publishCheckpoint.bind(client);
        (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (...a: Parameters<typeof origPublish>) => {
          published.push("publish");
          return origPublish(...a);
        };
        let successorTip = "";
        const rig: Rig = stopRig(iid, flavor, trigger, {
          beforeStop: async (_ctx, facts) => {
            // A successor claim's tracking ref: it must stay byte-identical through the stale exit.
            gitIn(rig.bare, "update-ref", `refs/uzi-runner/${rig.branch}`, facts.baseHead);
            successorTip = gitIn(rig.bare, "rev-parse", `refs/uzi-runner/${rig.branch}`);
          },
        });
        const retained: number[] = [];
        (rig.runner as unknown as { onSourceRetained: () => void }).onSourceRetained = () => { retained.push(1); };
        await rig.runner.execute(rig.claim);
        const facts = rig.facts();
        assert.equal(api.states.filter((s) => s.runId === rig.runId).length, rig.statesAtStop(),
          `no /state report after the stop: ${reportsFor(rig.runId).join(",")}`);
        assert.deepEqual(published, [], "no checkpoint publish");
        assert.equal(rig.gitlabCalls(), 0, "no MR or forge call");
        assert.equal(gitIn(rig.bare, "rev-parse", `refs/uzi-runner/${rig.branch}`), successorTip, "the successor's tracking ref is unchanged");
        assert.deepEqual(retained, [1], "the retained-source notify callback fired once, after retention");
        if (flavor === "attempt") {
          assert.equal(ledgerStates(rig.bare, rig.branch).get(attemptIdOf(facts.clonePath)), "reclaimed", "ledger state reclaimed");
          assert.equal(journalRaw(rig.bare, rig.branch), "", "the plain journal is cleared (ledger first)");
          assert.deepEqual(markers(rig.bare, rig.runId), [], "no canonical marker");
        } else {
          const journal = JSON.parse(journalRaw(rig.bare, rig.branch)) as { runId: string; clonePath: string };
          assert.equal(journal.clonePath, facts.clonePath, "the canonical journal is kept");
          const ms = markers(rig.bare, rig.runId).map((m) => JSON.parse(m) as { clonePath: string; generation: number | null; reason: string });
          assert.equal(ms.length, 1, "exactly one stopped-source marker");
          assert.equal(ms[0]!.clonePath, facts.clonePath);
          assert.equal(ms[0]!.generation, 2);
          assert.equal(ms[0]!.reason, trigger === "stale" ? "stale_claim" : "claim_fence");
        }
      });
    }
  }

  for (const holds of ["open", "released"] as const) {
    it(`a guarded claim (hold ${holds}) whose later /state report is a stale_claim ack keeps the dirty clone and calls no ownership, settle or archive API through the stale claim`, async () => {
      const rc = guardedRecovery(holds);
      let stopped = false;
      const ownershipCalls: string[] = [];
      client.getRunOwnership = async (runId: string) => {
        if (stopped) ownershipCalls.push(runId);
        return { status: "running", claim_generation: 3, inventory_guarded: true };
      };
      const rig = stopRig(17870 + (holds === "released" ? 1 : 0), "canonical", "stale", {
        guarded: true, recovery: rc.coordinator,
        // The committed head is fetched back under the run's tracking ownership at its generation;
        // the clone still holds a modified binary and an untracked file beyond that head.
        beforeStop: async (ctx) => {
          await fixtureFetchTracking(git, git.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, ctx.runId, 2);
          stopped = true;
        },
      });
      await rig.runner.execute(rig.claim);
      assertRecoverableFromExport(rig.facts(), `guarded/${holds}`);
      assert.deepEqual(ownershipCalls, [], "a stale guarded flight never probes ownership through the stale claim");
      assert.deepEqual(rc.client.releaseCalls, [], "and never releases custody");
      assert.deepEqual(rc.client.uploadCalls, [], "and uploads no archive");
    });
  }

  it("a stale exit with a blocked quiescence proof keeps the clone and the record, and logs the unresolved writers (result unknown)", async () => {
    const rl = recordingLogger();
    const requests: QuiesceRunRequest[] = [];
    const rig = stopRig(17880, "canonical", "stale", {
      log: rl.logger,
      runnerOpts: {
        quiesceRun: async (req: QuiesceRunRequest) => {
          requests.push(req);
          return {
            process: { state: "unverified", processes: [], killed: [], detail: "independent retirement proof blocked" },
            docker: { state: "not_wired", removed: [], detail: "" },
          };
        },
      },
    });
    await rig.runner.execute(rig.claim);
    assertRecoverableFromExport(rig.facts(), "blocked proof");
    const lines = rl.lines as Array<{ msg: string; stopped_source?: string }>;
    assert.ok(lines.some((l) => /unresolved writers/.test(l.msg)), "the unresolved writers are logged");
    assert.ok(lines.some((l) => l.stopped_source === "unknown"), "the stop result is unknown");
    assert.ok(requests.some((r) => r.site === "stopped_source" && r.mode === "own"), "the proof ran as the own-mode stopped_source site");
    assert.equal(markers(rig.bare, rig.runId).length, 1, "the record is written whatever the proof said");
    assert.ok(!requests.some((r) => r.site === "terminal_retire"), "the finally's own terminal retire proof is skipped by the latch");
  });

  it("the stale exit's quiescence proof reaps an attributed live child and asks for the Docker teardown (attempt clone)", async () => {
    // The Docker half is exercised through the injected proof: no Docker daemon is available to
    // these unit tests, so a real container teardown is NOT run here (only the request is asserted).
    const children: ChildProcess[] = [];
    const requests: QuiesceRunRequest[] = [];
    const rig = stopRig(17881, "attempt", "fence", {
      killAgentTree: () => { for (const c of children) if (c.pid !== undefined && isAlive(c.pid)) process.kill(c.pid, "SIGKILL"); },
      beforeStop: async (ctx) => {
        children.push(spawn("sleep", ["30"], { cwd: ctx.worktreePath, stdio: "ignore" }));
      },
      runnerOpts: {
        quiesceRun: async (req: QuiesceRunRequest) => { requests.push(req); return quiescentOutcome(); },
      },
    });
    try {
      await rig.runner.execute(rig.claim);
      const pid = children[0]!.pid!;
      await waitDead(pid);
      assert.equal(isAlive(pid), false, "the live child is killed at the stale exit");
      assertRecoverableFromExport(rig.facts(), "child kill");
      const req = requests.find((r) => r.site === "stopped_source");
      assert.ok(req, "the stopped_source proof ran");
      assert.equal(req!.dockerHost, DOCKER, "the proof asks for the Docker teardown of attributed containers");
      assert.equal(req!.mode, "own");
    } finally {
      for (const c of children) if (c.pid !== undefined && isAlive(c.pid)) process.kill(c.pid, "SIGKILL");
    }
  });

  it("clean tree, commit never fetched: retained (nothing else holds the commit)", async () => {
    const rig = stopRig(17882, "canonical", "stale", { cleanTree: true });
    await rig.runner.execute(rig.claim);
    const facts = rig.facts();
    assert.ok(fs.existsSync(facts.clonePath), "the clean but unfetched clone is retained");
    assert.equal(gitIn(facts.clonePath, "rev-parse", "HEAD"), facts.commit);
    assert.equal(markers(rig.bare, rig.runId).length, 1);
  });

  for (const flavor of ["canonical", "attempt"] as const) {
    it(`${flavor} clone, clean and fully fetched under this run's tracking ownership: teardown proceeds and retains nothing`, async () => {
      const rl = recordingLogger();
      const rig = stopRig(17883 + (flavor === "attempt" ? 1 : 0), flavor, "stale", {
        cleanTree: true, log: rl.logger,
        beforeStop: async (ctx) => {
          await fixtureFetchTracking(git, git.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, ctx.runId, 2);
        },
      });
      const retained: number[] = [];
      (rig.runner as unknown as { onSourceRetained: () => void }).onSourceRetained = () => { retained.push(1); };
      await rig.runner.execute(rig.claim);
      const facts = rig.facts();
      const lines = rl.lines as Array<{ msg: string; stopped_source?: string }>;
      assert.ok(lines.some((l) => l.stopped_source === "covered"), "the covered teardown is logged as `covered`, not `no_source`");
      assert.ok(!lines.some((l) => l.stopped_source === "no_source"));
      assert.equal(fs.existsSync(facts.clonePath), false, "a fully covered clean clone is retired by the normal teardown");
      assert.deepEqual(markers(rig.bare, rig.runId), [], "no marker");
      assert.deepEqual(retained, [], "nothing retained, nothing notified");
      assert.equal(gitIn(rig.bare, "rev-parse", `refs/uzi-runner/${rig.branch}`), facts.commit, "the tracking ref still holds the commit");
    });
  }

  for (const flavor of ["canonical", "attempt"] as const) {
    it(`${flavor} clone, clean and fully fetched, but the final proof reaps a late writer: the clone is retained with the late bytes and the record is written`, async () => {
      const iid = 17890 + (flavor === "attempt" ? 1 : 0);
      const LATE = "written after the byte checks\n";
      let coveredCalls = 0;
      let latePath = "";
      let writerPid = 0;
      const rig = stopRig(iid, flavor, "stale", {
        cleanTree: true,
        beforeStop: async (ctx) => {
          await fixtureFetchTracking(git, git.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, ctx.runId, 2);
        },
        runnerOpts: {
          quiesceRun: async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
            if (req.site !== "stopped_source:covered") return quiescentOutcome();
            coveredCalls++;
            if (coveredCalls < 2) return quiescentOutcome();
            // The "after" proof: a writer landed a file after the status read and is reaped here.
            latePath = path.join(req.targetPaths[0]!, "late.txt");
            fs.writeFileSync(latePath, LATE);
            writerPid = 4242;
            return { process: { state: "quiescent", processes: [], killed: [writerPid], detail: "reaped 1 process(es)" }, docker: { state: "not_wired", removed: [], detail: "" } };
          },
        },
      });
      await rig.runner.execute(rig.claim);
      const facts = rig.facts();
      assert.equal(coveredCalls, 2, "the coverage proof asked before and after");
      assert.ok(fs.existsSync(facts.clonePath), "the clone is kept");
      assert.equal(fs.readFileSync(latePath, "utf8"), LATE, "the late file survives");
      if (flavor === "attempt") assert.equal(ledgerStates(rig.bare, rig.branch).get(attemptIdOf(facts.clonePath)), "reclaimed");
      else assert.equal(markers(rig.bare, rig.runId).length, 1, "the retention record is written");
    });
  }

  // The reviewer probe: HEAD equals the run's owned tracking tip and the tree is clean, but the
  // clone also holds a commit on another local branch, or a stash entry. Neither is in any capture.
  for (const flavor of ["canonical", "attempt"] as const) {
    for (const extra of ["side-branch", "stash"] as const) {
      for (const trigger of ["stale", "fence"] as const) {
        it(`${flavor} clone, ${trigger}: clean at the owned tracking tip but holding a ${extra} is retained with that source intact`, async () => {
          const iid = 17900 + (flavor === "attempt" ? 20 : 0) + (extra === "stash" ? 10 : 0) + (trigger === "fence" ? 5 : 0);
          let sideTip = "";
          const rig = stopRig(iid, flavor, trigger, {
            cleanTree: true,
            beforeStop: async (ctx, facts) => {
              await fixtureFetchTracking(git, git.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, ctx.runId, 2);
              if (extra === "side-branch") {
                gitIn(ctx.worktreePath, "checkout", "-q", "-b", "side");
                fs.writeFileSync(path.join(ctx.worktreePath, "side.txt"), "only on the side branch\n");
                gitIn(ctx.worktreePath, "add", "side.txt");
                gitIn(ctx.worktreePath, ...IDENT, "commit", "-q", "-m", "side work");
                sideTip = gitIn(ctx.worktreePath, "rev-parse", "HEAD");
                gitIn(ctx.worktreePath, "checkout", "-q", "--detach", facts.commit);
              } else {
                fs.writeFileSync(path.join(ctx.worktreePath, "assets", "blob.bin"), BIN_V2);
                gitIn(ctx.worktreePath, ...IDENT, "stash", "push");
              }
              assert.equal(gitIn(ctx.worktreePath, "status", "--porcelain"), "", "the tree is clean at the tracked head");
            },
          });
          await rig.runner.execute(rig.claim);
          const facts = rig.facts();
          assert.equal(gitIn(rig.bare, "rev-parse", `refs/uzi-runner/${rig.branch}`), facts.commit, "HEAD is the owned tracking tip");
          assert.ok(fs.existsSync(facts.clonePath), "the clone is retained");
          if (extra === "side-branch") assert.equal(gitIn(facts.clonePath, "rev-parse", "side"), sideTip, "the side branch's commit survives");
          else assert.match(gitIn(facts.clonePath, "stash", "list"), /stash@\{0\}/, "the stash entry survives");
          if (flavor === "canonical") assert.equal(markers(rig.bare, rig.runId).length, 1, "a stopped-source marker records the retention");
          else assert.equal(ledgerStates(rig.bare, rig.branch).get(attemptIdOf(facts.clonePath)), "reclaimed");
        });
      }
    }
  }

  // The seed gives the clone a local default branch at the bare's HEAD and every bare tag, and a
  // resumed branch can be older than main: refs the bare already holds must not look like unfinished work.
  it("clean clone at the owned tracking tip whose local default branch and tag are held by the bare is retired, not retained", async () => {
    const rig = stopRig(17960, "canonical", "stale", {
      cleanTree: true,
      beforeStop: async (ctx, facts) => {
        await fixtureFetchTracking(git, git.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, ctx.runId, 2);
        const bare = git.barePathFor(fx.originPath);
        const wt = ctx.worktreePath;
        gitIn(wt, "checkout", "-q", "-b", "newer-main", facts.baseHead);
        fs.writeFileSync(path.join(wt, "main-moved.txt"), "main moved on\n");
        gitIn(wt, "add", "main-moved.txt");
        gitIn(wt, ...IDENT, "commit", "-q", "-m", "main moved on");
        gitIn(wt, "tag", "off-branch-release");
        gitIn(bare, "fetch", "-q", "--", wt, "refs/heads/newer-main:refs/remotes/origin/newer-main", "refs/tags/off-branch-release:refs/tags/off-branch-release");
        gitIn(wt, "checkout", "-q", "--detach", facts.commit);
      },
    });
    await rig.runner.execute(rig.claim);
    const facts = rig.facts();
    assert.equal(fs.existsSync(facts.clonePath), false, "refs the bare holds do not keep a fully fetched clone");
    assert.deepEqual(markers(rig.bare, rig.runId), [], "no marker");
  });

  // A flight stopped while planning never committed past its seed base: HEAD is an ancestor of the
  // bare's default branch, so there is no tracking ref to anchor and no worker-only commit either.
  for (const flavor of ["canonical", "attempt"] as const) {
    it(`${flavor} clone, clean at the seed base head already on the bare's default branch: teardown proceeds, nothing retained`, async () => {
      const rig = stopRig(17970 + (flavor === "attempt" ? 1 : 0), flavor, "stale", { seedOnly: "clean" });
      await rig.runner.execute(rig.claim);
      const facts = rig.facts();
      assert.equal(fs.existsSync(facts.clonePath), false, "nothing worker-only: the clone is retired");
      assert.deepEqual(markers(rig.bare, rig.runId), [], "no marker");
    });

    it(`${flavor} clone, seed base head but an untracked file: retained`, async () => {
      const rig = stopRig(17972 + (flavor === "attempt" ? 1 : 0), flavor, "stale", { seedOnly: "untracked" });
      await rig.runner.execute(rig.claim);
      const facts = rig.facts();
      assert.equal(fs.readFileSync(path.join(facts.clonePath, "untracked.txt"), "utf8"), UNTRACKED, "the untracked file survives");
    });
  }

  it("clean clone at the owned tracking tip holding a commit only under refs/remotes/ is retained", async () => {
    const rig = stopRig(17961, "canonical", "stale", {
      cleanTree: true,
      beforeStop: async (ctx, facts) => {
        await fixtureFetchTracking(git, git.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, ctx.runId, 2);
        const wt = ctx.worktreePath;
        gitIn(wt, "checkout", "-q", "-b", "planted", facts.baseHead);
        fs.writeFileSync(path.join(wt, "planted.txt"), "only under refs/remotes\n");
        gitIn(wt, "add", "planted.txt");
        gitIn(wt, ...IDENT, "commit", "-q", "-m", "planted");
        gitIn(wt, "update-ref", "refs/remotes/origin/x", gitIn(wt, "rev-parse", "HEAD"));
        gitIn(wt, "checkout", "-q", "--detach", facts.commit);
        gitIn(wt, "branch", "-D", "planted");
      },
    });
    await rig.runner.execute(rig.claim);
    const facts = rig.facts();
    assert.ok(fs.existsSync(facts.clonePath), "the clone is retained");
    assert.equal(markers(rig.bare, rig.runId).length, 1);
  });

  it("a stale_claim stop of a completed retained successor whose cleanup the api acknowledged keeps the clone and never runs the verified-successor retire", async () => {
    const rc = guardedRecovery("open");
    const verifiedRetires: unknown[] = [];
    const real = git.retireRunnerClone.bind(git);
    git.retireRunnerClone = (async (...a: Parameters<typeof real>) => {
      if ((a[4] as { verifiedSuccessor?: unknown } | undefined)?.verifiedSuccessor !== undefined) verifiedRetires.push(a[4]);
      return real(...a);
    }) as typeof git.retireRunnerClone;
    let flight: Record<string, unknown> | undefined;
    const rig = stopRig(17962, "attempt", "stale", {
      guarded: true, recovery: rc.coordinator,
      beforeStop: async (ctx) => {
        // Synthetic: a settled retained-successor flight whose terminal was already resolved.
        flight!.completedRetainedSuccessor = { runId: ctx.runId, clonePath: ctx.worktreePath };
        flight!.terminalResolved = true;
      },
    });
    const phaseClone = (rig.runner as unknown as { phaseClone: (...a: unknown[]) => Promise<unknown> }).phaseClone.bind(rig.runner);
    (rig.runner as unknown as { phaseClone: unknown }).phaseClone = async (...a: unknown[]) => {
      flight = a[1] as Record<string, unknown>;
      return phaseClone(...a);
    };
    await rig.runner.execute(rig.claim);
    assert.deepEqual(verifiedRetires, [], "the stopped flight never attempts the verified successor retire");
    assertRecoverableFromExport(rig.facts(), "retained successor stopped by a stale claim");
  });

  it("a NEW run of the same issue on the same worker, after a retained stale stop, fails once with worker_residue_blocked and starts no model", async () => {
    let modelled = false;
    const rig = stopRig(17963, "canonical", "stale", {
      executorFor: (n) => (n === 0 ? undefined : { run: async () => { modelled = true; throw new Error("the new run must not model"); } }),
    });
    await rig.runner.execute(rig.claim);
    const first = rig.facts();
    const newRunId = "22222222-0000-4000-8000-000000001963";
    await rig.runner.execute({ ...rig.claim, run_id: newRunId, claim_generation: 1 });
    const failed = api.states.filter((st) => st.runId === newRunId && st.body.status === "failed");
    assert.equal(failed.length, 1, `the new run fails once: ${reportsFor(newRunId).join(",")}`);
    assert.equal(failed[0]!.body.fail_origin, "worker_residue_blocked");
    assert.match(String(failed[0]!.body.failure_reason), /^worker_residue_blocked: retained stopped work: run /);
    assert.equal(modelled, false, "no model started");
    assertRecoverableFromExport(first, "the retained source of the stopped run");
    assert.equal(markers(rig.bare, rig.runId).length, 1, "the marker is kept");
  });

  // Coverage needs an anchor this run and generation owns, or an owed pin of this run.
  for (const owner of ["another run", "another generation"] as const) {
    it(`clean clone whose tracking ref sits at its head under ${owner} (and no owed pin of this run) is retained`, async () => {
      const iid = owner === "another run" ? 17950 : 17951;
      const rig = stopRig(iid, "canonical", "stale", {
        cleanTree: true,
        beforeStop: async (ctx) => {
          const bare = git.barePathFor(fx.originPath);
          if (owner === "another run") {
            await fixtureFetchTracking(git, bare, ctx.worktreePath, ctx.branch, "run-someone-else", 2);
          } else {
            await fixtureFetchTracking(git, bare, ctx.worktreePath, ctx.branch, ctx.runId, 1);
            const head = gitIn(ctx.worktreePath, "rev-parse", "HEAD");
            gitIn(bare, "update-ref", "-d", `refs/uzi-owed/${ctx.runId}/${head}`);
          }
        },
      });
      await rig.runner.execute(rig.claim);
      const facts = rig.facts();
      assert.equal(gitIn(rig.bare, "rev-parse", `refs/uzi-runner/${rig.branch}`), facts.commit, "the tracking ref is at the clone head");
      assert.ok(fs.existsSync(facts.clonePath), "the clone is retained");
      assert.equal(markers(rig.bare, rig.runId).length, 1);
    });
  }

  it("the preserve latch is set before the stopped-source proof is awaited", async () => {
    const seen: boolean[] = [];
    let flight: { preserveRecoveryClone: boolean } | undefined;
    let rig!: Rig;
    rig = stopRig(17952, "canonical", "stale", {
      runnerOpts: {
        quiesceRun: async (req: QuiesceRunRequest) => {
          if (req.site === "stopped_source") seen.push(flight!.preserveRecoveryClone === true);
          return quiescentOutcome();
        },
      },
    });
    const phaseClone = (rig.runner as unknown as { phaseClone: (...a: unknown[]) => Promise<unknown> }).phaseClone.bind(rig.runner);
    (rig.runner as unknown as { phaseClone: unknown }).phaseClone = async (...a: unknown[]) => {
      flight = a[1] as { preserveRecoveryClone: boolean };
      return phaseClone(...a);
    };
    await rig.runner.execute(rig.claim);
    assert.deepEqual(seen, [true], "the proof ran once, with the latch already set");
  });

  it("a terminal refusal of the first running report ends with no clone: no_source, ordinary teardown, no record", async () => {
    const claim = gitlabClaim(17953, { claim_generation: 2 });
    const rl = recordingLogger();
    const requests: QuiesceRunRequest[] = [];
    let ran = 0;
    api.failStateWhen(claim.run_id, (b) => b.status === "running", { httpStatus: 409, runStatus: "completed" });
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({
      executor: { run: async (): Promise<ExecutorResult> => { ran++; throw new Error("must not run"); } },
    }), gitlab, undefined, rl.logger, {
      quiesceRun: async (req: QuiesceRunRequest) => { requests.push(req); return quiescentOutcome(); },
    });
    await runner.execute(claim);
    const lines = rl.lines as Array<{ msg: string; stopped_source?: string }>;
    assert.equal(ran, 0);
    assert.ok(lines.some((l) => l.stopped_source === "no_source"), "the stop result is no_source");
    assert.equal(fs.existsSync(worktreeDirFor(17953)), false, "no clone path was ever created");
    assert.deepEqual(markers(git.barePathFor(fx.originPath), claim.run_id), [], "no record");
    assert.ok(!requests.some((r) => r.site === "stopped_source"), "no proof for a flight with no clone");
    assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"), "no failed report");
  });

  it("same-pod restart: a new GitCache over the same data dir finds the retained source", async () => {
    const rig = stopRig(17886, "canonical", "stale");
    await rig.runner.execute(rig.claim);
    const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const found = await fresh.inspectRetainedSources({ isLivePath: () => false });
    const mine = found.sources.filter((s) => s.runId === rig.runId);
    assert.equal(mine.length, 1);
    assert.equal(mine[0]!.kind, "canonical");
    assert.equal(mine[0]!.presence, "present");
    assert.equal(mine[0]!.generation, 2);
    assert.equal(mine[0]!.markerState, "bound");
  });

  for (const flavor of ["canonical", "attempt"] as const) {
    it(`${flavor} clone: the operator channel inspects the new source and reclaims it (no hold, and a released hold)`, async () => {
      const rig = stopRig(17890 + (flavor === "attempt" ? 1 : 0), flavor, "fence");
      await rig.runner.execute(rig.claim);
      const facts = rig.facts();
      const found = (await git.inspectRetainedSources({ isLivePath: () => false })).sources.find((s) => s.runId === rig.runId);
      assert.ok(found, "inspect lists the source");
      const selector = { runId: rig.runId, generation: found!.generation, attempt: found!.attempt };
      // An open server hold refuses with the owner action; an empty (released) list permits the local discard.
      const open = await git.inspectRetainedSources({
        selector, barePath: found!.barePath, isLivePath: () => false,
        holds: { state: "read", holds: [{ id: OPEN_HOLD, generation: 2 }] },
      });
      assert.equal(open.sources[0]!.disposition?.kind, "owner_hold_discard");
      const released = await git.inspectRetainedSources({
        selector, barePath: found!.barePath, isLivePath: () => false, holds: { state: "read", holds: [] },
      });
      assert.equal(released.sources[0]!.disposition?.kind, "local_discard");
      const result = await git.discardStoppedSource({
        selector, confirm: released.sources[0]!.descriptor, barePath: found!.barePath,
        holds: { state: "read", holds: [] }, isLivePath: () => false, quiesce: async () => true,
      });
      assert.equal(result.kind, "done", JSON.stringify(result));
      assert.equal(fs.existsSync(facts.clonePath), false, "the discarded source is gone");
      assert.deepEqual(markers(rig.bare, rig.runId), [], "and its marker is retired");
    });
  }

  it("identity race (canonical): a successor journal and a newer clone at the path appear at the lock boundary; nothing of theirs is captured, renamed or deleted", async () => {
    let replacement = "";
    let successorJournal = "";
    const real = git.markStoppedCanonical.bind(git);
    const rig = stopRig(17895, "canonical", "stale");
    git.markStoppedCanonical = (async (...a: Parameters<typeof real>) => {
      const clone = a[3].clonePath;
      fs.renameSync(clone, `${clone}.moved-by-test`);
      execFileSync("git", ["clone", "-q", "--no-hardlinks", "--", rig.bare, clone], { env: GIT_ENV, stdio: "pipe" });
      fs.writeFileSync(path.join(clone, "newer.txt"), "the successor's own bytes\n");
      replacement = clone;
      successorJournal = JSON.stringify({ runId: "successor-run", clonePath: clone, branch: rig.branch });
      gitIn(rig.bare, "config", "--local", `uzi-recovery.${rig.branch}.clone`, successorJournal);
      return real(...a);
    }) as typeof git.markStoppedCanonical;
    await rig.runner.execute(rig.claim);
    assert.equal(fs.readFileSync(path.join(replacement, "newer.txt"), "utf8"), "the successor's own bytes\n", "the newer clone is intact");
    assert.equal(journalRaw(rig.bare, rig.branch), successorJournal, "the successor journal is untouched");
    assert.deepEqual(markers(rig.bare, rig.runId), [], "no marker was written for a source the journal no longer names");
    assertRecoverableFromExport({ ...rig.facts(), clonePath: `${replacement}.moved-by-test` }, "the original bytes");
  });

  it("identity race (attempt): a successor's journal at the lock boundary stays untouched while the old attempt is reclaimed in the ledger", async () => {
    let successorJournal = "";
    const real = git.retainStoppedAttempt.bind(git);
    const rig = stopRig(17896, "attempt", "stale");
    git.retainStoppedAttempt = (async (...a: Parameters<typeof real>) => {
      const otherId = formatAttemptId(new Date(Date.UTC(2030, 0, 1)), 3, "0123456789abcdef");
      const own = JSON.parse(journalRaw(rig.bare, rig.branch)) as Record<string, unknown>;
      successorJournal = JSON.stringify({
        ...own, clonePath: `${a[2].clonePath.split(".attempt-")[0]}.attempt-${otherId}`, attemptId: otherId,
      });
      gitIn(rig.bare, "config", "--local", `uzi-recovery.${rig.branch}.clone`, successorJournal);
      return real(...a);
    }) as typeof git.retainStoppedAttempt;
    await rig.runner.execute(rig.claim);
    assert.equal(journalRaw(rig.bare, rig.branch), successorJournal, "the successor journal is untouched");
    assert.equal(ledgerStates(rig.bare, rig.branch).get(attemptIdOf(rig.facts().clonePath)), "reclaimed");
    assertRecoverableFromExport(rig.facts(), "attempt race");
  });

  it("same-worker newer successor on an attempt worker: the next claim seeds a fresh attempt path, captures nothing, fails nothing, and the old attempt is untouched", async () => {
    let secondPath = "";
    const stub = new StubExecutor(nullLogger());
    const rig = stopRig(17897, "attempt", "fence", {
      executorFor: (n) => (n === 0 ? undefined : {
        run: (ctx: RunContext) => { secondPath = ctx.worktreePath; return stub.run(ctx); },
      }),
    });
    await rig.runner.execute(rig.claim);
    const first = rig.facts();
    await rig.next(3);
    assert.match(path.basename(secondPath), /\.attempt-/, "the next claim ran in an attempt clone");
    assert.notEqual(secondPath, first.clonePath, "a fresh attempt path was seeded for the next claim");
    assertRecoverableFromExport(first, "the old attempt");
    const states = reportsFor(rig.runId);
    assert.ok(!states.includes("failed"), `the successor claim did not fail: ${states.join(",")}`);
    assert.ok(states.includes("completed"), `the successor claim completed: ${states.join(",")}`);
    assert.equal(ledgerStates(rig.bare, rig.branch).get(attemptIdOf(first.clonePath)), "reclaimed", "the old attempt is still reclaimed");
  });

  it("same-worker canonical successor: the next claim of the run is refused with the residue-blocked outcome, never an adoption", async () => {
    let modelled = false;
    const rig = stopRig(17898, "canonical", "fence", {
      executorFor: (n) => (n === 0 ? undefined : { run: async () => { modelled = true; throw new Error("the successor must not model"); } }),
    });
    await rig.runner.execute(rig.claim);
    const first = rig.facts();
    await rig.next(3);
    const failed = api.states.filter((s) => s.runId === rig.runId && s.body.status === "failed");
    assert.equal(failed.length, 1, `the successor claim fails once: ${reportsFor(rig.runId).join(",")}`);
    assert.match(String(failed[0]!.body.failure_reason), /^worker_residue_blocked: retained stopped work: run /);
    assert.match(String(failed[0]!.body.failure_reason), /inspect it with the worker-local retained-source command/);
    assert.equal(failed[0]!.body.fail_origin, "worker_residue_blocked");
    assert.equal(modelled, false, "no model started on the successor");
    assertRecoverableFromExport(first, "the stopped canonical source");
    assert.equal(markers(rig.bare, rig.runId).length, 1, "the marker is kept");
  });

  it("the successor run's guarded settle is not wedged by a reclaimed dirty stale attempt", async () => {
    const rc = guardedRecovery("open");
    const rig = stopRig(17899, "attempt", "fence", {
      executorFor: (n) => (n === 0 ? undefined : new StubExecutor(nullLogger())),
      recovery: rc.coordinator,
    });
    await rig.runner.execute(rig.claim);
    rc.client.holds = [{ hold_id: OPEN_HOLD, generation: 3, inventory_guarded: true, has_available_capture: false } as never];
    await rig.next(3, { inventory_guarded: true });
    const states = reportsFor(rig.runId);
    assert.ok(states.includes("completed"), `the guarded successor completed: ${states.join(",")}`);
    assert.ok(!states.includes("failed"));
    assertRecoverableFromExport(rig.facts(), "the stale attempt still holds its bytes");
  });
});
