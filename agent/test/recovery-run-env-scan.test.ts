import { NO_SCAN_NEEDLES } from "./run-env-test-support.js";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryRecord } from "../src/recovery.js";
import type {
  RecoveryCaptureStatusResponse, RecoveryHoldsResponse, RecoveryReleaseResponse, RecoveryReserveRequest,
  RecoveryReserveResponse, RecoveryUploadManifest, RunEnvRedactResponse,
} from "../src/protocol.js";
import { type ScanNeedle, runEnvClaimForms } from "../src/run-env-forms.js";
import { resolveScanNeedles, type NeedleResolution } from "../src/run-env-scan.js";

// PRD 4416 M2: every recovery bundle upload (the capture, the restart sweep's re-upload and the live
// re-drive share one upload function) is gated by the exact-value scan of the bundle's objects, bound to the
// journaled checksum. Real bare repos and real bundles, a fake archive client; values are obvious dummies.

const TOKEN = "worker-join-token-abcdef0123456789";
const BARE_DIR = "forge.example+org+repo.git";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const git = (cwd: string, args: string[]): string =>
  execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const SECRET = ["rv-Qk7", "Zp2m", "Xw9d", "Lt4s"].join("-");
const OLD_SECRET = ["rv-Hn3b", "Yc6v", "Fd1g", "Ke8j"].join("-");
const FIN = { bareDir: BARE_DIR, defaultBranch: "main", finalizationPin: true } as const;

class FakeClient implements RecoveryArchiveClient {
  reserveCalls = 0;
  uploadCalls = 0;
  async reserveRecoveryCapture(_runId: string, _req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls++;
    return { capture_id: "server-capture-1", state: "preparing" };
  }
  async getRecoveryCaptureStatus(_r: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(_run: string, captureId: string, _m: RecoveryUploadManifest, bundle: Readable): Promise<RecoveryCaptureStatusResponse> {
    for await (const c of bundle) assert.ok((c as Buffer).length);
    this.uploadCalls++;
    return { capture_id: captureId, state: "available", manifest_bound: true };
  }
  async releaseRecoveryCustody(runId: string): Promise<RecoveryReleaseResponse> { return { run_id: runId, released: true, holds_released: 1 }; }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> { return { run_id: runId, holds: [] }; }
}

let base: string;
let cache: GitCache;
let workSha: string;

beforeEach(() => {
  base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recovery-run-env-"));
  const dataDir = path.join(base, "data");
  cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = path.join(dataDir, "repos", BARE_DIR);
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
  git(work, ["checkout", "-b", "agent/issue-1"]);
  // The value sits in a MIDDLE commit and is gone from the tip.
  fs.writeFileSync(path.join(work, "conf.txt"), `db=${SECRET}\nold=${OLD_SECRET}\n`);
  git(work, ["add", "."]);
  git(work, ["commit", "-m", "add config"]);
  fs.writeFileSync(path.join(work, "conf.txt"), "db=redacted\n");
  git(work, ["add", "."]);
  git(work, ["commit", "-m", "scrub config"]);
  workSha = git(work, ["rev-parse", "HEAD"]);
  git(work, ["push", bare, "main:refs/remotes/origin/main", "agent/issue-1:refs/uzi-runner/agent/issue-1"]);
});

afterEach(() => {
  mock.restoreAll();
  fs.rmSync(base, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
});

const NEEDLES: ScanNeedle[] = runEnvClaimForms([{ var_id: UUID, scope: "repo", rev: 2, name: "DB_PASSWORD", value: SECRET, secret: true, description: "" }], []).needles;

function coordinator(client: RecoveryArchiveClient, scanNeedlesFor?: (runId: string) => Promise<NeedleResolution>): RecoveryCoordinator {
  return new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
    client, git: cache, log: nullLogger(), recoveryRoot: cache.recoveryRoot, workerToken: TOKEN, now: () => 1_700_000_000_000,
    ...(scanNeedlesFor ? { scanNeedlesFor } : {}),
  });
}
const only = async (c: RecoveryCoordinator, runId: string): Promise<RecoveryRecord> => (await c.inspect(runId))[0]!;
const pin = (c: RecoveryCoordinator) => c.pin({ runId: "r1", sourceSha: workSha, kind: "issue", branch: "agent/issue-1", generation: 7, ...FIN });

