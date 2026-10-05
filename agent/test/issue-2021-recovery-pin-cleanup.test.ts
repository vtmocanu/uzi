import { afterEach, beforeEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { RequestError } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { canonicalJson, RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer, type RecoveryRecord } from "../src/recovery.js";
import { PredecessorSettler, SettlementJournal } from "../src/recovery-settlement.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const TOKEN = "issue-2021-worker-fixture";
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
function coord(producer: RecoveryBundleProducer = cache) {
  const log = nullLogger();
  log.warn = (message, fields) => { warnings.push({ message, fields }); };
  return new RecoveryCoordinator({ client: archive, git: producer, log, recoveryRoot: cache.recoveryRoot,
    workerToken: TOKEN, now: () => now });
}
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
  root = await fs.mkdtemp(path.join(os.tmpdir(), "issue-2021-"));
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
  now = 100_000;
});
afterEach(async () => {
  mock.restoreAll();
  await fs.rm(root, { recursive: true, force: true });
});

for (const mode of ["boot", "live"] as const) describe(`${mode} durable pin cleanup`, () => {
  for (const generation of [4, 0, undefined]) it(`core success generation ${generation ?? "legacy"}`, async () => {
    // Re-MAC the generation-omitted legacy journal after the initial bytes are installed.
    let record = await journal(generation ?? 0);
    if (generation === undefined) {
      const { generation: _generation, ...legacy } = record;
      await raw(legacy);
      record = legacy;
    }
    assert.equal(record.recoveryPinBareDir, NAME);
    assert.equal(record.generation, generation);
    assert.equal(record.bareDir, undefined);
    assert.equal(record.defaultBranch, undefined);
    assert.equal(record.finalizationPin, undefined);
    assert.ok(present(generation ?? 0));
    archive.uploadError = undefined;
    const restarted = coord();
    await retry(restarted, mode);
    assert.equal((await only()).state, "uploaded");
    assert.equal(present(generation ?? 0), false);
    assert.equal(present(9), true);
    await assert.rejects(fs.stat(record.bundlePath!), { code: "ENOENT" });
    await retry(restarted, mode);
    assert.equal(archive.uploads, 2, "uploaded is skipped on repeated passes");
  });

  const errors = [
    ["transient", new Error("transport reset"), ""],
    ["credential", new RequestError("POST", "/capture", 401, ""), "credential_rejected"],
    ["ownership", new RequestError("POST", "/capture", 403, '{"reason":"not_authorized"}'), "stale_ownership"],
    ["route", new RequestError("POST", "/capture", 404, ""), "upload_rejected"],
    ["size", new RequestError("POST", "/capture", 413, ""), "archive_constraint"],
    ["matching integrity", new RequestError("POST", "/capture", 422, '{"reason":"integrity"}'), ""],
    ["mismatching integrity", new RequestError("POST", "/capture", 422, '{"reason":"integrity"}'), "local_bundle_mismatch"],
  ] as const;
  for (const stage of ["reserve", "upload"] as const) for (const [label, error, reason] of errors) {
    it(`retains on ${stage} ${label}`, async () => {
      const record = await journal();
      if (stage === "reserve") {
        const { serverCaptureId: _server, ...unreserved } = record;
        await raw(unreserved);
        archive.reserveError = error;
      } else archive.uploadError = error;
      if (label === "mismatching integrity") await fs.writeFile(record.bundlePath!, "corrupted bytes");
      await retry(coord(), mode);
      const failed = await only();
      assert.equal(failed.state, "needs_action");
      assert.equal(failed.reason, reason || (mode === "boot" ? "restart_upload_failed" : "upload_transient"));
      assert.equal(present(), true);
      assert.equal(present(9), true);
      assert.ok(await fs.stat(record.bundlePath!));
    });
  }

  for (const condition of ["incomplete", "missing bytes", "tampered", "removed", "aborted", ...(mode === "live" ? ["executing"] as const : [])] as const) {
    it(`retains on ${condition} without cleanup`, async () => {
      const record = await journal();
      archive.uploadError = undefined;
      if (condition === "incomplete") { const { checksum: _checksum, ...incomplete } = record; await raw(incomplete); }
      if (condition === "missing bytes") await fs.rm(record.bundlePath!);
      if (condition === "tampered") await fs.writeFile(file(record), JSON.stringify({ ...record, recoveryPinBareDir: "other", mac: "0".repeat(64) }));
      if (condition === "removed") await fs.rm(file(record));
      const controller = new AbortController();
      if (condition === "aborted") controller.abort();
      if (condition === "executing") {
        await coord().resumeLive({ authenticatedAtMs: now + 1, isExecuting: () => true });
      } else await retry(coord(), mode, controller.signal);
      assert.equal(present(), true);
      assert.equal(archive.uploads, 1);
    });
  }

  for (const condition of ["missing metadata", "old finalization metadata", "missing resolver", "missing delete", "missing bare", "resolver throws", "delete throws", "ref locked"] as const) {
    it(`uploads and retains on ${condition}`, async () => {
      let record = await journal();
      if (condition === "missing metadata" || condition === "old finalization metadata") {
        const { recoveryPinBareDir: _cleanup, ...old } = record;
        record = condition === "old finalization metadata" ? { ...old, bareDir: NAME, defaultBranch: "main", finalizationPin: true } : old;
        await raw(record);
      }
      const producer: RecoveryBundleProducer = {
        produceRecoveryBundle: cache.produceRecoveryBundle.bind(cache), fetchDefaultTip: cache.fetchDefaultTip.bind(cache),
        resolveRecoveryBareDir: cache.resolveRecoveryBareDir.bind(cache), deleteRecoveryPin: cache.deleteRecoveryPin.bind(cache),
      };
      if (condition === "missing resolver") delete producer.resolveRecoveryBareDir;
      if (condition === "missing delete") delete producer.deleteRecoveryPin;
      if (condition === "missing bare") producer.resolveRecoveryBareDir = async () => undefined;
      if (condition === "resolver throws") producer.resolveRecoveryBareDir = async () => { throw new Error("x".repeat(1024)); };
      if (condition === "delete throws") producer.deleteRecoveryPin = async () => { throw new Error("x".repeat(1024)); };
      if (condition === "ref locked") await fs.writeFile(path.join(bare, `${ref(4)}.lock`), "locked");
      archive.uploadError = undefined;
      const restarted = coord(producer);
      await retry(restarted, mode);
      const uploaded = await only();
      assert.equal(uploaded.state, "uploaded");
      assert.equal(uploaded.recoveryPinBareDir, record.recoveryPinBareDir);
      assert.equal(present(), true);
      await assert.rejects(fs.stat(record.bundlePath!), { code: "ENOENT" });
      const cleanupWarnings = warnings.filter(w => w.message === "recovery: uploaded bundle pin cleanup failed");
      assert.equal(cleanupWarnings.length, condition.endsWith("throws") ? 1 : 0);
      if (cleanupWarnings.length) assert.ok(JSON.stringify(cleanupWarnings[0]!.fields).length < 600);
      await retry(restarted, mode);
      assert.equal(archive.uploads, 2);
    });
  }

  for (const condition of ["removed", "tampered", "IO failure"] as const) it(`no cleanup when guarded write ${condition} after server acceptance`, async () => {
    const record = await journal();
    archive.uploadError = undefined;
    let unblock!: () => void;
    let accepted!: () => void;
    const reached = new Promise<void>(resolve => { accepted = resolve; });
    const gate = new Promise<void>(resolve => { unblock = resolve; });
    archive.accepted = async () => { accepted(); await gate; };
    const pass = retry(coord(), mode);
    await reached;
    if (condition === "removed") await fs.rm(file(record));
    if (condition === "tampered") await fs.writeFile(file(record), "{}");
    if (condition === "IO failure") mock.method(fs, "rename", async () => { throw Object.assign(new Error("journal write IO failure"), { code: "EIO" }); });
    unblock();
    await pass;
    assert.equal(archive.uploads, 2);
    assert.equal(present(), true);
    assert.equal(present(9), true);
    if (condition === "IO failure") assert.notEqual((await only()).state, "uploaded");
  });
});

