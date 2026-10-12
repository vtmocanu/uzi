import { afterEach, beforeEach, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import { existsSync } from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { GitCache } from "../src/git.js";
import { canonicalJson, RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer, type RecoveryRecord } from "../src/recovery.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

resetResidueQuarantineAfterEach();

const TOKEN = "issue-2263-worker-fixture";
const NAME = "forge.example+org+repo.git";
const RUN = "parked-run";
const ref = (generation: number) => `refs/uzi-recovery-pin/${RUN}/${generation}`;
const git = (cwd: string, ...args: string[]) => execFileSync("git", ["-C", cwd, ...args], {
  encoding: "utf8", stdio: ["ignore", "pipe", "pipe"],
  env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1" },
}).trim();

class Archive implements RecoveryArchiveClient {
  reserves = 0;
  uploads = 0;
  reserveError?: Error;
  uploadError?: Error = new Error("temporary upload failure");
  accepted?: () => Promise<void>;
  async reserveRecoveryCapture() {
    this.reserves++;
    if (this.reserveError) throw this.reserveError;
    return { capture_id: "server-capture", state: "preparing" as const };
  }
  async getRecoveryCaptureStatus() {
    return { capture_id: "server-capture", state: "preparing" as const, manifest_bound: false };
  }
  async uploadRecoveryBundle(_run: string, _capture: string, _manifest: unknown, bundle: Readable) {
    this.uploads++;
    for await (const _ of bundle) { /* consume the actual journaled bytes */ }
    if (this.uploadError) throw this.uploadError;
    await this.accepted?.();
    return { capture_id: "server-capture", state: "available" as const, manifest_bound: true };
  }
  async releaseRecoveryCustody(): Promise<never> { throw new Error("archive upload must not release custody"); }
  async listRecoveryHolds() { return { run_id: RUN, holds: [] }; }
}

let root: string;
let bare: string;
let sha: string;
let cache: GitCache;
let archive: Archive;
let now: number;
let warnings: Array<{ message: string; fields: unknown }>;
let infos: Array<{ message: string; fields: unknown }>;
function coord(producer: RecoveryBundleProducer = cache, isExecuting: (runId: string) => boolean = () => false,
  extra: { livePinSettleMs?: number; liveMaxPerPass?: number; liveBackoffBaseMs?: number } = {}) {
  const log = nullLogger();
  log.warn = (message, fields) => { warnings.push({ message, fields }); };
  log.info = (message, fields) => { infos.push({ message, fields }); };
  return new RecoveryCoordinator({ client: archive, git: producer, log, recoveryRoot: cache.recoveryRoot,
    workerToken: TOKEN, now: () => now, isExecuting, ...extra });
}
/** A producer with no archived-pin cleanup capability: uploads succeed and the pin survives. */
function withoutCleanup(): RecoveryBundleProducer {
  return { produceRecoveryBundle: cache.produceRecoveryBundle.bind(cache), fetchDefaultTip: cache.fetchDefaultTip.bind(cache),
    resolveRecoveryBareDir: cache.resolveRecoveryBareDir.bind(cache) };
}
const cleanupLogs = (list: Array<{ message: string; fields: unknown }>) =>
  list.filter(l => l.message === "recovery: uploaded record pin cleanup" || l.message === "recovery: uploaded record pin retained");
function file(record: RecoveryRecord) { return path.join(cache.recoveryRoot, RUN, `${record.captureId}.json`); }
function mac(record: object) {
  const key = createHmac("sha256", TOKEN).update("uzi-recovery-journal-v1").digest();
  return createHmac("sha256", key).update(canonicalJson(record)).digest("hex");
}
async function raw(record: RecoveryRecord) {
  await fs.writeFile(file(record), JSON.stringify({ ...record, mac: mac(record) }));
}
async function only(c = coord()) {
  const records = await c.inspect(RUN);
  assert.equal(records.length, 1);
  return records[0]!;
}
function present(generation = 4) {
  try { return git(bare, "rev-parse", "--verify", ref(generation)) === sha; } catch { return false; }
}
async function retry(c: RecoveryCoordinator, mode: "boot" | "live", signal?: AbortSignal) {
  now += 1_000_000;
  if (mode === "boot") await c.resumePending(signal);
  else await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => false, signal });
}
async function journal(generation: number | null = 4) {
  // Park-shaped: no finalization metadata, with the real transient pin and a sibling generation.
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, generation ?? 0, sha), true);
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, 9, sha), true);
  const c = coord();
  const record = (await c.pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: generation ?? undefined }))!;
  const outcome = await c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  assert.equal(outcome.state, "needs_action");
  const bundled = await only(c);
  assert.ok(bundled.bundlePath);
  return bundled;
}

