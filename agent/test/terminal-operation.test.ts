import { NO_SCAN_NEEDLES } from "./run-env-test-support.js";
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import os from "node:os";
import path from "node:path";

import { RecoveryCoordinator } from "../src/recovery.js";
import {
  mergeTerminalRecord,
  runTerminalOperation,
  type TerminalDelta,
  type TerminalManifest,
  type TerminalOperationDeps,
  type TerminalOperationRecord,
  type TerminalOperationStore,
  type PrepareOutcome,
} from "../src/terminal-operation.js";
import { serializeCheckpointRange } from "../src/terminal-range.js";
import type { TerminalPreservationRequest } from "../src/protocol.js";
import type { TerminalPreservationResult } from "../src/client.js";
import { FakeRecoveryClient, FakeRecoveryGit } from "./codex-reap-fixture.js";
import { nullLogger } from "./helpers.js";

const RUN = "11111111-1111-4111-8111-111111111111";
const TOKEN = "terminal-operation-test-worker-token-0123456789";
const A = "a".repeat(40), B = "b".repeat(40), C = "c".repeat(40);
const RANGE = serializeCheckpointRange({ tipSha: A, excludeSha: B });
const MANIFEST: TerminalManifest = {
  run_id: RUN, generation: 1, worker_id: "w1",
  roots: [{ source_id: "s1", attempt_id: "at1", commit: A, tree: B }], abandoned: [],
};

const roots: string[] = [];
afterEach(() => { for (const r of roots.splice(0)) fs.rmSync(r, { recursive: true, force: true }); });

function mkRoot(): string {
  const r = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-tp-op-"));
  roots.push(r);
  return r;
}

class SpyGit extends FakeRecoveryGit {
  calls: string[] = [];
}
function spyOnAll<T extends object>(target: T, sink: string[]): T {
  return new Proxy(target, {
    get(t, prop, recv) {
      const v = Reflect.get(t, prop, recv);
      if (typeof v === "function") return (...args: unknown[]) => { sink.push(String(prop)); return (v as (...a: unknown[]) => unknown).apply(t, args); };
      return v;
    },
  });
}

function mkCoord(root: string, on = true, spy?: string[]): RecoveryCoordinator {
  const git = spy ? spyOnAll(new SpyGit(), spy) : new FakeRecoveryGit();
  const client = spy ? spyOnAll(new FakeRecoveryClient(), spy) : new FakeRecoveryClient();
  return new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
    client, git, log: nullLogger(), recoveryRoot: path.join(root, "recovery"), workerToken: TOKEN,
    ...(on ? { terminalPreservationTestOverride: {} } : {}),
  });
}

const tpDir = (root: string, run = RUN) => path.join(root, "terminal-preservation", run);
const recFile = (root: string, gen = 1) => path.join(tpDir(root), `g${gen}.json`);

function rec(over: Partial<TerminalOperationRecord> = {}): TerminalOperationRecord {
  return {
    runId: RUN, workerId: "w1", generation: 1, authorityClass: "current_flight", universeDigest: "u1",
    verdicts: {}, cloneRef: "refs/uzi-terminal-local/x/g1", bareRef: "refs/uzi-terminal-local/x/g1", clonePath: "/c",
    flaggedCommits: [], phase: "capturing", invocations: [], localBlock: [], ...over,
  };
}
const preparedRec = (over: Partial<TerminalOperationRecord> = {}) => rec({
  phase: "prepared", manifest: MANIFEST, tip: A, range: RANGE, scanDigest: "s", transportDigest: "t", ...over,
});
const inv = (outcome: "invoked" | "published" | "recorded" | "publish_unknown" | "refused" | "conflict", reason?: string) =>
  ({ at: "2026-01-01T00:00:00.000Z", outcome, ...(reason ? { reason } : {}) });

function mustMerge(cur: TerminalOperationRecord, d: TerminalDelta): TerminalOperationRecord {
  const r = mergeTerminalRecord(cur, d);
  assert.ok(r.ok, r.ok ? "" : r.reason);
  return r.record;
}
function mergeReason(cur: TerminalOperationRecord, d: TerminalDelta): string {
  const r = mergeTerminalRecord(cur, d);
  assert.ok(!r.ok, "expected rejection");
  return r.ok ? "" : r.reason;
}

// ---- harness for the engine --------------------------------------------------------------------------------

interface H {
  sends: TerminalPreservationRequest[];
  captures: number;
  prepares: Array<{ range: unknown; expect: unknown }>;
  failure: string | undefined;
  late: string | undefined;
  boundary: boolean;
  prepareResult: () => PrepareOutcome;
  sendResult: () => Promise<TerminalPreservationResult>;
  deps: TerminalOperationDeps;
}
function harness(coord: RecoveryCoordinator, generation = 1): H {
  const h: H = {
    sends: [], captures: 0, prepares: [], failure: undefined, late: undefined, boundary: true,
    prepareResult: () => ({ pack: Buffer.from("pack"), range: { tipSha: A, excludeSha: B }, digests: { scan: "s", transport: "t" } }),
    sendResult: async () => ({ kind: "final", outcome: "published", response: {} as never }),
    deps: undefined as never,
  };
  h.deps = {
    enabled: () => coord.terminalPreservationEnabled(),
    store: coord.terminalOperationStore(),
    owner: coord.terminalOperationOwner(RUN, generation),
    failureFirst: () => h.failure,
    universe: async () => ({ kind: "proved", sources: [], roots: [], universeDigest: "u1" }),
    capture: async (onPhase) => {
      h.captures++;
      await onPhase("capturing", { cloneRef: "refs/c", bareRef: "refs/c" });
      await onPhase("captured", { cloneRef: "refs/c", bareRef: "refs/c", commit: A });
      return { kind: "ok", base: A, extraRoots: [], manifest: { ...MANIFEST, generation } };
    },
    aggregate: async ({ base }) => ({ tip: base }),
    prepare: async (range, expect) => { h.prepares.push({ range, expect }); return h.prepareResult(); },
    coverage: () => ({ ok: true }),
    recheck: async () => h.late,
    boundaryValid: () => h.boundary,
    send: async (req) => { h.sends.push(req); return h.sendResult(); },
    now: () => 1_700_000_000_000,
  };
  return h;
}
const input = (mode: "initial" | "resume" = "initial", generation = 1) => ({
  runId: RUN, workerId: "w1", generation, authorityClass: "current_flight" as const, clonePath: "/c", mode,
});
const readRec = async (store: TerminalOperationStore, gen = 1): Promise<TerminalOperationRecord> => {
  const r = await store.read(RUN, gen);
  assert.equal(r.kind, "ok");
  return (r as { record: TerminalOperationRecord }).record;
};
const gate = () => { let open!: () => void; const p = new Promise<void>(r => { open = r; }); return { p, open }; };
const tick = () => new Promise<void>(r => setImmediate(r));
async function until(cond: () => boolean): Promise<void> {
  for (let i = 0; i < 1000 && !cond(); i++) await new Promise<void>(r => setTimeout(r, 5));
  assert.ok(cond(), "condition never held");
}