it("foreground shared cleanup followed by the runner's old deletion is idempotent", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  assert.equal((await coord().captureAndUpload({ record, barePath: bare, defaultBranch: "main" })).state, "uploaded");
  assert.equal(present(), false);
  await cache.deleteRecoveryPin(bare, RUN, 4);
  assert.equal(present(), false);
  assert.equal(present(9), true);
});

it("fresh finalization restart production installs cleanup metadata and deletes the exact pin", async () => {
  await cache.anchorRecoveryHead(bare, RUN, 4, sha);
  await cache.anchorRecoveryHead(bare, RUN, 9, sha);
  await coord().pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: 4,
    finalizationPin: true, bareDir: NAME, defaultBranch: "main" });
  archive.uploadError = undefined;
  await coord().resumePending();
  const uploaded = await only();
  assert.equal(uploaded.recoveryPinBareDir, NAME);
  assert.equal(uploaded.generation, 4);
  assert.equal(uploaded.state, "uploaded");
  assert.equal(present(), false);
  assert.equal(present(9), true);
});

it("production failure retains pin and does not install cleanup metadata", async () => {
  await cache.anchorRecoveryHead(bare, RUN, 4, sha);
  const c = coord();
  const record = (await c.pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: 4,
    finalizationPin: true, bareDir: NAME, defaultBranch: "main" }))!;
  mock.method(cache, "produceRecoveryBundle", async () => { throw new Error("bundle failed"); });
  await c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  assert.equal((await only()).recoveryPinBareDir, undefined);
  await coord().resumePending();
  assert.equal(present(), true);
  assert.equal(archive.uploads, 0);
});

