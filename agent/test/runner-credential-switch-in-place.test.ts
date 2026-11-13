import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import type { OutgoingMessage } from "../src/protocol.js";
import { randomUUID } from "node:crypto";
import type { Logger } from "../src/log.js";
import type { ClaimResponse } from "../src/protocol.js";
import os from "node:os";
import { MessageBatcher, MAX_MESSAGE_BYTES } from "../src/batcher.js";
import { RequestError, ReportPreconditionError, type CredentialSwitchApplyResult, type WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { SecretSet } from "../src/redact.js";
import { CredentialSwitchSignal, SteeringChannel } from "../src/steering.js";
import { CredentialHolder } from "../src/credential-holder.js";
import { CredentialSwitchOpLedger, defaultLedgerFs } from "../src/credential-switch-op-ledger.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import type { ApplyScript } from "./fake-api.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import {
  api, assistant, client, fakeGitlab, fx, git, homeDir, installHarness, resultOk, runnerWith, secretRecordingLogger, worktreeDirFor,
} from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { SummaryRunner } from "../src/summary-runner.js";
import { RetainedRecoveryStop } from "../src/runner.js";
import { codexRig, FakeCodexExecutor } from "./runner-codex-sinks-fixture.js";

installHarness();

// Issue #4201 m2c: the in-place credential switch through the REAL Runner, a real SdkExecutor and a
// scripted SdkQueryFn. The capability is not advertised by the worker in this unit, so the client is
// made to negotiate it here the way a registered m2d worker will.

const NONCE = "n".repeat(43);
const GEN = 1;
const OLD_TOKEN = "dummy-oauth-do-not-scan";
// Assembled at runtime so no token-shaped literal sits in the source.
const NEW_TOKEN = ["sk-ant-oat01-", "NEWTOKEN-0123456789"].join("");
const TIMEOUT = { timeout: 60_000 };
const SESSION = "sess-in-place";

function negotiate(): void {
  client.protocolFeatures = [...client.protocolFeatures, "credential_switch_hold", "credential_switch_apply_v1"];
  (client as unknown as { registerNonce: string }).registerNonce = NONCE;
  (client as unknown as { hasInPlaceCredentialSwitchCapability: boolean }).hasInPlaceCredentialSwitchCapability = true;
}

const initFrame = (session = SESSION): SDKMessage =>
  ({ type: "system", subtype: "init", session_id: session, model: "m" }) as unknown as SDKMessage;
const planTurn = (plan = "# PLAN\n- do it"): (() => AsyncIterable<SDKMessage>) => async function* () {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }], SESSION);
  yield resultOk(SESSION);
};
const askTurn = (): (() => AsyncIterable<SDKMessage>) => async function* () {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "q", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Which db?" }] } }], SESSION);
  yield resultOk(SESSION);
};
const doneTurn = (text = "done implementing"): (() => AsyncIterable<SDKMessage>) => async function* () {
  yield initFrame();
  yield assistant([{ type: "text", text }, { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} }], SESSION);
  yield resultOk(SESSION);
};
/** A turn that opens (init), runs `onStart` (which requests the switch) and hangs until it is aborted. */
const hangTurn = (onStart: () => void): ((signal: AbortSignal) => AsyncIterable<SDKMessage>) => (signal) => ({
  async *[Symbol.asyncIterator]() {
    yield initFrame();
    onStart();
    await new Promise<void>((resolve) => {
      if (signal.aborted) return resolve();
      signal.addEventListener("abort", () => resolve(), { once: true });
    });
  },
});

interface TurnRecord { resume: string | undefined; token: string | undefined; env: unknown; cwd: string | undefined; prompt: string }

function scriptedQuery(turns: Array<(signal: AbortSignal) => AsyncIterable<SDKMessage>>, record: TurnRecord[]): SdkQueryFn {
  let i = 0;
  return (params) => {
    const script = turns[Math.min(i, turns.length - 1)]!;
    i++;
    const env = params.options.env as Record<string, string | undefined>;
    const turn: TurnRecord = { resume: params.options.resume, token: env.CLAUDE_CODE_OAUTH_TOKEN, env, cwd: params.options.cwd, prompt: "" };
    record.push(turn);
    return (async function* () {
      for await (const p of params.prompt) turn.prompt += JSON.stringify(p);
      yield* script(params.options.abortController!.signal);
    })();
  };
}

type Site = "plan_turn" | "plan_gate" | "plan_gate_revision" | "plan_gate_exhausted" | "ask_user" | "implement_turn" | "window"
  | "follow_up" | "question_reentry";

const parkRunTurn = (): (() => AsyncIterable<SDKMessage>) => async function* () {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "pr", name: "mcp__uzi__park_run", input: {} }], SESSION);
  yield resultOk(SESSION);
};

/** An interactive task claim: it parks at awaiting_followup after every clean signal_done instead of finalizing. */
function interactiveTaskClaim(): ClaimResponse {
  const runId = randomUUID();
  return freshClaim(GEN, {
    run_id: runId, kind: "task", interactive: true, issue_iid: null, issue_title: "Handoff: interactive session",
    issue_description: "Work with me interactively.", branch: `uzi/task/${runId}`, base_branch: "develop", open_mr: false,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
  });
}

interface Setup {
  site: Site;
  apply: ApplyScript[];
  /** Other runner knobs. */
  ledger?: CredentialSwitchOpLedger;
  quiesce?: (req: QuiesceRunRequest) => QuiesceRunOutcome | undefined;
  /** Replaces the final turn (default: a done turn that prints the new token). */
  lastTurn?: () => AsyncIterable<SDKMessage>;
  extra?: Record<string, unknown>;
  /** Runs once the claim exists, before the run starts. */
  before?: (runId: string) => void;
  /** Runs with the forge fake the run publishes against, before the run starts. */
  withGitlab?: (gl: ReturnType<typeof fakeGitlab>) => void;
  /** Holds the in-place quiescence proof (so the switch stays pending and unapplied) until it settles. */
  quiesceGate?: Promise<void>;
  /** ask_user site: the test, not the fixture, answers the re-asked question. */
  noAutoAnswer?: boolean;
}

const CLEAN: QuiesceRunOutcome = {
  process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" },
};
const BLOCKED: QuiesceRunOutcome = {
  process: { state: "unverified", processes: [], killed: [], detail: "fixture: not provable" }, docker: { state: "not_wired", removed: [], detail: "" },
};

async function scenario(setup: Setup) {
  newApi();
  negotiate();
  const claim = setup.site === "follow_up" ? interactiveTaskClaim()
    : setup.site === "question_reentry" ? freshClaim(GEN, { park_run_supported: true })
      : freshClaim(GEN);
  const runId = claim.run_id;
  setup.before?.(runId);
  const secretLog = secretRecordingLogger();
  // Every line any logger of the run is handed (msg and fields), so a test can prove a token never reached one.
  const logLines: string[] = [];
  const tee = (base: Logger): Logger => {
    const rec = (level: "debug" | "info" | "warn" | "error") => (msg: string, fields?: Record<string, unknown>): void => {
      logLines.push(`${level} ${msg} ${JSON.stringify(fields ?? {})}`);
      base[level](msg, fields);
    };
    return { ...base, debug: rec("debug"), info: rec("info"), warn: rec("warn"), error: rec("error"), child: (f) => tee(base.child(f)) };
  };
  const runLogger = tee(secretLog.logger);
  const ledgerWrites: string[] = [];
  const ledger = setup.ledger ?? new CredentialSwitchOpLedger(fx.dataDir, {
    ...defaultLedgerFs, writeFileSynced: async (file, data) => { ledgerWrites.push(data); return defaultLedgerFs.writeFileSynced(file, data); },
  });
  const turns: TurnRecord[] = [];
  const posted: OutgoingMessage[] = [];
  const postMessages = client.postMessages.bind(client);
  client.postMessages = (async (...args: Parameters<typeof postMessages>) => {
    if (args[0] === runId) posted.push(...args[1]);
    return postMessages(...args);
  }) as typeof client.postMessages;
  const requestSwitch = (): void => {
    api.requestCredentialSwitch(runId, GEN, "op-1");
    api.scriptApply(runId, ...setup.apply);
  };
  const last = setup.lastTurn ?? doneTurn(`the new token is ${NEW_TOKEN}`);
  let gates = 0;
  let asks = 0;
  let turnScripts: Array<(signal: AbortSignal) => AsyncIterable<SDKMessage>>;
  switch (setup.site) {
    case "plan_turn":
      turnScripts = [hangTurn(requestSwitch), planTurn(), last];
      api.onState(runId, (b) => { if (b.status === "awaiting_approval") send(runId, row("approve_plan")); });
      break;
    case "implement_turn":
      turnScripts = [planTurn(), hangTurn(requestSwitch), last];
      api.onState(runId, (b) => { if (b.status === "awaiting_approval") send(runId, row("approve_plan")); });
      break;
    case "plan_gate":
      turnScripts = [planTurn(), last];
      api.onState(runId, (b) => {
        if (b.status !== "awaiting_approval") return;
        if (++gates === 1) requestSwitch(); else send(runId, row("approve_plan"));
      });
      break;
    case "window":
      // The switch is already pending when the claim starts, so it arrives during the retained-recovery
      // prologue (phaseClone). The prologue hold latches it; it is served at the first executor site.
      requestSwitch();
      turnScripts = [planTurn(), last];
      api.onState(runId, (b) => { if (b.status === "awaiting_approval") send(runId, row("approve_plan")); });
      break;
    case "plan_gate_exhausted":
      // plan_max_revisions 0: the first revise verdict is already the exhausted one, and the gate re-presents it.
      claim.config = { ...claim.config, plan_max_revisions: 0 } as typeof claim.config;
      turnScripts = [planTurn(), last];
      api.onState(runId, (b) => {
        if (b.status !== "awaiting_approval") return;
        ++gates;
        if (gates === 1) send(runId, row("revise_plan", "please revise"));
        else if (gates === 2) requestSwitch();
        else send(runId, row("approve_plan"));
      });
      break;
    case "ask_user":
      turnScripts = [planTurn(), askTurn(), last];
      api.onState(runId, (b) => {
        if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
        else if (b.status === "awaiting_input") {
          if (++asks === 1) requestSwitch();
          else if (!setup.noAutoAnswer) send(runId, row("answer", JSON.stringify({ question_id: b.open_question_id, answers: ["postgres"] })));
        }
      });
      break;
    case "follow_up":
      // The task parks at awaiting_followup after its first clean done. The switch lands while it idles there; the
      // second park report is the re-armed wait after the in-place apply, which gets the follow-up; the third ends it.
      turnScripts = [planTurn(), doneTurn(), last];
      api.onState(runId, (b) => {
        if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
        if (b.status !== "awaiting_followup") return;
        if (++gates === 1) requestSwitch();
        else if (gates === 2) send(runId, row("follow_up", "one more thing"));
        else send(runId, row("stop"));
      });
      break;
    case "question_reentry":
      // The lead's park_run signal: the pause request is refused, so the owner question opens, and the switch lands there.
      api.parkRunGeneration = GEN;
      api.parkRunOwnership = { status: "running", claim_generation: GEN, hold_reason: null, claim_released: false };
      api.workerPauseRefused = true;
      turnScripts = [planTurn(), parkRunTurn(), last];
      api.onState(runId, (b) => {
        if (b.status === "awaiting_approval") send(runId, row("approve_plan"));
        else if (b.status === "awaiting_input") {
          if (++asks === 1) requestSwitch();
          else send(runId, row("answer", JSON.stringify({ question_id: b.open_question_id, answers: ["keep going"] })));
        }
      });
      break;
    case "plan_gate_revision":
      turnScripts = [planTurn(), planTurn("# PLAN v2\n- do it better"), last];
      api.onState(runId, (b) => {
        if (b.status !== "awaiting_approval") return;
        ++gates;
        if (gates === 1) send(runId, row("revise_plan", "please revise"));
        else if (gates === 2) requestSwitch();
        else send(runId, row("approve_plan"));
      });
      break;
  }
  const sdk = new SdkExecutor(runLogger, homeDir, { queryFn: scriptedQuery(turnScripts, turns), client });
  const attemptDir = path.dirname(worktreeDirFor(claim.issue_iid as number));
  const attempts = (): string[] =>
    fs.existsSync(attemptDir) ? fs.readdirSync(attemptDir).filter((n) => n.includes(".attempt-")) : [];
  const attemptsBefore = attempts();
  const gl = fakeGitlab();
  const { gitlab } = gl;
  setup.withGitlab?.(gl);
  const proofOrder: string[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    const verdict = setup.quiesce?.(req);
    if (req.site === "in_place_credential_switch") {
      api.requestLog.push("quiesce:in_place_credential_switch"); proofOrder.push(req.site);
      await setup.quiesceGate;
    }
    return verdict ?? CLEAN;
  };
  try {
    await runnerWith(() => ({ executor: sdk }), gitlab, undefined, runLogger, {
      recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, credentialSwitchLedger: ledger, quiesceRun,
      switchSupersedeWaitMs: 2_000, questionTimeoutMs: 20_000, ...setup.extra,
    }).execute(claim);
  } finally {
    client.postMessages = postMessages;
  }
  const states = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  return { claim, runId, turns, posted, states, ledger, ledgerWrites, attempts, attemptsBefore, proofOrder, secretLog, logLines };
}
type Result = Awaited<ReturnType<typeof scenario>>;

