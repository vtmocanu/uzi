import { afterEach, beforeEach, describe, it } from "node:test";
import { createHmac } from "node:crypto";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import type { AddressInfo } from "node:net";

import { RequestError, WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { RecoveryCoordinator, canonicalJson } from "../src/recovery.js";
import {
  PredecessorSettler,
  SETTLE_BACKOFF_BASE_MS,
  SETTLE_MAX_ATTEMPTS,
  SettlementJournal,
  settleBackoffMs,
  type LiveSettleLeg,
  type RecoverySettleClient,
  type SettlementCleanup,
  type SettlementLiveGit,
  type SettlementRecord,
} from "../src/recovery-settlement.js";
import type { RecoveryLiveSettleRequest, RecoverySettleRequest, RecoverySettleResponse } from "../src/protocol.js";
import { FakeRecoveryClient, FakeRecoveryGit } from "./codex-reap-fixture.js";
import { nullLogger } from "./helpers.js";

// issue #1582 M2 — the worker-side ancestry-settlement journal + settle driver, unit level.

const RUN = "11111111-2222-3333-4444-555555555555";
const HOLD = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const HOLD2 = "ffffffff-bbbb-cccc-dddd-eeeeeeeeeeee";
const TOKEN = "settle-unit-worker-join-token-0123456789";
const SRC = "1".repeat(40);
const ADOPTED = "2".repeat(40);
const PUSHED = "3".repeat(40);
const NOW = 1_700_000_000_000;

const roots: string[] = [];
function tmp(prefix: string): string {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  roots.push(d);
  return d;
}
afterEach(() => {
  for (const r of roots.splice(0)) fs.rmSync(r, { recursive: true, force: true });
});

function journal(root: string, now: () => number = () => NOW, token: string | null = TOKEN): SettlementJournal {
  return new SettlementJournal({ root, workerToken: token ?? undefined, log: nullLogger(), now });
}

function record(over: Partial<SettlementRecord> = {}): SettlementRecord {
  return {
    version: 1,
    runId: RUN,
    holdId: HOLD,
    predecessorGeneration: 1,
    successorGeneration: 2,
    sourceSha: SRC,
    sourceCaptureId: "cap-1",
    adoptedSha: ADOPTED,
    seededFrom: "tracking",
    branch: "agent/issue-7",
    barePath: "/data/repos/x.git",
    createdAt: NOW,
    state: "pending_settle",
    pushedSha: PUSHED,
    disposition: "publication",
    attempts: 0,
    ...over,
  };
}

class FakeSettleClient implements RecoverySettleClient {
  calls: Array<{ runId: string; holdId: string; req: RecoverySettleRequest }> = [];
  answers: Array<RecoverySettleResponse | Error> = [];
  async settleRecoveryHold(runId: string, holdId: string, req: RecoverySettleRequest): Promise<RecoverySettleResponse> {
    this.calls.push({ runId, holdId, req });
    const a = this.answers.shift() ?? { run_id: runId, hold_id: holdId, outcome: "released" };
    if (a instanceof Error) throw a;
    return a;
  }
}

class RecordingCleanup implements SettlementCleanup {
  calls: string[] = [];
  async deleteSettlementRefs(_bare: string, runId: string, holdId: string): Promise<void> {
    this.calls.push(`refs:${runId}/${holdId}`);
  }
  async deleteRecoveryPin(_bare: string, runId: string, gen: number): Promise<void> {
    this.calls.push(`pin:${runId}/${gen}`);
  }
  async forgetGeneration(runId: string, gen: number): Promise<void> {
    this.calls.push(`journal:${runId}/${gen}`);
  }
}

function retained(reason: string, holdId = HOLD): RecoverySettleResponse {
  return { run_id: RUN, hold_id: holdId, outcome: "retained", reason };
}

describe("SettlementJournal (issue #1582 M2)", () => {
  it("round-trips an authenticated record with 0600 file / 0700 dirs", async () => {
    const root = path.join(tmp("uzi-settle-j-"), "recovery-settlement");
    const j = journal(root);
    assert.equal(await j.put(record()), true);
    assert.deepEqual(await j.listRun(RUN), [record()]);
    assert.deepEqual(await j.listAll(), [record()]);
    const file = path.join(root, RUN, `${HOLD}.json`);
    assert.equal(fs.statSync(file).mode & 0o777, 0o600);
    assert.equal(fs.statSync(path.join(root, RUN)).mode & 0o777, 0o700);
    assert.equal(fs.statSync(root).mode & 0o777, 0o700);
  });

  it("a tampered field is REFUSED (MAC mismatch); an unparseable file is refused", async () => {
    const root = tmp("uzi-settle-tamper-");
    const j = journal(root);
    await j.put(record());
    await j.put(record({ holdId: HOLD2 }));
    const file = path.join(root, RUN, `${HOLD}.json`);
    const obj = JSON.parse(fs.readFileSync(file, "utf8"));
    obj.pushedSha = "4".repeat(40); // redirect the settle candidate
    fs.writeFileSync(file, JSON.stringify(obj));
    fs.writeFileSync(path.join(root, RUN, "garbage.json"), "{not json");
    const recs = await j.listRun(RUN);
    assert.deepEqual(
      recs.map((r) => r.holdId),
      [HOLD2],
      "only the untouched sibling survives; tampered + unparseable records are refused",
    );
  });

  it("a valid record MOVED under another hold's name is refused (the MAC binds content, the path binds identity)", async () => {
    const root = tmp("uzi-settle-move-");
    const j = journal(root);
    await j.put(record());
    fs.renameSync(path.join(root, RUN, `${HOLD}.json`), path.join(root, RUN, `${HOLD2}.json`));
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("a different worker token cannot read (or forge) the journal; token-less is disabled", async () => {
    const root = tmp("uzi-settle-key-");
    await journal(root).put(record());
    assert.deepEqual(await journal(root, () => NOW, "another-worker-token-000000000").listRun(RUN), []);
    const off = journal(root, () => NOW, null);
    assert.equal(off.enabled, false);
    assert.equal(await off.put(record({ holdId: HOLD2 })), false);
    assert.deepEqual(await off.listAll(), []);
  });

  it("refuses ids that are not a single safe path component", async () => {
    const root = tmp("uzi-settle-ids-");
    const j = journal(root);
    assert.equal(await j.put(record({ runId: "../escape" })), false);
    assert.equal(await j.put(record({ holdId: "a/b" })), false);
    assert.deepEqual(fs.readdirSync(root), []);
  });

  it("the serialized record carries NO credential (asserted on the file bytes)", async () => {
    const root = tmp("uzi-settle-nocred-");
    await journal(root).put(record());
    const bytes = fs.readFileSync(path.join(root, RUN, `${HOLD}.json`), "utf8");
    assert.doesNotMatch(bytes, /forge_pat|token|password|secret|oauth/i);
    assert.ok(!bytes.includes(TOKEN), "the worker token is never stored (only a derived MAC)");
  });

  it("survives the successor's exact-generation release AND removal of recovery/<runId> (a sibling root)", async () => {
    const dataDir = tmp("uzi-settle-sibling-");
    const gc = new GitCache(dataDir, nullLogger());
    assert.equal(gc.recoverySettlementRoot, path.join(dataDir, "recovery-settlement"));
    assert.ok(!gc.recoverySettlementRoot.startsWith(gc.recoveryRoot + path.sep), "never inside recovery/");
    const coord = new RecoveryCoordinator({
      client: new FakeRecoveryClient(),
      git: new FakeRecoveryGit(),
      log: nullLogger(),
      recoveryRoot: gc.recoveryRoot,
      workerToken: TOKEN,
      now: () => NOW,
    });
    const j = journal(gc.recoverySettlementRoot);
    await j.put(record());
    await coord.pin({ runId: RUN, sourceSha: PUSHED, kind: "issue", branch: "agent/issue-7", generation: 2 });
    assert.ok(fs.existsSync(path.join(gc.recoveryRoot, RUN)));
    await coord.release(RUN, 2, "publication"); // the successor's own release: drops recovery/<RUN>
    assert.ok(!fs.existsSync(path.join(gc.recoveryRoot, RUN)), "recovery/<runId> removed (sole generation)");
    fs.rmSync(path.join(gc.recoveryRoot), { recursive: true, force: true });
    await coord.resumePending(); // the recovery restart sweep never sees the settlement root
    assert.deepEqual(await j.listRun(RUN), [record()], "the settlement record survives");
  });
});

describe("PredecessorSettler outcome rules (issue #1582 M2)", () => {
  let root: string;
  let clock: number;
  let j: SettlementJournal;
  let client: FakeSettleClient;
  let cleanup: RecordingCleanup;
  let settler: PredecessorSettler;

  beforeEach(() => {
    root = tmp("uzi-settler-");
    clock = NOW;
    j = journal(root, () => clock);
    client = new FakeSettleClient();
    cleanup = new RecordingCleanup();
    settler = new PredecessorSettler({ journal: j, client, cleanup, log: nullLogger() });
  });

  it("sends the exact strict body and, on released, cleans up ONLY that hold (sibling untouched)", async () => {
    await j.put(record());
    await j.put(record({ holdId: HOLD2, predecessorGeneration: 3, successorGeneration: 4 }));
    client.answers = [{ run_id: RUN, hold_id: HOLD, outcome: "released", final_head_sha: PUSHED }];
    assert.equal(await settler.settleOne((await j.listRun(RUN)).find((r) => r.holdId === HOLD)!), "released");
    assert.deepEqual(client.calls, [
      {
        runId: RUN,
        holdId: HOLD,
        req: {
          predecessor_generation: 1,
          successor_generation: 2,
          pushed_sha: PUSHED,
          source_sha: SRC,
          adopted_sha: ADOPTED,
        },
      },
    ]);
    assert.deepEqual(Object.keys(client.calls[0]!.req).sort(), [
      "adopted_sha",
      "predecessor_generation",
      "pushed_sha",
      "source_sha",
      "successor_generation",
    ]);
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual((await j.listRun(RUN)).map((r) => r.holdId), [HOLD2], "the sibling record is untouched");
  });

  it("the released cleanup removes the journal record LAST: a failed cleanup step keeps it, and a re-send redoes the cleanup", async () => {
    const file = path.join(root, RUN, `${HOLD}.json`);
    // Each step records whether the settlement record still existed when it ran; the first
    // deleteSettlementRefs fails (e.g. a git error), which must leave the record for a re-send.
    class OrderedCleanup extends RecordingCleanup {
      failRefs = 1;
      override async deleteSettlementRefs(bare: string, runId: string, holdId: string): Promise<void> {
        await super.deleteSettlementRefs(bare, runId, holdId);
        this.calls.push(`record-present:${fs.existsSync(file)}`);
        if (this.failRefs-- > 0) throw new Error("git: cannot lock ref");
      }
      override async deleteRecoveryPin(bare: string, runId: string, gen: number): Promise<void> {
        await super.deleteRecoveryPin(bare, runId, gen);
        this.calls.push(`record-present:${fs.existsSync(file)}`);
      }
      override async forgetGeneration(runId: string, gen: number): Promise<void> {
        await super.forgetGeneration(runId, gen);
        this.calls.push(`record-present:${fs.existsSync(file)}`);
      }
    }
    const ordered = new OrderedCleanup();
    const s2 = new PredecessorSettler({ journal: j, client, cleanup: ordered, log: nullLogger() });
    await j.put(record());
    assert.equal(await s2.settleOne((await j.listRun(RUN))[0]!), "skipped", "the failed cleanup is a local failure");
    const kept = (await j.listRun(RUN))[0];
    assert.ok(kept, "the journal record survives a failed cleanup step");
    assert.deepEqual([kept.state, kept.attempts], ["pending_settle", 0], "unchanged, so it is re-sent");
    assert.equal(await s2.settleOne(kept), "released");
    assert.equal(client.calls.length, 2, "the identical settle was re-sent");
    assert.deepEqual(client.calls[1]!.req, client.calls[0]!.req);
    assert.deepEqual(ordered.calls, [
      `refs:${RUN}/${HOLD}`,
      "record-present:true",
      `refs:${RUN}/${HOLD}`,
      "record-present:true",
      `pin:${RUN}/1`,
      "record-present:true",
      `journal:${RUN}/1`,
      "record-present:true",
    ]);
    assert.deepEqual(await j.listRun(RUN), [], "removed last, once every cleanup step succeeded");
  });

  for (const reason of ["ancestry_unknown", "state_changed"]) {
    it(`retained/${reason} → retry with backoff; journal + pins intact`, async () => {
      await j.put(record());
      client.answers = [retained(reason)];
      assert.equal(await settler.settleOne(record()), "retry");
      const [r] = await j.listRun(RUN);
      assert.equal(r!.state, "pending_settle");
      assert.equal(r!.attempts, 1);
      assert.equal(r!.lastReason, reason);
      assert.equal(r!.nextAttemptAt, NOW + SETTLE_BACKOFF_BASE_MS);
      assert.deepEqual(cleanup.calls, [], "nothing cleaned up on a retained answer");
    });
  }

  for (const reason of ["not_ancestor", "not_eligible", "candidate_mismatch", "branch_missing"]) {
    it(`retained/${reason} → terminal (no more automatic retries); journal + pins intact`, async () => {
      await j.put(record());
      client.answers = [retained(reason)];
      assert.equal(await settler.settleOne(record()), "terminal");
      const [r] = await j.listRun(RUN);
      assert.equal(r!.state, "terminal");
      assert.equal(r!.lastReason, reason);
      assert.deepEqual(cleanup.calls, []);
      clock += 365 * 86_400_000;
      await settler.sweep();
      assert.equal(client.calls.length, 1, "a terminal record is never re-sent by the sweep");
    });
  }

  for (const [label, err, reason] of [
    ["429", new RequestError("POST", "/x", 429, "slow down"), "http_429"],
    ["503", new RequestError("POST", "/x", 503, "unavailable"), "http_503"],
    ["404 (older api)", new RequestError("POST", "/x", 404, "not found"), "http_404"],
    ["network error", new TypeError("fetch failed"), "transport_error"],
  ] as const) {
    it(`${label} → retry`, async () => {
      await j.put(record());
      client.answers = [err];
      assert.equal(await settler.settleOne(record()), "retry");
      const [r] = await j.listRun(RUN);
      assert.equal(r!.state, "pending_settle");
      assert.equal(r!.lastReason, reason);
      assert.deepEqual(cleanup.calls, []);
    });
  }

  it("a 400 (a request the api will never accept) → terminal", async () => {
    await j.put(record());
    client.answers = [new RequestError("POST", "/x", 400, "bad")];
    assert.equal(await settler.settleOne(record()), "terminal");
  });

  it("a released answer naming a DIFFERENT hold never cleans up", async () => {
    await j.put(record());
    client.answers = [{ run_id: RUN, hold_id: HOLD2, outcome: "released" }];
    assert.equal(await settler.settleOne(record()), "retry");
    assert.deepEqual(cleanup.calls, []);
    assert.equal((await j.listRun(RUN)).length, 1);
  });

  it("the sweep honours nextAttemptAt and never sends `adopted` or write-ahead `pushed` records", async () => {
    const HOLD3 = "99999999-bbbb-cccc-dddd-eeeeeeeeeeee";
    await j.put(record({ nextAttemptAt: NOW + 10_000 }));
    await j.put(record({ holdId: HOLD2, state: "adopted", pushedSha: undefined, disposition: undefined }));
    await j.put(record({ holdId: HOLD3, state: "pushed" }));
    await settler.sweep();
    assert.equal(client.calls.length, 0, "not yet due; adopted and pushed never sent");
    clock = NOW + 10_000;
    await settler.sweep();
    assert.deepEqual(client.calls.map((c) => c.holdId), [HOLD], "due pending_settle sent once");
    assert.equal(await settler.settleOne(record({ holdId: HOLD3, state: "pushed" })), "skipped");
    await settler.settleRun(RUN);
    assert.ok(!client.calls.some((c) => c.holdId === HOLD3), "a pushed record is never sent by any path");
  });

  for (const status of [401, 403]) {
    it(`a ${status} (a rotated join token is transient for the worker) → retry, not terminal`, async () => {
      await j.put(record());
      client.answers = [new RequestError("POST", "/x", status, "auth")];
      assert.equal(await settler.settleOne(record()), "retry");
      const [r] = await j.listRun(RUN);
      assert.equal(r!.state, "pending_settle");
      assert.equal(r!.lastReason, `http_${status}`);
    });
  }

  it("a 409 (another unlisted 4xx) stays terminal", async () => {
    await j.put(record());
    client.answers = [new RequestError("POST", "/x", 409, "conflict")];
    assert.equal(await settler.settleOne(record()), "terminal");
  });

  it("a STALE snapshot of a record already released + removed is dropped: no send, and a 5xx never resurrects it", async () => {
    await j.put(record());
    const stale = (await j.listRun(RUN))[0]!;
    assert.equal(await settler.settleOne(stale), "released");
    assert.deepEqual(await j.listRun(RUN), []);
    client.answers = [new RequestError("POST", "/x", 503, "unavailable")];
    assert.equal(await settler.settleOne(stale), "skipped");
    assert.equal(client.calls.length, 1, "the stale copy is never sent");
    assert.deepEqual(await j.listRun(RUN), [], "the record stays removed");
  });

  it("a record removed WHILE its settle is in flight is not resurrected by the retry (5xx) nor the terminal (400)", async () => {
    for (const err of [new RequestError("POST", "/x", 503, "u"), new RequestError("POST", "/x", 400, "b")]) {
      await j.put(record());
      const racing: RecoverySettleClient = {
        settleRecoveryHold: async () => {
          await j.remove(RUN, HOLD); // e.g. a concurrent released cleanup
          throw err;
        },
      };
      const s2 = new PredecessorSettler({ journal: j, client: racing, cleanup, log: nullLogger() });
      assert.equal(await s2.settleOne(record()), "skipped");
      assert.deepEqual(await j.listRun(RUN), [], `record stays removed after ${err.status}`);
    }
  });

  it("a snapshot whose journal copy CHANGED (attempts / nextAttemptAt / state / pushedSha) is dropped unsent", async () => {
    await j.put(record({ attempts: 2, nextAttemptAt: NOW - 1 }));
    for (const snap of [
      record({ attempts: 1, nextAttemptAt: NOW - 1 }),
      record({ attempts: 2, nextAttemptAt: NOW - 2 }),
      record({ attempts: 2, nextAttemptAt: NOW - 1, pushedSha: "4".repeat(40) }),
    ]) {
      assert.equal(await settler.settleOne(snap), "skipped");
    }
    await j.put(record({ state: "terminal", lastReason: "not_ancestor" }));
    assert.equal(await settler.settleOne(record()), "skipped");
    assert.equal(client.calls.length, 0);
  });

  it("an abort stops the sweep promptly: the in-flight settle is aborted and consumes no attempt", async () => {
    await j.put(record());
    let started!: () => void;
    const began = new Promise<void>((r) => (started = r));
    const hanging: RecoverySettleClient = {
      settleRecoveryHold: (_r, _h, _req, signal) =>
        new Promise((_resolve, reject) => {
          started();
          signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
          // Bound the hang so a missing abort reads as a named assertion failure, not a test timeout.
          setTimeout(() => reject(new Error("never aborted")), 2_000).unref();
        }),
    };
    const s2 = new PredecessorSettler({ journal: j, client: hanging, cleanup, log: nullLogger() });
    const ctl = new AbortController();
    const sweep = s2.sweep(ctl.signal);
    await began;
    ctl.abort();
    const t0 = Date.now();
    await sweep;
    assert.ok(Date.now() - t0 < 1_000, "the sweep returned promptly after the abort");
    const [r] = await j.listRun(RUN);
    assert.equal(r!.attempts, 0, "an aborted attempt is not counted");
    assert.equal(r!.state, "pending_settle");
  });

  it("backoff doubles from 1 min and is capped at 6 h; the attempt cap turns a record terminal", async () => {
    assert.equal(settleBackoffMs(1), 60_000);
    assert.equal(settleBackoffMs(2), 120_000);
    assert.equal(settleBackoffMs(50), 6 * 60 * 60_000);
    await j.put(record({ attempts: SETTLE_MAX_ATTEMPTS - 1 }));
    client.answers = [retained("ancestry_unknown")];
    assert.equal(await settler.settleOne(record({ attempts: SETTLE_MAX_ATTEMPTS - 1 })), "terminal");
    const [r] = await j.listRun(RUN);
    assert.equal(r!.state, "terminal");
    assert.equal(r!.attempts, SETTLE_MAX_ATTEMPTS);
  });
});

describe("PredecessorSettler terminal-ACK lifecycle (issue #1582 M2)", () => {
  let root: string;
  let j: SettlementJournal;
  let settler: PredecessorSettler;
  const HOLD3 = "99999999-bbbb-cccc-dddd-eeeeeeeeeeee";

  beforeEach(async () => {
    root = tmp("uzi-settle-ack-");
    j = journal(root);
    settler = new PredecessorSettler({ journal: j, client: new FakeSettleClient(), cleanup: new RecordingCleanup(), log: nullLogger() });
    await j.put(record({ state: "pushed" }));
    await j.put(record({ holdId: HOLD2, state: "adopted", pushedSha: undefined, disposition: undefined }));
    // another generation's record is never touched by gen 2's ACK
    await j.put(record({ holdId: HOLD3, state: "pushed", successorGeneration: 5 }));
  });
  const states = async (): Promise<Record<string, [string, string | undefined]>> =>
    Object.fromEntries((await j.listRun(RUN)).map((r) => [r.holdId, [r.state, r.lastReason]]));

  for (const [label, ack] of [
    ["applied + completed", { applied: true, status: "completed" }],
    ["409 already completed (a replay after a lost ACK)", { applied: false, status: "completed" }],
    ["applied with NO status (an older server)", { applied: true }],
  ] as const) {
    it(`${label}: pushed → pending_settle; adopted → terminal/successor_not_published`, async () => {
      await settler.observeTerminalAck(RUN, 2, { status: "completed" }, ack);
      assert.deepEqual(await states(), {
        [HOLD]: ["pending_settle", undefined],
        [HOLD2]: ["terminal", "successor_not_published"],
        [HOLD3]: ["pushed", undefined],
      });
    });
  }

  for (const status of ["failed", "cancelled"]) {
    it(`a completed report the server answered ${status}: both → terminal/successor_not_completed (not sendable)`, async () => {
      await settler.observeTerminalAck(RUN, 2, { status: "completed" }, { applied: true, status });
      assert.deepEqual(await states(), {
        [HOLD]: ["terminal", "successor_not_completed"],
        [HOLD2]: ["terminal", "successor_not_completed"],
        [HOLD3]: ["pushed", undefined],
      });
    });
  }

  it("the successor's own failed report (applied, no status) → terminal/successor_not_completed", async () => {
    await settler.observeTerminalAck(RUN, 2, { status: "failed" }, { applied: true });
    assert.equal((await states())[HOLD2]![1], "successor_not_completed");
  });

  for (const [label, report, ack] of [
    ["stale_claim", { status: "completed" }, { applied: false, staleClaim: true, status: "completed" }],
    ["a non-terminal 409 (running)", { status: "completed" }, { applied: false, status: "running" }],
    ["an unreadable 409 (no status)", { status: "completed" }, { applied: false }],
    ["a non-terminal report", { status: "limit_wait" }, { applied: true, status: "failed" }],
  ] as const) {
    it(`${label}: nothing changes`, async () => {
      await settler.observeTerminalAck(RUN, 2, report, ack);
      assert.deepEqual(await states(), {
        [HOLD]: ["pushed", undefined],
        [HOLD2]: ["adopted", undefined],
        [HOLD3]: ["pushed", undefined],
      });
    });
  }

  for (const [label, ack] of [
    ["completed (pushed promotion + adopted terminal)", { applied: true, status: "completed" }],
    ["failed (both markTerminal)", { applied: true, status: "failed" }],
  ] as const) {
    it(`${label}: a record released + removed between the observer's list and its write STAYS removed`, async () => {
      // The listRun snapshot is taken, then every gen-2 record is released and removed (a concurrent
      // settle's cleanup) before the observer writes: nothing may be resurrected from the snapshot.
      const racing = j;
      const list = racing.listRun.bind(racing);
      racing.listRun = async (runId: string) => {
        const snap = await list(runId);
        for (const r of snap) if (r.successorGeneration === 2) await racing.remove(r.runId, r.holdId);
        return snap;
      };
      await settler.observeTerminalAck(RUN, 2, { status: "completed" }, ack);
      racing.listRun = list;
      assert.deepEqual(await states(), { [HOLD3]: ["pushed", undefined] }, "removed records stay removed");
      assert.equal(fs.existsSync(path.join(root, RUN, `${HOLD}.json`)), false);
      assert.equal(fs.existsSync(path.join(root, RUN, `${HOLD2}.json`)), false);
    });
  }

  it("supersede: a record removed between the list and the write is not resurrected", async () => {
    const list = j.listRun.bind(j);
    j.listRun = async (runId: string) => {
      const snap = await list(runId);
      await j.remove(RUN, HOLD);
      return snap;
    };
    await settler.supersedeOlderGenerations(RUN, 5);
    j.listRun = list;
    const s = await states();
    assert.equal(s[HOLD], undefined, "the removed record stays removed");
    assert.deepEqual(s[HOLD2], ["terminal", "superseded"]);
  });

  it("a newer generation supersedes OLDER adopted/pushed records (terminal/superseded); same/newer untouched", async () => {
    await j.put(record({ holdId: "88888888-bbbb-cccc-dddd-eeeeeeeeeeee", state: "pending_settle" }));
    await settler.supersedeOlderGenerations(RUN, 5);
    const s = await states();
    assert.deepEqual(s[HOLD], ["terminal", "superseded"]);
    assert.deepEqual(s[HOLD2], ["terminal", "superseded"]);
    assert.deepEqual(s[HOLD3], ["pushed", undefined], "the current generation's own record is untouched");
    assert.deepEqual(s["88888888-bbbb-cccc-dddd-eeeeeeeeeeee"], ["pending_settle", undefined], "settle-eligible kept");
  });
});

describe("WorkerClient.settleRecoveryHold wire (issue #1582 M2 ↔ M1 handler)", () => {
  it("POST /api/worker/runs/{id}/recovery-holds/{holdID}/settle with exactly the five strict fields", async () => {
    const seen: Array<{ method: string; url: string; auth: string; body: string }> = [];
    const server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c) => chunks.push(c as Buffer));
      req.on("end", () => {
        seen.push({
          method: req.method ?? "",
          url: req.url ?? "",
          auth: String(req.headers.authorization),
          body: Buffer.concat(chunks).toString("utf8"),
        });
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ run_id: RUN, hold_id: HOLD, outcome: "retained", reason: "ancestry_unknown" }));
      });
    });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
    try {
      const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
      const c = new WorkerClient(base, "wire-token-0123456789", "0.1.0-test", nullLogger(), {
        sleep: async () => {},
      });
      const extra = {
        predecessor_generation: 1,
        successor_generation: 2,
        pushed_sha: PUSHED,
        source_sha: SRC,
        adopted_sha: ADOPTED,
        ancestry: "ancestor", // must never reach the wire
      } as RecoverySettleRequest;
      const res = await c.settleRecoveryHold(RUN, HOLD, extra);
      assert.equal(res.reason, "ancestry_unknown");
      assert.equal(seen.length, 1);
      assert.equal(seen[0]!.method, "POST");
      assert.equal(seen[0]!.url, `/api/worker/runs/${RUN}/recovery-holds/${HOLD}/settle`);
      assert.match(seen[0]!.auth, /^Bearer /);
      assert.deepEqual(JSON.parse(seen[0]!.body), {
        predecessor_generation: 1,
        successor_generation: 2,
        pushed_sha: PUSHED,
        source_sha: SRC,
        adopted_sha: ADOPTED,
      });
    } finally {
      await new Promise<void>((r) => server.close(() => r()));
    }
  });
});

