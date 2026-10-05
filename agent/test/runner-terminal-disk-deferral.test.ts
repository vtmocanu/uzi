import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { DataVolumeGuard, DataVolumeFullError } from "../src/disk-full.js";
import { DiskGovernor, DiskParkSignal } from "../src/cache-cap.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { PlanRejectedError, type CredentialFreeSettleOutcome, type ExecutorResult, type RunContext } from "../src/executor.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import type { BoundaryPermit, CodexExecutionSafety } from "../src/harness.js";
import { type ExecutorFactory, type RunnerOptions } from "../src/runner.js";
import type { QuiesceRunOutcome } from "../src/run-quiescence.js";
import type { StatfsSample } from "../src/stats.js";
import type { StateAck, StateRequest } from "../src/protocol.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

import { opaqueBoundaryCases, trustedBoundaryCases, trustedCaptureCases, trustedEnvelopeCases, trustedExecutionCases } from "./fixtures/trusted-execution-refusals.js";

installHarness();
const GIB = 1024 ** 3;
const ROOMY: StatfsSample = { bsize: 4096, blocks: 25 * GIB / 4096, bfree: 10 * GIB / 4096,
  bavail: 10 * GIB / 4096, files: 1_000_000, ffree: 900_000 };
const FULL: StatfsSample = { ...ROOMY, bavail: 1, bfree: 1 };
const QUIESCENT: QuiesceRunOutcome = {
  process: { state: "quiescent", processes: [], killed: [], detail: "fixture positively quiescent" },
  docker: { state: "not_wired", removed: [], detail: "not wired" },
};
function readGit(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

function fixture(opts: {
  error?: unknown; sample?: StatfsSample; unknown?: boolean; mismatch?: "home" | "clone";
  missingStat?: "home" | "clone"; features?: readonly string[]; status?: string; generation?: number | null;
  kind?: "issue" | "task"; beforeReject?: () => Promise<void> | void;
  reclaim?: () => Promise<void>; runner?: Partial<RunnerOptions>;
  result?: ExecutorResult; safety?: CodexExecutionSafety; observe?: (ctx: RunContext) => void;
  afterWork?: (ctx: RunContext) => Promise<void>;
  settleForCredentialFreeCapture?: () => Promise<CredentialFreeSettleOutcome>;
} = {}) {
  const claim = gitlabClaim(2201, { claim_generation: 4, ...(opts.kind ? { kind: opts.kind } : {}) });
  client.protocolFeatures = opts.features ? [...opts.features] :  ["recovery_cause_data_volume_full", "claim_generation_fence"];
  api.onState(claim.run_id, (body) => api.setOwnershipStatus(claim.run_id, body.status, claim.claim_generation));
  let full = false;
  let calls = 0;
  let reclaims = 0;
  let clone = "";
  const home = path.join(homeDir, claim.run_id);
  const error = Object.hasOwn(opts, "error") ? opts.error : new Error("opaque executor failure");
  const factory: ExecutorFactory = () => ({
    homeDir: home,
    executor: { safety: opts.safety, settleForCredentialFreeCapture: opts.settleForCredentialFreeCapture, run: async (ctx) => {
      opts.observe?.(ctx);
      calls++;
      clone = ctx.worktreePath;
      fs.mkdirSync(home, { recursive: true });
      fs.writeFileSync(path.join(home, "session"), "transcript");
      fs.writeFileSync(path.join(clone, "COMMITTED.txt"), "committed sentinel\n");
      readGit(clone, "add", "COMMITTED.txt");
      readGit(clone, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture committed work");
      fs.writeFileSync(path.join(clone, "DIRTY.txt"), "dirty sentinel\n");
      full = true; // Clone and preflight completed with room. Only executor rejection sees full.
      api.setOwnershipStatus(claim.run_id, opts.status ?? "running", opts.generation === null ? undefined : opts.generation ?? 4);
      await opts.beforeReject?.();
      await opts.afterWork?.(ctx);
      if (opts.result) return opts.result;
      throw error;
    } },
  });
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => {
      if (!full) return ROOMY;
      if (opts.unknown) throw new Error("statfs unavailable");
      return opts.sample ?? FULL;
    },
    stat: async (p) => {
      const which = p === home ? "home" : p === clone ? "clone" : "data";
      if (which === opts.missingStat) throw new Error("stat unavailable");
      return { dev: which === opts.mismatch ? 8 : 7 };
    },
    reclaim: async () => { reclaims++; await opts.reclaim?.(); },
  });
  const forge = fakeGitlab();
  const runner = runnerWith(factory, forge.gitlab, undefined, nullLogger(), {
    dataVolume: guard, recoveryRetryMs: 1, quiesceRun: async () => QUIESCENT, ...opts.runner,
  });
  return { claim, runner, guard, home, clone: () => clone, calls: () => calls, reclaims: () => reclaims,
    forge, setRoomy: () => { full = false; }, setFull: () => { full = true; } };
}
function parks() { return api.states.filter((s) => s.body.status === "recovery_wait"); }
function settlementGovernor(error: unknown): DiskGovernor {
  const gov = new DiskGovernor({
    config: { capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 1,
      hardStopEnabled: false, hardMargin: 0.03 },
    log: nullLogger(), volumeTotalBytes: () => 25 * GIB, thresholdOf: () => undefined,
  });
  gov.leftLoop = () => { throw error; };
  return gov;
}
async function waitForAbort(signal: AbortSignal): Promise<void> {
  const end = Date.now() + 2000;
  while (!signal.aborted && Date.now() < end) await new Promise((resolve) => setTimeout(resolve, 5));
  assert.equal(signal.aborted, true, "control reached the running flight");
}
function supervisedSafety(mode: "empty" | "absent" | "incomplete"): CodexExecutionSafety {
  return {
    kind: "codex",
    withBoundary: async (request, action) => {
      if (mode === "incomplete") throw new Error("supervisor settlement incomplete");
      if (mode === "absent") return undefined as never;
      return await action({ epoch: 1, boundary: request.boundary, signal: new AbortController().signal } as BoundaryPermit);
    },
    spawnBoundaryProcess: async () => { throw new Error("fixture has no subprocess admission"); },
    dispose: async () => ({ kind: "disposed" }),
  };
}

