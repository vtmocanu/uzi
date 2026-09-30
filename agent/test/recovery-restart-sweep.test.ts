import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHmac, randomBytes } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, RecoveryBundleTooLargeError } from "../src/git.js";
import { canonicalJson, RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
} from "../src/protocol.js";

// issue #1742 D4(a) — the boot sweep's handling of `pinned` records: a finalization-pinned
// record is verified against the real bare and captured PAT-less at its exact generation; an
// early pin is never bundled and never read as "no unpublished work"; only records snapshotted
// before register are processed. Real bare repos, a fake archive client.

const TOKEN = "worker-join-token-abcdef0123456789";
const BARE_DIR = "forge.example+org+repo.git";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const git = (cwd: string, args: string[]): string =>
  execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();

class FakeClient implements RecoveryArchiveClient {
  reserveCalls: Array<{ runId: string; req: RecoveryReserveRequest }> = [];
  uploadCalls: Array<{ runId: string; bytes: number }> = [];
  /** Throw on the next N reserve / upload calls (a transient server failure). */
  failReserves = 0;
  failUploads = 0;
  onReserve?: () => Promise<void>;
  async reserveRecoveryCapture(runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push({ runId, req });
    if (this.failReserves > 0) {
      this.failReserves--;
      throw new Error("reserve: server unavailable");
    }
    await this.onReserve?.();
    return { capture_id: "server-capture-1", state: "preparing" };
  }
  async getRecoveryCaptureStatus(_r: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(
    runId: string,
    captureId: string,
    _m: RecoveryUploadManifest,
    bundle: Readable,
  ): Promise<RecoveryCaptureStatusResponse> {
    if (this.failUploads > 0) {
      this.failUploads--;
      bundle.destroy();
      throw new Error("upload: server unavailable");
    }
    let n = 0;
    for await (const c of bundle) n += (c as Buffer).length;
    this.uploadCalls.push({ runId, bytes: n });
    return { capture_id: captureId, state: "available", manifest_bound: true };
  }
  async releaseRecoveryCustody(runId: string): Promise<RecoveryReleaseResponse> {
    return { run_id: runId, released: true, holds_released: 1 };
  }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    return { run_id: runId, holds: [] };
  }
}

let base: string;
let cache: GitCache;
let bare: string;
let baseSha: string;
let workSha: string;

beforeEach(() => {
  base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-restart-sweep-"));
  const dataDir = path.join(base, "data");
  cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = path.join(dataDir, "repos", BARE_DIR);
  fs.mkdirSync(bare, { recursive: true });
  execFileSync("git", ["init", "--bare", "-b", "main", bare], { env: GIT_ENV, stdio: "pipe" });
  git(bare, ["config", "gc.auto", "0"]);
  const work = path.join(base, "work");
  fs.mkdirSync(work);
  execFileSync("git", ["init", "-b", "main", work], { env: GIT_ENV, stdio: "pipe" });
  for (const [k, v] of [["user.email", "f@uzi.local"], ["user.name", "f"], ["commit.gpgsign", "false"]]) git(work, ["config", k!, v!]);
  fs.writeFileSync(path.join(work, "a.txt"), "base\n");
  git(work, ["add", "."]);
  git(work, ["commit", "-m", "base"]);
  baseSha = git(work, ["rev-parse", "HEAD"]);
  git(work, ["checkout", "-b", "agent/issue-1"]);
  fs.writeFileSync(path.join(work, "b.txt"), "unpublished work\n");
  git(work, ["add", "."]);
  git(work, ["commit", "-m", "work"]);
  workSha = git(work, ["rev-parse", "HEAD"]);
  // The bare keeps the default branch at refs/remotes/origin/main, as every fetch does.
  git(work, ["push", bare, "main:refs/remotes/origin/main", "agent/issue-1:refs/uzi-runner/agent/issue-1"]);
});

afterEach(() => {
  fs.rmSync(base, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
});

function coordinator(client: RecoveryArchiveClient = new FakeClient()): RecoveryCoordinator {
  return new RecoveryCoordinator({
    client,
    git: cache,
    log: nullLogger(),
    recoveryRoot: cache.recoveryRoot,
    workerToken: TOKEN,
    now: () => 1_700_000_000_000,
  });
}

/** Journal a record directly with a valid MAC (as a previous process would have left it). */
function writeRaw(rec: RecoveryRecord): void {
  const key = createHmac("sha256", TOKEN).update("uzi-recovery-journal-v1").digest();
  const mac = createHmac("sha256", key).update(canonicalJson(rec)).digest("hex");
  const dir = path.join(cache.recoveryRoot, rec.runId);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, `${rec.captureId}.json`), JSON.stringify({ ...rec, mac }));
}

