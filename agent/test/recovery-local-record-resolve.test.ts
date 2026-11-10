import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { afterEach, test } from "node:test";
import { AuthorityLockBusy } from "../src/authority-lock.js";
import { GitCache } from "../src/git.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { RecoveryCoordinator, type RecoveryRecord } from "../src/recovery.js";
import type { RecoveryExactHold } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { binding, closed, environment, fixture, git, inventory, patch, type Variant } from "./fixtures/recovery-bindingless-compat.js";
import { fakeSourceEvidence } from "./fixtures/recovery-case-b-evidence.js";

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
  producer.verifyRecoveryCoverage = cache.verifyRecoveryCoverage.bind(cache);
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

test("describe: saved FINAL without ACK is case_a_replay with a digest", async () => {
  const w = await world("saved-final");
  const described = await w.describe();
  assert.equal(described.kind === "described" && described.generations[0]?.classification, "case_a_replay");
  assert.match(await w.digest(), /^[0-9a-f]{64}$/);
});

test("describe: a replay refused persistently is replay_refused_persisted", async () => {
  const w = await world("saved-final");
  await patch(w.coordinator, w.f.record, { reason: "final_retry_refused" });
  assert.equal(await w.classification(), "replay_refused_persisted");
});

test("describe: a discarded hold, or a receipt that does not match, is never resolved as Case A", async () => {
  for (const variant of ["acknowledged", "saved-final"] as const) {
    const w = await world(variant);
    // A saved FINAL without ACK is Case B by this evidence (issue #3885): it needs source evidence and its own action.
    Object.assign(w.coordinator, { operatorSourceEvidence: async () => fakeSourceEvidence(w.runId, w.bare) });
    const ackd = variant === "acknowledged";
    const before = await inventory(w.root);
    const refs = w.refs();
    for (const [exact, caseB] of [[closed(w.f), "case_b_owner_discard"],
      [released(w, { final_receipt: { kind: "settled", coverage_digest: "b".repeat(64) } }), "case_b_released_mismatch"]] as const) {
      w.exact = exact;
      const expected = ackd ? "ack_evidence_inconsistent" : caseB;
      assert.equal(await w.classification(), expected);
      assert.deepEqual(await w.resolve("a".repeat(64)), { kind: "refused", reason: ackd ? "ack_evidence_inconsistent" : "descriptor_changed" });
      if (!ackd) {
        const digest = await w.digest();
        assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "case_b_disposition_required" });
      }
    }
    assert.equal(w.refs(), refs);
    assert.deepEqual(await inventory(w.root), before);
  }
});

