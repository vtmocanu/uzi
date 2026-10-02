import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkExecutorOptions, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext } from "../src/executor.js";
import { CredentialSwitchSignal, PauseNowSignal, type PauseMode, type PlanVerdict } from "../src/steering.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

/**
 * PRD #1809 D4 — the SdkExecutor side of the cache cap and the pressure stop:
 *  - at every implement turn boundary it asks ctx.cacheCapBoundary, handing it a quiet-point
 *    probe: a spawned CLI's process group still having members, OR a process attributed to the
 *    run by HOME / working directory (the CLI runs each Bash command detached, outside its groups;
 *    run-procs.ts), is a live process of the run; "park" throws a PREVENTIVE DiskParkSignal;
 *  - a `disk` stop (the steering channel's worker-local pause mode, a PauseNowSignal abort) drops
 *    the live turn and throws a COUNTED DiskParkSignal, never an owner pause park.
 * Harness mirrors sdk-executor-wall.test.ts (its helpers are not exported).
 */

function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function signalDone(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}
/** A turn that ended without signal_done: the loop goes round to its next boundary. */
function quietTurn(sessionId = "sess-1"): SDKMessage[] {
  return [resultSuccess(sessionId)];
}

type Script = SDKMessage[] | ((turnSignal: AbortSignal) => AsyncIterable<unknown>);

function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    // NEVER YIELDING IS THE FIXTURE, same as sdk-executor-wall.test.ts.
    // eslint-disable-next-line require-yield
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener(
          "abort",
          () => {
            clearInterval(keepAlive);
            resolve();
          },
          { once: true },
        );
      });
    },
  };
}

/** One scripted stream per turn; each turn "spawns" the CLI first so a pid is tracked. */
function spawningTurns(scripts: Script[]): SdkQueryFn {
  let i = 0;
  return (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      params.options.spawnClaudeCodeProcess?.({ command: "x", args: [] } as never);
      for await (const _ of params.prompt) {
        /* drain */
      }
      if (typeof script === "function") {
        yield* script(params.options.abortController!.signal) as AsyncIterable<SDKMessage>;
      } else {
        for (const m of script) yield m;
      }
    })();
  };
}

let homeDir: string;
let saved: Record<string, string | undefined>;
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-diskstop");

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; mode: { value: PauseMode }; ownerPauses: number[] } {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const mode: { value: PauseMode } = { value: null };
  const ownerPauses: number[] = [];
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: () => {},
    oauthToken: "dummy-oauth-token-do-not-scan-0000",
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: () => {},
    gatePlan: async () => approve,
    pullFollowUp: () => undefined,
    reportIteration: () => {},
    checkpoint: async () => {},
    worktreeFingerprint: async () => "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
    pauseModeRequested: () => mode.value,
    parkForPause: async (at) => {
      ownerPauses.push(at.completedCount);
      return true;
    },
    ...overrides,
  };
  return { ctx, mode, ownerPauses };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-diskstophome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = "dummy-join-token-do-not-scan-2222";
  process.env.UZI_FORGE_PAT = "dummy-forge-pat-do-not-scan-1111";
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("SdkExecutor — PRD #1809 D4 cache cap boundary", () => {
  it("asks the cap at every implement turn boundary and parks PREVENTIVELY when it says so", async () => {
    let boundaries = 0;
    const { ctx } = makeCtx({
      cacheCapBoundary: async () => (++boundaries >= 2 ? "park" : "continue"),
    });
    let pid = 7000;
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], quietTurn(), quietTurn()]),
      spawn: () => ({ pid: ++pid }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    await assert.rejects(exec.run(ctx), (err: unknown) => {
      assert.ok(err instanceof DiskParkSignal, `a DiskParkSignal, got ${String(err)}`);
      assert.strictEqual(err.preventive, true, "the soft layer's park is preventive (uncounted)");
      return true;
    });
    assert.strictEqual(boundaries, 2, "one check before each implement turn; the second parked before driving a turn");
  });

  it("the quiet-point probe reports a live process while a spawned CLI's process group still has members", async () => {
    const probes: boolean[] = [];
    const groups = new Map<number, boolean>();
    let pid = 7100;
    const { ctx } = makeCtx({
      cacheCapBoundary: async (processAlive) => {
        probes.push(await processAlive());
        return "continue";
      },
    });
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], quietTurn(), [signalDone(), resultSuccess()]]),
      spawn: () => {
        pid++;
        // The plan turn's CLI (7101) left a background job behind in its group; later CLIs exited.
        groups.set(pid, pid === 7101);
        return { pid };
      },
      kill: () => true,
      cliGroupPresent: (pgid) => groups.get(pgid) ?? false,
      quietSettleMs: 50,
    });
    await exec.run(ctx);
    assert.deepStrictEqual(probes, [true, true], "a background process of the run blocks the quiet point at every boundary");

    const quiet: boolean[] = [];
    const { ctx: ctx2 } = makeCtx({
      cacheCapBoundary: async (processAlive) => {
        quiet.push(await processAlive());
        return "continue";
      },
    });
    const exec2 = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: ++pid }),
      kill: () => true,
      cliGroupPresent: () => false,
      quietSettleMs: 50,
      // No process attributed to the run either (the real scan would read this host's proc tree).
      runProcesses: { scan: async () => ({ pids: [], complete: true }), reap: async () => ({ killed: [], left: [], complete: true }) },
    });
    await exec2.run(ctx2);
    assert.deepStrictEqual(quiet, [false], "every group gone: a proven quiet point");
  });

  it("an unknowable group counts as alive (fail closed)", async () => {
    const probes: boolean[] = [];
    const { ctx } = makeCtx({
      cacheCapBoundary: async (processAlive) => {
        probes.push(await processAlive());
        return "continue";
      },
    });
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 7200 }),
      kill: () => true,
      cliGroupPresent: () => undefined,
      quietSettleMs: 20,
    });
    await exec.run(ctx);
    assert.deepStrictEqual(probes, [true]);
  });
});