const FIN = { bareDir: BARE_DIR, defaultBranch: "main", finalizationPin: true } as const;

async function only(coord: RecoveryCoordinator, runId: string): Promise<RecoveryRecord> {
  const records = await coord.inspect(runId);
  assert.equal(records.length, 1);
  return records[0]!;
}

describe("RecoveryCoordinator.pin — finalization facts (issue #1742 D4a)", () => {
  it("the finalization pin updates the early record in place and the MAC verifies after reload", async () => {
    const coord = coordinator();
    const early = await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 3 });
    assert.equal(early!.finalizationPin, undefined, "the early pin never sets the flag");
    const fin = await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 3, ...FIN });
    assert.equal(fin!.captureId, early!.captureId, "the SAME record is reused");
    const reloaded = await only(coordinator(), "r1");
    assert.equal(reloaded.sourceSha, workSha);
    assert.equal(reloaded.bareDir, BARE_DIR);
    assert.equal(reloaded.defaultBranch, "main");
    assert.equal(reloaded.finalizationPin, true);
  });

  it("never alters an uploaded, bundled or needs_action-with-bundle record", async () => {
    const coord = coordinator();
    // uploaded: produced by a real sweep.
    await coord.pin({ runId: "up", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 3, ...FIN });
    await coord.resumePending();
    // bundled + needs_action(with bundle): journaled directly (a real bundle is not needed, the
    // pin must simply refuse to touch a record that already names journaled bytes).
    for (const [runId, state] of [["bu", "bundled"], ["na", "needs_action"]] as const) {
      writeRaw({
        version: 1, runId, captureId: `cap-${runId}`, sourceSha: workSha, kind: "issue", branch: "agent/issue-1",
        generation: 3, createdAt: 1, state, bundlePath: "/nonexistent/x.bundle", byteSize: 5, checksum: "ab",
        chunkCount: 1, bareDir: BARE_DIR, defaultBranch: "main", finalizationPin: true,
        ...(state === "needs_action" ? { reason: "restart_upload_failed" } : {}),
      });
    }
    for (const runId of ["up", "bu", "na"]) {
      const before = await only(coord, runId);
      await coord.pin({ runId, sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 3, ...FIN, defaultBranch: "other" });
      await coord.pin({ runId, sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 3 });
      assert.deepEqual(await only(coord, runId), before, runId);
    }
  });

  it("a later non-finalization pin that re-points the source clears the finalization label", async () => {
    const coord = coordinator();
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 3, ...FIN });
    // Same head, non-finalization pin: the label is still true.
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 3 });
    assert.equal((await only(coord, "r1")).finalizationPin, true);
    // A different head (a restore-point transfer): the label, bare and branch are dropped.
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 3 });
    const rec = await only(coordinator(), "r1");
    assert.equal(rec.sourceSha, baseSha);
    assert.equal(rec.finalizationPin, undefined);
    assert.equal(rec.bareDir, undefined);
    assert.equal(rec.defaultBranch, undefined);
    const client = new FakeClient();
    await coordinator(client).resumePending();
    assert.equal((await only(coord, "r1")).reason, "early_pin_only_after_restart");
    assert.equal(client.reserveCalls.length, 0);
  });
});

