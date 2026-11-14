import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import {
  api, assistant, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, resultOk, runnerWith, worktreeDirFor,
} from "./runner-harness.js";
import { newApi, row, send } from "./gate-revision-harness.js";
import type { SummaryRunner } from "../src/summary-runner.js";

// The executor's default SummaryRunner runs the plan and intent summaries through the real SDK,
// which spawns the bundled claude CLI (it leaves claude-<uid> and cc-socks in TMPDIR and fails the
// tmpdir leak guard on Linux CI). The summaries are not under test here.
const NO_EXECUTOR_SUMMARIES = {
  async generateIntentSummary(): Promise<string | null> { return null; },
  async generatePlanSummary(): Promise<null> { return null; },
} as unknown as SummaryRunner;


installHarness();

// Issue #4201 AC10: the live regression, written to run unchanged against the base commit
// (c4b2b63816b0dafa3db985438eccf2ddbf2fdbc7) as well as the candidate. A running Claude issue flight is in its
// implement turn and the owner switches the credential. The base answers with the release: a `credential_switch`
// report, the flight ends, and the server's requeue delivers a NEW claim (new generation, new token) that runs in a
// fresh `<key>.attempt-<id>` clone, so the session lineage breaks (`cwd_changed_attempt_path`). The candidate applies
// the new token in place instead: the same flight, the same cwd, the same session.
//
// Everything the test needs beyond the base's own helpers is in this file: the requeue re-delivery below, and the
// candidate's apply support (a stub of WorkerClient.applyCredentialSwitch, a negotiated registration, and the op
// ledger), each guarded so that on the base it is inert.

const GEN = 1;
const NONCE = "n".repeat(43);
const SESSION = "sess-live-switch";
const OLD_TOKEN = "dummy-oauth-do-not-scan";
// Assembled at runtime so no token-shaped literal sits in the source.
const NEW_TOKEN = ["sk-ant-oat01-", "LIVE-SWITCH-0123456789"].join("");
const FORGE_PAT = "fixture-forge-pat-000000";
const TIMEOUT = { timeout: 120_000 };

const initFrame = (): SDKMessage =>
  ({ type: "system", subtype: "init", session_id: SESSION, model: "m" }) as unknown as SDKMessage;

type Turn = (signal: AbortSignal) => AsyncIterable<SDKMessage>;

const planTurn: Turn = async function* () {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: "# PLAN\n- do it" } }], SESSION);
  yield resultOk(SESSION);
};
const doneTurn: Turn = async function* () {
  yield initFrame();
  yield assistant([{ type: "text", text: "done implementing" }, { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} }], SESSION);
  yield resultOk(SESSION);
};
/** A turn that opens, runs `onStart` (the owner's switch request) and hangs until the flight aborts it. */
const hangTurn = (onStart: () => void): Turn => (signal) => ({
  async *[Symbol.asyncIterator]() {
    yield initFrame();
    onStart();
    await new Promise<void>((resolve) => {
      if (signal.aborted) return resolve();
      signal.addEventListener("abort", () => resolve(), { once: true });
    });
  },
});

interface TurnRecord { resume: string | undefined; token: string | undefined; cwd: string | undefined }

function scriptedQuery(turns: Turn[], record: TurnRecord[]): SdkQueryFn {
  let i = 0;
  return (params) => {
    const script = turns[Math.min(i, turns.length - 1)]!;
    i++;
    const env = params.options.env as Record<string, string | undefined>;
    record.push({ resume: params.options.resume, token: env.CLAUDE_CODE_OAUTH_TOKEN, cwd: params.options.cwd });
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      yield* script(params.options.abortController!.signal);
    })();
  };
}

/**
 * The server's side of a released credential switch: it requeues the run and the next claim carries the NEW token, the
 * next generation and the session the run last reported. Sequential, like a worker's next claim after the flight ended.
 */
function redeliveredClaim(first: ReturnType<typeof gitlabClaim>): ReturnType<typeof gitlabClaim> {
  const generation = GEN + 1;
  const next = gitlabClaim(first.issue_iid as number, {
    run_id: first.run_id, claim_generation: generation, session_id: SESSION, plan_approved: true, plan_md: "# PLAN\n- do it",
    // What the server's runs row holds for a run released mid-implementation: the approved plan, the phase, the
    // session the run last reported and the tail of its feed (a claim at last_seq 0 would overwrite the old frames).
    plan_source: "agent", resume_phase: "implementing", auto_approve: false,
    last_seq: Math.max(0, ...api.messages(first.run_id).map((m) => m.seq)),
    secrets: { forge_pat: FORGE_PAT, anthropic_oauth_token: NEW_TOKEN },
  });
  api.setInputClaimGeneration(first.run_id, generation);
  api.setOwnershipStatus(first.run_id, "running", generation);
  api.overrideStateStatus(first.run_id, "running");
  return next;
}

/** The candidate's apply support, inert on the base: a negotiated registration and a stub of the apply route. */
async function candidateApplySupport(runId: string, onApply: () => void): Promise<{ ledger: unknown | undefined }> {
  const c = client as unknown as Record<string, unknown>;
  client.protocolFeatures = [...client.protocolFeatures, "credential_switch_hold", "credential_switch_apply_v1"];
  c.registerNonce = NONCE;
  c.hasInPlaceCredentialSwitchCapability = true;
  if (typeof c.applyCredentialSwitch === "function") {
    c.applyCredentialSwitch = async () => (onApply(), {
      kind: "applied", token: NEW_TOKEN, ordinal: 1, replay: false,
      messageSeq: Math.max(0, ...api.messages(runId).map((m) => m.seq)) + 1,
    });
  }
  // The op ledger exists only where the in-place route does.
  const mod = await import("../src/credential-switch-op-ledger.js").catch(() => undefined);
  return { ledger: mod ? new mod.CredentialSwitchOpLedger(fx.dataDir) : undefined };
}

