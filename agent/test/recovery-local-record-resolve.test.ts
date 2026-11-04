import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { afterEach, test } from "node:test";
import { AuthorityLockBusy } from "../src/authority-lock.js";
import { GitCache } from "../src/git.js";
import { RecoveryCoordinator, type RecoveryRecord } from "../src/recovery.js";
import type { RecoveryExactHold } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { binding, closed, environment, fixture, git, inventory, patch, type Variant } from "./fixtures/recovery-bindingless-compat.js";

const roots: string[] = [];
afterEach(async () => { for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true }); });

/** A real coordinator over a real Git fixture; only the API client is faked. */
async function world(variant: Variant = "acknowledged", opts: { bind?: boolean } = {}) {
  await fs.mkdir(path.resolve("../.uzi/scratch"), { recursive: true });
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/local-resolve-"));
  roots.push(root);
  const f = await fixture(root, variant);
  const bare = f.context.barePath;
  // Recovery-enabled real-Git fixtures: worker-only directories and a private config.
  await fs.chmod(path.dirname(bare), 0o700);
  await fs.chmod(bare, 0o700);
  await fs.chmod(path.join(bare, "config"), 0o600);
  const e = environment(f);
  const { coordinator } = e;
  const producer = coordinator["git"], client = coordinator["client"];
  const cache = new GitCache(path.join(root, "data"), nullLogger());
  producer.recoveryRefSnapshot = cache.recoveryRefSnapshot.bind(cache);
  producer.assertRecoveryBareClear = cache.assertRecoveryBareClear.bind(cache);
  e.state.open = false;
  if (opts.bind !== false) f.record = await patch(coordinator, f.record, { exactHoldBinding: binding });
  const runId = f.context.runId;
  const w = {
    f, e, coordinator, producer, client, cache, root, bare, runId,
    exact: undefined as RecoveryExactHold | undefined | "throw",
    exactCalls: 0, holdsCalls: 0, cleanupArgs: [] as number[],
    request: new AbortController(), shutdown: new AbortController(),
    sig: (over: Partial<{ deadlineAt: number }> = {}) => ({ requestSignal: w.request.signal, shutdownSignal: w.shutdown.signal,
      deadlineAt: Date.now() + 60_000, ...over }),
    describe: () => coordinator.describeLocalRecords(runId, w.sig()),
    resolve: (confirm: string, over: Partial<{ generation: number; deadlineAt: number }> = {}) =>
      coordinator.resolveLocalRecord({ runId, generation: 7, confirm, ...w.sig(), ...over }),
    async digest(): Promise<string> {
      const d = await w.describe();
      assert.equal(d.kind, "described");
      const g = d.kind === "described" ? d.generations[0] : undefined;
      assert.ok(g?.digest, JSON.stringify(d));
      return g.digest;
    },
    async classification(): Promise<string> {
      const d = await w.describe();
      assert.equal(d.kind, "described", JSON.stringify(d));
      return d.kind === "described" ? (d.generations[0]?.classification ?? d.blockers.join(",")) : "";
    },
    refs: () => git(bare, "for-each-ref", "--format=%(refname) %(objectname)"),
    record: async () => (await coordinator.inspect(runId))[0]!,
    /** A cleanup that honours the operator's boundary callback and records its argument count. */
    cleanup(before?: () => void) {
      producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
        w.cleanupArgs.push(args.length);
        const callback = args[8] as (() => void) | undefined;
        before?.();
        callback?.();
        for (const ref of e.refs) git(bare, "update-ref", "-d", ref);
        return "removed";
      };
    },
  };
  w.exact = released(w);
  client.getExactRecoveryHold = async () => {
    w.exactCalls++;
    if (w.exact === "throw") throw new Error("fixture exact read failed");
    return w.exact;
  };
  const list = client.listRecoveryHolds.bind(client);
  client.listRecoveryHolds = async (...args) => { w.holdsCalls++; return list(...args); };
  return w;
}
type World = Awaited<ReturnType<typeof world>>;

/** The closed hold whose receipt equals the saved FINAL request (the case-A evidence). */
function released(w: Pick<World, "f">, over: Partial<RecoveryExactHold> = {}): RecoveryExactHold | undefined {
  const request = w.f.record.finalRequest;
  if (!request) return undefined;
  return { ...closed(w.f), state: "released", run_status: "failed", release_evidence: request.evidence,
    final_receipt: request.disposition, ...over };
}