describe("SdkExecutor — PRD #1809 D4 `disk` stop", () => {
  it("a `disk` stop mid-turn drops the turn and throws a COUNTED DiskParkSignal, never an owner pause park", async () => {
    const runSignal = new AbortController();
    const holder: { mode?: { value: PauseMode } } = {};
    const killed: (number | undefined)[] = [];
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        (signal) => ({
          async *[Symbol.asyncIterator]() {
            // What SteeringChannel.requestDiskStop does: the sticky mode, then the PauseNowSignal abort.
            holder.mode!.value = "disk";
            runSignal.abort(new PauseNowSignal());
            yield* hangUntilAbort(signal);
          },
        }),
      ]),
      spawn: () => ({ pid: 7300 }),
      kill: (p) => (killed.push(p), true),
      cliGroupPresent: () => false,
    });
    const { ctx, mode, ownerPauses } = makeCtx({ signal: runSignal.signal });
    holder.mode = mode;
    await assert.rejects(exec.run(ctx), (err: unknown) => {
      assert.ok(err instanceof DiskParkSignal, `a DiskParkSignal, got ${String(err)}`);
      assert.strictEqual(err.preventive, false, "the hard layer's park is counted");
      return true;
    });
    assert.deepStrictEqual(ownerPauses, [], "a disk stop is never an owner pause park");
    assert.ok(killed.includes(7300), "the stopped turn's CLI process group was killed");
  });
});