test("describe and resolve: a matching receipt with different release evidence is Case B, with nothing touched", async () => {
  const w = await world("saved-final");
  Object.assign(w.coordinator, { operatorSourceEvidence: async () => fakeSourceEvidence(w.runId, w.bare) });
  const request = w.f.record.finalRequest!;
  w.f.record = await patch(w.coordinator, w.f.record, { finalRequest: { ...request, evidence: "operator_evidence" } });
  w.exact = released(w);
  assert.equal(w.exact?.release_evidence, "operator_evidence");
  assert.equal(await w.classification(), "case_a_replay", "the same evidence is resolvable");
  w.exact = released(w, { release_evidence: "some_other_evidence" });
  assert.deepEqual(w.exact?.final_receipt, w.f.record.finalRequest!.disposition, "the receipt itself matches");
  assert.equal(await w.classification(), "case_b_released_mismatch");
  w.e.state.allowFinal = true;
  const before = await inventory(w.root);
  const refs = w.refs();
  assert.deepEqual(await w.resolve("a".repeat(64)), { kind: "refused", reason: "descriptor_changed" });
  assert.deepEqual(await w.resolve(await w.digest()), { kind: "refused", reason: "case_b_disposition_required" });
  assert.equal(w.refs(), refs);
  assert.deepEqual(await inventory(w.root), before);
  assert.equal(w.e.calls.final, 0, "no release POST");
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
  // One guarded aggregate and no saved FINAL is the Case B anchor (issue #3885); this fixture serves no exact hold.
  assert.equal(await pending.classification(), "exact_evidence_unavailable");
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

// ── A2: operator exact replay of a saved FINAL whose closed hold carries a matching receipt ──────────

/** A saved-FINAL world whose release call is recorded together with the bytes present when it was sent. */
async function replayWorld(opts: { settlement?: boolean; variant?: Variant } = {}) {
  const w = await world(opts.variant ?? "saved-final");
  w.e.state.allowFinal = true;
  if (opts.settlement) await patch(w.coordinator, w.f.record, { holdSettlement: w.exact as RecoveryExactHold });
  const posts: Array<{ args: unknown[]; bytes: Awaited<ReturnType<typeof inventory>>; refs: string; objects: string }> = [];
  const release = w.client.releaseRecoveryCustody.bind(w.client);
  w.client.releaseRecoveryCustody = async (...args: Parameters<typeof release>) => {
    posts.push({ args, bytes: await inventory(w.root), refs: w.refs(), objects: git(w.bare, "count-objects", "-v") });
    return release(...args);
  };
  const calls = { retain: 0, reconcile: 0, existingWrites: 0, recordWrites: 0, status: 0 };
  const count = (name: string, key: keyof typeof calls) => {
    const original = (w.coordinator as unknown as Record<string, (...a: unknown[]) => unknown>)[name]!.bind(w.coordinator);
    Object.assign(w.coordinator, { [name]: (...a: unknown[]) => { calls[key]++; return original(...a); } });
  };
  count("retainFinal", "retain");
  count("reconcileInventory", "reconcile");
  count("writeExistingRecord", "existingWrites");
  count("writeRecordUnlocked", "recordWrites");
  const status = w.client.getRecoveryCaptureStatus.bind(w.client);
  w.client.getRecoveryCaptureStatus = async (...a: Parameters<typeof status>) => { calls.status++; return status(...a); };
  /** What the record file looks like at the moment the cleanup runs (J is held, so read the file directly). */
  const atCleanup: { acknowledged?: boolean; settled?: boolean } = {};
  w.cleanup(() => undefined);
  const fakeCleanup = w.producer.cleanupRecoveryGeneration!;
  w.producer.cleanupRecoveryGeneration = async (...args: Parameters<typeof fakeCleanup>) => {
    const raw = await fs.readFile(w.coordinator["recordPath"](w.f.record), "utf8");
    atCleanup.acknowledged = raw.includes('"finalAcknowledged":true');
    atCleanup.settled = raw.includes('"holdSettlement"');
    return fakeCleanup(...args);
  };
  return Object.assign(w, { posts, calls, atCleanup });
}
type ReplayWorld = Awaited<ReturnType<typeof replayWorld>>;

/** Nothing the replay's pre-POST proofs read may change a byte or a ref. */
async function assertByteNeutral(w: ReplayWorld, bytes: Awaited<ReturnType<typeof inventory>>, refs: string) {
  assert.deepEqual(await inventory(w.root), bytes, "every byte unchanged");
  assert.equal(w.refs(), refs, "every ref unchanged");
  assert.equal(w.posts.length, 0, "no release POST");
  assert.equal(w.calls.retain + w.calls.reconcile + w.calls.existingWrites + w.calls.recordWrites, 0, "no write path ran");
}

for (const settlement of [false, true]) {
  test(`A2 (${settlement ? "persisted matching settlement" : "no settlement"}): one canonical POST, the ACK, then the cleanup; nothing else is written`, async () => {
    const w = await replayWorld({ settlement });
    const request = w.f.record.finalRequest!;
    const digest = await w.digest();
    const bytes = await inventory(w.root);
    const refs = w.refs();
    const objects = git(w.bare, "count-objects", "-v");
    assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
    assert.equal(w.posts[0]!.objects, objects, "no git object is created before the POST");
    assert.equal(w.posts.length, 1, "exactly one release POST");
    assert.equal(w.posts[0]!.args[2], request.evidence);
    assert.deepEqual(w.posts[0]!.args[3], request.disposition, "the body is the saved request, canonically");
    assert.deepEqual(w.posts[0]!.bytes, bytes, "record bytes and all journal entries unchanged up to the POST");
    assert.equal(w.posts[0]!.refs, refs);
    assert.equal(w.atCleanup.acknowledged, true, "the ACK was persisted before the cleanup");
    assert.equal(w.atCleanup.settled, settlement, "a settlement is neither created nor lost");
    assert.equal(w.calls.recordWrites, 1, "the ACK is the only record write");
    assert.equal(w.calls.existingWrites + w.calls.retain + w.calls.reconcile, 0);
    assert.equal(w.e.calls.reserve + w.e.calls.upload + w.e.calls.produce + w.e.calls.held, 0);
    assert.equal(w.cleanupArgs.length, 1);
    assert.deepEqual(await w.coordinator.inspect(w.runId), []);
  });
}

test("A2 with the real cleanup retires the generation's refs and bundle", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  w.producer.cleanupRecoveryGeneration = (...args) => w.cache.cleanupRecoveryGeneration(...args);
  const bundle = w.f.record.bundlePath!;
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(w.posts.length, 1);
  await assert.rejects(fs.stat(bundle), { code: "ENOENT" });
  assert.deepEqual(await w.coordinator.inspect(w.runId), []);
  assert.doesNotMatch(w.refs(), new RegExp(`refs/uzi-coverage/${w.runId}/7/`));
});

test("A2 refuses a source boundary that retains the source, byte-neutrally and with no diagnostics write", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  w.e.state.boundary = false;
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "source_boundary_retained" });
  await assertByteNeutral(w, bytes, refs);
  // The same refusal when the boundary wrapper swallows an error thrown from inside the finalizer.
  const swallowed = await replayWorld();
  const swallowedDigest = await swallowed.digest();
  Object.assign(swallowed.coordinator, { withInventorySourceBoundary: async (_context: unknown, action: (prove: () => Promise<boolean>) => Promise<void>) => {
    try { await action(async () => true); return "passed"; } catch { return "retained"; }
  } });
  swallowed.producer.readRemediationRestrictionFacts = (async () => {
    swallowed.request.abort();
    return { blocked: false, enrolled: false, fingerprint: "f", roots: [], flaggedOIDs: [] };
  }) as never;
  assert.deepEqual(await swallowed.resolve(swallowedDigest), { kind: "refused", reason: "client_disconnected" });
});