test("A1: an acknowledged generation is described, confirmed and retired; the unchanged digest is accepted", async () => {
  const w = await world();
  const before = w.refs();
  for (const ref of w.e.refs) assert.match(before, new RegExp(ref));
  const described = await w.describe();
  assert.equal(described.kind, "described");
  assert.deepEqual(described.kind === "described" && described.generations.map(g => g.classification), ["case_a_ack"]);
  const digest = await w.digest();
  assert.equal(await w.digest(), digest, "describe is stable over an unchanged fixture");
  assert.match(digest, /^[0-9a-f]{64}$/);
  const listed = w.coordinator["listRecords"].bind(w.coordinator);
  let listCalls = 0;
  Object.assign(w.coordinator, { listRecords: (...args: Parameters<typeof listed>) => { listCalls++; return listed(...args); } });
  w.cleanup();
  const bundle = w.f.record.bundlePath!;
  await fs.stat(bundle);
  const result = await w.resolve(digest);
  assert.deepEqual(result, { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(listCalls, 0, "the operator path never lists records outside its one bounded read");
  const expected = before.split("\n").filter(line => !w.e.refs.some(ref => line.startsWith(ref + " "))).join("\n");
  assert.equal(w.refs(), expected, "exactly the generation's four refs were removed");
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
  await assert.rejects(fs.stat(bundle), { code: "ENOENT" });
  assert.deepEqual(w.cleanupArgs, [9], "the operator names the boundary callback");
  assert.equal(w.e.calls.reserve + w.e.calls.upload + w.e.calls.final + w.e.calls.held + w.e.calls.produce, 0);
});

test("the autonomous live path still cleans an acknowledged generation with the original argument list", async () => {
  const w = await world("acknowledged", { bind: false });
  let argc = -1;
  w.producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
    argc = args.length;
    for (const ref of w.e.refs) git(w.bare, "update-ref", "-d", ref);
    return "removed";
  };
  await w.coordinator.forgetGeneration(w.runId, 7);
  assert.equal(argc, 7);
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
});

test("forget names each outcome and refuses the legacy branch for the operator", async () => {
  const w = await world();
  const forget = (operator?: unknown) => w.coordinator["forgetGenerationWithinCycle"](w.runId, 7, undefined, undefined,
    operator as never);
  assert.equal(await forget(), "cleanup_unavailable", "no cleanup hook: retained");
  w.cleanup();
  const budget = { signal: new AbortController().signal, deadline: Date.now() + 10_000 };
  const operator = (over: Record<string, unknown> = {}) => ({ budget, expect: async () => undefined, beforeFirstMutation: () => {}, ...over });
  // A legacy (unguarded) record: the autonomous path would remove it; the operator is refused and nothing is touched.
  const legacyRun = "31050000-0000-4000-8000-0000000000bb";
  const pinned = await w.coordinator.pin({ runId: legacyRun, sourceSha: w.f.head, kind: "issue", branch: "agent/issue-1", generation: 3 });
  assert.ok(pinned && !pinned.inventoryGuarded);
  const before = await inventory(w.root);
  assert.equal(await w.coordinator["forgetGenerationWithinCycle"](legacyRun, 3, undefined, undefined, operator() as never), "not_guarded");
  assert.equal(await forget(operator({ expect: async () => "descriptor_changed" })), "operator_refused");
  assert.deepEqual(await inventory(w.root), before);
});

test("A1 refuses a client that left before the boundary and keeps every byte", async () => {
  const w = await world();
  const digest = await w.digest();
  w.cleanup(() => w.request.abort());
  const before = await inventory(w.root);
  const refs = w.refs();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  assert.equal(w.refs(), refs);
  assert.deepEqual(await inventory(w.root), before);
});

test("a client that leaves after the boundary does not stop the cleanup", async () => {
  const w = await world();
  const digest = await w.digest();
  w.producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
    (args[8] as () => void)();
    w.request.abort();
    for (const ref of w.e.refs) git(w.bare, "update-ref", "-d", ref);
    return "removed";
  };
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
});