describe("terminal operation storage", () => {
  it("lives in the sibling root and never in the recovery run directory", async () => {
    const root = mkRoot();
    const recoveryRun = path.join(root, "recovery", RUN);
    fs.mkdirSync(recoveryRun, { recursive: true });
    fs.writeFileSync(path.join(recoveryRun, "cap1.json"), "{}");
    const count = () => fs.readdirSync(recoveryRun).filter(n => n.endsWith(".json")).length;
    const before = count();
    const store = mkCoord(root).terminalOperationStore();
    assert.equal((await store.create(rec())).kind, "created");
    assert.equal((await store.update(RUN, 1, { phase: "captured" })).kind, "ok");
    assert.equal(count(), before);
    assert.deepEqual(fs.readdirSync(recoveryRun).sort(), ["cap1.json"]);
    assert.ok(fs.existsSync(recFile(root)));
    assert.equal(path.dirname(path.dirname(path.dirname(recFile(root)))), root);
    assert.equal(fs.statSync(tpDir(root)).mode & 0o777, 0o700);
    assert.equal(fs.statSync(path.join(root, "terminal-preservation")).mode & 0o777, 0o700);
    assert.equal(fs.statSync(recFile(root)).mode & 0o777, 0o600);
    assert.deepEqual(fs.readdirSync(tpDir(root)), ["g1.json"]);
  });

  it("round-trips, authenticates, and keeps absent, corrupt and oversize distinct", async () => {
    const root = mkRoot();
    const store = mkCoord(root).terminalOperationStore();
    assert.equal((await store.read(RUN, 1)).kind, "absent");
    const created = await store.create(preparedRec());
    assert.equal(created.kind, "created");
    assert.deepEqual(await readRec(store), preparedRec());
    assert.equal((await store.create(rec())).kind, "exists");
    // A second coordinator (same token) reads it; a different token does not.
    assert.deepEqual(await readRec(mkCoord(root).terminalOperationStore()), preparedRec());
    const other = new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
      client: new FakeRecoveryClient(), git: new FakeRecoveryGit(), log: nullLogger(),
      recoveryRoot: path.join(root, "recovery"), workerToken: TOKEN + "x", terminalPreservationTestOverride: {},
    });
    assert.equal((await other.terminalOperationStore().read(RUN, 1)).kind, "corrupt");
    // Tamper: change a field without the MAC.
    const file = recFile(root);
    const good = fs.readFileSync(file, "utf8");
    fs.writeFileSync(file, good.replace('"phase":"prepared"', '"phase":"closed"'));
    assert.equal((await store.read(RUN, 1)).kind, "corrupt");
    fs.writeFileSync(file, "not json");
    assert.equal((await store.read(RUN, 1)).kind, "corrupt");
    // Oversize.
    fs.writeFileSync(file, good + " ".repeat(300 * 1024));
    assert.equal((await store.read(RUN, 1)).kind, "corrupt");
    // A symlinked record file is refused even when its target is a perfectly valid record.
    fs.writeFileSync(path.join(root, "elsewhere.json"), good);
    fs.rmSync(file);
    fs.symlinkSync(path.join(root, "elsewhere.json"), file);
    assert.equal((await store.read(RUN, 1)).kind, "corrupt");
    // Restored bytes read fine again; absent generation is still absent.
    fs.rmSync(file);
    fs.writeFileSync(file, good);
    assert.equal((await store.read(RUN, 1)).kind, "ok");
    assert.equal((await store.read(RUN, 2)).kind, "absent");
  });

  it("refuses an invalid run id or generation before touching the filesystem", async () => {
    const root = mkRoot();
    const store = mkCoord(root).terminalOperationStore();
    assert.equal((await store.update("../evil", 1, { phase: "captured" })).kind, "error");
    assert.equal((await store.update(RUN, 0, { phase: "captured" })).kind, "error");
    assert.equal((await store.create(rec({ runId: "../x" }))).kind, "error");
    assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
  });
});

describe("terminal operation merge", () => {
  it("invocations are append-only and invoked needs a prepared, unblocked, open record", () => {
    const p = preparedRec();
    const a = mustMerge(p, { appendInvocations: [inv("invoked")] });
    const b = mustMerge(a, { appendInvocations: [inv("publish_unknown", "transport")] });
    assert.deepEqual(b.invocations.map(i => i.outcome), ["invoked", "publish_unknown"]);
    assert.deepEqual(p.invocations, []);
    assert.equal(mergeReason(rec(), { appendInvocations: [inv("invoked")] }), "not_prepared");
    assert.equal(mergeReason(preparedRec({ localBlock: ["x"] }), { appendInvocations: [inv("invoked")] }), "local_block");
    assert.equal(mergeReason(preparedRec({ phase: "closed", invocations: [inv("published")] }), { appendInvocations: [inv("refused")] }), "closed");
  });

  it("localBlock and flagged commits only grow, and a conflict adds server_conflict", () => {
    const a = mustMerge(rec(), { addLocalBlock: ["a"], addFlaggedCommits: [A] });
    const b = mustMerge(a, { addLocalBlock: ["b", "a"], addFlaggedCommits: [A, B] });
    assert.deepEqual(b.localBlock, ["a", "b"]);
    assert.deepEqual(b.flaggedCommits, [A, B]);
    const c = mustMerge(mustMerge(preparedRec(), { appendInvocations: [inv("invoked")] }), { appendInvocations: [inv("conflict", "generation_bound")] });
    assert.deepEqual(c.localBlock, ["server_conflict"]);
    assert.equal(c.phase, "prepared");
  });

  it("the phase never moves backward; incomplete only from capture; closed is terminal", () => {
    assert.equal(mergeReason(preparedRec(), { phase: "captured" }), "phase_regression");
    assert.equal(mergeReason(preparedRec(), { phase: "incomplete" }), "incomplete_only_from_capture");
    assert.equal(mergeReason(rec({ phase: "incomplete" }), { phase: "captured" }), "phase_regression");
    assert.equal(mergeReason(rec({ phase: "incomplete" }), { phase: "closed", closedReason: "x" }), "phase_regression");
    assert.equal(mergeReason(rec({ phase: "captured" }), { phase: "prepared" }), "prepared_incomplete");
    assert.equal(mergeReason(preparedRec({ phase: "closed", closedReason: "x" }), { phase: "prepared" }), "closed");
    assert.equal(mustMerge(rec(), { phase: "captured" }).phase, "captured");
    assert.equal(mustMerge(rec({ phase: "captured" }), { phase: "incomplete" }).phase, "incomplete");
    // Closed with no change is idempotent.
    const closed = preparedRec({ phase: "closed", closedReason: "published", invocations: [inv("invoked"), inv("published")] });
    assert.equal(mustMerge(closed, { phase: "closed" }).phase, "closed");
  });

  it("identity fields are immutable once set", () => {
    const p = preparedRec();
    assert.equal(mergeReason(p, { tip: B }), "identity_changed");
    assert.equal(mergeReason(mustMerge(p, { priorTip: C }), { priorTip: B }), "identity_changed");
    assert.equal(mergeReason(p, { range: serializeCheckpointRange({ tipSha: A, excludeSha: C }) }), "identity_changed");
    assert.equal(mergeReason(p, { manifest: { ...MANIFEST, worker_id: "other" } }), "identity_changed");
    assert.equal(mergeReason(p, { scanDigest: "z" }), "identity_changed");
    assert.equal(mustMerge(p, { tip: A }).tip, A);
  });

  it("closes only after a final outcome, or as refused when positively uninvoked", () => {
    assert.equal(mergeReason(preparedRec(), { phase: "closed" }), "close_requires_reason");
    assert.equal(mergeReason(preparedRec({ invocations: [inv("invoked")] }), { phase: "closed", closedReason: "refused" }), "close_requires_final");
    assert.equal(mergeReason(preparedRec({ invocations: [inv("invoked")] }),
      { phase: "closed", closedReason: "r", appendInvocations: [inv("refused", "update_refused")] }), "close_requires_final");
    assert.equal(mergeReason(preparedRec({ invocations: [inv("invoked"), inv("publish_unknown")] }), { phase: "closed", closedReason: "x" }), "close_requires_final");
    assert.equal(mustMerge(preparedRec(), { phase: "closed", closedReason: "secret_found", appendInvocations: [inv("refused", "secret_found")] }).phase, "closed");
    assert.equal(mustMerge(preparedRec({ invocations: [inv("invoked")] }), { phase: "closed", appendInvocations: [inv("recorded")] }).phase, "closed");
  });
});