it("refuses unsafe and wrongly typed authenticated cleanup basenames and MAC tampering", async () => {
  const record = await journal();
  archive.uploadError = undefined;
  for (const name of ["", ".", "..", "../escape", "nested/repo", "back\\slash", "nul\0name", 42, null]) {
    await raw({ ...record, recoveryPinBareDir: name } as RecoveryRecord);
    assert.deepEqual(await coord().inspect(RUN), []);
    await coord().resumePending();
    assert.equal(present(), true);
  }
  await fs.writeFile(file(record), JSON.stringify({ ...record, recoveryPinBareDir: "other.git", mac: mac(record) }));
  assert.deepEqual(await coord().inspect(RUN), []);
  assert.equal(archive.uploads, 1);
});

it("real bare resolver refuses traversal, symlink, nonbare, file and missing directories", async () => {
  assert.equal(await cache.resolveRecoveryBareDir(NAME), bare);
  const repos = path.dirname(bare);
  await fs.symlink(bare, path.join(repos, "linked.git"));
  await fs.mkdir(path.join(repos, "empty.git"));
  await fs.writeFile(path.join(repos, "file.git"), "file");
  for (const name of ["", ".", "..", "../work", "nested/repo", "back\\slash", "nul\0name", "linked.git", "empty.git", "file.git", "missing.git"]) {
    assert.equal(await cache.resolveRecoveryBareDir(name), undefined, name);
    assert.equal((await cache.resolveRestartSource(name, sha, "main")).status, "missing_bare", name);
  }
});

it("new reader accepts legacy MAC records; old recognized-field projection rejects new records", async () => {
  const record = await journal();
  const { recoveryPinBareDir: _cleanup, ...oldProjection } = record;
  assert.notEqual(mac(oldProjection), mac(record));
  await raw(oldProjection);
  assert.deepEqual(await only(), oldProjection);
  await raw(record);
  assert.deepEqual(await only(), record);
});

