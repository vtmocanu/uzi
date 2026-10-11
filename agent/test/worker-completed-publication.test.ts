import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, before, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { RunRunner, StaleClaimError } from "../src/runner.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { RecoveryCoordinator, canonicalJson } from "../src/recovery.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import { resolvePendingTerminal } from "../src/terminal-resolve.js";
import type { GitCache } from "../src/git.js";
import type { StateRequest, StateAck, RunKind } from "../src/protocol.js";
import { nullLogger, makeClaim } from "./helpers.js";
import "./setup/hermetic-proc.js";

const wire = JSON.parse(readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const literal = wire.ack.completed_publication_receipt;
const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
const feature = "recovery_completed_publication_v1";

async function fixture(kind: RunKind = "issue", mode: "journal" | "none" | "reserve" = "journal", guarded = true) {
  await fs.mkdir(scratch, { recursive: true });
  const root = await fs.mkdtemp(path.join(scratch, "completion-test-"));
  const events: string[] = [];
  const sent: StateRequest[] = [];
  let loseResponse = false, refuse = false, enabled = true, released = false;
  let stateReplies: Array<"decode" | "lost" | "stale"> = [];
  let workerId = literal.worker_id;
  let retirement: { ownership: unknown; custody: unknown } | undefined;
  const branch = kind === "self_improve" ? "uzi/self-improve/" + literal.run_id : literal.branch;
  const receipt = { ...literal, branch };
  const claim = makeClaim({ run_id: receipt.run_id, claim_generation: 1, inventory_guarded: guarded,
    kind, issue_iid: 7, branch,
    repo: { ...makeClaim().repo, id: literal.repo_id, forge_type: "gitlab" } });
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith("/register")) return Response.json({ worker_id: workerId,
        protocol_features: ["claim_generation_fence", "recovery_inventory_v1", "terminal_rejection_report", ...(enabled ? [feature] : [])] });
      if (url.pathname.endsWith("/claim")) return Response.json(claim);
      if (url.pathname.endsWith("/ownership")) return Response.json(retirement?.ownership ?? {});
      if (url.pathname.endsWith("/terminal-rejection-custody")) {
        events.push("custody");
        return Response.json(retirement?.custody ?? {});
      }
      if (url.pathname.endsWith("/recovery-holds")) return Response.json({ run_id: receipt.run_id,
        holds: released || !guarded ? [] : [{ hold_id: receipt.hold_id, generation: receipt.generation, inventory_guarded: true, has_available_capture: false }] });
      if (url.pathname.endsWith("/state")) {
        const body = JSON.parse(String(init?.body)) as StateRequest;
        events.push("state:" + body.status);
        sent.push(body);
        const reply = stateReplies.shift();
        if (reply === "decode") return Response.json({ error: "invalid request body" }, { status: 400 });
        if (reply === "lost") throw new Error("lost ACK");
        if (reply === "stale") return Response.json({ disposition: "stale_claim" }, { status: 409 });
        if (body.status === "completed" && !refuse) released = true;
        if (loseResponse) throw new Error("response lost after API release");
        return Response.json({ run: { ...wire.ack.run, id: receipt.run_id, worker_id: workerId },
          ...(!refuse && enabled ? { completed_publication_receipt: receipt } : { completed_publication_reason: "mr_missing" }) });
      }
      if (url.pathname.includes("recovery-captures")) {
        events.push("capture-refused");
        return Response.json({ error: "hold closed", reason: "not_authorized" }, { status: 409 });
      }
      throw new Error("unexpected RPC " + url.pathname);
  });
  const client = new WorkerClient("http://completion.test", "completion-worker-fixture", "test", nullLogger(), {
    sleep: async () => {}, terminalRetrySchedule: [1],
  });
  await client.register("fixture");
  await client.claimRun();
  await client.listRecoveryHolds(receipt.run_id);
  const git = { recoveryRoot: path.join(root, "recovery"), pinRecoverySource: async () => {} } as unknown as GitCache;
  const recovery = new RecoveryCoordinator({ recoveryRoot: git.recoveryRoot, git, client,
    workerToken: "completion-worker-fixture", log: nullLogger() });
  const record = await recovery.pin({ runId: receipt.run_id, generation: 1, sourceSha: "b".repeat(40),
    inventoryGuarded: guarded, branch, kind });
  assert.ok(record);
  const rawWrite: RawWriteSeam | undefined = mode === "reserve" ? async (write, ctx) => {
    if (ctx.kind === "terminal") throw Object.assign(new Error("full"), { code: "ENOSPC" });
    await write();
  } : undefined;
  const outbox = mode === "none" ? undefined : new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
    runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86400000, rawWrite });
  await outbox?.init();
  const run = new RunRunner(client, git, () => { throw new Error("not executing"); }, nullLogger(), 1,
    "completion-worker-fixture", { recovery, outbox });
  const body: StateRequest = { ...wire.request, branch };
  const flight = {
    runId: receipt.run_id, claimGeneration: 1, inventoryGuarded: guarded, runKind: kind,
    terminalResolved: false, completionSendAttempted: false, successfulPushedSha: literal.final_head,
    batcher: { currentSeq: () => 7, close: async () => {}, awaitPermanentFailureSettled: async () => {}, emit: () => events.push("failed-feed") },
    runLog: nullLogger(), redactText: (text: string) => text, executor: {}, cancel: new AbortController(),
    reportState: async (b: StateRequest) => send(b),
    prepareTerminalInventory: async () => {
      events.push("prepare");
      flight.successfulPushedSha = "c".repeat(40);
      if (released) {
        try { await client.reserveRecoveryCapture(receipt.run_id, { generation: 1 } as never); }
        catch { events.push("closed-retained"); }
      }
    },
  };
  const send = async (b: StateRequest): Promise<StateAck> => {
    const ack = await client.reportState(receipt.run_id, b);
    await run.observeSettlementTerminalAck(receipt.run_id, receipt.generation, b, ack);
    if (ack.staleClaim) throw new StaleClaimError();
    return ack;
  };
  const terminal = (beforeResolve?: () => Promise<void>) => (run as unknown as {
    journalAndSendTerminal(f: unknown, phase: string, b: StateRequest, sender: (b: StateRequest) => Promise<StateAck>, beforeResolve?: () => Promise<void>): Promise<void>;
  }).journalAndSendTerminal(flight, "running", body, send, beforeResolve);
  const replay = () => resolvePendingTerminal(run.protectRecoveryTerminalDeps({
    outbox: outbox!, client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger(),
  }), { runId: receipt.run_id, claimGeneration: receipt.generation, send });
  return { root, events, sent, receipt, recovery, record, run, flight, body, terminal, replay, outbox, client,
    replies: (...values: typeof stateReplies) => { stateReplies = values; },
    retirement: (ownership: unknown, custody: unknown) => { retirement = { ownership, custody }; },
    freshClient: async () => {
      const fresh = new WorkerClient("http://completion.test", "completion-worker-fixture", "test", nullLogger(),
        { sleep: async () => {}, terminalRetrySchedule: [1] });
      await fresh.register("fresh");
      return fresh;
    },
    freshRunner: (c: WorkerClient = client) => new RunRunner(c, git,
      () => { throw new Error("not executing"); }, nullLogger(), 1, "completion-worker-fixture", { recovery, outbox }),
    registerAs: async (id: string) => {
      workerId = id; receipt.worker_id = id;
      await client.register("identity-change");
    },
    select: (runId: string, generation: number) => {
      receipt.run_id = runId; receipt.generation = generation;
      claim.run_id = runId; claim.claim_generation = generation;
      flight.runId = runId; flight.claimGeneration = generation;
      body.claim_generation = generation;
      released = false;
    },
    ineligible: (value: boolean) => { claim.kind = value ? "chat" : kind; },
    lose: (value: boolean) => { loseResponse = value; }, refuse: () => { refuse = true; },
    disable: async () => { enabled = false; await client.register("fixture"); },
    close: async () => { mock.restoreAll(); await fs.rm(root, { recursive: true, force: true }); } };
}

