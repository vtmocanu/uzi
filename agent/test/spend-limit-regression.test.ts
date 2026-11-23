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

// Issue #4426: the credit-depletion lead terminal, characterized at SDK 0.3.319 (CLI
// 2.1.319) from the SDK's own handling in its bundled `claude` binary:
// - the error mapper turns any error whose message contains "credit balance is too low"
//   (case-insensitive) into an assistant API-error message with content exactly
//   "Credit balance is too low" and `error: "billing_error"`, with no `API Error:` wrapper;
// - its wrapper copies the APIError's HTTP status (400) into `apiErrorStatus`;
// - the error is never retried (400 is not retryable and it sits in the non-retryable list);
// - the query loop ends an API-error turn with `terminal_reason: "api_error"`;
// - the result frame takes `is_error` from the API-error message (true), `api_error_status` from
//   its status (400), `subtype: "success"`, and `result` from its text.
// Observed on a hosted worker (0.87.0-rc.15) as `provider error: 400 Credit balance is too low`,
// which is exactly providerErrorMessage(400, "Credit balance is too low").
const CREDIT_SENTENCE = "Credit balance is too low";
function creditResult(): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: true,
    terminal_reason: "api_error",
    api_error_status: 400,
    result: CREDIT_SENTENCE,
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

async function rejectOnce(frame: SDKMessage): Promise<{ err: unknown; turns: number }> {
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
  const script = oneTurn([frame]);
  const err = await new SdkExecutor(nullLogger(), homeDir, {
    queryFn: script.queryFn,
    emptyTurnBackoffBaseMs: 1,
    emptyTurnMaxRetries: 2,
  }).run(ctx).then(
    () => { throw new Error("expected the run to reject"); },
    (e: unknown) => e,
  );
  return { err, turns: script.turns };
}

describe("spend-limit lead terminal regression (issue #3767)", () => {
  it("a 400 spend-limit result frame rejects as SpendLimitHoldError carrying the provider sentence", async () => {
    const { err, turns } = await rejectOnce(spendResult());
    assert.ok(err instanceof Error);
    assert.equal(err.name, "SpendLimitHoldError");
    assert.equal((err as Error & { detail?: string }).detail, SPEND_SENTENCE);
    assert.equal(turns, 1, "a spend limit never retries in process");
  });

  it("the characterized 400 credit-depletion result frame rejects as SpendLimitHoldError (issue #4426)", async () => {
    const { err, turns } = await rejectOnce(creditResult());
    assert.ok(err instanceof Error);
    assert.equal(err.name, "SpendLimitHoldError");
    assert.equal(err.message, "provider error: 400 Credit balance is too low");
    assert.equal((err as Error & { detail?: string }).detail, CREDIT_SENTENCE);
    assert.equal(turns, 1, "credit depletion never retries in process");
  });
});
