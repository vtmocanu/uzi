import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, mock, test } from "node:test";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { RecoveryExactHold } from "../src/protocol.js";
import { RunRunner } from "../src/runner.js";
import { setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { nullLogger } from "./helpers.js";
import { makeFakeProcRoot, plantInScope } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { binding, closed, environment, fixture, git, inventory, patch, type Variant } from "./fixtures/recovery-bindingless-compat.js";

// issue #3885 M2 U2: identity, consumer and process controls for the operator path. The suites below run the
// operator describe/resolve (RecoveryCoordinator) against (a) the REAL WorkerClient exact-hold validation over a
// fake transport and (b) the REAL RunRunner.withInventorySourceBoundary; only the API transport, the git
// inventory read and the fake /proc are controlled seams.

const roots: string[] = [];
const scratchDirs: string[] = [];
afterEach(async () => {
  mock.restoreAll();
  restoreHermeticView();
  for (const root of [...roots.splice(0), ...scratchDirs.splice(0)]) await fs.rm(root, { recursive: true, force: true });
});

const id = (n: number) => `31050000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const CAP = "recovery_held_publication_v2";

/** A real coordinator over a real Git fixture; the API client is a fake except where a test installs the real one. */
async function world(variant: Variant = "saved-final") {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "uzi-local-controls-"));
  roots.push(root);
  const f = await fixture(root, variant);
  const bare = f.context.barePath;
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
  e.state.allowFinal = true;
  f.record = await patch(coordinator, f.record, { exactHoldBinding: binding });
  const runId = f.context.runId;
  const request = f.record.finalRequest!;
  const exactBody = (): RecoveryExactHold => ({ ...closed(f), state: "released", run_status: "failed",
    // The saved archive FINAL carries no evidence of its own; the server's closed proof names "archive".
    release_evidence: request.evidence ?? "archive", final_receipt: request.disposition });
  client.getExactRecoveryHold = async () => exactBody();
  const posts: unknown[][] = [];
  const release = client.releaseRecoveryCustody.bind(client);
  client.releaseRecoveryCustody = async (...args: Parameters<typeof release>) => { posts.push(args); return release(...args); };
  const calls = { retain: 0, reconcile: 0, existingWrites: 0, recordWrites: 0, cleanup: 0 };
  const count = (name: string, key: keyof typeof calls) => {
    const original = (coordinator as unknown as Record<string, (...a: unknown[]) => unknown>)[name]!.bind(coordinator);
    Object.assign(coordinator, { [name]: (...a: unknown[]) => { calls[key]++; return original(...a); } });
  };
  count("retainFinal", "retain");
  count("reconcileInventory", "reconcile");
  count("writeExistingRecord", "existingWrites");
  count("writeRecordUnlocked", "recordWrites");
  producer.cleanupRecoveryGeneration = async () => {
    calls.cleanup++;
    for (const ref of e.refs) git(bare, "update-ref", "-d", ref);
    return "removed";
  };
  const requestAbort = new AbortController(), shutdownAbort = new AbortController();
  const sig = () => ({ requestSignal: requestAbort.signal, shutdownSignal: shutdownAbort.signal, deadlineAt: Date.now() + 60_000 });
  const describe = () => coordinator.describeLocalRecords(runId, sig());
  const refs = () => git(bare, "for-each-ref", "--format=%(refname) %(objectname)");
  /** Every byte under the fixture root and every ref: the "nothing touched" state. */
  const state = async () => ({ bytes: await inventory(root), refs: refs() });
  return {
    f, e, coordinator, producer, client, cache, root, bare, runId, posts, calls, exactBody, describe, refs, state,
    resolve: (confirm: string) => coordinator.resolveLocalRecord({ runId, generation: 7, confirm, ...sig() }),
    async classification(): Promise<string> {
      const d = await describe();
      assert.equal(d.kind, "described", JSON.stringify(d));
      return d.kind === "described" ? (d.generations[0]?.classification ?? d.blockers.join(",")) : "";
    },
    async digest(): Promise<string> {
      const d = await describe();
      const g = d.kind === "described" ? d.generations[0] : undefined;
      assert.ok(g?.digest, JSON.stringify(d));
      return g.digest;
    },
    /** The refusal left the journal, refs and bundle byte-identical and sent no release. */
    async assertUntouched(before: { bytes: Awaited<ReturnType<typeof inventory>>; refs: string }) {
      assert.deepEqual(await inventory(root), before.bytes, "every byte unchanged (record, bundle, pins)");
      assert.equal(refs(), before.refs, "every ref unchanged");
      assert.equal(posts.length, 0, "no release POST");
      assert.equal(calls.retain + calls.reconcile + calls.existingWrites + calls.recordWrites + calls.cleanup, 0,
        "no write or cleanup path ran (retainFinal included)");
    },
  };
}
type World = Awaited<ReturnType<typeof world>>;

// ── 1. identity: the REAL WorkerClient over a fake transport ─────────────────────────────────────────────

/** Install a fake fetch and a real, registered WorkerClient as the coordinator's exact-hold reader. */
async function realClient(w: World) {
  mock.restoreAll();
  const api = {
    worker: id(5),
    reads: 0,
    envelope: (): unknown => ({ run_id: w.runId, holds: [], exact_hold: w.exactBody() }),
    /** One-shot: runs while the next exact-hold GET is in flight (after the request left, before the answer). */
    inFlight: undefined as undefined | (() => Promise<void>),
  };
  mock.method(globalThis, "fetch", async (url: string | URL | Request) => {
    const u = new URL(String(url));
    if (u.pathname.endsWith("/register")) return Response.json({ worker_id: api.worker, protocol_features: [] });
    if (!u.pathname.includes("/recovery-holds")) throw new Error("unexpected request " + u.pathname);
    api.reads++;
    const hook = api.inFlight;
    api.inFlight = undefined;
    await hook?.();
    return Response.json(api.envelope());
  });
  const real = new WorkerClient("http://exact.test", "worker-token", "test", nullLogger());
  const register = (caps: string[] = [CAP]) => real.register("worker", undefined, 1, undefined, caps);
  await register();
  w.client.getExactRecoveryHold = (runId: string, generation: number, expected: { holdId: string; repoId: string }, signal?: AbortSignal) =>
    real.getExactRecoveryHold(runId, generation, expected, signal);
  return { api, real, register };
}

test("identity (real WorkerClient): a matching body is the positive control for replay and for the acknowledged retirement", async () => {
  const replay = await world("saved-final");
  const api = await realClient(replay);
  assert.equal(await replay.classification(), "case_a_replay");
  assert.deepEqual(await replay.resolve(await replay.digest()), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(replay.posts.length, 1, "exactly one release POST");
  assert.ok(api.api.reads >= 3, "describe, the pre-check and the finalizer read through the real client");

  const ack = await world("acknowledged");
  await realClient(ack);
  assert.equal(await ack.classification(), "case_a_ack");
  assert.deepEqual(await ack.resolve(await ack.digest()), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(ack.posts.length, 0, "the acknowledged path never posts");
});

const mismatches: Array<[string, (e: RecoveryExactHold) => unknown]> = [
  ["original_worker_id of another worker", e => ({ ...e, original_worker_id: id(9) })],
  ["a different run in the hold", e => ({ ...e, run_id: id(8) })],
  ["a different generation in the hold", e => ({ ...e, generation: 8 })],
  ["a different hold id", e => ({ ...e, hold_id: id(10) })],
  ["a different repo id", e => ({ ...e, repo_id: id(11) })],
  ["inventory_guarded false", e => ({ ...e, inventory_guarded: false })],
  ["an empty original_worker_identity", e => ({ ...e, original_worker_identity: "" })],
];

for (const variant of ["saved-final", "acknowledged"] as const) {
  for (const [name, mutate] of mismatches) {
    test(`identity (real WorkerClient, ${variant}): ${name} is exact_evidence_unavailable and nothing is touched`, async () => {
      const w = await world(variant);
      const { api } = await realClient(w);
      const digest = await w.digest();
      const before = await w.state();
      api.envelope = () => ({ run_id: w.runId, holds: [], exact_hold: mutate(w.exactBody()) });
      assert.equal(await w.classification(), "exact_evidence_unavailable");
      assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "exact_evidence_unavailable" });
      await w.assertUntouched(before);
    });
  }
  test(`identity (real WorkerClient, ${variant}): a different run in the response envelope is exact_evidence_unavailable`, async () => {
    const w = await world(variant);
    const { api } = await realClient(w);
    const digest = await w.digest();
    const before = await w.state();
    api.envelope = () => ({ run_id: id(8), holds: [], exact_hold: w.exactBody() });
    assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "exact_evidence_unavailable" });
    await w.assertUntouched(before);
  });
}

test("identity (real WorkerClient): a registration that changed while the request was in flight is exact_evidence_unavailable", async () => {
  // The worker is re-registered as another worker between the request and the response.
  const switched = await world("saved-final");
  const s = await realClient(switched);
  const digest = await switched.digest();
  const before = await switched.state();
  s.api.inFlight = async () => { s.api.worker = id(6); await s.register(); };
  assert.deepEqual(await switched.resolve(digest), { kind: "refused", reason: "exact_evidence_unavailable" });
  await switched.assertUntouched(before);

  // The same worker re-registers without the capability while the request is in flight.
  const dropped = await world("saved-final");
  const d = await realClient(dropped);
  const droppedDigest = await dropped.digest();
  const droppedBefore = await dropped.state();
  d.api.inFlight = async () => { await d.register([]); };
  assert.deepEqual(await dropped.resolve(droppedDigest), { kind: "refused", reason: "exact_evidence_unavailable" });
  await dropped.assertUntouched(droppedBefore);
  assert.equal(await dropped.classification(), "exact_evidence_unavailable", "and the client keeps refusing afterwards");
});

// ── 3. a plain failure before the POST ───────────────────────────────────────────────────────────────────

test("A2: a plain error before the POST is replay_failed_before_post, byte-neutral, with retainFinal never called", async () => {
  const w = await world("saved-final");
  const digest = await w.digest();
  const before = await w.state();
  // frozenCoverageProven awaits the owed-candidate enumeration unguarded: its error is the caller's.
  w.producer.enumerateOwedCandidates = async () => { throw new Error("fixture enumeration failure"); };
  assert.deepEqual(await w.resolve(digest), { kind: "refused", reason: "replay_failed_before_post" });
  await w.assertUntouched(before);
});

// ── 2. A2 through the PRODUCTION boundary RunRunner.withInventorySourceBoundary ──────────────────────────

/** The real RunRunner method as the coordinator's boundary, over a controlled git inventory read and (by default)
 *  the real quiescence reaper reading the (hermetic) fake /proc. `diagnostics` records each refusal the boundary
 *  itself reports, so a test proves WHICH guard fired. */
async function boundaryWorld(identityOf: (read: number) => string = () => "stable") {
  const w = await world("saved-final");
  const clonePath = path.join(w.root, "clone-under-test");
  await fs.mkdir(clonePath, { recursive: true });
  const reads = { inventory: 0, status: 0 };
  const fakeGit = {
    readInventoryCloneHeads: async () => {
      reads.inventory++;
      return { kind: "verified" as const, heads: [], foreignOwners: [],
        clones: [{ clonePath, branch: w.f.context.branch, runId: w.runId, identity: identityOf(reads.inventory) }] };
    },
    inventorySourceContentProof: async () => { reads.status++; return "clean" as const; },
  };
  const runner = new RunRunner({} as never, fakeGit as never, () => { throw new Error("no executor"); }, nullLogger(), 20, undefined, { pollMs: 5 });
  const diagnostics: Array<{ check: string }> = [];
  let entered = 0;
  Object.assign(w.coordinator, {
    withInventorySourceBoundary: (context: Record<string, unknown>, action: unknown) => {
      entered++;
      const report = context.reportDiagnostic as ((d: { check: string }) => unknown) | undefined;
      return (runner as unknown as Record<string, (c: unknown, a: unknown) => Promise<unknown>>)["withInventorySourceBoundary"]!({
        ...context, reportDiagnostic: (d: { check: string }) => { diagnostics.push(d); return report?.(d); },
      }, action);
    },
  });
  return { w, runner, clonePath, reads, diagnostics, entered: () => entered,
    tails: () => (runner as unknown as { executionTails: Map<string, unknown> })["executionTails"] };
}

// The production source proof is Linux-only (it refuses `non_linux_host` elsewhere), so the
// cases that need it to pass run on Linux; the control below pins the refusal everywhere else.
const LINUX_ONLY = { skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host" };

test("A2 through the production boundary: a quiescent, stable source is the positive control (one POST, retired)", LINUX_ONLY, async () => {
  const b = await boundaryWorld();
  assert.deepEqual(await b.w.resolve(await b.w.digest()), { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(b.entered(), 1, "the production boundary ran");
  assert.ok(b.reads.status >= 1, "the clone's worktree was inspected by the boundary");
  assert.equal(b.w.posts.length, 1);
  assert.deepEqual(b.diagnostics, []);
  assert.equal(b.tails().size, 0, "the boundary released its execution tail");
});

test("A2 through the production boundary: a non-Linux host refuses before the POST", { skip: process.platform === "linux" ? "non-Linux control" : false }, async () => {
  const b = await boundaryWorld();
  const digest = await b.w.digest();
  const before = await b.w.state();
  assert.deepEqual(await b.w.resolve(digest), { kind: "refused", reason: "source_boundary_retained" });
  assert.deepEqual(b.diagnostics, [{ check: "non_linux_host" }]);
  assert.equal(b.w.posts.length, 0, "no release request was sent");
  await b.w.assertUntouched(before);
  assert.equal(b.tails().size, 0);
});

test("A2 through the production boundary: an execution tail already present refuses before the POST", async () => {
  const b = await boundaryWorld();
  const digest = await b.w.digest();
  b.tails().set(b.w.runId, Promise.resolve());
  const before = await b.w.state();
  assert.deepEqual(await b.w.resolve(digest), { kind: "refused", reason: "source_boundary_retained" });
  assert.deepEqual(b.diagnostics, [{ check: "execution_tail_present" }]);
  assert.equal(b.reads.inventory, 0, "refused before any source read");
  await b.w.assertUntouched(before);
  assert.ok(b.tails().has(b.w.runId), "the other lane's tail is left alone");
});

test("A2 through the production boundary: a live process attributed to the clone refuses before the POST", LINUX_ONLY, async () => {
  const b = await boundaryWorld();
  const digest = await b.w.digest();
  const proc = makeFakeProcRoot();
  scratchDirs.push(proc);
  plantInScope(proc, 4242, b.clonePath);
  setQuiescenceViewForTests({ procRoot: proc });
  const before = await b.w.state();
  assert.deepEqual(await b.w.resolve(digest), { kind: "refused", reason: "source_boundary_retained" });
  assert.equal(b.diagnostics.length, 1);
  assert.equal(b.diagnostics[0]!.check, "process_not_quiescent_before_git");
  assert.equal(b.reads.status, 0, "refused before the runner's own git status");
  await b.w.assertUntouched(before);
  assert.equal(b.tails().size, 0);
});

test("A2 through the production boundary: a source identity that changes inside the boundary's own proof refuses", LINUX_ONLY, async () => {
  // Reads 1 and 2 are the two reads of the first proof; the second differs.
  const b = await boundaryWorld(read => (read === 1 ? "before" : "after"));
  const digest = await b.w.digest();
  const before = await b.w.state();
  assert.deepEqual(await b.w.resolve(digest), { kind: "refused", reason: "source_boundary_retained" });
  assert.deepEqual(b.diagnostics, [{ check: "source_identity_changed" }]);
  await b.w.assertUntouched(before);
});

test("A2 through the production boundary: a source identity that changes between the two proofs refuses before the POST", LINUX_ONLY, async () => {
  // The first proof (reads 1 and 2) is stable; the finalizer's own re-proof sees a different source.
  const b = await boundaryWorld(read => (read <= 2 ? "first-proof" : "second-proof"));
  const digest = await b.w.digest();
  const before = await b.w.state();
  const result = await b.w.resolve(digest);
  assert.equal(result.kind, "refused", JSON.stringify(result));
  assert.deepEqual(b.diagnostics, [{ check: "source_identity_changed" }]);
  assert.ok(b.reads.inventory >= 3, "the changed identity was seen by a later proof, not the first");
  await b.w.assertUntouched(before);
  assert.deepEqual(result, { kind: "refused", reason: "replay_precondition_unmet:source_proof" });
});