describe("terminal operation bounds, closure and authority", () => {
  const probes: Array<[string, TerminalDelta]> = [
    ["closedReason over the decoder bound", { closedReason: "x".repeat(300) }],
    ["more verdicts than the decoder allows", { verdicts: Object.fromEntries(Array.from({ length: 65 }, (_, i) => [`v${i}`, "ok"])) }],
    ["an empty localBlock string", { addLocalBlock: [""] }],
  ];
  for (const [name, delta] of probes) {
    it(`${name}: the merge and the store refuse it, write nothing and leave the record readable`, async () => {
      assert.equal(mergeReason(rec(), delta), "bound");
      const root = mkRoot();
      const store = mkCoord(root).terminalOperationStore();
      await store.create(rec());
      const before = fs.readFileSync(recFile(root), "utf8");
      assert.deepEqual(await store.update(RUN, 1, delta), { kind: "rejected", reason: "bound" });
      assert.equal(fs.readFileSync(recFile(root), "utf8"), before);
      assert.deepEqual(fs.readdirSync(tpDir(root)), ["g1.json"]);
      assert.equal((await store.read(RUN, 1)).kind, "ok");
    });
  }

  it("an injected reason is clamped to [a-z0-9_] and a safe length before it is persisted", async () => {
    const root = mkRoot();
    const h = harness(mkCoord(root));
    h.deps.aggregate = async () => ({ refused: "Bad Reason! " + "Z".repeat(600) });
    const res = await runTerminalOperation(h.deps, input());
    assert.equal(res.outcome, "kept");
    const r = await readRec(h.deps.store);
    assert.equal(r.phase, "incomplete");
    assert.match(r.closedReason!, /^[a-z0-9_]{1,200}$/);
    assert.ok(r.closedReason!.startsWith("aggregate_bad_reason_"));
    assert.equal(h.sends.length, 0);
  });

  it("a server refusal reason is clamped before it lands in the invocation history", async () => {
    const h = harness(mkCoord(mkRoot()));
    h.sendResult = async () => ({ kind: "refused", reason: ("Weird Reason " + "y".repeat(600)) as never, httpStatus: 422 });
    await runTerminalOperation(h.deps, input());
    const r = await readRec(h.deps.store);
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "refused"]);
    assert.match(r.invocations[1]!.reason!, /^[a-z0-9_]{1,200}$/);
  });

  it("a closed record accepts no delta at all", () => {
    const closed = preparedRec({ phase: "closed", closedReason: "published", invocations: [inv("invoked"), inv("published")] });
    for (const d of [{ closedReason: "other" }, { verdicts: { x: "y" } }, { tip: B }, { tip: A }, { addLocalBlock: ["x"] },
      { addFlaggedCommits: [A] }, { appendInvocations: [inv("refused")] }, { phase: "prepared" as const }] as TerminalDelta[]) {
      assert.equal(mergeReason(closed, d), "closed", JSON.stringify(d));
    }
    assert.equal(mustMerge(closed, {}).phase, "closed");
    assert.equal(mustMerge(closed, { phase: "closed", addLocalBlock: [], verdicts: {} }).phase, "closed");
  });

  it("a closed secret_found reason alone (no block, no flagged commit) still blocks every send of the run", async () => {
    const root = mkRoot();
    const store = mkCoord(root).terminalOperationStore();
    await store.create(preparedRec());
    await store.create(rec({ generation: 2, phase: "closed", closedReason: "secret_found" }));
    assert.deepEqual(await store.invoke(RUN, 1, "2026-01-01T00:00:00.000Z"), { kind: "blocked", reason: "secret_found" });
    assert.deepEqual((await readRec(store, 1)).invocations, []);
  });

  it("two runs racing the first write both succeed and leave a real shared root", async () => {
    const RUN2 = "22222222-2222-4222-8222-222222222222";
    for (let i = 0; i < 5; i++) {
      const root = mkRoot();
      const a = mkCoord(root).terminalOperationStore(), b = mkCoord(root).terminalOperationStore();
      const [x, y] = await Promise.all([a.create(rec()), b.create(rec({ runId: RUN2 }))]);
      assert.equal(x.kind, "created");
      assert.equal(y.kind, "created");
      const st = fs.lstatSync(path.join(root, "terminal-preservation"));
      assert.ok(st.isDirectory() && !st.isSymbolicLink());
      assert.equal((await a.read(RUN, 1)).kind, "ok");
      assert.equal((await b.read(RUN2, 1)).kind, "ok");
    }
  });

  it("a symlinked shared root is never written through", async () => {
    const root = mkRoot();
    fs.mkdirSync(path.join(root, "elsewhere"));
    fs.symlinkSync(path.join(root, "elsewhere"), path.join(root, "terminal-preservation"));
    const res = await mkCoord(root).terminalOperationStore().create(rec());
    assert.notEqual(res.kind, "created");
    assert.deepEqual(fs.readdirSync(path.join(root, "elsewhere")), []);
  });

  const lostScope = (coord: RecoveryCoordinator, live: () => boolean) =>
    <T>(fn: () => Promise<T>): Promise<T> =>
      (coord as unknown as { predecessorScope: { run: <R>(s: unknown, f: () => R) => R } }).predecessorScope.run({ ended: false, live }, fn);

  it("a lost predecessor scope writes nothing: not on create, not on invoke", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const store = coord.terminalOperationStore();
    const created = await lostScope(coord, () => false)(() => store.create(rec()));
    assert.deepEqual(created, { kind: "error", reason: "authority_lost" });
    assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
    await store.create(preparedRec());
    const before = fs.readFileSync(recFile(root), "utf8");
    const res = await lostScope(coord, () => false)(() => store.invoke(RUN, 1, "2026-01-01T00:00:00.000Z"));
    assert.deepEqual(res, { kind: "error", reason: "authority_lost" });
    assert.equal(fs.readFileSync(recFile(root), "utf8"), before);
  });

  it("authority lost between the temp write and the rename publishes nothing and removes the temp file", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    let calls = 0;
    const res = await lostScope(coord, () => ++calls < 2)(() => coord.terminalOperationStore().create(rec()));
    assert.deepEqual(res, { kind: "error", reason: "authority_lost" });
    assert.deepEqual(fs.readdirSync(tpDir(root)), []);
  });
});