// These capture cases require the runner's Linux process-proof branch, even when
// the proof itself is injected. Portable rejection-only cases remain unguarded.
const LINUX_CAPTURE = process.platform !== "linux" ? "requires Linux terminal-capture process proof" : false;

describe("terminal execution disk deferral", () => {
  it("setup failure on a full volume keeps the original failure path", async () => {
    const f = fixture();
    git.createOrAttachRunnerClone = async () => { f.setFull(); throw new Error("opaque setup failure"); };
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 0);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.failure_reason, "opaque setup failure");
  });

  it("successful execution followed by finalize failure never takes occupancy deferral", async () => {
    const f = fixture({ result: { branch: "agent/issue-2201" } });
    git.pushBranch = async () => { throw new Error("opaque finalize failure"); };
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 1);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.some((s) => s.body.status === "failed"), true);
  });

  it("an actual awaiting-approval gate cannot become a disk park", async () => {
    const f = fixture({ afterWork: async (ctx) => {
      const verdict = await ctx.gatePlan!("# Plan\n- implement the approved work", [], async () => {
        api.setInputs(f.claim.run_id, [{ id: 1, kind: "revise_plan", body: "tighten the scope" }]);
      });
      assert.equal(verdict.kind, "revise");
      throw new Error("revision turn failed");
    } });
    await f.runner.execute({ ...f.claim, auto_approve: false });
    assert.equal(api.states.some((s) => s.body.status === "awaiting_approval"), true);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.some((s) => s.body.status === "failed"), true);
  });

  it("durable terminal write-ahead wins over coincident full occupancy", async () => {
    const outbox = new Outbox({ root: path.join(fx.dataDir, "terminal-outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    const f = fixture({ runner: { outbox }, beforeReject: async () => {
      const result = await outbox.journalTerminal(f.claim.run_id, 4, "running", 0,
        { status: "failed", failure_reason: "durable original failure", claim_generation: 4 });
      assert.equal(result.journaled, true);
    } });
    await f.runner.execute(f.claim);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").length, 0);
    assert.equal(outbox.hasPendingTerminal(f.claim.run_id, 4), true);
    assert.equal((await outbox.readTerminalJournal(f.claim.run_id, 4))?.body.failure_reason, "durable original failure");
  });

  it("a finalize journal write failure after execution success stays best effort", async () => {
    const outbox = new Outbox({ root: path.join(fx.dataDir, "finalize-outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    let journaled = 0;
    outbox.journalFinalize = async () => { journaled++; throw new Error("journal write refused"); };
    const f = fixture({ result: { branch: "agent/issue-2201", reportOnly: true }, runner: { outbox } });
    await f.runner.execute(f.claim);
    assert.equal(journaled, 1);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.some((s) => s.body.status === "completed"), true);
  });

  for (const value of [new Error("same reused error"), "same primitive", undefined, NaN]) {
    for (const success of [false, true]) {
      it(`settlement replacement cannot defer: ${String(value)}, executor success=${success}`, async () => {
        const f = fixture({ error: value, result: success ? { branch: "agent/issue-2201" } : undefined,
          runner: { diskGovernor: settlementGovernor(value) } });
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), 1);
        assert.equal(f.reclaims(), 0);
        assert.equal(parks().length, 0);
        assert.equal(api.states.filter((s) => s.body.status === "failed").length, 1);
      });
    }
  }

  for (const mode of ["empty", "absent", "incomplete"] as const) {
    it(`Codex supervisor ${mode} must affirmatively enter a successful boundary`, async () => {
      let markers = 0;
      git.commitWipMarker = async () => { markers++; return false; };
      const f = fixture({ safety: supervisedSafety(mode),
        runner: { quiesceRun: async () => ({ ...QUIESCENT, process: undefined }) } });
      await f.runner.execute(f.claim);
      assert.equal(parks().length, mode === "empty" ? 1 : 0);
      assert.equal(markers, mode === "empty" ? 3 : 0);
      assert.equal(fs.existsSync(f.clone()), true);
    });
  }

  for (const site of ["reclaim", "capture", "coincident"] as const) {
    for (const control of ["cancel", "pause", "shutdown"] as const) {
      it(`${control} during ${site} wins over full occupancy`, { skip: site === "capture" && LINUX_CAPTURE }, async () => {
        let signal: AbortSignal;
        let fired = false;
        const interrupt = async () => {
          if (fired) return;
          fired = true;
          if (control === "shutdown") f.runner.shutdown();
          else api.setInputs(f.claim.run_id, [{ id: 1, kind: control, ...(control === "pause" ? { body: "now" } : {}) }]);
          await waitForAbort(signal);
        };
        const f = fixture({ observe: (ctx) => { signal = ctx.signal!; },
          beforeReject: site === "coincident" ? interrupt : undefined,
          reclaim: site === "reclaim" ? interrupt : undefined });
        if (control === "shutdown" && site === "coincident") git.commitWipMarker = async () => false;
        if (site === "capture") {
          const status = git.worktreeStatus.bind(git);
          git.worktreeStatus = async (...args) => { await interrupt(); return status(...args); };
        }
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), 1);
        assert.equal(parks().filter((s) => s.body.recovery_cause === "data_volume_full").length, 0);
        if (control === "shutdown") {
          assert.equal(api.states.some((s) => s.body.status === "failed"), false);
          // A pre-existing shutdown uses its original checkpoint/clone retirement path.
          // Interrupts after this policy begins must retain the still-unverified clone.
          if (site !== "coincident") assert.equal(fs.existsSync(f.clone()), true);
          assert.equal(fs.existsSync(path.join(f.home, "session")), true);
        } else if (control === "pause") assert.equal(api.states.some((s) => s.body.status === "failed"), false);
        else assert.equal(api.states.find((s) => s.body.status === "failed")?.body.failure_reason, "run cancelled");
      });
    }
  }

  for (const location of ["source", "proof", "post-WIP proof", "report"] as const) {
    it(`typed plan rejection raised during ${location} uses canonical failure`, { skip: location !== "proof" && LINUX_CAPTURE }, async () => {
      const error = new PlanRejectedError("capture policy rejected");
      const f = fixture({ runner: { quiesceRun: async (req) => {
        if ((location === "proof" && req.site === "terminal_disk:after_runner_git")
          || (location === "post-WIP proof" && req.site === "recovery_capture:after_runner_git")) throw error;
        return QUIESCENT;
      } } });
      if (location === "source") git.commitWipMarker = async () => { throw error; };
      const report = client.reportState.bind(client);
      if (location === "report") client.reportState = async (id, body, signal) => {
        if (body.status === "recovery_wait") throw error;
        return report(id, body, signal);
      };
      try {
        await f.runner.execute(f.claim);
        assert.equal(parks().length, 0);
        assert.equal(api.states.find((s) => s.body.status === "failed")?.body.failure_reason, error.message);
      } finally { client.reportState = report; }
    });
  }

  for (const deferral of ["vault_locked", "refresh_unknown"] as const) {
    for (const location of ["boundary", "post-WIP proof"] as const) {
      it(`full-volume opaque rejection redispatches ${location} ${deferral} into credential-free recovery`, async () => {
        let deferred = false;
        let settles = 0;
        let unverified = 0;
        const boundaries: string[] = [];
        const interruption = new CodexBoundaryError("reconcile", [], undefined, deferral);
        const safety = supervisedSafety("empty");
        const boundary = safety.withBoundary.bind(safety);
        safety.withBoundary = async (request, action) => {
          assert.equal(deferred, false, "no credentialed boundary after the deferral");
          boundaries.push(request.boundary);
          if (location === "boundary" && request.boundary === "shutdown") {
            deferred = true;
            throw interruption;
          }
          return boundary(request, action);
        };
        const f = fixture({ safety,
          features: ["recovery_cause_data_volume_full", "claim_generation_fence", "recovery_cause_vault_locked"],
          settleForCredentialFreeCapture: async () => {
            assert.equal(deferred, true);
            settles++;
            return { kind: "observed_empty" };
          },
          runner: { quiesceRun: async (req) => {
            if (!deferred && location === "post-WIP proof" && req.site === "recovery_capture:after_runner_git") {
              deferred = true;
              throw interruption;
            }
            return QUIESCENT;
          } },
        });
        let custodySettles = 0;
        const custody = f.runner as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;
        for (const name of ["reapThenSettleRecoveryGeneration", "settleRecoveryGeneration"]) {
          const original = custody[name]!.bind(f.runner);
          custody[name] = async (...args) => { custodySettles++; return original(...args); };
        }
        const verify = git.verifyRunnerTrackingCovers.bind(git);
        git.verifyRunnerTrackingCovers = async (...args) => {
          if (deferred && unverified++ < 4) {
            assert.equal(settles, 1, "credential-free settlement precedes recapture");
            assert.equal(fs.existsSync(f.clone()), true);
            assert.equal(fs.existsSync(path.join(f.home, "session")), true);
            assert.equal(custodySettles, 0);
            return false;
          }
          return verify(...args);
        };
        await f.runner.execute(f.claim);
        assert.equal(deferred, true);
        assert.equal(f.calls(), 1, "the executor was never replayed");
        assert.equal(f.reclaims(), 1, "canonical redispatch never re-enters disk admission");
        assert.equal(settles, 1);
        assert.equal(unverified, 5, "credential deferrals outlive the three-attempt disk capture budget");
        assert.deepEqual(boundaries, ["shutdown"]);
        assert.equal(custodySettles, 0, "custody is retained");
        assert.equal(parks().length, 1);
        assert.equal(parks()[0]!.body.recovery_cause, deferral === "vault_locked" ? "vault_locked" : undefined);
        assert.equal(parks()[0]!.body.claim_generation, 4);
        assert.equal(api.states.some((state) => state.body.status === "failed" || state.body.status === "completed"), false);
        assert.equal(f.forge.calls.length, 0);
        assert.equal(api.completionPermitRequests.length, 0);
        assert.equal(fs.existsSync(path.join(f.home, "session")), true);
        const bare = git.barePathFor(fx.originPath);
        assert.equal(readGit(bare, "show", "refs/uzi-runner/agent/issue-2201:COMMITTED.txt"), "committed sentinel");
        assert.equal(readGit(bare, "show", "refs/uzi-runner/agent/issue-2201:DIRTY.txt"), "dirty sentinel");
        const feed = api.messages(f.claim.run_id).filter((message) => message.kind === "status")
          .map((message) => String((message.payload as { text?: unknown }).text));
        assert.equal(feed.filter((text) => text === "Recovery checkpoint could not be verified. Keeping the local work and session and retrying before pausing.").length, 1);
        assert.ok(feed.includes(deferral === "vault_locked"
          ? "Paused: the run owner's vault is locked. The recovery checkpoint is saved only on this worker; this run resumes automatically at its next retry once the vault is unlocked."
          : "Paused for credential recovery. The recovery checkpoint is saved only on this worker; this run can resume at its next retry."));
        if (deferral === "refresh_unknown") assert.ok(feed.every((text) => !/vault/i.test(text)));
      });
    }
  }

  it("alternating safety blocks permits at most fifteen captures and five separate final proofs", { skip: LINUX_CAPTURE }, async () => {
    let captures = 0;
    let finals = 0;
    let markers = 0;
    git.commitWipMarker = async () => { markers++; return false; };
    const blocked = { ...QUIESCENT, process: { state: "unverified" as const, processes: [], killed: [], detail: "unknown" } };
    const f = fixture({ runner: { quiesceRun: async (req) => {
      if (req.site !== "terminal_disk:after_runner_git") return QUIESCENT;
      if (markers === 3) { finals++; return blocked; }
      captures++;
      return captures % 5 === 0 ? QUIESCENT : blocked;
    } } });
    await f.runner.execute(f.claim);
    assert.equal(captures, 15);
    assert.equal(markers, 3);
    assert.equal(finals, 5);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.fail_origin, "worker_residue_blocked");
  });

  for (const verified of [false, true]) {
    it(`lost and statusless ACKs keep the original capture budget, verified=${verified}`, { skip: LINUX_CAPTURE }, async () => {
      let markers = 0;
      const marker = git.commitWipMarker.bind(git);
      git.commitWipMarker = async (...args) => { markers++; return verified ? marker(...args) : false; };
      const f = fixture();
      const report = client.reportState.bind(client);
      let sends = 0;
      const latest: unknown[] = [];
      client.reportState = async (id, body, signal) => {
        if (body.status === "recovery_wait") {
          sends++;
          latest.push(body.checkpoint_contains_latest);
          assert.equal(body.recovery_cause, "data_volume_full");
          if (sends === 1) throw new Error("lost reply");
          if (sends === 2) return { applied: false };
          api.overrideStateStatus(id, "failed");
        }
        return report(id, body, signal);
      };
      try {
        await f.runner.execute(f.claim);
        assert.equal(sends, 3);
        assert.equal(markers, verified ? 1 : 3);
        assert.equal(f.calls(), 1);
        assert.equal(f.reclaims(), 1);
        assert.deepEqual(latest, [undefined, undefined, undefined], "no publish ACK means no latest-checkpoint claim");
        assert.equal(api.states.filter((s) => s.body.status === "failed").length, 0, "cap ACK causes no extra failure report");
        assert.equal(fs.existsSync(f.clone()), false, "server-terminal cap uses normal cleanup");
      } finally { client.reportState = report; }
    });
  }

  for (const [name, sample, error] of [
    ["opaque bytes", FULL, new Error("the agent exited without detail")],
    ["opaque inodes with roomy bytes", { ...ROOMY, ffree: 1 }, "opaque value"],
    ["undefined rejection", FULL, undefined],
    ["NaN rejection", FULL, NaN],
  ] as const) {
    it(name, { skip: LINUX_CAPTURE }, async () => {
      const f = fixture({ sample, error });
      await f.runner.execute(f.claim);
      assert.equal(f.calls(), 1);
      assert.equal(f.reclaims(), 1);
      assert.equal(parks().length, 1);
      assert.equal(parks()[0]!.body.recovery_cause, "data_volume_full");
      assert.equal(parks()[0]!.body.claim_generation, 4);
      assert.notEqual(parks()[0]!.body.disk_park_preventive, true);
      assert.equal(api.states.some((s) => s.body.status === "failed" || s.body.status === "completed"), false);
      assert.equal(f.forge.calls.length, 0, "no finalize/MR");
      assert.equal(fs.existsSync(path.join(f.home, "session")), true);
      const bare = git.barePathFor(fx.originPath);
      assert.equal(readGit(bare, "show", "refs/uzi-runner/agent/issue-2201:COMMITTED.txt"), "committed sentinel");
      assert.equal(readGit(bare, "show", "refs/uzi-runner/agent/issue-2201:DIRTY.txt"), "dirty sentinel");
    });
  }
  it("fresh generations exhaust the counted disk cap without replay or a second failure", { skip: LINUX_CAPTURE }, async () => {
    const options = { generation: 4 };
    const f = fixture(options);
    const report = client.reportState.bind(client);
    let counted = 0;
    const records: { body: StateRequest; ack: StateAck }[] = [];
    client.reportState = async (id, body, signal) => {
      if (body.status !== "recovery_wait") return report(id, body, signal);
      assert.equal(body.claim_generation, options.generation);
      assert.equal(body.recovery_cause, "data_volume_full");
      assert.notEqual(body.disk_park_preventive, true);
      if (body.recovery_cause === "data_volume_full" && body.disk_park_preventive !== true) counted++;
      const status = counted > 3 ? "failed" : "recovery_wait";
      api.overrideStateStatus(id, status);
      const ack = await report(id, body, signal);
      api.setOwnershipStatus(id, status, options.generation);
      records.push({ body, ack });
      return ack;
    };
    try {
      // Four explicit fresh claims are the only admission to another executor turn.
      // Each generation starts roomy; the executor makes the volume full before rejecting.
      for (let leg = 0; leg < 4; leg++) {
        options.generation = 4 + leg;
        f.claim.claim_generation = options.generation;
        f.setRoomy();
        api.setOwnershipStatus(f.claim.run_id, "claimed", options.generation);
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), leg + 1);
        assert.equal(f.reclaims(), leg + 1);
        assert.equal(counted, leg + 1);
        assert.equal(records.length, leg + 1);
        await new Promise((resolve) => setTimeout(resolve, 10));
        assert.equal(f.calls(), leg + 1, "park/terminal ACK cannot replay the executor");
        if (leg < 3) assert.equal(fs.existsSync(path.join(f.home, "session")), true);
      }
      assert.deepEqual(records.map((r) => r.ack.status), ["recovery_wait", "recovery_wait", "recovery_wait", "failed"]);
      assert.deepEqual(records.map((r) => r.body.claim_generation), [4, 5, 6, 7]);
      // Server origin stamping is proved by TestDiskParkCapFailsRunLiveDB, not this ACK model.
      assert.equal(api.states.filter((s) => s.body.status === "failed").length, 0, "no second generic failure report");
      assert.equal(f.forge.calls.length, 0, "no finalize/MR");
      assert.equal(fs.existsSync(f.clone()), false, "terminal cap ACK cleans up the clone");
      assert.equal(fs.existsSync(f.home), false, "terminal cap ACK cleans up the session");
    } finally { client.reportState = report; }
  });

  it("reclaim relief does not restart execution or withdraw the counted park", { skip: LINUX_CAPTURE }, async () => {
    const f = fixture({ reclaim: async () => f.setRoomy() });
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 1);
    assert.equal(f.reclaims(), 1);
    assert.equal(parks().length, 1);
  });

  for (const [name, options] of [
    ["roomy", { sample: ROOMY }], ["unknown statfs", { unknown: true }],
    ["missing files", { sample: { ...FULL, files: undefined } }],
    ["missing ffree with positive files", { sample: { ...FULL, ffree: undefined } }],
    ["HOME device mismatch", { mismatch: "home" }], ["clone device mismatch", { mismatch: "clone" }],
    ["HOME stat missing", { missingStat: "home" }], ["clone stat missing", { missingStat: "clone" }],
    ["old API", { features: ["claim_generation_fence"] }], ["no stamping", { features: ["recovery_cause_data_volume_full"] }],
    ["missing ownership generation", { generation: null }], ["awaiting approval revision", { status: "awaiting_approval" }],
  ] as const) {
    it(`keeps failure for ${name}`, async () => {
      const f = fixture(options);
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(api.states.filter((s) => s.body.status === "failed").length, 1);
    });
  }
  for (const options of [{ generation: 5 }, { status: "completed" }]) {
    it(`stops silently for confirmed ${JSON.stringify(options)}`, async () => {
      const f = fixture(options);
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(api.states.some((s) => s.body.status === "failed"), false);
    });
  }

  // Audit data travels with the reusable cases in the milestone checkpoint.
  for (const entry of [...trustedExecutionCases, ...trustedEnvelopeCases]) {
    it(`trusted refusal: ${entry.reason} [${entry.representation}]`, async () => {
      const f = fixture({ error: entry.error() });
      await f.runner.execute(f.claim);
      assert.equal(f.calls(), 1, "one executor call");
      assert.equal(f.forge.calls.length, 0, "no finalize/MR");
      assert.equal(f.reclaims(), 0, "trusted rejection must precede reclaim");
      assert.equal(parks().length, 0, "trusted rejection must never park");
      const failures = api.states.filter((s) => s.body.status === "failed");
      assert.equal(failures.length, 1, "one terminal failure");
      assert.equal(failures[0]!.body.failure_reason, entry.display, "preserve original display reason");
      if (entry.display === "the planning turn ended without a plan or a structured question after a corrective nudge") {
        assert.equal(failures[0]!.body.fail_origin, "plan_missing", "retain plan-missing disposition");
      }
      if (entry.display === "tool provisioning failed before the agent could start: invalid run id") {
        assert.equal(failures[0]!.body.fail_origin, "provisioning_failed", "retain provisioning disposition");
      }
      if (entry.display === "no Anthropic OAuth token was provided for this run") {
        assert.equal(failures[0]!.body.fail_origin, "credential_unavailable", "retain credential disposition");
      }
      assert.equal(api.states.some((s) => s.body.status === "completed"), false);
    });
  }

  for (const [name, error] of trustedBoundaryCases) {
    it(`trusted boundary: ${name}`, async () => {
      const rejection = error();
      const f = fixture({ error: rejection });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(f.forge.calls.length, 0);
      const failures = api.states.filter((s) => s.body.status === "failed");
      assert.equal(failures.length, 1);
      assert.equal(failures[0]!.body.failure_reason, rejection.message);
    });
  }
  for (const [name, error] of opaqueBoundaryCases) {
    it(`opaque boundary defers: ${name}`, { skip: LINUX_CAPTURE }, async () => {
      const f = fixture({ error: error() });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 1);
      assert.equal(parks().length, 1);
      assert.equal(parks()[0]!.body.recovery_cause, "data_volume_full");
      assert.equal(api.states.some((s) => s.body.status === "failed" || s.body.status === "completed"), false);
      assert.equal(f.forge.calls.length, 0);
    });
  }

  for (const entry of trustedCaptureCases) {
    for (const site of ["source", "fetch"] as const) {
      it(`trusted capture ${site}: ${entry.reason} [${entry.representation}]`, { skip: LINUX_CAPTURE }, async () => {
        let captures = 0;
        const reject = async () => { captures++; throw entry.error(); };
        if (site === "source") git.commitWipMarker = reject;
        else git.fetchAgentBranch = reject;
        const f = fixture();
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), 1, "one executor call");
        assert.equal(f.forge.calls.length, 0, "no finalize/MR");
        assert.equal(captures, 1, "propagate once, without capture retry");
        assert.equal(f.reclaims(), 1, "only original opaque rejection reclaims");
        assert.equal(parks().length, 0, "capture refusal cannot park");
        const failures = api.states.filter((s) => s.body.status === "failed");
        assert.equal(failures.length, 1, "one terminal failure");
        assert.equal(failures[0]!.body.failure_reason, entry.display);
      });
    }
  }

  class UnknownError extends Error {}
  class NativeNamedError extends Error { constructor() { super("opaque"); this.name = "Error"; } }
  for (const error of [
    new UnknownError("opaque"), new NativeNamedError(), new TypeError("opaque"),
    Object.assign(new Error("opaque"), { name: "UnknownError" }),
    new Error("wrapped", { cause: new UnknownError("opaque") }),
    new Error("wrapped", { cause: new PlanRejectedError("not approved") }),
    new Error("wrapped", { cause: new DataVolumeFullError("typed write", new Error("full")) }),
    new Error("wrapped", { cause: new Error("denied by guardrail: other trusted policy") }),
    new Error("wrapped", { cause: new Error("run cancelled") }),
    new Error("run cancelled"), new Error("codex plan revision budget exhausted"),
    new Error("Codex claim block is missing a valid capability"),
    new Error("command cwd escapes the worktree sandbox"),
    new Error("denied by guardrail: reading the process environment is not permitted"),
    new Error("plan-gate input delivery failed: refused"),
  ]) {
    it(`preserves typed/control/security failure: ${error.name}: ${error.message}`, async () => {
      const f = fixture({ error });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(api.states.some((s) => s.body.status === "failed"), true);
    });
  }

  for (const [name, error] of [
    ["plain object", { message: "command cwd escapes the worktree sandbox" }],
    ["string", "denied by guardrail: reading the process environment is not permitted"],
    ["string cause", new Error("wrapped", { cause: "denied by guardrail: reading the process environment is not permitted" })],
  ] as const) {
    it(`protects security rejection represented as ${name}`, async () => {
      const f = fixture({ error });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(api.states.filter((s) => s.body.status === "failed").length, 1);
    });
  }

  for (const error of [
    new Error("wrapped", { cause: new PlanRejectedError("capture policy rejected") }),
    new Error("denied by guardrail: other trusted policy"),
    { message: "command cwd escapes the worktree sandbox" },
    new Error("wrapped", { cause: "denied by guardrail: other trusted policy" }),
  ]) {
    it(`propagates protected capture failure: ${String(error)}`, { skip: LINUX_CAPTURE }, async () => {
      let markers = 0;
      git.commitWipMarker = async () => { markers++; throw error; };
      const f = fixture();
      await f.runner.execute(f.claim);
      assert.equal(markers, 1, "protected failures cannot become repeated capture attempts");
      assert.equal(f.reclaims(), 1);
      assert.equal(parks().length, 0);
      assert.equal(api.states.filter((s) => s.body.status === "failed").length, 1);
    });
  }

  for (const failure of ["wip", "fetch"] as const) {
    it(`bounds persistent real-Git ${failure} failure and retains both sentinels plus session/journal`, { skip: LINUX_CAPTURE }, async () => {
      const marker = git.commitWipMarker.bind(git);
      const fetch = git.fetchAgentBranch.bind(git);
      let attempts = 0;
      if (failure === "wip") git.commitWipMarker = async () => { attempts++; return false; };
      else git.fetchAgentBranch = async () => { attempts++; throw new Error("persistent fetch failure"); };
      const f = fixture({ runner: { dockerHost: "unix:///fixture-no-daemon" } });
      await f.runner.execute(f.claim);
      assert.equal(attempts, 3);
      assert.equal(f.calls(), 1);
      assert.equal(f.reclaims(), 1);
      assert.equal(parks().length, 1);
      assert.equal(fs.readFileSync(path.join(f.clone(), "COMMITTED.txt"), "utf8"), "committed sentinel\n");
      assert.equal(fs.readFileSync(path.join(f.clone(), "DIRTY.txt"), "utf8"), "dirty sentinel\n");
      assert.equal(fs.readFileSync(path.join(f.home, "session"), "utf8"), "transcript");
      assert.equal(parks()[0]!.body.checkpoint_contains_latest, undefined);
      const journal = JSON.parse(readGit(git.barePathFor(fx.originPath), "config", "--local", "--get", "uzi-recovery.agent/issue-2201.clone")) as { clonePath: string; runId: string; attemptId: string };
      assert.equal(journal.clonePath, f.clone());
      assert.equal(journal.runId, f.claim.run_id);
      assert.ok(journal.attemptId);
      assert.equal(fs.existsSync(journal.clonePath), true);
      git.commitWipMarker = marker;
      git.fetchAgentBranch = fetch;
      f.setRoomy();
      // Retained predecessor capture must happen before the resumed executor can read the tree.
      // A capture-only reclaim is allowed; a fresh next generation then reads BOTH sentinels.
      const { gitlab } = fakeGitlab();
      let resumedCalls = 0;
      const resumed = runnerWith(() => ({ homeDir: f.home, executor: { run: async (ctx) => {
        resumedCalls++;
        assert.notEqual(ctx.worktreePath, journal.clonePath, "successor executes in a fresh attempt path");
        assert.match(ctx.worktreePath, /\.attempt-/);
        assert.equal(fs.readFileSync(path.join(f.home, "session"), "utf8"), "transcript");
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "COMMITTED.txt"), "utf8"), "committed sentinel\n");
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "DIRTY.txt"), "utf8"), "dirty sentinel\n");
        throw new PlanRejectedError("stop after durability proof");
      } } }), gitlab, undefined, nullLogger(), { quiesceRun: async () => QUIESCENT, dockerHost: "unix:///fixture-no-daemon" });
      const next = { ...f.claim, claim_generation: 5 };
      api.setOwnershipStatus(next.run_id, "running", 5);
      await resumed.execute(next);
      if (!resumedCalls) {
        api.setOwnershipStatus(next.run_id, "running", 6);
        await resumed.execute({ ...next, claim_generation: 6 });
      }
      assert.equal(resumedCalls, 1, "fresh resumed executor read both independently durable sentinels");
    });
  }

  it("five consecutive safety blocks fail retained, without a disk park", async () => {
    let proofs = 0;
    const f = fixture({ runner: { quiesceRun: async (req) => {
      if (req.site === "terminal_disk:after_runner_git") {
        proofs++;
        return { ...QUIESCENT, process: { state: "unverified", processes: [], killed: [], detail: "unknown process" } };
      }
      return QUIESCENT;
    } } });
    await f.runner.execute(f.claim);
    assert.equal(proofs, 5);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.fail_origin, "worker_residue_blocked");
    assert.equal(fs.existsSync(f.clone()), true);
    assert.equal(fs.existsSync(path.join(f.home, "session")), true);
  });

  it("absent process proof cannot authorize degraded capture", async () => {
    const f = fixture({ runner: { quiesceRun: async () => ({ ...QUIESCENT, process: undefined }) } });
    await f.runner.execute(f.claim);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.fail_origin, "worker_residue_blocked");
    assert.equal(fs.existsSync(f.clone()), true);
  });

  for (const [name, error, expectedPark] of [
    ["disk stop", new DiskParkSignal(false), true],
    ["transient recovery", new TransientRecoveryError(), true],
    ["plan rejected", new PlanRejectedError("rejected"), false],
  ] as const) {
    it(`keeps existing ${name} disposition without occupancy reclaim`, async () => {
      const f = fixture({ error });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, expectedPark ? 1 : 0);
      assert.equal(f.calls(), 1);
    });
  }
});