const statusesOf = (r: Result): string[] => r.states.map((s) => s.status);
const applied = (): Array<Record<string, unknown>> => api.applyRequests.map((a) => a.body);

function feedFrames(runId: string): OutgoingMessage[] {
  return [...api.messages(runId)].sort((a, b) => a.seq - b.seq);
}

/** The first post-apply frame is message_seq+1, no worker frame took the server frame's seq, and none was dropped. */
function assertSeqContinuity(r: Result): void {
  const frames = feedFrames(r.runId);
  const server = frames.find((m) => (m.kind as string) === "credential_switch");
  assert.ok(server, "the server appended its credential_switch frame");
  const S = server.seq;
  const workerSeqs = r.posted.map((m) => m.seq);
  assert.equal(new Set(workerSeqs).size, workerSeqs.length, "no seq was sent twice");
  assert.ok(!workerSeqs.includes(S), `no worker frame took the server's seq ${S}`);
  const after = workerSeqs.filter((q) => q > S).sort((a, b) => a - b);
  assert.equal(after[0], S + 1, "the first post-apply frame is message_seq+1");
  const max = Math.max(...workerSeqs);
  const expected = Array.from({ length: max }, (_, i) => i + 1).filter((q) => q !== S);
  assert.deepEqual([...workerSeqs].sort((a, b) => a - b), expected, "no frame was dropped and none left a gap");
  // The wire carries the ordinal on post-apply frames only.
  const ordinals = frames.filter((m) => m.seq > S && m.credential_epoch_ordinal !== undefined);
  assert.ok(ordinals.length > 0, "frames after the apply carry a credential epoch ordinal");
  assert.ok(ordinals.every((m) => m.credential_epoch_ordinal === 1), "frames after the apply carry ordinal 1");
  assert.ok(frames.filter((m) => m.seq < S).every((m) => m.credential_epoch_ordinal === undefined), "no earlier frame carries one");
}

describe("issue #4201 m2c: an applied in-place credential switch continues the run where it was", () => {
  for (const site of ["plan_turn", "plan_gate", "plan_gate_revision", "plan_gate_exhausted", "ask_user", "implement_turn"] as const) {
    it(`${site}: same worker, generation, cwd and session; new token; proof before apply; seq and redaction intact`, TIMEOUT, async () => {
      const r = await scenario({ site, apply: [{ outcome: "applied", token: NEW_TOKEN }] });
      // Applied exactly once, for op-1, fenced on the generation and registration nonce.
      assert.deepEqual(applied().map((b) => [b.op_id, b.claim_generation, b.register_nonce]), [["op-1", GEN, NONCE]]);
      // Continued in place: no release, no hold, no failure, no uncertain report; the run completed.
      const s = statusesOf(r);
      for (const status of ["credential_switch", "credential_switch_held", "credential_switch_failed", "credential_switch_uncertain", "failed"])
        assert.ok(!s.includes(status), `no ${status} report: ${s.join(",")}`);
      assert.ok(s.includes("completed"), `the run completed: ${s.join(",")}`);
      assert.ok(r.states.every((b) => b.claim_generation === undefined || b.claim_generation === GEN), "the generation never changed");
      assert.deepEqual(r.attempts(), r.attemptsBefore, "no new attempt dir");
      assert.equal(new Set(r.turns.map((t) => t.cwd)).size, 1, "one cwd for every turn");
      // The same session is resumed on the new token; the SDK env object is shared (so is the lane's).
      const firstNew = r.turns.findIndex((t) => t.token === NEW_TOKEN);
      assert.ok(firstNew >= 1, `a later turn spawned on the new token: ${JSON.stringify(r.turns.map((t) => t.token))}`);
      assert.ok(r.turns.slice(0, firstNew).every((t) => t.token === OLD_TOKEN), "earlier turns ran on the old token");
      assert.ok(r.turns.slice(firstNew).every((t) => t.token === NEW_TOKEN), "every later turn runs on the new token");
      assert.ok(r.turns.slice(firstNew).every((t) => t.resume === SESSION), "the same resumeId continues the session");
      assert.equal(new Set(r.turns.map((t) => t.env)).size, 1, "one shared env object, rewritten in place");
      // A new init leg opened after the switch.
      const inits = api.messages(r.runId).filter((m) => m.kind === "status" && (m.payload as { event?: string }).event === "init");
      assert.equal(inits.length, r.turns.length, "every provider turn opened its own init leg, including the one after the apply");
      // Proof first: quiescence at the in-place site, then the apply request.
      const proof = api.requestLog.indexOf("quiesce:in_place_credential_switch");
      const apply = api.requestLog.indexOf("apply");
      assert.ok(proof >= 0 && apply > proof, `proof before apply: ${api.requestLog.join(",")}`);
      assertSeqContinuity(r);
      // The token the run printed after the switch never reached the feed in the clear.
      const feed = JSON.stringify(api.messages(r.runId));
      assert.ok(!feed.includes(NEW_TOKEN), "the new token is scrubbed from every posted frame");
      assert.ok(feed.includes("the new token is"), "the frame carrying it was posted");
      // The logger's registry was told of the token the moment it was published, and evicted it at the end.
      assert.ok(r.secretLog.added.includes(NEW_TOKEN), "the log registry scrubs the new token");
      assert.ok(r.secretLog.removed.includes(NEW_TOKEN), "and evicts it when the run ends");
      // AC5: no line the worker logged (runner, executor, children) carries the new token.
      assert.ok(r.logLines.length > 0, "the worker logged something");
      const leaked = r.logLines.find((l) => l.includes(NEW_TOKEN));
      assert.equal(leaked, undefined, "the new token appears in no log line");
      // The ledger wrote the op ahead, settled it, never stored the token, and was cleaned at the terminal state.
      assert.ok(r.ledgerWrites.some((w) => w.includes('"sending"')) && r.ledgerWrites.some((w) => w.includes('"settled"')));
      assert.ok(r.ledgerWrites.every((w) => !w.includes(NEW_TOKEN)), "the token is never persisted");
      assert.equal(fs.existsSync(path.join(fx.dataDir, "credential-switch-ops", `${r.runId}.json`)), false, "cleaned up at the terminal state");
    });
  }
});

/** The per-site invariants of an in-place switch (AC1), for a site the main loop's table does not cover. */
function assertInPlaceInvariants(r: Result): void {
  assert.deepEqual(applied().map((b) => [b.op_id, b.claim_generation, b.register_nonce]), [["op-1", GEN, NONCE]], `applied exactly once: ${statusesOf(r).join(",")} ${JSON.stringify(r.states.filter((b) => b.status === "failed").map((b) => b.failure_reason))}`);
  const s = statusesOf(r);
  for (const status of ["credential_switch", "credential_switch_held", "credential_switch_failed", "credential_switch_uncertain", "failed"])
    assert.ok(!s.includes(status), `no ${status} report: ${s.join(",")}`);
  assert.ok(s.includes("completed"), `the run completed: ${s.join(",")}`);
  assert.ok(r.states.every((b) => b.claim_generation === undefined || b.claim_generation === GEN), "the generation never changed");
  assert.deepEqual(r.attempts(), r.attemptsBefore, "no new attempt dir");
  assert.equal(new Set(r.turns.map((t) => t.cwd)).size, 1, "one cwd for every turn");
  const firstNew = r.turns.findIndex((t) => t.token === NEW_TOKEN);
  assert.ok(firstNew >= 1, `a later turn spawned on the new token: ${JSON.stringify(r.turns.map((t) => t.token))}`);
  assert.ok(r.turns.slice(0, firstNew).every((t) => t.token === OLD_TOKEN), "earlier turns ran on the old token");
  assert.ok(r.turns.slice(firstNew).every((t) => t.token === NEW_TOKEN && t.resume === SESSION), "later turns resume the same session on the new token");
  assert.equal(r.logLines.find((l) => l.includes(NEW_TOKEN)), undefined, "the new token appears in no log line");
}

describe("issue #4201 m5 (AC1): the follow_up and question_reentry sites apply in place at runner level", () => {
  it("follow_up: an interactive task idling between turns applies in place and the follow-up turn runs on the new token", TIMEOUT, async () => {
    const r = await scenario({ site: "follow_up", apply: [{ outcome: "applied", token: NEW_TOKEN }] });
    assertInPlaceInvariants(r);
    assert.ok(statusesOf(r).filter((x) => x === "awaiting_followup").length >= 2, "the wait was re-armed after the apply");
    assert.ok(api.requestLog.includes("quiesce:in_place_credential_switch"), "quiescence was proven first");
  });

  it("question_reentry: the park_run owner question re-asked in place after the apply, then the answer continues on the new token", TIMEOUT, async () => {
    const r = await scenario({ site: "question_reentry", apply: [{ outcome: "applied", token: NEW_TOKEN }] });
    assertInPlaceInvariants(r);
    assert.equal(statusesOf(r).filter((x) => x === "awaiting_input").length, 2, "the question was asked, then re-asked after the apply");
    assert.equal(api.workerPauseRequests.length, 1, "the explicit park was attempted once, not repeated by the re-entry");
    assert.ok(!statusesOf(r).includes("paused"), "the run did not park");
  });
});

/**
 * Runs `site` with the in-place quiescence proof held open (the switch pending, nothing applied), sends `inputs`
 * meanwhile against the in-place server model (api.inPlaceInputDrain: GET drains during the pending switch, APPLIED for
 * these rows is refused switch_pending). Proves the worker routes them during the hold and that their APPLIED lands
 * only once the stamp clears, then returns the finished run with the input ids.
 */