type CompletionFixture = Awaited<ReturnType<typeof fixture>>;

function barrier() {
  let release!: () => void;
  const wait = new Promise<void>(resolve => { release = resolve; });
  return { wait, release };
}

async function assertOverlapRetained(f: CompletionFixture) {
  assert.equal(f.flight.completionSendAttempted, true);
  assertAttempted(f.run, f);
  await unit2Failure(f);
  assert.equal(f.sent.every(b => b.status === "completed"), true, "no competing failed report");
  assert.equal(await f.run.recoveryInventoryPending(f.receipt.run_id, 1), true);
  assert.equal((await f.client.listRecoveryHolds(f.receipt.run_id)).holds
    .some(h => h.generation === 1 && h.inventory_guarded === true), true, "current hold retained");
  assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
}

for (const mode of ["none", "reserve"] as const) {
  for (const firstToSend of [0, 1]) {
    it("Unit 2: " + mode + " concurrent preparation preserves ambiguity, send order " + firstToSend, async () => {
      const f = await fixture("issue", mode);
      const entered = [barrier(), barrier()];
      const gates = [barrier(), barrier()];
      const attempts: Promise<void>[] = [];
      try {
        for (let i = 0; i < 2; i++) {
          attempts.push(assert.rejects(f.terminal(async () => {
            entered[i]!.release();
            await gates[i]!.wait;
          })));
          await entered[i]!.wait;
        }
        f.replies("lost", "lost");
        gates[firstToSend]!.release();
        await attempts[firstToSend];
        f.replies("decode", "decode");
        gates[1 - firstToSend]!.release();
        await attempts[1 - firstToSend];
        await assertOverlapRetained(f);
      } finally {
        gates.forEach(g => g.release());
        await Promise.allSettled(attempts);
        await f.close();
      }
    });
  }

  for (const refusalFirst of [false, true]) {
    it("Unit 2: " + mode + " actual sends overlap, refusal settles first " + refusalFirst, async t => {
      const f = await fixture("issue", mode);
      const entered = [barrier(), barrier()];
      const gates = [barrier(), barrier()];
      const rawReport = f.client.reportState.bind(f.client);
      let calls = 0;
      const attempts: Promise<void>[] = [];
      try {
        t.mock.method(f.client, "reportState", async (...args: Parameters<WorkerClient["reportState"]>) => {
          const i = calls++;
          entered[i]!.release();
          await gates[i]!.wait;
          return rawReport(...args);
        });
        attempts.push(assert.rejects(f.terminal()));
        await entered[0]!.wait;
        attempts.push(assert.rejects(f.terminal()));
        await entered[1]!.wait;
        const first = refusalFirst ? 0 : 1;
        f.replies(...(refusalFirst ? ["decode", "decode"] as const : ["lost", "lost"] as const));
        gates[first]!.release();
        await attempts[first];
        assert.equal(f.flight.completionSendAttempted, true, "other unresolved send keeps marker");
        f.replies(...(refusalFirst ? ["lost", "lost"] as const : ["decode", "decode"] as const));
        gates[1 - first]!.release();
        await attempts[1 - first];
        await assertOverlapRetained(f);
      } finally {
        gates.forEach(g => g.release());
        await Promise.allSettled(attempts);
        await f.close();
      }
    });
  }
}

it("Unit 2: reserve absence await cannot clear another send's marker", async t => {
  const f = await fixture("issue", "reserve");
  const entered = barrier(), gate = barrier();
  const attempts: Promise<void>[] = [];
  try {
    t.mock.method(f.outbox!, "confirmTerminalAbsent", async () => {
      entered.release();
      await gate.wait;
      return true;
    });
    f.replies("decode", "decode");
    attempts.push(assert.rejects(f.terminal()));
    await entered.wait;
    assert.equal(f.client.hasFeature(feature), false, "decode refusal clears client features before overlap");
    f.replies("lost", "lost");
    attempts.push(assert.rejects(f.terminal()));
    await attempts[1];
    gate.release();
    await attempts[0];
    await assertOverlapRetained(f);
  } finally {
    gate.release();
    await Promise.allSettled(attempts);
    await f.close();
  }
});

async function unit2Failure(f: CompletionFixture) {
  await (f.run as unknown as { reportGenericFailure(c: unknown, f: unknown, e: unknown, o: unknown): Promise<void> })
    .reportGenericFailure(makeClaim(), f.flight, new Error("completion refused"), { keepCustody: true });
}