test("A2 pre-POST disconnect and shutdown at the last proof leave every byte and call retainFinal never", async () => {
  const facts = (hook: () => void) => async () => { hook(); return { blocked: false, enrolled: false, fingerprint: "f", roots: [], flaggedOIDs: [] }; };
  const gone = await replayWorld();
  const goneDigest = await gone.digest();
  const goneBytes = await inventory(gone.root);
  const goneRefs = gone.refs();
  gone.producer.readRemediationRestrictionFacts = facts(() => gone.request.abort()) as never;
  assert.deepEqual(await gone.resolve(goneDigest), { kind: "refused", reason: "client_disconnected" });
  await assertByteNeutral(gone, goneBytes, goneRefs);

  const down = await replayWorld();
  const downDigest = await down.digest();
  const downBytes = await inventory(down.root);
  const downRefs = down.refs();
  down.producer.readRemediationRestrictionFacts = facts(() => down.shutdown.abort()) as never;
  assert.deepEqual(await down.resolve(downDigest), { kind: "refused", reason: "shutting_down" });
  await assertByteNeutral(down, downBytes, downRefs);
});

test("A2 client that leaves after the last pre-POST proof, at the final checkpoint, is client_disconnected with no POST and no write", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  // The finalizer asks restrictionsAllowCapturedC once as an early gate and once more as its last step
  // before the beforeFirstMutation checkpoint: the client leaves exactly after the second answer, so only
  // that checkpoint can notice (nothing is awaited between them and the POST).
  const original = w.coordinator["restrictionsAllowCapturedC"].bind(w.coordinator);
  let asked = 0;
  Object.assign(w.coordinator, { restrictionsAllowCapturedC: async (...a: Parameters<typeof original>) => {
    const allowed = await original(...a);
    if (++asked === 2) w.request.abort();
    return allowed;
  } });
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  assert.equal(asked, 2, "the abort landed on the last proof before the POST");
  await assertByteNeutral(w, bytes, refs);
  assert.equal(w.cleanupArgs.length, 0, "the cleanup is never reached");
});

test("A2 refuses an unmet pre-POST precondition without writing anything (replay_precondition_unmet)", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  w.producer.readRemediationRestrictionFacts = (async () => { throw new Error("fixture facts failure"); }) as never;
  // restrictionsAllowCapturedC treats an unreadable fact as a held record: a named precondition, no write.
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "replay_precondition_unmet:restrictions" });
  await assertByteNeutral(w, bytes, refs);
  const thrown = await replayWorld();
  const thrownDigest = await thrown.digest();
  thrown.client.getRecoveryCaptureStatus = async () => { throw new Error("fixture status failure"); };
  assert.deepEqual(await thrown.resolve(thrownDigest), { kind: "refused", reason: "replay_precondition_unmet:capture_status" });
  assert.equal(thrown.posts.length, 0);
  assert.equal(thrown.calls.retain + thrown.calls.reconcile, 0);
});

test("A2 refuses when the exact observation drifts from the confirmed one (descriptor_changed), without writing", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  // The first reads (pre-check and under J) agree; the finalizer's own read sees a changed claim generation.
  let reads = 0;
  const original = async (): Promise<RecoveryExactHold> => w.exact as RecoveryExactHold;
  w.client.getExactRecoveryHold = async () => {
    reads++;
    const exact = await original();
    return reads >= 3 ? { ...exact, current_claim_generation: exact.current_claim_generation + 1 } : exact;
  };
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "descriptor_changed" });
  await assertByteNeutral(w, bytes, refs);
});

test("A2 hold closed by a different observation: an open or unreadable hold in the finalizer is a named refusal with no retainFinal", async () => {
  for (const [reason, answer] of [["hold_open", (e: RecoveryExactHold): RecoveryExactHold =>
    ({ ...e, state: "open", live_worker_id: "w", live_run_id: e.run_id, release_evidence: undefined, final_receipt: undefined })],
  ["holds_unknown", (e: RecoveryExactHold): RecoveryExactHold => ({ ...e, hold_id: "31050000-0000-4000-8000-0000000000ff" })]] as const) {
    const w = await replayWorld();
    const digest = await w.digest();
    const bytes = await inventory(w.root);
    const refs = w.refs();
    let reads = 0;
    const original = async (): Promise<RecoveryExactHold> => w.exact as RecoveryExactHold;
    w.client.getExactRecoveryHold = async () => {
      reads++;
      const exact = await original();
      return reads >= 3 ? answer(exact) : exact;
    };
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason });
    await assertByteNeutral(w, bytes, refs);
  }
});