beforeEach(async () => {
  root = await fs.mkdtemp(path.join(os.tmpdir(), "issue-2263-"));
  cache = new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
  bare = path.join(root, "data", "repos", NAME);
  await fs.mkdir(bare, { recursive: true });
  git(bare, "init", "--bare", "-b", "main");
  const work = path.join(root, "work");
  await fs.mkdir(work);
  git(work, "init", "-b", "main");
  git(work, "config", "user.name", "fixture");
  git(work, "config", "user.email", "fixture@example.invalid");
  git(work, "config", "commit.gpgsign", "false");
  await fs.writeFile(path.join(work, "work.txt"), "parked work\n");
  git(work, "add", "work.txt");
  git(work, "commit", "-m", "park");
  sha = git(work, "rev-parse", "HEAD");
  git(work, "push", bare, "HEAD:refs/uzi-runner/agent/park");
  mock.method(cache, "fetchDefaultTip", async () => { throw new Error("no forge credentials"); });
  archive = new Archive();
  warnings = [];
  infos = [];
  now = 100_000;
});
afterEach(async () => {
  mock.restoreAll();
  await fs.rm(root, { recursive: true, force: true });
});


/** A pre-upgrade journal: the bundled record never carried recoveryPinBareDir. */
async function preUpgrade() {
  const record = await journal();
  const { recoveryPinBareDir: _cleanup, ...old } = record;
  await raw(old);
  return old as RecoveryRecord;
}

it("case 1: a pre-upgrade self-contained record without recoveryPinBareDir loses its pin on upload", async () => {
  const record = await preUpgrade();
  assert.ok(present(4));
  archive.uploadError = undefined;
  const out = await coord().captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  assert.equal(out.state, "uploaded");
  assert.equal(present(4), false, "pin found by discovery under the repos root and deleted");
  assert.equal(present(9), true, "a sibling generation's pin is untouched");
  assert.deepEqual(cleanupLogs(infos).map(l => (l.fields as { state: string }).state), ["deleted"]);
});

it("case 2: an uploaded record with a surviving pin is cleaned at boot; the next boot is already_absent and the journal is untouched", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  assert.equal((await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" })).state, "uploaded");
  assert.equal(present(4), true, "crash-shaped: uploaded was written but the pin survived");
  const uploadsAfterCapture = archive.uploads;
  const bytes = await fs.readFile(file(record));
  const c = coord();
  const snapshot = await c.snapshotBootRecords();
  assert.equal(snapshot.length, 1);
  assert.equal(snapshot[0]!.state, "uploaded");
  await c.resumePending(undefined, snapshot);
  assert.equal(present(4), false);
  assert.equal(present(9), true);
  assert.equal(archive.uploads, uploadsAfterCapture, "no re-upload of an uploaded record");
  assert.deepEqual(await fs.readFile(file(record)), bytes, "nothing is written to the journal");
  assert.deepEqual(cleanupLogs(infos).map(l => (l.fields as { state: string }).state), ["deleted"]);
  infos.length = 0;
  await coord().resumePending();
  assert.deepEqual(cleanupLogs(infos).map(l => (l.fields as { state: string }).state), ["already_absent"]);
  assert.deepEqual(await fs.readFile(file(record)), bytes);
  assert.deepEqual(cleanupLogs(warnings), []);
});

for (const mode of ["boot", "live"] as const) it(`${mode}: a recreated exact pin is deleted again`, async () => {
  const record = await journal();
  archive.uploadError = undefined;
  await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  const c = coord(cache, () => false, { livePinSettleMs: 1_000 });
  await retry(c, mode);
  assert.equal(present(4), false);
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, 4, sha), true);
  assert.equal(present(4), true);
  await retry(c, mode);
  assert.equal(present(4), false);
  assert.equal(present(9), true);
});