test("a client already gone is refused before anything is read", async () => {
  const w = await world();
  const digest = await w.digest();
  const calls = w.exactCalls;
  w.request.abort();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  assert.equal(w.exactCalls, calls);
});

test("boundary fallback: a cleanup that removes nothing still crosses the boundary before the first unlink", async () => {
  const w = await world();
  const digest = await w.digest();
  // The cleanup reports "removed" without ever naming the boundary; the client leaves meanwhile.
  w.producer.cleanupRecoveryGeneration = async () => { w.request.abort(); return "removed"; };
  const bundle = w.f.record.bundlePath!;
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  await fs.stat(bundle);
  assert.equal((await w.coordinator.inspect(w.runId)).length, 1, "no journal was unlinked");
  // The same cleanup with the client present completes the unlinks.
  const again = await world();
  const againDigest = await again.digest();
  again.producer.cleanupRecoveryGeneration = async () => "removed";
  assert.deepEqual(await again.resolve(againDigest), { kind: "retired", generation: 7, remaining: 0 });
});

test("a deadline already spent is a named refusal", async () => {
  const w = await world();
  const digest = await w.digest();
  assert.deepEqual(await w.resolve(digest, { deadlineAt: Date.now() - 1 }), { kind: "refused", reason: "deadline_exhausted" });
  assert.equal((await w.describe()).kind, "described");
  assert.deepEqual(await w.coordinator.describeLocalRecords(w.runId, w.sig({ deadlineAt: Date.now() - 1 })),
    { kind: "refused", reason: "deadline_exhausted" });
});

test("shutdown ends a request with a named refusal", async () => {
  const w = await world();
  const digest = await w.digest();
  w.shutdown.abort();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "shutting_down" });
});

// ── describe classifications ─────────────────────────────────────────────────────────────────

test("describe: saved FINAL without ACK is case_a_replay with a digest, and resolve defers it", async () => {
  const w = await world("saved-final");
  const described = await w.describe();
  assert.equal(described.kind === "described" && described.generations[0]?.classification, "case_a_replay");
  const digest = await w.digest();
  const before = await inventory(w.root);
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "replay_not_implemented_yet" });
  assert.deepEqual(await inventory(w.root), before);
});

test("describe: a replay refused persistently is replay_refused_persisted", async () => {
  const w = await world("saved-final");
  await patch(w.coordinator, w.f.record, { reason: "final_retry_refused" });
  assert.equal(await w.classification(), "replay_refused_persisted");
});

test("describe: a discarded hold, or a receipt that does not match, is Case B and is never resolved", async () => {
  for (const variant of ["acknowledged", "saved-final"] as const) {
    const w = await world(variant);
    w.exact = closed(w.f);
    assert.equal(await w.classification(), "case_b_pending_owner_review");
    const before = await inventory(w.root);
    const refs = w.refs();
    assert.deepEqual(await w.resolve("a".repeat(64)), { kind: "refused", reason: "case_b_pending_owner_review" });
    w.exact = released(w, { final_receipt: { kind: "settled", coverage_digest: "b".repeat(64) } });
    assert.equal(await w.classification(), "case_b_pending_owner_review");
    assert.equal(w.refs(), refs);
    assert.deepEqual(await inventory(w.root), before);
  }
});

test("describe: missing binding, foreign bare, missing or open exact evidence and non-terminal runs", async () => {
  const unbound = await world("acknowledged", { bind: false });
  assert.equal(await unbound.classification(), "binding_missing");
  assert.equal(unbound.exactCalls, 0);

  const bare = await world();
  bare.producer.resolveRecoveryBareDir = async () => path.join(bare.root, "data", "repos", "elsewhere.git");
  assert.equal(await bare.classification(), "bare_mismatch");

  const unavailable = await world();
  unavailable.exact = undefined;
  assert.equal(await unavailable.classification(), "exact_evidence_unavailable");
  unavailable.exact = "throw";
  assert.equal(await unavailable.classification(), "exact_evidence_unavailable");
  unavailable.exact = released(unavailable, { hold_id: "31050000-0000-4000-8000-0000000000ff" });
  assert.equal(await unavailable.classification(), "exact_evidence_unavailable");

  const open = await world();
  open.exact = released(open, { state: "open", live_worker_id: "w", live_run_id: open.runId, release_evidence: undefined, final_receipt: undefined });
  assert.equal(await open.classification(), "hold_open");

  const running = await world();
  running.exact = released(running, { run_status: "running" });
  assert.equal(await running.classification(), "run_state_not_terminal");
});