// ── issue #1751 M2: the LIVE settle leg ─────────────────────────────────────────────────────────

const PUBLISHED = "5".repeat(40);
const PUBLISHED2 = "6".repeat(40);
const ADOPTED3 = "7".repeat(40);
/** Drop undefined-valued keys (the journal round-trip omits them). */
const plain = (x: unknown): unknown => JSON.parse(JSON.stringify(x));

/** A settle client with BOTH RPCs; the live one can be gated so a test holds a send in flight. */
class FakeLiveClient extends FakeSettleClient {
  liveCalls: Array<{ runId: string; holdId: string; req: RecoveryLiveSettleRequest }> = [];
  liveAnswers: Array<RecoverySettleResponse | Error> = [];
  /** When set, every settle (completed AND live) waits on it after recording the call. */
  gate: Promise<void> | undefined;
  entered: Array<() => void> = [];
  private async hold(): Promise<void> {
    for (const e of this.entered.splice(0)) e();
    if (this.gate) await this.gate;
  }
  override async settleRecoveryHold(runId: string, holdId: string, req: RecoverySettleRequest): Promise<RecoverySettleResponse> {
    this.calls.push({ runId, holdId, req });
    await this.hold();
    const a = this.answers.shift() ?? { run_id: runId, hold_id: holdId, outcome: "released" };
    if (a instanceof Error) throw a;
    return a;
  }
  async settleRecoveryHoldLive(runId: string, holdId: string, req: RecoveryLiveSettleRequest): Promise<RecoverySettleResponse> {
    this.liveCalls.push({ runId, holdId, req });
    await this.hold();
    const a = this.liveAnswers.shift() ?? { run_id: runId, hold_id: holdId, outcome: "released" };
    if (a instanceof Error) throw a;
    return a;
  }
  /** Resolves once the next settle call (completed or live) is in flight. */
  nextEntered(): Promise<void> {
    return new Promise((r) => this.entered.push(r));
  }
}