async function withInputsWhileSwitchPending(
  site: "ask_user" | "implement_turn",
  inputs: (runId: string, question: string | undefined) => Parameters<typeof send> extends [string, ...infer R] ? R : never,
  /** Waits for the inputs to be routed first, so a steering message cannot race the run's last turn. */
  settleFirst = false,
) {
  let release!: () => void;
  const hold = new Promise<void>((resolve) => { release = resolve; });
  let runId = "";
  let ids: number[] = [];
  let finalCalls = 0;
  // The first turn after the apply ends without signal_done once the inputs are routed, so the loop turns again and
  // pulls them into the next prompt; the second one completes.
  const settlingTurn = (): AsyncIterable<SDKMessage> => (async function* () {
    if (++finalCalls === 1) {
      await until(() => ids.length > 0 && ids.every((id) => api.isAcked(runId, id)), 10_000);
      yield initFrame();
      yield assistant([{ type: "text", text: "continuing" }], SESSION);
      yield resultOk(SESSION);
      return;
    }
    yield* doneTurn()();
  })();
  const finished = scenario({
    site, apply: [{ outcome: "applied", token: NEW_TOKEN }], quiesceGate: hold, noAutoAnswer: true,
    before: (id) => { runId = id; api.inPlaceInputDrain = true; }, ...(settleFirst ? { lastTurn: settlingTurn } : {}),
  });
  try {
    assert.ok(await until(() => api.requestLog.includes("quiesce:in_place_credential_switch"), 30_000), "the proof is running (held)");
    assert.equal(applied().length, 0, "the switch is pending and unapplied");
    const question = api.states.filter((s) => s.runId === runId && s.body.status === "awaiting_input").at(-1)?.body.open_question_id;
    const rows = send(runId, ...inputs(runId, question));
    assert.ok(rows.length >= 1);
    ids = rows.map((input) => input.id);
    // The in-place server drains inputs during a pending switch, so the worker fetches and routes (ACKs) them DURING the
    // hold, before any apply. Their APPLIED receipts are refused 409 switch_pending, so the worker parks the ids ready
    // and the rows stay unapplied server-side until the stamp clears.
    assert.ok(await until(() => ids.every((id) => api.isAcked(runId, id)), 10_000), "every input was fetched and routed during the hold");
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.equal(applied().length, 0, "the switch is still unapplied: delivery did not wait for it");
    assert.deepEqual(ids.map((id) => api.isApplied(runId, id)), ids.map(() => false), "no APPLIED receipt landed while the stamp was pending");
    assert.ok(api.pendingInputRows(runId).length >= ids.length, "the rows are still pending server-side: none was lost");
  } finally { release(); }
  const r = await finished;
  assert.equal(applied().length, 1, "the held switch then applied");
  for (const id of ids) assert.ok(api.isAcked(runId, id) && api.isApplied(runId, id), `input ${id} was routed and applied once the switch resolved`);
  return { r, runId, ids };
}

describe("issue #4201 m5 (AC7): owner inputs sent while a switch is pending and unapplied are routed during the hold and applied once it clears, never lost", () => {
  it("an answer to the open question is routed during the hold, resolves the re-asked question and reaches the model on the new token", TIMEOUT, async () => {
    const { r } = await withInputsWhileSwitchPending("ask_user", (_runId, question) =>
      [row("answer", JSON.stringify({ question_id: question, answers: ["postgres-ANSWER"] }))]);
    assert.ok(r.turns.filter((t) => t.token === NEW_TOKEN).some((t) => t.prompt.includes("postgres-ANSWER")), "the answer reached the model after the apply");
    assert.ok(statusesOf(r).includes("completed"), statusesOf(r).join(","));
  });

  it("a steering message is routed during the hold and reaches the model on the new token", TIMEOUT, async () => {
    const { r } = await withInputsWhileSwitchPending("implement_turn", () => [row("follow_up", "STEER-ME-NOW")], true);
    assert.ok(r.turns.filter((t) => t.token === NEW_TOKEN).some((t) => t.prompt.includes("STEER-ME-NOW")),
      `the steering message reached the model after the apply: ${statusesOf(r).join(",")}`);
    assert.ok(statusesOf(r).includes("completed"), statusesOf(r).join(","));
  });

  it("an owner pause is routed during the hold and is honoured: the executor acknowledges it and enters the pause park", TIMEOUT, async () => {
    const { r, runId } = await withInputsWhileSwitchPending("implement_turn", (runId) => {
      // The server decides the pause boundary: it stamps the request and answers the next running report with pause_requested.
      api.setPauseRequestedAck(runId);
      return [row("pause", "milestone")];
    });
    // The executor acknowledged the pause and entered the pause park (this fixture's checkpoint cannot publish, so the
    // park ends as pause_failed and the run carries on; what is pinned is that the request was honoured, not lost).
    assert.ok(api.messages(runId).some((m) => (m.payload as { directive?: string }).directive === "pause"), "the pause was acknowledged in the feed");
    assert.ok(statusesOf(r).includes("paused") || statusesOf(r).includes("pause_failed"), `the pause park was entered: ${statusesOf(r).join(",")}`);
  });
});

describe("issue #4201 m2d: the PR-description editor pass runs on the holder's token", () => {
  it("an applied switch, then completion: the editor pass spawns on the NEW token and its summary_usage carries the start ordinal", TIMEOUT, async () => {
    const editorTokens: Array<string | undefined> = [];
    const queryFn = (async function* (params: { options: { env?: Record<string, string | undefined> } }) {
      editorTokens.push(params.options.env?.CLAUDE_CODE_OAUTH_TOKEN);
      yield {
        type: "assistant",
        message: {
          id: "m1", model: "claude-haiku-4-5-20251001", role: "assistant",
          content: [{ type: "text", text: JSON.stringify({ summary: "Adds a thing.", changes: ["c"], scope_notes: [], review_pointers: [] }) }],
          usage: { input_tokens: 21, output_tokens: 4 },
        },
      };
      yield { type: "result", subtype: "success", is_error: false };
    }) as unknown as SdkQueryFn;
    const summaryRunner = new SummaryRunner(nullLogger(), { queryFn, homeRoot: homeDir });
    const r = await scenario({
      site: "implement_turn",
      apply: [{ outcome: "applied", token: NEW_TOKEN }],
      extra: { summaryRunner },
      before: () => { api.prDescription = new FakePrDescApi(); },
      withGitlab: (gl) => { followSuccessfulPush(git, gl.pr, true); },
    });
    assert.deepEqual(applied().map((b) => b.op_id), ["op-1"]);
    assert.ok(statusesOf(r).includes("completed"), `the run completed: ${statusesOf(r).join(",")}`);
    assert.deepEqual(editorTokens, [NEW_TOKEN], "the PR-description pass ran once, on the post-apply token");
    const usage = api.messages(r.runId).filter((m) => m.kind === "summary_usage");
    assert.equal(usage.length, 1);
    assert.equal((usage[0]!.payload as { pass: string }).pass, "pr_description");
    assert.equal(usage[0]!.credential_epoch_ordinal, 1, "the pass's frame carries the epoch it started on");
  });

  it("a newer credential published while the editor pass is in flight: the frame still carries the START ordinal", TIMEOUT, async () => {
    // The run's holder is private to the flight: capture it when the apply publishes into it.
    const holders: CredentialHolder[] = [];
    const realPublish = CredentialHolder.prototype.publish;
    CredentialHolder.prototype.publish = function (this: CredentialHolder, next) { holders.push(this); return realPublish.call(this, next); };
    let publishedAtOrdinal: number | undefined;
    const queryFn = (async function* () {
      // A later switch lands while the pass runs: the holder moves on to ordinal 2.
      const holder = holders[0]!;
      holder.publish({ token: ["sk-ant-oat01-", "LATER-0123456789"].join(""), ordinal: 2 });
      publishedAtOrdinal = holder.current().ordinal;
      yield {
        type: "assistant",
        message: {
          id: "m1", model: "claude-haiku-4-5-20251001", role: "assistant",
          content: [{ type: "text", text: JSON.stringify({ summary: "Adds a thing.", changes: ["c"], scope_notes: [], review_pointers: [] }) }],
          usage: { input_tokens: 21, output_tokens: 4 },
        },
      };
      yield { type: "result", subtype: "success", is_error: false };
    }) as unknown as SdkQueryFn;
    const summaryRunner = new SummaryRunner(nullLogger(), { queryFn, homeRoot: homeDir });
    let r: Awaited<ReturnType<typeof scenario>>;
    try {
      r = await scenario({
        site: "implement_turn",
        apply: [{ outcome: "applied", token: NEW_TOKEN }],
        extra: { summaryRunner },
        before: () => { api.prDescription = new FakePrDescApi(); },
        withGitlab: (gl) => { followSuccessfulPush(git, gl.pr, true); },
      });
    } finally { CredentialHolder.prototype.publish = realPublish; }
    assert.equal(publishedAtOrdinal, 2, "the holder moved to ordinal 2 during the pass");
    const usage = api.messages(r.runId).filter((m) => m.kind === "summary_usage");
    assert.equal(usage.length, 1);
    assert.equal(usage[0]!.credential_epoch_ordinal, 1, "the frame carries the ordinal read at publication start, not the one current at emit time");
  });
});

describe("issue #4201 m2c: apply outcomes other than applied", () => {
  it("implement_turn: an apply whose reply is lost is retried through the ledger and replays as applied", TIMEOUT, async () => {
    const r = await scenario({ site: "implement_turn", apply: [{ outcome: "applied", token: NEW_TOKEN, replyLost: true }] });
    assert.deepEqual(applied().map((b) => b.op_id), ["op-1", "op-1"], "the same op is sent again");
    assert.ok(statusesOf(r).includes("completed") && !statusesOf(r).some((x) => x.startsWith("credential_switch")));
    assert.equal(r.turns.at(-1)!.token, NEW_TOKEN);
    assertSeqContinuity(r);
  });

  for (const site of ["implement_turn", "plan_gate"] as const) {
    it(`${site}: superseded, then the newer op is applied`, TIMEOUT, async () => {
      const r = await scenario({ site, apply: [{ outcome: "superseded", newOpId: "op-2" }, { outcome: "applied", token: NEW_TOKEN }] });
      assert.deepEqual(applied().map((b) => b.op_id), ["op-1", "op-2"]);
      assert.ok(statusesOf(r).includes("completed"));
      assert.ok(!statusesOf(r).some((x) => x.startsWith("credential_switch")), statusesOf(r).join(","));
      assert.ok(r.turns.at(-1)!.token === NEW_TOKEN, "the newer op's token is in use");
    });

    for (const kind of ["not_applied", "ledger_unavailable"] as const) {
      it(`${site}: ${kind} gives up on the old token with an op-fenced credential_switch_failed`, TIMEOUT, async () => {
        const ledger = kind === "ledger_unavailable"
          ? new CredentialSwitchOpLedger(fx.dataDir, { ...defaultLedgerFs, writeFileSynced: async () => { throw new Error("disk full"); } })
          : undefined;
        const r = await scenario({ site, apply: [{ outcome: "not_applied", reason: "no_pending_switch" }], ...(ledger ? { ledger } : {}) });
        const failed = r.states.filter((b) => b.status === "credential_switch_failed");
        assert.equal(failed.length, 1);
        assert.equal((failed[0] as { op_id?: string }).op_id, "op-1");
        assert.equal(applied().length, kind === "not_applied" ? 1 : 0, "a ledger that cannot write ahead never sends");
        assert.ok(!statusesOf(r).includes("credential_switch"), "no release");
        assert.ok(statusesOf(r).includes("completed"), "the run continued and completed");
        assert.ok(r.turns.every((t) => t.token === OLD_TOKEN), "every turn stayed on the old token");
      });
    }
  }

  it("implement_turn: a transport that cannot drain gives up with nothing applied", TIMEOUT, async () => {
    const real = MessageBatcher.prototype.holdForCredentialSwitch;
    MessageBatcher.prototype.holdForCredentialSwitch = function (this: MessageBatcher) {
      const hold = real.call(this);
      return { ...hold, prepare: async () => { hold.cancel(); return { prepared: false, reason: "unflushed" as const }; } };
    };
    try {
      const r = await scenario({ site: "implement_turn", apply: [{ outcome: "applied", token: NEW_TOKEN }] });
      assert.equal(applied().length, 0, "nothing was applied");
      assert.equal(r.states.filter((b) => b.status === "credential_switch_failed").length, 1);
      assert.ok(statusesOf(r).includes("completed"));
      assert.ok(r.turns.every((t) => t.token === OLD_TOKEN));
    } finally { MessageBatcher.prototype.holdForCredentialSwitch = real; }
  });

  it("a clean route_absent takes today's release path unchanged", TIMEOUT, async () => {
    // Today's capture double: a clean tree whose tracking covers HEAD verifies, so the release is acked.
    git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
    mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
    git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
    git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
    git.checkpointPack = (async () => null) as typeof git.checkpointPack;
    newApi();
    const claim = freshClaim(GEN);
    negotiate();
    api.scriptApply(claim.run_id, { outcome: "route_absent" });
    const turns: TurnRecord[] = [];
    const sdk = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: scriptedQuery([planTurn(), hangTurn(() => {
        // The release requeues the run, so only from here do state acks say `queued`.
        api.overrideStateStatus(claim.run_id, "queued");
        api.requestCredentialSwitch(claim.run_id, GEN, "op-1");
      }), doneTurn()], turns), client,
    });
    api.onState(claim.run_id, (b) => { if (b.status === "awaiting_approval") send(claim.run_id, row("approve_plan")); });
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor: sdk }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir),
      quiesceRun: async () => CLEAN,
    }).execute(claim);
    const s = api.states.filter((x) => x.runId === claim.run_id).map((x) => x.body.status);
    assert.equal(applied().length, 1, "the apply route was asked once");
    assert.ok(s.includes("credential_switch"), `released as today: ${s.join(",")}`);
    assert.ok(!s.includes("completed") && !s.includes("credential_switch_failed"));
    assert.equal(fs.existsSync(worktreeDirFor(claim.issue_iid as number)), true, "the clone is kept for recapture");
  });

  it("a fenced apply stops the flight (stale claim) with the work retained", TIMEOUT, async () => {
    const r = await scenario({ site: "implement_turn", apply: [{ outcome: "fenced" }] });
    const s = statusesOf(r);
    assert.ok(!s.includes("completed") && !s.includes("failed") && !s.includes("credential_switch"), s.join(","));
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "the source clone is retained");
    assert.equal(r.turns.length, 2, "no turn ran after the fence");
  });
});

