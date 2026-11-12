// Issue #3767 baseline-loadable regression. This file deliberately imports ONLY modules and
// symbols that already existed before the spend-limit work (no SpendLimitHoldError, nothing
// from limit.ts), so it can be loaded against the pre-change tree: there it fails on the
// assertion below (a plain Error), not on a missing import.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

const nonexistentWorktree = nonexistentWorktreeFactory("uzi-nonexistent");
const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

const SPEND_SENTENCE = "You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC.";

// The raw SDK result frame a spend-limited lead terminal arrives as.
function spendResult(): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: true,
    terminal_reason: "api_error",
    api_error_status: 400,
    result: `API Error: 400 ${SPEND_SENTENCE}`,
    session_id: "s",
  } as unknown as SDKMessage;
}

function oneTurn(frames: SDKMessage[]): { queryFn: SdkQueryFn; turns: number } {
  const state = { queryFn: undefined as unknown as SdkQueryFn, turns: 0 };
  state.queryFn = (params) => {
    state.turns++;
    return (async function* () {
      // Drain the prompt stream first, as the real CLI does, then replay the script.
      for await (const p of params.prompt) void p;
      for (const m of frames) yield m;
    })();
  };
  return state;
}

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sdkhome-"));
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

describe("spend-limit lead terminal regression (issue #3767)", () => {
  it("a 400 spend-limit result frame rejects as SpendLimitHoldError carrying the provider sentence", async () => {
    const emits: EmittedMessage[] = [];
    const ctx: RunContext = {
      runId: "r1",
      issueIid: 5,
      issueTitle: "Fix login",
      issueDescription: "please implement",
      worktreePath: nonexistentWorktree(),
      branch: "agent/issue-5",
      emit: (m) => emits.push(m),
      oauthToken: OAUTH,
      agents: [],
      config: null,
      sessionId: null,
      onSessionId: () => {},
      gatePlan: async () => ({ kind: "approve", selection: { status: "absent" } }),
      pullFollowUps: () => [],
      reportIteration: () => {},
    };
    const script = oneTurn([spendResult()]);
    const err = await new SdkExecutor(nullLogger(), homeDir, {
      queryFn: script.queryFn,
      emptyTurnBackoffBaseMs: 1,
      emptyTurnMaxRetries: 2,
    }).run(ctx).then(
      () => { throw new Error("expected the run to reject"); },
      (e: unknown) => e,
    );
    assert.ok(err instanceof Error);
    assert.equal(err.name, "SpendLimitHoldError");
    assert.equal((err as Error & { detail?: string }).detail, SPEND_SENTENCE);
    assert.equal(script.turns, 1, "a spend limit never retries in process");
  });
});
