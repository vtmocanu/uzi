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
import { m3Calls } from "./m3-recovery-fixture.js";

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

describe("SdkExecutor — credential switch during the plan source report (PRD #3914 M3)", () => {
  const revisingGate = () => {
    let gateCalls = 0;
    return {
      calls: () => gateCalls,
      gatePlan: async (): Promise<PlanVerdict> => {
        gateCalls++;
        return gateCalls === 1
          ? { kind: "revise", feedback: "tighten it" } as PlanVerdict
          : { kind: "approve", selection: { status: "absent" } };
      },
    };
  };
  const resumedAtGate = { planApproved: false, resumePhase: "awaiting_approval", approvedPlan: "# Submitted plan" } as const;

  it("PRD #3914 M3: a switch during the first plan source report gives up in place, retries the report, then plans", async () => {
    const { queryFn, turnCount } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    let reports = 0;
    let attempts = 0;
    let turnsAtRetry = -1;
    const { ctx } = makeCtx({
      reportPlanAgentSource: async () => {
        reports++;
        if (reports === 1) throw new CredentialSwitchSignal();
        turnsAtRetry = turnCount();
      },
      attemptCredentialSwitch: async () => { attempts++; return "gave_up"; },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, undefined);
    assert.strictEqual(attempts, 1, "the switch was attempted once, in place");
    assert.strictEqual(reports, 2, "the interrupted report was retried after the give-up");
    assert.strictEqual(turnsAtRetry, 0, "the retry ran before any provider turn");
    assert.strictEqual(turnCount(), 2, "planning and implementation both ran");
  });

  it("PRD #3914 M3: a switch during the first plan source report that releases starts no provider turn", async () => {
    const { queryFn, turnCount } = fakeTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    let reports = 0;
    let attempts = 0;
    const { ctx } = makeCtx({
      reportPlanAgentSource: async () => { reports++; throw new CredentialSwitchSignal(); },
      attemptCredentialSwitch: async () => { attempts++; return "released"; },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, true);
    assert.strictEqual(attempts, 1);
    assert.strictEqual(reports, 1);
    assert.strictEqual(turnCount(), 0, "no provider turn started after the release");
  });

  it("PRD #3914 M3: a switch during the revision's plan source report gives up in place and the revision stays deferred", async () => {
    const { queryFn, turnCount } = fakeTurns([[submitPlan("# Revised plan"), resultSuccess()], doneTurn()]);
    const gate = revisingGate();
    let reports = 0;
    let attempts = 0;
    let deferred = 0;
    let reportsInDefer = 0;
    let depth = 0;
    const { ctx } = makeCtx({
      ...resumedAtGate,
      gatePlan: gate.gatePlan,
      reportPlanAgentSource: async () => {
        reports++;
        if (depth > 0) reportsInDefer++;
        if (reports === 1) throw new CredentialSwitchSignal();
      },
      attemptCredentialSwitch: async () => { attempts++; return "gave_up"; },
      deferCredentialSwitch: async (fn) => {
        deferred++;
        depth++;
        try { return await fn(); } finally { depth--; }
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, undefined);
    assert.strictEqual(attempts, 1, "the switch was attempted once, in place");
    assert.strictEqual(reports, 2, "the interrupted report was retried after the give-up");
    assert.strictEqual(reportsInDefer, 0, "the report runs before the deferred revision turn");
    assert.strictEqual(deferred, 1, "the revision turn still runs with the switch deferred");
    assert.strictEqual(gate.calls(), 2, "the revised plan was gated and approved");
    assert.strictEqual(turnCount(), 2, "the revision turn and implementation both ran");
  });

  it("PRD #3914 M3: a switch during the revision's plan source report that releases starts no provider turn", async () => {
    const { queryFn, turnCount } = fakeTurns([[submitPlan("# Revised plan"), resultSuccess()], doneTurn()]);
    const gate = revisingGate();
    let attempts = 0;
    let deferred = 0;
    const { ctx } = makeCtx({
      ...resumedAtGate,
      gatePlan: gate.gatePlan,
      reportPlanAgentSource: async () => { throw new CredentialSwitchSignal(); },
      attemptCredentialSwitch: async () => { attempts++; return "released"; },
      deferCredentialSwitch: async (fn) => { deferred++; return fn(); },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, true);
    assert.strictEqual(attempts, 1);
    assert.strictEqual(deferred, 0, "no revision turn was entered");
    assert.strictEqual(turnCount(), 0, "no provider turn started after the release");
  });
});

// Issue #4201 m2c: an "applied" hook outcome continues the run in place at EVERY executor site, exactly as a
// give-up does, but on the new token: the wait re-runs (or the turn restarts with the same resumeId and cwd)
// and the next provider turn spawns with the published credential in the shared env.
describe("SdkExecutor — an applied in-place credential switch (issue #4201)", () => {
  const NEW_OAUTH = "dummy-new-oauth-token-do-not-scan-9999";
  const askUserTurn = (): SDKMessage => ({
    type: "assistant",
    session_id: "sess-1",
    message: { content: [{ type: "tool_use", id: "ta", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Which db?" }] } }] },
  } as unknown as SDKMessage);

  const parkRunTurn = (): SDKMessage => ({
    type: "assistant",
    session_id: "sess-1",
    message: { content: [{ type: "tool_use", id: "tp", name: "mcp__uzi__park_run", input: {} }] },
  } as unknown as SDKMessage);

  function recordingTurns(scripts: Script[]): { queryFn: SdkQueryFn; turns: Array<{ token: string | undefined; resume: string | undefined; cwd: string | undefined }> } {
    const turns: Array<{ token: string | undefined; resume: string | undefined; cwd: string | undefined }> = [];
    const { queryFn: inner } = fakeTurns(scripts);
    const queryFn: SdkQueryFn = (params) => {
      const env = params.options.env as Record<string, string | undefined>;
      turns.push({ token: env.CLAUDE_CODE_OAUTH_TOKEN, resume: params.options.resume, cwd: params.options.cwd });
      return inner(params);
    };
    return { queryFn, turns };
  }

  /** The runner's half, in miniature: publish the new credential, then answer "applied". */
  function applying() {
    const calls: Array<{ site: string; laneSettled: boolean }> = [];
    const credential = { token: OAUTH, ordinal: 0 };
    const overrides: Partial<RunContext> = {
      credential: () => ({ ...credential }),
      attemptCredentialSwitch: async (opts) => {
        calls.push({ site: opts?.site ?? "none", laneSettled: opts?.laneSettled ?? false });
        credential.token = NEW_OAUTH;
        credential.ordinal = 1;
        return "applied";
      },
    };
    return { calls, overrides };
  }
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const revise = { kind: "revise", feedback: "tighten it" } as PlanVerdict;
  const resumedAtGate = { planApproved: false, resumePhase: "awaiting_approval", approvedPlan: "# Submitted plan" } as const;

  const cases: Array<{ site: string; build: () => { scripts: Script[]; ctx: Partial<RunContext>; settles: () => boolean; firstNewTurn: number } }> = [
    {
      site: "gate_resumed",
      build: () => {
        let n = 0;
        return {
          scripts: [doneTurn()],
          ctx: { ...resumedAtGate, takeResumedGateEvent: async () => { if (++n === 1) throw new CredentialSwitchSignal(); return undefined; } },
          settles: () => n === 2, firstNewTurn: 0,
        };
      },
    },
    {
      site: "plan_turn",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], doneTurn()],
          ctx: { reportPlanAgentSource: async () => { if (++n === 1) throw new CredentialSwitchSignal(); } },
          settles: () => n === 2, firstNewTurn: 0,
        };
      },
    },
    {
      site: "plan_gate",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], doneTurn()],
          ctx: { gatePlan: async () => { if (++n === 1) throw new CredentialSwitchSignal(); return approve; } },
          settles: () => n === 2, firstNewTurn: 1,
        };
      },
    },
    {
      site: "plan_gate_exhausted",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], doneTurn()],
          ctx: {
            config: { plan_max_revisions: 0 } as never,
            gatePlan: async () => { n++; if (n === 1) return revise; if (n === 2) throw new CredentialSwitchSignal(); return approve; },
          },
          settles: () => n === 3, firstNewTurn: 1,
        };
      },
    },
    {
      site: "plan_source",
      build: () => {
        let reports = 0;
        let gates = 0;
        return {
          scripts: [[submitPlan("# Revised plan"), resultSuccess()], doneTurn()],
          ctx: {
            ...resumedAtGate,
            gatePlan: async () => (++gates === 1 ? revise : approve),
            reportPlanAgentSource: async () => { if (++reports === 1) throw new CredentialSwitchSignal(); },
            deferCredentialSwitch: async (fn) => fn(),
          },
          settles: () => reports === 2, firstNewTurn: 0,
        };
      },
    },
    {
      site: "plan_gate_revision",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], [submitPlan("# Plan v2"), resultSuccess()], doneTurn()],
          ctx: {
            gatePlan: async () => { n++; if (n === 1) return revise; if (n === 2) throw new CredentialSwitchSignal(); return approve; },
            deferCredentialSwitch: async (fn) => fn(),
          },
          settles: () => n === 3, firstNewTurn: 2,
        };
      },
    },
    {
      site: "ask_user",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], [askUserTurn(), resultSuccess()], doneTurn()],
          ctx: { askUser: async () => { if (++n === 1) throw new CredentialSwitchSignal(); return { kind: "answer", answers: ["postgres"] }; } },
          settles: () => n === 2, firstNewTurn: 2,
        };
      },
    },
    {
      site: "follow_up",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], doneTurn(), doneTurn()],
          ctx: {
            interactive: true,
            awaitFollowUp: async () => {
              n++;
              if (n === 1) throw new CredentialSwitchSignal();
              if (n === 2) return { kind: "followup", id: 1, body: "one more thing" };
              return { kind: "ended", reason: "idle" };
            },
          },
          settles: () => n === 3, firstNewTurn: 2,
        };
      },
    },
    {
      // The lead's park_run signal re-enters the owner question; a switch landing there re-asks it in place.
      site: "question_reentry",
      build: () => {
        let n = 0;
        return {
          scripts: [[submitPlan("# Plan"), resultSuccess()], [parkRunTurn(), resultSuccess()], doneTurn()],
          ctx: {
            parkRunSupported: true,
            requestParkRun: async () => {
              if (++n === 1) throw new CredentialSwitchSignal();
              return { kind: "answer", answers: ["keep going"] };
            },
          },
          settles: () => n === 2, firstNewTurn: 2,
        };
      },
    },
  ];

  for (const c of cases) {
    it(`${c.site}: the wait re-runs and the next provider turn spawns on the new token`, async () => {
      const built = c.build();
      const { queryFn, turns } = recordingTurns(built.scripts);
      const hook = applying();
      const { ctx } = makeCtx({ ...built.ctx, ...hook.overrides });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      assert.strictEqual(result.switchReleased, undefined, "an apply never releases");
      assert.deepEqual(hook.calls, [{ site: c.site, laneSettled: true }], "the hook is told the site and that the lane is settled");
      assert.ok(built.settles(), "the interrupted wait ran again after the apply");
      assert.strictEqual(ctx.oauthToken, NEW_OAUTH, "ctx.oauthToken follows the published credential");
      assert.ok(turns.length > c.build().firstNewTurn, `a provider turn followed the apply: ${JSON.stringify(turns)}`);
      assert.ok(turns.slice(0, built.firstNewTurn).every((t) => t.token === OAUTH), "turns before the apply used the old token");
      assert.ok(turns.slice(built.firstNewTurn).every((t) => t.token === NEW_OAUTH), `turns after the apply use the new token: ${JSON.stringify(turns)}`);
      assert.strictEqual(new Set(turns.map((t) => t.cwd)).size, 1, "one cwd for every turn");
    });
  }

  it("implement_ready: the iteration-park readiness step re-runs after an apply, then parks on the new credential", async (t) => {
    // The readiness step is the callback runThroughSwitch wraps just before an iteration park; the switch lands
    // inside it (identified by the stack, since nothing else names that step).
    let fired = 0;
    const real = SdkExecutor.prototype.closeAndSettlePlanSummary;
    t.mock.method(SdkExecutor.prototype, "closeAndSettlePlanSummary", async function (this: SdkExecutor) {
      if (fired === 0 && new Error().stack?.includes("parkIteration")) { fired++; throw new CredentialSwitchSignal(); }
      return real.call(this);
    });
    const { queryFn, turns } = recordingTurns([[submitPlan("# Plan"), resultSuccess()], [resultSuccess()]]);
    const hook = applying();
    let parked = 0;
    const { ctx } = makeCtx({
      ...hook.overrides,
      kind: "issue",
      config: { max_iterations: 1, iteration_park_version: 1 },
      reportIteration: async () => ({ completionAttempts: 0 }),
      parkForIteration: async () => { parked++; return "parked"; },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(fired, 1, "the switch landed in the readiness step");
    assert.deepEqual(hook.calls, [{ site: "implement_ready", laneSettled: true }]);
    assert.strictEqual(result.switchReleased, undefined, "an apply never releases");
    assert.strictEqual(parked, 1, "the park ran after the apply");
    assert.deepEqual(result.iterationParked, { outcome: "parked" });
    assert.strictEqual(ctx.oauthToken, NEW_OAUTH, "the context follows the published credential");
    assert.ok(turns.every((x) => x.token === OAUTH), "no provider turn ran after the apply");
  });

  it("implement_turn: the aborted turn restarts in place with the same resumeId and cwd on the new token", async () => {
    const { queryFn, turns } = recordingTurns([[submitPlan("# Plan"), resultSuccess()], doneTurn()]);
    const cancel = new AbortController();
    const hook = applying();
    const { ctx } = makeCtx({
      ...hook.overrides,
      signal: cancel.signal,
      reportIteration: async (n) => { if (n === 1) cancel.abort(new CredentialSwitchSignal()); return undefined; },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.switchReleased, undefined);
    assert.deepEqual(hook.calls, [{ site: "implement_turn", laneSettled: true }]);
    const last = turns.at(-1)!;
    assert.strictEqual(last.token, NEW_OAUTH, "the restarted turn spawns on the new token");
    assert.ok(turns.slice(0, -1).every((t) => t.token === OAUTH));
    assert.strictEqual(new Set(turns.map((t) => t.cwd)).size, 1);
    assert.ok(last.resume === undefined || last.resume === "sess-1", "the restart continues the same session");
  });

  // Issue #4201 m2d: resume refusal after an applied in-place switch. The restarted turn resumes the same
  // session on the new credential; the new credential's account can refuse it.
  describe("resume refusal after an applied switch", () => {
    const initFrame = (session: string): SDKMessage =>
      ({ type: "system", subtype: "init", session_id: session, model: "m" }) as unknown as SDKMessage;
    /** A stream that fails before yielding any event (the CLI's pre-model resolution failure). */
    const failingStream = (message: string): AsyncIterable<SDKMessage> => ({
      [Symbol.asyncIterator]: () => ({ next: async () => { throw new Error(message); } }),
    });
    const refusal = (emits: EmittedMessage[]) =>
      emits.filter((e) => e.kind === "status" && (e.payload as { event?: string }).event === "resume_lineage_break");

    /** Plan turn, then the switch trips the first implement turn (reportIteration), the hook applies, and
     *  `restart` scripts the turn the restart runs. */
    async function drive(restart: Script) {
      const { queryFn, turns } = recordingTurns([[initFrame("sess-1"), submitPlan("# Plan"), resultSuccess()], restart]);
      const cancel = new AbortController();
      const hook = applying();
      const { ctx, emits } = makeCtx({
        ...hook.overrides,
        signal: cancel.signal,
        reportIteration: async (n) => { if (n === 1) cancel.abort(new CredentialSwitchSignal()); return undefined; },
      });
      const outcome = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx).then(() => undefined, (e: unknown) => e);
      return { turns, emits, outcome };
    }

    it("(a) an init session that is not the resumed one emits one lineage break and continues in the same cwd", async () => {
      const r = await drive([initFrame("fresh-sess"), ...doneTurn("fresh-sess")]);
      assert.strictEqual(r.outcome, undefined);
      const restart = r.turns.at(-1)!;
      assert.strictEqual(restart.token, NEW_OAUTH);
      assert.strictEqual(restart.resume, "sess-1", "the restart asked to resume the switch's session");
      const events = refusal(r.emits);
      assert.strictEqual(events.length, 1, "exactly one lineage break");
      assert.strictEqual((events[0]!.payload as { reason?: string }).reason, "credential_switch_resume_refused");
      assert.strictEqual(new Set(r.turns.map((t) => t.cwd)).size, 1, "the cwd is unchanged");
      assert.strictEqual(events[0]!.credentialEpochOrdinal, 1, "attributed to the new epoch");
    });

    it("a resumed init that matches emits no lineage break", async () => {
      const r = await drive([initFrame("sess-1"), ...doneTurn("sess-1")]);
      assert.strictEqual(r.outcome, undefined);
      assert.strictEqual(refusal(r.emits).length, 0);
    });

    it("(b) an allowlisted transcript-incompatibility with no model evidence restarts once with resume unset", async () => {
      let calls = 0;
      const failing: Script = () => {
        calls++;
        // The first restart attempt fails before any model event; the retry runs the done turn.
        if (calls === 1) return failingStream("No conversation found with session ID: sess-1");
        return (async function* () { for (const m of doneTurn("fresh-2")) yield m; })();
      };
      const r = await drive(failing);
      assert.strictEqual(r.outcome, undefined, String(r.outcome));
      const resumes = r.turns.slice(1).map((t) => t.resume);
      assert.strictEqual(resumes[0], "sess-1");
      assert.strictEqual(resumes[1], undefined, "the one restart runs with resume unset");
      assert.strictEqual(r.turns.length, 3);
      assert.strictEqual(new Set(r.turns.map((t) => t.cwd)).size, 1, "same cwd");
      assert.strictEqual(refusal(r.emits).length, 1);
      assert.strictEqual((refusal(r.emits)[0]!.payload as { reason?: string }).reason, "credential_switch_resume_refused");
    });

    /** A turn script that records the prompt text its attempt was dispatched with. */
    function withPrompts(scripts: Script[], prompts: string[]): SdkQueryFn {
      let i = 0;
      return (params) => {
        const script = scripts[Math.min(i, scripts.length - 1)]!;
        i++;
        return (async function* () {
          let text = "";
          for await (const m of params.prompt) text += JSON.stringify(m);
          prompts.push(text);
          const out = typeof script === "function" ? script(params.options.abortController!.signal) : script;
          if (Array.isArray(out)) for (const m of out) yield m;
          else yield* out as AsyncIterable<SDKMessage>;
        })();
      };
    }

    /** A seeded resume parked at the plan gate whose first implement dispatch carries the interrupted-calls note.
     *  The switch trips at the resumed gate and the hook applies, so the implement turn that follows is a new
     *  ordinary turn (not an in-place restart) that resumes the session on the new credential. */
    function seededCtx(extra: Partial<RunContext> = {}) {
      const hook = applying();
      let n = 0;
      return makeCtx({
        ...hook.overrides,
        seeded: true, planApproved: false, resumePhase: "awaiting_approval", approvedPlan: "# Submitted plan", sessionId: "sess-1",
        takeResumedGateEvent: async () => { if (++n === 1) throw new CredentialSwitchSignal(); return undefined; },
        interruptedSnapshot: { base: "interrupted", interruptedEdits: false, commits: 0 },
        config: { max_iterations: 5 },
        getInterruptedCalls: () => m3Calls(),
        ...extra,
      } as Partial<RunContext>);
    }

    it("N3: the fresh-session restart re-sends the interrupted-calls note the refused attempt appended", async () => {
      const prompts: string[] = [];
      let calls = 0;
      const refusedThenDone: Script = () => {
        calls++;
        if (calls === 1) return failingStream("No conversation found with session ID: sess-1");
        return (async function* () { for (const m of doneTurn("fresh-2")) yield m; })();
      };
      const { ctx, emits } = seededCtx();
      const outcome = await new SdkExecutor(nullLogger(), homeDir, { queryFn: withPrompts([refusedThenDone], prompts) }).run(ctx).then(() => undefined, (e: unknown) => e);
      assert.strictEqual(outcome, undefined, String(outcome));
      assert.strictEqual(prompts.length, 2, "the refused attempt and its one restart");
      assert.match(prompts[0]!, /interrupted_calls_/, "the refused attempt carried the note");
      assert.match(prompts[1]!, /could not be resumed after a credential switch/, "the restart is the fresh-session restart");
      assert.match(prompts[1]!, /interrupted_calls_/, "the restart carries the note again");
      assert.strictEqual(refusal(emits).length, 1);
    });

    it("N4: a first post-switch turn with no resume spends the flag; a later fresh init is not labelled a refusal", async () => {
      const turn = (session: string, done: boolean): Script => [
        initFrame(session),
        ...(done ? doneTurn(session) : [
          { type: "assistant", session_id: session, message: { content: [{ type: "text", text: "still working" }] } } as unknown as SDKMessage,
          resultSuccess(session),
        ]),
      ];
      const { queryFn, turns } = recordingTurns([turn("sess-a", false), turn("sess-b-fresh", true)]);
      const { ctx, emits } = seededCtx({ sessionId: null });
      const outcome = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx).then(() => undefined, (e: unknown) => e);
      assert.strictEqual(outcome, undefined, String(outcome));
      assert.strictEqual(turns[0]!.resume, undefined, "the first post-switch turn had nothing to resume");
      assert.strictEqual(turns[1]!.resume, "sess-a", "a later turn resumes, and its fresh init is unrelated to the switch");
      assert.strictEqual(refusal(emits).length, 0, "no credential_switch_resume_refused label on an unrelated fresh init");
    });

    it("N4: two fresh init frames in the one post-switch turn emit one lineage break, and a following refused resume adds none", async () => {
      const calls: Script = () => (async function* () {
        yield initFrame("fresh-1");
        yield initFrame("fresh-2");
        throw new Error("No conversation found with session ID: sess-1");
      })();
      const { queryFn } = recordingTurns([[initFrame("sess-1"), submitPlan("# Plan"), resultSuccess()], calls, doneTurn("fresh-3")]);
      const cancel = new AbortController();
      const hook = applying();
      const { ctx, emits } = makeCtx({
        ...hook.overrides, signal: cancel.signal,
        reportIteration: async (n) => { if (n === 1) cancel.abort(new CredentialSwitchSignal()); return undefined; },
      });
      const outcome = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx).then(() => undefined, (e: unknown) => e);
      assert.strictEqual(outcome, undefined, String(outcome));
      assert.strictEqual(refusal(emits).length, 1, "at most one lineage break per applied switch");
    });

    it("the same error after model evidence takes the ordinary path (the run fails, no restart)", async () => {
      const failing: Script = () => (async function* () {
        yield { type: "assistant", session_id: "sess-1", message: { content: [{ type: "text", text: "working" }] } } as unknown as SDKMessage;
        throw new Error("No conversation found with session ID: sess-1");
      })();
      const r = await drive(failing);
      assert.match(String(r.outcome), /No conversation found/);
      assert.strictEqual(r.turns.length, 2, "no fresh-session restart");
      assert.strictEqual(refusal(r.emits).length, 0);
    });

    it("another failure with no model evidence (auth) is not a refusal", async () => {
      const failing: Script = () => failingStream("401 Unauthorized: invalid x-api-key");
      const r = await drive(failing);
      assert.match(String(r.outcome), /401/);
      assert.strictEqual(r.turns.length, 2);
      assert.strictEqual(refusal(r.emits).length, 0);
    });
  });

  it("laneSettled is false when ONLY the guarded lane is poisoned (its quiesce succeeds)", async (t) => {
    const { ClaudeCommandLane } = await import("../src/claude-command/lane.js");
    const cancel = new AbortController();
    t.mock.method(ClaudeCommandLane.prototype, "quiesce", async () => undefined);
    t.mock.getter(ClaudeCommandLane.prototype, "poisoned", () => true);
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      (signal) => { cancel.abort(new CredentialSwitchSignal()); return hangUntilAbort(signal); },
      doneTurn(),
    ]);
    const opts: Array<{ site: string; laneSettled: boolean } | undefined> = [];
    const commands = { protect: () => ({}), unprotect: () => true };
    const { ctx } = makeCtx({
      signal: cancel.signal,
      capturedMemory: () => ({ commands, context: {}, flight: {}, lifecycleRevision: 1 }) as never,
      attemptCredentialSwitch: async (o) => { opts.push(o); throw new Error("stop here"); },
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /stop here|unconfirmed/);
    assert.deepEqual(opts, [{ site: "implement_turn", laneSettled: false }]);
  });

  it("laneSettled is false when the guarded lane's quiesce failed after the switch trip took the carrier", async (t) => {
    const { ClaudeCommandLane, GuardedCommandUnconfirmedError } = await import("../src/claude-command/lane.js");
    const cancel = new AbortController();
    // Only the turn the switch interrupted fails its join; the plan turn before it joins cleanly.
    t.mock.method(ClaudeCommandLane.prototype, "quiesce", async () => {
      if (cancel.signal.aborted) throw new GuardedCommandUnconfirmedError("fixture unconfirmed");
    });
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      // The switch lands while the implement turn is live; its join then fails.
      (signal) => { cancel.abort(new CredentialSwitchSignal()); return hangUntilAbort(signal); },
      doneTurn(),
    ]);
    const opts: Array<{ site: string; laneSettled: boolean } | undefined> = [];
    const commands = { protect: () => ({}), unprotect: () => true };
    const { ctx } = makeCtx({
      signal: cancel.signal,
      capturedMemory: () => ({ commands, context: {}, flight: {}, lifecycleRevision: 1 }) as never,
      // The switch trip is held first, so the lane failure never becomes the turn's error: the hook sees it.
      attemptCredentialSwitch: async (o) => { opts.push(o); throw new Error("stop here"); },
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /stop here|unconfirmed/);
    assert.deepEqual(opts, [{ site: "implement_turn", laneSettled: false }]);
  });
});