test("A2 refuses a missing saved FINAL (no_saved_final) and never mints one", async () => {
  const w = await replayWorld();
  const uploaded = await world("uploaded");
  const scope = (verified: RecoveryExactHold) => {
    const budget = new MaintenanceBudget(60_000);
    const io = { requestSignal: new AbortController().signal, shutdownSignal: new AbortController().signal, deadlineAt: Date.now() + 60_000,
      budget, journal: { signal: budget.signal, deadline: Date.now() + 60_000 } };
    return { budget, scope: { io, verified, posted: false, beforeFirstMutation: () => undefined } as { refusal?: string; posted: boolean } };
  };
  void w;
  const { budget, scope: s } = scope(released(uploaded) ?? closed(uploaded.f));
  const bytes = await inventory(uploaded.root);
  const outcome = await MaintenanceBudget.run(budget, () => uploaded.coordinator["operatorReplay"].run(s as never,
    () => uploaded.coordinator["finalizeInventory"](uploaded.f.record, false, "deny")));
  budget.dispose();
  assert.equal(outcome, undefined);
  assert.equal(s.refusal, "no_saved_final");
  assert.deepEqual(await inventory(uploaded.root), bytes);
  assert.equal(uploaded.e.calls.final, 0);
});

test("A2 after the POST: an unexpected answer is replay_ack_mismatch with no reconcile and no write", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  w.client.releaseRecoveryCustody = async () => ({ run_id: w.runId, generation: 7, released: true, holds_released: 0 });
  const before = await w.record();
  assert.deepEqual(await w.resolve(digest), { kind: "partial", reason: "replay_ack_mismatch" });
  assert.equal(w.calls.reconcile + w.calls.retain + w.calls.existingWrites + w.calls.recordWrites, 0);
  assert.deepEqual(await w.record(), before, "the record is unchanged");
  assert.equal((await w.describe()).kind, "described");
});

test("A2 after the POST: a failed release keeps the existing retainFinal write and never reconciles", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  w.client.releaseRecoveryCustody = async () => { throw new Error("fixture release transport failure"); };
  const result = await w.resolve(digest);
  assert.deepEqual(result, { kind: "partial", reason: "replay_failed:final_retry_unknown" });
  assert.equal(w.calls.reconcile, 0);
  assert.equal(w.calls.retain, 1);
  assert.equal((await w.record()).finalAcknowledged, undefined);
});

test("A2 reports an acknowledged cleanup that the boundary refuses", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  w.producer.cleanupRecoveryGeneration = async () => "retained";
  assert.deepEqual(await w.resolve(digest), { kind: "partial", reason: "acknowledged_cleanup_refused:cleanup_retained" });
  const after = await w.record();
  assert.equal(after.finalAcknowledged, true);
  assert.equal(await w.classification(), "case_a_ack", "re-describing the same generation now offers the A1 path");
});

test("A2 crash point 2: POST sent, ACK not written, then the identical replay is offered and a second replay retires", async () => {
  const w = await replayWorld();
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  // The ACK write is prevented (a crash between the POST and the journal write).
  const write = w.coordinator["writeRecordUnlocked"].bind(w.coordinator);
  w.coordinator["writeRecordUnlocked"] = async () => { throw new Error("fixture crash before the ACK write"); };
  const first = await w.resolve(digest);
  assert.equal(first.kind, "partial");
  assert.equal(w.posts.length, 1);
  w.coordinator["writeRecordUnlocked"] = write;
  assert.deepEqual(await inventory(w.root), bytes, "the record is byte-identical after the crash");
  assert.equal(await w.classification(), "case_a_replay");
  assert.equal(await w.digest(), digest, "the same digest is offered again");
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(w.posts.length, 2, "the replay sends the same request once more");
  assert.deepEqual(JSON.stringify(w.posts[1]!.args.slice(2, 4)), JSON.stringify(w.posts[0]!.args.slice(2, 4)));
});

test("outside the operator scope the autonomous live pass still persists the settlement, retains the FINAL and reconciles", async () => {
  // A closed hold observed by the autonomous path is written to the journal (the operator replay never does that).
  const settled = await world("saved-final");
  const record = await settled.record();
  assert.equal(record.holdSettlement, undefined);
  assert.equal(await settled.coordinator.heldOperationState(record), "settled");
  assert.equal((await settled.record()).holdSettlement?.state, "released");

  // A refused FINAL release is retained and reconciled by the autonomous finalizer.
  const w = await replayWorld();
  w.client.releaseRecoveryCustody = async () => { throw new Error("fixture release transport failure"); };
  const record2 = await w.record();
  await w.coordinator["finalizeInventory"](record2, true, "deny");
  assert.equal(w.calls.retain, 1, "retainFinal ran");
  assert.ok(w.calls.reconcile >= 1, "reconcileInventory ran");
});