for (const mode of ["none", "reserve"] as const) {
  it("Unit 2: " + mode + " fresh decode refusal permits ordinary failed", async () => {
    const f = await fixture("issue", mode);
    try {
      f.replies("decode", "decode");
      await assert.rejects(f.terminal());
      assert.equal(f.flight.completionSendAttempted, false);
      assert.equal((Reflect.get(f.run, "attemptedPublicationTerminals") as Map<string, object>).size, 0);
      await unit2Failure(f);
      assert.equal(f.sent.at(-1)?.status, "failed");
      assert.equal(f.sent.at(-1)?.completion_final_head, undefined);
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
    } finally { await f.close(); }
  });

  for (const replies of [["decode", "lost", "lost"], ["lost", "decode", "decode"]] as const) {
    it("Unit 2: " + mode + " " + replies.join("->") + " suppresses competing failed", async () => {
      const f = await fixture("issue", mode);
      try {
        f.replies(...replies);
        await assert.rejects(f.terminal());
        assert.equal(f.flight.completionSendAttempted, true);
        await unit2Failure(f);
        assert.equal(f.sent.every(b => b.status === "completed"), true);
      } finally { await f.close(); }
    });
  }

  it("Unit 2: " + mode + " prior ambiguous attempt survives later decode refusal", async () => {
    const f = await fixture("issue", mode);
    try {
      f.replies("lost", "lost");
      await assert.rejects(f.terminal());
      f.replies("decode", "decode");
      await assert.rejects(f.terminal());
      assert.equal(f.flight.completionSendAttempted, true);
      await unit2Failure(f);
      assert.equal(f.sent.every(b => b.status === "completed"), true);
    } finally { await f.close(); }
  });

  it("Unit 2: " + mode + " fallback exception cannot conceal definitive nonapplication", async t => {
    const f = await fixture("issue", mode);
    try {
      const fallback = t.mock.method(f.flight, "prepareTerminalInventory", async () => { throw new Error("fallback failed"); });
      f.replies("decode", "decode");
      await assert.rejects(f.terminal(), { name: "StateReportUnappliedError" });
      assert.equal(f.flight.completionSendAttempted, false);
      assert.equal((Reflect.get(f.run, "attemptedPublicationTerminals") as Map<string, object>).size, 0);
      fallback.mock.restore();
      await unit2Failure(f);
      assert.equal(f.sent.at(-1)?.status, "failed");
    } finally { await f.close(); }
  });

  it("Unit 2: " + mode + " retained runner marker prevents a fresh flight clearing prior ambiguity", async () => {
    const f = await fixture("issue", mode);
    try {
      f.replies("lost", "lost");
      await assert.rejects(f.terminal());
      f.flight.completionSendAttempted = false;
      f.replies("decode", "decode");
      await assert.rejects(f.terminal());
      assert.equal(f.flight.completionSendAttempted, true);
      assertAttempted(f.run, f);
      await unit2Failure(f);
      assert.equal(f.sent.every(b => b.status === "completed"), true);
    } finally { await f.close(); }
  });

  it("Unit 2: " + mode + " stale fence takes precedence before publication", async () => {
    const f = await fixture("issue", mode);
    try {
      Reflect.set(f.flight, "steering", { claimFence: () => "released" });
      f.replies("decode", "decode");
      await assert.rejects(f.terminal(), { name: "StaleClaimError" });
      assert.equal(f.sent.length, 0);
      assert.equal(f.flight.completionSendAttempted, false);
    } finally { await f.close(); }
  });

  it("Unit 2: " + mode + " stale claim never sends competing failed", async () => {
    const f = await fixture("issue", mode);
    try {
      f.replies("stale");
      if (mode === "none") {
        await assert.rejects(f.terminal(), StaleClaimError);
        assert.equal(f.flight.terminalResolved, false, "direct stale throw skips resolution latch");
      } else {
        await f.terminal();
        assert.equal(f.flight.terminalResolved, true, "reserve wrapper normalizes stale throw");
      }
      assert.equal(f.flight.completionSendAttempted, true);
      await unit2Failure(f);
      assert.equal(f.sent.length, 1);
      assert.equal(f.sent[0]?.status, "completed");
    } finally { await f.close(); }
  });
}

it("Unit 2: reserve unavailable absence proof retains original attempt", async t => {
  const f = await fixture("issue", "reserve");
  try {
    t.mock.method(f.outbox!, "confirmTerminalAbsent", async () => false);
    f.replies("decode", "decode");
    await assert.rejects(f.terminal());
    assert.equal(f.flight.completionSendAttempted, true);
    await unit2Failure(f);
    assert.equal(f.sent.every(b => b.status === "completed"), true);
  } finally { await f.close(); }
});

it("Unit 2: journaled decode refusal preserves exact original for replay", async () => {
  const f = await fixture();
  try {
    f.replies("decode", "decode");
    await f.terminal();
    const original = await f.outbox!.readTerminalJournal(f.receipt.run_id, 1);
    assert.deepEqual(original?.body, f.body);
    await unit2Failure(f);
    assert.equal(f.sent.every(b => b.status === "completed"), true);
    f.replies("decode", "decode");
    await f.replay();
    assert.equal(canonicalJson(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1)), canonicalJson(original));
  } finally { await f.close(); }
});

function discardAuthority(f: CompletionFixture, state = "discarded") {
  return {
    run_id: f.receipt.run_id, worker_id: f.receipt.worker_id, generation: 1,
    outcome: "settled", complete: true, exact_complete: true, sibling_complete: true,
    exact_count: 1, sibling_count: 0,
    exact_holds: [{ id: f.receipt.hold_id, state }], sibling_holds: [],
  };
}

function terminalOwnership(inventoryGuarded = true) {
  return { status: "completed", claim_generation: 1, inventory_guarded: inventoryGuarded };
}

function discardWorker(f: CompletionFixture, run: RunRunner, client = f.client) {
  return new Worker({ gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as Config,
    client, run, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
    () => ({ ok: true, missing: [] }), f.outbox, new Map()) as unknown as {
      resolveBootTerminals(signal: AbortSignal): Promise<void>;
      sweepPendingTerminals(signal: AbortSignal): Promise<void>;
      sweepPendingFinalizes(signal: AbortSignal): Promise<void>;
    };
}

async function discardInventory(f: CompletionFixture, inventory: "pending" | "absent") {
  if (inventory === "absent")
    await fs.rm(path.join(f.root, "recovery", f.receipt.run_id), { recursive: true, force: true });
}

function assertAttempted(run: RunRunner, f: CompletionFixture) {
  assert.equal((Reflect.get(run, "attemptedPublicationTerminals") as Map<string, object>)
    .has(f.receipt.run_id + ":1"), true, "replay restores attempted publication marker");
}

for (const inventory of ["pending", "absent"] as const) {
  for (const actor of ["runner", "Worker boot"] as const) {
    it("discard regression: " + actor + " retires terminal and finalize with " + inventory + " inventory", async () => {
      const f = await fixture();
      try {
        f.refuse();
        await discardInventory(f, inventory);
        assert.equal((await f.outbox!.journalFinalize(f.receipt.run_id, 1)).written, true);
        if (actor === "runner") await f.terminal();
        else await f.outbox!.journalTerminal(f.receipt.run_id, 1, "running", 7, { ...f.body });
        f.retirement(terminalOwnership(), discardAuthority(f, "released"));
        const run = actor === "runner" ? f.run : f.freshRunner();
        const worker = discardWorker(f, run);
        if (actor === "Worker boot") await worker.resolveBootTerminals(new AbortController().signal);
        assertAttempted(run, f);
        assert.equal(await run.recoveryInventoryPending(f.receipt.run_id, 1), true);
        assert.ok(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1));
        await worker.sweepPendingFinalizes(new AbortController().signal);
        assert.equal(f.outbox!.listPendingFinalizeGenerations(true).length, 1,
          "finalize journal installed before attempted completion remains before discard");

        f.retirement(terminalOwnership(), discardAuthority(f));
        assert.equal(await run.recoveryInventoryPending(f.receipt.run_id, 1), false);
        if (actor === "runner") {
          await f.replay();
          await (run as unknown as {
            retireFinalizeRecord(flight: unknown, site: string): Promise<void>;
          }).retireFinalizeRecord(f.flight, "discard regression");
        } else {
          await worker.sweepPendingTerminals(new AbortController().signal);
          await worker.sweepPendingFinalizes(new AbortController().signal);
        }
        assert.equal(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1), undefined);
        assert.equal(f.outbox!.listPendingFinalizeGenerations(true).length, 0);
        assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
        assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(f.receipt.run_id + ":1"), false,
          "Unit 1: actual discard retirement detaches nonreceipt provenance");
      } finally { await f.close(); }
    });
  }

  for (const authority of ["released", "missing", "unknown", "wrong generation", "wrong worker", "incomplete", "legacy"] as const) {
    it("discard regression: attempted completion retains " + authority + " authority with " + inventory + " inventory", async () => {
      const f = await fixture();
      try {
        f.refuse();
        await discardInventory(f, inventory);
        assert.equal((await f.outbox!.journalFinalize(f.receipt.run_id, 1)).written, true);
        await f.outbox!.journalTerminal(f.receipt.run_id, 1, "running", 7, { ...f.body });
        // In particular, legacy absence must have no process-local guarded claim history.
        const client = await f.freshClient();
        const run = f.freshRunner(client);
        assert.equal(client.knowsInventoryGuardedClaim(f.receipt.run_id, 1), false);
        let custody: unknown = discardAuthority(f);
        if (authority === "released") custody = discardAuthority(f, "released");
        if (authority === "missing") custody = {};
        if (authority === "unknown") custody = { ...discardAuthority(f), outcome: "unknown",
          exact_count: 0, exact_holds: [] };
        if (authority === "wrong generation") custody = { ...discardAuthority(f), generation: 2 };
        if (authority === "wrong worker") custody = { ...discardAuthority(f), worker_id: literal.owner_id };
        if (authority === "incomplete") custody = { ...discardAuthority(f), complete: false,
          exact_complete: false, exact_count: 257,
          exact_holds: Array.from({ length: 256 }, (_, n) => ({
            id: n.toString(16).padStart(8, "0") + "-aaaa-4aaa-8aaa-aaaaaaaaaaaa", state: "discarded",
          })) };
        if (authority === "legacy") custody = {};
        f.retirement(terminalOwnership(authority !== "legacy"), custody);
        if (authority === "legacy" && inventory === "absent")
          assert.equal(await client.hasRecoveryRetirementAuthority(f.receipt.run_id, 1, "absent"), true,
            "exact explicit legacy ownership authorizes ordinary absence");
        const worker = discardWorker(f, run, client);
        await worker.resolveBootTerminals(new AbortController().signal);
        assertAttempted(run, f);
        assert.equal(await run.recoveryInventoryPending(f.receipt.run_id, 1), true);
        await worker.sweepPendingFinalizes(new AbortController().signal);
        assert.ok(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1));
        assert.equal(f.outbox!.listPendingFinalizeGenerations(true).length, 1);
      } finally { await f.close(); }
    });
  }
}

