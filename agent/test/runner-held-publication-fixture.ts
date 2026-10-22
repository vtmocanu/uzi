// Issue #2545 M5: shared fixture for the runner-held-publication*.test.ts files. A guarded
// "api" for one run (holds, ownership, archive capture, held publication, FINAL) installed on the
// harness client, an executor that leaves a realistic failed workspace, and the custody-retention
// assertions every fault test makes.
import assert from "node:assert/strict";
import type { TestContext } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { HeldFeatureUnavailableError, RequestError, type HeldPublishResult } from "../src/client.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { RecoveryFinalDisposition, RecoveryReleaseResponse, StateRequest } from "../src/protocol.js";
import { RecoveryCoordinator, type RecoveryRecord } from "../src/recovery.js";
import type { RunRunner } from "../src/runner.js";
import { api, client, fx, git } from "./runner-harness.js";

export const HELD_FEATURES = ["recovery_inventory_v1", "recovery_held_publication_v1"] as const;
export const PUBLICATION_ID = "7c1f0d2e-5b8a-4c39-9d41-0a6e2b7f3c15";
export const SERVER_CAPTURE_ID = "00000000-0000-4000-8000-000000002545";
export const JOIN_TOKEN = "held-publication-journal-key";
/** Held cases whose redrive FINAL is attributed through Linux process attribution; the runner refuses non-Linux hosts. */
export const HELD_LINUX_ONLY = process.platform !== "linux" && "physical source FINAL proof requires Linux process attribution";

export function command(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
}
export function commit(dir: string, name: string, body = name + "\n"): string {
  fs.mkdirSync(path.dirname(path.join(dir, name)), { recursive: true });
  fs.writeFileSync(path.join(dir, name), body);
  command(dir, ["add", name]);
  command(dir, ["-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", name]);
  return command(dir, ["rev-parse", "HEAD"]);
}
/** A GitHub-PAT-shaped value, assembled at runtime so no token-shaped literal sits in source. */
export function patShaped(): string {
  return "gh" + "p_" + "a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8";
}

export interface HeldCall { run: string; generation: number; tip: string; coverage: string; pack: Buffer }
export interface FinalCall { generation: number | undefined; disposition: RecoveryFinalDisposition | undefined }
export type FinalAnswer = RecoveryReleaseResponse | Error;

/** Everything a test can see or steer on the fake api. */
export interface HeldServer {
  features: Set<string>;
  heldCalls: HeldCall[];
  finals: FinalCall[];
  archiveReserves: () => number;
  archiveUploads: () => number;
  /** FINALs the api answered released:true to. */
  acked: () => number;
  holdOpen: () => boolean;
  failedAcked: () => boolean;
  /** Steer step A. Default: created at the requested tip. */
  heldAnswer: (call: HeldCall) => HeldPublishResult | Promise<HeldPublishResult>;
  /** Steer step B / archive FINAL. Default: released. */
  finalAnswer: (call: FinalCall) => FinalAnswer | Promise<FinalAnswer>;
  /** Runs inside publishHeld, before the answer: the moment between the failed send and the held FINAL. */
  onHeld?: (call: HeldCall) => void | Promise<void>;
  /** Runs inside releaseRecoveryCustody, before the answer. */
  onFinal?: (call: FinalCall) => void | Promise<void>;
}

export function created(call: HeldCall): HeldPublishResult {
  return { ok: true, body: { publication_id: PUBLICATION_ID, ref: `refs/uzi-held/${call.run}/${call.generation}`,
    tip: call.tip, state: "created" } };
}
export function retained(claim: { run_id: string; claim_generation?: number }, reason: string): RecoveryReleaseResponse {
  return { run_id: claim.run_id, generation: claim.claim_generation!, released: false, holds_released: 0, retained: true, reason };
}

