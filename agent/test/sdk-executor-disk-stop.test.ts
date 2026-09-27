import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext } from "../src/executor.js";
import { PauseNowSignal, type PauseMode, type PlanVerdict } from "../src/steering.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

/**
 * PRD #1809 D4 — the SdkExecutor side of the cache cap and the pressure stop:
 *  - at every implement turn boundary it asks ctx.cacheCapBoundary, handing it a quiet-point
 *    probe built on its own process tracking (a spawned CLI's process group still having
 *    members = a live process of the run); "park" throws a PREVENTIVE DiskParkSignal;
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