test("describe: holds are read once and an open or unknown inventory blocks the generation", async () => {
  const w = await world();
  w.e.state.open = true;
  assert.equal(await w.classification(), "hold_open");
  assert.equal(w.exactCalls, 0, "an open hold is refused before the exact read");
  w.e.state.open = false;
  w.client.listRecoveryHolds = async () => { throw new Error("fixture holds failure"); };
  assert.equal(await w.classification(), "holds_unknown");
  w.client.listRecoveryHolds = async () => ({ run_id: "other", holds: [] });
  assert.equal(await w.classification(), "holds_unknown");
  w.client.hasFeature = () => false;
  assert.equal(await w.classification(), "holds_unknown");
});

test("describe: one holds read and one exact read for a one-generation run", async () => {
  const w = await world();
  await w.describe();
  assert.deepEqual([w.holdsCalls, w.exactCalls], [1, 1]);
});

test("describe: the other record classes", async () => {
  const receipt = await world("completion-receipt");
  assert.equal(await receipt.classification(), "completion_receipt_generation");
  const pending = await world("uploaded");
  assert.equal(await pending.classification(), "cleanup_pending");
  const protectedRun = await world();
  Object.assign(protectedRun.coordinator, { terminalRecordProtection: async () => true });
  assert.equal(await protectedRun.classification(), "terminal_protection");
  const settlement = await world();
  await fs.mkdir(path.join(settlement.root, "data", "recovery-settlement", settlement.runId), { recursive: true });
  await fs.writeFile(path.join(settlement.root, "data", "recovery-settlement", settlement.runId, "x.json"), "{}");
  assert.equal(await settlement.classification(), "settlement_record_unsupported");
  const report = await world();
  Object.assign(report.coordinator, { pendingGenerationReport: () => true });
  assert.equal(await report.classification(), "pending_generation_report");
  const legacy = await world();
  assert.ok(await legacy.coordinator.pin({ runId: legacy.runId, sourceSha: legacy.f.head, kind: "issue", branch: "agent/issue-1", generation: 3 }));
  const described = await legacy.describe();
  assert.deepEqual(described.kind === "described" && described.generations.map(g => [g.generation, g.classification]),
    [[3, "not_guarded"], [7, "case_a_ack"]]);
});