describe("terminal operation resume identity", () => {
  async function leftPrepared(): Promise<H> {
    const h = harness(mkCoord(mkRoot()));
    h.boundary = false;
    await runTerminalOperation(h.deps, input());
    h.boundary = true;
    return h;
  }
  const cases: Array<[string, (h: H) => void, { workerId?: string; priorTip?: string }]> = [
    ["scan digest", h => { h.prepareResult = () => ({ pack: Buffer.from("p"), range: { tipSha: A, excludeSha: B }, digests: { scan: "other", transport: "t" } }); }, {}],
    ["transport digest", h => { h.prepareResult = () => ({ pack: Buffer.from("p"), range: { tipSha: A, excludeSha: B }, digests: { scan: "s", transport: "other" } }); }, {}],
    ["range", h => { h.prepareResult = () => ({ pack: Buffer.from("p"), range: { tipSha: A, excludeSha: C }, digests: { scan: "s", transport: "t" } }); }, {}],
    ["manifest worker", () => undefined, { workerId: "w2" }],
    ["prior tip", () => undefined, { priorTip: C }],
  ];
  for (const [name, arrange, over] of cases) {
    it(`a ${name} mismatch on resume sends nothing and invokes nothing`, async () => {
      const h = await leftPrepared();
      arrange(h);
      const resumeInput = { ...input("resume"), ...over };
      const res = await runTerminalOperation(h.deps, resumeInput);
      assert.deepEqual(res, { outcome: "kept", reason: "digest_mismatch" });
      assert.equal(h.sends.length, 0);
      assert.deepEqual((await readRec(h.deps.store)).invocations, []);
    });
  }
});

