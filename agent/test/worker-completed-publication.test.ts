import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
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
  const branch = kind === "self_improve" ? "uzi/self-improve/" + literal.run_id : literal.branch;
  const receipt = { ...literal, branch };
  const claim = makeClaim({ run_id: receipt.run_id, claim_generation: 1, inventory_guarded: true,
    kind, issue_iid: 7, branch,
    repo: { ...makeClaim().repo, id: literal.repo_id, forge_type: "gitlab" } });
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname.endsWith("/register")) return Response.json({ worker_id: literal.worker_id,
        protocol_features: ["claim_generation_fence", "recovery_inventory_v1", ...(enabled ? [feature] : [])] });
      if (url.pathname.endsWith("/claim")) return Response.json(claim);
      if (url.pathname.endsWith("/recovery-holds")) return Response.json({ run_id: receipt.run_id,
        holds: released ? [] : [{ hold_id: receipt.hold_id, generation: 1, inventory_guarded: true, has_available_capture: false }] });
      if (url.pathname.endsWith("/state")) {
        const body = JSON.parse(String(init?.body)) as StateRequest;
        events.push("state:" + body.status);
        sent.push(body);
        if (body.status === "completed" && !refuse) released = true;
        if (loseResponse) throw new Error("response lost after API release");
        return Response.json({ run: { ...wire.ack.run },
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
    await run.observeSettlementTerminalAck(receipt.run_id, 1, b, ack);
    return ack;
  };
  const terminal = () => (run as unknown as {
    journalAndSendTerminal(f: unknown, phase: string, b: StateRequest, sender: (b: StateRequest) => Promise<StateAck>): Promise<void>;
  }).journalAndSendTerminal(flight, "running", body, send);
  const replay = () => resolvePendingTerminal(run.protectRecoveryTerminalDeps({
    outbox: outbox!, client, gapFillMax: 100, terminalMaxBytes: 1 << 20, log: nullLogger(),
  }), { runId: receipt.run_id, claimGeneration: 1, send });
  return { root, events, sent, receipt, recovery, record, run, flight, terminal, replay, outbox, client,
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