for (const mode of ["boot", "live"] as const) it(`${mode}: a pin recreated after deletion is deleted by a restarted (fresh) coordinator, with no journal update in between`, async () => {
  const record = await journal();
  archive.uploadError = undefined;
  await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  await retry(coord(), mode);
  assert.equal(present(4), false, "first process deleted the pin");
  const bytes = await fs.readFile(file(record));
  // The runner re-anchors the exact pin; the process then dies before anything is journaled.
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, 4, sha), true);
  assert.equal(present(4), true);
  assert.deepEqual(await fs.readFile(file(record)), bytes, "no journal write after recreation");
  infos.length = 0;
  const restarted = coord(); // fresh process: empty in-memory attempt/backoff state
  if (mode === "boot") {
    const snapshot = await restarted.snapshotBootRecords();
    await restarted.resumePending(undefined, snapshot);
  } else {
    await retry(restarted, "live");
  }
  assert.equal(present(4), false, "the restarted coordinator deletes the recreated pin");
  assert.equal(present(9), true);
  assert.deepEqual(cleanupLogs(infos).map(l => (l.fields as { state: string }).state), ["deleted"]);
  assert.deepEqual(await fs.readFile(file(record)), bytes);
});

for (const mode of ["boot", "live"] as const) it(`${mode}: an already-uploaded self-contained record without recoveryPinBareDir or bareDir loses its surviving pin via discovery`, async () => {
  const uploaded = await survivor();
  const { recoveryPinBareDir: _p, bareDir: _b, ...old } = uploaded;
  await raw(old);
  const bytes = await fs.readFile(file(old as RecoveryRecord));
  assert.equal(present(4), true, "pre-upgrade + crash: the pin survived");
  await retry(coord(), mode); // fresh coordinator
  assert.equal(present(4), false, "found under the repos root and deleted");
  assert.equal(present(9), true);
  assert.deepEqual(cleanupLogs(infos).map(l => (l.fields as { state: string }).state), ["deleted"]);
  assert.deepEqual(await fs.readFile(file(old as RecoveryRecord)), bytes);
});

// Both authenticated directory fields reach the primitive independently, so a disagreement between
// them is refused there (invalid_identity) instead of one silently winning.
for (const mode of ["boot", "live"] as const) it(`${mode}: an uploaded record whose recoveryPinBareDir and bareDir disagree retains its pin`, async () => {
  const uploaded = await survivor();
  const { recoveryPinBareDir: _p, bareDir: _b, ...rest } = uploaded;
  const conflicting = { ...rest, recoveryPinBareDir: NAME, bareDir: "forge.example+org+other.git" } as RecoveryRecord;
  await raw(conflicting);
  const bytes = await fs.readFile(file(conflicting));
  await retry(coord(), mode);
  assert.equal(present(4), true, "the pin is retained");
  assert.equal(git(bare, "rev-parse", "--verify", ref(4)), sha, "the ref is untouched");
  assert.equal(present(9), true);
  assert.deepEqual(cleanupLogs(infos), []);
  assert.deepEqual(cleanupLogs(warnings).map(l => (l.fields as { reason: string }).reason), ["invalid_identity"]);
  assert.deepEqual(await fs.readFile(file(conflicting)), bytes, "nothing is written to the journal");
});

for (const [shape, dirs] of [
  ["agreeing recoveryPinBareDir and bareDir", { recoveryPinBareDir: NAME, bareDir: NAME }],
  ["bareDir only", { bareDir: NAME }],
  ["recoveryPinBareDir only", { recoveryPinBareDir: NAME }],
] as const) for (const mode of ["boot", "live"] as const) it(`${mode}: an uploaded record with ${shape} loses its surviving pin`, async () => {
  const uploaded = await survivor();
  const { recoveryPinBareDir: _p, bareDir: _b, ...rest } = uploaded;
  const record = { ...rest, ...dirs } as RecoveryRecord;
  await raw(record);
  await retry(coord(), mode);
  assert.equal(present(4), false);
  assert.equal(present(9), true);
  assert.deepEqual(cleanupLogs(infos).map(l => (l.fields as { state: string }).state), ["deleted"]);
  assert.deepEqual(cleanupLogs(warnings), []);
});

it("retains and warns when the archive proof does not hold (not self-contained)", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  await raw({ ...record, selfContained: false });
  const out = await coord().captureAndUpload({ record: { ...record, selfContained: false }, barePath: bare, defaultBranch: "main" });
  assert.equal(out.state, "uploaded");
  assert.equal(present(4), true);
  const retained = warnings.filter(w => w.message === "recovery: uploaded record pin retained");
  assert.equal(retained.length, 1);
  assert.equal((retained[0]!.fields as { reason: string }).reason, "archive_unproven");
  await retry(coord(), "live");
  assert.equal(present(4), true, "later passes keep retaining it");
});