/** Start with the real pinned producer, never archive reservation/upload. Save the FINAL through the
 * existing settled freeze or held-publication producer; optionally let its original ACK land. */
async function archiveFreeWorld(kind: "settled" | "held_publication", acknowledged: boolean) {
  const w = await replayWorld({ variant: "pinned" });
  w.e.state.open = true;
  w.e.state.allowFinal = acknowledged;
  w.exact = { ...closed(w.f), state: "open", live_worker_id: closed(w.f).original_worker_id,
    live_run_id: w.runId, release_evidence: undefined };
  const cleanup = w.producer.cleanupRecoveryGeneration;
  // Keep an acknowledged fixture available for the operator, rather than the autonomous cleanup.
  delete w.producer.cleanupRecoveryGeneration;
  if (kind === "settled") {
    // Remove the initial nonempty pinned inventory and its coverage pin from fixture setup.
    // The new empty inventory below is produced by freezeInventory itself.
    await fs.unlink(w.coordinator["recordPath"](w.f.record));
    git(w.bare, "update-ref", "-d", `refs/uzi-coverage/${w.runId}/7/${w.f.record.coverageDigest}`);
    w.producer.enumerateOwedCandidates = async () => [];
    const record = await w.coordinator.freezeInventory({ context: w.f.context, currentSha: w.f.head,
      defaultBranch: "main", locallyQuiescent: true, settledEvidence: "forge_no_output", repoId: binding.repoId });
    assert.ok(record);
    w.f.record = record;
    await w.coordinator["finalizeInventory"](record, false, "deny");
  } else {
    await w.coordinator["releaseHeldWithinCycle"](w.f.record,
      "31050000-0000-4000-8000-0000000000a1");
  }
  assert.ok(cleanup);
  w.producer.cleanupRecoveryGeneration = cleanup;
  w.f.record = await w.record();
  assert.equal(w.f.record.inventoryGuarded, true);
  assert.equal(w.f.record.finalRequest?.disposition.kind, kind);
  assert.equal(w.f.record.finalAcknowledged === true, acknowledged);
  assert.equal(w.f.record.serverCaptureId, undefined, "the producer never reserved an archive capture");
  assert.equal(w.f.record.bundlePath, undefined, "no archive bundle was produced");
  assert.equal(w.e.calls.produce + w.e.calls.reserve + w.e.calls.upload, 0);
  w.e.state.open = false;
  w.e.state.allowFinal = true;
  w.exact = released(w);
  assert.ok(w.exact);
  for (const key of Object.keys(w.calls) as Array<keyof typeof w.calls>) w.calls[key] = 0;
  for (const key of Object.keys(w.e.calls) as Array<keyof typeof w.e.calls>) w.e.calls[key] = 0;
  w.posts.length = 0;
  w.cleanupArgs.length = 0;
  w.producer.cleanupRecoveryGeneration = async (...args) => {
    w.cleanupArgs.push(args.length);
    const raw = await fs.readFile(w.coordinator["recordPath"](w.f.record), "utf8");
    w.atCleanup.acknowledged = raw.includes('"finalAcknowledged":true');
    w.atCleanup.settled = raw.includes('"holdSettlement"');
    return w.cache.cleanupRecoveryGeneration(...args);
  };
  return w;
}

for (const kind of ["settled", "held_publication"] as const) {
  for (const acknowledged of [false, true]) {
    test(`archive-free Case A ${kind} ${acknowledged ? "A1" : "A2"}: matching receipt retires without archive authority`, async () => {
      const w = await archiveFreeWorld(kind, acknowledged);
      const request = w.f.record.finalRequest!;
      assert.equal(await w.classification(), acknowledged ? "case_a_ack" : "case_a_replay");
      const confirm = await w.digest();
      const bytes = await inventory(w.root);
      const refs = w.refs();
      assert.equal(await w.digest(), confirm, "the archive-free digest is stable");
      assert.deepEqual(await w.resolve(confirm), { kind: "retired", generation: 7, remaining: 0 });
      assert.equal(w.posts.length, acknowledged ? 0 : 1);
      if (!acknowledged) {
        assert.equal(w.posts[0]!.args[2], request.evidence);
        assert.deepEqual(w.posts[0]!.args[3], request.disposition, "one identical saved FINAL replay");
        assert.deepEqual(w.posts[0]!.bytes, bytes, "no record change before POST");
        assert.equal(w.posts[0]!.refs, refs);
        assert.equal(w.calls.recordWrites, 1, "only the replay ACK is persisted");
      } else assert.equal(w.calls.recordWrites, 0);
      assert.equal(w.atCleanup.acknowledged, true);
      assert.equal(w.cleanupArgs.length, 1);
      assert.equal(w.calls.retain + w.calls.reconcile + w.calls.existingWrites, 0);
      assert.equal(w.e.calls.reserve + w.e.calls.upload + w.e.calls.produce + w.e.calls.held, 0);
      assert.deepEqual(await w.coordinator.inspect(w.runId), []);
      assert.doesNotMatch(w.refs(), new RegExp(`refs/uzi-coverage/${w.runId}/7/`));
      for (const ref of w.e.refs.filter(ref => refs.includes(ref + " "))) {
        assert.match(w.refs(), new RegExp(ref), "the real cleanup keeps unrelated fixture refs");
      }
    });
  }
}