/** Install a guarded fake api for `claim` on the harness client. */
export function heldServer(claim: { run_id: string; claim_generation?: number }, featureSet: readonly string[] = HELD_FEATURES): HeldServer {
  const generation = claim.claim_generation!;
  const features = new Set(featureSet);
  const realHasFeature = client.hasFeature.bind(client);
  client.hasFeature = name =>
    name === "recovery_inventory_v1" || name === "recovery_held_publication_v1" ? features.has(name) : realHasFeature(name);
  const failedAcked = (): boolean => api.states.some(s => s.runId === claim.run_id && s.body.status === "failed");
  client.getRunOwnership = async () => ({
    status: failedAcked() ? "failed" : "running", claim_generation: generation, inventory_guarded: true,
  });
  let open = true, acked = 0, reserves = 0, uploads = 0;
  let manifest: { checksum: string; byte_size: number } | undefined;
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: "inventory-hold", generation, inventory_guarded: true, has_available_capture: !!manifest,
  }] : [] });
  client.reserveRecoveryCapture = async () => { reserves++; return { capture_id: SERVER_CAPTURE_ID, state: "preparing" }; };
  client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
    for await (const chunk of stream) assert.ok(chunk);
    uploads++;
    manifest = m;
    return { capture_id: SERVER_CAPTURE_ID, state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async () => ({
    capture_id: SERVER_CAPTURE_ID, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
    checksum: manifest?.checksum, byte_size: manifest?.byte_size,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
  });
  const server: HeldServer = {
    features, heldCalls: [], finals: [],
    archiveReserves: () => reserves, archiveUploads: () => uploads, acked: () => acked,
    holdOpen: () => open, failedAcked,
    heldAnswer: created,
    finalAnswer: () => ({ run_id: claim.run_id, generation, released: true, holds_released: 1 }),
  };
  client.publishHeld = async (run, gen, tip, coverage, pack) => {
    const call: HeldCall = { run, generation: gen, tip, coverage, pack: Buffer.isBuffer(pack) ? pack : Buffer.concat(await collect(pack)) };
    server.heldCalls.push(call);
    await server.onHeld?.(call);
    return server.heldAnswer(call);
  };
  client.releaseRecoveryCustody = async (_run, gen, _evidence, disposition) => {
    // The real client throws this typed error BEFORE sending when the feature is not negotiated
    // (pinned in client-held-publication.test.ts); a non-definitive outcome for the coordinator.
    if (disposition?.kind === "held_publication" && !features.has("recovery_held_publication_v1")) {
      throw new HeldFeatureUnavailableError();
    }
    const call: FinalCall = { generation: gen, disposition };
    server.finals.push(call);
    await server.onFinal?.(call);
    const answer = await server.finalAnswer(call);
    if (answer instanceof Error) throw answer;
    if (answer.released === true) { open = false; acked++; }
    return answer;
  };
  return server;
}

async function collect(stream: AsyncIterable<Buffer | string>): Promise<Buffer[]> {
  const out: Buffer[] = [];
  for await (const c of stream) out.push(Buffer.isBuffer(c) ? c : Buffer.from(c));
  return out;
}

export function badRequest(runId: string): RequestError {
  return new RequestError("POST", `/api/worker/runs/${runId}/archives/release`, 400, JSON.stringify({ error: "invalid final disposition" }));
}

/** What the failed executor leaves behind (see {@link failingWork}). */
export interface Work { clone: string; rootA: string; rootB: string; afterCheckpoint: string; wipBlobs: Record<string, string> }

/**
 * An executor that, in a real clone, commits root A, checkpoints, resets and commits root B (two
 * unrelated owed roots), commits once more after the last checkpoint, then modifies a tracked file and
 * adds an untracked one, and finally fails. `extra` runs last, for a fault test's own mutation.
 */
export function failingWork(work: Partial<Work>, extra?: (ctx: RunContext) => Promise<void> | void, failWith = "executor failed"): Executor {
  return { run: async (ctx: RunContext) => {
    const clone = ctx.worktreePath;
    const base = command(clone, ["rev-parse", "HEAD"]);
    work.clone = clone;
    work.rootA = commit(clone, "root-a.txt");
    await ctx.checkpoint!({ reap: false });
    command(clone, ["reset", "--hard", base]);
    work.rootB = commit(clone, "root-b.txt");
    await ctx.checkpoint!({ reap: false });
    work.afterCheckpoint = commit(clone, "after-checkpoint.txt");
    fs.appendFileSync(path.join(clone, "README.md"), "modified after the last commit\n");
    fs.writeFileSync(path.join(clone, "untracked-note.txt"), "untracked wip\n");
    work.wipBlobs = {
      "README.md": command(clone, ["hash-object", "README.md"]),
      "untracked-note.txt": command(clone, ["hash-object", "untracked-note.txt"]),
    };
    await extra?.(ctx);
    throw new Error(failWith);
  } };
}

/** Stub the checkpoint broker: every checkpoint is skipped (workflow_scope), so roots stay owed. */
export function skipCheckpoints(): void {
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    return { ok: true, body: { published: false, ref: "refs/uzi-checkpoints/test", reason: "workflow_scope" } };
  };
}

export const bare = (): string => git.barePathFor(fx.originPath);
export const pinRef = (claim: { run_id: string; claim_generation?: number }): string =>
  `refs/uzi-recovery-pin/${claim.run_id}/${claim.claim_generation}`;

/** A coordinator over the harness stores, to read the journal the runner wrote. */
export function journal(): RecoveryCoordinator {
  return new RecoveryCoordinator({ client, git, log: nullLog(), recoveryRoot: git.recoveryRoot, workerToken: JOIN_TOKEN });
}
function nullLog() {
  const self: import("../src/log.js").Logger = {
    debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child: () => self,
  };
  return self;
}