it("retains while the run is executing, and the live pass cleans it once it is not", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  let executing = true;
  const c = coord(cache, () => executing);
  assert.equal((await c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" })).state, "uploaded");
  assert.equal(present(4), true);
  assert.deepEqual(cleanupLogs(warnings), [], "the expected executing retention is not a warning");
  const retained = infos.filter(w => w.message === "recovery: uploaded record pin retained");
  assert.equal((retained[0]!.fields as { reason: string }).reason, "executing");
  assert.equal((retained[0]!.fields as { mode: string }).mode, "capture");
  now += 1_000_000;
  await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => true });
  assert.equal(present(4), true, "live pass skips an executing run");
  executing = false;
  now += 1_000_000;
  await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => false });
  assert.equal(present(4), false);
});

it("a settled record is not re-attempted before the settle interval, and is after it", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  const c = coord(cache, () => false, { livePinSettleMs: 10_000_000 });
  await retry(c, "live");
  assert.equal(present(4), false);
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, 4, sha), true);
  await retry(c, "live"); // 1_000_000 later: inside the settle interval
  assert.equal(present(4), true, "not re-attempted inside the settle interval");
  now += 10_000_000;
  await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => false });
  assert.equal(present(4), false, "re-attempted once the interval elapsed");
});

it("boot reaches a proven record behind four or more unproven ones, oldest first", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  const uploaded = await only();
  for (let i = 0; i < 5; i++) {
    await raw({ ...uploaded, captureId: `unproven-${i}`, selfContained: false, createdAt: uploaded.createdAt - 1_000 - i });
  }
  assert.equal(present(4), true);
  await coord().resumePending();
  assert.equal(present(4), false, "the proven record is not starved by unproven ones");
  assert.deepEqual(cleanupLogs(warnings), [], "unproven records are not re-warned at boot");
});

it("a credential-blocked pass with only pin cleanup runs it without moving the upload schedule", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  const c = coord();
  const internals = c as unknown as { credentialBlockedAt?: number; nextPassAt: number };
  internals.credentialBlockedAt = now + 5;
  await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => false });
  assert.equal(present(4), false, "local cleanup still ran while blocked");
  assert.equal(internals.nextPassAt, 0, "the blocked pass did not delay a newer authenticated heartbeat");
});

// ── issue #2263 M3: archive proof, journal-wide guards, primitive refusals, live scheduling ──

const retainedReasons = () => [...warnings, ...infos]
  .filter(l => l.message === "recovery: uploaded record pin retained").map(l => (l.fields as { reason: string }).reason);
const sha1 = (n: number) => String(n).repeat(40).slice(0, 40);
/** A bundled park record with the real pins, a journal tweak applied, then uploaded in capture mode. */
async function captureWith(tweak: (record: RecoveryRecord) => Promise<RecoveryRecord | void>, generation: number | null = 4,
  c = coord()) {
  const record = await journal(generation);
  archive.uploadError = undefined;
  const passed = (await tweak(record)) ?? record;
  const out = await c.captureAndUpload({ record: passed, barePath: bare, defaultBranch: "main" });
  assert.equal(out.state, "uploaded");
  return record;
}
/** An uploaded record whose pin survived (crash-shaped), ready for a boot or live pass. */
async function survivor(generation: number | null = 4) {
  const record = await journal(generation);
  archive.uploadError = undefined;
  await coord(withoutCleanup()).captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  return only();
}
async function repoint(generation: number, to: string) {
  git(bare, "update-ref", ref(generation), to);
}
async function otherCommit() {
  const work = path.join(root, "work");
  git(work, "commit", "--allow-empty", "-m", "elsewhere");
  const moved = git(work, "rev-parse", "HEAD");
  git(work, "push", bare, `${moved}:refs/uzi-runner/agent/elsewhere`);
  return moved;
}