for (const kind of ["settled", "held_publication"] as const) {
  const mismatches = kind === "settled" ? ["digest", "evidence"] : ["publication", "source", "digest"];
  for (const acknowledged of [false, true]) {
    for (const mismatch of mismatches) {
      test(`archive-free ${kind} ${acknowledged ? "A1" : "A2"}: a mismatched ${mismatch} refuses without mutation`, async () => {
        const w = await archiveFreeWorld(kind, acknowledged);
        const confirm = await w.digest();
        assert.ok(w.exact && w.exact !== "throw" && w.exact.final_receipt);
        if (mismatch === "evidence") w.exact = { ...w.exact, release_evidence: "publication" };
        else {
          const receipt = w.exact.final_receipt;
          const field = mismatch === "publication" ? { publication_id: "31050000-0000-4000-8000-0000000000a2" }
            : mismatch === "source" ? { source_sha: "f".repeat(40) } : { coverage_digest: "b".repeat(64) };
          w.exact = { ...w.exact, final_receipt: { ...receipt, ...field } };
        }
        const bytes = await inventory(w.root);
        const refs = w.refs();
        // The acknowledged generation's evidence is inconsistent; the unacknowledged one is Case B (its Case A digest is stale).
        Object.assign(w.coordinator, { operatorSourceEvidence: async () => fakeSourceEvidence(w.runId, w.bare) });
        assert.equal(await w.classification(), acknowledged ? "ack_evidence_inconsistent" : "case_b_released_mismatch");
        assert.deepEqual(await w.resolve(confirm), { kind: "refused", reason: acknowledged ? "ack_evidence_inconsistent" : "descriptor_changed" });
        await assertByteNeutral(w, bytes, refs);
        assert.equal(w.cleanupArgs.length, 0);
        assert.equal(w.e.calls.reserve + w.e.calls.upload + w.e.calls.produce + w.e.calls.held, 0);
      });
    }
  }
}

for (const serverCaptureId of [undefined, "31050000-0000-4000-8000-0000000000a2"]) {
  test(`archive FINAL still refuses a ${serverCaptureId ? "wrong" : "missing"} archive capture identity`, async () => {
    const w = await world("saved-final");
    const confirm = await w.digest();
    await patch(w.coordinator, w.f.record, { serverCaptureId });
    const bytes = await inventory(w.root);
    const refs = w.refs();
    const described = await w.describe();
    assert.equal(described.kind, "described");
    assert.deepEqual(described.kind === "described" && described.blockers, ["record_unverified"]);
    assert.deepEqual(await w.resolve(confirm), { kind: "refused", reason: "record_unverified" });
    assert.deepEqual(await inventory(w.root), bytes);
    assert.equal(w.refs(), refs);
    assert.equal(w.e.calls.final + w.e.calls.reserve + w.e.calls.upload + w.e.calls.produce + w.e.calls.held, 0);
  });
}

// ── A2, held_publication disposition ─────────────────────────────────────────────────────────────

async function heldReplayWorld() {
  const w = await replayWorld();
  const disposition = { kind: "held_publication" as const, publication_id: "31050000-0000-4000-8000-0000000000a1",
    source_sha: w.f.record.sourceSha, coverage_digest: w.f.record.coverageDigest! };
  await patch(w.coordinator, w.f.record, { finalRequest: { disposition }, serverCaptureId: undefined });
  w.f.record = await w.record();
  w.exact = { ...closed(w.f), state: "released", run_status: "failed", release_evidence: undefined, final_receipt: disposition };
  for (const key of Object.keys(w.calls) as Array<keyof typeof w.calls>) w.calls[key] = 0;
  return Object.assign(w, { disposition });
}

test("A2 (held_publication): one canonical POST, the ACK, then the cleanup; no fallback, retain or reconcile", async () => {
  const w = await heldReplayWorld();
  assert.equal(await w.classification(), "case_a_replay");
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  assert.deepEqual(await w.resolve(digest), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(w.posts.length, 1);
  assert.deepEqual(w.posts[0]!.args[3], w.disposition);
  assert.deepEqual(w.posts[0]!.bytes, bytes);
  assert.equal(w.atCleanup.acknowledged, true);
  assert.equal(w.atCleanup.settled, false);
  assert.equal(w.calls.retain + w.calls.reconcile + w.calls.existingWrites, 0);
  assert.equal(w.e.calls.held, 0);
});

test("A2 (held_publication): a retained answer needing the archive fallback is refused without clearing the request", async () => {
  const w = await heldReplayWorld();
  const digest = await w.digest();
  w.client.releaseRecoveryCustody = async () => ({ run_id: w.runId, generation: 7, released: false, holds_released: 0,
    retained: true, reason: "held_publication_ref_missing" }) as never;
  const result = await w.resolve(digest);
  assert.equal(result.kind, "partial");
  assert.match(result.kind === "partial" ? result.reason : "", /^(held_fallback_required:|replay_ack_mismatch)/);
  assert.equal((await w.record()).finalRequest?.disposition.kind, "held_publication", "the held request is not cleared");
  assert.equal(w.calls.existingWrites + w.calls.reconcile, 0);
});

test("A2 (held_publication): a pre-POST disconnect writes nothing", async () => {
  const w = await heldReplayWorld();
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  w.producer.readRemediationRestrictionFacts = (async () => {
    w.request.abort();
    return { blocked: false, enrolled: false, fingerprint: "f", roots: [], flaggedOIDs: [] };
  }) as never;
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "client_disconnected" });
  await assertByteNeutral(w, bytes, refs);
});

