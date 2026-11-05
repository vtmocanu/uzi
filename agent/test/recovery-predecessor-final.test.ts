// Issue #2969 M1: the coordinator side of a predecessor FINAL. A lower generation's guarded hold is settled while a
// later claim generation of the same run executes. Everything here drives the REAL RecoveryCoordinator through
// operateFrozenInventory with a `predecessor` option; only the api client, the Git producer and the runner's
// source boundary are fakes. Every refusal control carries a positive twin in the same harness so that a zero
// counter proves a refusal and not an inability to act.
import { it } from "node:test";
import assert from "node:assert/strict";
import { createHash, createHmac, randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RequestError } from "../src/client.js";
import {
  PREDECESSOR_REFUSAL_CLASSES, PredecessorAuthorityLost, RecoveryCoordinator, canonicalJson, classifyPredecessorRefusal,
  type FrozenOperationOptions, type InventorySourceBoundaryContext, type PredecessorMeasurements, type PredecessorOperation,
  type RecoveryOutcome, type RecoveryRecord,
} from "../src/recovery.js";
import type { OwedCandidate, PositiveOwedCandidateContext } from "../src/git.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const H = "a".repeat(40);
const H3 = "c".repeat(40);
const RUN = "00000000-0000-4000-8000-000000000001";
const HOLD = "00000000-0000-4000-8000-000000000007";
const REPO = "00000000-0000-4000-8000-000000000008";
const PUB = "00000000-0000-4000-8000-000000000009";
const CLOSURE = "recovery_prerequisite_closure_v1";
const FAR = "2099-01-01T00:00:00Z";
const PAST = "1970-01-01T00:00:00Z";

/** The fake producer's bundle, byte for byte: tests derive allowance sizes from it. */
function bundleBytes(sourceSha: string, thin: boolean): Buffer {
  const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 0])]);
  return Buffer.concat([Buffer.from(`# v2 git bundle\n${thin ? `-${H} retained dependency\n` : ""}${sourceSha} refs/heads/recovered-source\n\n`),
    pack, createHash("sha1").update(pack).digest()]);
}

interface HarnessOptions { thin?: boolean; bindingless?: boolean; v2?: boolean; closureFeature?: boolean; kind?: PositiveOwedCandidateContext["kind"] }