describe("RecoveryCoordinator.resumePending — pinned records after restart (issue #1742 D4a)", () => {
  it("captures a finalization-pinned record with unpublished work at its exact generation", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    const restarted = coordinator(client);
    await restarted.resumePending();
    assert.equal(client.reserveCalls.length, 1);
    assert.equal(client.reserveCalls[0]!.req.generation, 7);
    assert.equal(client.reserveCalls[0]!.req.source_sha, workSha);
    assert.equal(client.uploadCalls.length, 1);
    assert.ok(client.uploadCalls[0]!.bytes > 0);
    assert.equal((await only(restarted, "r1")).state, "uploaded");
  });

  it("holds a finalization-pinned head already on the default branch: no bundle, no reserve", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    await coordinator(client).resumePending();
    assert.equal(client.reserveCalls.length, 0);
    const rec = await only(coord, "r1");
    assert.equal(rec.state, "needs_action");
    assert.equal(rec.reason, "no_unpublished_work_after_restart");
    assert.equal(rec.bundlePath, undefined);
  });

  it("a missing bare is source_not_verifiable_after_restart with no reserve", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({
      runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7,
      ...FIN, bareDir: "gone.example+org+repo.git",
    });
    await coordinator(client).resumePending();
    assert.equal(client.reserveCalls.length, 0);
    assert.equal((await only(coord, "r1")).reason, "source_not_verifiable_after_restart");
  });

  it("a SHA absent from the bare is source_not_verifiable_after_restart with no reserve", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({
      runId: "r1", sourceSha: "9".repeat(40), kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN,
    });
    await coordinator(client).resumePending();
    assert.equal(client.reserveCalls.length, 0);
    assert.equal((await only(coord, "r1")).reason, "source_not_verifiable_after_restart");
  });

  it("an early pin is early_pin_only_after_restart: never bundled, never no-unpublished-work", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    // Even with the start tip ON the default branch, the early pin must not be classified as
    // "no unpublished work": the committed work never reached this record.
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 7 });
    await coordinator(client).resumePending();
    assert.equal(client.reserveCalls.length, 0);
    assert.equal(client.uploadCalls.length, 0);
    const rec = await only(coord, "r1");
    assert.equal(rec.reason, "early_pin_only_after_restart");
    assert.equal(rec.bundlePath, undefined);
  });

  it("an old-shape record (none of the new fields) still authenticates and maps like an early pin", async () => {
    const coord = coordinator();
    const legacy = await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1" });
    assert.ok(legacy);
    assert.equal("bareDir" in legacy, false);
    const client = new FakeClient();
    await coordinator(client).resumePending();
    assert.equal(client.reserveCalls.length, 0);
    assert.equal((await only(coord, "r1")).reason, "early_pin_only_after_restart");
  });

  it("leaves a record created after the boot snapshot untouched", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "old", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 1 });
    const snapshot = await coord.snapshotBootRecords();
    await coord.pin({ runId: "live", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 2, ...FIN });
    await coord.resumePending(undefined, snapshot);
    assert.equal((await only(coord, "old")).state, "needs_action");
    const live = await only(coord, "live");
    assert.equal(live.state, "pinned");
    assert.equal(client.reserveCalls.length, 0);
  });

  it("refuses a re-MACed record whose bareDir escapes the repos root", async () => {
    const outside = path.join(base, "data", "evil.git");
    execFileSync("git", ["init", "--bare", outside], { env: GIT_ENV, stdio: "pipe" });
    const coord = coordinator();
    for (const [i, bareDir] of ["../evil.git", "a/b.git", ".."].entries()) {
      const rec: RecoveryRecord = {
        version: 1, runId: `t${i}`, captureId: `cap${i}`, sourceSha: workSha, kind: "issue", branch: "agent/issue-1",
        generation: 1, createdAt: 1, state: "pinned", bareDir, defaultBranch: "main", finalizationPin: true,
      };
      const key = createHmac("sha256", TOKEN).update("uzi-recovery-journal-v1").digest();
      const mac = createHmac("sha256", key).update(canonicalJson(rec)).digest("hex");
      const dir = path.join(cache.recoveryRoot, rec.runId);
      fs.mkdirSync(dir, { recursive: true });
      fs.writeFileSync(path.join(dir, "cap" + i + ".json"), JSON.stringify({ ...rec, mac }));
      assert.deepEqual(await coord.inspect(rec.runId), [], `${bareDir} is refused at load`);
    }
    // Defence in depth: the git cache itself refuses to resolve them.
    for (const bareDir of ["../evil.git", "a/b.git", "..", ".", ""]) {
      assert.equal((await cache.resolveRestartSource(bareDir, workSha, "main")).status, "missing_bare", bareDir);
    }
    const client = new FakeClient();
    await coordinator(client).resumePending();
    assert.equal(client.reserveCalls.length, 0);
  });
});