// 1. Archive proof.
for (const [name, tweak] of [
  ["non-empty prerequisiteShas", (r: RecoveryRecord) => ({ ...r, selfContained: true, prerequisiteShas: [sha1(7)] })],
  ["missing prerequisiteShas", (r: RecoveryRecord) => { const { prerequisiteShas: _p, ...rest } = r; return { ...rest, selfContained: true } as RecoveryRecord; }],
  ["missing selfContained", (r: RecoveryRecord) => { const { selfContained: _s, ...rest } = r; return { ...rest, prerequisiteShas: [] } as RecoveryRecord; }],
] as const) it(`archive proof: ${name} keeps the pin (archive_unproven), now and at boot`, async () => {
  await captureWith(async record => { const t = tweak(record); await raw(t); return t; });
  assert.equal(present(4), true);
  assert.deepEqual(retainedReasons(), ["archive_unproven"]);
  await retry(coord(), "boot");
  await retry(coord(), "live");
  assert.equal(present(4), true);
});
it("archive proof: a guarded uploaded record is never cleaned, at boot or live", async () => {
  const uploaded = await survivor();
  await raw({ ...uploaded, inventoryGuarded: true });
  await retry(coord(), "boot");
  await retry(coord(), "live");
  assert.equal(present(4), true);
  assert.deepEqual(cleanupLogs(infos).filter(l => (l.fields as { state?: string }).state !== undefined), []);
});

// 2. Same pin-generation siblings.
for (const state of ["bundled", "pinned"] as const) it(`a same-generation ${state} sibling retains (sibling_pending)`, async () => {
  await captureWith(async record => { await raw({ ...record, captureId: "sib", state }); });
  assert.deepEqual(retainedReasons(), ["sibling_pending"]);
  assert.equal(present(4), true);
});
it("a different-generation non-uploaded sibling does not block", async () => {
  await captureWith(async record => { await raw({ ...record, captureId: "sib", generation: 9, state: "bundled" }); });
  assert.deepEqual(retainedReasons(), []);
  assert.equal(present(4), false);
  assert.equal(present(9), true, "the sibling generation's pin survives");
});

// 3. v1 records: generation undefined is pin generation 0.
it("v1: an uploaded record with a pending v1 sibling keeps its generation-0 pin", async () => {
  await captureWith(async record => { await raw({ ...record, captureId: "sib", state: "bundled" }); }, null);
  assert.deepEqual(retainedReasons(), ["sibling_pending"]);
  assert.equal(present(0), true);
});
it("v1: an explicit generation 0 sibling is the same pin generation", async () => {
  await captureWith(async record => { await raw({ ...record, captureId: "sib", generation: 0, state: "bundled" }); }, null);
  assert.deepEqual(retainedReasons(), ["sibling_pending"]);
  assert.equal(present(0), true);
});
it("v1: a pending sibling of another generation does not block, and an uploaded v1 sibling does not either", async () => {
  await captureWith(async record => {
    await raw({ ...record, captureId: "sib", generation: 5, state: "bundled" });
    await raw({ ...record, captureId: "done", state: "uploaded" });
  }, null);
  assert.deepEqual(retainedReasons(), []);
  assert.equal(present(0), false);
  assert.equal(present(9), true);
});

// 4. Any guarded record in the run.
it("a guarded record of another generation retains inventory_guarded", async () => {
  await captureWith(async record => { await raw({ ...record, captureId: "guard", generation: 9, state: "uploaded", inventoryGuarded: true }); });
  assert.deepEqual(retainedReasons(), ["inventory_guarded"]);
  assert.equal(present(4), true);
});

// 5. An unreadable or tampered journal file anywhere in the run.
for (const [name, write] of [
  ["garbage", (dir: string) => fs.writeFile(path.join(dir, "zzz.json"), "{not json")],
  ["bad mac", async (dir: string) => { const r = await only(); await fs.writeFile(path.join(dir, "zzz.json"),
    JSON.stringify({ ...r, captureId: "zzz", mac: "0".repeat(64) })); }],
] as const) it(`an unverifiable sibling journal file (${name}) retains unverified_journal`, async () => {
  await captureWith(async record => { await raw(record); await write(path.join(cache.recoveryRoot, RUN)); });
  assert.deepEqual(retainedReasons(), ["unverified_journal"]);
  assert.equal(present(4), true);
});

