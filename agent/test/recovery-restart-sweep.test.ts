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
  async reserveRecoveryCapture(runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push({ runId, req });
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

  it("never alters a bundled record", async () => {
    const coord = coordinator();
    await coord.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 3, ...FIN });
    await coord.resumePending(); // bundles + uploads
    const before = await only(coord, "r1");
    assert.equal(before.state, "uploaded");
    await coord.pin({ runId: "r1", sourceSha: baseSha, kind: "issue", branch: "agent/issue-1", generation: 3, ...FIN, defaultBranch: "other" });
    const after = await only(coord, "r1");
    assert.deepEqual(after, before);
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