describe("RecoveryCoordinator.resumePending — retry, second restart, live-flight races (issue #1742 D4)", () => {
  it("a failed upload keeps the bundle with a retryable reason; the next restart uploads it", async () => {
    const client = new FakeClient();
    client.failUploads = 1;
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    await coordinator(client).resumePending();
    const failed = await only(coord, "r1");
    assert.equal(failed.state, "needs_action");
    assert.equal(failed.reason, "restart_upload_failed");
    assert.ok(failed.bundlePath && failed.checksum && typeof failed.byteSize === "number");
    assert.equal(failed.serverCaptureId, "server-capture-1", "the reserved capture id survives");
    assert.ok(fs.existsSync(failed.bundlePath!), "the bundle file is not orphaned");
    await coordinator(client).resumePending();
    assert.equal((await only(coord, "r1")).state, "uploaded");
    assert.equal(client.uploadCalls.length, 1);
  });

  it("a failed reserve keeps the bundle with a retryable reason; the next restart uploads it", async () => {
    const client = new FakeClient();
    client.failReserves = 1;
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    await coordinator(client).resumePending();
    const failed = await only(coord, "r1");
    assert.equal(failed.state, "needs_action");
    assert.equal(failed.reason, "restart_upload_failed");
    assert.ok(failed.bundlePath && failed.checksum, "the bundle facts are kept");
    assert.ok(fs.existsSync(failed.bundlePath!));
    await coordinator(client).resumePending();
    const ok = await only(coord, "r1");
    assert.equal(ok.state, "uploaded");
    assert.equal(client.uploadCalls.length, 1);
  });

  it("an early pin keeps early_pin_only_after_restart across two restarts", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 7 });
    await coordinator(client).resumePending();
    const first = await only(coord, "r1");
    await coordinator(client).resumePending();
    assert.deepEqual(await only(coord, "r1"), first);
    assert.equal(first.reason, "early_pin_only_after_restart");
    assert.equal(client.reserveCalls.length, 0);
  });

  it("source_not_verifiable becomes uploaded on the second restart once the SHA is present", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    // Pin a head the bare does not have yet (a transient miss: the fetch-back had not landed).
    const work = path.join(base, "work");
    const other = path.join(base, "other");
    execFileSync("git", ["clone", "-q", work, other], { env: GIT_ENV, stdio: "pipe" });
    for (const [k, v] of [["user.email", "f@uzi.local"], ["user.name", "f"], ["commit.gpgsign", "false"]]) git(other, ["config", k!, v!]);
    git(other, ["checkout", "-q", "-b", "later", workSha]);
    fs.writeFileSync(path.join(other, "c.txt"), "later\n");
    git(other, ["add", "."]);
    git(other, ["commit", "-q", "-m", "later"]);
    const laterSha = git(other, ["rev-parse", "HEAD"]);
    await coord.pin({ runId: "r1", sourceSha: laterSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    await coordinator(client).resumePending();
    assert.equal((await only(coord, "r1")).reason, "source_not_verifiable_after_restart");
    assert.equal(client.reserveCalls.length, 0);
    git(other, ["push", "-q", bare, "later:refs/uzi-runner/agent/later"]);
    await coordinator(client).resumePending();
    assert.equal((await only(coord, "r1")).state, "uploaded");
    assert.equal(client.reserveCalls[0]!.req.source_sha, laterSha);
  });

  it("does not resurrect a record the live flight removed while the sweep bundles", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    let produced = "";
    const producer = {
      fetchDefaultTip: cache.fetchDefaultTip.bind(cache),
      resolveRestartSource: cache.resolveRestartSource.bind(cache),
      async produceRecoveryBundle(b: string, o: Parameters<GitCache["produceRecoveryBundle"]>[1]) {
        const res = await cache.produceRecoveryBundle(b, o);
        produced = res.bundlePath;
        await coord.forgetGeneration("r1", 7); // the live G+1 flight's cleanup lands mid-sweep
        return res;
      },
    };
    const sweeper = new RecoveryCoordinator({
      client, git: producer, log: nullLogger(), recoveryRoot: cache.recoveryRoot, workerToken: TOKEN,
    });
    await sweeper.resumePending();
    assert.deepEqual(await coord.inspect("r1"), [], "not resurrected");
    assert.ok(produced);
    assert.equal(fs.existsSync(produced), false, "the bundle this sweep produced is removed");
    assert.equal(client.reserveCalls.length, 0);
  });

  it("does not resurrect a record the live flight removed while the sweep reserves", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    client.onReserve = async () => coord.forgetGeneration("r1", 7);
    await coordinator(client).resumePending();
    assert.deepEqual(await coord.inspect("r1"), []);
    assert.equal(client.uploadCalls.length, 0);
    const dir = path.join(cache.recoveryRoot, "r1");
    assert.deepEqual(fs.existsSync(dir) ? fs.readdirSync(dir).filter((n) => !n.endsWith(".tmp")) : [], []);
  });

  it("refuses an oversized self-contained bundle and leaves no bundle file behind", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    const outPaths: string[] = [];
    const producer = {
      fetchDefaultTip: cache.fetchDefaultTip.bind(cache),
      resolveRestartSource: cache.resolveRestartSource.bind(cache),
      produceRecoveryBundle: (b: string, o: Parameters<GitCache["produceRecoveryBundle"]>[1]) => {
        outPaths.push(o.outPath);
        return cache.produceRecoveryBundle(b, { ...o, maxBytes: 10 });
      },
    };
    const sweeper = new RecoveryCoordinator({
      client, git: producer, log: nullLogger(), recoveryRoot: cache.recoveryRoot, workerToken: TOKEN,
    });
    await sweeper.resumePending();
    assert.equal(outPaths.length, 1);
    assert.equal(fs.existsSync(outPaths[0]!), false, "the partial bundle was removed");
    const rec = await only(coord, "r1");
    assert.equal(rec.reason, "oversized");
    assert.equal(rec.bundlePath, undefined);
    assert.equal(client.reserveCalls.length, 0);
  });

  it("removes the bundle file when the record is removed before the bundle is journaled", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    let produced = "";
    const producer = {
      fetchDefaultTip: cache.fetchDefaultTip.bind(cache),
      resolveRestartSource: cache.resolveRestartSource.bind(cache),
      async produceRecoveryBundle(b: string, o: Parameters<GitCache["produceRecoveryBundle"]>[1]) {
        // The record disappears after the sweep's existence check, BEFORE the bundle bytes exist,
        // so the live flight's own cleanup cannot have removed the file this sweep then writes.
        await coord.forgetGeneration("r1", 7);
        fs.mkdirSync(path.dirname(o.outPath), { recursive: true }); // forget removed the empty run dir
        const res = await cache.produceRecoveryBundle(b, o);
        produced = res.bundlePath;
        return res;
      },
    };
    const sweeper = new RecoveryCoordinator({
      client, git: producer, log: nullLogger(), recoveryRoot: cache.recoveryRoot, workerToken: TOKEN,
    });
    await sweeper.resumePending();
    assert.ok(produced);
    assert.equal(fs.existsSync(produced), false, "no orphan bundle file");
    assert.deepEqual(await coord.inspect("r1"), []);
    assert.equal(client.reserveCalls.length, 0);
  });

  it("a journal write failure on one record does not abort the sweep of the next", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 1 }); // early pin
    await coord.pin({ runId: "r2", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 1, ...FIN });
    const sweeper = coordinator(client);
    const priv = sweeper as unknown as { writeRecord: (r: RecoveryRecord) => Promise<void> };
    const real = priv.writeRecord.bind(sweeper);
    priv.writeRecord = async (r) => {
      if (r.runId === "r1") throw Object.assign(new Error("ENOSPC: no space left on device"), { code: "ENOSPC" });
      return real(r);
    };
    await sweeper.resumePending();
    assert.equal((await only(coord, "r2")).state, "uploaded");
    assert.equal(client.uploadCalls.length, 1);
  });

  it("re-evaluates only source_not_verifiable_after_restart; other bundle-less needs_action reasons stay put", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    // Every one of these sources is fine (unpublished, present), so a re-evaluation would upload.
    for (const reason of ["oversized", "bundle_failed", "no_unpublished_work_after_restart", "capture_error"]) {
      writeRaw({
        version: 1, runId: `r-${reason}`, captureId: `cap-${reason}`, sourceSha: workSha, kind: "issue",
        branch: "agent/issue-1", generation: 3, createdAt: 1, state: "needs_action", reason, ...FIN,
      });
    }
    await coordinator(client).resumePending();
    await coordinator(client).resumePending();
    for (const reason of ["oversized", "bundle_failed", "no_unpublished_work_after_restart", "capture_error"]) {
      const rec = await only(coord, `r-${reason}`);
      assert.equal(rec.state, "needs_action", reason);
      assert.equal(rec.reason, reason);
    }
    assert.equal(client.reserveCalls.length, 0);
  });

  it("a non-finalization bundle-less needs_action record (live capture failure) keeps its reason", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    for (const reason of ["oversized", "bundle_failed", "capture_error"]) {
      writeRaw({
        version: 1, runId: `r-${reason}`, captureId: `cap-${reason}`, sourceSha: workSha, kind: "issue",
        branch: "agent/issue-1", generation: 3, createdAt: 1, state: "needs_action", reason,
      });
    }
    await coordinator(client).resumePending();
    for (const reason of ["oversized", "bundle_failed", "capture_error"]) {
      assert.equal((await only(coord, `r-${reason}`)).reason, reason);
    }
    assert.equal(client.reserveCalls.length, 0);
  });

  it("leaves a same-run G+1 record created after the snapshot untouched", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 1 });
    const snapshot = await coord.snapshotBootRecords();
    await coord.pin({
      runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 2, ...FIN,
    });
    const next = (await coord.inspect("r1")).find((r) => r.generation === 2)!;
    await coord.resumePending(undefined, snapshot);
    const records = await coord.inspect("r1");
    assert.equal(records.length, 2);
    assert.equal(records.find((r) => r.generation === 1)!.state, "needs_action");
    const live = records.find((r) => r.generation === 2)!;
    assert.deepEqual(live, next);
    assert.equal(client.reserveCalls.length, 0);
  });
});