describe("terminal operation engine", () => {
  it("publishes through the approved order and closes the record", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const h = harness(coord);
    const res = await runTerminalOperation(h.deps, input());
    assert.deepEqual(res, { outcome: "published", tip: A, manifest: { ...MANIFEST, generation: 1 } });
    assert.equal(h.sends.length, 1);
    assert.equal(h.sends[0]!.tip, A);
    assert.equal(h.sends[0]!.pack, Buffer.from("pack").toString("base64"));
    const r = await readRec(h.deps.store);
    assert.equal(r.phase, "closed");
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "published"]);
    assert.equal(r.range, RANGE);
    // A duplicate in the lane takes the idempotent path: no second send.
    const again = await runTerminalOperation(h.deps, input());
    assert.equal(again.outcome, "published");
    assert.equal(h.sends.length, 1);
  });

  it("gate off: engine and rediscovery do nothing and create nothing", async () => {
    const root = mkRoot();
    const coord = mkCoord(root, false);
    const h = harness(coord);
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "disabled" });
    assert.equal(h.captures, 0);
    assert.equal(h.sends.length, 0);
    assert.deepEqual(await coord.resumeTerminalOperations(), { resumePending: [], incomplete: 0, corrupt: 0, truncated: false });
    assert.equal((await coord.terminalOperationStore().create(rec())).kind, "disabled");
    assert.equal((await coord.terminalOperationStore().update(RUN, 1, { phase: "captured" })).kind, "disabled");
    assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
    assert.throws(() => coord.terminalOperationOwner(RUN, 1)(async () => 1));
  });

  for (const reason of ["push_secret_blocked", "worker_residue_blocked"]) {
    it(`failure-first ${reason}: zero writes, zero sends`, async () => {
      const root = mkRoot();
      const h = harness(mkCoord(root));
      h.failure = reason;
      assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason });
      assert.equal(h.captures, 0);
      assert.equal(h.sends.length, 0);
      assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
    });
  }

  it("a late failure before any invocation closes refused/local_failure_late and never sends", async () => {
    const root = mkRoot();
    const h = harness(mkCoord(root));
    h.late = "secret_remediation_pending";
    const res = await runTerminalOperation(h.deps, input());
    assert.equal(res.outcome, "kept");
    assert.equal(h.sends.length, 0);
    const r = await readRec(h.deps.store);
    assert.equal(r.phase, "closed");
    assert.equal(r.closedReason, "local_failure_late");
    assert.deepEqual(r.invocations.map(i => [i.outcome, i.reason]), [["refused", "local_failure_late"]]);
    assert.ok(r.localBlock.includes("local_failure_late"));
  });

  it("the same late failure after an earlier invocation latches without closing", async () => {
    const root = mkRoot();
    const h = harness(mkCoord(root));
    h.sendResult = async () => ({ kind: "unknown", cause: "transport" });
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "unknown", reason: "transport" });
    h.late = "worker_residue";
    const res = await runTerminalOperation(h.deps, input("resume"));
    assert.equal(res.outcome, "kept");
    assert.equal(h.sends.length, 1);
    const r = await readRec(h.deps.store);
    assert.equal(r.phase, "prepared");
    assert.ok(r.localBlock.includes("local_failure_late"));
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "publish_unknown"]);
  });

  it("the first send of a positively uninvoked prepared record is allowed through resume", async () => {
    const root = mkRoot();
    const h = harness(mkCoord(root));
    h.boundary = false;
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "boundary_invalid" });
    assert.equal(h.sends.length, 0);
    assert.deepEqual((await readRec(h.deps.store)).invocations, []);
    // Initial mode never adopts an existing non-final record.
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "existing_record" });
    h.boundary = true;
    const res = await runTerminalOperation(h.deps, input("resume"));
    assert.equal(res.outcome, "published");
    assert.equal(h.sends.length, 1);
    // Resume reran preparation over the persisted range with the persisted digests.
    assert.deepEqual(h.prepares.at(-1), { range: { tipSha: A, excludeSha: B }, expect: { scan: "s", transport: "t" } });
  });

  it("resume refuses a missing or non-prepared record", async () => {
    const h = harness(mkCoord(mkRoot()));
    assert.deepEqual(await runTerminalOperation(h.deps, input("resume")), { outcome: "kept", reason: "not_resumable" });
    assert.equal(h.sends.length, 0);
  });

  for (const reason of ["generation_bound", "prior_tip_mismatch"] as const) {
    it(`${reason} after invoke records a conflict and a local block, never closes`, async () => {
      const h = harness(mkCoord(mkRoot()));
      h.sendResult = async () => ({ kind: "refused", reason, httpStatus: 409 });
      assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason });
      const r = await readRec(h.deps.store);
      assert.equal(r.phase, "prepared");
      assert.ok(r.localBlock.includes("server_conflict"));
      assert.deepEqual(r.invocations.map(i => [i.outcome, i.reason]), [["invoked", undefined], ["conflict", reason]]);
      // And it blocks any later send.
      assert.equal((await runTerminalOperation(h.deps, input("resume"))).outcome, "kept");
      assert.equal(h.sends.length, 1);
    });
  }

  it("another server refusal after invoke appends refused but never closes", async () => {
    const h = harness(mkCoord(mkRoot()));
    h.sendResult = async () => ({ kind: "refused", reason: "update_refused", httpStatus: 422 });
    await runTerminalOperation(h.deps, input());
    const r = await readRec(h.deps.store);
    assert.equal(r.phase, "prepared");
    assert.equal(r.localBlock.length, 0);
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "refused"]);
  });

  it("a latch recorded during an in-flight send stops the next send", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const h = harness(coord);
    const sendGate = gate();
    h.sendResult = async () => { await sendGate.p; return { kind: "unknown", cause: "transport" }; };
    const running = runTerminalOperation(h.deps, input());
    await until(() => h.sends.length === 1);
    // The latch writer takes only the journal lock, so it lands while the owner lane is held by the send.
    const latched = await h.deps.store.addLocalBlock(RUN, 1, ["secret_found"], [C]);
    assert.equal(latched.kind, "ok");
    sendGate.open();
    assert.deepEqual(await running, { outcome: "unknown", reason: "transport" });
    const r = await readRec(h.deps.store);
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "publish_unknown"]);
    assert.deepEqual(r.localBlock, ["secret_found"]);
    h.sendResult = async () => ({ kind: "final", outcome: "published", response: {} as never });
    assert.deepEqual(await runTerminalOperation(h.deps, input("resume")), { outcome: "kept", reason: "local_block" });
    assert.equal(h.sends.length, 1);
  });

  it("writers from the same older record in either order keep both the outcome and the block", async () => {
    for (const order of ["outcome-first", "block-first"] as const) {
      const root = mkRoot();
      const coord = mkCoord(root);
      const store = coord.terminalOperationStore();
      await store.create(preparedRec({ invocations: [inv("invoked")] }));
      const outcome = () => store.update(RUN, 1, { appendInvocations: [inv("publish_unknown", "transport")] });
      const block = () => store.addLocalBlock(RUN, 1, ["secret_found"], [C]);
      if (order === "outcome-first") { await outcome(); await block(); } else { await block(); await outcome(); }
      const r = await readRec(store);
      assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "publish_unknown"], order);
      assert.deepEqual(r.localBlock, ["secret_found"], order);
      assert.deepEqual(r.flaggedCommits, [C], order);
    }
  });

  it("resume versus an in-lane duplicate: the owner serializes and the second takes the idempotent path", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const h = harness(coord);
    h.boundary = false;
    await runTerminalOperation(h.deps, input());
    h.boundary = true;
    const sendGate = gate();
    h.sendResult = async () => { await sendGate.p; return { kind: "final", outcome: "published", response: {} as never }; };
    const first = runTerminalOperation(h.deps, input("resume"));
    await until(() => h.sends.length === 1);
    const second = runTerminalOperation(h.deps, input("resume"));
    await tick(); await tick();
    assert.equal(h.sends.length, 1);
    sendGate.open();
    assert.equal((await first).outcome, "published");
    assert.equal((await second).outcome, "published");
    assert.equal(h.sends.length, 1);
    assert.deepEqual((await readRec(h.deps.store)).invocations.map(i => i.outcome), ["invoked", "published"]);
  });

  it("replay versus rediscovery in either order overwrites neither the uncertainty nor a newer block", async () => {
    for (const order of ["replay-first", "rediscovery-first", "concurrent"] as const) {
      const root = mkRoot();
      const store = mkCoord(root).terminalOperationStore();
      await store.create(preparedRec({ invocations: [inv("invoked")] }));
      const fresh = mkCoord(root);
      const replay = () => store.update(RUN, 1, { appendInvocations: [inv("publish_unknown", "transport")] });
      const block = () => store.addLocalBlock(RUN, 1, ["secret_found"]);
      const rediscover = () => fresh.resumeTerminalOperations();
      if (order === "replay-first") { await replay(); await rediscover(); await block(); }
      else if (order === "rediscovery-first") { await rediscover(); await block(); await replay(); }
      else await Promise.all([replay(), rediscover(), block()]);
      const r = await readRec(store);
      assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "publish_unknown"], order);
      assert.deepEqual(r.localBlock, ["secret_found"], order);
      assert.equal(r.phase, "prepared", order);
    }
  });

  it("a secret finding from preparation latches, closes the uninvoked record, survives restart and blocks later runs", async () => {
    const root = mkRoot();
    const h = harness(mkCoord(root));
    h.prepareResult = () => ({ reason: "secret_found", flaggedCommits: [C] });
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    assert.equal(h.sends.length, 0);
    // New coordinator instance: nothing in memory.
    const restarted = mkCoord(root);
    const r = await readRec(restarted.terminalOperationStore());
    assert.deepEqual(r.localBlock, ["secret_found"]);
    assert.deepEqual(r.flaggedCommits, [C]);
    assert.equal(r.phase, "closed");
    assert.equal(r.closedReason, "secret_found");
    // A later generation whose scanner now says clean still never sends.
    const h2 = harness(restarted, 2);
    const res = await runTerminalOperation(h2.deps, input("initial", 2));
    assert.equal(res.outcome, "kept");
    assert.equal(h2.sends.length, 0);
    assert.deepEqual((await readRec(h2.deps.store, 2)).invocations, []);
    // A resume of the same record is also refused.
    assert.equal((await runTerminalOperation(harness(restarted).deps, input("resume"))).outcome, "kept");
  });

  it("a malformed or oversized flagged-commit list never costs the secret latch", async () => {
    const root = mkRoot();
    const h = harness(mkCoord(root));
    const many = Array.from({ length: 300 }, (_, i) => i.toString(16).padStart(40, "a"));
    h.prepareResult = () => ({ reason: "secret_found", flaggedCommits: ["abc123", ...many] });
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    assert.equal(h.sends.length, 0);
    const r = await readRec(mkCoord(root).terminalOperationStore());
    assert.deepEqual(r.localBlock, ["secret_found"]);
    assert.ok(r.flaggedCommits.length <= 256 && !r.flaggedCommits.includes("abc123"));
    assert.equal(r.phase, "closed");
  });

  it("a corrupt sibling record blocks a send (fail closed)", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const store = coord.terminalOperationStore();
    await store.create(rec({ generation: 3, phase: "closed", closedReason: "x" }));
    fs.writeFileSync(path.join(tpDir(root), "g3.json"), "garbage");
    const h = harness(coord);
    const res = await runTerminalOperation(h.deps, input());
    assert.equal(res.outcome, "kept");
    assert.equal(h.sends.length, 0);
  });
});

