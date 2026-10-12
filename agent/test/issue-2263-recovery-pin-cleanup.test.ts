import { afterEach, beforeEach, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { GitCache } from "../src/git.js";
import { canonicalJson, RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer, type RecoveryRecord } from "../src/recovery.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
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
  extra: { livePinSettleMs?: number } = {}) {
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
async function journal(generation: number | undefined = 4) {
  // Park-shaped: no finalization metadata, with the real transient pin and a sibling generation.
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, generation ?? 0, sha), true);
  assert.equal(await cache.anchorRecoveryHead(bare, RUN, 9, sha), true);
  const c = coord();
  const record = (await c.pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation }))!;
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