/** Local git for the live leg: ancestry answers by pair (default true), pins recorded. */
class FakeLiveGit implements SettlementLiveGit {
  calls: string[] = [];
  notAncestor = new Set<string>();
  pinOk = true;
  /** When set, isAncestor waits on it (holds observeLivePublication under the hold's lock). */
  gate: Promise<void> | undefined;
  entered: Array<() => void> = [];
  async isAncestor(_bare: string, a: string, d: string): Promise<boolean> {
    for (const e of this.entered.splice(0)) e();
    if (this.gate) await this.gate;
    return !this.notAncestor.has(`${a}>${d}`);
  }
  async pinPublished(_bare: string, runId: string, holdId: string, sha: string): Promise<boolean> {
    this.calls.push(`pin:${runId}/${holdId}/${sha}`);
    return this.pinOk;
  }
  async unpinPublished(_bare: string, runId: string, holdId: string): Promise<void> {
    this.calls.push(`unpin:${runId}/${holdId}`);
  }
  nextEntered(): Promise<void> {
    return new Promise((r) => this.entered.push(r));
  }
}

function gateOf(): { promise: Promise<void>; open: () => void } {
  let open!: () => void;
  const promise = new Promise<void>((r) => (open = r));
  return { promise, open };
}

function adoptedRecord(over: Partial<SettlementRecord> = {}): SettlementRecord {
  return record({ state: "adopted", pushedSha: undefined, disposition: undefined, ...over });
}