describe("terminal operation final authority", () => {
  const SNAP = { commit: C, tree: B };
  const SNAP_MANIFEST: TerminalManifest = {
    ...MANIFEST, roots: [...MANIFEST.roots, { source_id: "s1:snapshot", attempt_id: "at1", commit: C, tree: B }],
  };

  it("a block that lands while the send is in flight keeps the receipt but returns no authority", async () => {
    const coord = mkCoord(mkRoot());
    const h = harness(coord);
    const sendGate = gate();
    h.sendResult = async () => { await sendGate.p; return { kind: "final", outcome: "published", response: {} as never }; };
    const running = runTerminalOperation(h.deps, input());
    await until(() => h.sends.length === 1);
    assert.equal((await h.deps.store.addLocalBlock(RUN, 1, ["secret_found"])).kind, "ok");
    sendGate.open();
    assert.deepEqual(await running, { outcome: "kept", reason: "secret_found" });
    const r = await readRec(h.deps.store);
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "published"]);
    assert.equal(r.phase, "closed");
    // The closed record read on the idempotent path gives no authority either.
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    assert.equal(h.sends.length, 1);
  });

  it("a latch that lands right after the final outcome is written returns no authority either", async () => {
    const coord = mkCoord(mkRoot());
    const h = harness(coord);
    const real = h.deps.store;
    h.deps.store = { ...real, update: async (r, g, d) => {
      const out = await real.update(r, g, d);
      if (d.phase === "closed") h.failure = "secret_found";
      return out;
    }, read: real.read.bind(real), create: real.create.bind(real), addLocalBlock: real.addLocalBlock.bind(real), invoke: real.invoke.bind(real) };
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    assert.deepEqual((await readRec(real)).invocations.map(i => i.outcome), ["invoked", "published"]);
  });

  for (const [label, flip, expected] of [
    ["the boundary lapses", (h: H) => { h.boundary = false; }, { outcome: "kept", reason: "boundary_lost" }],
    ["a failure latches", (h: H) => { h.failure = "secret_found"; }, { outcome: "kept", reason: "secret_found" }],
  ] as const) {
    it(`closed-record replay: ${label} while runBlock is pending returns no head authority`, async () => {
      const coord = mkCoord(mkRoot());
      const h = harness(coord);
      assert.equal((await runTerminalOperation(h.deps, input())).outcome, "published");
      const real = h.deps.store;
      h.deps.store = { ...real, runBlock: async (id: string) => { const out = await real.runBlock(id); flip(h); return out; },
        read: real.read.bind(real), create: real.create.bind(real), update: real.update.bind(real),
        addLocalBlock: real.addLocalBlock.bind(real), invoke: real.invoke.bind(real) };
      assert.deepEqual(await runTerminalOperation(h.deps, input()), expected);
      assert.equal(h.sends.length, 1, "the replay never sends again");
    });
  }

  it("live final: a failure that latches while the post-send runBlock is pending returns no head authority", async () => {
    const coord = mkCoord(mkRoot());
    const h = harness(coord);
    const real = h.deps.store;
    let flipped = false;
    h.deps.store = { ...real, runBlock: async (id: string) => {
      const out = await real.runBlock(id);
      // Only the post-send check: the latch lands after the factual receipt was recorded.
      if (h.sends.length > 0 && !flipped) { flipped = true; h.failure = "secret_found"; }
      return out;
    }, read: real.read.bind(real), create: real.create.bind(real), update: real.update.bind(real),
      addLocalBlock: real.addLocalBlock.bind(real), invoke: real.invoke.bind(real) };
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    assert.equal(flipped, true, "the latch landed during the post-send runBlock");
    assert.equal(h.sends.length, 1);
    assert.deepEqual((await readRec(real)).invocations.map(i => i.outcome), ["invoked", "published"], "the receipt stays factual");
  });

  it("a boundary lost while invoke is in flight sends nothing, latches no block, and a later resume may send", async () => {
    let lapses = 1;
    const coord = mkCoord(mkRoot());
    const h = harness(coord);
    const real = h.deps.store;
    h.deps.store = { ...real, invoke: async (r, g, at) => { const out = await real.invoke(r, g, at); if (lapses-- > 0) h.boundary = false; return out; },
      read: real.read.bind(real), create: real.create.bind(real), update: real.update.bind(real), addLocalBlock: real.addLocalBlock.bind(real) };
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "boundary_lost" });
    assert.equal(h.sends.length, 0);
    const r = await readRec(real);
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked"]);
    assert.deepEqual(r.localBlock, [], "a lapse before any send must not block the run");
    h.boundary = true;
    await runTerminalOperation(h.deps, input("resume"));
    assert.equal(h.sends.length, 1, "the resume re-proves and sends the identical request once");
  });

  it("a boundary lost during the send records the receipt but returns no authority", async () => {
    const coord = mkCoord(mkRoot());
    const h = harness(coord);
    h.sendResult = async () => { h.boundary = false; return { kind: "final", outcome: "recorded", response: {} as never }; };
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "boundary_lost" });
    const r = await readRec(h.deps.store);
    assert.deepEqual(r.invocations.map(i => i.outcome), ["invoked", "recorded"]);
    assert.equal(r.phase, "closed");
  });

  it("the capture's snapshot reaches the coverage check on the first run and, from the record, on resume", async () => {
    const coord = mkCoord(mkRoot());
    const h = harness(coord);
    const seen: Array<{ commit: string; tree: string } | undefined> = [];
    h.deps.coverage = (_u, _m, snapshot) => { seen.push(snapshot); return { ok: true }; };
    h.deps.capture = async onPhase => {
      await onPhase("capturing", { cloneRef: "refs/c", bareRef: "refs/c" });
      await onPhase("captured", { cloneRef: "refs/c", bareRef: "refs/c", commit: C });
      return { kind: "ok", base: C, extraRoots: [], manifest: SNAP_MANIFEST, snapshot: SNAP };
    };
    h.deps.aggregate = async ({ base }) => ({ tip: base });
    h.sendResult = async () => ({ kind: "unknown", cause: "transport" });
    await runTerminalOperation(h.deps, input());
    assert.deepEqual(seen, [SNAP, SNAP]);
    await runTerminalOperation(h.deps, input("resume"));
    assert.deepEqual(seen.slice(2), [SNAP, SNAP]);
  });
});

