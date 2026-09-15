import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { nullLogger } from "./helpers.js";

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

let seq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-switch-sdk-wt-${process.pid}-${seq++}`);
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
    pullFollowUp: () => undefined,
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