for (const kind of ["issue", "mr_rework", "self_improve"] as const) {
  it(kind + " exact receipt precedes retirement with zero preparation/capture/heartbeat", async () => {
    const f = await fixture(kind);
    try {
      const retire = f.outbox!.retireTerminal.bind(f.outbox);
      f.outbox!.retireTerminal = async (...args) => {
        assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), true,
          "authenticated receipt must persist before terminal retirement");
        f.events.push("retire");
        return retire(...args);
      };
      await f.terminal();
      assert.deepEqual(f.events, ["state:completed", "retire"]);
      assert.equal(f.flight.terminalResolved, true);
      assert.equal(f.sent[0]!.completion_final_head, literal.final_head);
      const record = (await f.recovery.inspect(f.receipt.run_id))[0]!;
      assert.deepEqual(record.completionReceipt, f.receipt);
      assert.equal(record.finalAcknowledged, undefined);
      assert.equal(record.coverageDigest, undefined);
      assert.notEqual(record.sourceSha, record.completionReceipt?.final_head);
    } finally { await f.close(); }
  });
}

it("receipt persistence failure retains installed journal and source without preparation or failed", async t => {
  const f = await fixture();
  try {
    t.mock.method(f.recovery, "persistCompletionReceipt", async () => false);
    await f.terminal();
    assert.deepEqual(f.events, ["state:completed"]);
    assert.ok(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1));
    assert.equal((await f.recovery.inspect(f.receipt.run_id)).length, 1);
    assert.equal(f.flight.terminalResolved, true);
  } finally { await f.close(); }
});

it("lost ACK keeps original WA body through closed capture refusal and replay persists before retire without predecessor", async () => {
  const f = await fixture();
  try {
    f.lose(true);
    await f.terminal();
    assert.ok(f.events.includes("prepare"));
    assert.ok(f.events.includes("closed-retained"));
    assert.equal(f.sent.every(b => b.status === "completed"), true);
    const original = await f.outbox!.readTerminalJournal(f.receipt.run_id, 1);
    assert.ok(original);
    assert.equal(original.body.completion_final_head, literal.final_head);
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
    assert.equal((await f.recovery.inspect(f.receipt.run_id)).length, 1);
    f.lose(false);
    const retire = f.outbox!.retireTerminal.bind(f.outbox);
    f.outbox!.retireTerminal = async (...args) => {
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), true);
      return retire(...args);
    };
    await f.replay();
    assert.equal(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1), undefined);
    assert.equal(f.sent.at(-1)?.completion_final_head, literal.final_head);
    assert.equal((await f.recovery.inspect(f.receipt.run_id))[0]?.completionReceipt?.final_head, literal.final_head);
  } finally { await f.close(); }
});

for (const mode of ["none", "reserve"] as const) {
  it(mode + " unknown completion send suppresses generic failed without inventing a journal or authority", async () => {
    const f = await fixture("issue", mode);
    try {
      f.lose(true);
      await assert.rejects(f.terminal());
      assert.equal(f.flight.completionSendAttempted, true);
      assert.equal(f.flight.terminalResolved, false);
      assert.equal(f.outbox?.hasPendingTerminal(f.receipt.run_id, 1) ?? false, false);
      await (f.run as unknown as { reportGenericFailure(c: unknown, f: unknown, e: unknown, o: unknown): Promise<void> })
        .reportGenericFailure(makeClaim(), f.flight, new Error("lost"), { keepCustody: true });
      assert.equal(f.sent.every(b => b.status === "completed"), true);
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
      assert.ok(f.events.includes("closed-retained"));
    } finally { await f.close(); }
  });
}

for (const kind of ["issue", "self_improve"] as const) {
  it(kind + " receipt refusal runs unchanged fallback and preserves delivered head", async () => {
    const f = await fixture(kind);
    try {
      f.refuse();
      await f.terminal();
      assert.ok(f.events.includes("prepare"));
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
      assert.equal((await f.outbox!.readTerminalJournal(f.receipt.run_id, 1))?.body.completion_final_head, literal.final_head);
    } finally { await f.close(); }
  });
}

it("feature loss replays outbound copy without rewriting original stored body", async () => {
  const f = await fixture();
  try {
    f.lose(true);
    await f.terminal();
    const original = await f.outbox!.readTerminalJournal(f.receipt.run_id, 1);
    f.lose(false);
    await f.disable();
    await f.replay();
    assert.equal(f.sent.at(-1)?.completion_final_head, undefined);
    assert.equal(canonicalJson(await f.outbox!.readTerminalJournal(f.receipt.run_id, 1)), canonicalJson(original));
  } finally { await f.close(); }
});