describe("issue #4201 m5: op-carrying failed and uncertain reports are fenced on the flight's registration nonce", () => {
  const cases: Array<[string, ApplyScript, string]> = [
    ["a proven unapplied refusal (not_applied)", { outcome: "not_applied", reason: "no_pending_switch" }, "credential_switch_failed"],
    ["refused_committed", { outcome: "refused_committed", reason: "already_committed" }, "credential_switch_uncertain"],
  ];
  for (const [name, script, status] of cases) {
    it(`${name}: the /state body sent through WorkerClient.reportState carries the apply request's register_nonce`, TIMEOUT, async () => {
      const r = await scenario({ site: "implement_turn", apply: [script] });
      const reports = r.states.filter((b) => b.status === status);
      assert.equal(reports.length, 1, statusesOf(r).join(","));
      const nonce = (reports[0] as { register_nonce?: string }).register_nonce;
      assert.ok(typeof nonce === "string" && nonce !== "", "the report carries a non-empty register_nonce");
      assert.equal((reports[0] as { op_id?: string }).op_id, "op-1");
      const sent = applied().map((b) => b.register_nonce);
      assert.ok(sent.length >= 1 && sent.every((n) => n === nonce), `the apply request(s) carried the same nonce: ${JSON.stringify(sent)}`);
      assert.equal(nonce, NONCE);
    });
  }
});

describe("issue #4201 m2c: an unproven quiescence fences instead of applying", () => {
  it("implement_turn: held with the op id, nothing applied, no turn on either token, hold_settled after the join", TIMEOUT, async () => {
    const r = await scenario({
      site: "implement_turn", apply: [{ outcome: "applied", token: NEW_TOKEN }],
      quiesce: (req) => (req.site === "in_place_credential_switch" ? BLOCKED : undefined),
    });
    const held = api.holdRequests.filter((h) => h.status === "credential_switch_held");
    assert.deepEqual(held.map((h) => [h.op_id, h.claim_generation, h.register_nonce]), [["op-1", GEN, NONCE]]);
    assert.equal(applied().length, 0, "no apply request");
    assert.equal(r.turns.length, 2, "the interrupted turn was the last: nothing restarted on either token");
    assert.ok(!statusesOf(r).includes("completed") && !statusesOf(r).includes("credential_switch"));
    assert.equal(api.holdState(r.runId), "unsettled", "the flight joined its operations and acknowledged the hold settled");
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
  });

  it("the hold report is retried through lost replies, then confirmed", TIMEOUT, async () => {
    newApi();
    api.dropHoldReplies("credential_switch_held", 3);
    const r = await scenario({
      site: "implement_turn", apply: [],
      quiesce: (req) => (req.site === "in_place_credential_switch" ? BLOCKED : undefined),
    });
    const held = api.holdRequests.filter((h) => h.status === "credential_switch_held");
    assert.equal(held.length, 4, "three dropped replies, then the confirmed fourth");
    assert.ok(held.every((h) => h.op_id === "op-1"));
    assert.equal(applied().length, 0);
    assert.equal(api.holdState(r.runId), "unsettled");
  });

  it("a lane that did not settle is unproven too (the executor's own word)", TIMEOUT, async () => {
    const real = SdkExecutor.prototype.settleWorkerCommands;
    SdkExecutor.prototype.settleWorkerCommands = async () => ({ confirmed: false });
    try {
      const r = await scenario({ site: "implement_turn", apply: [{ outcome: "applied", token: NEW_TOKEN }] });
      assert.equal(api.holdRequests.filter((h) => h.status === "credential_switch_held").length, 1);
      assert.equal(applied().length, 0);
      assert.equal(r.turns.length, 2);
    } finally { SdkExecutor.prototype.settleWorkerCommands = real; }
  });

});

