import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { CredentialSwitchSignal, PauseNowSignal } from "../src/steering.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

// PRD #1247 M5b — the SdkExecutor half of a held-state credential switch, driven directly (the
// pause-honor.test.ts harness shape): a switch that lands MID-TURN must abort the in-flight SDK
// turn so driveTurn throws a CredentialSwitchSignal, and that signal must PROPAGATE out of run()
// (the implement loop's turn catch does NOT swallow it, unlike a PauseNowSignal which it parks) so
// the runner's release state machine can take it. Two trip mechanisms, both exercised:
//   1. the shared abort controller aborted WITH a CredentialSwitchSignal reason (onSignal maps it),
//   2. the re-armable ctx.onCredentialSwitch interrupt (a switch that lands after the controller
//      was already spent) — mirrors ctx.onPauseNow.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

const nonexistentWorktree = nonexistentWorktreeFactory("uzi-switch-sdk");

function barrier<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);

function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; turnCount: () => number } {
  let i = 0;
  let count = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    count++;
    return (async function* () {
      for await (const _ of params.prompt) {
        /* drain */
      }
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turnCount: () => count };
}

/** A turn that hangs until its signal aborts — a genuinely in-flight turn to interrupt. */
function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    // eslint-disable-next-line require-yield
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener("abort", () => {
          clearInterval(keepAlive);
          resolve();
        }, { once: true });
      });
    },
  };
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emits: EmittedMessage[] } {
  const emits: EmittedMessage[] = [];
  const verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const ctx: RunContext = {
    runId: "r-switch",
    issueIid: 9,
    issueTitle: "Fix thing",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-9",
    emit: (m) => emits.push(m),
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    gatePlan: async () => verdict,
    pullFollowUps: () => [],
    checkpoint: async () => {},
    reportIteration: async () => undefined,
    ...overrides,
  };
  return { ctx, emits };
}

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-switch-sdk-home-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
});

afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("SdkExecutor — credential switch mid-turn (PRD #1247 M5b)", () => {
  it("propagates a CredentialSwitchSignal out of run() when the shared controller aborts with that reason mid-turn", async () => {
    // Plan turn, then an implement turn that HANGS until aborted. At iteration 1 the shared
    // controller is aborted WITH a CredentialSwitchSignal (what steering does) — onSignal maps the
    // reason to REASON_CREDENTIAL_SWITCH, driveTurn throws a CredentialSwitchSignal, and the turn
    // catch RE-THROWS it (it neither parks like a pause nor fails like a cancel).
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      (signal) => hangUntilAbort(signal),
    ]);
    const cancel = new AbortController();
    const { ctx } = makeCtx({
      signal: cancel.signal,
      reportIteration: async (n) => {
        if (n === 1) cancel.abort(new CredentialSwitchSignal());
        return undefined;
      },
    });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx),
      (e) => e instanceof CredentialSwitchSignal,
      "the mid-turn switch propagates as a CredentialSwitchSignal (never a cancel/failed, never a park)",
    );
  });

  it("propagates a CredentialSwitchSignal via the re-armable onCredentialSwitch interrupt (controller already spent)", async () => {
    // No controller abort here — the switch arrives through the re-armable interrupt the executor
    // registers (ctx.onCredentialSwitch), which trips the current turn with REASON_CREDENTIAL_SWITCH.
    // This is the path that fires when the shared controller has already been consumed.
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      (signal) => hangUntilAbort(signal),
    ]);
    let switchCb: (() => void) | undefined;
    const localAbort = new AbortController();
    const { ctx } = makeCtx({
      // The turn hangs on THIS signal; the interrupt's trip aborts the turn's own abort controller,
      // not this one, so we release the hang manually right after tripping.
      signal: localAbort.signal,
      onCredentialSwitch: (cb) => {
        switchCb = cb;
      },
      reportIteration: async (n) => {
        if (n === 1) {
          switchCb?.(); // trip via the re-armable interrupt (REASON_CREDENTIAL_SWITCH)
          localAbort.abort(); // release the hanging turn so driveTurn reaches its trip guard
        }
        return undefined;
      },
    });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx),
      (e) => e instanceof CredentialSwitchSignal,
      "the re-armable interrupt trips the turn as a credential switch",
    );
  });
});