/** A frozen generation-7 record of a run whose generation 8 is executing, behind a real coordinator. */
async function harness(opts: HarnessOptions = {}) {
  const root = makeScratchRoot("predecessor-", os.tmpdir());
  const context: PositiveOwedCandidateContext = {
    runId: RUN, generation: 7, kind: opts.kind ?? "issue", branch: "task", barePath: path.join(root, "repo.git"),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) },
  };
  const s = {
    // The api's view.
    status: "failed", ownGeneration: 7, holdState: "open" as "open" | "released" | "discarded", receipt: undefined as unknown,
    v2: opts.v2 ?? true, closureFeature: opts.closureFeature ?? true, protocolFeatures: undefined as string[] | undefined,
    holdsOverride: undefined as undefined | (() => unknown[]), exactOverride: undefined as undefined | ((run: string, gen: number) => unknown),
    statusPatch: undefined as undefined | (() => Record<string, unknown>), reconcileAnswer: undefined as undefined | ((req: { generation: number }) => unknown),
    expires: FAR, now: 1000,
    // Injected faults and hooks.
    reserveError: undefined as Error | undefined, uploadError: undefined as Error | undefined, releaseError: undefined as Error | undefined,
    loseAck: false, produceHook: undefined as undefined | (() => Promise<void> | void), reserveHook: undefined as undefined | (() => Promise<void> | void),
    uploadHook: undefined as undefined | (() => Promise<void> | void), releaseHook: undefined as undefined | (() => Promise<void> | void),
    exactHook: undefined as undefined | (() => Promise<void> | void),
    proofRefused: false, executing: false, live: true, thin: opts.thin ?? false,
    // The run's physical world.
    candidates: [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(context)] }] as OwedCandidate[],
    cloneHeads: [] as string[], coverageSha: undefined as string | undefined, ancestors: new Set<string>([H]),
    // Counters.
    cleanups: 0, reserves: 0, uploads: 0, produced: 0, releases: [] as unknown[][], publishes: 0, reconciles: 0, listReads: 0, exactReads: 0,
    boundaries: [] as InventorySourceBoundaryContext[], proofs: 0,
  };
  const captures = new Map<string, { id: string; request: unknown; manifest?: { checksum: string; byte_size: number } }>();
  const client = {
    hasFeature: (name: string) => name === CLOSURE ? s.closureFeature : true,
    hasHeldPublicationV2: () => s.v2,
    get protocolFeatures() { return s.protocolFeatures; },
    getRunOwnership: async () => ({ status: s.status, claim_generation: s.ownGeneration, inventory_guarded: true }),
    listRecoveryHolds: async () => {
      s.listReads++;
      return { run_id: RUN, holds: s.holdsOverride ? s.holdsOverride() : s.holdState === "open"
        ? [{ hold_id: HOLD, generation: 7, inventory_guarded: true, has_available_capture: false }] : [] };
    },
    getExactRecoveryHold: async (run: string, gen: number) => {
      s.exactReads++;
      await s.exactHook?.();
      if (s.exactOverride) return s.exactOverride(run, gen);
      return {
        run_id: run, hold_id: HOLD, repo_id: REPO, generation: gen, state: s.holdState,
        original_worker_id: "original", original_worker_identity: "original", inventory_guarded: true,
        run_status: s.status, current_claim_generation: s.ownGeneration, claim_released: true,
        ...(s.holdState === "open" ? { live_worker_id: "original", live_run_id: run }
          : { release_evidence: s.holdState === "discarded" ? "owner_discard" : undefined, final_receipt: s.holdState === "released" ? s.receipt : undefined }),
      };
    },
    publishHeld: async () => { s.publishes++; return { ok: false, reason: "pack_invalid", httpStatus: 400 }; },
    reserveRecoveryCapture: async (_run: string, request: { idempotency_key: string }) => {
      s.reserves++;
      await s.reserveHook?.();
      if (s.reserveError) throw s.reserveError;
      let capture = captures.get(request.idempotency_key);
      if (!capture) { capture = { id: "server-" + (captures.size + 1), request: structuredClone(request) }; captures.set(request.idempotency_key, capture); }
      return { capture_id: capture.id, state: "preparing" };
    },
    getRecoveryCaptureStatus: async (_run: string, id: string) => {
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      const status: Record<string, unknown> = {
        capture_id: id, state: capture.manifest ? "available" : "preparing", manifest_bound: !!capture.manifest,
        checksum: capture.manifest?.checksum, byte_size: capture.manifest?.byte_size, expires_at: s.expires, reason: undefined,
        ...s.statusPatch?.(),
      };
      if (s.thin && s.closureFeature && !s.statusPatch) {
        status.prerequisite_closure = { state: "pinned", prerequisite_shas: [H], checksum: capture.manifest?.checksum };
      }
      return status;
    },
    uploadRecoveryBundle: async (_run: string, id: string, manifest: { checksum: string; byte_size: number }, stream: AsyncIterable<unknown>) => {
      s.uploads++;
      await s.uploadHook?.();
      for await (const chunk of stream) assert.ok(chunk);
      if (s.uploadError) throw s.uploadError;
      const capture = [...captures.values()].find(c => c.id === id);
      assert.ok(capture);
      capture.manifest = structuredClone(manifest);
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    reconcileRecoveryCapture: async (run: string, id: string, req: { generation: number }) => {
      s.reconciles++;
      if (s.reconcileAnswer) return s.reconcileAnswer(req);
      // The api answers a committed release with its receipt, like the real reconciliation does.
      return s.receipt
        ? { run_id: run, generation: req.generation, capture_id: id, outcome: "accepted", final_receipt: structuredClone(s.receipt) }
        : { run_id: run, generation: req.generation, capture_id: id, outcome: "replaceable" };
    },
    releaseRecoveryCustody: async (...args: unknown[]) => {
      s.releases.push(structuredClone(args));
      await s.releaseHook?.();
      if (s.releaseError) throw s.releaseError;
      const disposition = args[3];
      if (s.holdState === "discarded") throw new RequestError("POST", "/release", 409, JSON.stringify({ reason: "hold_not_open" }));
      if (s.holdState === "released") assert.deepEqual(disposition, s.receipt, "a committed FINAL cannot be replaced");
      s.receipt = structuredClone(disposition);
      s.holdState = "released";
      if (s.loseAck) throw new Error("ACK lost");
      return { run_id: args[0], generation: args[1] as number, released: true, holds_released: 1 };
    },
  };
  const git = {
    worktreeStatus: async () => [] as string[],
    recoverySettlementRoot: path.join(root, "settlement"),
    deleteRecoveryPin: async () => undefined,
    cleanupRecoveryGeneration: async () => { s.cleanups++; return "removed"; },
    readInventoryCloneHeads: async () => ({ kind: "verified", heads: s.cloneHeads, clones: [], foreignOwners: [] }),
    ancestry: async (_bare: string, head: string) => s.ancestors.has(head) ? "ancestor" : "unknown",
    enumerateOwedCandidates: async () => structuredClone(s.candidates),
    discoverOwedCandidates: async () => [{ context, candidates: structuredClone(s.candidates) }],
    resolveRecoveryBareDir: async () => context.barePath,
    buildRecoveryCoverage: async (_bare: string, _ctx: unknown, roots: string[], currentSha: string) => {
      const fingerprint = createHash("sha256").update(JSON.stringify({ roots: [...roots].sort(), currentSha })).digest("hex");
      return { sha: s.coverageSha ?? fingerprint.slice(0, 40), fingerprint };
    },
    fetchDefaultTip: async () => { throw new Error("guarded capture must be credential free"); },
    produceRecoveryBundle: async (_bare: string, o: { outPath: string; sourceSha: string }) => {
      s.produced++;
      await s.produceHook?.();
      const bytes = bundleBytes(o.sourceSha, s.thin);
      await fs.writeFile(o.outPath, bytes);
      return { bundlePath: o.outPath, byteSize: bytes.length, checksum: createHash("sha256").update(bytes).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const log = nullLogger();
  const coordinator = new RecoveryCoordinator({
    recoveryRoot: path.join(root, "journal"), workerToken: "local-worker-fixture", log, client: client as never, git: git as never,
    now: () => s.now, isExecuting: () => s.executing,
    withInventorySourceBoundary: async (ctx, action) => {
      s.boundaries.push(ctx);
      await action(async () => { s.proofs++; return !s.proofRefused; });
      return "passed";
    },
  });
  const record = await coordinator.freezeInventory({
    context, currentSha: H, defaultBranch: "main",
    ...(opts.bindingless ? {} : { repoId: REPO, heldPolicy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: [".custom-ci/**"], extraFloors: [] } }),
  });
  assert.ok(record, "the generation-7 identity freezes while that generation is the executing claim's predecessor");
  // The successor claim (generation 8) now executes the run.
  s.ownGeneration = 8; s.status = "running"; s.executing = true;
  s.listReads = 0; s.exactReads = 0;

  const calls = { captureCycle: 0, installBundle: 0, writes: 0, freeze: 0, reconstruct: 0 };
  const spied = coordinator as unknown as Record<string, (...args: unknown[]) => unknown>;
  for (const [name, key] of [["captureCycle", "captureCycle"], ["installBundle", "installBundle"], ["writeRecordUnlocked", "writes"],
    ["freezeInventory", "freeze"], ["newReconstructionRecords", "reconstruct"]] as const) {
    const original = spied[name]!.bind(coordinator);
    spied[name] = (...args: unknown[]) => { calls[key]++; return original(...args); };
  }

  const token = {};
  const allowance = { remaining: 64 * 1024 * 1024 };
  const measurements: PredecessorMeasurements = { uploadedBytes: 0, chargedBytes: 0, uploadCount: 0, retainedUnuploadedBytes: 0, reusedBytes: 0 };
  const options = (over: Partial<PredecessorOperation> = {}, extra: FrozenOperationOptions = {}): FrozenOperationOptions => ({
    ...extra, predecessor: { claimGeneration: 8, repoId: REPO, boundary: token, live: () => s.live, uploadAllowance: allowance, measurements, ...over },
  });
  const operate = (over: Partial<PredecessorOperation> = {}, extra: FrozenOperationOptions = {}, from: RecoveryRecord = record,
    signal?: AbortSignal) =>
    coordinator.operateFrozenInventory({ record: from, barePath: context.barePath, defaultBranch: "main", ...(signal ? { signal } : {}) },
      options(over, extra));
  const saved = async (): Promise<RecoveryRecord> => (await coordinator.inspect(RUN)).find(r => r.generation === 7)!;
  const file = (r: Pick<RecoveryRecord, "runId" | "captureId">) => path.join(root, "journal", r.runId, r.captureId + ".json");
  const key = createHmac("sha256", "local-worker-fixture").update("uzi-recovery-journal-v1").digest();
  /** Rewrite a journal record with a valid MAC (the harness's stand-in for state the coordinator produced earlier). */
  const patch = async (changes: Partial<RecoveryRecord>, target: Pick<RecoveryRecord, "runId" | "captureId"> = record) => {
    const envelope = JSON.parse(await fs.readFile(file(target), "utf8"));
    delete envelope.mac;
    Object.assign(envelope, changes);
    await fs.writeFile(file(target), JSON.stringify({ ...envelope, mac: createHmac("sha256", key).update(canonicalJson(envelope)).digest("hex") }));
  };
  /** Plant an authenticated record of ANOTHER run, copied from this one. */
  const plant = async (changes: Partial<RecoveryRecord>): Promise<RecoveryRecord> => {
    const planted = { ...(await saved()), ...changes, runId: "other-run", captureId: randomUUID() } as RecoveryRecord;
    await fs.mkdir(path.join(root, "journal", planted.runId), { recursive: true });
    await fs.writeFile(file(planted), JSON.stringify({ ...planted, mac: createHmac("sha256", key).update(canonicalJson(planted)).digest("hex") }));
    return planted;
  };
  const snapshot = async () => ({ bytes: await fs.readFile(file(record)), files: (await fs.readdir(path.join(root, "journal", RUN))).sort() });
  const sideEffects = () => ({ captureCycle: calls.captureCycle, installBundle: calls.installBundle, writes: calls.writes, produced: s.produced,
    reserves: s.reserves, uploads: s.uploads, publishes: s.publishes, releases: s.releases.length, reconciles: s.reconciles });
  return { root, context, s, coordinator, record, calls, token, allowance, measurements, operate, saved, patch, plant, snapshot, sideEffects,
    options, close: () => fs.rm(root, { recursive: true, force: true }) };
}

type Harness = Awaited<ReturnType<typeof harness>>;
const NONE = { captureCycle: 0, installBundle: 0, writes: 0, produced: 0, reserves: 0, uploads: 0, publishes: 0, releases: 0, reconciles: 0 };

/** The outcome of a clean positive run: a produced, uploaded, FINAL-released and acknowledged archive. */
async function assertAcked(h: Harness, outcome: RecoveryOutcome): Promise<void> {
  assert.equal(outcome.held, "acked");
  assert.equal(h.s.releases.length, 1);
  assert.equal((await h.saved()).finalAcknowledged, true);
}

// ── D2 step 5: the fresh exact observation ─────────────────────────────────────────────────────────────────────

it("a valid observation lets a predecessor produce, upload, release and acknowledge its own generation", async () => {
  const h = await harness();
  try {
    const outcome = await h.operate();
    await assertAcked(h, outcome);
    assert.equal(h.s.produced, 1); assert.equal(h.s.reserves, 1); assert.equal(h.s.uploads, 1);
    assert.equal(h.s.publishes, 0, "a predecessor never publishes a held ref");
    const release = h.s.releases[0]!;
    assert.equal(release[1], 7);
    assert.equal((release[3] as { kind: string }).kind, "archive");
    assert.equal(h.s.boundaries.length > 0 && h.s.boundaries.every(c => c.predecessor === h.token), true,
      "every source boundary for the scope's own generation carries the runner's token");
    assert.equal(h.measurements.uploadCount, 1);
  } finally { await h.close(); }
});

const refusals: Array<[string, (h: Harness) => void, Partial<PredecessorOperation>?]> = [
  ["no listed hold", h => { h.s.holdsOverride = () => []; }],
  ["two guarded holds for the generation", h => { h.s.holdsOverride = () => [
    { hold_id: HOLD, generation: 7, inventory_guarded: true }, { hold_id: "00000000-0000-4000-8000-0000000000aa", generation: 7, inventory_guarded: true }]; }],
  ["only an unguarded hold", h => { h.s.holdsOverride = () => [{ hold_id: HOLD, generation: 7, inventory_guarded: false }]; }],
  ["a hold of another generation", h => { h.s.holdsOverride = () => [{ hold_id: HOLD, generation: 6, inventory_guarded: true }]; }],
  ["a non-canonical listed hold id", h => { h.s.holdsOverride = () => [{ hold_id: "hold-7", generation: 7, inventory_guarded: true }]; }],
  ["a non-canonical repo id", () => undefined, { repoId: "repo-8" }],
  ["a different repo id in the exact hold", h => { h.s.exactOverride = (run, gen) => ({ ...openExact(h, run, gen), repo_id: "00000000-0000-4000-8000-0000000000bb" }); }],
  ["a different hold id in the exact hold", h => { h.s.exactOverride = (run, gen) => ({ ...openExact(h, run, gen), hold_id: "00000000-0000-4000-8000-0000000000cc" }); }],
  ["a different generation in the exact hold", h => { h.s.exactOverride = (run) => ({ ...openExact(h, run, 6) }); }],
  ["a different run in the exact hold", h => { h.s.exactOverride = (_run, gen) => ({ ...openExact(h, "other-run", gen) }); }],
  ["an unguarded exact hold", h => { h.s.exactOverride = (run, gen) => ({ ...openExact(h, run, gen), inventory_guarded: false }); }],
  ["an absent exact hold", h => { h.s.exactOverride = () => undefined; }],
  ["a released hold", h => { h.s.holdState = "released"; h.s.holdsOverride = () => [{ hold_id: HOLD, generation: 7, inventory_guarded: true }]; }],
  ["a discarded hold", h => { h.s.holdState = "discarded"; h.s.holdsOverride = () => [{ hold_id: HOLD, generation: 7, inventory_guarded: true }]; }],
  ["a completed run", h => { h.s.status = "completed"; }],
  ["a failed run", h => { h.s.status = "failed"; }],
  ["a cancelled run", h => { h.s.status = "cancelled"; }],
  ["an unknown run status", h => { h.s.status = "teleporting"; }],
  ["a current claim generation equal to the hold's", h => { h.s.ownGeneration = 7; }],
  ["a current claim generation below the hold's", h => { h.s.ownGeneration = 6; }],
  ["a non-integer current claim generation", h => { h.s.exactOverride = (run, gen) => ({ ...openExact(h, run, gen), current_claim_generation: "8" }); }],
  ["held publication v2 absent", h => { h.s.v2 = false; }],
  ["a scope that is not live", h => { h.s.live = false; }],
  ["a claim generation equal to the record's (misuse)", () => undefined, { claimGeneration: 7 }],
  ["a claim generation below the record's (misuse)", () => undefined, { claimGeneration: 3 }],
];

function openExact(h: Harness, run: string, gen: number): Record<string, unknown> {
  return { run_id: run, hold_id: HOLD, repo_id: REPO, generation: gen, state: "open", original_worker_id: "original",
    original_worker_identity: "original", inventory_guarded: true, run_status: h.s.status, current_claim_generation: h.s.ownGeneration,
    claim_released: true, live_worker_id: "original", live_run_id: run };
}

for (const [name, arrange, over] of refusals) {
  it(`observation refusal (${name}) stops before any capture, bundle, reserve, upload, write or FINAL`, async () => {
    const h = await harness();
    try {
      arrange(h);
      const before = await h.snapshot();
      Object.assign(h.calls, { captureCycle: 0, installBundle: 0, writes: 0 });
      const outcome = await h.operate(over ?? {});
      assert.equal(outcome.reason, "predecessor_observation_refused");
      assert.equal(outcome.held, "kept");
      assert.deepEqual(h.sideEffects(), NONE);
      assert.equal(outcome.predecessor, undefined, "an observation refusal is an ordinary skip, not an integrity failure");
      const after = await h.snapshot();
      assert.ok(before.bytes.equals(after.bytes), "the journal record is byte-identical");
      assert.deepEqual(after.files, before.files, "no bundle or temp file appeared");
    } finally { await h.close(); }
  });
}

it("a refusal and a valid observation differ only by the observed facts (positive twin for the matrix)", async () => {
  const h = await harness();
  try {
    h.s.ownGeneration = 7;
    assert.equal((await h.operate()).reason, "predecessor_observation_refused");
    assert.deepEqual(h.sideEffects(), NONE);
    h.s.ownGeneration = 8;
    await assertAcked(h, await h.operate());
    assert.ok(h.calls.captureCycle > 0 && h.calls.installBundle > 0 && h.calls.writes > 0 && h.s.produced > 0);
  } finally { await h.close(); }
});

it("an exception while observing is a refusal, never a capture", async () => {
  const h = await harness();
  try {
    h.s.exactOverride = () => { throw new Error("transport down"); };
    const outcome = await h.operate();
    assert.equal(outcome.reason, "predecessor_observation_refused");
    assert.deepEqual(h.sideEffects(), NONE);
  } finally { await h.close(); }
});

// ── D2 steps 1-4: skips and the observation-free paths ─────────────────────────────────────────────────────────

for (const [name, changes, held] of [
  ["a held-create-uncertain record", () => ({ heldCreateUncertain: true }), "kept"],
  ["a saved held_publication FINAL", (r: RecoveryRecord) => ({ finalRequest: { disposition: { kind: "held_publication", publication_id: PUB,
    source_sha: r.sourceSha, coverage_digest: r.coverageDigest! } } }), "kept"],
  ["a settled hold", () => ({ holdSettlement: { run_id: RUN, hold_id: HOLD, repo_id: REPO, generation: 7, state: "discarded",
    original_worker_id: "o", original_worker_identity: "o", inventory_guarded: true, run_status: "running", current_claim_generation: 8,
    claim_released: true, release_evidence: "owner_discard" } }), "settled"],
  ["an acknowledged FINAL", (r: RecoveryRecord) => ({ serverCaptureId: "server-1", finalAcknowledged: true, finalRequest: { disposition: { kind: "archive",
    capture_id: "server-1", source_sha: r.sourceSha, coverage_digest: r.coverageDigest! } } }), "acked"],
] as const) {
  it(`skips ${name} with no observation and no side effect`, async () => {
    const h = await harness();
    try {
      await h.patch(changes(h.record) as unknown as Partial<RecoveryRecord>);
      const before = await h.snapshot();
      const outcome = await h.operate();
      assert.equal(outcome.held, held);
      assert.deepEqual(h.sideEffects(), NONE);
      assert.equal(h.s.listReads, 0, "no fresh hold observation was attempted");
      assert.ok((await h.snapshot()).bytes.equals(before.bytes));
    } finally { await h.close(); }
  });
}

/** Leaves a saved, unacknowledged archive FINAL whose release the api may or may not have committed. */
async function savedArchiveFinal(h: Harness, lost: boolean): Promise<RecoveryRecord> {
  if (lost) {
    h.s.loseAck = true;
    h.s.reconcileAnswer = () => { throw new Error("reconciliation unavailable"); };
  } else h.s.releaseError = new Error("network down before the answer");
  const outcome = await h.operate();
  assert.notEqual(outcome.held, "acked");
  h.s.loseAck = false; h.s.releaseError = undefined; h.s.reconcileAnswer = undefined;
  const record = await h.saved();
  assert.equal(record.finalRequest?.disposition.kind, "archive");
  assert.equal(record.finalAcknowledged, undefined);
  return record;
}

it("a lost ACK whose closed hold is journaled is skipped in flight with zero POSTs, then replayed by the ordinary pass", async () => {
  const h = await harness();
  try {
    const record = await savedArchiveFinal(h, true);
    assert.equal(h.s.holdState, "released", "the api committed the release behind the lost ACK");
    const request = record.finalRequest;
    const posts = h.s.releases.length;
    const outcome = await h.operate();
    assert.equal(outcome.held, "settled", "a journaled closed hold is an unconditional pre-entry skip");
    assert.equal(h.s.releases.length, posts, "no POST in flight");
    const settled = await h.saved();
    assert.equal(settled.holdSettlement?.state, "released");
    assert.equal(settled.finalAcknowledged, undefined);
    assert.equal(h.s.produced, 1); assert.equal(h.s.uploads, 1); assert.equal(h.s.reserves, 1);
    // After the flight the run is no longer executing and the ordinary re-drive replays exactly that request.
    h.s.live = false; h.s.executing = false; h.s.status = "failed";
    await h.coordinator.resumePending(undefined, [settled]);
    assert.equal(h.s.releases.length, posts + 1);
    assert.deepEqual(h.s.releases.at(-1)![3], request!.disposition);
  } finally { await h.close(); }
});

it("a lost ACK with the hold not yet journaled as settled replays the identical request inside the flight", async () => {
  const h = await harness();
  try {
    const record = await savedArchiveFinal(h, false);
    assert.equal(h.s.holdState, "open");
    const outcome = await h.operate();
    assert.equal(outcome.held, "acked");
    assert.deepEqual(h.s.releases.at(-1)![3], record.finalRequest!.disposition);
    assert.deepEqual((await h.saved()).finalRequest, record.finalRequest);
  } finally { await h.close(); }
});

it("a saved archive FINAL replays with no observation when the api refused the first attempt before committing", async () => {
  const h = await harness();
  try {
    const record = await savedArchiveFinal(h, false);
    assert.equal(h.s.holdState, "open");
    h.s.status = "teleporting"; // observation would now refuse
    const outcome = await h.operate();
    assert.equal(outcome.held, "acked");
    assert.deepEqual(h.s.releases.at(-1)![3], record.finalRequest!.disposition);
  } finally { await h.close(); }
});

it("a concurrent discard after the FINAL was saved yields no ACK and no replacement", async () => {
  const h = await harness();
  try {
    const record = await savedArchiveFinal(h, false);
    h.s.holdState = "discarded";
    const releases = h.s.releases.length;
    const outcome = await h.operate();
    assert.notEqual(outcome.held, "acked");
    assert.equal(h.s.releases.length, releases, "a discarded hold never gets a release POST");
    const after = await h.saved();
    assert.equal(after.finalAcknowledged, undefined);
    assert.deepEqual(after.finalRequest, record.finalRequest);
  } finally { await h.close(); }
});

it("a hold discarded mid-upload mints no FINAL and sends no release", async () => {
  const h = await harness();
  try {
    h.s.uploadHook = () => { h.s.holdState = "discarded"; };
    const outcome = await h.operate();
    assert.notEqual(outcome.held, "acked");
    assert.equal(h.s.releases.length, 0);
    const after = await h.saved();
    assert.equal(after.finalRequest, undefined, "a closed hold mints no FINAL request");
    assert.equal(after.finalAcknowledged, undefined);
  } finally { await h.close(); }
});

/** An uploaded self-contained archive that has no FINAL request yet. */
async function uploadedSelfContained(h: Harness): Promise<RecoveryRecord> {
  await assertAcked(h, await h.operate());
  await h.patch({ finalRequest: undefined, finalAcknowledged: undefined, holdSettlement: undefined });
  h.s.holdState = "open"; h.s.receipt = undefined; h.s.releases.length = 0;
  const record = await h.saved();
  assert.equal(record.state, "uploaded"); assert.equal(record.selfContained, true); assert.equal(record.prerequisiteShas?.length, 0);
  return record;
}

it("an uploaded self-contained archive is finalized with no observation", async () => {
  const h = await harness();
  try {
    await uploadedSelfContained(h);
    h.s.status = "teleporting"; // an observation would refuse
    const sideEffects = h.sideEffects();
    const outcome = await h.operate();
    assert.equal(outcome.held, "acked");
    assert.equal(h.s.releases.length, 1);
    assert.equal(h.calls.captureCycle, sideEffects.captureCycle, "no capture cycle ran");
    assert.equal(h.s.produced, sideEffects.produced);
  } finally { await h.close(); }
});

// ── Grants 1, 3, 4 stay denied; grant 2 is strict ──────────────────────────────────────────────────────────────

it("held-create-uncertain and bindingless records mint nothing and publish nothing for a predecessor, unlike an ordinary caller", async () => {
  const h = await harness();
  try {
    await h.patch({ heldCreateUncertain: true });
    const outcome = await h.operate();
    assert.equal(outcome.held, "kept");
    assert.deepEqual(h.sideEffects(), NONE, "grant 3 and any held probe are unreachable");
    // The same journal for an ordinary caller of an inactive run does reach the held probe.
    h.s.executing = false; h.s.status = "failed";
    await h.coordinator.operateFrozenInventory({ record: h.record, barePath: h.context.barePath, defaultBranch: "main" });
    assert.ok(h.s.publishes > 0, "control: the ordinary path probes the held publication");
  } finally { await h.close(); }
});

it("a bindingless v2 record is not minted (grant 1) when the observation refuses, but is by an ordinary caller", async () => {
  const h = await harness({ bindingless: true });
  try {
    h.s.status = "completed";
    const refused = await h.operate();
    assert.equal(refused.reason, "predecessor_observation_refused");
    assert.deepEqual(h.sideEffects(), NONE);
    h.s.executing = false;
    await h.coordinator.operateFrozenInventory({ record: h.record, barePath: h.context.barePath, defaultBranch: "main" });
    assert.ok(h.s.releases.length > 0, "control: grant 1 mints for an ordinary caller of the same record");
  } finally { await h.close(); }
});

it("v1 and closure-only feature views mint nothing for a predecessor (grant 4), unlike an ordinary caller", async () => {
  const h = await harness({ thin: true, v2: false });
  try {
    h.s.protocolFeatures = [CLOSURE, "recovery_inventory_v1"];
    const refused = await h.operate();
    assert.equal(refused.reason, "predecessor_observation_refused");
    assert.deepEqual(h.sideEffects(), NONE);
    h.s.executing = false;
    await h.coordinator.operateFrozenInventory({ record: h.record, barePath: h.context.barePath, defaultBranch: "main" });
    assert.equal(h.s.releases.length, 1, "control: grant 4 mints the thin FINAL for an ordinary caller");
  } finally { await h.close(); }
});

it("strict greater-than: a thin archive mints and releases only when the current claim generation is above the hold's", async () => {
  const equal = await harness({ thin: true });
  try {
    equal.s.ownGeneration = 7;
    assert.equal((await equal.operate()).reason, "predecessor_observation_refused");
    assert.deepEqual(equal.sideEffects(), NONE);
  } finally { await equal.close(); }
  const above = await harness({ thin: true });
  try {
    const outcome = await above.operate();
    await assertAcked(above, outcome);
    const record = await above.saved();
    assert.equal(record.selfContained, false);
    assert.deepEqual(record.prerequisiteShas, [H]);
    assert.equal((above.s.releases[0]![3] as { kind: string }).kind, "archive");
  } finally { await above.close(); }
});

it("a thin archive without the closure feature is a plain mint denial: uploaded, not released, no integrity failure", async () => {
  const h = await harness({ thin: true, closureFeature: false });
  try {
    const outcome = await h.operate();
    assert.notEqual(outcome.held, "acked");
    assert.equal(h.s.releases.length, 0);
    assert.equal(outcome.predecessor, undefined, "mint_denied is ordinary");
    assert.equal((await h.saved()).state, "uploaded");
  } finally { await h.close(); }
});

// ── D2 step 8: frozen identity ─────────────────────────────────────────────────────────────────────────────────

it("the journaled identity survives drift: later roots and a moved default tip change nothing and nothing is re-frozen", async () => {
  const h = await harness();
  try {
    const identity = (r: RecoveryRecord) => canonicalJson({ sourceSha: r.sourceSha, coverageDigest: r.coverageDigest, originalSourceSha: r.originalSourceSha,
      originalRoots: r.originalRoots, inventoryCurrentSha: r.inventoryCurrentSha, coverageContext: r.coverageContext, exactHoldBinding: r.exactHoldBinding,
      heldPolicy: r.heldPolicy });
    const before = identity(h.record);
    // The successor advances the roots (a later-generation context) and its clone head; the default tip moves on.
    h.s.candidates.push({ sha: H3, pinRef: "refs/owed/c", contexts: [{ ...h.context, generation: 8 }] });
    h.s.cloneHeads = [H3];
    Object.assign(h.context.defaultIdentity, { sha: "e".repeat(40) });
    await assertAcked(h, await h.operate());
    assert.equal(identity(await h.saved()), before);
    assert.equal(h.calls.freeze, 0, "freezeInventory is never called for a predecessor");
    assert.equal(h.calls.reconstruct, 0, "no reconstruction of the journal identity");
    // The ordinary caller does reconstruct against the same world: a different identity is frozen.
    const refrozen = await h.coordinator.freezeInventory({ context: { ...h.context, generation: 7 }, currentSha: "e".repeat(40),
      defaultBranch: "main", repoId: REPO, locallyQuiescent: true });
    assert.equal(h.calls.freeze, 1);
    assert.notEqual(refrozen?.coverageDigest, h.record.coverageDigest, "control: drift changes what an ordinary freeze would capture");
  } finally { await h.close(); }
});

// ── D3: saved-request preservation ─────────────────────────────────────────────────────────────────────────────

for (const [name, status] of [
  ["expired", () => ({ state: "expired" })],
  ["expired while available", () => ({ state: "available", expires_at: PAST })],
  ["quota-exhausted", () => ({ state: "needs_action", reason: "storage quota exceeded" })],
] as const) {
  it(`a saved FINAL survives a ${name} reconcile for a predecessor, and is replaced for an ordinary caller`, async () => {
    const h = await harness();
    try {
      const record = await savedArchiveFinal(h, false);
      h.s.statusPatch = status;
      const reserves = h.s.reserves;
      const outcome = await h.operate();
      assert.notEqual(outcome.held, "acked");
      assert.ok(h.s.reconciles > 0, "the reconcile path ran");
      const after = await h.saved();
      assert.deepEqual(after.finalRequest, record.finalRequest, "the saved request is untouched");
      assert.equal(after.serverCaptureId, record.serverCaptureId);
      assert.equal(after.bundlePath, record.bundlePath);
      assert.equal(after.state, "uploaded");
      assert.equal(h.s.reserves, reserves, "no replacement capture was reserved");
      // Control: the same journal and server answers, handled by an ordinary caller of an inactive run, replaces it.
      h.s.executing = false; h.s.status = "failed";
      await h.coordinator.captureAndUpload({ record: h.record, barePath: h.context.barePath, defaultBranch: "main" });
      assert.ok(h.s.reserves > reserves, "control: the ordinary path resets and reserves a replacement");
    } finally { await h.close(); }
  });
}

it("an expired capture with NO saved request is still reset and replaced by a predecessor", async () => {
  const h = await harness();
  try {
    const record = await uploadedSelfContained(h);
    h.s.statusPatch = () => ({ state: "expired" });
    const reserves = h.s.reserves;
    await h.operate();
    assert.ok(h.s.reconciles > 0);
    const after = await h.saved();
    assert.notEqual(after.serverCaptureId, record.serverCaptureId);
    assert.ok(h.s.reserves > reserves || after.state === "bundled", "the unsaved identity was reset for a fresh capture");
  } finally { await h.close(); }
});

// ── D3: upload allowance, quota, uncertain commit ──────────────────────────────────────────────────────────────

it("an upload larger than the remaining allowance is refused before the reserve and writes nothing", async () => {
  const h = await harness();
  try {
    const size = bundleBytes(h.record.sourceSha, false).length;
    h.allowance.remaining = size - 1;
    const outcome = await h.operate();
    assert.equal(outcome.reason, "predecessor_upload_budget");
    assert.equal(h.s.reserves, 0); assert.equal(h.s.uploads, 0); assert.equal(h.s.releases.length, 0);
    assert.equal(h.allowance.remaining, size - 1, "nothing was charged");
    assert.equal(h.measurements.retainedUnuploadedBytes, size);
    assert.equal(h.measurements.chargedBytes, 0);
    const writesAtRefusal = h.calls.writes;
    const record = await h.saved();
    assert.equal(record.state, "bundled", "the bundle stays journaled as local evidence");
    assert.equal(record.serverCaptureId, undefined);
    assert.equal(outcome.predecessor, undefined, "budget exhaustion is ordinary");
    // The same record with enough allowance reuses that bundle and completes (positive twin).
    h.allowance.remaining = size;
    const retry = await h.operate();
    await assertAcked(h, retry);
    assert.ok(h.calls.writes > writesAtRefusal);
    assert.equal(h.s.produced, 1, "the retained bundle was reused, not re-produced");
    assert.equal(h.measurements.reusedBytes, size);
    assert.equal(h.allowance.remaining, 0, "an allowance equal to the bundle is exactly spent");
    assert.equal(h.measurements.chargedBytes, size); assert.equal(h.measurements.uploadedBytes, size); assert.equal(h.measurements.uploadCount, 1);
    assert.equal(h.measurements.retainedUnuploadedBytes, 0, "a refused bundle that later uploads is not counted as retained twice");
  } finally { await h.close(); }
});

it("an upload whose answer is lost stays charged and is not retried inside the operation", async () => {
  const h = await harness();
  try {
    const size = bundleBytes(h.record.sourceSha, false).length;
    h.allowance.remaining = size * 2;
    h.s.uploadError = new Error("connection reset after the body was sent");
    const outcome = await h.operate();
    assert.notEqual(outcome.state, "uploaded");
    assert.equal(h.s.uploads, 1, "one dispatch, no retry");
    assert.equal(h.allowance.remaining, size, "the lost answer still spent the bytes");
    assert.equal(h.measurements.chargedBytes, size);
    assert.equal(h.measurements.uploadedBytes, 0); assert.equal(h.measurements.uploadCount, 0);
    assert.equal(h.measurements.retainedUnuploadedBytes, size);
    assert.equal(h.s.releases.length, 0);
    assert.equal((await h.saved()).state, "needs_action");
  } finally { await h.close(); }
});

it("a typed quota refusal is journaled once with no retry inside the operation", async () => {
  const h = await harness();
  try {
    h.s.uploadError = new RequestError("PUT", "/upload", 507, JSON.stringify({ error: "quota exceeded", reason: "quota" }));
    const outcome = await h.operate();
    assert.equal(outcome.reason, "storage_quota_exceeded");
    assert.equal(h.s.uploads, 1); assert.equal(h.s.reserves, 1);
    const record = await h.saved();
    assert.equal(record.archiveRefusal, "quota");
    assert.equal(record.reason, "storage_quota_exceeded");
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

it("a bare 507 is a transient failure, never a quota refusal", async () => {
  const h = await harness();
  try {
    h.s.uploadError = new RequestError("PUT", "/upload", 507, "Insufficient Storage");
    const outcome = await h.operate();
    assert.equal(outcome.reason, "capture_error");
    assert.equal(h.s.uploads, 1);
    const record = await h.saved();
    assert.equal(record.archiveRefusal, undefined);
    assert.equal(record.reason, "capture_error");
  } finally { await h.close(); }
});

// ── D3: RPCs are awaited to settlement ─────────────────────────────────────────────────────────────────────────

it("predecessor RPCs bypass raceIO while an ordinary caller's RPCs still race", async () => {
  const h = await harness();
  const budget = new MaintenanceBudget(60_000);
  try {
    let raced = 0;
    const race = budget.raceIO.bind(budget);
    budget.raceIO = ((...args: Parameters<typeof race>) => { raced++; return race(...args); }) as typeof race;
    const outcome = await MaintenanceBudget.run(budget, () => h.operate());
    await assertAcked(h, outcome);
    assert.ok(h.s.listReads + h.s.exactReads + h.s.reserves + h.s.uploads + h.s.releases.length >= 5, "the predecessor made RPCs");
    assert.equal(raced, 0, "none of them went through raceIO");
    await MaintenanceBudget.run(budget, () => h.coordinator.inventoryHolds(RUN));
    assert.equal(raced, 1, "control: an ordinary RPC under the same budget races");
  } finally { budget.dispose(); await h.close(); }
});

it("an RPC in flight when authority is lost is still awaited and its late answer is not journaled", async () => {
  const h = await harness();
  try {
    let settled = false;
    h.s.uploadHook = async () => {
      h.s.live = false;
      await new Promise(resolve => setTimeout(resolve, 30));
      settled = true;
    };
    await assert.rejects(h.operate(), PredecessorAuthorityLost);
    assert.equal(settled, true, "the operation did not return before the dispatched upload settled");
    assert.equal((await h.saved()).state, "bundled", "nothing about the late upload reached the journal");
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

// ── D1/§10: authority checkpoints ──────────────────────────────────────────────────────────────────────────────

/** Flip the scope off right after the first journal write that satisfies `when`: the next checkpoint must refuse. */
function loseAuthorityAfterWrite(h: Harness, when: (record: RecoveryRecord) => boolean): void {
  const target = h.coordinator as unknown as Record<string, (...a: unknown[]) => Promise<void>>;
  const write = target["writeRecordUnlocked"]!.bind(h.coordinator);
  target["writeRecordUnlocked"] = async (record: unknown) => {
    await write(record);
    if (when(record as RecoveryRecord)) h.s.live = false;
  };
}

for (const [name, arm, dispatched] of [
  ["the bundle install", (h: Harness) => { h.s.produceHook = () => { h.s.live = false; }; }, (h: Harness) => h.s.reserves],
  ["the reserve", (h: Harness) => loseAuthorityAfterWrite(h, r => r.state === "bundled"), (h: Harness) => h.s.reserves],
  ["the upload", (h: Harness) => { h.s.reserveHook = () => { h.s.live = false; }; }, (h: Harness) => h.s.uploads],
  ["the release POST", (h: Harness) => loseAuthorityAfterWrite(h, r => !!r.finalRequest && !r.finalAcknowledged), (h: Harness) => h.s.releases.length],
] as const) {
  it(`losing authority before ${name} throws PredecessorAuthorityLost and dispatches nothing further`, async () => {
    const h = await harness();
    try {
      arm(h);
      await assert.rejects(h.operate(), PredecessorAuthorityLost);
      assert.equal(dispatched(h), 0);
      assert.equal(h.s.releases.length, 0);
      if (name === "the bundle install") {
        const files = await fs.readdir(path.join(h.root, "journal", RUN));
        assert.deepEqual(files.filter(f => f.endsWith(".bundle")), [], "the bundle was never renamed into place");
        assert.equal((await h.saved()).bundlePath, undefined);
      }
    } finally { await h.close(); }
  });
}

it("authority lost before the release POST leaves the saved request untouched for an ordinary replay", async () => {
  const h = await harness();
  try {
    loseAuthorityAfterWrite(h, r => !!r.finalRequest && !r.finalAcknowledged);
    await assert.rejects(h.operate(), PredecessorAuthorityLost);
    assert.equal(h.s.releases.length, 0, "no POST went out");
    const record = await h.saved();
    assert.equal(record.finalRequest?.disposition.kind, "archive", "the FINAL request is journaled before dispatch");
    assert.equal(record.finalAcknowledged, undefined);
    // After the flight ends the run is no longer executing and the ordinary re-drive replays exactly that request.
    h.s.live = false; h.s.executing = false; h.s.status = "failed";
    await h.coordinator.resumePending(undefined, [record]);
    assert.equal(h.s.releases.length, 1);
    assert.deepEqual(h.s.releases[0]![3], record.finalRequest!.disposition);
  } finally { await h.close(); }
});

/** Unenrolled restriction facts through both seams the coordinator consults; `onRead` sees the running read count. */
function restrictionFacts(h: Harness, onRead: (reads: number) => void): void {
  let reads = 0;
  const facts = async () => { onRead(++reads); return { enrolled: false, blocked: false, roots: [], flaggedOIDs: [], fingerprint: "unenrolled" }; };
  const git = (h.coordinator as unknown as { git: Record<string, unknown> }).git;
  git.readRemediationRestrictionFacts = facts;
  git.withRemediationRestrictionFacts = async (_bare: string, _branch: string, _run: string, consume: (f: unknown) => Promise<unknown>) => consume(await facts());
}

it("authority lost between the last proof and the release POST stops before the POST", async () => {
  const baseline = await harness();
  let readsAtPost = 0;
  try {
    let reads = 0;
    restrictionFacts(baseline, count => { reads = count; });
    baseline.s.releaseHook = () => { readsAtPost = reads; };
    await assertAcked(baseline, await baseline.operate());
    assert.ok(readsAtPost > 0, "the baseline consulted restriction facts before its POST");
  } finally { await baseline.close(); }
  const h = await harness();
  try {
    restrictionFacts(h, count => { if (count === readsAtPost) h.s.live = false; });
    await assert.rejects(h.operate(), PredecessorAuthorityLost);
    assert.equal(h.s.releases.length, 0, "the last proof returned, authority was gone, and nothing was posted");
    const record = await h.saved();
    assert.equal(record.finalRequest?.disposition.kind, "archive");
    assert.equal(record.finalAcknowledged, undefined);
  } finally { await h.close(); }
});

it("assertPredecessorAuthority is a no-op for an ordinary caller", async () => {
  const h = await harness();
  try {
    h.s.live = false; // irrelevant: no scope
    h.s.executing = false; h.s.status = "failed";
    const outcome = await h.coordinator.captureAndUpload({ record: h.record, barePath: h.context.barePath, defaultBranch: "main" });
    assert.equal(outcome.state, "uploaded");
    assert.equal(h.s.releases.length, 1);
  } finally { await h.close(); }
});

// ── Integrity channel ──────────────────────────────────────────────────────────────────────────────────────────

it("the classification table has disjoint lists and unknown codes are integrity", () => {
  const { integrity, ordinary } = PREDECESSOR_REFUSAL_CLASSES;
  assert.deepEqual(integrity.filter(code => ordinary.includes(code)), []);
  for (const code of integrity) assert.equal(classifyPredecessorRefusal(code), "integrity", code);
  for (const code of ordinary) assert.equal(classifyPredecessorRefusal(code), "ordinary", code);
  assert.equal(classifyPredecessorRefusal("a_code_nobody_classified"), "integrity");
  for (const code of ["record_or_run_state", "restrictions", "bundle", "mint_denied", "capture_state", "capture_status", "predecessor_observation_refused"]) {
    assert.equal(classifyPredecessorRefusal(code), "ordinary", code);
  }
  for (const code of ["bundle_header", "local_bytes_unverified", "frozen_coverage", "source_proof", "record_unauthenticated", "record_removed",
    "inventory_snapshot_changed", "inventory_quiescence_breach", "source_advanced", "coverage_context"]) assert.equal(classifyPredecessorRefusal(code), "integrity", code);
});

it("every refusal code the coordinator can record is classified", async () => {
  const source = (await fs.readFile(new URL("../src/recovery.ts", import.meta.url), "utf8"))
    .replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
  const codes = new Set<string>();
  for (const match of source.matchAll(/(?:this\.unmet|this\.predecessorNote)\(\s*"([a-z_]+)"/g)) codes.add(match[1]!);
  for (const match of source.matchAll(/return "(mint_denied|local_bytes_unverified|record_unauthenticated|record_removed)"/g)) codes.add(match[1]!);
  assert.ok(codes.size >= 15, "the scan found the call sites");
  const classified = new Set([...PREDECESSOR_REFUSAL_CLASSES.integrity, ...PREDECESSOR_REFUSAL_CLASSES.ordinary]);
  assert.deepEqual([...codes].filter(code => !classified.has(code)).sort(), [], "every recorded code is listed explicitly");
});

/** A thin uploaded archive with no FINAL: the closure feature was absent, then returns. */
async function uploadedThinWithoutFinal(): Promise<Harness> {
  const h = await harness({ thin: true, closureFeature: false });
  const outcome = await h.operate();
  assert.equal(outcome.predecessor, undefined);
  assert.equal((await h.saved()).state, "uploaded");
  h.s.closureFeature = true;
  return h;
}

it("a thin archive whose journaled bytes changed is local_bytes_unverified (integrity); intact bytes mint", async () => {
  const bad = await uploadedThinWithoutFinal();
  try {
    const record = await bad.saved();
    const bytes = await fs.readFile(record.bundlePath!);
    bytes[bytes.length - 1] = bytes[bytes.length - 1]! ^ 0xff;
    await fs.writeFile(record.bundlePath!, bytes);
    const outcome = await bad.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "local_bytes_unverified");
    assert.equal(bad.s.releases.length, 0);
  } finally { await bad.close(); }
  const good = await uploadedThinWithoutFinal();
  try {
    const outcome = await good.operate();
    assert.equal(outcome.held, "acked");
    assert.equal(outcome.predecessor, undefined);
  } finally { await good.close(); }
});

it("an unreadable bundle header is bundle_header (integrity)", async () => {
  const h = await harness();
  try {
    const record = await uploadedSelfContained(h);
    await fs.writeFile(record.bundlePath!, "not a bundle");
    const outcome = await h.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "bundle_header");
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

it("a coverage that no longer reproduces is frozen_coverage (integrity)", async () => {
  const h = await harness();
  try {
    await uploadedSelfContained(h);
    h.s.coverageSha = "f".repeat(40);
    const outcome = await h.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "frozen_coverage");
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

it("an unproven source ancestry is an inventory_snapshot_changed write and an integrity failure", async () => {
  const h = await harness();
  try {
    await uploadedSelfContained(h);
    h.s.cloneHeads = [H3]; // an unattributed clone head that is not an ancestor of the frozen source
    const outcome = await h.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "inventory_snapshot_changed");
    assert.equal((await h.saved()).reason, "inventory_snapshot_changed");
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

it("a source proof that fails before a saved FINAL's POST is source_proof (integrity)", async () => {
  const h = await harness();
  try {
    await savedArchiveFinal(h, false);
    const posts = h.s.releases.length;
    h.s.proofRefused = true;
    const outcome = await h.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "source_proof");
    assert.equal(h.s.releases.length, posts, "no further POST");
  } finally { await h.close(); }
});

for (const [name, tamper, code] of [
  ["removed", (file: string) => fs.rm(file), "record_removed"],
  ["unauthenticated", (file: string) => fs.writeFile(file, "{}"), "record_unauthenticated"],
] as const) {
  it(`a record ${name} after selection is ${code} (integrity)`, async () => {
    const h = await harness();
    try {
      // Read 1 is the entry probe, read 2 the observation: the record is damaged after the observation selected it.
      h.s.exactHook = async () => {
        if (h.s.exactReads !== 2) return;
        await tamper(path.join(h.root, "journal", RUN, h.record.captureId + ".json"));
      };
      const outcome = await h.operate();
      assert.equal(outcome.predecessor?.integrityFailure, code);
      assert.equal(h.s.releases.length, 0);
    } finally { await h.close(); }
  });
}

it("ordinary refusals never latch the integrity channel", async () => {
  // record_or_run_state: the run's ownership no longer proves the generation ended.
  const a = await harness();
  try {
    await uploadedSelfContained(a);
    a.s.ownGeneration = 7; a.s.status = "running";
    const outcome = await a.operate();
    assert.equal(outcome.held === "acked", false);
    assert.equal(outcome.predecessor, undefined, "record_or_run_state is ordinary");
    assert.equal(a.s.releases.length, 0);
  } finally { await a.close(); }
  // restrictions: a blocking remediation fact.
  const b = await harness();
  try {
    await uploadedSelfContained(b);
    const git = (b.coordinator as unknown as { git: Record<string, unknown> }).git;
    git.readRemediationRestrictionFacts = async () => ({ enrolled: true, blocked: true, roots: [], flaggedOIDs: [], fingerprint: "blocked" });
    const outcome = await b.operate();
    assert.equal(outcome.held === "acked", false);
    assert.equal(outcome.predecessor, undefined, "restrictions is ordinary");
    assert.equal(b.s.releases.length, 0);
  } finally { await b.close(); }
  // capture_status: a freshly uploaded capture the api does not yet report as healthy.
  const c = await harness();
  try {
    c.s.statusPatch = () => ({ state: "preparing" });
    const outcome = await c.operate();
    assert.equal(outcome.held === "acked", false);
    assert.equal(outcome.predecessor, undefined, "capture_status is ordinary");
    assert.equal(c.s.releases.length, 0);
  } finally { await c.close(); }
});

// ── Isolation ──────────────────────────────────────────────────────────────────────────────────────────────────

interface Disqualifier { inactiveInventoryDisqualifier(record: RecoveryRecord): Promise<string | undefined> }

it("the executing exemption is exact: this record, this run, live scope, nothing else", async () => {
  const h = await harness();
  try {
    const probe = h.coordinator as unknown as Disqualifier;
    const seen: Record<string, string | undefined> = {};
    h.s.reserveHook = async () => {
      const record = await h.saved();
      seen.exact = await probe.inactiveInventoryDisqualifier(record);
      seen.otherCapture = await probe.inactiveInventoryDisqualifier({ ...record, captureId: randomUUID() });
      seen.otherRun = await probe.inactiveInventoryDisqualifier({ ...record, runId: "other-run" });
      seen.otherGeneration = await probe.inactiveInventoryDisqualifier({ ...record, generation: 8 });
      h.s.live = false;
      try { seen.notLive = await probe.inactiveInventoryDisqualifier(record); }
      catch (error) { seen.notLive = error instanceof PredecessorAuthorityLost ? "authority_lost" : "other:" + String(error); }
      h.s.live = true;
    };
    await h.operate();
    assert.equal(seen.exact, undefined, "the named record is exempt while the scope is live");
    assert.equal(seen.otherCapture, "executing");
    assert.equal(seen.otherRun, "executing");
    assert.equal(seen.otherGeneration, "executing", "generation 8 is not below the executing claim");
    assert.equal(seen.notLive, "authority_lost", "a scope that is not live exempts nothing and is reported as lost authority");
  } finally { await h.close(); }
});

it("a concurrent chain outside the scope gets no exemption while the predecessor runs", async () => {
  const h = await harness();
  try {
    const probe = h.coordinator as unknown as Disqualifier;
    let inside: string | undefined = "unset";
    let release!: () => void;
    let parked!: () => void;
    const reached = new Promise<void>(resolve => { parked = resolve; });
    const gate = new Promise<void>(resolve => { release = resolve; });
    h.s.reserveHook = async () => { inside = await probe.inactiveInventoryDisqualifier(await h.saved()); parked(); await gate; };
    const running = h.operate();
    await reached;
    const outside = await probe.inactiveInventoryDisqualifier(await h.saved());
    release();
    await assertAcked(h, await running);
    assert.equal(inside, undefined, "inside the scope the record is exempt");
    assert.equal(outside, "executing", "a chain the scope did not enter is not");
  } finally { await h.close(); }
});

it("a continuation that outlives the scope gets no exemption", async () => {
  const h = await harness();
  try {
    const probe = h.coordinator as unknown as Disqualifier;
    let late: Promise<string | undefined> | undefined;
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    h.s.reserveHook = () => { late = (async () => { await gate; return probe.inactiveInventoryDisqualifier(await h.saved()); })(); };
    await assertAcked(h, await h.operate());
    h.s.live = false;
    release();
    await assert.rejects(late!, PredecessorAuthorityLost, "no exemption: the ended scope refuses instead of granting");
    // And once the run's tail is the only thing left, the ordinary reading is unchanged for every other chain.
    assert.equal(await (h.coordinator as unknown as Disqualifier).inactiveInventoryDisqualifier(await h.saved()), "executing");
  } finally { await h.close(); }
});

it("nested held release and generation cleanup still refuse under the scope", async () => {
  const h = await harness();
  try {
    const nested = h.coordinator as unknown as {
      releaseHeldWithinCycle(record: RecoveryRecord, publicationId: string): Promise<string>;
      forgetGenerationWithinCycle(runId: string, generation: number): Promise<string>;
    };
    const planted = await h.plant({ finalRequest: { disposition: { kind: "held_publication", publication_id: PUB, source_sha: h.record.sourceSha,
      coverage_digest: h.record.coverageDigest! } } });
    const seen: Record<string, string> = {};
    const forgets: string[] = [];
    const forget = nested.forgetGenerationWithinCycle.bind(h.coordinator);
    Object.assign(h.coordinator, { forgetGenerationWithinCycle: async (...args: [string, number]) => {
      const result = await forget(...args);
      forgets.push(result);
      return result;
    } });
    h.s.uploadHook = async () => {
      seen.release = await nested.releaseHeldWithinCycle(planted, PUB);
      seen.forgetBeforeAck = await nested.forgetGenerationWithinCycle(RUN, 7);
    };
    await assertAcked(h, await h.operate());
    assert.equal(seen.release, "kept");
    assert.equal(h.s.releases.length, 1, "the held release sent nothing; only the predecessor's own archive FINAL went out");
    assert.notEqual(seen.forgetBeforeAck, "forgotten");
    assert.equal(forgets.at(-1), "source_protected", "after the ACK the executing run still protects the source");
    assert.ok(!forgets.includes("forgotten"));
    assert.equal(h.s.cleanups, 0, "no ref, pin or bundle was removed while the successor executes");
    assert.equal((await h.saved()).generation, 7, "the record survived every cleanup attempt");
    assert.ok((await h.saved()).bundlePath && await fs.stat((await h.saved()).bundlePath!), "and so did its bundle");
  } finally { await h.close(); }
});

it("an ordinary caller's cycle in the same coordinator is untouched by a predecessor scope that already ended", async () => {
  const h = await harness();
  try {
    await assertAcked(h, await h.operate());
    const probe = h.coordinator as unknown as Disqualifier;
    assert.equal(await probe.inactiveInventoryDisqualifier(await h.saved()), "executing");
  } finally { await h.close(); }
});

// ── M1 rework ──────────────────────────────────────────────────────────────────────────────────────────────────

it("a record removed mid-cycle keeps the orphan bundle and latches record_removed", async () => {
  const h = await harness();
  try {
    h.s.reserveHook = async () => { await fs.rm(path.join(h.root, "journal", RUN, h.record.captureId + ".json")); };
    const outcome = await h.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "record_removed");
    assert.equal(outcome.reason, "record_removed");
    const bundles = (await fs.readdir(path.join(h.root, "journal", RUN))).filter(f => f.endsWith(".bundle"));
    assert.equal(bundles.length, 1, "the only local bundle copy survives");
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

it("losing authority during the capture-status read rejects instead of becoming an ordinary refusal", async () => {
  const h = await harness();
  try {
    // Authority is cut after the last proof before the status read, so the status RPC itself refuses to dispatch.
    const git = (h.coordinator as unknown as { git: { buildRecoveryCoverage(...a: unknown[]): Promise<unknown> } }).git;
    const build = git.buildRecoveryCoverage.bind(git);
    git.buildRecoveryCoverage = async (...a: unknown[]) => { const out = await build(...a); h.s.live = false; return out; };
    await assert.rejects(h.operate(), PredecessorAuthorityLost);
    assert.equal(h.s.releases.length, 0);
  } finally { await h.close(); }
});

it("a record that vanishes during the observation-free FINAL path is never reported acked", async () => {
  const h = await harness();
  try {
    await uploadedSelfContained(h);
    const target = h.coordinator as unknown as Record<string, unknown>;
    target.finalizeInventory = async () => { await fs.rm(path.join(h.root, "journal", RUN, h.record.captureId + ".json")); return undefined; };
    const outcome = await h.operate();
    assert.notEqual(outcome.held, "acked");
    assert.equal(outcome.predecessor?.integrityFailure, "record_removed");
  } finally { await h.close(); }
});

it("a genuine bundle failure latches integrity; the same failure caused by the operation's own abort does not", async () => {
  const real = await harness();
  try {
    real.s.produceHook = () => { throw new Error("pack-objects crashed"); };
    const outcome = await real.operate();
    assert.equal(outcome.predecessor?.integrityFailure, "bundle_failed");
  } finally { await real.close(); }
  const aborted = await harness();
  try {
    const controller = new AbortController();
    aborted.s.produceHook = () => { controller.abort(new Error("cap")); throw new Error("aborted by the cap"); };
    const outcome = await aborted.operate({}, {}, aborted.record, controller.signal);
    assert.equal(outcome.reason, "bundle_failed");
    assert.equal(outcome.predecessor, undefined, "an abort-caused failure stays unrecorded for the runner to classify");
  } finally { await aborted.close(); }
});

for (const [name, binding] of [
  ["hold id", { holdId: "00000000-0000-4000-8000-0000000000dd", repoId: REPO }],
  ["repo id", { holdId: HOLD, repoId: "00000000-0000-4000-8000-0000000000ee" }],
] as const) {
  it(`a journaled exact binding that differs in its ${name} refuses the observation`, async () => {
    const h = await harness();
    try {
      await h.patch({ exactHoldBinding: binding });
      const outcome = await h.operate();
      assert.equal(outcome.reason, "predecessor_observation_refused");
      assert.deepEqual(h.sideEffects(), NONE);
    } finally { await h.close(); }
  });
}

it("an ended scope exempts and writes nothing even while live() still answers true", async () => {
  const h = await harness();
  try {
    const probe = h.coordinator as unknown as Disqualifier & {
      writeExistingRecord(r: RecoveryRecord, m: (c: RecoveryRecord) => RecoveryRecord | undefined): Promise<unknown>;
    };
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let late: Promise<unknown> | undefined, lateWrite: Promise<unknown> | undefined;
    // Both continuations start inside the scope and resume after it ended.
    h.s.reserveHook = () => { late = (async () => { await gate; return probe.inactiveInventoryDisqualifier(await h.saved()); })(); late.catch(() => undefined); };
    h.s.uploadHook = () => { lateWrite = (async () => { await gate; return probe.writeExistingRecord(h.record, cur => ({ ...cur, reason: "late" })); })(); lateWrite.catch(() => undefined); };
    await assertAcked(h, await h.operate());
    assert.equal(h.s.live, true, "live() is still true: only the ended scope refuses");
    const before = await h.snapshot();
    release();
    await assert.rejects(late!, PredecessorAuthorityLost);
    await assert.rejects(lateWrite!, PredecessorAuthorityLost);
    assert.ok((await h.snapshot()).bytes.equals(before.bytes), "the late write changed nothing");
  } finally { await h.close(); }
});

it("a record of another run with the scope's capture id and generation gets no exemption", async () => {
  const h = await harness();
  try {
    const probe = h.coordinator as unknown as Disqualifier;
    let seen: string | undefined = "unset";
    h.s.reserveHook = async () => { seen = await probe.inactiveInventoryDisqualifier({ ...(await h.saved()), runId: "other-run" }); };
    await h.operate();
    assert.equal(seen, "executing");
  } finally { await h.close(); }
});

it("an explicit closure budget is used by identity and clamped to the cache's decoded ceiling", async () => {
  const { GitCache } = await import("../src/git.js");
  const { testGitCacheOptions } = await import("./helpers.js");
  const data = makeScratchRoot("closure-budget-", os.tmpdir());
  try {
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const shared = { delivered: 5, limit: cache.recoveryDecodedLimitBytes + 1000 };
    let seen: unknown;
    await cache.withRecoveryOperation(new AbortController().signal, Infinity, async () => {
      seen = (cache as unknown as { recoveryOperations: { getStore(): { closureBudget: unknown } } }).recoveryOperations.getStore().closureBudget;
    }, undefined, { closureBudget: shared });
    assert.equal(seen, shared, "the very same mutable object");
    assert.equal(shared.limit, cache.recoveryDecodedLimitBytes, "clamped to the cache ceiling");
    const small = { delivered: 0, limit: 10 };
    await cache.withRecoveryOperation(new AbortController().signal, Infinity, async () => undefined, undefined, { closureBudget: small });
    assert.equal(small.limit, 10, "a lower limit is kept");
  } finally { await fs.rm(data, { recursive: true, force: true }); }
});

// ── M1 rework round 2 ──────────────────────────────────────────────────────────────────────────────────────────

it("two operations sharing one allowance cannot both charge it: the late re-check refuses the second", async () => {
  const a = await harness(), b = await harness();
  try {
    const size = bundleBytes(a.record.sourceSha, false).length;
    const shared = { remaining: Math.floor(size * 1.5) };
    let arrived = 0;
    let open!: () => void;
    const barrier = new Promise<void>(resolve => { open = resolve; });
    const hook = async () => { if (++arrived === 2) open(); await barrier; };
    a.s.reserveHook = hook; b.s.reserveHook = hook;
    const [x, y] = await Promise.all([a.operate({ uploadAllowance: shared }), b.operate({ uploadAllowance: shared })]);
    assert.equal(a.s.uploads + b.s.uploads, 1, "exactly one upload dispatched");
    assert.deepEqual([x.reason, y.reason].filter(r => r === "predecessor_upload_budget").length, 1);
    assert.ok(shared.remaining >= 0, "the allowance never goes negative");
    assert.equal(shared.remaining, Math.floor(size * 1.5) - size);
  } finally { await a.close(); await b.close(); }
});

it("a non-live scope with a matching record is lost authority even where no RPC would run", async () => {
  const h = await harness();
  try {
    const probe = h.coordinator as unknown as Disqualifier;
    const client = (h.coordinator as unknown as { client: { hasFeature(name: string): boolean } }).client;
    const hasFeature = client.hasFeature;
    let seen = "unset";
    h.s.reserveHook = async () => {
      const record = await h.saved();
      h.s.live = false; client.hasFeature = () => false;
      try { seen = String(await probe.inactiveInventoryDisqualifier(record)); }
      catch (error) { seen = error instanceof PredecessorAuthorityLost ? "authority_lost" : "other:" + String(error); }
      client.hasFeature = hasFeature; h.s.live = true;
    };
    await h.operate();
    assert.equal(seen, "authority_lost", "not inventory_feature: the exemption itself requires a live scope");
  } finally { await h.close(); }
});

for (const scoped of [true, false]) {
  it(`installBundle ${scoped ? "keeps" : "removes"} the renamed bundle when the journal write fails (${scoped ? "predecessor" : "ordinary"})`, async () => {
    const h = await harness();
    try {
      const target = h.coordinator as unknown as Record<string, (...a: unknown[]) => Promise<void>>;
      const write = target["writeRecordUnlocked"]!.bind(h.coordinator);
      let failed = false;
      target["writeRecordUnlocked"] = async (record: unknown) => {
        if (!failed && (record as RecoveryRecord).state === "bundled") { failed = true; throw new Error("disk full"); }
        return write(record);
      };
      if (scoped) await h.operate();
      else {
        h.s.executing = false; h.s.status = "failed";
        await h.coordinator.captureAndUpload({ record: h.record, barePath: h.context.barePath, defaultBranch: "main" });
      }
      assert.equal(failed, true);
      const bundles = (await fs.readdir(path.join(h.root, "journal", RUN))).filter(f => f.endsWith(".bundle"));
      assert.equal(bundles.length, scoped ? 1 : 0);
    } finally { await h.close(); }
  });
}

it("a closure budget limit that is NaN or negative admits nothing; infinity is capped", async () => {
  const { GitCache } = await import("../src/git.js");
  const { testGitCacheOptions } = await import("./helpers.js");
  const data = makeScratchRoot("closure-budget-", os.tmpdir());
  try {
    const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
    const run = (limit: number) => {
      const budget = { delivered: 0, limit };
      return cache.withRecoveryOperation(new AbortController().signal, Infinity, async () => budget.limit, undefined, { closureBudget: budget });
    };
    assert.equal(await run(Number.NaN), 0);
    assert.equal(await run(-5), 0);
    assert.equal(await run(Number.NEGATIVE_INFINITY), 0);
    assert.equal(await run(Number.POSITIVE_INFINITY), cache.recoveryDecodedLimitBytes);
  } finally { await fs.rm(data, { recursive: true, force: true }); }
});