it("canonical failed winner keeps preparation before send despite competing completion", async () => {
  const f = await fixture();
  try {
    await f.outbox!.journalTerminal(f.receipt.run_id, 1, "running", 7, { status: "failed", claim_generation: 1 });
    await f.terminal();
    assert.deepEqual(f.events.slice(0, 2), ["prepare", "state:failed"]);
    assert.equal(f.flight.completionSendAttempted, false);
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), false);
  } finally { await f.close(); }
});

it("canonical completion winner defers preparation despite competing failed caller", async () => {
  const f = await fixture();
  try {
    await f.outbox!.journalTerminal(f.receipt.run_id, 1, "running", 7, { ...wire.request });
    await (f.run as unknown as {
      journalAndSendTerminal(f: unknown, phase: string, b: StateRequest, send: (b: StateRequest) => Promise<StateAck>): Promise<void>;
    }).journalAndSendTerminal(f.flight, "running", { status: "failed" }, async b => {
      const ack = await f.client.reportState(f.receipt.run_id, b);
      await f.run.observeSettlementTerminalAck(f.receipt.run_id, 1, b, ack);
      return ack;
    });
    assert.deepEqual(f.events, ["state:completed"]);
    assert.equal(f.flight.completionSendAttempted, true);
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(f.receipt.run_id, 1), true);
  } finally { await f.close(); }
});

it("no-outbox lost completion blocks retirement on empty server hold listing", async () => {
  const f = await fixture("issue", "none");
  try {
    f.lose(true);
    await assert.rejects(f.terminal());
    assert.equal(await f.run.recoveryInventoryPending(f.receipt.run_id, 1), true,
      "server release without an authenticated receipt cannot authorize retirement");
  } finally { await f.close(); }
});

it("closed hold fallback exception retains selected completion and original final head", async t => {
  const f = await fixture();
  try {
    f.lose(true);
    t.mock.method(f.flight, "prepareTerminalInventory", async () => { throw new Error("closed hold"); });
    await f.terminal();
    assert.equal(f.flight.terminalResolved, true);
    assert.equal((await f.outbox!.readTerminalJournal(f.receipt.run_id, 1))?.body.completion_final_head, literal.final_head);
    assert.equal(f.sent.every(b => b.status === "completed"), true);
  } finally { await f.close(); }
});

it("persistence failure blocks finalize retirement as well as terminal retirement", async t => {
  const f = await fixture();
  try {
    t.mock.method(f.recovery, "persistCompletionReceipt", async () => false);
    let retired = false;
    t.mock.method(f.outbox!, "retireFinalize", async () => { retired = true; return true; });
    await f.terminal();
    await (f.run as unknown as { retireFinalizeRecord(f: unknown, site: string): Promise<void> })
      .retireFinalizeRecord(f.flight, "test");
    assert.equal(retired, false);
    assert.equal(await f.run.recoveryInventoryPending(f.receipt.run_id, 1), true);
  } finally { await f.close(); }
});

it("missing key and missing exact generation cannot silently persist or create receipt records", async () => {
  const f = await fixture();
  try {
    const disabled = new RecoveryCoordinator({ recoveryRoot: f.root, client: f.client, git: {} as GitCache, log: nullLogger() });
    assert.equal(await disabled.persistCompletionReceipt(f.receipt), false);
    assert.equal(await f.recovery.persistCompletionReceipt({ ...f.receipt, generation: 2 }), false);
    assert.equal((await f.recovery.inspect(f.receipt.run_id)).length, 1);
    assert.equal((await f.recovery.inspect(f.receipt.run_id))[0]?.completionReceipt, undefined);
  } finally { await f.close(); }
});


describe("P1-a/c: 4097 authenticated completing generations reclaim bindings through real protected retirement", () => {
  let f: Awaited<ReturnType<typeof fixture>>;
  before(async () => { f = await fixture(); });
  after(async () => { await f.close(); });
  // Serial chunks retain one client/runner and bound each test well below the timeout.
  for (let chunk = 0; chunk < 5; chunk++) it("successful history chunk " + chunk, async () => {
    // Remove only this fixture's proved record between iterations to keep disk work linear.
    for (let n = chunk * 1024 + 1; n <= Math.min((chunk + 1) * 1024, 4097); n++) {
      const id = n.toString(16).padStart(8, "0") + "-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
      f.select(id, n);
      await f.client.claimRun();
      await f.client.listRecoveryHolds(id);
      assert.ok(await f.recovery.pin({ runId: id, generation: n, sourceSha: "b".repeat(40),
        inventoryGuarded: true, branch: literal.branch, kind: "issue" }));
      await f.terminal();
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(id, n), true, "persisted receipt " + n);
      assert.equal(f.outbox!.hasPendingTerminal(id, n), false, "real protected retirement " + n);
      await fs.rm(path.join(f.root, "recovery", id), { recursive: true, force: true });
      if (n === 4096) await f.client.register("same-worker-after-history");
    }
    assert.equal(f.events.includes("prepare"), false, "successful history never forces fallback");
    assert.equal(f.sent.some(body => body.status === "failed"), false);
  });
});

it("Unit 1: eligible unguarded failed generation detaches only after runner retirement", async () => {
  const f = await fixture("issue", "journal", false);
  try {
    await discardInventory(f, "absent");
    f.retirement({ ...terminalOwnership(false), status: "failed" }, discardAuthority(f));
    assert.equal((await f.client.reportState(literal.run_id, f.body)).completedPublicationReceipt, undefined,
      "unguarded provenance still refuses a completion receipt");
    await f.outbox!.journalTerminal(literal.run_id, 1, "running", 7, { status: "failed" });
    const claims = Reflect.get(f.client, "completionClaims") as Map<string, unknown>;
    assert.equal(claims.has(literal.run_id + ":1"), true);
    const reports = f.run.protectRecoveryTerminalDeps({ outbox: f.outbox!, client: f.client,
      gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() }).outbox;
    assert.equal(await reports.retireTerminal(literal.run_id, 1), true);
    assert.equal(await f.outbox!.confirmReportsAbsent(literal.run_id, 1), true);
    assert.equal(claims.has(literal.run_id + ":1"), false);
    assert.equal((Reflect.get(f.client, "completionHolds") as Map<string, unknown>).has(literal.run_id + ":1"), false);
  } finally { await f.close(); }
});

it("Unit 1: publication refusal reclaims only after acknowledged archive fallback retires the original", async t => {
  const f = await fixture();
  let archived = false;
  try {
    f.refuse();
    t.mock.method(f.recovery, "inventoryCleanupState", async () => archived ? "acknowledged" as const : "pending" as const);
    // Model the existing authenticated archive FINAL ACK; completion is sent first.
    t.mock.method(f.flight, "prepareTerminalInventory", async () => { archived = true; });
    await f.terminal();
    const original = await f.outbox!.readTerminalJournal(literal.run_id, 1);
    assert.ok(original, "refused publication keeps its original until archive coverage is acknowledged");
    assert.equal(original.body.completion_final_head, literal.final_head);
    assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), true);
    assert.equal(archived, true);
    await f.replay();
    assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, 1), false);
    assert.deepEqual(f.sent.at(-1), { ...original.body, claim_generation: 1 });
    assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), false);
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 1), false);
  } finally { await f.close(); }
});

