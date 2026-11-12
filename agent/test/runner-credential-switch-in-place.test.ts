import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import type { OutgoingMessage } from "../src/protocol.js";
import os from "node:os";
import { MessageBatcher, MAX_MESSAGE_BYTES } from "../src/batcher.js";
import { RequestError, type CredentialSwitchApplyResult, type WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import { SecretSet } from "../src/redact.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { CredentialSwitchOpLedger, defaultLedgerFs } from "../src/credential-switch-op-ledger.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import type { ApplyScript } from "./fake-api.js";
import type { QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import {
  api, assistant, client, fakeGitlab, fx, git, homeDir, installHarness, resultOk, runnerWith, secretRecordingLogger, worktreeDirFor,
} from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";

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

interface TurnRecord { resume: string | undefined; token: string | undefined; env: unknown; cwd: string | undefined }

function scriptedQuery(turns: Array<(signal: AbortSignal) => AsyncIterable<SDKMessage>>, record: TurnRecord[]): SdkQueryFn {
  let i = 0;
  return (params) => {
    const script = turns[Math.min(i, turns.length - 1)]!;
    i++;
    const env = params.options.env as Record<string, string | undefined>;
    record.push({ resume: params.options.resume, token: env.CLAUDE_CODE_OAUTH_TOKEN, env, cwd: params.options.cwd });
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      yield* script(params.options.abortController!.signal);
    })();
  };
}