const liveLeg = (over: Partial<LiveSettleLeg> = {}): LiveSettleLeg => ({
  target: "checkpoint",
  publishedSha: PUBLISHED,
  predecessorGeneration: 1,
  successorGeneration: 2,
  sourceSha: SRC,
  adoptedSha: ADOPTED,
  sent: false,
  attempts: 0,
  ...over,
});

describe("PredecessorSettler live leg (issue #1751 M2)", () => {
  let root: string;
  let clock: number;
  let j: SettlementJournal;
  let client: FakeLiveClient;
  let cleanup: RecordingCleanup;
  let lg: FakeLiveGit;
  let settler: PredecessorSettler;

  beforeEach(() => {
    root = tmp("uzi-settle-live-");
    clock = NOW;
    j = journal(root, () => clock);
    client = new FakeLiveClient();
    cleanup = new RecordingCleanup();
    lg = new FakeLiveGit();
    settler = new PredecessorSettler({ journal: j, client, cleanup, liveGit: lg, log: nullLogger() });
  });
  const one = async (holdId = HOLD): Promise<SettlementRecord | undefined> =>
    (await j.listRun(RUN)).find((r) => r.holdId === holdId);

  it("records an unsent leg on an adopted record (local ancestry + published pin), sends the exact /settle-live body, released cleans up ONLY that hold", async () => {
    await j.put(adoptedRecord());
    await j.put(adoptedRecord({ holdId: HOLD2, predecessorGeneration: 3, successorGeneration: 4 }));
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 1);
    assert.deepEqual((await one())!.live, liveLeg(), "an unsent leg, MAC-covered in the journal");
    assert.equal((await one())!.state, "adopted");
    assert.deepEqual(lg.calls, [`pin:${RUN}/${HOLD}/${PUBLISHED}`]);
    client.liveAnswers = [{ run_id: RUN, hold_id: HOLD, outcome: "released", final_head_sha: PUBLISHED }];
    await settler.settleLive(RUN);
    assert.deepEqual(client.liveCalls, [
      {
        runId: RUN,
        holdId: HOLD,
        req: {
          predecessor_generation: 1,
          successor_generation: 2,
          published_sha: PUBLISHED,
          source_sha: SRC,
          adopted_sha: ADOPTED,
          target: "checkpoint",
        },
      },
    ]);
    assert.deepEqual(Object.keys(client.liveCalls[0]!.req).sort(), [
      "adopted_sha",
      "predecessor_generation",
      "published_sha",
      "source_sha",
      "successor_generation",
      "target",
    ]);
    assert.equal(client.calls.length, 0, "the completed /settle is never used by the live leg");
    // released → the one cleanup: every settlement pin (published included), recovery pin, journal.
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual((await j.listRun(RUN)).map((r) => r.holdId), [HOLD2], "the sibling record is untouched");
    assert.equal((await one(HOLD2))!.live, undefined, "a different generation's record takes no leg");
  });

  it("the branch target is carried verbatim", async () => {
    await j.put(adoptedRecord());
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "branch"), 1);
    await settler.settleLive(RUN);
    assert.equal(client.liveCalls[0]!.req.target, "branch");
  });

  for (const [label, answer, reason] of [
    ["retained/ancestry_unknown", retained("ancestry_unknown"), "ancestry_unknown"],
    ["retained/state_changed", retained("state_changed"), "state_changed"],
    ["401", new RequestError("POST", "/x", 401, "auth"), "http_401"],
    ["403", new RequestError("POST", "/x", 403, "auth"), "http_403"],
    ["404 (older api)", new RequestError("POST", "/x", 404, "nf"), "http_404"],
    ["408", new RequestError("POST", "/x", 408, "t"), "http_408"],
    ["429", new RequestError("POST", "/x", 429, "slow"), "http_429"],
    ["503", new RequestError("POST", "/x", 503, "u"), "http_503"],
    ["network error", new TypeError("fetch failed"), "transport_error"],
  ] as const) {
    it(`${label} → the leg backs off (sent, attempts++, nextAttemptAt); record, evidence and pins kept`, async () => {
      await j.put(adoptedRecord({ live: liveLeg() }));
      client.liveAnswers = [answer];
      await settler.settleLive(RUN);
      const r = (await one())!;
      assert.equal(r.state, "adopted");
      assert.deepEqual(r.live, liveLeg({ sent: true, attempts: 1, lastReason: reason, nextAttemptAt: NOW + SETTLE_BACKOFF_BASE_MS }));
      assert.deepEqual(cleanup.calls, []);
      assert.deepEqual(lg.calls, [], "the published pin is kept");
      await settler.settleLive(RUN);
      await settler.sweep();
      assert.equal(client.liveCalls.length, 1, "not re-sent before it is due");
      clock = NOW + SETTLE_BACKOFF_BASE_MS;
      await settler.sweep();
      assert.equal(client.liveCalls.length, 2, "the sweep retries it once due");
      assert.deepEqual(await j.listRun(RUN), [], "the retry's released answer cleaned up");
    });
  }

  for (const [label, answer] of [
    ["retained/not_ancestor", retained("not_ancestor")],
    ["retained/not_eligible", retained("not_eligible")],
    ["retained/candidate_mismatch", retained("candidate_mismatch")],
    ["retained/branch_missing", retained("branch_missing")],
    ["an unrecognised retained reason", retained("mystery")],
    ["400", new RequestError("POST", "/x", 400, "bad")],
    ["409", new RequestError("POST", "/x", 409, "conflict")],
  ] as const) {
    it(`${label} → clears ONLY the live leg + its published pin; the record keeps its state and still settles on completion`, async () => {
      await j.put(adoptedRecord({ live: liveLeg() }));
      client.liveAnswers = [answer];
      await settler.settleLive(RUN);
      const r = (await one())!;
      assert.equal(r.state, "adopted", "the record keeps its state");
      assert.equal(r.live, undefined, "only the leg is cleared");
      assert.deepEqual(lg.calls, [`unpin:${RUN}/${HOLD}`], "only the published pin is dropped");
      assert.deepEqual(cleanup.calls, [], "no release cleanup");
      // The completion lifecycle is unaffected.
      assert.equal(await settler.recordPushedHead(r, PUSHED), true);
      await settler.observeTerminalAck(RUN, 2, { status: "completed" }, { applied: true, status: "completed" });
      await settler.settleRun(RUN);
      assert.equal(client.calls.length, 1, "the completed /settle is still sent");
      assert.deepEqual(await j.listRun(RUN), []);
    });
  }

  it("a definitive answer on a pending_settle record's leg keeps it pending_settle", async () => {
    await j.put(record({ live: liveLeg({ sent: true }) }));
    client.liveAnswers = [retained("not_eligible")];
    client.answers = [retained("ancestry_unknown")];
    await settler.sweep();
    const r = (await one())!;
    assert.equal(r.state, "pending_settle");
    assert.equal(r.live, undefined);
  });

  it("the attempt cap clears the leg (record kept)", async () => {
    await j.put(adoptedRecord({ live: liveLeg({ sent: true, attempts: SETTLE_MAX_ATTEMPTS - 1 }) }));
    client.liveAnswers = [retained("ancestry_unknown")];
    await settler.settleLive(RUN);
    const r = (await one())!;
    assert.equal(r.state, "adopted");
    assert.equal(r.live, undefined);
    assert.deepEqual(lg.calls, [`unpin:${RUN}/${HOLD}`]);
  });

  it("a released answer naming a DIFFERENT hold never cleans up (backs off)", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    client.liveAnswers = [{ run_id: RUN, hold_id: HOLD2, outcome: "released" }];
    await settler.settleLive(RUN);
    assert.deepEqual(cleanup.calls, []);
    assert.equal((await one())!.live!.lastReason, "response_mismatch");
  });

  it("an abort consumes no attempt: the sent leg stays due", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    const ctl = new AbortController();
    const s2 = new PredecessorSettler({
      journal: j,
      client: {
        settleRecoveryHold: async () => ({ run_id: RUN, hold_id: HOLD, outcome: "released" }),
        settleRecoveryHoldLive: (_r, _h, _req, signal) =>
          new Promise((_res, rej) => {
            signal?.addEventListener("abort", () => rej(new Error("aborted")), { once: true });
            setTimeout(() => rej(new Error("never aborted")), 2_000).unref();
          }),
      },
      cleanup,
      liveGit: lg,
      log: nullLogger(),
    });
    const p = s2.settleLive(RUN, ctl.signal);
    await new Promise((r) => setImmediate(r));
    await new Promise((r) => setTimeout(r, 20));
    ctl.abort();
    await p;
    assert.deepEqual((await one())!.live, liveLeg({ sent: true }), "sent persisted; no attempt counted, still due");
  });

  it("observeLivePublication acts only on an EXISTING adopted record of the same generation with local ancestry and a pin", async () => {
    await j.put(record({ state: "pushed" })); // HOLD: pushed — never overwritten
    await j.put(adoptedRecord({ holdId: HOLD2, successorGeneration: 3 })); // another generation
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0);
    assert.equal(await settler.observeLivePublication(RUN, 9, PUBLISHED, "checkpoint"), 0);
    assert.equal(await settler.observeLivePublication("22222222-2222-3333-4444-555555555555", 2, PUBLISHED, "checkpoint"), 0);
    assert.equal((await one())!.live, undefined);
    assert.equal((await one(HOLD2))!.live, undefined);
    assert.equal(fs.existsSync(path.join(root, "22222222-2222-3333-4444-555555555555")), false, "nothing is created");

    await j.put(adoptedRecord());
    lg.notAncestor.add(`${SRC}>${PUBLISHED}`);
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0, "source not contained");
    lg.notAncestor.clear();
    lg.notAncestor.add(`${ADOPTED}>${PUBLISHED}`);
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0, "adopted tip not contained");
    lg.notAncestor.clear();
    lg.pinOk = false;
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0, "pin failed");
    lg.pinOk = true;
    assert.equal(await settler.observeLivePublication(RUN, 2, "not-a-sha", "checkpoint"), 0, "a malformed sha");
    assert.equal((await one())!.live, undefined);
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 1);
  });

  it("a SENT leg is never replaced: a newer publication is ignored until the original request resolves", async () => {
    await j.put(adoptedRecord());
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 1);
    client.liveAnswers = [new TypeError("fetch failed")]; // the ACK is lost
    await settler.settleLive(RUN);
    const sent = (await one())!.live!;
    assert.deepEqual([sent.sent, sent.attempts], [true, 1]);
    // A newer publication (it descends the sent tip) arrives while the sent leg is unresolved.
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED2, "checkpoint"), 0, "ignored");
    assert.deepEqual((await one())!.live, sent, "the sent leg is unchanged");
    assert.deepEqual(lg.calls, [`pin:${RUN}/${HOLD}/${PUBLISHED}`], "the newer tip is never pinned");
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    client.liveAnswers = [{ run_id: RUN, hold_id: HOLD, outcome: "released" }];
    await settler.sweep();
    assert.equal(client.liveCalls.length, 2);
    assert.deepEqual(client.liveCalls[1]!.req, client.liveCalls[0]!.req, "the retry re-sends the ORIGINAL identity");
    assert.equal(client.liveCalls[1]!.req.published_sha, PUBLISHED);
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("supersede drops an UNSENT leg with its published pin; a SENT leg rides along", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    await j.put(adoptedRecord({ holdId: HOLD2, live: liveLeg({ sent: true }) }));
    await settler.supersedeOlderGenerations(RUN, 3);
    const r1 = (await one())!;
    const r2 = (await one(HOLD2))!;
    assert.deepEqual([r1.state, r1.lastReason, r1.live], ["terminal", "superseded", undefined]);
    assert.deepEqual([r2.state, r2.lastReason, r2.live], ["terminal", "superseded", liveLeg({ sent: true })]);
    assert.deepEqual(lg.calls, [`unpin:${RUN}/${HOLD}`], "only the dropped unsent leg's published pin is removed");
    await settler.sweep();
    assert.deepEqual(client.liveCalls.map((c) => c.holdId), [HOLD2], "only the sent leg is still sent");
  });

  it("each live RPC is bounded by its own timeout: a hanging send backs off and frees the hold's lock", async () => {
    await j.put(record({ live: liveLeg() })); // pending_settle with a due live leg
    const hanging = new PredecessorSettler({
      journal: j,
      client: {
        settleRecoveryHold: async (runId, holdId) => ({ run_id: runId, hold_id: holdId, outcome: "released" }),
        settleRecoveryHoldLive: (_r, _h, _req, signal) =>
          new Promise((_res, rej) => {
            signal?.addEventListener("abort", () => rej(signal.reason), { once: true });
            setTimeout(() => rej(new Error("never aborted")), 5_000).unref();
          }),
      },
      cleanup,
      liveGit: lg,
      liveTimeoutMs: 50,
      log: nullLogger(),
    });
    const t0 = Date.now();
    const live = hanging.settleLive(RUN);
    await new Promise((r) => setImmediate(r));
    // A same-hold completion step queues behind the in-flight live send, then runs.
    const completed = hanging.settleRun(RUN);
    await Promise.all([live, completed]);
    assert.ok(Date.now() - t0 < 2_000, "bounded by the live timeout, not the hang");
    assert.deepEqual(await j.listRun(RUN), [], "the queued completed settle ran after the bound and released");
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
  });

  it("a timed-out live RPC counts as a transient attempt (reason timeout)", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    const hanging = new PredecessorSettler({
      journal: j,
      client: {
        settleRecoveryHold: async (runId, holdId) => ({ run_id: runId, hold_id: holdId, outcome: "released" }),
        settleRecoveryHoldLive: (_r, _h, _req, signal) =>
          new Promise((_res, rej) => {
            signal?.addEventListener("abort", () => rej(signal.reason), { once: true });
            setTimeout(() => rej(new Error("never aborted")), 5_000).unref();
          }),
      },
      cleanup,
      liveGit: lg,
      liveTimeoutMs: 50,
      log: nullLogger(),
    });
    await hanging.settleLive(RUN);
    assert.deepEqual((await one())!.live, liveLeg({ sent: true, attempts: 1, lastReason: "timeout", nextAttemptAt: NOW + SETTLE_BACKOFF_BASE_MS }));
  });

  it("a legacy leg (written before the request snapshot) parses under its MAC and is sent from its record", async () => {
    const rec = adoptedRecord();
    const legacy = { ...rec, live: { target: "checkpoint", publishedSha: PUBLISHED, sent: true, attempts: 1 } };
    const key = createHmac("sha256", TOKEN).update("uzi-recovery-settlement-v1").digest();
    const mac = createHmac("sha256", key).update(canonicalJson(legacy)).digest("hex");
    fs.mkdirSync(path.join(root, RUN), { recursive: true });
    fs.writeFileSync(path.join(root, RUN, `${HOLD}.json`), JSON.stringify({ ...legacy, mac }));
    assert.deepEqual((await one())!.live, liveLeg({ sent: true, attempts: 1 }), "completed from the record");
    client.liveAnswers = [retained("ancestry_unknown")];
    await settler.sweep();
    assert.deepEqual(client.liveCalls[0]!.req, {
      predecessor_generation: 1,
      successor_generation: 2,
      published_sha: PUBLISHED,
      source_sha: SRC,
      adopted_sha: ADOPTED,
      target: "checkpoint",
    });
    assert.equal((await one())!.live!.attempts, 2, "the retry rewrote the leg (now with its snapshot)");
    // A partial snapshot is not a legacy shape: refused.
    const partial = { ...rec, live: { target: "checkpoint", publishedSha: PUBLISHED, sent: true, attempts: 1, sourceSha: SRC } };
    const pmac = createHmac("sha256", key).update(canonicalJson(partial)).digest("hex");
    fs.writeFileSync(path.join(root, RUN, `${HOLD}.json`), JSON.stringify({ ...partial, mac: pmac }));
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("an UNSENT leg is replaced only by a NEWER publication of the same generation", async () => {
    await j.put(adoptedRecord({ live: liveLeg({ attempts: 0 }) }));
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0, "the same tip is a no-op");
    lg.notAncestor.add(`${PUBLISHED}>${PUBLISHED2}`);
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED2, "checkpoint"), 0, "not newer: kept");
    assert.equal((await one())!.live!.publishedSha, PUBLISHED);
    lg.notAncestor.clear();
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED2, "checkpoint"), 1);
    assert.deepEqual((await one())!.live, liveLeg({ publishedSha: PUBLISHED2 }));
  });

  it("the leg is MAC-covered: flipping `sent` or the published tip is refused", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    const file = path.join(root, RUN, `${HOLD}.json`);
    const obj = JSON.parse(fs.readFileSync(file, "utf8"));
    obj.live.publishedSha = PUBLISHED2;
    fs.writeFileSync(file, JSON.stringify(obj));
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("no live RPC on the client, or no local git: nothing is recorded", async () => {
    await j.put(adoptedRecord());
    const noLive = new PredecessorSettler({ journal: j, client: new FakeSettleClient(), cleanup, liveGit: lg, log: nullLogger() });
    assert.equal(await noLive.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0);
    const noGit = new PredecessorSettler({ journal: j, client, cleanup, log: nullLogger() });
    assert.equal(await noGit.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0);
    assert.equal((await one())!.live, undefined);
  });

  it("restart: a NEW settler over the same journal retries a due leg with its evidence and pins preserved", async () => {
    await j.put(adoptedRecord());
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 1);
    client.liveAnswers = [new RequestError("POST", "/x", 503, "u")];
    await settler.settleLive(RUN);
    const before = (await one())!;
    assert.equal(before.live!.attempts, 1);
    // A new worker life: a fresh journal instance + settler over the same root.
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    const j2 = journal(root, () => clock);
    const client2 = new FakeLiveClient();
    const lg2 = new FakeLiveGit();
    const cleanup2 = new RecordingCleanup();
    const s2 = new PredecessorSettler({ journal: j2, client: client2, cleanup: cleanup2, liveGit: lg2, log: nullLogger() });
    client2.liveAnswers = [retained("ancestry_unknown")];
    await s2.sweep();
    assert.deepEqual(client2.liveCalls.map((c) => c.req), [client.liveCalls[0]!.req], "the identical body is re-sent");
    const after = (await j2.listRun(RUN))[0]!;
    assert.deepEqual({ ...after, live: undefined }, { ...before, live: undefined }, "the evidence is unchanged");
    assert.equal(after.live!.attempts, 2);
    assert.deepEqual(lg2.calls, [], "the published pin is never dropped on a retry");
    assert.deepEqual(cleanup2.calls, [], "no pin is deleted");
    clock += settleBackoffMs(2);
    await s2.sweep();
    assert.deepEqual(cleanup2.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual(await j2.listRun(RUN), []);
  });
});