/** The verifier inputs exactly as frozenCoverageProven derives them from the saved record. */
async function verifyInputs(w: ReplayWorld) {
  const rec = await w.record();
  const roots = [...new Set([...(rec.originalRoots ?? []).map(c => c.sha), rec.originalSourceSha!])].sort();
  const pin = `refs/uzi-coverage/${w.runId}/7/${rec.coverageDigest}`;
  const reused = rec.sourceSha === rec.inventoryCurrentSha;
  const verify = (over: { roots?: string[]; sha?: string; fingerprint?: string; reuse?: boolean } = {}) =>
    w.cache.verifyRecoveryCoverage(w.bare, w.f.context, over.roots ?? roots, rec.inventoryCurrentSha!,
      { sha: over.sha ?? rec.sourceSha, fingerprint: over.fingerprint ?? rec.coverageDigest! },
      (over.reuse ?? reused) ? { reuseCoveredCurrent: "required" } : {});
  return { rec, roots, pin, verify };
}

test("verifyRecoveryCoverage accepts the built coverage and writes no ref or object", async () => {
  const w = await replayWorld();
  const { verify } = await verifyInputs(w);
  const refs = w.refs(), objects = git(w.bare, "count-objects", "-v");
  assert.equal(await verify(), true);
  assert.equal(w.refs(), refs, "no ref written");
  assert.equal(git(w.bare, "count-objects", "-v"), objects, "no object written");
});

test("verifyRecoveryCoverage refuses a wrong fingerprint, a moved or missing pin, a wrong expected sha and a missing root", async () => {
  const w = await replayWorld();
  const { rec, roots, pin, verify } = await verifyInputs(w);
  assert.equal(await verify({ fingerprint: "f".repeat(64) }), false, "fingerprint mismatch");
  assert.equal(await verify({ sha: rec.inventoryCurrentSha === rec.sourceSha ? w.f.head : rec.inventoryCurrentSha! }), false,
    "expected sha differs from the pin");
  await assert.rejects(verify({ roots: [] }), /original roots/);
  const original = git(w.bare, "rev-parse", pin);
  git(w.bare, "update-ref", pin, w.f.attempts[1]!);
  assert.equal(await verify(), false, "pin moved to another commit");
  git(w.bare, "update-ref", "-d", pin);
  assert.equal(await verify(), false, "pin missing");
  git(w.bare, "update-ref", pin, original);
  assert.equal(await verify(), true);
  assert.ok(roots.length > 0);
});

test("verifyRecoveryCoverage refuses a root that is not an ancestor of the expected source", async () => {
  const w = await replayWorld();
  const { rec, roots, verify } = await verifyInputs(w);
  const tree = git(w.bare, "rev-parse", `${rec.inventoryCurrentSha}^{tree}`);
  const stray = git(w.bare, "commit-tree", tree, "-m", "stray root");
  const widened = [...roots, stray].sort();
  const fingerprint = createHash("sha256").update(JSON.stringify({ roots: widened, currentSha: rec.inventoryCurrentSha, tree })).digest("hex");
  // The pin and fingerprint are honest for the widened roots, yet the recorded source never descended from the stray root.
  git(w.bare, "update-ref", `refs/uzi-coverage/${w.runId}/7/${fingerprint}`, rec.sourceSha);
  assert.equal(await verify({ roots: widened, fingerprint }), false);
});

test("verifyRecoveryCoverage refuses while a bare mutation is pending", async () => {
  const w = await replayWorld();
  const { verify } = await verifyInputs(w);
  Object.assign(w.cache, { authority: { assertBareClear: async () => { throw new Error("pending bare mutation"); } } });
  await assert.rejects(verify(), /pending bare mutation/);
});

test("A2 refuses a coverage pin moved to another commit, byte-neutrally", async () => {
  const w = await replayWorld();
  const { pin } = await verifyInputs(w);
  git(w.bare, "update-ref", pin, w.f.attempts[1]!);
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "replay_precondition_unmet:frozen_coverage" });
  await assertByteNeutral(w, bytes, refs);
});