// 6. Residue quarantine.
it("quarantine latched at capture retains; unlatched later pass cleans", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  archive.accepted = async () => { latchResidueQuarantine({ cause: "c", runId: RUN, site: "terminal_drive" }, nullLogger()); };
  assert.equal((await coord().captureAndUpload({ record, barePath: bare, defaultBranch: "main" })).state, "uploaded");
  assert.deepEqual(retainedReasons(), ["quarantined"]);
  assert.equal(present(4), true);
  resetResidueQuarantineForTests();
  await retry(coord(), "boot");
  assert.equal(present(4), false);
});
it("boot: quarantine latched before the pass retains quarantined at the cleanup check, and cleans once unlatched", async () => {
  await survivor();
  latchResidueQuarantine({ cause: "c", runId: RUN, site: "terminal_drive" }, nullLogger());
  await retry(coord(), "boot");
  assert.equal(present(4), true);
  assert.deepEqual(retainedReasons(), ["quarantined"], "boot reaches the cleanup check, which refuses");
  resetResidueQuarantineForTests();
  await retry(coord(), "boot");
  assert.equal(present(4), false, "cleaned once the latch is gone");
});
it("live: quarantine latched before the pass is skipped by resumeLive's quarantine early return, and cleans once unlatched", async () => {
  await survivor();
  latchResidueQuarantine({ cause: "c", runId: RUN, site: "terminal_drive" }, nullLogger());
  await retry(coord(), "live");
  assert.equal(present(4), true);
  assert.deepEqual(retainedReasons(), [], "the early return never reaches the cleanup check");
  resetResidueQuarantineForTests();
  await retry(coord(), "live");
  assert.equal(present(4), false, "cleaned once the latch is gone");
});

// 7. Primitive refusals through the coordinator.
it("a pin re-pointed to another SHA is retained (target_changed) and left as found", async () => {
  const moved = await otherCommit();
  await captureWith(async () => { await repoint(4, moved); });
  assert.deepEqual(retainedReasons(), ["target_changed"]);
  assert.equal(git(bare, "rev-parse", ref(4)), moved);
  assert.equal(present(9), true);
});
it("an ambiguous pin in two bares (discovery) is retained and both survive", async () => {
  const record = await preUpgrade();
  const second = path.join(root, "data", "repos", "second.git");
  await fs.mkdir(second);
  git(second, "init", "--bare");
  git(second, "fetch", bare, `${sha}:refs/heads/main`);
  git(second, "update-ref", ref(4), sha);
  archive.uploadError = undefined;
  assert.equal((await coord().captureAndUpload({ record, barePath: bare, defaultBranch: "main" })).state, "uploaded");
  assert.deepEqual(retainedReasons(), ["ambiguous"]);
  assert.equal(present(4), true);
  assert.equal(git(second, "rev-parse", ref(4)), sha);
});
it("a successful cleanup leaves sibling generation pins and refs/uzi-settle refs", async () => {
  git(bare, "update-ref", `refs/uzi-settle/${RUN}/hold/source`, sha);
  await captureWith(async () => undefined);
  assert.equal(present(4), false);
  assert.equal(present(9), true);
  assert.equal(git(bare, "rev-parse", `refs/uzi-settle/${RUN}/hold/source`), sha);
});

// 8. Execution recheck at the commit gate.
it("a run that starts executing while the primitive holds the ref lock keeps its pin", async () => {
  let executing = false;
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const original = seam.spawnGit.bind(cache);
  mock.method(seam, "spawnGit", async (...args: any[]) => {
    if (args[1].includes("update-ref") && args[1].includes("--stdin")) executing = true;
    return original(...args);
  });
  await captureWith(async () => undefined, 4, coord(cache, () => executing));
  assert.equal(executing, true);
  assert.deepEqual(retainedReasons(), ["ineligible"]);
  assert.equal(present(4), true);
  assert.equal(existsSync(path.join(bare, ref(4) + ".lock")), false);
});