describe("the restart re-upload of a recovery bundle: a value published as a path", () => {
  it("is withheld when only a nested PATH joins to the value, and keeps the bundle as local custody", async () => {
    const pathValue = ["rv-Ab3k", "Mq7w-Zx2n", "Fe9t"].join("/");
    const work = path.join(base, "work");
    git(work, ["checkout", "-q", "-b", "agent/issue-2", "main"]);
    fs.mkdirSync(path.dirname(path.join(work, pathValue)), { recursive: true });
    fs.writeFileSync(path.join(work, pathValue), "harmless\n");
    git(work, ["add", "."]);
    git(work, ["commit", "-m", "update"]);
    const pathSha = git(work, ["rev-parse", "HEAD"]);
    git(work, ["push", path.join(base, "data", "repos", BARE_DIR), "agent/issue-2:refs/uzi-runner/agent/issue-2"]);
    const client = new FakeClient();
    const pinned = coordinator(client);
    await pinned.pin({ runId: "r2", sourceSha: pathSha, kind: "issue", branch: "agent/issue-2", generation: 7, ...FIN });
    const needles = runEnvClaimForms([{ var_id: UUID, scope: "repo", rev: 1, name: "DB_PASSWORD", value: pathValue, secret: true, description: "" }], []).needles;
    const restarted = coordinator(client, async () => ({ kind: "needles", needles }));
    await restarted.resumePending();
    assert.equal(client.uploadCalls, 0, "nothing uploaded");
    const held = await only(restarted, "r2");
    assert.equal(held.state, "needs_action");
    assert.equal(held.reason, "publish_scan_secret_found");
    assert.ok(held.bundlePath && fs.existsSync(held.bundlePath), "the bundle stays as local custody");
  });
});