it("adopted predecessor archive keeps actual settlement evidence and both journals until release", async () => {
  const record = await journal();
  const journalStore = new SettlementJournal({ root: path.join(root, "settlement"), workerToken: TOKEN, log: nullLogger() });
  const settler = new PredecessorSettler({ journal: journalStore, log: nullLogger(),
    client: { settleRecoveryHold: async () => { throw new Error("not released"); } },
    cleanup: { deleteSettlementRefs: cache.deleteSettlementRefs.bind(cache), deleteRecoveryPin: cache.deleteRecoveryPin.bind(cache),
      forgetGeneration: async (runId, generation) => coord().forgetGeneration(runId, generation) } });
  assert.equal(await cache.pinSettlementRefs(bare, RUN, "hold-4", { source: sha, adopted: sha, pushed: sha }), true);
  assert.equal(await settler.recordAdoption({ version: 1, runId: RUN, holdId: "hold-4", predecessorGeneration: 4,
    successorGeneration: 9, sourceSha: sha, sourceCaptureId: record.captureId, adoptedSha: sha,
    seededFrom: "tracking", branch: "agent/park", barePath: bare, createdAt: now, state: "adopted", attempts: 0 }), true);
  archive.uploadError = undefined;
  await retry(coord(), "live");
  assert.equal(present(), false);
  for (const kind of ["source", "adopted", "pushed"]) assert.equal(git(bare, "rev-parse", `refs/uzi-settle/${RUN}/hold-4/${kind}`), sha);
  assert.equal((await journalStore.get(RUN, "hold-4"))!.state, "adopted");
  assert.equal((await only()).state, "uploaded");
});

for (const branch of ["source_advanced", "existing_bundle"] as const) it(`installer ${branch} leaves cleanup metadata unchanged`, async () => {
  const c = coord();
  const record = (await c.pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: 4 }))!;
  let produced!: () => void;
  let unblock!: () => void;
  const reached = new Promise<void>(resolve => { produced = resolve; });
  const gate = new Promise<void>(resolve => { unblock = resolve; });
  const original = cache.produceRecoveryBundle.bind(cache);
  mock.method(cache, "produceRecoveryBundle", async (barePath: Parameters<GitCache["produceRecoveryBundle"]>[0], opts: Parameters<GitCache["produceRecoveryBundle"]>[1]) => {
    const result = await original(barePath, opts);
    produced();
    await gate;
    return result;
  });
  const capture = c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  await reached;
  if (branch === "source_advanced") {
    await c.pin({ runId: RUN, sourceSha: "a".repeat(40), kind: "issue", branch: "agent/park", generation: 4 });
  } else {
    // Model another writer installing journaled bytes before this installer gets its lock.
    const bundlePath = path.join(root, "winner.bundle");
    const result = await original(bare, { sourceSha: sha, outPath: bundlePath });
    await raw({ ...record, state: "bundled", bundlePath, byteSize: result.byteSize, checksum: result.checksum,
      chunkCount: result.chunkCount, prerequisiteShas: result.prerequisiteShas, selfContained: result.selfContained });
  }
  unblock();
  const outcome = await capture;
  assert.equal((await only()).recoveryPinBareDir, undefined);
  if (branch === "source_advanced") {
    assert.equal(outcome.reason, "source_advanced");
    assert.equal(archive.uploads, 0);
  } else assert.equal(archive.uploads, 1);
});

it("boot production failure leaves the actual pin without cleanup metadata", async () => {
  await cache.anchorRecoveryHead(bare, RUN, 4, sha);
  await coord().pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: 4,
    finalizationPin: true, bareDir: NAME, defaultBranch: "main" });
  mock.method(cache, "produceRecoveryBundle", async () => { throw new Error("production failed"); });
  await coord().resumePending();
  assert.equal((await only()).reason, "bundle_failed");
  assert.equal((await only()).recoveryPinBareDir, undefined);
  assert.equal(present(), true);
  assert.equal(archive.uploads, 0);
});

it("no-archive already-published outcome still permits the foreground pin deletion", async () => {
  await cache.anchorRecoveryHead(bare, RUN, 4, sha);
  const c = coord();
  const record = (await c.pin({ runId: RUN, sourceSha: sha, kind: "issue", branch: "agent/park", generation: 4 }))!;
  const original = cache.produceRecoveryBundle.bind(cache);
  mock.method(cache, "produceRecoveryBundle", async (barePath: Parameters<GitCache["produceRecoveryBundle"]>[0], opts: Parameters<GitCache["produceRecoveryBundle"]>[1]) => ({ ...await original(barePath, opts), alreadyPublished: true }));
  const out = await c.captureAndUpload({ record, barePath: bare, defaultBranch: "main" });
  assert.equal(out.reason, "already_published");
  assert.equal(archive.uploads, 0);
  assert.equal(present(), true, "foreground owns the no-archive deletion");
  await cache.deleteRecoveryPin(bare, RUN, 4);
  assert.equal(present(), false);
});