describe("issue #4201 m2c: option A (an apply that may have committed)", () => {
  const A: Array<[string, ApplyScript[], (r: Result) => void]> = [
    ["refused_committed", [{ outcome: "refused_committed", reason: "already_committed" }], () => undefined],
    ["uncertain on every retry", [{ outcome: "garbage" }, { outcome: "garbage" }, { outcome: "garbage" }], (r) =>
      assert.equal(applied().length, 3, `bounded same-op retries: ${r.runId}`)],
  ];
  for (const [name, apply, extra] of A) {
    it(`${name}: credential_switch_uncertain with the op id, neither token used, custody kept`, TIMEOUT, async () => {
      const r = await scenario({ site: "implement_turn", apply });
      const uncertain = api.uncertainRequests.map((u) => u.body);
      assert.equal(uncertain.length, 1);
      assert.deepEqual([uncertain[0]!.op_id, uncertain[0]!.claim_generation, uncertain[0]!.register_nonce], ["op-1", GEN, NONCE]);
      const s = statusesOf(r);
      assert.ok(!s.includes("credential_switch") && !s.includes("credential_switch_failed") && !s.includes("completed") && !s.includes("failed"), s.join(","));
      assert.equal(r.turns.length, 2, "no turn after the report");
      assert.ok(r.turns.every((t) => t.token === OLD_TOKEN));
      assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
      extra(r);
    });
  }

  it("a route_absent after an ambiguous send is uncertain, not clean", TIMEOUT, async () => {
    const r = await scenario({ site: "implement_turn", apply: [{ outcome: "garbage" }, { outcome: "route_absent" }, { outcome: "route_absent" }] });
    assert.equal(api.uncertainRequests.length, 1);
    assert.ok(!statusesOf(r).includes("credential_switch"), "never released");
  });

  it("option A emits exactly one worker-owned credential_switch_uncertain frame, after the held frames, losing and duplicating none", TIMEOUT, async () => {
    const r = await scenario({ site: "implement_turn", apply: [{ outcome: "refused_committed", reason: "already_committed" }] });
    const frames = r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain");
    assert.equal(frames.length, 1, "exactly one uncertain frame");
    assert.deepEqual(frames[0]!.payload, { op_id: "op-1", generation: GEN });
    const seqs = r.posted.map((m) => m.seq);
    assert.equal(new Set(seqs).size, seqs.length, "no seq was sent twice");
    const others = r.posted.filter((m) => m !== frames[0]);
    const lastHeld = Math.max(...others.map((m) => m.seq));
    assert.ok(frames[0]!.seq > lastHeld || others.every((m) => m.seq < frames[0]!.seq), "the uncertain frame follows every frame emitted before it");
    const stored = feedFrames(r.runId).filter((m) => (m.kind as string) === "credential_switch_uncertain");
    assert.deepEqual(stored.map((m) => m.seq), [frames[0]!.seq], "the api stored it once, at the worker's seq");
    const missing = Array.from({ length: Math.max(...seqs) }, (_, i) => i + 1).filter((q) => !seqs.includes(q));
    assert.ok(missing.length <= 1, `at most the one slot reserved for a possible server frame is unused: ${missing.join(",")}`);
  });

  it("the uncertain report is retried through lost replies, then confirmed", TIMEOUT, async () => {
    const r = await scenario({
      site: "implement_turn", apply: [{ outcome: "refused_committed" }], before: (id) => api.dropUncertainReplies(id, 2),
    });
    assert.equal(api.uncertainRequests.length, 3, "two lost replies, then the confirmed third");
    assert.ok(api.uncertainRequests.every((u) => u.body.op_id === "op-1"));
    assert.equal(r.turns.length, 2, "neither token ran again");
    const frames = r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain");
    assert.equal(frames.length, 1, "three reports, one feed line");
  });

  it("three runner-level report attempts (two fail, the third confirms) leave exactly one feed line", TIMEOUT, async () => {
    // The client retries a dropped reply itself, so wire-level drops do not make the runner loop: fail the
    // runner's own reportState call twice.
    const real = client.reportState.bind(client);
    let attempts = 0;
    client.reportState = (async (runId: string, body: Parameters<typeof real>[1], ...rest: unknown[]) => {
      if (body.status === "credential_switch_uncertain" && ++attempts <= 2) throw new Error("transport down");
      return (real as (...a: unknown[]) => unknown)(runId, body, ...rest);
    }) as typeof client.reportState;
    let r: Result;
    try {
      r = await scenario({ site: "implement_turn", apply: [{ outcome: "refused_committed" }] });
    } finally { client.reportState = real; }
    assert.equal(attempts, 3, "the runner attempted the report three times");
    const frames = r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain");
    assert.equal(frames.length, 1, "three attempts, one feed line");
    assert.deepEqual(frames[0]!.payload, { op_id: "op-1", generation: GEN });
  });

  it("an api-refused uncertain report leaves no credential_switch_uncertain feed line (the run is not paused)", TIMEOUT, async () => {
    const real = client.reportState.bind(client);
    client.reportState = (async (runId: string, body: Parameters<typeof real>[1], ...rest: unknown[]) => {
      if (body.status === "credential_switch_uncertain") return { applied: false, status: "running" };
      return (real as (...a: unknown[]) => unknown)(runId, body, ...rest);
    }) as typeof client.reportState;
    let r: Result;
    try {
      r = await scenario({ site: "implement_turn", apply: [{ outcome: "refused_committed" }] });
    } finally { client.reportState = real; }
    assert.deepEqual(r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain"), []);
    assert.deepEqual(feedFrames(r.runId).filter((m) => (m.kind as string) === "credential_switch_uncertain"), []);
    assert.equal(r.turns.length, 2, "neither token ran again");
  });

  it("an uncertain report that is never confirmed (until the claim goes stale) leaves no feed line", TIMEOUT, async () => {
    const real = client.reportState.bind(client);
    let attempts = 0;
    client.reportState = (async (runId: string, body: Parameters<typeof real>[1], ...rest: unknown[]) => {
      if (body.status === "credential_switch_uncertain") {
        attempts++;
        // Capped fake: a stale-claim answer after 8 calls ends the retries, so a regression fails the assertions below.
        if (attempts > 8) return { applied: false, staleClaim: true, status: "queued" };
        throw new Error("transport down");
      }
      return (real as (...a: unknown[]) => unknown)(runId, body, ...rest);
    }) as typeof client.reportState;
    let r: Result;
    try {
      r = await scenario({ site: "implement_turn", apply: [{ outcome: "refused_committed" }] });
    } finally { client.reportState = real; }
    assert.equal(attempts, 9, "the report kept retrying past 5 attempts until the stale-claim answer");
    assert.deepEqual(r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain"), []);
  });
});

interface HoldSpy {
  releases: Array<{ messageSeq?: number }>;
  cancels: number;
  batcher: MessageBatcher | undefined;
  restore(): void;
}

/** Records how the runner disposes of the batcher's credential switch hold, and exposes the batcher. */
function spyHold(): HoldSpy {
  const real = MessageBatcher.prototype.holdForCredentialSwitch;
  const spy: HoldSpy = { releases: [], cancels: 0, batcher: undefined, restore: () => { MessageBatcher.prototype.holdForCredentialSwitch = real; } };
  MessageBatcher.prototype.holdForCredentialSwitch = function (this: MessageBatcher) {
    const hold = real.call(this);
    spy.batcher = this;
    return {
      ...hold,
      release: (arg: Parameters<typeof hold.release>[0]) => { spy.releases.push({ messageSeq: arg?.messageSeq }); return hold.release(arg); },
      cancel: () => { spy.cancels++; return hold.cancel(); },
    };
  };
  return spy;
}

/**
 * Replaces the client's apply call: the FIRST call commits the op on the server (the credential_switch frame
 * lands at tail+1) and emits two worker frames into the open hold, then every call answers from `answers`
 * (the last one repeats). This is the lost-reply shape: the server committed, the worker never learned it.
 */
function committedThenAnswers(spy: HoldSpy, answers: CredentialSwitchApplyResult[]): { calls: () => number; restore(): void } {
  const real = client.applyCredentialSwitch.bind(client);
  let calls = 0;
  client.applyCredentialSwitch = (async (runId: string, req: Parameters<typeof real>[1]) => {
    calls++;
    if (calls === 1) {
      (api as unknown as { applySwitch(r: string, o: string, t: string): unknown }).applySwitch(runId, req.opId, NEW_TOKEN);
      spy.batcher!.emit({ kind: "text", agent: "lead", payload: { text: "held-1" } });
      spy.batcher!.emit({ kind: "text", agent: "lead", payload: { text: "held-2" } });
    }
    return answers[Math.min(calls, answers.length) - 1]!;
  }) as typeof client.applyCredentialSwitch;
  return { calls: () => calls, restore: () => { client.applyCredentialSwitch = real; } };
}

/** As committedThenAnswers, but the first (ambiguous) send was rolled back on the server: nothing commits. */
function uncommittedThenAnswers(spy: HoldSpy, answers: CredentialSwitchApplyResult[]): { calls: () => number; restore(): void } {
  const real = client.applyCredentialSwitch.bind(client);
  let calls = 0;
  client.applyCredentialSwitch = (async () => {
    calls++;
    if (calls === 1) {
      spy.batcher!.emit({ kind: "text", agent: "lead", payload: { text: "held-1" } });
      spy.batcher!.emit({ kind: "text", agent: "lead", payload: { text: "held-2" } });
    }
    return answers[Math.min(calls, answers.length) - 1]!;
  }) as typeof client.applyCredentialSwitch;
  return { calls: () => calls, restore: () => { client.applyCredentialSwitch = real; } };
}

/** The hold was released (never cancelled) past the possibly committed apply: held frames number from tail+2. */
function assertReleasedPastPossibleApply(r: Result, spy: HoldSpy): void {
  const server = feedFrames(r.runId).find((m) => (m.kind as string) === "credential_switch");
  assert.ok(server, "the server committed its credential_switch frame");
  const S = server.seq;
  assert.equal(spy.cancels, 0, "the hold is never cancelled once an apply may have committed");
  assert.deepEqual(spy.releases, [{ messageSeq: S }], "released so the first held frame is tail+2 (the server frame owns tail+1)");
  const seqOf = (text: string): number | undefined => r.posted.find((m) => m.payload["text"] === text)?.seq;
  assert.equal(seqOf("held-1"), S + 1);
  assert.equal(seqOf("held-2"), S + 2);
  assert.ok(!r.posted.some((m) => m.seq === S), `no worker frame took the server's seq ${S}`);
}

describe("issue #4201 m2c rework: a definite non-applied answer after an ambiguous send keeps the sequence rule and the ledger memory", () => {
  const AMBIGUOUS: CredentialSwitchApplyResult = { kind: "uncertain", detail: "transport_error" };

  it("uncertain then fenced: the hold is released past the possible apply, the op stays ambiguous, the flight stops", TIMEOUT, async () => {
    const spy = spyHold();
    const apply = committedThenAnswers(spy, [AMBIGUOUS, { kind: "fenced" }]);
    try {
      const r = await scenario({ site: "implement_turn", apply: [] });
      assert.equal(apply.calls(), 2);
      assertReleasedPastPossibleApply(r, spy);
      assert.equal((await r.ledger.stateOf(r.runId, "op-1"))?.state, "ambiguous", "a fenced answer does not settle an op whose earlier send may have committed");
      assert.ok(!statusesOf(r).includes("completed") && !statusesOf(r).includes("credential_switch"), statusesOf(r).join(","));
    } finally { apply.restore(); spy.restore(); }
  });

  // A not_applied whose reason leaves the op pending (anything outside NOT_APPLIED_PROOF_REASONS) does NOT prove the
  // earlier send never committed: a delayed send could still land. Option A, never the give-up.
  for (const reason of ["credential_busy", "vault_locked", "credential_unavailable"]) {
    it(`uncertain then not_applied:${reason}: option A, hold released past the possible apply, op stays ambiguous`, TIMEOUT, async () => {
      const spy = spyHold();
      const apply = committedThenAnswers(spy, [AMBIGUOUS, { kind: "not_applied", reason }]);
      try {
        const r = await scenario({ site: "implement_turn", apply: [] });
        assert.equal(apply.calls(), 2);
        assertReleasedPastPossibleApply(r, spy);
        assert.equal(api.uncertainRequests.length, 1, "credential_switch_uncertain was reported");
        assert.equal(api.uncertainRequests[0]!.body.op_id, "op-1");
        assert.ok(!statusesOf(r).includes("credential_switch_failed"), `no give-up: ${statusesOf(r).join(",")}`);
        assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
        assert.equal(r.turns.length, 2, "neither token ran again");
        assert.equal((await r.ledger.stateOf(r.runId, "op-1"))?.state, "ambiguous", "the ledger keeps the memory of the possible commit");
      } finally { apply.restore(); spy.restore(); }
    });
  }

  // The server answers superseded, not_applied:no_pending_switch and not_applied:generation_mismatch only AFTER
  // its replay check, in the same transaction under the run lock: a committed op would have replayed as
  // applied. So after an uncertain send these three answers prove the op never committed, and the hold must be CANCELLED (frames continue at tail+1, no seq
  // hole) and the ledger settled (review NB1).
  for (const answer of [{ kind: "superseded" }, { kind: "not_applied", reason: "no_pending_switch" }, { kind: "not_applied", reason: "generation_mismatch" }] as CredentialSwitchApplyResult[]) {
    it(`uncertain (rolled back) then ${answer.kind === "not_applied" ? `${answer.kind}:${answer.reason}` : answer.kind}: the hold is cancelled with no seq hole and the op settles`, TIMEOUT, async () => {
      const spy = spyHold();
      const apply = uncommittedThenAnswers(spy, [AMBIGUOUS, answer]);
      try {
        const r = await scenario({ site: "implement_turn", apply: [], extra: { switchSupersedeWaitMs: 50 } });
        assert.equal(apply.calls(), 2);
        assert.equal(spy.cancels, 1, "a proven non-commit cancels the hold");
        assert.deepEqual(spy.releases, [], "never released past an apply that cannot exist");
        assert.ok(!feedFrames(r.runId).some((m) => (m.kind as string) === "credential_switch"), "nothing committed");
        const seqs = r.posted.map((m) => m.seq).sort((a, b) => a - b);
        for (let i = 1; i < seqs.length; i++) assert.ok(seqs[i]! - seqs[i - 1]! <= 1, `no seq hole: ${seqs.join(",")}`);
        assert.ok(r.posted.some((m) => m.payload["text"] === "held-1"), "the held frames were delivered");
        // Settled, so terminal cleanup may already have removed it; never left ambiguous.
        assert.notEqual((await r.ledger.stateOf(r.runId, "op-1"))?.state, "ambiguous");
      } finally { apply.restore(); spy.restore(); }
    });
  }
});

describe("issue #4201: an ambiguity persisted in the ledger reaches the runner (restart or re-entry)", () => {
  /** A ledger that, on the FIRST markSending, first leaves the op the way an earlier invocation did. */
  class PriorLedger extends CredentialSwitchOpLedger {
    private seeded = false;
    constructor(dir: string, private readonly prior: "ambiguous" | "sending") { super(dir); }
    override async markSending(runId: string, opId: string, generation: number): Promise<{ wasAmbiguous: boolean }> {
      if (!this.seeded) {
        this.seeded = true;
        await super.markSending(runId, opId, generation); // leaves `sending`: the crash-left shape
        if (this.prior === "ambiguous") await this.record(runId, opId, "ambiguous", "uncertain", generation);
      }
      return super.markSending(runId, opId, generation);
    }
  }
  for (const prior of ["ambiguous", "sending"] as const) {
    it(`a prior ${prior} ledger entry, then an immediate not_applied:credential_busy: option A, no give-up, no turn on either token`, TIMEOUT, async () => {
      const r = await scenario({
        site: "implement_turn", apply: [{ outcome: "not_applied", reason: "credential_busy" }],
        ledger: new PriorLedger(fx.dataDir, prior),
      });
      assert.equal(applied().length, 1, "one apply request");
      assert.equal(api.uncertainRequests.length, 1, "credential_switch_uncertain was reported");
      assert.equal(api.uncertainRequests[0]!.body.op_id, "op-1");
      assert.ok(!statusesOf(r).includes("credential_switch_failed"), `no give-up: ${statusesOf(r).join(",")}`);
      assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
      assert.equal(r.turns.length, 2, "neither token ran again");
      assert.ok(r.turns.every((t) => t.token === OLD_TOKEN));
      assert.equal((await r.ledger.stateOf(r.runId, "op-1"))?.state, "ambiguous");
    });
  }
});

describe("issue #4201 m2c rework: option A releases the hold past a possible apply (tail+2)", () => {
  it("refused_committed: frames emitted while the hold is open number from tail+2; a server frame at tail+1 survives", TIMEOUT, async () => {
    const spy = spyHold();
    const apply = committedThenAnswers(spy, [{ kind: "refused_committed", reason: "already_committed" }]);
    try {
      const r = await scenario({ site: "implement_turn", apply: [] });
      assert.equal(api.uncertainRequests.length, 1);
      assertReleasedPastPossibleApply(r, spy);
    } finally { apply.restore(); spy.restore(); }
  });

  it("uncertain on every retry: same sequence rule", TIMEOUT, async () => {
    const spy = spyHold();
    const apply = committedThenAnswers(spy, [{ kind: "uncertain", detail: "transport_error" }]);
    try {
      const r = await scenario({ site: "implement_turn", apply: [] });
      assert.equal(apply.calls(), 3);
      assert.equal(api.uncertainRequests.length, 1);
      assertReleasedPastPossibleApply(r, spy);
    } finally { apply.restore(); spy.restore(); }
  });
});

describe("issue #4201 m2c rework: the in-place route is closed without the negotiated capability or feature", () => {
  async function v1ReleaseRun(tweak: () => void) {
    git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
    mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
    git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
    git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
    git.checkpointPack = (async () => null) as typeof git.checkpointPack;
    newApi();
    const claim = freshClaim(GEN);
    negotiate();
    tweak();
    // Were the in-place route wrongly taken, this would apply.
    api.scriptApply(claim.run_id, { outcome: "applied", token: NEW_TOKEN });
    const sdk = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: scriptedQuery([planTurn(), hangTurn(() => {
        api.overrideStateStatus(claim.run_id, "queued");
        api.requestCredentialSwitch(claim.run_id, GEN, "op-1"); // the signal carries an op id
      }), doneTurn()], []), client,
    });
    api.onState(claim.run_id, (b) => { if (b.status === "awaiting_approval") send(claim.run_id, row("approve_plan")); });
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor: sdk }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir),
      quiesceRun: async () => CLEAN,
    }).execute(claim);
    return api.states.filter((x) => x.runId === claim.run_id).map((x) => x.body.status as string);
  }

  for (const [name, tweak] of [
    ["the worker did not register the capability", () => {
      (client as unknown as { hasInPlaceCredentialSwitchCapability: boolean }).hasInPlaceCredentialSwitchCapability = false;
    }],
    ["the server does not advertise the feature", () => {
      client.protocolFeatures = client.protocolFeatures.filter((f) => f !== "credential_switch_apply_v1");
    }],
  ] as const) {
    it(`${name}: the v1 release path, no apply request, no in-place report`, TIMEOUT, async () => {
      const s = await v1ReleaseRun(tweak);
      assert.equal(applied().length, 0, "no apply request was sent");
      assert.ok(s.includes("credential_switch"), `released as today: ${s.join(",")}`);
      for (const status of ["credential_switch_held", "credential_switch_failed", "credential_switch_uncertain", "completed"])
        assert.ok(!s.includes(status), `no ${status}: ${s.join(",")}`);
      assert.equal(api.holdRequests.length, 0);
      assert.equal(api.uncertainRequests.length, 0);
    });
  }
});