describe("Unit 1: proven nonreceipt history preserves completion admission", () => {
  let f: CompletionFixture;
  before(async () => { f = await fixture(); });
  after(async () => { await f.close(); });
  for (let chunk = 0; chunk < 3; chunk++) it("nonreceipt history chunk " + chunk, async t => {
    // Model the existing acknowledged FINAL coverage seam; real reports, deletion and client admission.
    t.mock.method(f.recovery, "inventoryCleanupState", async () => "acknowledged" as const);
    const terminal = f.run as unknown as {
      journalAndSendTerminal(flight: unknown, phase: string, body: StateRequest,
        send: (body: StateRequest) => Promise<StateAck>): Promise<void>;
    };
    for (let n = chunk * 1024 + 1; n <= Math.min((chunk + 1) * 1024, 2050); n++) {
      f.select(literal.run_id, n);
      await f.client.claimRun();
      await f.client.listRecoveryHolds(literal.run_id);
      const status = (["failed", "cancelled", "completed"] as const)[n % 3]!;
      await terminal.journalAndSendTerminal(f.flight, "running", {
        status: status === "cancelled" ? "failed" : status, claim_generation: n,
      }, async () => ({ applied: status !== "cancelled", status }));
      assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, n), false, "real nonreceipt deletion");
      assert.equal(f.client.canStartPublicationCompletion(literal.run_id, n + 1), true,
        "Unit 1: retired history must free admission");
    }
    if (chunk === 2) {
      f.select(literal.run_id, 2051);
      // The prior ordinary terminals prepared inventory and changed the pushed SHA.
      // Model a fresh publication flight with its actual successful push and unsent terminal.
      f.flight.successfulPushedSha = literal.final_head;
      f.flight.terminalResolved = false;
      f.flight.completionSendAttempted = false;
      await f.client.claimRun();
      await f.client.listRecoveryHolds(literal.run_id);
      assert.ok(await f.recovery.pin({ runId: literal.run_id, generation: 2051, sourceSha: "b".repeat(40),
        inventoryGuarded: true, branch: literal.branch, kind: "issue" }));
      await f.terminal();
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 2051), true,
        "fresh admitted completion still validates and persists its receipt");
    }
  });
});

for (const actor of ["terminal", "runner finalize", "worker finalize"] as const) {
  for (const registration of ["same", "changed", "A-B-A"] as const) {
    it("Unit 1: " + actor + " retirement fences " + registration + " registration", async t => {
      const f = await fixture();
      let finish!: () => void;
      try {
        let entered!: () => void;
        const started = new Promise<void>(resolve => { entered = resolve; });
        const pending = new Promise<void>(resolve => { finish = resolve; });
        t.mock.method(f.run, "recoveryInventoryPending", async () => { entered(); await pending; return false; });
        f.retirement(terminalOwnership(), discardAuthority(f));
        const deps = f.run.protectRecoveryTerminalDeps({
          outbox: f.outbox!, client: f.client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger(),
        });
        if (actor === "terminal") await f.outbox!.journalTerminal(literal.run_id, 1, "running", 7, { status: "failed" });
        else await f.outbox!.journalFinalize(literal.run_id, 1);
        const operation = actor === "terminal" ? deps.outbox.retireTerminal(literal.run_id, 1) :
          actor === "runner finalize" ? (f.run as unknown as {
            retireFinalizeRecord(flight: unknown, site: string): Promise<void>;
          }).retireFinalizeRecord(f.flight, "Unit 1") :
          discardWorker(f, f.run).sweepPendingFinalizes(new AbortController().signal);
        await started;
        await f.registerAs(registration === "same" ? literal.worker_id : literal.owner_id);
        if (registration === "A-B-A") await f.registerAs(literal.worker_id);
        await f.client.claimRun();
        await f.client.listRecoveryHolds(literal.run_id);
        finish();
        await operation;
        assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, 1), false);
        assert.equal(f.outbox!.listPendingFinalizeGenerations(true).length, 0);
        assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"),
          registration !== "same", "only unchanged incarnation can detach");
      } finally { finish?.(); await f.close(); }
    });
  }
}

it("Unit 1: receipt persistence failure during report absence retains provenance", async t => {
  const f = await fixture();
  try {
    f.retirement(terminalOwnership(), discardAuthority(f));
    await discardInventory(f, "absent");
    await f.outbox!.journalTerminal(literal.run_id, 1, "running", 7, { status: "failed" });
    t.mock.method(f.recovery, "persistCompletionReceipt", async () => false);
    const confirm = f.outbox!.confirmReportsAbsent.bind(f.outbox!);
    t.mock.method(f.outbox!, "confirmReportsAbsent", async (runId: string, generation: number) => {
      const absent = await confirm(runId, generation);
      assert.equal(absent, true);
      const ack = await f.client.reportState(runId, f.body);
      assert.ok(ack.completedPublicationReceipt, "real receipt admission during the absence check");
      await f.run.observeSettlementTerminalAck(runId, generation, f.body, ack);
      return absent;
    });
    assert.equal(await f.run.protectRecoveryTerminalDeps({ outbox: f.outbox!, client: f.client,
      gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() }).outbox.retireTerminal(literal.run_id, 1), true);
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 1), false);
    assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), true);
    assert.equal(await f.run.recoveryInventoryPending(literal.run_id, 1), true);
  } finally { await f.close(); }
});

for (const protection of ["finalize", "unlink", "unknown", "void finalize", "absent finalize"] as const) {
  it("Unit 1: report notification retains provenance after " + protection, async t => {
    const f = await fixture();
    try {
      t.mock.method(f.run, "recoveryInventoryPending", async () => false);
      const provider = f.run as unknown as { retireFinalizeRecord(flight: unknown, site: string): Promise<void> };
      if (protection === "void finalize" || protection === "absent finalize") {
        if (protection === "void finalize") {
          await f.outbox!.journalFinalize(literal.run_id, 1);
          t.mock.method(f.outbox!, "retireFinalize", async () => undefined as never);
        }
        await provider.retireFinalizeRecord(f.flight, "Unit 1");
      } else {
        await f.outbox!.journalTerminal(literal.run_id, 1, "running", 7, { status: "failed" });
        if (protection === "finalize") await f.outbox!.journalFinalize(literal.run_id, 1);
        if (protection === "unknown") t.mock.method(f.outbox!, "confirmReportsAbsent", async () => false);
        if (protection === "unlink") t.mock.method(f.outbox!, "retireTerminal", async () => false);
        await f.run.protectRecoveryTerminalDeps({ outbox: f.outbox!, client: f.client,
          gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger() }).outbox.retireTerminal(literal.run_id, 1);
        if (protection === "finalize") {
          assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), true);
          await provider.retireFinalizeRecord(f.flight, "Unit 1");
          assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), false);
          return;
        }
      }
      assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), true);
    } finally { await f.close(); }
  });
}