describe("terminal operation rediscovery", () => {
  it("runs on the boot recovery pass when the gate is on, and not at all when it is off", async () => {
    const root = mkRoot();
    const writer = mkCoord(root).terminalOperationStore();
    await writer.create(rec({ generation: 1 }));
    await mkCoord(root, false).resumePending(undefined, []);
    assert.equal((await readRec(writer, 1)).phase, "capturing", "gate off: the boot pass leaves terminal records alone");
    await mkCoord(root).resumePending(undefined, []);
    assert.equal((await readRec(writer, 1)).phase, "incomplete", "gate on: the boot pass rediscovers terminal records");
  });


  it("restores every phase from disk with a new coordinator, never sending or touching git", async () => {
    const root = mkRoot();
    const writer = mkCoord(root).terminalOperationStore();
    await writer.create(rec({ generation: 1 }));
    await writer.create(rec({ generation: 2 }));
    await writer.update(RUN, 2, { phase: "captured" });
    await writer.create(rec({ generation: 3 }));
    await writer.update(RUN, 3, { phase: "prepared", manifest: MANIFEST, tip: A, range: RANGE, scanDigest: "s", transportDigest: "t" });
    await writer.create(rec({ generation: 4 }));
    await writer.update(RUN, 4, { phase: "prepared", manifest: MANIFEST, tip: A, range: RANGE, scanDigest: "s", transportDigest: "t" });
    await writer.update(RUN, 4, { appendInvocations: [inv("invoked")] });
    await writer.create(preparedRec({ generation: 5, invocations: [inv("invoked"), inv("published")], phase: "closed", closedReason: "published" }));
    await writer.create(preparedRec({ generation: 6, localBlock: ["secret_found"] }));
    await writer.create(rec({ generation: 7 }));
    fs.writeFileSync(path.join(tpDir(root), "g7.json"), "garbage");
    const before = fs.readFileSync(path.join(tpDir(root), "g5.json"), "utf8");

    const spy: string[] = [];
    const fresh = mkCoord(root, true, spy);
    const res = await fresh.resumeTerminalOperations();
    assert.equal(res.incomplete, 2);
    assert.equal(res.corrupt, 1);
    assert.deepEqual(res.resumePending.map(p => p.generation), [3, 4]);
    assert.deepEqual(spy, [], "rediscovery must not call git or the client");
    const store = fresh.terminalOperationStore();
    for (const g of [1, 2]) {
      const r = await readRec(store, g);
      assert.equal(r.phase, "incomplete");
      assert.equal(r.cloneRef, "refs/uzi-terminal-local/x/g1");
      assert.equal(r.bareRef, "refs/uzi-terminal-local/x/g1");
      assert.equal(r.clonePath, "/c");
    }
    assert.equal((await readRec(store, 3)).phase, "prepared");
    assert.deepEqual((await readRec(store, 4)).invocations.map(i => i.outcome), ["invoked"]);
    assert.equal(fs.readFileSync(path.join(tpDir(root), "g5.json"), "utf8"), before);
    assert.equal(fs.readFileSync(path.join(tpDir(root), "g7.json"), "utf8"), "garbage");
    // Idempotent on a second pass.
    const again = await fresh.resumeTerminalOperations();
    assert.equal(again.incomplete, 0);
    assert.deepEqual(again.resumePending.map(p => p.generation), [3, 4]);
  });

  it("processes the lowest generations first when a run has more records than the per-run cap", async () => {
    const root = mkRoot();
    const store = mkCoord(root).terminalOperationStore();
    for (let g = 1; g <= 70; g++) await store.create(preparedRec({ generation: g }));
    const res = await mkCoord(root).resumeTerminalOperations();
    assert.equal(res.truncated, true);
    assert.deepEqual(res.resumePending.map(p => p.generation), Array.from({ length: 64 }, (_, i) => i + 1));
  });

  it("does nothing when the root is absent", async () => {
    const root = mkRoot();
    assert.deepEqual(await mkCoord(root).resumeTerminalOperations(), { resumePending: [], incomplete: 0, corrupt: 0, truncated: false });
    assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
  });

  it("skips a capture-phase record whose lane is held by a live operation", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    await coord.terminalOperationStore().create(rec());
    const lane = gate();
    const held = coord.terminalOperationOwner(RUN, 1)(async () => { await lane.p; });
    await tick();
    const res = await coord.resumeTerminalOperations();
    assert.equal(res.incomplete, 0);
    assert.equal((await readRec(coord.terminalOperationStore())).phase, "capturing");
    lane.open();
    await held;
  });

  describe("journal-lock waits are bounded", () => {
    const mkNowCoord = (root: string, now: () => number): RecoveryCoordinator => new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
      client: new FakeRecoveryClient(), git: new FakeRecoveryGit(), log: nullLogger(), recoveryRoot: path.join(root, "recovery"),
      workerToken: TOKEN, terminalPreservationTestOverride: {}, now,
    });
    const settle = () => new Promise<void>(r => setTimeout(r, 100));

    it("a read blocked behind the journal lock gives up when the caller signal fires, and nothing is written after release", async () => {
      const root = mkRoot();
      const coord = mkCoord(root);
      await coord.terminalOperationStore().create(rec());
      const g = gate();
      const held = coord.withRestoreJournalLock(RUN, () => g.p);
      try {
        await tick();
        const t0 = Date.now();
        const res = await coord.resumeTerminalOperations(AbortSignal.timeout(200));
        assert.ok(Date.now() - t0 < 2000, "returned promptly");
        assert.equal(res.truncated, true);
        assert.equal(res.incomplete, 0);
      } finally { g.open(); await held; }
      await settle();
      assert.equal((await readRec(coord.terminalOperationStore())).phase, "capturing");
    });

    it("the remaining 30 s budget bounds the wait when the caller passes no signal", async () => {
      const root = mkRoot();
      let calls = 0;
      const coord = mkNowCoord(root, () => (calls++ === 0 ? Date.now() : Date.now() + 29_900));
      await coord.terminalOperationStore().create(rec());
      const g = gate();
      const held = coord.withRestoreJournalLock(RUN, () => g.p);
      try {
        await tick();
        const t0 = Date.now();
        const res = await coord.resumeTerminalOperations();
        assert.ok(Date.now() - t0 < 2000, "returned promptly");
        assert.equal(res.truncated, true);
      } finally { g.open(); await held; }
      await settle();
      assert.equal((await readRec(coord.terminalOperationStore())).phase, "capturing");
    });

    it("an update blocked behind the journal lock gives up and never writes after release", async () => {
      const root = mkRoot();
      const coord = mkCoord(root);
      const real = coord.terminalOperationStore();
      await real.create(rec());
      const g = gate();
      let held: Promise<unknown> | undefined;
      coord.terminalOperationStore = () => ({
        ...real,
        read: async (runId, generation, opts) => {
          const r = await real.read(runId, generation, opts);
          held = coord.withRestoreJournalLock(RUN, () => g.p);
          return r;
        },
      });
      try {
        const t0 = Date.now();
        const res = await coord.resumeTerminalOperations(AbortSignal.timeout(200));
        assert.ok(Date.now() - t0 < 2000, "returned promptly");
        assert.equal(res.truncated, true);
        assert.equal(res.incomplete, 0);
      } finally { g.open(); await held; }
      await settle();
      assert.equal((await readRec(real)).phase, "capturing");
    });

    it("a read failure that is not a forfeited wait rejects instead of reading as truncated", async () => {
      const root = mkRoot();
      const coord = mkCoord(root);
      const real = coord.terminalOperationStore();
      await real.create(rec());
      coord.terminalOperationStore = () => ({
        ...real,
        read: async () => { throw new Error("injected lock failure"); },
      });
      await assert.rejects(() => coord.resumeTerminalOperations(), /injected lock failure/);
      assert.equal((await readRec(real)).phase, "capturing");
    });
  });
});