describe("PredecessorSettler per-hold queued lock (issue #1751 M2, R3)", () => {
  let root: string;
  let clock: number;
  let j: SettlementJournal;
  let client: FakeLiveClient;
  let cleanup: RecordingCleanup;
  let lg: FakeLiveGit;
  let settler: PredecessorSettler;

  beforeEach(() => {
    root = tmp("uzi-settle-lock-");
    clock = NOW;
    j = journal(root, () => clock);
    client = new FakeLiveClient();
    cleanup = new RecordingCleanup();
    lg = new FakeLiveGit();
    settler = new PredecessorSettler({ journal: j, client, cleanup, liveGit: lg, log: nullLogger() });
  });
  const one = async (): Promise<SettlementRecord | undefined> => (await j.listRun(RUN))[0];

  it("a live publication racing the pushed head + completion ACK: the promotion is never lost, and the leg rides along", async () => {
    await j.put(adoptedRecord());
    const g = gateOf();
    lg.gate = g.promise;
    const inAncestry = lg.nextEntered();
    // The live publication snapshots the record as `adopted` and blocks in its local ancestry check.
    const live = settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint");
    await inAncestry;
    // Meanwhile the run completes: write-ahead pushed head, then the completion ACK.
    const snap = (await one())!;
    const completion = (async () => {
      await settler.recordPushedHead(snap, PUSHED);
      await settler.observeTerminalAck(RUN, 2, { status: "completed" }, { applied: true, status: "completed" });
    })();
    await new Promise((r) => setTimeout(r, 20));
    lg.gate = undefined;
    g.open();
    await Promise.all([live, completion]);
    const r = (await one())!;
    assert.equal(r.state, "pending_settle", "the pushed → pending_settle promotion is not lost");
    assert.equal(r.pushedSha, PUSHED);
    assert.deepEqual(r.live, liveLeg(), "the leg written first is carried by both transitions");
  });

  it("a LATE live publication never overwrites a pushed / pending_settle record", async () => {
    for (const state of ["pushed", "pending_settle"] as const) {
      await j.put(record({ state }));
      assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint"), 0);
      const r = (await one())!;
      assert.equal(r.state, state);
      assert.equal(r.live, undefined);
    }
  });

  it("a live release vs a completed settlement: exactly one send + one cleanup; nothing is deleted while the other attempt is in flight", async () => {
    await j.put(record({ live: liveLeg() })); // pending_settle with a due live leg
    const g = gateOf();
    client.gate = g.promise;
    const inLive = client.nextEntered();
    const live = settler.settleLive(RUN);
    await inLive;
    const completed = settler.settleRun(RUN);
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(client.calls.length, 0, "the completed settle waits for the in-flight live one");
    assert.deepEqual(cleanup.calls, [], "no pin is deleted while the live attempt is in flight");
    assert.ok((await one())!.live!.sent, "the leg was marked sent before the request left");
    client.gate = undefined;
    g.open();
    await Promise.all([live, completed]);
    assert.equal(client.liveCalls.length, 1);
    assert.equal(client.calls.length, 0, "the completed settle found the record released and sent nothing");
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`], "exactly one cleanup");
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("a completed release vs a live settle: the live one waits, then finds nothing to send", async () => {
    await j.put(record({ live: liveLeg() }));
    const g = gateOf();
    client.gate = g.promise;
    const inCompleted = client.nextEntered();
    const completed = settler.settleRun(RUN);
    await inCompleted;
    const live = settler.settleLive(RUN);
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(client.liveCalls.length, 0);
    assert.deepEqual(cleanup.calls, [], "no pin is deleted while the completed attempt is in flight");
    client.gate = undefined;
    g.open();
    await Promise.all([live, completed]);
    assert.equal(client.calls.length, 1);
    assert.equal(client.liveCalls.length, 0);
    assert.equal(cleanup.calls.length, 3, "exactly one cleanup");
  });

  it("a live publication after (or racing) the released cleanup never recreates the record", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    const g = gateOf();
    client.gate = g.promise;
    const inLive = client.nextEntered();
    const live = settler.settleLive(RUN);
    await inLive;
    // A newer publication arrives while the release is in flight (it saw the record `adopted`).
    const pub = settler.observeLivePublication(RUN, 2, PUBLISHED2, "checkpoint");
    await new Promise((r) => setTimeout(r, 20));
    client.gate = undefined;
    g.open();
    assert.equal(await pub.then(async (n) => (await live, n)), 0);
    assert.deepEqual(await j.listRun(RUN), [], "released and not recreated");
    assert.equal(fs.existsSync(path.join(root, RUN, `${HOLD}.json`)), false);
    assert.equal(await settler.observeLivePublication(RUN, 2, PUBLISHED2, "checkpoint"), 0, "a later one neither");
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("a concurrent sweep and settleLive send exactly once", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    const g = gateOf();
    client.gate = g.promise;
    const inLive = client.nextEntered();
    const a = settler.sweep();
    await inLive;
    const b = settler.settleLive(RUN);
    await new Promise((r) => setTimeout(r, 20));
    client.gate = undefined;
    g.open();
    await Promise.all([a, b]);
    assert.equal(client.liveCalls.length, 1, "exactly one send");
    assert.equal(cleanup.calls.length, 3, "exactly one cleanup");
  });

  it("a retained answer while a second attempt queues: the queued one re-reads the backoff and does not send", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    client.liveAnswers = [retained("ancestry_unknown")];
    const g = gateOf();
    client.gate = g.promise;
    const inLive = client.nextEntered();
    const a = settler.settleLive(RUN);
    await inLive;
    const b = settler.sweep();
    client.gate = undefined;
    g.open();
    await Promise.all([a, b]);
    assert.equal(client.liveCalls.length, 1);
    assert.equal((await one())!.live!.attempts, 1);
  });

  it("different holds never block each other", async () => {
    await j.put(adoptedRecord({ live: liveLeg() }));
    await j.put(adoptedRecord({ holdId: HOLD2, live: liveLeg() }));
    const g = gateOf();
    client.gate = g.promise;
    const inLive = client.nextEntered();
    const a = settler.settleLive(RUN); // HOLD first (sorted), held in flight
    await inLive;
    // A HOLD2 transition proceeds while HOLD's send is held (a HOLD one would queue).
    const snap2 = (await j.listRun(RUN)).find((r) => r.holdId === HOLD2)!;
    assert.equal(await settler.markTerminal(snap2, "superseded"), true);
    const rec2 = (await j.listRun(RUN)).find((r) => r.holdId === HOLD2)!;
    assert.equal(rec2.state, "terminal", "HOLD2 moved while HOLD was locked");
    assert.deepEqual(rec2.live, liveLeg(), "HOLD2's unsent leg rides along");
    client.gate = undefined;
    g.open();
    await a;
  });
});

describe("PredecessorSettler — a SENT live leg survives the record's lifecycle (issue #1751 M2, R1)", () => {
  let root: string;
  let clock: number;
  let j: SettlementJournal;
  let client: FakeLiveClient;
  let cleanup: RecordingCleanup;
  let lg: FakeLiveGit;
  let settler: PredecessorSettler;

  beforeEach(async () => {
    root = tmp("uzi-settle-r1-");
    clock = NOW;
    j = journal(root, () => clock);
    client = new FakeLiveClient();
    cleanup = new RecordingCleanup();
    lg = new FakeLiveGit();
    settler = new PredecessorSettler({ journal: j, client, cleanup, liveGit: lg, log: nullLogger() });
    // A live send whose answer was lost (transport error): sent, backing off.
    await j.put(adoptedRecord());
    await settler.observeLivePublication(RUN, 2, PUBLISHED, "checkpoint");
    client.liveAnswers = [new TypeError("fetch failed")];
    await settler.settleLive(RUN);
  });
  const one = async (): Promise<SettlementRecord | undefined> => (await j.listRun(RUN))[0];
  const sentLeg = (): LiveSettleLeg =>
    liveLeg({ sent: true, attempts: 1, lastReason: "transport_error", nextAttemptAt: NOW + SETTLE_BACKOFF_BASE_MS });

  it("completion (pushed → pending_settle) carries the leg; its later retry answering released cleans up once", async () => {
    await settler.recordPushedHead((await one())!, PUSHED);
    await settler.observeTerminalAck(RUN, 2, { status: "completed" }, { applied: true, status: "completed" });
    let r = (await one())!;
    assert.equal(r.state, "pending_settle");
    assert.deepEqual(r.live, sentLeg());
    // The completed settle is retained (e.g. the api is still catching up); the live leg is retried.
    client.answers = [retained("ancestry_unknown")];
    await settler.settleRun(RUN);
    r = (await one())!;
    assert.deepEqual(r.live, sentLeg(), "a completed-path retry keeps the leg");
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    client.answers = [retained("ancestry_unknown")]; // the completed path still cannot prove it
    client.liveAnswers = [{ run_id: RUN, hold_id: HOLD, outcome: "released" }];
    await settler.sweep();
    assert.equal(client.calls.length, 2);
    assert.equal(client.liveCalls.length, 2, "the sweep retried the sent leg on a pending_settle record");
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("an adopted completion (→ terminal/successor_not_published) carries the leg; the retry releases", async () => {
    await settler.observeTerminalAck(RUN, 2, { status: "completed" }, { applied: true, status: "completed" });
    const r = (await one())!;
    assert.deepEqual([r.state, r.lastReason], ["terminal", "successor_not_published"]);
    assert.deepEqual(r.live, sentLeg());
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    await settler.sweep();
    assert.equal(client.liveCalls.length, 2, "a terminal record's sent leg is still retried");
    assert.equal(cleanup.calls.length, 3);
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("supersede (→ terminal/superseded) carries the leg; the retry releases", async () => {
    await settler.supersedeOlderGenerations(RUN, 3);
    const r = (await one())!;
    assert.deepEqual([r.state, r.lastReason], ["terminal", "superseded"]);
    assert.deepEqual(r.live, sentLeg());
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    await settler.sweep();
    assert.equal(cleanup.calls.length, 3);
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("same/older-generation evidence never replaces a record whose leg was sent", async () => {
    for (const gen of [2, 1]) {
      assert.equal(await settler.recordAdoption(adoptedRecord({ successorGeneration: gen, adoptedSha: ADOPTED3 })), false);
    }
    assert.deepEqual((await one())!.live, sentLeg());
    assert.deepEqual([(await one())!.successorGeneration, (await one())!.adoptedSha], [2, ADOPTED]);
  });

  it("NEWER-generation evidence replaces the record's evidence and CARRIES the sent leg, which re-sends its own snapshot and releases", async () => {
    const gen3 = adoptedRecord({ successorGeneration: 3, adoptedSha: ADOPTED3, createdAt: NOW + 5 });
    assert.equal(await settler.recordAdoption(gen3), true);
    const r = (await one())!;
    assert.deepEqual(plain({ ...r, live: undefined }), plain(gen3), "the evidence is gen 3's");
    assert.deepEqual(r.live, sentLeg(), "the gen-2 sent leg is carried unchanged");
    assert.deepEqual(lg.calls, [`pin:${RUN}/${HOLD}/${PUBLISHED}`], "the published pin is kept");
    // A gen-3 publication is ignored while the gen-2 leg is unresolved.
    assert.equal(await settler.observeLivePublication(RUN, 3, PUBLISHED2, "checkpoint"), 0);
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    client.liveAnswers = [{ run_id: RUN, hold_id: HOLD, outcome: "released" }];
    await settler.sweep();
    assert.equal(client.liveCalls.length, 2);
    assert.deepEqual(client.liveCalls[1]!.req, client.liveCalls[0]!.req, "exactly the gen-2 request");
    assert.equal(client.liveCalls[1]!.req.successor_generation, 2);
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual(await j.listRun(RUN), []);
  });

  it("gen-2 sent leg, gen-3 adoption, then gen 2 answers not_eligible: the leg clears and gen 3 settles via the completed path", async () => {
    const gen3 = adoptedRecord({ successorGeneration: 3, adoptedSha: ADOPTED3 });
    assert.equal(await settler.recordAdoption(gen3), true);
    clock = NOW + SETTLE_BACKOFF_BASE_MS;
    client.liveAnswers = [retained("not_eligible")];
    await settler.sweep();
    const r = (await one())!;
    assert.equal(r.live, undefined, "the gen-2 leg is cleared");
    assert.deepEqual(plain(r), plain(gen3), "the record still carries gen 3's evidence");
    assert.deepEqual(lg.calls, [`pin:${RUN}/${HOLD}/${PUBLISHED}`, `unpin:${RUN}/${HOLD}`]);
    assert.deepEqual(cleanup.calls, []);
    // Generation 3 completes: pushed head, completion ACK, completed settle.
    assert.equal(await settler.recordPushedHead(r, PUSHED), true);
    await settler.observeTerminalAck(RUN, 3, { status: "completed" }, { applied: true, status: "completed" });
    await settler.settleRun(RUN);
    assert.deepEqual(client.calls.map((c) => c.req), [
      { predecessor_generation: 1, successor_generation: 3, pushed_sha: PUSHED, source_sha: SRC, adopted_sha: ADOPTED3 },
    ]);
    assert.deepEqual(cleanup.calls, [`refs:${RUN}/${HOLD}`, `pin:${RUN}/1`, `journal:${RUN}/1`]);
    assert.deepEqual(await j.listRun(RUN), []);
  });
});

describe("WorkerClient.settleRecoveryHoldLive wire (issue #1751 M2)", () => {
  it("POST /api/worker/runs/{id}/recovery-holds/{holdID}/settle-live with exactly the six strict fields", async () => {
    const seen: Array<{ method: string; url: string; body: string }> = [];
    const server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c) => chunks.push(c as Buffer));
      req.on("end", () => {
        seen.push({ method: req.method ?? "", url: req.url ?? "", body: Buffer.concat(chunks).toString("utf8") });
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ run_id: RUN, hold_id: HOLD, outcome: "retained", reason: "not_eligible" }));
      });
    });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
    try {
      const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
      const c = new WorkerClient(base, "wire-token-0123456789", "0.1.0-test", nullLogger(), { sleep: async () => {} });
      const extra = {
        predecessor_generation: 1,
        successor_generation: 2,
        published_sha: PUBLISHED,
        source_sha: SRC,
        adopted_sha: ADOPTED,
        target: "branch",
        pushed_sha: PUSHED, // must never reach the wire
      } as RecoveryLiveSettleRequest;
      const res = await c.settleRecoveryHoldLive(RUN, HOLD, extra);
      assert.equal(res.reason, "not_eligible");
      assert.equal(seen.length, 1);
      assert.equal(seen[0]!.method, "POST");
      assert.equal(seen[0]!.url, `/api/worker/runs/${RUN}/recovery-holds/${HOLD}/settle-live`);
      assert.deepEqual(JSON.parse(seen[0]!.body), {
        predecessor_generation: 1,
        successor_generation: 2,
        published_sha: PUBLISHED,
        source_sha: SRC,
        adopted_sha: ADOPTED,
        target: "branch",
      });
    } finally {
      await new Promise<void>((r) => server.close(() => r()));
    }
  });
});