it("Unit 1: lost ACK original remains selected until actual proven discard retirement", async () => {
  const f = await fixture();
  try {
    f.refuse();
    f.lose(true);
    await f.outbox!.journalFinalize(literal.run_id, 1);
    await f.terminal();
    const original = await f.outbox!.readTerminalJournal(literal.run_id, 1);
    assert.ok(original);
    f.retirement(terminalOwnership(), discardAuthority(f, "released"));
    await f.replay();
    assert.deepEqual((await f.outbox!.readTerminalJournal(literal.run_id, 1))?.body, original.body);
    assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), true);
    f.retirement(terminalOwnership(), discardAuthority(f));
    f.lose(false);
    await f.replay();
    await (f.run as unknown as { retireFinalizeRecord(flight: unknown, site: string): Promise<void> })
      .retireFinalizeRecord(f.flight, "Unit 1");
    assert.deepEqual(f.sent.at(-1), { ...original.body, claim_generation: 1 });
    assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), false);
  } finally { await f.close(); }
});

for (const mode of ["none", "reserve"] as const) {
  for (const applied of [true, false]) {
    it("Unit 1: " + mode + " nonreceipt terminal requires applied ACK: " + applied, async t => {
      const f = await fixture("issue", mode);
      try {
        t.mock.method(f.run, "recoveryInventoryPending", async () => false);
        await (f.run as unknown as {
          journalAndSendTerminal(flight: unknown, phase: string, body: StateRequest,
            send: () => Promise<StateAck>): Promise<void>;
        }).journalAndSendTerminal(f.flight, "running", { status: "failed" },
          async () => ({ applied, status: "failed" }));
        assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), !applied);
      } finally { await f.close(); }
    });
  }
}

async function pressure(f: Awaited<ReturnType<typeof fixture>>) {
  // Claim-only observations reserve the missing hold slot. The final unseen attempt taints.
  for (let generation = 2; generation <= 4097; generation++) {
    f.select(literal.run_id, generation);
    await f.client.claimRun();
  }
  f.select(literal.run_id, 1);
}

it("P1-d: lost ACK original replays at capacity, persists and retires exact body/head", async () => {
  const f = await fixture();
  try {
    f.lose(true);
    await f.terminal();
    const original = await f.outbox!.readTerminalJournal(literal.run_id, 1);
    assert.ok(original);
    await pressure(f);
    await f.client.register("same-worker-under-pressure");
    f.lose(false);
    await f.replay();
    assert.deepEqual(f.sent.at(-1), { ...original.body, claim_generation: 1 }, "exact original replay");
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 1), true,
      "original receipt remains accepted under pressure");
    assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, 1), false, "protected original retired");
    assert.equal(f.sent.some(body => body.status === "failed"), false);
  } finally { await f.close(); }
});

for (const mode of ["journal", "none", "reserve"] as const) {
it("MR2598 regression 3: " + mode + " separate new attempt at capacity prepares and omits completion disposition", async () => {
  const f = await fixture("issue", mode);
  try {
    await pressure(f);
    f.select(literal.run_id, 4098);
    f.body.head = literal.final_head; // Keep the exact completion-permit head through fallback.
    const originalBody = structuredClone(f.body);
    await f.terminal();
    assert.deepEqual(f.body, originalBody, "refused admission leaves the caller body immutable");
    assert.ok(f.events.indexOf("prepare") >= 0);
    assert.ok(f.events.indexOf("prepare") < f.events.indexOf("state:completed"),
      "refused new completion-first attempt prepares before sending");
    assert.ok(f.sent[0]);
    assert.equal("completion_final_head" in f.sent[0], false,
      "new attempt refused admission sends ordinary completion without disposition");
    const ordinary = { ...originalBody };
    delete ordinary.completion_final_head;
    assert.deepEqual(f.sent[0], ordinary, "ordinary completion retains delivered head and all other fields");
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 4098), false);
    assert.equal(f.sent.some(body => body.status === "failed"), false);
  } finally { await f.close(); }
});
}

for (const mode of ["none", "reserve"] as const) {
  it(mode + " attempted completion stays exact after lost ACK and later saturation", async () => {
    const f = await fixture("issue", mode);
    try {
      f.lose(true);
      await assert.rejects(f.terminal());
      const attempted = structuredClone(f.sent[0]);
      assert.ok(attempted, "lost ACK follows an actual terminal send");
      assert.equal(attempted.completion_final_head, literal.final_head);
      await pressure(f);
      f.lose(false);
      await f.terminal();
      assert.deepEqual(f.sent.at(-1), attempted, "already attempted body never degrades to ordinary completion");
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 1), true);
    } finally { await f.close(); }
  });
}

for (const status of ["completed", "failed"] as const) {
  it(status + " ordering: original durable at first preparation and first provider reap", async t => {
    const f = await fixture();
    try {
      if (status === "completed") {
        for (let slot = 0; slot < 4096; slot++)
          f.client.canStartPublicationCompletion("00000000-0000-4000-8000-" + String(slot).padStart(12, "0"), 1);
        f.select(literal.run_id, 4098);
        assert.equal(f.client.canStartPublicationCompletion(f.receipt.run_id, 4098), false);
      }
      let preparations = 0, reaps = 0;
      const assertOriginal = async (stage: string) => {
        const journal = await f.outbox!.readTerminalJournal(f.receipt.run_id, f.flight.claimGeneration);
        assert.ok(journal, "original not journaled before " + stage);
        assert.equal(journal.body.status, status);
        if (status === "completed") assert.equal("completion_final_head" in journal.body, false,
          "refused new attempt journals ordinary completion before preparation");
      };
      const provider = f.run as unknown as {
        reapRecoveryProviderForSettle: (...args: unknown[]) => Promise<boolean>;
      };
      t.mock.method(provider, "reapRecoveryProviderForSettle", async () => {
        if (reaps++ === 0) await assertOriginal("first provider reap");
        return true;
      });
      t.mock.method(f.flight, "prepareTerminalInventory", async () => {
        if (preparations++ === 0) await assertOriginal("first preparation");
        await provider.reapRecoveryProviderForSettle(makeClaim(), f.flight, nullLogger(), "terminal");
        f.events.push("prepare");
      });
      await (f.run as unknown as {
        journalAndSendTerminal(f: unknown, phase: string, b: StateRequest, send: (b: StateRequest) => Promise<StateAck>, hook?: () => Promise<void>): Promise<void>;
      }).journalAndSendTerminal(f.flight, "running",
        status === "completed" ? { ...wire.request, claim_generation: 4098 } : { status: "failed", failure_reason: "ordinary failure" },
        async b => { f.events.push("send"); return f.client.reportState(f.receipt.run_id, b); },
        async () => {
          f.flight.cancel.abort();
          await provider.reapRecoveryProviderForSettle(makeClaim(), f.flight, nullLogger(), "terminal");
        });
      assert.ok(preparations > 0);
      assert.ok(reaps > 0);
      assert.ok(f.events.indexOf("prepare") < f.events.indexOf("send"));
    } finally { await f.close(); }
  });
}