test("describe: an unbound temp file or orphan bundle keeps the generation and names itself", async () => {
  for (const name of ["partial.json.tmp", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.bundle"]) {
    const w = await world();
    const dir = path.dirname(w.coordinator["recordPath"](w.f.record));
    await fs.writeFile(path.join(dir, name), "stray");
    const digest = await w.classification();
    assert.equal(digest, "unbound_artifact:" + name);
    const refs = w.refs();
    assert.deepEqual(await w.resolve("c".repeat(64)), { kind: "refused", reason: "unbound_artifact:" + name });
    assert.equal(w.refs(), refs);
    assert.equal(await fs.readFile(path.join(dir, name), "utf8"), "stray");
  }
});

test("describe: a foreign-token journal is record_unverified and more than 64 entries or 1 MiB is inventory_incomplete", async () => {
  const w = await world();
  const foreign = environment(w.f, RecoveryCoordinator, "another-worker-token").coordinator;
  const described = await foreign.describeLocalRecords(w.runId, w.sig());
  assert.deepEqual(described, { kind: "described", runId: w.runId, generations: [], blockers: ["record_unverified"] });

  const many = await world();
  const dir = path.dirname(many.coordinator["recordPath"](many.f.record));
  for (let i = 0; i < 70; i++) await fs.writeFile(path.join(dir, `fake-${i}.json`), "{}");
  assert.equal(await many.classification(), "inventory_incomplete");
  let listCalls = 0;
  Object.assign(many.coordinator, { listRecords: async () => { listCalls++; return []; } });
  assert.deepEqual(await many.resolve("d".repeat(64)), { kind: "refused", reason: "inventory_incomplete" });
  assert.equal(listCalls, 0);

  const big = await world();
  const bigDir = path.dirname(big.coordinator["recordPath"](big.f.record));
  await fs.writeFile(path.join(bigDir, "big.json"), Buffer.alloc(1024 * 1024 + 1, 0x20));
  assert.equal(await big.classification(), "inventory_incomplete");
});

test("describe: a missing run is empty and a bad run id is invalid", async () => {
  const w = await world();
  assert.deepEqual(await w.coordinator.describeLocalRecords("31050000-0000-4000-8000-0000000000aa", w.sig()),
    { kind: "described", runId: "31050000-0000-4000-8000-0000000000aa", generations: [], blockers: [] });
  assert.deepEqual(await w.coordinator.describeLocalRecords("../escape", w.sig()), { kind: "refused", reason: "invalid_request" });
  assert.deepEqual(await w.resolve("not-a-digest"), { kind: "refused", reason: "invalid_request" });
  assert.deepEqual(await w.resolve("e".repeat(64), { generation: 9 }), { kind: "refused", reason: "not_found" });
});

/** Rewrite the (acknowledged, hence immutable to the normal writer) record with the journal's own MAC writer. */
const rewrite = (w: World, fields: Partial<RecoveryRecord>) =>
  w.coordinator["writeRecordUnlocked"]({ ...w.f.record, ...fields } as RecoveryRecord);

// ── resolve refusals ─────────────────────────────────────────────────────────────────────────

async function refused(w: World, reason: string, mutate?: (digest: string) => Promise<void> | void, over: Parameters<World["resolve"]>[1] = {},
  changesJournal = false) {
  const digest = await w.digest();
  await mutate?.(digest);
  w.cleanup();
  const before = await inventory(w.root);
  const refs = w.refs();
  assert.deepEqual(await w.resolve(digest, over), { kind: "refused", reason });
  assert.equal(w.refs(), refs, "refs untouched");
  // A test that itself rewrites the journal under the lock changes those bytes; nothing else may change.
  if (!changesJournal) assert.deepEqual(await inventory(w.root), before, "state byte-identical");
  assert.deepEqual(w.cleanupArgs, [], "cleanup never reached");
}

test("A1 refuses a hold opened before the pre-check, with the cleanup never reached", async () => {
  const w = await world();
  await refused(w, "hold_open", () => { w.e.state.open = true; });
  assert.equal(w.exactCalls, 1, "only describe's exact read ran");
});

test("A1 refuses a hold opened while waiting for the journal lock (second holds read)", async () => {
  const w = await world();
  const holdsBefore = w.holdsCalls;
  await refused(w, "hold_open", () => {
    Object.assign(w.coordinator, { onForgetJournalLockedForTest: () => { w.e.state.open = true; } });
  });
  assert.equal(w.holdsCalls - holdsBefore, 3, "describe, pre-check and the one under J");
});

test("A1 refuses an exact hold that turned open or non-terminal under the journal lock (second exact read)", async () => {
  for (const [reason, answer] of [["hold_open", { state: "open" as const, live_worker_id: "w", release_evidence: undefined, final_receipt: undefined }],
    ["run_state_not_terminal", { run_status: "running" }]] as const) {
    const w = await world();
    await refused(w, reason, () => {
      Object.assign(w.coordinator, { onForgetJournalLockedForTest: () => {
        w.exact = released(w, { ...answer, ...("state" in answer ? { live_run_id: w.runId } : {}) });
      } });
    });
  }
});

test("A1 refuses unknown holds, unavailable exact evidence and a run that is not terminal", async () => {
  const unknown = await world();
  await refused(unknown, "holds_unknown", () => { unknown.client.listRecoveryHolds = async () => { throw new Error("down"); }; });
  const unavailable = await world();
  await refused(unavailable, "exact_evidence_unavailable", () => { unavailable.exact = undefined; });
  const running = await world();
  await refused(running, "run_state_not_terminal", () => { running.exact = released(running, { run_status: "running" }); });
});

test("A1 refuses executing, quarantine-independent live gates and a pending generation report", async () => {
  const executing = await world();
  await refused(executing, "executing", () => { Object.assign(executing.coordinator, { isExecuting: () => true }); });
  const report = await world();
  await refused(report, "pending_generation_report", () => { Object.assign(report.coordinator, { pendingGenerationReport: () => true }); });
});

test("A1 refuses a stale digest whatever changed: a record, a ref, the exact observation, the settlement root", async () => {
  const record = await world();
  await refused(record, "descriptor_changed", async () => { await rewrite(record, { reason: "changed" }); }, {}, true);
  const ref = await world();
  await refused(ref, "descriptor_changed", () => { git(ref.bare, "update-ref", `refs/uzi-owed/${ref.runId}/${ref.f.head}`, ref.f.head); });
  const exact = await world();
  await refused(exact, "descriptor_changed", () => { exact.exact = released(exact, { release_evidence: "owner_release" }); });
  const wrong = await world();
  assert.deepEqual(await wrong.resolve("0".repeat(64)), { kind: "refused", reason: "descriptor_changed" });
  const settlement = await world();
  await refused(settlement, "settlement_record_unsupported", async () => {
    const dir = path.join(settlement.root, "data", "recovery-settlement", settlement.runId);
    await fs.mkdir(dir, { recursive: true });
    await fs.writeFile(path.join(dir, "x.json"), "{}");
  });
});

test("A1 refuses a record or ref changed while waiting for the journal lock", async () => {
  const record = await world();
  await refused(record, "descriptor_changed", () => {
    Object.assign(record.coordinator, { onForgetJournalLockedForTest: async () => {
      // Inside J: write the record directly (the lock is not reentrant).
      await rewrite(record, { reason: "changed under J" });
    } });
  }, {}, true);
  const ref = await world();
  const digest = await ref.digest();
  ref.cleanup();
  const added = `refs/uzi-owed/${ref.runId}/${ref.f.head}`;
  Object.assign(ref.coordinator, { onForgetJournalLockedForTest: () => { git(ref.bare, "update-ref", added, ref.f.head); } });
  assert.deepEqual(await ref.resolve(digest), { kind: "refused", reason: "descriptor_changed" });
  assert.deepEqual(ref.cleanupArgs, [], "cleanup never reached");
  for (const name of ref.e.refs) assert.match(ref.refs(), new RegExp(name), "the generation's refs are all still present");
});

test("A1 refuses a pending shared-bare deletion (bare_mutation_unresolved)", async () => {
  const w = await world();
  await refused(w, "bare_mutation_unresolved", () => {
    Object.assign(w.cache, { authority: { assertBareClear: async () => { throw new Error("pending bare mutation"); } } });
  });
});

test("A1 refuses a busy authority, a busy cycle and a client that left while waiting for the authority", async () => {
  const busy = await world();
  Object.assign(busy.coordinator, { authority: { withTransaction: async () => { throw new AuthorityLockBusy(); } } });
  await refused(busy, "authority_busy");

  const gone = await world();
  const waits: Array<number | undefined> = [];
  Object.assign(gone.coordinator, { authority: { withTransaction: (_fn: unknown, options: { signal: AbortSignal; timeoutMs: number }) => {
    waits.push(options.timeoutMs);
    return new Promise((_resolve, reject) => {
      options.signal.addEventListener("abort", () => reject(options.signal.reason), { once: true });
      queueMicrotask(() => gone.request.abort());
    });
  } } });
  await refused(gone, "client_disconnected");
  assert.ok(waits[0]! <= 30_000 && waits[0]! > 0);

  const held = await world();
  const digest = await held.digest();
  held.cleanup();
  let reached!: () => void, release!: () => void;
  const inLock = new Promise<void>(resolve => { reached = resolve; });
  const gate = new Promise<void>(resolve => { release = resolve; });
  Object.assign(held.coordinator, { onForgetJournalLockedForTest: async () => { reached(); await gate; } });
  const first = held.resolve(digest);
  await inLock;
  assert.deepEqual(await held.resolve(digest), { kind: "refused", reason: "cycle_busy" });
  release();
  assert.equal((await first).kind, "retired");
});

test("a borrowed authority transaction runs the resolution", async () => {
  const w = await world();
  const digest = await w.digest();
  w.cleanup();
  Object.assign(w.coordinator, { authority: { withTransaction: async (fn: () => Promise<unknown>) => fn() } });
  assert.equal((await w.resolve(digest)).kind, "retired");
});

// ── the real cleanup boundary ────────────────────────────────────────────────────────────────

test("real cleanup: the boundary is crossed once, before the first ref deletion, and only the generation's pins go", async () => {
  const w = await world();
  const digest = await w.digest();
  const events: string[] = [];
  const deleteRef = w.cache["deleteRefSettled"].bind(w.cache);
  Object.assign(w.cache, { deleteRefSettled: (...args: Parameters<typeof deleteRef>) => { events.push("delete"); return deleteRef(...args); } });
  w.producer.cleanupRecoveryGeneration = (...args) => {
    const callback = args[8] as () => void;
    args[8] = () => { events.push("boundary"); callback(); };
    return w.cache.cleanupRecoveryGeneration(...args);
  };
  const coverage = `refs/uzi-coverage/${w.runId}/7/`;
  assert.match(w.refs(), new RegExp(coverage));
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(events.filter(e => e === "boundary").length, 1);
  assert.equal(events[0], "boundary");
  assert.ok(events.includes("delete"));
  assert.doesNotMatch(w.refs(), new RegExp(coverage));
  assert.match(w.refs(), /refs\/owed\/fixture/, "refs the cleanup does not own are untouched");
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
});

test("real cleanup: a client that leaves exactly at the boundary leaves every ref and byte in place", async () => {
  const w = await world();
  const digest = await w.digest();
  w.producer.cleanupRecoveryGeneration = (...args) => {
    const callback = args[8] as () => void;
    args[8] = () => { w.request.abort(); callback(); };
    return w.cache.cleanupRecoveryGeneration(...args);
  };
  const before = await inventory(w.root);
  const refs = w.refs();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  assert.equal(w.refs(), refs);
  assert.deepEqual(await inventory(w.root), before);
});

test("shutdown or the deadline after the boundary is partial, never a refusal", async () => {
  const shutdown = await world();
  const digest = await shutdown.digest();
  shutdown.producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
    (args[8] as () => void)();
    shutdown.shutdown.abort();
    for (const ref of shutdown.e.refs) git(shutdown.bare, "update-ref", "-d", ref);
    return "removed";
  };
  const first = await shutdown.resolve(digest);
  assert.equal(first.kind, "partial");
  assert.deepEqual(first, { kind: "partial", reason: "shutting_down" });

  const late = await world();
  const lateDigest = await late.digest();
  late.producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
    (args[8] as () => void)();
    await new Promise(resolve => setTimeout(resolve, 3_500));
    for (const ref of late.e.refs) git(late.bare, "update-ref", "-d", ref);
    return "removed";
  };
  assert.deepEqual(await late.resolve(lateDigest, { deadlineAt: Date.now() + 3_000 }), { kind: "partial", reason: "deadline_exhausted" });
  // The partial outcome is re-describable: the records that remain are classified, not trusted.
  const after = await late.describe();
  assert.equal(after.kind, "described");
});

test("A1 does not consult the client signal after the boundary: an abort at any later point cannot refuse the cleanup", async () => {
  const w = await world();
  const digest = await w.digest();
  w.producer.cleanupRecoveryGeneration = async (...args: unknown[]) => {
    (args[8] as () => void)();
    w.request.abort();
    // Journal and bundle removal after the abort still run to completion.
    for (const ref of w.e.refs) git(w.bare, "update-ref", "-d", ref);
    return "removed";
  };
  assert.equal((await w.resolve(digest)).kind, "retired");
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
});

test("a producer-origin record in the generation is non_ordinary_origin; an ACK the journal cannot authenticate never reaches classification", async () => {
  const maintenance = await world();
  await maintenance.coordinator["writeRecordUnlocked"]({ ...maintenance.f.record, captureId: "maintenance-v1-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" } as RecoveryRecord);
  assert.equal(await maintenance.classification(), "non_ordinary_origin");
  // An ACK without its FINAL request or coverage digest is rejected by the authenticated reader itself, so the
  // whole run reports record_unverified rather than a guessed classification (final_ack_unknown is defence in depth).
  const noCoverage = await world();
  await rewrite(noCoverage, { coverageDigest: undefined });
  assert.equal(await noCoverage.classification(), "record_unverified");
});