describe("issue #4201 AC10: a live credential switch continues the flight in place", () => {
  it("the next model turn runs on the NEW token in the same cwd, with no attempt dir, no lineage break and no release", TIMEOUT, async () => {
    newApi();
    // The release path's capture doubles (a clean tree whose tracking covers HEAD), as the base's own release tests use.
    // They stand in only while the switch is being served: the base's release needs them to verify its capture, and
    // the flight's own finalize must see the real repository again.
    const real = {
      worktreeStatus: git.worktreeStatus, verifyRunnerTrackingCovers: git.verifyRunnerTrackingCovers,
      trackingTip: git.trackingTip, checkpointPack: git.checkpointPack,
      fetchAgentBranch: git.fetchAgentBranch, committedTrackingOwnership: git.committedTrackingOwnership,
    };
    let installed = false;
    const restoreReal = (): void => {
      if (!installed) return;
      installed = false;
      git.worktreeStatus = real.worktreeStatus;
      git.verifyRunnerTrackingCovers = real.verifyRunnerTrackingCovers;
      git.trackingTip = real.trackingTip;
      git.checkpointPack = real.checkpointPack;
      git.fetchAgentBranch = real.fetchAgentBranch;
      git.committedTrackingOwnership = real.committedTrackingOwnership;
    };
    const installReleaseDoubles = (): void => {
      installed = true;
      git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
      mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
      git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
      git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
      git.checkpointPack = (async () => null) as typeof git.checkpointPack;
    };

    const claim = gitlabClaim(17_960, { claim_generation: GEN });
    const runId = claim.run_id;
    api.setInputClaimGeneration(runId, GEN);
    const { ledger } = await candidateApplySupport(runId, restoreReal);

    const turns: TurnRecord[] = [];
    const requestSwitch = (): void => {
      installReleaseDoubles();
      // The op id is the candidate's; the base ignores it.
      (api.requestCredentialSwitch as (run: string, generation: number, opId?: string) => void)(runId, GEN, "op-1");
    };
    const sdk = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: scriptedQuery([planTurn, hangTurn(requestSwitch), doneTurn], turns), client,
      summaryRunner: NO_EXECUTOR_SUMMARIES,
    } as ConstructorParameters<typeof SdkExecutor>[2]);
    api.onState(runId, (b) => {
      if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
      // The release requeues the run: from here the server's acks say `queued`.
      if (b.status === "credential_switch") api.overrideStateStatus(runId, "queued");
    });

    const attemptDir = path.dirname(worktreeDirFor(claim.issue_iid as number));
    const attempts = (): string[] =>
      fs.existsSync(attemptDir) ? fs.readdirSync(attemptDir).filter((n) => n.includes(".attempt-")).sort() : [];
    const attemptsBefore = attempts();

    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: sdk }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, switchSupersedeWaitMs: 2_000, questionTimeoutMs: 20_000,
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" },
      }),
      ...(ledger ? { credentialSwitchLedger: ledger } : {}),
    } as Parameters<typeof runnerWith>[4]);

    await runner.execute(claim);
    // The base released the switch: the server requeues the run and the next claim carries the new token.
    let second: unknown;
    if (api.states.some((s) => s.runId === runId && s.body.status === "credential_switch")) {
      restoreReal();
      const next = redeliveredClaim(claim);
      second = await runner.execute(next).then(() => undefined, (e: unknown) => e);
    }

    const statuses = api.states.filter((s) => s.runId === runId).map((s) => s.body.status as string);
    const feed = api.messages(runId);
    const payloadOf = (m: { payload?: unknown }): { event?: string; reason?: string } => (m.payload ?? {}) as { event?: string; reason?: string };

    assert.deepEqual(attempts(), attemptsBefore, "no <key>.attempt-<id> clone directory was created");
    assert.equal(feed.some((m) => payloadOf(m).reason === "cwd_changed_attempt_path"), false, "no cwd_changed_attempt_path reason");
    assert.equal(feed.some((m) => payloadOf(m).event === "resume_lineage_break"), false, "no resume_lineage_break event");
    assert.equal(statuses.includes("credential_switch"), false, `no credential_switch release report was sent: ${statuses.join(",")}`);
    assert.equal(second, undefined, `the flight needed no second claim: ${String(second)}`);
    assert.equal(new Set(turns.map((t) => t.cwd)).size, 1, `every turn ran in the same cwd: ${JSON.stringify(turns.map((t) => t.cwd))}`);
    const next = turns.at(-1)!;
    assert.equal(turns.length >= 3, true, `a model turn followed the switch: ${turns.length} turns`);
    assert.equal(next.token, NEW_TOKEN, "the next model turn runs on the NEW token");
    assert.ok(turns.slice(0, -1).every((t) => t.token === OLD_TOKEN), "every earlier turn ran on the old token");
    assert.equal(next.resume, SESSION, "the same session was resumed");
    const failure = api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body.failure_reason;
    assert.ok(statuses.includes("completed"), `the run completed: ${statuses.join(",")} ${String(failure)}`);
  });
});