it("P1-b: delayed pre-retirement inventory cannot recreate a released generation", async t => {
  const f = await fixture();
  try {
    let complete!: (value: Response) => void;
    const pending = new Promise<Response>(resolve => { complete = resolve; });
    const originalFetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      if (new URL(String(input)).pathname.endsWith("/recovery-holds")) return pending;
      return originalFetch(input, init);
    });
    const inventory = f.client.listRecoveryHolds(literal.run_id);
    await f.terminal();
    complete(Response.json({ run_id: literal.run_id, holds: [{
      hold_id: literal.owner_id, generation: 1, inventory_guarded: true,
    }] }));
    await inventory;
    const ack = await f.client.reportState(literal.run_id, wire.request);
    assert.deepEqual(ack.completedPublicationReceipt, f.receipt, "drained old inventory does not poison exact replay");
    assert.equal(f.client.canStartPublicationCompletion(literal.run_id, 2), true, "unrelated generation remains admissible");
    assert.equal((Reflect.get(f.client, "completionHolds") as Map<string, unknown>).has(literal.run_id + ":1"), false,
      "old response must not recreate the retired binding");
  } finally { await f.close(); }
});

it("P1-b: unlink failure keeps pending journal and exact client conflict protection", async t => {
  const f = await fixture();
  try {
    const originalUnlink = fs.unlink;
    t.mock.method(fs, "unlink", async (...args: Parameters<typeof fs.unlink>) => {
      if (String(args[0]).includes("terminal")) throw Object.assign(new Error("unlink refused"), { code: "EIO" });
      return originalUnlink(...args);
    });
    await f.terminal();
    assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, 1), true, "failed unlink retains pending entry");
    assert.ok((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"));
  } finally { await f.close(); }
});

for (const mode of ["journal", "none", "reserve"] as const) {
  it("P1-d: " + mode + " admission stays stable through pressure before first send", async () => {
    const f = await fixture("issue", mode);
    try {
      await f.terminal(async () => { await pressure(f); });
      assert.equal(f.events.includes("prepare"), false, "admitted completion-first choice stays stable");
      assert.equal(f.sent[0]?.completion_final_head, literal.final_head);
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 1), true);
      assert.equal(f.sent.some(body => body.status === "failed"), false);
    } finally { await f.close(); }
  });
}

for (const returnToA of [false, true]) {
  it("P1-b: delayed receipt persistence cannot release bindings after " + (returnToA ? "A-B-A" : "A-B"), async t => {
    const f = await fixture();
    let finish!: () => void;
    try {
      const persist = f.recovery.persistCompletionReceipt.bind(f.recovery);
      const pending = new Promise<void>(resolve => { finish = resolve; });
      let entered!: () => void;
      const started = new Promise<void>(resolve => { entered = resolve; });
      t.mock.method(f.recovery, "persistCompletionReceipt", async (receipt: Parameters<RecoveryCoordinator["persistCompletionReceipt"]>[0]) => {
        entered(); await pending; return persist(receipt);
      });
      const terminal = f.terminal();
      await started;
      await f.registerAs(literal.owner_id);
      if (returnToA) await f.registerAs(literal.worker_id);
      f.receipt.hold_id = literal.owner_id;
      f.select(literal.run_id, 1);
      await f.client.claimRun();
      const inventory = await f.client.listRecoveryHolds(literal.run_id);
      assert.equal(inventory.holds[0]?.hold_id, literal.owner_id, "conflicting new-incarnation hold was observed");
      finish();
      await terminal;
      f.receipt.hold_id = literal.hold_id;
      assert.equal((await f.client.reportState(literal.run_id, wire.request)).completedPublicationReceipt, undefined,
        "new-incarnation foreign-hold binding survives old persisted retirement proof");
    } finally { finish?.(); await f.close(); }
  });
}

it("guarded new completion after ineligible history skips preparation", async () => {
  const f = await fixture();
  try {
    f.select(literal.run_id, 2);
    f.ineligible(true);
    await f.client.claimRun();
    f.select(literal.run_id, 3);
    f.ineligible(false);
    await f.client.claimRun();
    await f.client.listRecoveryHolds(literal.run_id);
    assert.ok(await f.recovery.pin({ runId: literal.run_id, generation: 3,
      sourceSha: "b".repeat(40), inventoryGuarded: true, branch: literal.branch, kind: "issue" }));
    await f.terminal();
    assert.equal(f.events.includes("prepare"), false);
    assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 3), true);
    assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, 3), false);
  } finally { await f.close(); }
});

for (const handler of ["resolveBootTerminals", "resolveRunTerminal", "sweepPendingTerminals"] as const) {
  it("Worker " + handler + " replays lost ACK original after pressure with persisted receipt and retirement", async () => {
    const f = await fixture();
    try {
      f.lose(true);
      await f.terminal();
      const original = await f.outbox!.readTerminalJournal(literal.run_id, 1);
      assert.ok(original);
      await pressure(f);
      f.lose(false);
      const w = new Worker({ gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as Config,
        f.client, f.run, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
        () => ({ ok: true, missing: [] }), f.outbox, new Map()) as unknown as {
          resolveBootTerminals(signal: AbortSignal): Promise<void>;
          resolveRunTerminal(id: string, signal: AbortSignal): Promise<void>;
          sweepPendingTerminals(signal: AbortSignal): Promise<void>;
        };
      const signal = new AbortController().signal;
      if (handler === "resolveRunTerminal") await w.resolveRunTerminal(literal.run_id, signal);
      else await w[handler](signal);
      assert.deepEqual(f.sent.at(-1), { ...original.body, claim_generation: 1 });
      assert.equal(await f.recovery.hasPersistedCompletionReceipt(literal.run_id, 1), true);
      assert.equal(f.outbox!.hasPendingTerminal(literal.run_id, 1), false);
      assert.equal(f.sent.some(body => body.status === "failed"), false);
    } finally { await f.close(); }
  });
}

it("retiring different journal head cannot reclaim old persisted receipt bindings", async () => {
  const f = await fixture();
  try {
    await f.terminal();
    await f.client.claimRun();
    await f.client.listRecoveryHolds(literal.run_id);
    await f.outbox!.journalTerminal(literal.run_id, 1, "running", 7,
      { ...wire.request, completion_final_head: "c".repeat(40) });
    const deps = f.run.protectRecoveryTerminalDeps({
      outbox: f.outbox!, client: f.client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger(),
    });
    assert.equal(await deps.outbox.retireTerminal(literal.run_id, 1), true);
    assert.equal((Reflect.get(f.client, "completionClaims") as Map<string, unknown>).has(literal.run_id + ":1"), true);
  } finally { await f.close(); }
});