// 9. Budget expiry while waiting for the journal lock.
for (const mode of ["boot", "live"] as const) it(`${mode}: an aborted budget while the journal lock is held retains without throwing`, async () => {
  await survivor();
  const holder = coord() as unknown as { withJournalLock: <T>(run: string, fn: () => Promise<T>) => Promise<T> };
  let release!: () => void;
  const gate = new Promise<void>(r => { release = r; });
  let held!: () => void;
  const acquired = new Promise<void>(r => { held = r; });
  const holding = holder.withJournalLock(RUN, async () => { held(); await gate; });
  await acquired;
  const stop = new AbortController();
  // Evidence the pass is queued on the journal lock: the abortable acquire registers its waiter
  // synchronously inside this call, so aborting after `waiting` resolves can only cancel a queued waiter.
  const passCoord = coord();
  const pc = passCoord as unknown as { withJournalLock: <T>(run: string, fn: () => Promise<T>, signal?: AbortSignal) => Promise<T> };
  const realLock = pc.withJournalLock.bind(passCoord);
  let queued!: () => void;
  const waiting = new Promise<void>(r => { queued = r; });
  pc.withJournalLock = (run, fn, signal) => {
    const result = realLock(run, fn, signal);
    if (run === RUN && signal) queued();
    return result;
  };
  const pass = retry(passCoord, mode, stop.signal);
  await waiting;
  stop.abort();
  await pass; // must not throw
  release();
  await holding;
  assert.equal(present(4), true);
  assert.ok(["deadline", "cancelled"].includes(retainedReasons().at(-1) ?? ""), String(retainedReasons()));
  await retry(coord(), mode);
  assert.equal(present(4), false, "a later unblocked pass still cleans it");
});

// 10. Live scheduling.
/** N uploaded records, each with its own surviving pin generation (11 + i), distinct createdAt. */
async function manySurvivors(n: number, createdAts?: number[]) {
  const base = await survivor();
  const gens: number[] = [];
  for (let i = 0; i < n; i++) {
    const g = 11 + i;
    gens.push(g);
    assert.equal(await cache.anchorRecoveryHead(bare, RUN, g, sha), true);
    await raw({ ...base, captureId: `rec-${i}`, generation: g, createdAt: createdAts?.[i] ?? base.createdAt + i });
  }
  // Only the numbered fixtures stay in play: drop the original generation-4 record and pin.
  await fs.rm(file(base));
  git(bare, "update-ref", "-d", ref(4));
  return gens;
}
const cleanedGens = () => cleanupLogs(infos).filter(l => (l.fields as { mode?: string }).mode === "live")
  .map(l => (l.fields as { generation: number }).generation);
it("live: at most liveMaxPerPass records per pass, rotating least-recently-attempted first", async () => {
  const gens = await manySurvivors(5);
  const c = coord(cache, () => false, { liveMaxPerPass: 2, livePinSettleMs: 1_000_000_000 });
  const gone = () => gens.filter(g => !present(g));
  await retry(c, "live");
  assert.equal(gone().length, 2, "first pass is capped");
  await retry(c, "live");
  assert.equal(gone().length, 4);
  await retry(c, "live");
  assert.equal(gone().length, 5);
});
it("live: the cap rotates through records that stay attemptable (no starvation)", async () => {
  const gens = await manySurvivors(3);
  const c = coord(cache, () => false, { liveMaxPerPass: 2, livePinSettleMs: 0 });
  await retry(c, "live");
  const first = cleanedGens();
  assert.equal(first.length, 2);
  await retry(c, "live");
  assert.deepEqual(new Set(cleanedGens()), new Set(gens), "the record skipped by pass one is reached by pass two");
});
it("live: a retained record backs off exponentially", async () => {
  const moved = await otherCommit();
  await survivor();
  await repoint(4, moved);
  const c = coord(cache, () => false, { liveBackoffBaseMs: 1_000 });
  // One pass per second (the pass spacing equals the backoff base); a retained record is retried after 2, 4, 8 s.
  const attemptedAt: number[] = [];
  for (let k = 0; k <= 15; k++) {
    now = 1_000_000 + k * 1_000;
    const before = retainedReasons().length;
    await c.resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => false });
    if (retainedReasons().length > before) attemptedAt.push(k);
  }
  assert.deepEqual(attemptedAt, [0, 2, 6, 14]);
  assert.equal(git(bare, "rev-parse", ref(4)), moved);
});

// Boot: cap and order.
it("boot handles at most liveMaxPerPass records, oldest first, then the next ones", async () => {
  const gens = await manySurvivors(4, [4_000, 1_000, 3_000, 2_000]);
  const c = coord(cache, () => false, { liveMaxPerPass: 2 });
  const gone = () => gens.filter(g => !present(g));
  await c.resumePending();
  // createdAt order: rec-1 (gen 12), rec-3 (gen 14)
  assert.deepEqual(gone(), [12, 14]);
  await c.resumePending();
  assert.deepEqual(gone(), [11, 12, 13, 14]);
});
