import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { after, before, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
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

async function fixture(kind: RunKind = "issue", mode: "journal" | "none" | "reserve" = "journal") {
  const root = await fs.mkdtemp(path.join(scratch, "completion-test-"));
  const events: string[] = [];
  const sent: StateRequest[] = [];
  let loseResponse = false, refuse = false, enabled = true, released = false;
  let workerId = literal.worker_id;
  const branch = kind === "self_improve" ? "uzi/self-improve/" + literal.run_id : literal.branch;
  const receipt = { ...literal, branch };
  const claim = makeClaim({ run_id: receipt.run_id, claim_generation: 1, inventory_guarded: true,
    kind, issue_iid: 7, branch,
    repo: { ...makeClaim().repo, id: literal.repo_id, forge_type: "gitlab" } });
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith("/register")) return Response.json({ worker_id: workerId,
        protocol_features: ["claim_generation_fence", "recovery_inventory_v1", ...(enabled ? [feature] : [])] });
      if (url.pathname.endsWith("/claim")) return Response.json(claim);
      if (url.pathname.endsWith("/recovery-holds")) return Response.json({ run_id: receipt.run_id,
        holds: released ? [] : [{ hold_id: receipt.hold_id, generation: receipt.generation, inventory_guarded: true, has_available_capture: false }] });
      if (url.pathname.endsWith("/state")) {
        const body = JSON.parse(String(init?.body)) as StateRequest;
        events.push("state:" + body.status);
        sent.push(body);
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
    inventoryGuarded: true, branch, kind });
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
    runId: receipt.run_id, claimGeneration: 1, inventoryGuarded: true, runKind: kind,
    terminalResolved: false, completionSendAttempted: false, successfulPushedSha: literal.final_head,
    batcher: { currentSeq: () => 7, close: async () => {}, awaitPermanentFailureSettled: async () => {}, emit: () => events.push("failed-feed") },
    runLog: nullLogger(), redactText: (text: string) => text, executor: {}, cancel: new AbortController(),
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
    return ack;
  };
  const terminal = (beforeResolve?: () => Promise<void>) => (run as unknown as {
    journalAndSendTerminal(f: unknown, phase: string, b: StateRequest, sender: (b: StateRequest) => Promise<StateAck>, beforeResolve?: () => Promise<void>): Promise<void>;
  }).journalAndSendTerminal(flight, "running", body, send, beforeResolve);
  const replay = () => resolvePendingTerminal(run.protectRecoveryTerminalDeps({
    outbox: outbox!, client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger(),
  }), { runId: receipt.run_id, claimGeneration: receipt.generation, send });
  return { root, events, sent, receipt, recovery, record, run, flight, terminal, replay, outbox, client,
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
    t.mock.method(f.outbox!, "retireFinalize", async () => { retired = true; });
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

it("P1-d: separate new attempt at capacity prepares before terminal send", async () => {
  const f = await fixture();
  try {
    await pressure(f);
    f.select(literal.run_id, 4098);
    await f.terminal();
    assert.ok(f.events.indexOf("prepare") >= 0);
    assert.ok(f.events.indexOf("prepare") < f.events.indexOf("state:completed"),
      "refused new completion-first attempt prepares before sending");
    assert.equal(f.sent[0]?.completion_final_head, literal.final_head);
    assert.equal(f.sent.some(body => body.status === "failed"), false);
  } finally { await f.close(); }
});

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
        if (status === "completed") assert.equal(journal.body.completion_final_head, literal.final_head);
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
    assert.equal(ack.completedPublicationReceipt, undefined, "skipped old inventory taints unknown provenance");
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