describe("GitCache.resolveRestartSource — containment and ref-name hardening (issue #1742)", () => {
  it("rejects a bare that is a symlink, even to a real bare", async () => {
    const link = path.join(base, "data", "repos", "link.example+org+repo.git");
    fs.symlinkSync(bare, link);
    assert.equal((await cache.resolveRestartSource("link.example+org+repo.git", workSha, "main")).status, "missing_bare");
    assert.notEqual((await cache.resolveRestartSource(BARE_DIR, workSha, "main")).status, "missing_bare");
  });

  it("does not resolve a hostile defaultBranch through the reflog (main@{1} must not be on_default)", async () => {
    // A permissive splice would resolve refs/remotes/origin/main@{1} through the reflog to workSha's
    // OLD value, reporting an unpublished head as already on the default branch.
    git(bare, ["config", "core.logAllRefUpdates", "always"]);
    git(bare, ["update-ref", "refs/remotes/origin/main", workSha]);
    git(bare, ["update-ref", "refs/remotes/origin/main", baseSha]);
    assert.equal(git(bare, ["rev-parse", "refs/remotes/origin/main@{1}"]), workSha, "fixture: the reflog resolves");
    assert.equal((await cache.resolveRestartSource(BARE_DIR, workSha, "main")).status, "unpublished");
    assert.equal((await cache.resolveRestartSource(BARE_DIR, workSha, "main@{1}")).status, "unpublished");
  });

  it("does not treat a hostile defaultBranch as a ref (no on_default for @{, non-ASCII or control names)", async () => {
    for (const db of ["main@{0}", "ma\u00efn", "ma\tin", "main.lock", "@{-1}", "main/"]) {
      const res = await cache.resolveRestartSource(BARE_DIR, baseSha, db);
      assert.equal(res.status, "unpublished", JSON.stringify(db));
    }
    assert.equal((await cache.resolveRestartSource(BARE_DIR, baseSha, "main")).status, "on_default");
  });
});

