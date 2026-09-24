import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import type { AddressInfo } from "node:net";

import { RequestError, WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import {
  PredecessorSettler,
  SETTLE_BACKOFF_BASE_MS,
  SETTLE_MAX_ATTEMPTS,
  SettlementJournal,
  settleBackoffMs,
  type RecoverySettleClient,
  type SettlementCleanup,
  type SettlementRecord,
} from "../src/recovery-settlement.js";
import type { RecoverySettleRequest, RecoverySettleResponse } from "../src/protocol.js";
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