describe("terminal record byte bound on write", () => {
  const bigManifest = (n: number, unit: string, len: number): TerminalManifest => ({
    ...MANIFEST, roots: Array.from({ length: n }, (_, i) => ({ source_id: `${i}`.padStart(4, "0") + unit.repeat(len - 4), attempt_id: "at1", commit: A, tree: B })),
  });
  // 80 roots x 4096 ASCII chars = about 320 KiB serialized, over the 256 KiB read bound.
  const oversized = (): TerminalOperationRecord => rec({ manifest: bigManifest(80, "a", 4096) });
  // Control chars: 1 UTF-16 unit each but 6 bytes JSON-escaped; 50 x 4000 = 200000 chars, under the limit as a count.
  const controlHeavy = (): TerminalOperationRecord => rec({ manifest: bigManifest(50, "\u0001", 4000) });
  // Astral chars: 2 units each, 4 bytes; 5-unit-safe: 2000 chars = 4000 units, 8000 bytes.
  const astralHeavy = (): TerminalOperationRecord => rec({ manifest: bigManifest(70, "\u{1F600}", 2000) });

  for (const [name, make] of [["ascii", oversized], ["control-char", controlHeavy], ["astral", astralHeavy]] as const) {
    it(`an oversized ${name} create is an error that creates no directory or file`, async () => {
      const root = mkRoot();
      const store = mkCoord(root).terminalOperationStore();
      const r = make();
      assert.ok(Buffer.byteLength(JSON.stringify(r), "utf8") > 256 * 1024);
      const res = await store.create(r);
      assert.equal(res.kind, "error");
      assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
    });
  }

  it("the character count of the control-char record stays within the limit while its bytes exceed it", () => {
    const r = controlHeavy();
    const chars = r.manifest!.roots.reduce((n, x) => n + x.source_id.length, 0);
    assert.ok(chars <= 256 * 1024);
    assert.ok(Buffer.byteLength(JSON.stringify(r), "utf8") > 256 * 1024);
  });

  // BMP multibyte chars JSON does not escape: the serialized UTF-16 length is within the limit while the UTF-8 bytes are not,
  // so only a byte-based bound (not a string-length bound) refuses it.
  const multibyte = (unit: string): TerminalManifest => bigManifest(50, unit, 4000);
  const mac = "0".repeat(64);
  const assertLengthWithinBytesOver = (r: TerminalOperationRecord) => {
    const serialized = JSON.stringify({ ...r, mac });
    assert.ok(serialized.length <= 256 * 1024, `serialized length ${serialized.length} is within the limit`);
    assert.ok(Buffer.byteLength(serialized, "utf8") > 256 * 1024, "its UTF-8 bytes exceed the limit");
  };

  for (const [name, unit] of [["euro (3 bytes)", "\u20AC"], ["e-acute (2 bytes)", "\u00E9"]] as const) {
    it(`a ${name} create whose string length fits but whose bytes exceed the limit is an error that creates no file`, async () => {
      const root = mkRoot();
      const store = mkCoord(root).terminalOperationStore();
      const r = rec({ manifest: multibyte(unit) });
      assertLengthWithinBytesOver(r);
      assert.equal((await store.create(r)).kind, "error");
      assert.equal(fs.existsSync(path.join(root, "terminal-preservation")), false);
    });

    it(`a ${name} update whose string length fits but whose bytes exceed the limit leaves the prior file byte-identical`, async () => {
      const root = mkRoot();
      const store = mkCoord(root).terminalOperationStore();
      assert.equal((await store.create(rec())).kind, "created");
      const before = fs.readFileSync(recFile(root));
      assertLengthWithinBytesOver(rec({ manifest: multibyte(unit) }));
      assert.equal((await store.update(RUN, 1, { manifest: multibyte(unit) })).kind, "error");
      assert.deepEqual(fs.readFileSync(recFile(root)), before);
      assert.equal((await store.read(RUN, 1)).kind, "ok");
      assert.deepEqual(fs.readdirSync(tpDir(root)), ["g1.json"]);
    });
  }

  it("an oversized update is an error that leaves the prior file byte-identical and readable", async () => {
    const root = mkRoot();
    const store = mkCoord(root).terminalOperationStore();
    assert.equal((await store.create(rec())).kind, "created");
    const before = fs.readFileSync(recFile(root));
    const res = await store.update(RUN, 1, { manifest: controlHeavy().manifest! });
    assert.equal(res.kind, "error");
    assert.deepEqual(fs.readFileSync(recFile(root)), before);
    assert.equal((await store.read(RUN, 1)).kind, "ok");
    assert.deepEqual(fs.readdirSync(tpDir(root)), ["g1.json"]);
  });
});

describe("run-wide block gates head authority", () => {
  it("a block latched on another generation during the final send keeps the receipt but yields no head, now and on replay", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const h = harness(coord);
    assert.equal((await h.deps.store.create(rec({ generation: 2 }))).kind, "created");
    h.sendResult = async () => {
      assert.equal((await h.deps.store.addLocalBlock(RUN, 2, ["secret_found"])).kind, "ok");
      return { kind: "final", outcome: "published", response: {} as never };
    };
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    const r = await readRec(h.deps.store);
    assert.equal(r.phase, "closed");
    assert.equal(r.closedReason, "published");
    assert.deepEqual(await runTerminalOperation(h.deps, input()), { outcome: "kept", reason: "secret_found" });
    assert.equal(h.sends.length, 1);
  });
});

describe("terminal invocation bound leaves room for the final outcome", () => {
  const alternating = (n: number) =>
    Array.from({ length: n }, (_, i) => (i % 2 === 0 ? inv("invoked") : inv("publish_unknown", "transport")));

  it("merge refuses a 255th-entry invoked as bound", () => {
    assert.equal(mergeReason(preparedRec({ invocations: alternating(255) }), { appendInvocations: [inv("invoked")] }), "bound");
  });

  it("merge admits invoked at 254 and the final outcome then closes the record at 256", () => {
    const a = mustMerge(preparedRec({ invocations: alternating(254) }), { appendInvocations: [inv("invoked")] });
    assert.equal(a.invocations.length, 255);
    const b = mustMerge(a, { phase: "closed", appendInvocations: [inv("published")] });
    assert.equal(b.invocations.length, 256);
    assert.equal(b.phase, "closed");
  });

  it("the store refuses to invoke at 255 and the engine never sends", async () => {
    const h = harness(mkCoord(mkRoot()));
    assert.equal((await h.deps.store.create(preparedRec({ invocations: alternating(255) }))).kind, "created");
    const res = await h.deps.store.invoke(RUN, 1, "2026-01-01T00:00:00.000Z");
    assert.equal(res.kind, "rejected");
    assert.deepEqual(res, { kind: "rejected", reason: "bound" });
    await runTerminalOperation(h.deps, input("resume"));
    assert.equal(h.sends.length, 0);
    assert.equal((await readRec(h.deps.store)).invocations.length, 255);
  });

  it("the store invokes at 254 and records a closing final outcome", async () => {
    const store = mkCoord(mkRoot()).terminalOperationStore();
    assert.equal((await store.create(preparedRec({ invocations: alternating(254) }))).kind, "created");
    assert.equal((await store.invoke(RUN, 1, "2026-01-01T00:00:00.000Z")).kind, "invoked");
    const closed = await store.update(RUN, 1, { phase: "closed", appendInvocations: [inv("published")] });
    assert.equal(closed.kind, "ok");
    assert.equal((await readRec(store)).invocations.length, 256);
  });
});

describe("terminal record read never blocks on a non-regular file", () => {
  it("a FIFO record reads as corrupt promptly and releases the restore journal lock", async () => {
    const root = mkRoot();
    const coord = mkCoord(root);
    const dir = tpDir(root);
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    const fifo = recFile(root);
    // Fail (never skip) when mkfifo is unavailable.
    execFileSync("mkfifo", [fifo]);
    let timer: NodeJS.Timeout | undefined;
    const bound = <T>(p: Promise<T>, what: string) => Promise.race([
      p, new Promise<never>((_, rej) => { timer = setTimeout(() => rej(new Error(`${what} hung on the FIFO`)), 2000); }),
    ]).finally(() => clearTimeout(timer));
    try {
      const read = await bound(coord.terminalOperationStore().read(RUN, 1), "terminal read");
      assert.deepEqual(read, { kind: "corrupt" });
      assert.equal(await bound(coord.withRestoreJournalLock(RUN, async () => "ok"), "restore journal lock"), "ok");
    } finally {
      // Unblock any thread-pool open still parked on the FIFO so the process can exit.
      try { fs.closeSync(fs.openSync(fifo, fs.constants.O_RDWR)); } catch { /* best effort */ }
    }
  });
});