describe("SdkExecutor — PRD #1809 D4 a `disk` stop is never lost to a path that clears the trip (BLOCKING 2)", () => {
  // Each race: the stop lands while another trip's handler runs (the stop's own turn drop loses to
  // it: trip() is first-wins), and that handler then clears the trip and continues. The next turn
  // would finish the run (signal_done); the stop must park it first, COUNTED.
  const expectCountedDiskPark = (err: unknown): boolean => {
    assert.ok(err instanceof DiskParkSignal, `a DiskParkSignal, got ${String(err)}`);
    assert.strictEqual(err.preventive, false);
    return true;
  };

  it("a stop racing a DECLINED owner `now` park", async () => {
    const runSignal = new AbortController();
    const holder: { mode?: { value: PauseMode } } = {};
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        (signal) => ({
          async *[Symbol.asyncIterator]() {
            holder.mode!.value = "now";
            runSignal.abort(new PauseNowSignal());
            yield* hangUntilAbort(signal);
          },
        }),
        [signalDone(), resultSuccess()],
      ]),
      spawn: () => ({ pid: 7400 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx, mode } = makeCtx({
      signal: runSignal.signal,
      parkForPause: async () => {
        mode.value = "disk"; // the hard layer's stop, while the owner park is being declined
        return false;
      },
    });
    holder.mode = mode;
    await assert.rejects(exec.run(ctx), expectCountedDiskPark);
  });

  it("a stop racing a credential switch that GAVE UP (implement turn)", async () => {
    let switchTrip: () => void = () => {};
    const holder: { mode?: { value: PauseMode } } = {};
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        (signal) => ({
          async *[Symbol.asyncIterator]() {
            switchTrip();
            yield* hangUntilAbort(signal);
          },
        }),
        [signalDone(), resultSuccess()],
      ]),
      spawn: () => ({ pid: 7500 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx, mode } = makeCtx({
      onCredentialSwitch: (cb) => (switchTrip = cb),
      attemptCredentialSwitch: async () => {
        mode.value = "disk";
        return "gave_up";
      },
    });
    holder.mode = mode;
    await assert.rejects(exec.run(ctx), expectCountedDiskPark);
  });

  it("a stop racing a credential switch that GAVE UP inside a wait (runThroughSwitch)", async () => {
    let gates = 0;
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 7600 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx, mode } = makeCtx({
      gatePlan: async () => {
        gates++;
        if (gates === 1) throw new CredentialSwitchSignal();
        return { kind: "approve", selection: { status: "absent" } };
      },
      attemptCredentialSwitch: async () => {
        mode.value = "disk";
        return "gave_up";
      },
    });
    await assert.rejects(exec.run(ctx), expectCountedDiskPark);
    assert.strictEqual(gates, 1, "the wait is not re-run: the stop parks first");
  });

  it("a stop racing a REFUSED wall park", async () => {
    const runSignal = new AbortController();
    const holder: { mode?: { value: PauseMode } } = {};
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        (signal) => ({
          async *[Symbol.asyncIterator]() {
            holder.mode!.value = "wall";
            runSignal.abort(new PauseNowSignal());
            yield* hangUntilAbort(signal);
          },
        }),
        [signalDone(), resultSuccess()],
      ]),
      spawn: () => ({ pid: 7700 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx, mode } = makeCtx({
      signal: runSignal.signal,
      parkForWall: async () => {
        mode.value = "disk";
        return "refused";
      },
      // As SteeringChannel.clearWallMode: only a `wall` mode is cleared.
      clearWallMode: () => {
        if (mode.value === "wall") mode.value = null;
      },
    });
    holder.mode = mode;
    await assert.rejects(exec.run(ctx), expectCountedDiskPark);
  });

  it("a stop set while the cap boundary measured wins over the soft park: COUNTED", async () => {
    const { ctx, mode } = makeCtx({
      cacheCapBoundary: async () => {
        mode.value = "disk";
        return "park";
      },
    });
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], quietTurn()]),
      spawn: () => ({ pid: 7800 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    await assert.rejects(exec.run(ctx), expectCountedDiskPark);
  });
});