describe("issue #4201 m2c rework: each half of the lane-settled proof fences on its own", () => {
  it("runner: the executor's laneSettlementProven() === false alone (settle otherwise confirmed) holds with the op id and applies nothing", TIMEOUT, async () => {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    const executor = {
      laneSettlementProven: () => false,
      run: async (ctx: import("../src/executor.js").RunContext) => {
        api.requestCredentialSwitch(runId, GEN, "op-1");
        assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
        throw ctx.signal!.reason;
      },
    };
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir), quiesceRun: async () => CLEAN,
    }).execute(claim);
    assert.deepEqual(api.holdRequests.filter((h) => h.status === "credential_switch_held").map((h) => h.op_id), ["op-1"]);
    assert.equal(applied().length, 0, "no apply request");
    assert.equal(api.uncertainRequests.length, 0);
  });

  // The executor's own poisoned-lane term (guardLane.poisoned) is pinned in sdk-executor-credential-switch.test.ts:
  // the in-place scenarios here have no guarded lane (no capturedMemory), so it cannot be reached from this harness.
});

describe("issue #4201: an unconfirmed in-place report keeps retrying while the claim is live", () => {
  // Counted at the runner's reportState call (the client retries a dropped connection by itself, so wire requests
  // are a multiple of the runner's attempts). `outage` calls throw a transport error; call outage+1 answers
  // `after`: a recovery (the real client), a stale claim, or a definitive refusal. A stale/refusal cap makes a
  // regression fail an assertion within seconds instead of hanging until the test timeout.
  async function outage(status: string, outageCalls: number, after: "recover" | "stale" | "refuse", run: () => ReturnType<typeof scenario>) {
    const realReport = client.reportState.bind(client);
    let attempts = 0;
    client.reportState = (async (runId: string, body: Parameters<typeof realReport>[1], ...rest: unknown[]) => {
      if (body.status === status) {
        attempts++;
        if (attempts <= outageCalls) throw new Error("transport down");
        if (after === "stale") return { applied: false, staleClaim: true, status: "queued" };
        if (after === "refuse") return { applied: false, status: "running" };
      }
      return (realReport as (...a: unknown[]) => unknown)(runId, body, ...rest);
    }) as typeof client.reportState;
    try {
      const r = await run();
      return { r, attempts };
    } finally { client.reportState = realReport as typeof client.reportState; }
  }
  const heldScenario = () => scenario({
    site: "implement_turn", apply: [],
    quiesce: (req) => (req.site === "in_place_credential_switch" ? BLOCKED : undefined),
  });
  const uncertainScenario = () => scenario({ site: "implement_turn", apply: [{ outcome: "refused_committed" }] });

  it("a hold report outage longer than 5 attempts that then recovers ends confirmed (held in place)", TIMEOUT, async () => {
    const { r, attempts } = await outage("credential_switch_held", 7, "recover", heldScenario);
    assert.equal(attempts, 8, "seven failed attempts, then the confirmed eighth");
    assert.equal(api.holdState(r.runId), "unsettled", "the hold is confirmed server-side");
    assert.equal(applied().length, 0);
    assert.equal(r.turns.length, 2, "no turn after the report");
  });

  it("a hold report that never recovers keeps retrying until a stale-claim answer ends it, work retained", TIMEOUT, async () => {
    const { r, attempts } = await outage("credential_switch_held", 7, "stale", heldScenario);
    assert.equal(attempts, 8, "retried past 5 attempts; the stale-claim answer ended it");
    assert.equal(applied().length, 0);
    assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
    assert.equal(r.turns.length, 2, "no turn after the report");
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
  });

  it("a definitively refused hold report stops at once (one attempt)", TIMEOUT, async () => {
    const { r, attempts } = await outage("credential_switch_held", 0, "refuse", heldScenario);
    assert.equal(attempts, 1);
    assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
  });

  it("an uncertain report outage longer than 5 attempts that then recovers ends confirmed (option A in place)", TIMEOUT, async () => {
    const { r, attempts } = await outage("credential_switch_uncertain", 7, "recover", uncertainScenario);
    assert.equal(attempts, 8, "seven failed attempts, then the confirmed eighth");
    const frames = r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain");
    assert.equal(frames.length, 1, "one feed line, only once confirmed");
    assert.equal(r.turns.length, 2, "neither token ran again");
  });

  it("an uncertain report that never recovers keeps retrying until a stale-claim answer ends it, work retained", TIMEOUT, async () => {
    const { r, attempts } = await outage("credential_switch_uncertain", 7, "stale", uncertainScenario);
    assert.equal(attempts, 8, "retried past 5 attempts; the stale-claim answer ended it");
    assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
    assert.equal(r.turns.length, 2, "neither token ran again");
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
  });

  it("a definitively refused uncertain report stops at once (one attempt)", TIMEOUT, async () => {
    const { attempts } = await outage("credential_switch_uncertain", 0, "refuse", uncertainScenario);
    assert.equal(attempts, 1);
  });

  // `behave(attempt, real)` answers each runner-level report of `status`. A regression that keeps retrying a
  // deterministic failure gets a stale-claim answer after 7 calls, so it fails an assertion instead of hanging.
  async function scripted(
    status: string, behave: (attempt: number, real: () => Promise<unknown>) => Promise<unknown>, run: () => ReturnType<typeof scenario>,
  ) {
    const realReport = client.reportState.bind(client);
    let attempts = 0;
    client.reportState = (async (runId: string, body: Parameters<typeof realReport>[1], ...rest: unknown[]) => {
      if (body.status !== status) return (realReport as (...a: unknown[]) => unknown)(runId, body, ...rest);
      attempts++;
      return behave(attempts, () => (realReport as (...a: unknown[]) => Promise<unknown>)(runId, body, ...rest));
    }) as typeof client.reportState;
    try {
      const r = await run();
      return { r, attempts };
    } finally { client.reportState = realReport as typeof client.reportState; }
  }
  const STALE = { applied: false, staleClaim: true, status: "queued" };
  const precondition = (): Error => new ReportPreconditionError("credential switch uncertain report requires the credential_switch_apply_v1 feature, a registration nonce, a positive claim_generation and an op_id");

  for (const [label, status, make, failure] of [
    ["a 400 RequestError", "credential_switch_held", heldScenario, () => new RequestError("POST", "/state", 400, "bad request")],
    ["a 400 RequestError", "credential_switch_uncertain", uncertainScenario, () => new RequestError("POST", "/state", 400, "bad request")],
    ["the client's local precondition throw", "credential_switch_held", heldScenario, precondition],
    ["the client's local precondition throw", "credential_switch_uncertain", uncertainScenario, precondition],
  ] as const) {
    it(`${status}: ${label} is deterministic, so the in-place report stops at once unconfirmed with work and custody kept`, TIMEOUT, async () => {
      const { r, attempts } = await scripted(status, async (n) => { if (n <= 7) throw failure(); return STALE; }, make);
      assert.equal(attempts, 1, "a non-transient failure is not retried");
      assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
      assert.equal(r.turns.length, 2, "no turn after the report");
      assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
      assert.equal(r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain").length, 0, "no uncertain feed line");
      assert.equal(api.holdState(r.runId), undefined, "no hold was confirmed");
    });
  }

  // The flight's own report wrapper throws RetainedRecoveryStop when an iteration park has begun (runner.ts, the
  // `flight.iterationParkState !== "none"` check) while guard() still passes. That stop must end the loop, not be
  // retried as a transient failure.
  for (const [status, make] of [["credential_switch_held", heldScenario], ["credential_switch_uncertain", uncertainScenario]] as const) {
    it(`${status}: a RetainedRecoveryStop from the report itself ends the loop at once (it is rethrown, not retried)`, TIMEOUT, async () => {
      const { r, attempts } = await scripted(status, async (n) => { if (n <= 7) throw new RetainedRecoveryStop(); return STALE; }, make);
      assert.equal(attempts, 1, "a retained stop is not retried");
      assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
      assert.equal(r.turns.length, 2, "no turn after the report");
      assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
      assert.equal(r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain").length, 0, "no uncertain feed line");
    });
  }

  it("an uncertain report whose every reply is lost ends confirmed by the ownership probe from the fifth attempt", TIMEOUT, async () => {
    // The first report reaches the server (it pauses the run) and its reply is lost; every attempt fails at the transport.
    const { r, attempts } = await scripted("credential_switch_uncertain", async (n, real) => {
      if (n === 1) await real();
      if (n > 7) return STALE;
      throw new Error("transport down");
    }, uncertainScenario);
    assert.equal(attempts, 5, "the probe runs after attempt index 4 and confirms");
    assert.ok(api.ownershipRequests.includes(r.runId), "the ownership read confirmed it");
    assert.equal(r.posted.filter((m) => (m.kind as string) === "credential_switch_uncertain").length, 1, "exactly one feed line");
    assert.equal(r.turns.length, 2, "neither token ran again");
  });

  it("a hold report whose every reply is lost ends confirmed by the ownership probe from the fifth attempt", TIMEOUT, async () => {
    const { r, attempts } = await scripted("credential_switch_held", async (n, real) => {
      if (n === 1) await real();
      if (n > 7) return STALE;
      throw new Error("transport down");
    }, heldScenario);
    assert.equal(attempts, 5, "the probe runs after attempt index 4 and confirms");
    assert.notEqual(api.holdState(r.runId), undefined, "the hold the lost reply landed is confirmed");
    assert.equal(r.turns.length, 2, "no turn after the report");
  });

  for (const [status, make] of [["credential_switch_held", heldScenario], ["credential_switch_uncertain", uncertainScenario]] as const) {
    it(`${status}: a cancel delivered while the report retries ends the loop as a cancel, not a swallowed error`, TIMEOUT, async () => {
      const real = SteeringChannel.prototype.isCancelled;
      try {
        const { r, attempts } = await scripted(status, async (n) => {
          if (n === 1) SteeringChannel.prototype.isCancelled = () => true;
          if (n <= 7) throw new Error("transport down");
          return STALE;
        }, make);
        assert.equal(attempts, 1, "the cancel ended the loop");
        assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
        const failed = r.states.find((b) => b.status === "failed");
        assert.ok(failed, `reported through the retained-stop arm: ${statusesOf(r).join(",")}`);
        assert.match(String((failed as { failure_reason?: string }).failure_reason ?? ""), /cancel/i);
        assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
      } finally { SteeringChannel.prototype.isCancelled = real; }
    });
  }
});

describe("issue #4201 m2c: a token published later is scrubbed from every sink", () => {
  it("failure_reason and every posted frame of the run (the batcher sinks are covered below)", TIMEOUT, async () => {
    const r = await scenario({
      site: "implement_turn", apply: [{ outcome: "applied", token: NEW_TOKEN }],
      lastTurn: () => (async function* () {
        yield initFrame();
        throw new Error(`auth failed for ${NEW_TOKEN}`);
      })(),
    });
    const failed = r.states.filter((b) => b.status === "failed").at(-1);
    assert.ok(failed, `the run failed: ${statusesOf(r).join(",")}`);
    assert.ok(!String(failed.failure_reason).includes(NEW_TOKEN), "failure_reason is scrubbed");
    assert.ok(!JSON.stringify(api.messages(r.runId)).includes(NEW_TOKEN));
  });
});

describe("issue #4201 m2c: a token added to the flight's SecretSet later is scrubbed from every batcher sink", () => {
  const RUN = "run-secret-set";
  const cleanups: string[] = [];
  const stub = (failing: boolean): { client: WorkerClient; landed: OutgoingMessage[] } => {
    const landed: OutgoingMessage[] = [];
    const client = {
      async postMessages(_id: string, msgs: OutgoingMessage[]): Promise<void> {
        if (failing) throw new RequestError("POST", "/messages", 503, '{"error":"unavailable"}');
        landed.push(...msgs);
      },
    } as unknown as WorkerClient;
    return { client, landed };
  };

  it("a posted frame and a tombstone", async () => {
    const secrets = new SecretSet(["forge-pat-0123456789"]);
    const a = stub(false);
    const b = new MessageBatcher(a.client, RUN, 0, 0, nullLogger(), secrets.payload, secrets.text, { generation: 1 });
    secrets.add(NEW_TOKEN); // published AFTER the batcher was built
    b.emit({ kind: "text", agent: "lead", agentLabel: `label ${NEW_TOKEN}`, payload: { text: `leaked ${NEW_TOKEN}` } });
    // An oversize frame is replaced by a tombstone that keeps the original attribution.
    b.emit({ kind: "text", agent: "lead", agentLabel: `label ${NEW_TOKEN}`, payload: { text: "x".repeat(MAX_MESSAGE_BYTES + 10) } });
    await b.close();
    assert.equal(a.landed.length, 2);
    assert.ok(!JSON.stringify(a.landed).includes(NEW_TOKEN), "neither the frame nor the tombstone carries the token");
    assert.ok(JSON.stringify(a.landed[0]).includes("***REDACTED***"));
    assert.equal(a.landed[1]!.payload["event"], "message_truncated");
    assert.equal(a.landed[1]!.kind, "status");
  });

  it("a spilled outbox segment", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "secret-set-outbox-"));
    cleanups.push(dir);
    const outbox = new Outbox({ root: path.join(dir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
    await outbox.init();
    const secrets = new SecretSet(["forge-pat-0123456789"]);
    const a = stub(true);
    const b = new MessageBatcher(a.client, RUN, 0, 5, nullLogger(), secrets.payload, secrets.text, { outbox, generation: 1, transientTripMs: 30 });
    secrets.add(NEW_TOKEN);
    b.emit({ kind: "text", agent: "lead", payload: { text: `spilled ${NEW_TOKEN}` } });
    assert.ok(await until(() => b.isSpilled(), 4_000), "the batcher spilled to the outbox");
    await b.close();
    const replayed: OutgoingMessage[] = [];
    await outbox.drainRun(RUN, async (batch) => { replayed.push(...batch); });
    assert.equal(replayed.length, 1);
    assert.ok(!JSON.stringify(replayed).includes(NEW_TOKEN), "the journaled segment never held the token");
    const onDisk = fs.readdirSync(dir, { recursive: true, withFileTypes: true }).filter((e) => e.isFile())
      .map((e) => fs.readFileSync(path.join(e.parentPath, e.name), "utf8")).join("\n");
    assert.ok(!onDisk.includes(NEW_TOKEN), "nothing on disk under the outbox holds the token");
    for (const d of cleanups.splice(0)) fs.rmSync(d, { recursive: true, force: true });
  });
});

describe("issue #4201 m2c: SEQ RULE for an apply that may have committed", () => {
  it("held frames are released from tail+2, so a server frame at tail+1 never collides", async () => {
    const landed: OutgoingMessage[] = [];
    const client = { async postMessages(_id: string, msgs: OutgoingMessage[]): Promise<void> { landed.push(...msgs); } } as unknown as WorkerClient;
    const b = new MessageBatcher(client, "run-seq-rule", 7, 0, nullLogger());
    const hold = b.holdForCredentialSwitch();
    assert.deepEqual(await hold.prepare(), { prepared: true });
    b.emit({ kind: "text", agent: "lead", payload: { text: "h1" } });
    b.emit({ kind: "text", agent: "lead", payload: { text: "h2" } });
    // The apply may have committed: its credential_switch frame could already own seq 8.
    assert.equal(hold.release({ messageSeq: b.currentSeq() + 1 }), true);
    b.emit({ kind: "text", agent: "lead", payload: { text: "after" } });
    await b.close();
    assert.deepEqual(landed.map((m) => m.seq), [9, 10, 11]);
    // The contrast: cancel (nothing applied) would have numbered the first held frame 8.
    const again: OutgoingMessage[] = [];
    const b2 = new MessageBatcher({ async postMessages(_id: string, msgs: OutgoingMessage[]) { again.push(...msgs); } } as unknown as WorkerClient, "run-seq-rule-2", 7, 0, nullLogger());
    const hold2 = b2.holdForCredentialSwitch();
    b2.emit({ kind: "text", agent: "lead", payload: { text: "h1" } });
    hold2.cancel();
    await b2.close();
    assert.deepEqual(again.map((m) => m.seq), [8]);
  });
});

describe("issue #4201 m2d: a switch that arrives during the prologue is latched and served at the first executor site", () => {
  it("applied in place before any model turn; no window threw it, nothing was released or held", TIMEOUT, async () => {
    const r = await scenario({ site: "window", apply: [{ outcome: "applied", token: NEW_TOKEN }] });
    assert.deepEqual(applied().map((b) => b.op_id), ["op-1"]);
    const s = statusesOf(r);
    for (const status of ["credential_switch", "credential_switch_held", "credential_switch_failed", "credential_switch_uncertain", "failed"])
      assert.ok(!s.includes(status), `no ${status} report: ${s.join(",")}`);
    assert.ok(s.includes("completed"), s.join(","));
    assert.ok(r.turns.length >= 2 && r.turns.every((t) => t.token === NEW_TOKEN), "every turn ran on the new token: the apply came first");
    assert.deepEqual(r.attempts(), r.attemptsBefore, "no new attempt dir");
    assert.ok(!r.states.some((b) => (b.status as string) === "queued"), "never a requeue");
  });
});

describe("issue #4201 m2d: a prologue that ends discards the latched switch", () => {
  it("the prologue throws after a switch was latched: the hold is closed without service, nothing is applied, the stamp stays pending", TIMEOUT, async () => {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.requestCredentialSwitch(runId, GEN, "op-1");
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    const turns: TurnRecord[] = [];
    const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery([planTurn(), doneTurn()], turns), client });
    const { gitlab } = fakeGitlab();
    const r = runnerWith(() => ({ executor: sdk }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir), quiesceRun: async () => CLEAN,
      switchSupersedeWaitMs: 2_000, questionTimeoutMs: 20_000,
    });
    // The prologue's clone step throws, after the first `running` ack latched the pending stamp under the hold.
    type Steer = { credentialSwitchHoldOpen(): boolean; tripCredentialSwitch(generation: number, opId?: string): void; latchedSwitchGeneration?: number };
    let steering: Steer | undefined;
    let holdOpenAtThrow: boolean | undefined;
    let latchedAtThrow: number | undefined;
    const realClone = git.ensureClone.bind(git);
    // Records how every hold opened on this run's channel was closed.
    const closes: Array<{ service: boolean }> = [];
    const proto = SteeringChannel.prototype;
    const realHold = proto.holdCredentialSwitch;
    proto.holdCredentialSwitch = function (this: SteeringChannel) {
      const hold = realHold.call(this);
      const close = hold.close.bind(hold);
      hold.close = (o) => { closes.push({ service: o.service }); close(o); };
      return hold;
    };
    git.ensureClone = (async () => {
      steering = (r as unknown as { activeRuns: Map<string, { steering: Steer }> }).activeRuns.get(runId)!.steering;
      holdOpenAtThrow = steering.credentialSwitchHoldOpen();
      steering.tripCredentialSwitch(GEN, "op-1"); // the stamp is observed mid-prologue: the hold latches it
      latchedAtThrow = steering.latchedSwitchGeneration;
      throw new RequestError("GET", "/clone", 404, "repository not found");
    }) as typeof git.ensureClone;
    try {
      await r.execute(claim);
    } finally {
      git.ensureClone = realClone;
      proto.holdCredentialSwitch = realHold;
    }
    const r2 = { states: api.states };
    assert.equal(holdOpenAtThrow, true, "the prologue hold was open when the prologue threw");
    assert.equal(latchedAtThrow, GEN, "the switch was latched by the hold");
    const s = r2.states.filter((x) => x.runId === runId).map((x) => x.body.status);
    assert.ok(!s.includes("credential_switch") && !s.includes("credential_switch_held"), `the failed prologue is a failure, never a switch release: ${s.join(",")}`);
    assert.equal(steering!.credentialSwitchHoldOpen(), false, "the prologue hold is closed when the flight ends");
    assert.ok(closes.length >= 1 && closes.every((c) => !c.service), `the prologue hold was closed, and only ever to discard the latch: ${JSON.stringify(closes)}`);
    assert.equal(applied().length, 0, "no switch was applied");
    assert.equal(turns.length, 0, "no model turn ran");
    assert.equal((api as unknown as { switchPendingRuns: Set<string> }).switchPendingRuns.has(runId), true, "the stamp is still pending server-side");
  });
});