describe("GitCache.produceRecoveryBundle — the size cap bounds the bytes actually written (issue #1742)", () => {
  const MIB = 1024 * 1024;
  const mkWork = (name: string): string => {
    const w = path.join(base, name);
    fs.mkdirSync(w);
    execFileSync("git", ["init", "-b", "main", w], { env: GIT_ENV, stdio: "pipe" });
    for (const [k, v] of [["user.email", "f@uzi.local"], ["user.name", "f"], ["commit.gpgsign", "false"]]) git(w, ["config", k!, v!]);
    return w;
  };
  const mkBare = (name: string): string => {
    const b = path.join(base, "data", "repos", name);
    fs.mkdirSync(b, { recursive: true });
    execFileSync("git", ["init", "--bare", "-b", "main", b], { env: GIT_ENV, stdio: "pipe" });
    git(b, ["config", "gc.auto", "0"]);
    return b;
  };

  it("refuses a delta-heavy history without ever writing more than the cap", async () => {
    const work = mkWork("delta-work");
    const blob = randomBytes(4 * MIB);
    fs.writeFileSync(path.join(work, "big.bin"), blob);
    git(work, ["add", "."]);
    git(work, ["commit", "-m", "A"]);
    const flipped = Buffer.from(blob);
    flipped[0] = flipped[0]! ^ 0xff;
    git(work, ["checkout", "--orphan", "h"]);
    git(work, ["rm", "-rf", "-q", "."]);
    fs.writeFileSync(path.join(work, "big.bin"), flipped);
    git(work, ["add", "."]);
    git(work, ["commit", "-m", "H"]);
    const h = git(work, ["rev-parse", "HEAD"]);
    const b = mkBare("delta.example+org+repo.git");
    git(work, ["push", b, "main:refs/heads/main", "h:refs/heads/h"]);
    git(b, ["gc", "--aggressive", "--prune=now", "-q"]);
    // The old estimate (`rev-list --disk-usage` of H) sees only the delta against the other branch.
    const cap = 1 * MIB;
    const out = path.join(base, "delta.bundle");
    let maxSeen = 0;
    const poll = setInterval(() => {
      try { maxSeen = Math.max(maxSeen, fs.statSync(out).size); } catch { /* not created yet */ }
    }, 1);
    try {
      await assert.rejects(
        cache.produceRecoveryBundle(b, { sourceSha: h, outPath: out, maxBytes: cap }),
        (err: unknown) => err instanceof RecoveryBundleTooLargeError && err.byteSize > cap && err.maxBytes === cap,
      );
    } finally {
      clearInterval(poll);
    }
    assert.ok(maxSeen <= cap, `the file grew to ${maxSeen} bytes, over the ${cap}-byte cap`);
    assert.equal(fs.existsSync(out), false, "the partial file is removed");
    assert.equal(git(b, ["for-each-ref", "refs/heads/recovered-source"]), "", "the transient ref is gone");
  });

  it("accepts a bundle that fits although its loose objects are far over the cap", async () => {
    const work = mkWork("loose-work");
    const buf = randomBytes(500 * 1024);
    let h = "";
    for (let i = 0; i < 30; i++) {
      buf.writeUInt32BE(i, 1000 + i * 4);
      fs.writeFileSync(path.join(work, "f.bin"), buf);
      git(work, ["add", "."]);
      git(work, ["commit", "-m", `c${i}`]);
    }
    h = git(work, ["rev-parse", "HEAD"]);
    const b = mkBare("loose.example+org+repo.git");
    git(b, ["config", "core.compression", "0"]);
    // Loose objects: fetch-by-push above the unpack limit keeps them unpacked.
    git(b, ["config", "receive.unpackLimit", "1000000"]);
    git(work, ["push", b, "main:refs/heads/main"]);
    const loose = execFileSync("git", ["-C", b, "count-objects", "-v"], { env: GIT_ENV, encoding: "utf8" });
    const looseKiB = Number(/^size: (\d+)/m.exec(loose)![1]);
    const cap = 4 * MIB;
    assert.ok(looseKiB * 1024 > cap, `fixture: loose objects (${looseKiB} KiB) must exceed the cap`);
    const out = path.join(base, "loose.bundle");
    const res = await cache.produceRecoveryBundle(b, { sourceSha: h, outPath: out, maxBytes: cap });
    assert.ok(res.byteSize > 0 && res.byteSize <= cap);
    assert.equal(res.selfContained, true);
    assert.equal(fs.statSync(out).size, res.byteSize);
    git(b, ["bundle", "verify", out]);
  });

  it("a normal fitting bundle verifies and imports into a clean clone", async () => {
    const out = path.join(base, "ok.bundle");
    const res = await cache.produceRecoveryBundle(bare, { sourceSha: workSha, outPath: out });
    assert.equal(fs.statSync(out).size, res.byteSize);
    git(bare, ["bundle", "verify", out]);
    const clone = path.join(base, "clean");
    fs.mkdirSync(clone);
    execFileSync("git", ["init", "-q", clone], { env: GIT_ENV, stdio: "pipe" });
    git(clone, ["fetch", "-q", out, "refs/heads/recovered-source:refs/heads/x"]);
    assert.equal(git(clone, ["rev-parse", "refs/heads/x"]), workSha);
  });
});