describe("SdkExecutor — PRD #1809 D4 process attribution (BLOCKING 1)", () => {
  it("a detached background job outside every CLI group keeps the boundary not quiet; a clean scan is quiet", async () => {
    const probes: boolean[] = [];
    const scans: { home: string; worktree: string | undefined }[] = [];
    let attributed: number[] = [4242];
    const { ctx } = makeCtx({
      cacheCapBoundary: async (processAlive) => {
        probes.push(await processAlive());
        attributed = []; // the background job exits before the next boundary
        return "continue";
      },
    });
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], quietTurn(), [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 7900 }),
      kill: () => true,
      // Every CLI group is gone: the group probe alone would call this a quiet point.
      cliGroupPresent: () => false,
      quietSettleMs: 20,
      runProcesses: {
        scan: async (home, worktree) => {
          scans.push({ home, worktree });
          return { pids: attributed, complete: true };
        },
        reap: async () => ({ killed: [], left: [], complete: true }),
      },
    });
    await exec.run(ctx);
    assert.deepStrictEqual(probes, [true, false]);
    assert.strictEqual(scans[0]?.home, homeDir, "attributed by the run's own HOME");
    assert.strictEqual(scans[0]?.worktree, ctx.worktreePath, "and its worktree");
  });

  it("an incomplete (unknown) scan is not quiet", async () => {
    const probes: boolean[] = [];
    const { ctx } = makeCtx({
      cacheCapBoundary: async (processAlive) => {
        probes.push(await processAlive());
        return "continue";
      },
    });
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 7901 }),
      kill: () => true,
      cliGroupPresent: () => false,
      quietSettleMs: 20,
      runProcesses: {
        scan: async () => ({ pids: [], complete: false }),
        reap: async () => ({ killed: [], left: [], complete: true }),
      },
    });
    await exec.run(ctx);
    assert.deepStrictEqual(probes, [true]);
  });

  it("a CLI group confirmed gone (ESRCH) is pruned from the run-end reap (N7)", async () => {
    const killed: (number | undefined)[] = [];
    const { ctx } = makeCtx({ cacheCapBoundary: async (processAlive) => (await processAlive(), "continue") });
    let pid = 8000;
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: ++pid }),
      kill: (p) => (killed.push(p), true),
      cliGroupPresent: () => false,
      quietSettleMs: 20,
      runProcesses: {
        scan: async () => ({ pids: [], complete: true }),
        reap: async () => ({ killed: [], left: [], complete: true }),
      },
    });
    await exec.run(ctx);
    assert.ok(!killed.includes(8001), `the plan turn's CLI group (8001) was confirmed gone and never signalled: ${JSON.stringify(killed)}`);
    assert.ok(killed.includes(8002), "the last turn's CLI is still reaped at run end");
  });

  it("reapAttributedProcesses kills through the attribution with the run's HOME and worktree", async () => {
    const reaps: { home: string; worktree: string | undefined }[] = [];
    const { ctx } = makeCtx();
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 8100 }),
      kill: () => true,
      cliGroupPresent: () => false,
      runProcesses: {
        scan: async () => ({ pids: [], complete: true }),
        reap: async (home, worktree) => {
          reaps.push({ home, worktree });
          return { killed: [4243], left: [], complete: true };
        },
      },
    });
    await exec.run(ctx);
    await exec.reapAttributedProcesses();
    assert.deepStrictEqual(reaps, [{ home: homeDir, worktree: ctx.worktreePath }]);
  });
});