type Site = "plan_turn" | "plan_gate" | "plan_gate_revision" | "plan_gate_exhausted" | "ask_user" | "implement_turn" | "window";

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
  const claim = freshClaim(GEN);
  const runId = claim.run_id;
  setup.before?.(runId);
  const secretLog = secretRecordingLogger();
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
          else send(runId, row("answer", JSON.stringify({ question_id: b.open_question_id, answers: ["postgres"] })));
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
  const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn: scriptedQuery(turnScripts, turns), client });
  const attemptDir = path.dirname(worktreeDirFor(claim.issue_iid as number));
  const attempts = (): string[] =>
    fs.existsSync(attemptDir) ? fs.readdirSync(attemptDir).filter((n) => n.includes(".attempt-")) : [];
  const attemptsBefore = attempts();
  const { gitlab } = fakeGitlab();
  const proofOrder: string[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    const verdict = setup.quiesce?.(req);
    if (req.site === "in_place_credential_switch") { api.requestLog.push("quiesce:in_place_credential_switch"); proofOrder.push(req.site); }
    return verdict ?? CLEAN;
  };
  try {
    await runnerWith(() => ({ executor: sdk }), gitlab, undefined, secretLog.logger, {
      recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, credentialSwitchLedger: ledger, quiesceRun,
      switchSupersedeWaitMs: 2_000, questionTimeoutMs: 20_000, ...setup.extra,
    }).execute(claim);
  } finally {
    client.postMessages = postMessages;
  }
  const states = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  return { claim, runId, turns, posted, states, ledger, ledgerWrites, attempts, attemptsBefore, proofOrder, secretLog };
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
      // The ledger wrote the op ahead, settled it, never stored the token, and was cleaned at the terminal state.
      assert.ok(r.ledgerWrites.some((w) => w.includes('"sending"')) && r.ledgerWrites.some((w) => w.includes('"settled"')));
      assert.ok(r.ledgerWrites.every((w) => !w.includes(NEW_TOKEN)), "the token is never persisted");
      assert.equal(fs.existsSync(path.join(fx.dataDir, "credential-switch-ops", `${r.runId}.json`)), false, "cleaned up at the terminal state");
    });
  }
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

  it("the uncertain report is retried through lost replies, then confirmed", TIMEOUT, async () => {
    const r = await scenario({
      site: "implement_turn", apply: [{ outcome: "refused_committed" }], before: (id) => api.dropUncertainReplies(id, 2),
    });
    assert.equal(api.uncertainRequests.length, 3, "two lost replies, then the confirmed third");
    assert.ok(api.uncertainRequests.every((u) => u.body.op_id === "op-1"));
    assert.equal(r.turns.length, 2, "neither token ran again");
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

  it("uncertain then superseded with no newer op: the hold is released past the possible apply, the op stays ambiguous", TIMEOUT, async () => {
    const spy = spyHold();
    const apply = committedThenAnswers(spy, [AMBIGUOUS, { kind: "superseded" }]);
    try {
      const r = await scenario({ site: "implement_turn", apply: [], extra: { switchSupersedeWaitMs: 50 } });
      assert.equal(apply.calls(), 2);
      assertReleasedPastPossibleApply(r, spy);
      assert.equal((await r.ledger.stateOf(r.runId, "op-1"))?.state, "ambiguous");
    } finally { apply.restore(); spy.restore(); }
  });
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

describe("issue #4201 m2c rework: the report retries are bounded", () => {
  // IN_PLACE_REPORT_ATTEMPTS (runner.ts) is 5. Counted at the runner's reportState call (the client retries
  // a dropped connection by itself, so wire requests are a multiple of the runner's attempts).
  async function neverConfirmed(status: string, run: () => ReturnType<typeof scenario>) {
    const realReport = client.reportState.bind(client);
    let attempts = 0;
    client.reportState = (async (runId: string, body: Parameters<typeof realReport>[1], ...rest: unknown[]) => {
      if (body.status === status) { attempts++; throw new Error("transport down"); }
      return (realReport as (...a: unknown[]) => unknown)(runId, body, ...rest);
    }) as typeof client.reportState;
    try {
      const r = await run();
      return { r, attempts };
    } finally { client.reportState = realReport as typeof client.reportState; }
  }

  it("a hold report that never confirms is attempted exactly 5 times, then the flight stops with its work retained", TIMEOUT, async () => {
    const { r, attempts } = await neverConfirmed("credential_switch_held", () => scenario({
      site: "implement_turn", apply: [],
      quiesce: (req) => (req.site === "in_place_credential_switch" ? BLOCKED : undefined),
    }));
    assert.equal(attempts, 5);
    assert.equal(applied().length, 0);
    assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
    assert.equal(r.turns.length, 2, "no turn after the report");
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
  });

  it("an uncertain report that never confirms is attempted exactly 5 times, then the flight stops with its work retained", TIMEOUT, async () => {
    const { r, attempts } = await neverConfirmed("credential_switch_uncertain", () => scenario({
      site: "implement_turn", apply: [{ outcome: "refused_committed" }],
    }));
    assert.equal(attempts, 5);
    assert.ok(!statusesOf(r).includes("completed"), statusesOf(r).join(","));
    assert.equal(r.turns.length, 2, "neither token ran again");
    assert.equal(fs.existsSync(worktreeDirFor(r.claim.issue_iid as number)), true, "custody kept");
  });
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

describe("issue #4201 m2d: the signal escaped the executor (the escaped arm)", () => {
  /** A stub executor that never handles the switch: it waits for it and rethrows (as a test executor does). */
  async function escaped(quiesce: (req: QuiesceRunRequest) => QuiesceRunOutcome | undefined) {
    newApi();
    negotiate();
    const claim = freshClaim(GEN);
    const runId = claim.run_id;
    api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
    let ran = 0;
    const executor = {
      run: async (ctx: import("../src/executor.js").RunContext) => {
        ran++;
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
    return { runId, ran, states: api.states.filter((x) => x.runId === runId).map((x) => x.body) };
  }

  it("proven quiescent: credential_switch_uncertain with the op id; never a release, an apply or a requeue", TIMEOUT, async () => {
    const r = await escaped(() => undefined);
    const s: string[] = r.states.map((b) => b.status);
    assert.equal(api.uncertainRequests.length, 1, s.join(","));
    assert.equal(api.uncertainRequests[0]!.body.op_id, "op-1");
    assert.equal(applied().length, 0, "the executor has unwound: nothing is applied");
    for (const status of ["credential_switch", "queued", "completed", "failed"]) assert.ok(!s.includes(status), `no ${status}: ${s.join(",")}`);
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