export async function frozenRecords(claim: { run_id: string; claim_generation?: number }): Promise<RecoveryRecord[]> {
  return (await journal().inspect(claim.run_id)).filter(r => r.generation === claim.claim_generation && r.coverageDigest);
}

/**
 * The custody-retention assertions (plan gate): from the deferred capture until the FINAL is ACKed the
 * guarded hold stays open, no release was ACKed, the pin resolves to the frozen head, the frozen record
 * exists without finalAcknowledged, and the runner clone and the bare are still there.
 */
export async function assertCustodyRetained(
  server: HeldServer, claim: { run_id: string; claim_generation?: number }, work: Partial<Work>, label: string,
): Promise<RecoveryRecord> {
  assert.equal(server.holdOpen(), true, `${label}: the guarded hold stays open`);
  assert.equal(server.acked(), 0, `${label}: no release was ACKed`);
  const records = await frozenRecords(claim);
  assert.equal(records.length, 1, `${label}: exactly one frozen record for the generation`);
  const record = records[0]!;
  assert.notEqual(record.finalAcknowledged, true, `${label}: the frozen record is not acknowledged`);
  assert.equal(command(bare(), ["rev-parse", "--verify", pinRef(claim)]), record.inventoryCurrentSha,
    `${label}: the recovery pin resolves to the frozen head`);
  assert.ok(fs.existsSync(bare()), `${label}: the trusted bare remains`);
  if (work.clone) assert.ok(fs.existsSync(work.clone), `${label}: the runner clone remains`);
  return record;
}

/** Files of `pack` (a raw pack) as the object ids its index lists, via a scratch repo. */
export function packObjects(pack: Buffer): Set<string> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-held-pack-"));
  try {
    execFileSync("git", ["init", "-q", "--bare", dir]);
    execFileSync("git", ["-C", dir, "index-pack", "--stdin"], { input: pack });
    const listing = execFileSync("git", ["-C", dir, "show-index"], {
      input: fs.readFileSync(path.join(dir, "objects", "pack", fs.readdirSync(path.join(dir, "objects", "pack")).find(f => f.endsWith(".idx"))!)),
      encoding: "utf8",
    });
    return new Set(listing.split("\n").filter(Boolean).map(l => l.split(" ")[1]!));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

export function failedStates(runId: string): StateRequest[] {
  return api.states.filter(s => s.runId === runId && s.body.status === "failed").map(s => s.body as StateRequest);
}

/** The runner's coordinator, to spy on the capture. */
export function recoveryOf(r: RunRunner): RecoveryCoordinator {
  return Reflect.get(r, "recovery") as RecoveryCoordinator;
}

/** Observe first construction of each coverage identity. FINAL verification may
 * rebuild the same aggregate; that is not a second immutable inventory freeze.
 * New identities must precede the failed send (all observations remain false). */
export function watchFreezes(server: HeldServer): { failedAckedAtFreeze: boolean[] } {
  const seen = { failedAckedAtFreeze: [] as boolean[] };
  const fingerprints = new Set<string>();
  const build = git.buildRecoveryCoverage.bind(git);
  git.buildRecoveryCoverage = async (...args) => {
    const coverage = await build(...args);
    if (!fingerprints.has(coverage.fingerprint)) {
      fingerprints.add(coverage.fingerprint);
      seen.failedAckedAtFreeze.push(server.failedAcked());
    }
    return coverage;
  };
  return seen;
}

/**
 * These legacy cases own a LIVE/foreground/restart interleaving after execute.
 * Hold the automatic notification BEFORE trackPass registers it, preserving that
 * boundary. Dedicated notification tests exercise automatic replay without this barrier.
 * Test cleanup closes the runner first, then releases and joins deferred notifications
 * through the production shutdown guard, including when an assertion fails.
 */
export function deferTerminalEvidence(r: RunRunner, t: Pick<TestContext, "after" | "mock">): void {
  const recovery = recoveryOf(r);
  const resume = recovery.resumeTerminalEvidence.bind(recovery);
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  const pending: Promise<void>[] = [];
  t.mock.method(recovery, "resumeTerminalEvidence", (...args: Parameters<typeof resume>) => {
    const notification = gate.then(() => resume(...args));
    pending.push(notification);
    return notification;
  });
  t.after(async () => {
    // TestContext.after follows the harness's afterEach cleanup. Close admission
    // before releasing the artificial barrier, so no notification can use retired
    // fixture storage or a closed fake API. Already-owned passes still settle.
    r.shutdown();
    release();
    const results = await Promise.allSettled([...pending, r.shutdownRecoveries()]);
    const failed = results.find(result => result.status === "rejected");
    if (failed?.status === "rejected") throw failed.reason;
  });
}

/** Run the live re-drive the worker performs after execute(); each pass may send one journaled FINAL. */
export async function redrive(r: RunRunner, passes = 3): Promise<void> {
  for (let i = 0; i < passes; i++) await r.resumeLiveRecoveries(Date.now() + i);
}