describe("issue #4201 m2d: the signal escaped the executor (the escaped arm)", () => {
  /** A stub executor that never handles the switch: it waits for it and rethrows (as a test executor does). */
  async function escaped(quiesce: (req: QuiesceRunRequest) => QuiesceRunOutcome | undefined) {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    let ran = 0;
    const cwds: string[] = [];
    const attemptDir = path.dirname(worktreeDirFor(0));
    const attempts = (): string[] =>
      fs.existsSync(attemptDir) ? fs.readdirSync(attemptDir).filter((n) => n.includes(".attempt-")).sort() : [];
    const attemptsAtTurn: string[] = [];
    const executor = {
      run: async (ctx: import("../src/executor.js").RunContext) => {
        ran++;
        cwds.push(ctx.worktreePath);
        attemptsAtTurn.push(...attempts());
        api.requestCredentialSwitch(runId, GEN, "op-1");
        assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
        throw ctx.signal!.reason;
      },
    };
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir),
      quiesceRun: async (req) => quiesce(req) ?? CLEAN,
    }).execute(claim);
    return { runId, ran, cwds, attemptsAtTurn, attempts, states: api.states.filter((x) => x.runId === runId).map((x) => x.body) };
  }

  /** The escaped arm neither moves the flight to another generation nor creates another clone. */
  function assertSameFlight(r: Awaited<ReturnType<typeof escaped>>): void {
    assert.ok(r.states.every((b) => b.claim_generation === undefined || b.claim_generation === GEN), "the generation never changed");
    assert.deepEqual(r.attempts(), r.attemptsAtTurn, "no attempt dir was created by the switch");
    assert.equal(r.cwds.length, 1, "the executor ran once, in one cwd");
  }

  it("proven quiescent: credential_switch_uncertain with the op id; never a release, an apply or a requeue", TIMEOUT, async () => {
    const r = await escaped(() => undefined);
    const s: string[] = r.states.map((b) => b.status);
    assert.equal(api.uncertainRequests.length, 1, s.join(","));
    assert.equal(api.uncertainRequests[0]!.body.op_id, "op-1");
    assert.equal(applied().length, 0, "the executor has unwound: nothing is applied");
    for (const status of ["credential_switch", "queued", "completed", "failed"]) assert.ok(!s.includes(status), `no ${status}: ${s.join(",")}`);
    assertSameFlight(r);
  });

  it("unproven: credential_switch_held with the op id, then settled after the join; never a requeue", TIMEOUT, async () => {
    const r = await escaped((req) => (req.site === "in_place_credential_switch" ? BLOCKED : undefined));
    const s: string[] = r.states.map((b) => b.status);
    const held = api.holdRequests.filter((h) => h.status === "credential_switch_held");
    assert.deepEqual(held.map((h) => h.op_id), ["op-1"], s.join(","));
    assert.equal(applied().length, 0);
    assert.equal(api.uncertainRequests.length, 0);
    for (const status of ["credential_switch", "queued", "completed", "failed"]) assert.ok(!s.includes(status), `no ${status}: ${s.join(",")}`);
    assert.equal(api.holdState(r.runId), "unsettled");
    assertSameFlight(r);
  });

  it("a Codex flight on a negotiated worker never takes the in-place fence: no hold and no uncertain report, as before m3a", TIMEOUT, async () => {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    const rig = codexRig();
    const executor = new FakeCodexExecutor(rig.safety, async (ctx) => {
      api.requestCredentialSwitch(runId, GEN, "op-1");
      assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
      throw ctx.signal!.reason;
    });
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir),
      quiesceRun: async () => CLEAN,
    }).execute(claim);
    assert.deepEqual(api.holdRequests.filter((h) => h.runId === runId), [], "no credential_switch_held");
    assert.equal(api.uncertainRequests.filter((u) => u.runId === runId).length, 0);
    assert.equal(applied().length, 0, "Codex never applies in place");
  });
});