/** A planning turn that asks the owner a question instead of submitting a plan. */
function askUserTurn(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: {
      content: [{ type: "tool_use", id: "t3", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Which database?", header: "DB" }] } }],
    },
  } as unknown as SDKMessage;
}

// CHARACTERIZATION (issue #1830): the three tests below pass on the code from before the fix. They
// pin the existing executor behaviour the runner's reportState choke point relies on (a disk stop
// and a DiskParkSignal thrown by a wait callback both escape run() untouched); they do not guard
// the fix itself, which is covered through the runner in runner-disk-cap-park.test.ts.
describe("SdkExecutor — issue #1830 characterization: the propagation the choke point relies on", () => {
  it("pins the propagation the #1830 choke point relies on: a disk stop during the first planning turn rejects run() with the PauseNowSignal while the disk mode is pending", async () => {
    const runSignal = new AbortController();
    const holder: { mode?: { value: PauseMode } } = {};
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([
        (signal) => ({
          async *[Symbol.asyncIterator]() {
            // What SteeringChannel.requestDiskStop does, mid planning turn.
            holder.mode!.value = "disk";
            runSignal.abort(new PauseNowSignal());
            yield* hangUntilAbort(signal);
          },
        }),
      ]),
      spawn: () => ({ pid: 7900 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx, mode } = makeCtx({ signal: runSignal.signal });
    holder.mode = mode;
    await assert.rejects(exec.run(ctx), (err: unknown) => {
      assert.ok(err instanceof PauseNowSignal, `a PauseNowSignal, got ${String(err)}`);
      return true;
    });
    assert.strictEqual(mode.value, "disk", "the runner routes the escaped signal to the counted disk park from this mode");
  });

  it("pins the propagation the #1830 choke point relies on: a DiskParkSignal thrown by ctx.askUser in the planning phase propagates out of run()", async () => {
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[askUserTurn(), resultSuccess()]]),
      spawn: () => ({ pid: 8000 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx } = makeCtx({
      askUser: async () => {
        throw new DiskParkSignal(false);
      },
    });
    await assert.rejects(exec.run(ctx), (err: unknown) => err instanceof DiskParkSignal && err.preventive === false);
  });

  it("pins the propagation the #1830 choke point relies on: a DiskParkSignal thrown by ctx.askCompletionQuestion propagates out of run()", async () => {
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 8100 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    const { ctx } = makeCtx({
      config: { max_iterations: 10 },
      completionInterlock: true,
      recordCompletionAttempt: async () => ({ unmet: ["m1"], attemptCount: 1 }),
      enterCompletionHold: async () => true,
      askCompletionQuestion: async () => {
        throw new DiskParkSignal(false);
      },
    });
    await assert.rejects(exec.run(ctx), (err: unknown) => err instanceof DiskParkSignal && err.preventive === false);
  });
});

describe("SdkExecutor — issue #1830 reclaimCachesInPlace", () => {
  // The cache drop is a descriptor-pinned walk that refuses on a host without it.
  const HAS_PIN_WALK = fs.existsSync(path.join("/pro" + "c", "self", "fd"));

  it("keeps .npm/_cacache until the background deps install is joined, drops it after, and Go caches always", async (t) => {
    if (!HAS_PIN_WALK) return t.skip("no descriptor-pinned walk on this host: the cache drop refuses here by design");
    const seed = (): void => {
      fs.mkdirSync(path.join(homeDir, ".cache", "go-build", "0a"), { recursive: true });
      fs.writeFileSync(path.join(homeDir, ".cache", "go-build", "0a", "x-d"), "x");
      fs.mkdirSync(path.join(homeDir, "go", "pkg", "mod", "example.com"), { recursive: true });
      fs.mkdirSync(path.join(homeDir, ".npm", "_cacache", "index-v5"), { recursive: true });
    };
    seed();
    let releaseInstall: () => void = () => {};
    const installGate = new Promise<void>((r) => (releaseInstall = r));
    const installDeps: SdkExecutorOptions["installDeps"] = async () => {
      await installGate;
      return { results: [], truncated: false };
    };
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      spawn: () => ({ pid: 8200 }),
      kill: () => true,
      cliGroupPresent: () => false,
      installDeps,
    });
    const npm = path.join(homeDir, ".npm", "_cacache");
    const seen: { beforeJoin?: boolean[]; afterJoin?: boolean[] } = {};
    const { ctx } = makeCtx({
      gatePlan: async () => {
        // At the plan gate the install is still running (it is gated shut): the drop leaves its cache.
        await exec.reclaimCachesInPlace();
        seen.beforeJoin = [
          fs.existsSync(path.join(homeDir, ".cache", "go-build")),
          fs.existsSync(path.join(homeDir, "go", "pkg", "mod")),
          fs.existsSync(npm),
        ];
        releaseInstall();
        seed();
        return { kind: "approve", selection: { status: "absent" } };
      },
      // The first implement boundary comes after the join.
      cacheCapBoundary: async () => {
        await exec.reclaimCachesInPlace();
        seen.afterJoin ??= [
          fs.existsSync(path.join(homeDir, ".cache", "go-build")),
          fs.existsSync(path.join(homeDir, "go", "pkg", "mod")),
          fs.existsSync(npm),
        ];
        return "continue";
      },
    });
    await exec.run(ctx);
    assert.deepStrictEqual(seen.beforeJoin, [false, false, true], "Go caches dropped, the npm cache kept while the install runs");
    assert.deepStrictEqual(seen.afterJoin, [false, false, false], "after the join the npm cache goes too");
  });
});