describe("the restart re-upload of a recovery bundle", () => {
  it("is withheld when a middle commit carries a secret value, keeps the record and the bundle, and uploads once clean", async () => {
    const client = new FakeClient();
    await pin(coordinator(client));
    const restarted = coordinator(client, async () => ({ kind: "needles", needles: NEEDLES }));
    await restarted.resumePending();
    assert.equal(client.reserveCalls, 0, "not even a reserve");
    assert.equal(client.uploadCalls, 0);
    const held = await only(restarted, "r1");
    assert.equal(held.state, "needs_action");
    assert.equal(held.reason, "publish_scan_secret_found");
    assert.ok(held.bundlePath && fs.existsSync(held.bundlePath), "the bundle stays as local custody");
    assert.doesNotMatch(JSON.stringify(held), new RegExp(SECRET), "the journal never carries a value");

    // A later restart whose scan finds nothing (the variable was never registered for this run) uploads it.
    await coordinator(client, async () => ({ kind: "needles", needles: [] })).resumePending();
    assert.equal(client.uploadCalls, 1);
    assert.equal((await only(restarted, "r1")).state, "uploaded");
  });

  it("holds a RETIRED version that only the maintenance answer still names (restart, rotate, resume)", async () => {
    const client = new FakeClient();
    await pin(coordinator(client));
    const fetchProtected = async (): Promise<RunEnvRedactResponse> =>
      ({ protected: true, versions: [{ var_id: UUID, scope: "repo", rev: 1, name: "OLD_KEY", value: OLD_SECRET }] });
    const restarted = coordinator(client, () => resolveScanNeedles({ live: undefined, fetchProtected }));
    await restarted.resumePending();
    assert.equal(client.uploadCalls, 0);
    assert.equal((await only(restarted, "r1")).reason, "publish_scan_secret_found");
  });

  it("uploads when the maintenance answer says the run holds no secret receipt", async () => {
    const client = new FakeClient();
    await pin(coordinator(client));
    await coordinator(client, () => resolveScanNeedles({ live: undefined, fetchProtected: async () => ({ protected: false }) })).resumePending();
    assert.equal(client.uploadCalls, 1);
  });

  for (const status of [404, 409]) {
    it(`withholds when the maintenance answer is HTTP ${status} (terminal, restart, repo deleted or vault locked), then retries later`, async () => {
      const client = new FakeClient();
      await pin(coordinator(client));
      const failing = coordinator(client, () => resolveScanNeedles({
        live: undefined, fetchProtected: async () => { throw Object.assign(new Error("http"), { status }); },
      }));
      await failing.resumePending();
      assert.equal(client.reserveCalls, 0);
      assert.equal(client.uploadCalls, 0, "nothing unscanned leaves the worker");
      const held = await only(failing, "r1");
      assert.equal(held.state, "needs_action");
      assert.equal(held.reason, "publish_scan_withheld");
      assert.ok(held.bundlePath && fs.existsSync(held.bundlePath));
      await coordinator(client, () => resolveScanNeedles({ live: undefined, fetchProtected: async () => ({ protected: false }) })).resumePending();
      assert.equal(client.uploadCalls, 1, "the next pass, with an answer, uploads");
    });
  }

  it("the LIVE lane retries a withheld bundle on the same coordinator once the answer arrives (no restart)", async () => {
    const client = new FakeClient();
    await pin(coordinator(client));
    let answer: "down" | "up" = "down";
    let clock = 1_700_000_000_000;
    const live = new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
      client, git: cache, log: nullLogger(), recoveryRoot: cache.recoveryRoot, workerToken: TOKEN, now: () => clock,
      scanNeedlesFor: () => resolveScanNeedles({
        live: undefined,
        fetchProtected: async () => {
          if (answer === "down") throw Object.assign(new Error("http"), { status: 409 });
          return { protected: false };
        },
      }),
    });
    await live.resumePending();
    assert.equal((await only(live, "r1")).reason, "publish_scan_withheld");
    assert.equal(client.uploadCalls, 0);
    // Still down: the live pass retries, is withheld again, and uploads nothing.
    clock += 3_600_000;
    await live.resumeLive({ authenticatedAtMs: clock, isExecuting: () => false });
    assert.equal(client.uploadCalls, 0);
    assert.equal((await only(live, "r1")).reason, "publish_scan_withheld");
    // The answer comes back; a later live pass (past its backoff) uploads, with no new coordinator.
    answer = "up";
    clock += 24 * 3_600_000;
    await live.resumeLive({ authenticatedAtMs: clock, isExecuting: () => false });
    assert.equal(client.uploadCalls, 1);
    assert.equal((await only(live, "r1")).state, "uploaded");
  });

  it("withholds when the scanner cannot read the bundle or the bundle changed after it was journaled", async () => {
    const client = new FakeClient();
    await pin(coordinator(client));
    const needles = async (): Promise<NeedleResolution> => ({ kind: "needles", needles: NEEDLES });
    // Scanner failure: untrusted.
    mock.method(cache, "scanBundleForRunEnv", async () => ({ trusted: false as const, reason: "bundle_unreadable" }));
    await coordinator(client, needles).resumePending();
    assert.equal(client.uploadCalls, 0);
    assert.equal((await only(coordinator(client), "r1")).reason, "publish_scan_withheld");
    mock.restoreAll();
    // Scanner throws.
    mock.method(cache, "scanBundleForRunEnv", async () => { throw new Error("scanner exploded"); });
    await coordinator(client, needles).resumePending();
    assert.equal(client.uploadCalls, 0);
    mock.restoreAll();
    // The journaled checksum no longer matches the file's bytes.
    const record = await only(coordinator(client), "r1");
    fs.appendFileSync(record.bundlePath!, "x");
    await coordinator(client, async () => ({ kind: "needles", needles: NEEDLES })).resumePending();
    assert.equal(client.uploadCalls, 0);
    assert.equal((await only(coordinator(client), "r1")).reason, "publish_scan_withheld");
  });

  it("a run with no secret variables does no scan work and uploads as before", async (t) => {
    const client = new FakeClient();
    await pin(coordinator(client));
    const probe = t.mock.method(cache, "scanBundleForRunEnv");
    await coordinator(client, async () => ({ kind: "needles", needles: [] })).resumePending();
    assert.equal(client.uploadCalls, 1);
    assert.equal(probe.mock.callCount(), 0);
  });
});