// PRD #1247 M5b (data-integrity fix) — the executor now handles the switch IN PLACE via
// ctx.attemptCredentialSwitch instead of always propagating it. A "released" outcome ends the run
// with switchReleased surfaced (the runner skips finalize); a "gave_up" outcome CONTINUES the run on
// the OLD token (a restarted turn, or a re-presented gate/question/follow-up wait). Driven directly
// with a fake attemptCredentialSwitch so the dispatch is provable with no runner and no steering.

/** An implement/done turn: a text frame plus signal_done, then a success result. */
function doneTurn(sessionId = "sess-1"): SDKMessage[] {
  return [
    {
      type: "assistant",
      session_id: sessionId,
      message: {
        content: [
          { type: "text", text: "done implementing" },
          { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
        ],
      },
    } as unknown as SDKMessage,
    resultSuccess(sessionId),
  ];
}

describe("SdkExecutor — sticky terminal cancellation", () => {
  for (const interruption of ["switch", "pause"] as const) {
    it(`interrupts a live provider turn after a declined ${interruption} spent the shared listener`, { timeout: 10_000 }, async (t) => {
      const shared = new AbortController();
      const terminal = new AbortController();
      const resumed = barrier<AbortSignal>();
      t.after(() => terminal.abort());
      let recoveries = 0;
      const { queryFn, turnCount } = fakeTurns([
        [submitPlan("# Plan"), resultSuccess()],
        (signal) => {
          shared.abort(interruption === "switch" ? new CredentialSwitchSignal() : new PauseNowSignal());
          return hangUntilAbort(signal);
        },
        (signal) => {
          resumed.resolve(signal);
          return hangUntilAbort(signal);
        },
      ]);
      const { ctx } = makeCtx({
        signal: shared.signal,
        terminalLifecycleSignal: terminal.signal,
        attemptCredentialSwitch: async () => { recoveries++; return "gave_up"; },
        parkForPause: async () => { recoveries++; return false; },
      });
      const rejected = assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /run cancelled/);
      const currentSignal = await resumed.promise;
      assert.equal(shared.signal.aborted, true, "the shared once-listener has already fired");
      assert.equal(currentSignal.aborted, false, "recovery started a genuinely live SDK turn");
      assert.equal(recoveries, 1);
      terminal.abort();
      assert.equal(currentSignal.aborted, true, "the stable signal aborts the current provider controller");
      await rejected;
      assert.equal(turnCount(), 3, "no turn starts after terminal cancellation");
    });
  }

  for (const phase of ["planning", "implementation"] as const) {
    for (const terminalKind of ["signal", "claim fence"] as const) {
      it(`does not restart ${phase} after terminal ${terminalKind} during awaited switch settlement`, { timeout: 10_000 }, async () => {
        const shared = new AbortController();
        const terminal = new AbortController();
        const settling = barrier<void>();
        const cleanup = barrier<"gave_up">();
        let fenced = false;
        let attempts = 0;
        const interrupted: Script = (signal) => {
          shared.abort(new CredentialSwitchSignal());
          return hangUntilAbort(signal);
        };
        const scripts: Script[] = phase === "planning"
          ? [interrupted, [submitPlan("# Plan"), resultSuccess()], doneTurn()]
          : [[submitPlan("# Plan"), resultSuccess()], interrupted, doneTurn()];
        const { queryFn, turnCount } = fakeTurns(scripts);
        const { ctx } = makeCtx({
          signal: shared.signal,
          ...(terminalKind === "signal" ? { terminalLifecycleSignal: terminal.signal } : {}),
          claimFenced: () => fenced,
          attemptCredentialSwitch: async () => {
            attempts++;
            settling.resolve();
            return cleanup.promise;
          },
        });
        const rejected = assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /run cancelled/);
        await settling.promise;
        const before = turnCount();
        assert.equal(shared.signal.reason instanceof CredentialSwitchSignal, true);
        // The switch owns the first trip. Terminal arrives while marker cleanup is still awaited.
        if (terminalKind === "signal") terminal.abort();
        else fenced = true;
        assert.equal(turnCount(), before);
        cleanup.resolve("gave_up");
        await rejected;
        assert.equal(attempts, 1, "the earlier switch reached settlement");
        assert.equal(turnCount(), phase === "planning" ? 1 : 2, "recovery must not start another provider turn");
      });
    }
  }

  it("does not restart implementation when terminal arrives during a declined pause refusal", { timeout: 10_000 }, async () => {
    const shared = new AbortController();
    const terminal = new AbortController();
    const refusing = barrier<void>();
    const refusal = barrier<boolean>();
    let parks = 0;
    const { queryFn, turnCount } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      (signal) => {
        shared.abort(new PauseNowSignal());
        return hangUntilAbort(signal);
      },
      doneTurn(),
    ]);
    const { ctx } = makeCtx({
      signal: shared.signal,
      terminalLifecycleSignal: terminal.signal,
      parkForPause: async () => {
        parks++;
        refusing.resolve();
        return refusal.promise;
      },
    });
    const rejected = assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /run cancelled/);
    await refusing.promise;
    terminal.abort();
    refusal.resolve(false);
    await rejected;
    assert.equal(parks, 1, "the earlier pause won its trip and reached the refusal barrier");
    assert.equal(turnCount(), 2, "no provider turn starts after the refusal settles");
  });

  it("rejects an already-aborted stable signal before starting a provider turn", async () => {
    const terminal = new AbortController();
    terminal.abort();
    const { queryFn, turnCount } = fakeTurns([doneTurn()]);
    const { ctx } = makeCtx({ terminalLifecycleSignal: terminal.signal });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /run cancelled/);
    assert.equal(turnCount(), 0);
  });

  it("rejects an already-fenced claim without a terminal signal", async () => {
    const { queryFn, turnCount } = fakeTurns([doneTurn()]);
    const { ctx } = makeCtx({ claimFenced: () => true });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /run cancelled/);
    assert.equal(turnCount(), 0);
  });

  for (const setupError of [false, true]) {
    it(`removes the stable listener on ${setupError ? "phaseSetup error" : "normal completion"}`, async (t) => {
      const terminal = new AbortController();
      const add = t.mock.method(terminal.signal, "addEventListener");
      const remove = t.mock.method(terminal.signal, "removeEventListener");
      const { queryFn } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
      const { ctx } = makeCtx({
        terminalLifecycleSignal: terminal.signal,
        ...(setupError ? { oauthToken: "" } : {}),
      });
      const run = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      if (setupError) await assert.rejects(run, /OAuth/i);
      else await run;
      assert.equal(add.mock.callCount(), 1);
      assert.equal(remove.mock.callCount(), 1);
      assert.equal(add.mock.calls[0]!.arguments[0], "abort");
      assert.equal(remove.mock.calls[0]!.arguments[1], add.mock.calls[0]!.arguments[1], "remove the exact installed callback");
    });
  }
});