test("A2 never rebuilds the coverage: a missing coverage pin is a named refusal and the bare is not written", async () => {
  const w = await replayWorld();
  const pin = w.refs().split("\n").find(line => line.startsWith(`refs/uzi-coverage/${w.runId}/7/`))!.split(" ")[0]!;
  git(w.bare, "update-ref", "-d", pin);
  const digest = await w.digest();
  const bytes = await inventory(w.root);
  const refs = w.refs();
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "replay_precondition_unmet:frozen_coverage" });
  await assertByteNeutral(w, bytes, refs);
});

// ── run-level blockers: retained artifacts stay visible without any record generation ─────────────

const latch = () => latchResidueQuarantine({ cause: "fixture", site: "test" }, nullLogger());

/** Remove every journal entry of the run so only what the test adds remains. */
async function emptyJournal(w: World) {
  const dir = w.coordinator["runDir"](w.runId) as string;
  for (const name of await fs.readdir(dir)) await fs.rm(path.join(dir, name), { recursive: true, force: true });
  return dir;
}

async function describedBlockers(w: World, blocker: string) {
  const bytes = await inventory(w.root);
  const refs = w.refs();
  const d = await w.describe();
  assert.deepEqual(d, { kind: "described", runId: w.runId, generations: [], blockers: [blocker] });
  assert.deepEqual(await inventory(w.root), bytes, "describe wrote nothing");
  assert.equal(w.refs(), refs);
}

test("describe reports an orphan bundle, an orphan temp file, a settlement-only run and terminal protection with no generation", async () => {
  const bundle = await world();
  const dir = await emptyJournal(bundle);
  await fs.writeFile(path.join(dir, "orphan-capture.bundle"), "x");
  await describedBlockers(bundle, "unbound_artifact:orphan-capture.bundle");

  const tmp = await world();
  await fs.writeFile(path.join(await emptyJournal(tmp), "partial.tmp"), "x");
  await describedBlockers(tmp, "unbound_artifact:partial.tmp");

  const settlement = await world();
  await emptyJournal(settlement);
  const sdir = path.join(settlement.root, "data", "recovery-settlement", settlement.runId);
  await fs.mkdir(sdir, { recursive: true });
  await fs.writeFile(path.join(sdir, "x.json"), "{}");
  await describedBlockers(settlement, "settlement_record_unsupported");

  const terminal = await world();
  await emptyJournal(terminal);
  Object.assign(terminal.coordinator, { terminalRecordProtection: async () => true });
  await describedBlockers(terminal, "terminal_protection");
});

test("an orphan artifact next to a record is also a run-level blocker, and resolve still refuses it", async () => {
  const w = await world();
  await fs.writeFile(path.join(w.coordinator["runDir"](w.runId) as string, "stray.tmp"), "x");
  const d = await w.describe();
  assert.equal(d.kind === "described" && d.blockers.join(","), "unbound_artifact:stray.tmp");
  assert.equal(await w.classification(), "unbound_artifact:stray.tmp");
});

// ── refusal controls: an unreadable record and the residue quarantine ───────────────────────────────

test("an unreadable record is a run-level blocker for describe and a byte-neutral refusal for resolve", async () => {
  const unreadable = () => { throw new Error("fixture read failure"); };
  const w = await world();
  await refused(w, "inventory_incomplete", () => { Object.assign(w.coordinator, { readRecordResult: unreadable }); });
  const d = await w.describe();
  assert.deepEqual(d, { kind: "described", runId: w.runId, generations: [], blockers: ["inventory_incomplete"] });

  const a2 = await replayWorld();
  const digest = await a2.digest();
  const bytes = await inventory(a2.root);
  const refs = a2.refs();
  Object.assign(a2.coordinator, { readRecordResult: unreadable });
  assert.deepEqual(await a2.resolve(digest), { kind: "refused", reason: "inventory_incomplete" });
  await assertByteNeutral(a2, bytes, refs);
});

test("a residue quarantine refuses before the pre-check and again between the pre-check and the forget expectation", async () => {
  const before = await world();
  await refused(before, "worker_quarantined", () => { latch(); });
  resetResidueQuarantineForTests();
  const duringJ = await world();
  await refused(duringJ, "worker_quarantined", () => {
    Object.assign(duringJ.coordinator, { onForgetJournalLockedForTest: () => { latch(); } });
  });
  for (const ref of duringJ.e.refs) assert.match(duringJ.refs(), new RegExp(ref), "nothing was deleted");
  resetResidueQuarantineForTests();
});

test("A2 refuses a residue quarantine latched before the POST: zero POSTs, record bytes unchanged", async () => {
  resetResidueQuarantineForTests();
  const early = await replayWorld();
  const digest = await early.digest();
  const bytes = await inventory(early.root);
  const refs = early.refs();
  latch();
  assert.deepEqual(await early.resolve(digest), { kind: "refused", reason: "worker_quarantined" });
  await assertByteNeutral(early, bytes, refs);
  resetResidueQuarantineForTests();
});
