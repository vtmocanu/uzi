import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
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

  it("refuses an oversized self-contained bundle BEFORE writing it", async () => {
    const client = new FakeClient();
    const coord = coordinator(client);
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });
    const outPaths: string[] = [];
    // The post-write check also deletes an oversized file, so prove the bundle was never CREATED.
    const gitCalls: string[][] = [];
    const priv = cache as unknown as { runGit: (cwd: string | undefined, args: string[]) => Promise<string> };
    const realRunGit = priv.runGit.bind(cache);
    priv.runGit = (cwd, args) => {
      gitCalls.push(args);
      return realRunGit(cwd, args);
    };
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
    assert.equal(gitCalls.some((a) => a[0] === "bundle" && a[1] === "create"), false, "git bundle create never ran");
    assert.equal(fs.existsSync(outPaths[0]!), false, "no bundle was ever written");
    const rec = await only(coord, "r1");
    assert.equal(rec.reason, "oversized");
    assert.equal(rec.bundlePath, undefined);
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

  it("does not treat a hostile defaultBranch as a ref (no on_default for @{, non-ASCII or control names)", async () => {
    for (const db of ["main@{0}", "ma\u00efn", "ma\tin", "main.lock", "@{-1}", "main/"]) {
      const res = await cache.resolveRestartSource(BARE_DIR, baseSha, db);
      assert.equal(res.status, "unpublished", JSON.stringify(db));
    }
    assert.equal((await cache.resolveRestartSource(BARE_DIR, baseSha, "main")).status, "on_default");
  });
});