describe("SdkExecutor — credential switch handled IN PLACE (PRD #1247 M5b data-integrity fix)", () => {
  it("RUNNING switch → attemptCredentialSwitch 'released' → run() ENDS with switchReleased, no throw", async () => {
    // Plan turn, then the implement turn is tripped at iteration 1 by aborting the shared controller
    // with a CredentialSwitchSignal reason. The turn catch calls attemptCredentialSwitch → "released"
    // and BREAKS the loop, so run() RESOLVES (never rejects) with switchReleased latched.
    const { queryFn } = fakeTurns([[submitPlan("# Plan"), resultSuccess()]]);
    const cancel = new AbortController();
    let attempts = 0;
    const { ctx } = makeCtx({
      signal: cancel.signal,
      attemptCredentialSwitch: async () => {
        attempts++;
        return "released";
      },
      reportIteration: async (n) => {
        if (n === 1) cancel.abort(new CredentialSwitchSignal());
        return undefined;
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, true, "the release is surfaced so the runner skips finalize");
    assert.strictEqual(attempts, 1, "the switch was handled in place exactly once");
  });

  it("RUNNING switch → attemptCredentialSwitch 'gave_up' → the loop CONTINUES on the old token and completes normally", async () => {
    // The iteration-1 implement turn is tripped by the switch; attemptCredentialSwitch returns
    // "gave_up", so the turn catch clears the trip and CONTINUES. The next iteration drives the done
    // turn and the run completes NORMALLY — no throw, and switchReleased is NOT surfaced (the run kept
    // executing rather than being released).
    const { queryFn } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    const cancel = new AbortController();
    let attempts = 0;
    const { ctx } = makeCtx({
      signal: cancel.signal,
      attemptCredentialSwitch: async () => {
        attempts++;
        return "gave_up";
      },
      reportIteration: async (n) => {
        if (n === 1) cancel.abort(new CredentialSwitchSignal());
        return undefined;
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, undefined, "a give-up does NOT surface switchReleased — the run continued");
    assert.strictEqual(attempts, 1, "the switch was attempted once, in place");
  });

  it("HELD plan gate switch → 'gave_up' → RE-PRESENTS the gate on the old token, then a real approve completes the run", async () => {
    // The gate waiter rejects with a CredentialSwitchSignal (a run idling at the plan gate). The gate
    // wait's switch handling attempts the switch → "gave_up" → runThroughSwitch re-runs ctx.gatePlan
    // (re-present the gate). The SECOND gatePlan returns approve, so the run proceeds to implement and
    // completes — proving the gate stayed live on the old token after the give-up.
    const { queryFn } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    let gateCalls = 0;
    let attempts = 0;
    const { ctx } = makeCtx({
      gatePlan: async () => {
        gateCalls++;
        if (gateCalls === 1) throw new CredentialSwitchSignal();
        return { kind: "approve", selection: { status: "absent" } };
      },
      attemptCredentialSwitch: async () => {
        attempts++;
        return "gave_up";
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, undefined, "a gate give-up does NOT release — it re-waits");
    assert.strictEqual(gateCalls, 2, "the gate was RE-PRESENTED after the give-up");
    assert.strictEqual(attempts, 1, "the switch was attempted once at the gate");
  });

  it("HELD plan gate switch → 'released' → run() ENDS with switchReleased and NO implement turn runs", async () => {
    // A switch at the gate that RELEASES: phasePlanGate returns early with switchReleased, so run()
    // resolves without ever entering the implement loop (the reclaim re-presents this plan on the new
    // token). The done turn is scripted but must never be consumed.
    const { queryFn, turnCount } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    let attempts = 0;
    const { ctx } = makeCtx({
      gatePlan: async () => {
        throw new CredentialSwitchSignal();
      },
      attemptCredentialSwitch: async () => {
        attempts++;
        return "released";
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, true, "a gate release is surfaced so the runner skips finalize");
    assert.strictEqual(attempts, 1, "the switch was attempted once at the gate");
    assert.strictEqual(turnCount(), 1, "only the plan turn ran — the release ended the run before implementing");
  });

  it("interactive follow-up wait switch → 'released' → run() ENDS with switchReleased", async () => {
    // An interactive task run parks at awaitFollowUp on a clean signal_done. The follow-up waiter
    // rejects with a CredentialSwitchSignal; the wait's switch handling attempts the switch →
    // "released" → the loop breaks with switchReleased surfaced.
    const { queryFn } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    let attempts = 0;
    let followUpCalls = 0;
    const { ctx } = makeCtx({
      interactive: true,
      awaitFollowUp: async () => {
        followUpCalls++;
        throw new CredentialSwitchSignal();
      },
      attemptCredentialSwitch: async () => {
        attempts++;
        return "released";
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, true, "the follow-up-wait release is surfaced");
    assert.strictEqual(attempts, 1, "the switch was attempted once at the follow-up wait");
    assert.strictEqual(followUpCalls, 1, "the follow-up wait was entered once before the release");
  });

  it("interactive follow-up wait switch → 'gave_up' → RE-PARKS the wait on the old token, then a normal idle end completes", async () => {
    // The follow-up waiter rejects with a switch; attemptCredentialSwitch → "gave_up" → runThroughSwitch
    // re-parks the wait. The SECOND awaitFollowUp resolves with a normal idle end, so the run finalizes
    // normally (no release, no throw) — the audit flagged this waiter as previously untested.
    const { queryFn } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    let attempts = 0;
    let followUpCalls = 0;
    const { ctx } = makeCtx({
      interactive: true,
      awaitFollowUp: async () => {
        followUpCalls++;
        if (followUpCalls === 1) throw new CredentialSwitchSignal();
        return { kind: "ended", reason: "idle" };
      },
      attemptCredentialSwitch: async () => {
        attempts++;
        return "gave_up";
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, undefined, "a follow-up give-up does NOT release — it re-parks");
    assert.strictEqual(attempts, 1, "the switch was attempted once at the follow-up wait");
    assert.strictEqual(followUpCalls, 2, "the follow-up wait was RE-PARKED on the old token after the give-up");
  });
});