describe("issue #4201 m2d rework: the escaped arm keeps lifecycle stops and the settled-signal rule", () => {
  type Ctx = import("../src/executor.js").RunContext;
  type Steer = { markCancelled(): void };

  it("N1: a cancel during the escaped proof is a cancel (reported, custody kept), not a swallowed settle failure", TIMEOUT, async () => {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    const executor = {
      run: async (ctx: Ctx) => {
        api.requestCredentialSwitch(runId, GEN, "op-1");
        assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
        throw ctx.signal!.reason;
      },
    };
    const { gitlab } = fakeGitlab();
    let rr!: ReturnType<typeof runnerWith>;
    let cancelled = 0;
    rr = runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir),
      quiesceRun: async (req) => {
        if (req.site === "in_place_credential_switch" && cancelled++ === 0) {
          (rr as unknown as { activeRuns: Map<string, { steering: Steer }> }).activeRuns.get(runId)!.steering.markCancelled();
        }
        return CLEAN;
      },
    });
    await rr.execute(claim);
    const states = api.states.filter((x) => x.runId === runId).map((x) => x.body);
    const s: string[] = states.map((b) => b.status);
    assert.equal(cancelled >= 1, true, "the cancel landed during the escaped proof");
    assert.equal(api.uncertainRequests.length, 0, `a cancelled flight reports no uncertain switch: ${s.join(",")}`);
    assert.equal(applied().length, 0);
    const failed = states.find((b) => b.status === "failed");
    assert.ok(failed, `the cancel is reported through the retained-stop arm: ${s.join(",")}`);
    assert.match(String((failed as { failure_reason?: string }).failure_reason ?? ""), /cancel/i);
  });

  it("N2: a spent switch signal surfacing after an apply never takes the v1 release", TIMEOUT, async () => {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    const executor = {
      run: async (ctx: Ctx) => {
        api.requestCredentialSwitch(runId, GEN, "op-1");
        assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
        const spent = ctx.signal!.reason;
        assert.equal(await ctx.attemptCredentialSwitch!({ site: "implement_turn", laneSettled: true }), "applied");
        throw spent; // the old, now settled, signal surfaces after the apply
      },
    };
    const { gitlab } = fakeGitlab();
    const rec = recordingLogger();
    await runnerWith(() => ({ executor }), gitlab, undefined, rec.logger, {
      recoveryRetryMs: 1, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir), quiesceRun: async () => CLEAN,
    }).execute(claim);
    const s: string[] = api.states.filter((x) => x.runId === runId).map((x) => x.body.status);
    assert.deepEqual(applied().map((b) => b.op_id), ["op-1"], "the apply happened once");
    for (const status of ["credential_switch", "credential_switch_uncertain", "credential_switch_held", "queued", "completed", "failed"])
      assert.ok(!s.includes(status), `no ${status} after an applied switch: ${s.join(",")}`);
    // Custody kept and no terminal or hold report of any kind: the settled-signal arm ended the flight.
    assert.equal(fs.existsSync(worktreeDirFor(claim.issue_iid as number)), true, "the source clone is retained");
    assert.equal(api.holdRequests.length, 0, "no hold report");
    assert.equal(api.uncertainRequests.length, 0, "no uncertain report");
    assert.ok((rec.lines as Array<{ msg?: string }>).some((l) => l.msg?.startsWith("a settled credential switch signal surfaced after its settlement")),
      "the settled-signal arm ran");
  });

  it("a stale claim during the escaped proof's hold report is a stale stop with the work retained, not a swallowed warning", TIMEOUT, async () => {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    const executor = {
      run: async (ctx: Ctx) => {
        api.requestCredentialSwitch(runId, GEN, "op-1");
        assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
        throw ctx.signal!.reason;
      },
    };
    // The proof is unprovable, so the hold report goes out; the api answers it with the stale_claim disposition.
    let held = 0;
    const realReport = client.reportState.bind(client);
    client.reportState = (async (id: string, body: Parameters<typeof realReport>[1], ...rest: unknown[]) => {
      if (body.status === "credential_switch_held") { held++; return { applied: false, staleClaim: true, status: "queued" }; }
      return (realReport as (...a: unknown[]) => unknown)(id, body, ...rest);
    }) as typeof client.reportState;
    const rec = recordingLogger();
    const { gitlab } = fakeGitlab();
    try {
      await runnerWith(() => ({ executor }), gitlab, undefined, rec.logger, {
        recoveryRetryMs: 1, credentialSwitchLedger: new CredentialSwitchOpLedger(fx.dataDir),
        quiesceRun: async (req) => (req.site === "in_place_credential_switch" ? BLOCKED : CLEAN),
      }).execute(claim);
    } finally { client.reportState = realReport as typeof client.reportState; }
    const msgs = (rec.lines as Array<{ msg?: string }>).map((l) => l.msg ?? "");
    const s: string[] = api.states.filter((x) => x.runId === runId).map((x) => x.body.status);
    assert.equal(held, 1, "the held report was answered stale once and not retried");
    assert.ok(msgs.some((m) => m.startsWith("run claim superseded server-side (stale_claim)")), `the stale stop arm ran: ${msgs.join(" | ")}`);
    assert.ok(!msgs.some((m) => m.includes("could not be settled")), "the stale claim was not swallowed as a settle warning");
    assert.deepEqual(s.filter((x) => ["completed", "failed", "credential_switch", "credential_switch_uncertain"].includes(x)), [], s.join(","));
    assert.equal(applied().length, 0);
    assert.equal(fs.existsSync(worktreeDirFor(claim.issue_iid as number)), true, "the source clone is retained");
  });
});

describe("issue #4201 m2d: a stale claim while the hold report retries", () => {
  it("stops stale with the work retained: no more retries, no apply, no completion", TIMEOUT, async () => {
    let held = 0;
    const realReport = client.reportState.bind(client);
    // The first report fails in transport; the retry finds the claim already superseded: the api answers the
    // stale_claim disposition the runner's reportState closure turns into a StaleClaimError.
    client.reportState = (async (runId: string, body: Parameters<typeof realReport>[1], ...rest: unknown[]) => {
      if (body.status === "credential_switch_held" && ++held === 1) throw new Error("transport down");
      if (body.status === "credential_switch_held" && held === 2) return { applied: false, staleClaim: true, status: "queued" };
      return (realReport as (...a: unknown[]) => unknown)(runId, body, ...rest);
    }) as typeof client.reportState;
    let r: Awaited<ReturnType<typeof scenario>>;
    try {
      r = await scenario({
        site: "implement_turn", apply: [{ outcome: "applied", token: NEW_TOKEN }],
        quiesce: (req) => (req.site === "in_place_credential_switch" ? BLOCKED : undefined),
      });
    } finally { client.reportState = realReport as typeof client.reportState; }
    assert.equal(held, 2, "the report was not retried past the stale answer");
    assert.equal(applied().length, 0);
    const s = statusesOf(r);
    assert.ok(!s.includes("completed") && !s.includes("failed") && !s.includes("credential_switch"), s.join(","));
    assert.equal(r.turns.length, 2, "no turn after the stale stop");
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "the source clone is retained");
  });
});

describe("issue #4201 m2d: the checkpoint authority now aborts with the switch's own CredentialSwitchSignal (v1 routing)", () => {
  it("a checkpoint stopped by a switch surfaces the switch, and the run still takes today's release path (never a failure)", TIMEOUT, async () => {
    // No in-place negotiation: this is today's v1 path. Its capture double verifies a clean tree.
    git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
    mockCommittedTracking(git, "cafef00dcafef00dcafef00dcafef00dcafef00d");
    git.verifyRunnerTrackingCovers = (async () => true) as typeof git.verifyRunnerTrackingCovers;
    git.trackingTip = (async () => "cafef00dcafef00dcafef00dcafef00dcafef00d") as typeof git.trackingTip;
    git.checkpointPack = (async () => null) as typeof git.checkpointPack;
    newApi();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    let checkpointError: unknown = "returned";
    const executor = {
      run: async (ctx: import("../src/executor.js").RunContext) => {
        api.overrideStateStatus(runId, "queued"); // the release requeues the run
        api.requestCredentialSwitch(runId, GEN, "op-1");
        assert.ok(await until(() => ctx.signal?.aborted === true, 10_000), "the switch aborted ctx.signal");
        try { await ctx.checkpoint!({ reap: true }); } catch (error) { checkpointError = error; }
        throw ctx.signal!.reason;
      },
    };
    const { gitlab } = fakeGitlab();
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    const s: string[] = api.states.filter((x) => x.runId === runId).map((x) => x.body.status);
    assert.ok(s.includes("credential_switch"), `released as today: ${s.join(",")}`);
    assert.ok(!s.includes("failed") && !s.includes("completed"), s.join(","));
    // Whatever the checkpoint did, it never reported the interruption as an AbortError of its own.
    assert.ok(checkpointError === "returned" || checkpointError instanceof Error, String(checkpointError));
    assert.ok(!(checkpointError instanceof Error && checkpointError.name === "AbortError" && !(checkpointError instanceof CredentialSwitchSignal)),
      `a checkpoint stopped by a switch must not surface a bare AbortError: ${String(checkpointError)}`);
  });
});
