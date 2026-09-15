import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { nullLogger } from "./helpers.js";
import { GitCache, type RecoveryBundleResult } from "../src/git.js";
import { RUN_KINDS, type RunKind } from "../src/protocol.js";
import {
  CODE_PUBLISHING_KINDS,
  isCodePublishingKind,
  RecoveryCoordinator,
  type RecoveryArchiveClient,
  type RecoveryBundleProducer,
} from "../src/recovery.js";
import type {
  RecoveryCaptureStatusResponse,
  RecoveryHold,
  RecoveryHoldsResponse,
  RecoveryReleaseResponse,
  RecoveryReserveRequest,
  RecoveryReserveResponse,
  RecoveryUploadManifest,
} from "../src/protocol.js";

// PRD #1296 M3 (D1/D3/D5/D6/D9) — the WORKER recovery coordinator: the unconditional
// authenticated pin, the model-free produce→journal→upload capture, restart-safe
// byte-identical re-upload with NO forge PAT, tampered-journal refusal, and the six
// code-publishing profiles reaching the capture path. Uses fakes for the client and (where
// bytes don't matter) the producer, plus the REAL GitCache producer for the byte-identical
// restart proof. No DB, no network.

const TOKEN = "worker-join-token-abcdef0123456789";
const H = "1111111111111111111111111111111111111111";
const H_PRIME = "2222222222222222222222222222222222222222";
const BASE = "4444444444444444444444444444444444444444";

// ── fakes ───────────────────────────────────────────────────────────────────────

interface ReserveCall {
  runId: string;
  req: RecoveryReserveRequest;
}
interface UploadCall {
  runId: string;
  captureId: string;
  manifest: RecoveryUploadManifest;
  bytes: Buffer;
}

class FakeClient implements RecoveryArchiveClient {
  reserveCalls: ReserveCall[] = [];
  uploadCalls: UploadCall[] = [];
  releaseCalls: string[] = [];
  /** PRD #1349 M2: the EXACT generation each release named (parallel to releaseCalls). */
  releaseGenerations: Array<number | undefined> = [];
  /** PRD #1392 M1/M2: the evidence class each release stamped (parallel to releaseCalls). */
  releaseEvidence: Array<string | undefined> = [];
  listCalls: string[] = [];
  /** PRD #1349 M2: the holds listRecoveryHolds returns (the post-clone inventory). */
  holds: RecoveryHold[] = [];
  serverCaptureId = "server-capture-uuid-1";
  uploadShouldThrow = false;
  /** PRD #1349 M2: when set, the server RETAINED the hold instead of releasing it. */
  releaseRetained = false;

  async reserveRecoveryCapture(runId: string, req: RecoveryReserveRequest): Promise<RecoveryReserveResponse> {
    this.reserveCalls.push({ runId, req });
    return { capture_id: this.serverCaptureId, state: "preparing" };
  }
  async getRecoveryCaptureStatus(_runId: string, captureId: string): Promise<RecoveryCaptureStatusResponse> {
    return { capture_id: captureId, state: "preparing", manifest_bound: false };
  }
  async uploadRecoveryBundle(
    runId: string,
    captureId: string,
    manifest: RecoveryUploadManifest,
    bundle: Readable,
  ): Promise<RecoveryCaptureStatusResponse> {
    const chunks: Buffer[] = [];
    for await (const c of bundle) chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c as string));
    this.uploadCalls.push({ runId, captureId, manifest, bytes: Buffer.concat(chunks) });
    if (this.uploadShouldThrow) throw new Error("upload rejected");
    return { capture_id: captureId, state: "available", manifest_bound: true };
  }
  async releaseRecoveryCustody(runId: string, generation?: number, releaseEvidence?: string): Promise<RecoveryReleaseResponse> {
    this.releaseCalls.push(runId);
    this.releaseGenerations.push(generation);
    this.releaseEvidence.push(releaseEvidence);
    if (this.releaseRetained) {
      return { run_id: runId, released: false, holds_released: 0, retained: true, reason: "ambiguous" };
    }
    return { run_id: runId, released: true, holds_released: 1 };
  }
  async listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse> {
    this.listCalls.push(runId);
    return { run_id: runId, holds: this.holds };
  }
}

/** A producer fake that writes deterministic bytes to outPath and returns a matching
 *  manifest, or throws / signals already-published per the flags. */
class FakeGit implements RecoveryBundleProducer {
  fetchCalls = 0;
  produceCalls: Array<{ barePath: string; opts: { sourceSha: string; outPath: string; forgeTip?: string } }> = [];
  forgeTip = "3333333333333333333333333333333333333333";
  bytes = Buffer.from("deterministic-fake-bundle-bytes-\x00\u00ff");
  throwOnProduce: Error | undefined;
  alreadyPublished = false;

  async fetchDefaultTip(): Promise<string> {
    this.fetchCalls++;
    return this.forgeTip;
  }
  async produceRecoveryBundle(
    barePath: string,
    opts: { sourceSha: string; outPath: string; forgeTip?: string; maxBytes?: number },
  ): Promise<RecoveryBundleResult> {
    this.produceCalls.push({ barePath, opts });
    if (this.throwOnProduce) throw this.throwOnProduce;
    if (this.alreadyPublished) {
      return {
        bundlePath: "",
        byteSize: 0,
        checksum: "",
        chunkCount: 0,
        prerequisiteShas: [],
        sourceSha: opts.sourceSha,
        selfContained: false,
        alreadyPublished: true,
      };
    }
    await fsp.writeFile(opts.outPath, this.bytes);
    return {
      bundlePath: opts.outPath,
      byteSize: this.bytes.length,
      checksum: createHash("sha256").update(this.bytes).digest("hex"),
      chunkCount: 1,
      prerequisiteShas: [opts.forgeTip ?? "base"],
      sourceSha: opts.sourceSha,
      selfContained: false,
      alreadyPublished: false,
    };
  }
}

/** A producer that MUST NOT be called on the restart path (no PAT, no re-derivation). */
class PoisonGit implements RecoveryBundleProducer {
  fetchCalls = 0;
  produceCalls = 0;
  async fetchDefaultTip(): Promise<string> {
    this.fetchCalls++;
    throw new Error("fetchDefaultTip must not be called on the restart re-upload path");
  }
  async produceRecoveryBundle(): Promise<RecoveryBundleResult> {
    this.produceCalls++;
    throw new Error("produceRecoveryBundle must not be called on the restart re-upload path");
  }
}

let root: string;

function makeCoordinator(opts: {
  client?: RecoveryArchiveClient;
  git?: RecoveryBundleProducer;
  token?: string | undefined;
  recoveryRoot?: string;
} = {}): RecoveryCoordinator {
  return new RecoveryCoordinator({
    client: opts.client ?? new FakeClient(),
    git: opts.git ?? new FakeGit(),
    log: nullLogger(),
    recoveryRoot: opts.recoveryRoot ?? root,
    workerToken: "token" in opts ? opts.token : TOKEN,
    now: () => 1_700_000_000_000,
  });
}

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recovery-coord-"));
});

afterEach(() => {
  fs.rmSync(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
});

describe("RecoveryCoordinator — disabled without a worker token", () => {
  it("is a no-op everywhere (a token-less harness is unaffected)", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoordinator({ client, git, token: undefined });
    assert.equal(coord.enabled, false);
    assert.equal(await coord.pin({ runId: "r1", sourceSha: H, kind: "issue", branch: "b" }), undefined);
    const outcome = await coord.captureAndUpload({
      record: {
        version: 1,
        runId: "r1",
        captureId: "c1",
        sourceSha: H,
        kind: "issue",
        branch: "b",
        createdAt: 0,
        state: "pinned",
      },
      barePath: "/bare",
      defaultBranch: "main",
    });
    assert.equal(outcome.state, "pinned");
    await coord.resumePending();
    assert.deepEqual(await coord.inspect("r1"), []);
    assert.equal(client.reserveCalls.length, 0);
    assert.equal(git.fetchCalls, 0);
  });
});

describe("RecoveryCoordinator — authenticated pin & journal (D1)", () => {
  it("pins the ORIGINAL H, MAC round-trips, and a restarted coordinator reads it back", async () => {
    const coord = makeCoordinator();
    const rec = await coord.pin({
      runId: "run-A",
      sourceSha: H,
      kind: "issue",
      branch: "agent/x",
      attemptedHeadSha: H_PRIME,
    });
    assert.ok(rec);
    assert.equal(rec.sourceSha, H);
    assert.equal(rec.state, "pinned");
    // On disk: a `mac` field is present (the record is authenticated).
    const runDir = path.join(root, "run-A");
    const file = fs.readdirSync(runDir).find((n) => n.endsWith(".json"))!;
    const onDisk = JSON.parse(fs.readFileSync(path.join(runDir, file), "utf8"));
    assert.match(onDisk.mac, /^[0-9a-f]{64}$/);
    assert.equal(onDisk.sourceSha, H);
    // A fresh coordinator (restart) with the SAME token re-derives the key and accepts it.
    const restarted = makeCoordinator();
    const seen = await restarted.inspect("run-A");
    assert.equal(seen.length, 1);
    assert.equal(seen[0]!.sourceSha, H);
    assert.equal(seen[0]!.attemptedHeadSha, H_PRIME);
  });

  it("is idempotent for the same (runId, sourceSha): a re-pin reuses the record", async () => {
    const coord = makeCoordinator();
    const a = await coord.pin({ runId: "run-B", sourceSha: H, kind: "task", branch: "b" });
    const b = await coord.pin({ runId: "run-B", sourceSha: H, kind: "task", branch: "b" });
    assert.ok(a && b);
    assert.equal(a.captureId, b.captureId);
    assert.equal((await coord.inspect("run-B")).length, 1);
  });

  it("a record written under a DIFFERENT worker token is refused (wrong-key MAC)", async () => {
    const coord = makeCoordinator({ token: TOKEN });
    await coord.pin({ runId: "run-C", sourceSha: H, kind: "issue", branch: "b" });
    // A coordinator with a different join token cannot authenticate the record.
    const other = makeCoordinator({ token: "a-completely-different-join-token-9999" });
    assert.deepEqual(await other.inspect("run-C"), []);
  });
});

describe("RecoveryCoordinator — tampered journal is refused, never a substituted head (D1)", () => {
  it("drops a record whose sourceSha was edited, and never uploads the substituted head", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoordinator({ client, git });
    // Pin then produce+journal a bundle (state -> bundled) via a normal (successful) capture
    // path but stop before upload by making upload throw, so a `bundled`/`needs_action`
    // record with a real journaled bundle file is left on disk for the restart sweep.
    client.uploadShouldThrow = true;
    const rec = await coord.pin({ runId: "run-T", sourceSha: H, kind: "issue", branch: "b" });
    await coord.captureAndUpload({
      record: rec!,
      barePath: "/bare",
      defaultBranch: "main",
    });
    // Tamper: substitute an attacker-chosen head, WITHOUT recomputing the MAC.
    const runDir = path.join(root, "run-T");
    const file = fs.readdirSync(runDir).find((n) => n.endsWith(".json"))!;
    const fp = path.join(runDir, file);
    const obj = JSON.parse(fs.readFileSync(fp, "utf8"));
    obj.sourceSha = "9999999999999999999999999999999999999999";
    fs.writeFileSync(fp, JSON.stringify(obj));

    // A restarted coordinator refuses the tampered record: it is NOT returned as authority.
    const restarted = makeCoordinator({ client, git });
    assert.deepEqual(await restarted.inspect("run-T"), [], "tampered record is refused");
    const before = client.uploadCalls.length;
    await restarted.resumePending();
    // The substituted head was never uploaded (the tampered record is not acted upon).
    assert.equal(client.uploadCalls.length, before, "no upload of a tampered/substituted head");
    for (const c of client.uploadCalls) {
      assert.notEqual(c.manifest.checksum, undefined);
    }
    // Control: an UNTAMPERED record is accepted and re-uploaded on restart.
    client.uploadShouldThrow = false;
    const rec2 = await coord.pin({ runId: "run-OK", sourceSha: H, kind: "issue", branch: "b" });
    client.uploadShouldThrow = true;
    await coord.captureAndUpload({ record: rec2!, barePath: "/bare", defaultBranch: "main" });
    client.uploadShouldThrow = false;
    const restarted2 = makeCoordinator({ client, git });
    await restarted2.resumePending();
    assert.ok(
      client.uploadCalls.some((c) => c.runId === "run-OK"),
      "an authentic record IS re-uploaded on restart",
    );
  });
});

describe("RecoveryCoordinator — capture forwards original H + fresh forge tip (D5)", () => {
  it("passes the pinned original H (never H') and the fetchDefaultTip result to the producer", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({
      runId: "run-D",
      sourceSha: H,
      kind: "issue",
      branch: "b",
      attemptedHeadSha: H_PRIME,
    });
    const outcome = await coord.captureAndUpload({
      record: rec!,
      barePath: "/bare",
      defaultBranch: "main",
      forgePat: "unused-fake-pat",
      attemptedHeadSha: H_PRIME,
    });
    assert.equal(outcome.state, "uploaded");
    assert.equal(git.fetchCalls, 1, "a fresh forge tip is resolved once");
    assert.equal(git.produceCalls.length, 1);
    assert.equal(git.produceCalls[0]!.opts.sourceSha, H, "producer gets ORIGINAL H, never H'");
    assert.notEqual(git.produceCalls[0]!.opts.sourceSha, H_PRIME);
    assert.equal(git.produceCalls[0]!.opts.forgeTip, git.forgeTip);
    // Reserve idempotency key is the durable capture id; source_sha is H.
    assert.equal(client.reserveCalls.length, 1);
    assert.equal(client.reserveCalls[0]!.req.idempotency_key, rec!.captureId);
    assert.equal(client.reserveCalls[0]!.req.source_sha, H);
    assert.equal(client.reserveCalls[0]!.req.attempted_head_sha, H_PRIME);
    // The uploaded bytes + manifest are the produced ones, and the local file is freed.
    assert.equal(client.uploadCalls.length, 1);
    assert.ok(client.uploadCalls[0]!.bytes.equals(git.bytes));
    assert.equal(
      client.uploadCalls[0]!.manifest.checksum,
      createHash("sha256").update(git.bytes).digest("hex"),
    );
    assert.equal((await coord.inspect("run-D"))[0]!.state, "uploaded");
    // The uploaded record's local bundle is removed; a restart sweep skips it.
    await coord.resumePending();
    assert.equal(client.uploadCalls.length, 1, "an uploaded record is not re-uploaded");
  });
});

describe("RecoveryCoordinator — producer failure retains custody (D3)", () => {
  it("marks needs_action and never reserves/uploads when the bundle cannot be produced", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    git.throwOnProduce = new Error("object graph broken");
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({ runId: "run-E", sourceSha: H, kind: "issue", branch: "b" });
    const outcome = await coord.captureAndUpload({ record: rec!, barePath: "/bare", defaultBranch: "main" });
    assert.equal(outcome.state, "needs_action");
    assert.equal(outcome.reason, "bundle_failed");
    assert.equal(client.reserveCalls.length, 0);
    assert.equal(client.uploadCalls.length, 0);
    const seen = await coord.inspect("run-E");
    assert.equal(seen[0]!.state, "needs_action");
    assert.equal(seen[0]!.sourceSha, H, "the original source is retained");
  });
});

describe("RecoveryCoordinator — already-published releases custody (D3)", () => {
  it("releases custody and archives nothing when H is already on the forge", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    git.alreadyPublished = true;
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({ runId: "run-F", sourceSha: H, kind: "issue", branch: "b" });
    const outcome = await coord.captureAndUpload({ record: rec!, barePath: "/bare", defaultBranch: "main" });
    assert.equal(outcome.state, "uploaded");
    assert.equal(outcome.reason, "already_published");
    assert.deepEqual(client.releaseCalls, ["run-F"]);
    // PRD #1392 M1/M2 (fact 9): a proven fresh-forge no-output release stamps forge_no_output.
    assert.deepEqual(client.releaseEvidence, ["forge_no_output"]);
    assert.equal(client.uploadCalls.length, 0);
  });
});

describe("RecoveryCoordinator — release on successful publication (D3)", () => {
  it("calls release and removes the run journal (best-effort, never throws)", async () => {
    const client = new FakeClient();
    const coord = makeCoordinator({ client });
    await coord.pin({ runId: "run-G", sourceSha: H, kind: "issue", branch: "b" });
    await coord.release("run-G");
    assert.deepEqual(client.releaseCalls, ["run-G"]);
    assert.deepEqual(await coord.inspect("run-G"), [], "the run journal dir is removed");
    // A release failure does not throw to the caller (completion is unaffected).
    const failing = new FakeClient();
    failing.releaseRecoveryCustody = async () => {
      throw new Error("release rejected");
    };
    const coord2 = makeCoordinator({ client: failing });
    await coord2.pin({ runId: "run-H", sourceSha: H, kind: "issue", branch: "b" });
    await coord2.release("run-H"); // must not throw
  });
});

describe("RecoveryCoordinator — all six code-publishing profiles reach the capture path (D9)", () => {
  it("gates exactly the six code-publishing kinds (chat/judge excluded)", () => {
    const expected: RunKind[] = ["issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework"];
    for (const k of expected) assert.equal(isCodePublishingKind(k), true, `${k} publishes code`);
    assert.equal(isCodePublishingKind("chat"), false);
    assert.equal(isCodePublishingKind("judge"), false);
    // Cross-check the set against the wire enum so a new kind can't silently slip the gate.
    const publishing = RUN_KINDS.filter((k) => isCodePublishingKind(k)).sort();
    assert.deepEqual(publishing, [...expected].sort());
    assert.equal(CODE_PUBLISHING_KINDS.size, 6);
  });

  it("pins and captures for each of the six kinds", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoordinator({ client, git });
    const kinds: RunKind[] = ["issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework"];
    for (const kind of kinds) {
      const runId = `run-${kind}`;
      const rec = await coord.pin({ runId, sourceSha: H, kind, branch: `agent/${kind}` });
      assert.ok(rec, `${kind} pins`);
      const outcome = await coord.captureAndUpload({ record: rec, barePath: "/bare", defaultBranch: "main" });
      assert.equal(outcome.state, "uploaded", `${kind} reaches upload`);
    }
    assert.equal(client.reserveCalls.length, 6);
    assert.equal(client.uploadCalls.length, 6);
  });
});

describe("RecoveryCoordinator — byte-identical restart re-upload with NO forge PAT (D5, real producer)", () => {
  // Isolate git from host/global config, mirroring the producer test.
  const GIT_ENV = {
    ...process.env,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0",
  };
  const rawGit = (cwd: string, args: string[]): string =>
    execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();

  it("re-uploads the exact journaled bytes after restart, never re-fetching or re-deriving", async () => {
    // Build a small local topology: a forge bare with `main` at B, and a trusted worker
    // bare carrying a run branch H = B + one commit, with `origin` pointing at the forge so
    // the FIRST capture can resolve a fresh forge tip locally (no real PAT needed).
    const gitBase = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recovery-real-"));
    const work = path.join(gitBase, "work");
    const forge = path.join(gitBase, "forge.git");
    const worker = path.join(gitBase, "worker.git");
    fs.mkdirSync(work, { recursive: true });
    execFileSync("git", ["init", "-b", "main", work], { env: GIT_ENV, stdio: "pipe" });
    rawGit(work, ["config", "user.email", "f@uzi.local"]);
    rawGit(work, ["config", "user.name", "f"]);
    rawGit(work, ["config", "commit.gpgsign", "false"]);
    rawGit(work, ["config", "maintenance.auto", "false"]);
    rawGit(work, ["config", "gc.auto", "0"]);
    for (const bare of [forge, worker]) {
      execFileSync("git", ["init", "--bare", "-b", "main", bare], { env: GIT_ENV, stdio: "pipe" });
      rawGit(bare, ["config", "maintenance.auto", "false"]);
      rawGit(bare, ["config", "gc.auto", "0"]);
    }
    fs.writeFileSync(path.join(work, "README.md"), "# fixture\n");
    rawGit(work, ["add", "."]);
    rawGit(work, ["commit", "-m", "base"]);
    rawGit(work, ["remote", "add", "forge", forge]);
    rawGit(work, ["remote", "add", "worker", worker]);
    rawGit(work, ["push", "forge", "main"]);
    rawGit(work, ["push", "worker", "main"]);
    rawGit(work, ["checkout", "-b", "agent/run-1"]);
    fs.writeFileSync(path.join(work, "impl.txt"), "committed work\n");
    rawGit(work, ["add", "."]);
    rawGit(work, ["commit", "-m", "feat"]);
    const head = rawGit(work, ["rev-parse", "HEAD"]);
    rawGit(work, ["push", "worker", "agent/run-1:agent/run-1"]);
    // The worker bare's own `origin` is the forge (as the runner clone's would resolve to).
    rawGit(worker, ["remote", "add", "origin", forge]);

    const realGit = new GitCache(path.join(gitBase, "data"), nullLogger());

    // FIRST capture attempt: real producer builds+journals a REAL bundle, then the upload
    // fails, leaving a journaled bundle on disk (source retained).
    const client1 = new FakeClient();
    client1.uploadShouldThrow = true;
    const coord1 = makeCoordinator({ client: client1, git: realGit });
    const rec = await coord1.pin({ runId: "run-real", sourceSha: head, kind: "issue", branch: "agent/run-1" });
    const first = await coord1.captureAndUpload({
      record: rec!,
      barePath: worker,
      defaultBranch: "main",
      forgePat: "fixture-pat-unused-for-local-fetch",
    });
    assert.equal(first.state, "needs_action", "the failed upload retains the source as needs_action");

    // Capture the journaled bundle bytes for the byte-identical comparison.
    const journaled = await coord1.inspect("run-real");
    const bundlePath = journaled[0]!.bundlePath!;
    assert.ok(bundlePath && fs.existsSync(bundlePath), "the verified bundle file is journaled");
    const journaledBytes = fs.readFileSync(bundlePath);
    assert.ok(journaledBytes.length > 0);

    // RESTART: a new coordinator with a PoisonGit (any fetch/produce = test failure) and a
    // fresh client. resumePending must re-upload from the journaled file alone — no PAT,
    // no forge fetch, no re-derivation.
    const poison = new PoisonGit();
    const client2 = new FakeClient();
    const coord2 = makeCoordinator({ client: client2, git: poison });
    await coord2.resumePending();

    assert.equal(poison.fetchCalls, 0, "no forge tip fetch on restart");
    assert.equal(poison.produceCalls, 0, "no bundle re-derivation on restart");
    assert.equal(client2.uploadCalls.length, 1, "the journaled bundle is re-uploaded once");
    const uploaded = client2.uploadCalls[0]!;
    assert.ok(uploaded.bytes.equals(journaledBytes), "re-uploaded bytes are BYTE-IDENTICAL");
    // The manifest checksum is the journaled one (never swapped under a bound capture id).
    assert.equal(uploaded.manifest.checksum, createHash("sha256").update(journaledBytes).digest("hex"));
    // The reserve idempotency key is the SAME durable capture id, so a lost ACK re-reserves
    // the same server capture rather than duplicating it.
    assert.equal(client2.reserveCalls.length, 1);
    assert.equal(client2.reserveCalls[0]!.req.idempotency_key, rec!.captureId);
    // After a successful re-upload the record is uploaded and the local bundle is freed.
    const after = await coord2.inspect("run-real");
    assert.equal(after[0]!.state, "uploaded");
    assert.equal(fs.existsSync(bundlePath), false);

    fs.rmSync(gitBase, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });
});

// PRD #1349 M2 — exact-generation custody: the release/reserve/already-published paths carry
// the exact claim generation, one journal record per generation (the early evidence pin and the
// finalization pin collapse), a server-RETAINED release keeps the source, the post-clone
// inventory only observes, and a cancelled capture retains rather than releasing.

describe("RecoveryCoordinator — exact generation identity end to end (PRD #1349 M2, D1/D2)", () => {
  it("pin is idempotent by (runId, generation): the finalization pin UPDATES the early pin's source", async () => {
    const coord = makeCoordinator();
    // The early generation-evidence pin records the restore point the run starts from (BASE).
    const early = await coord.pin({ runId: "run-gen", sourceSha: BASE, kind: "issue", branch: "b", generation: 5 });
    // The finalization pin at the SAME generation advances the source to the committed head H.
    const fin = await coord.pin({ runId: "run-gen", sourceSha: H, kind: "issue", branch: "b", generation: 5 });
    assert.ok(early && fin);
    assert.equal(early.captureId, fin.captureId, "same record — no duplicate for the generation");
    const recs = await coord.inspect("run-gen");
    assert.equal(recs.length, 1, "exactly one record per (runId, generation)");
    assert.equal(recs[0]!.sourceSha, H, "the pinned source advanced base → committed head");
    assert.equal(recs[0]!.generation, 5);
  });

  it("a non-pinned record is NOT re-pointed by a later pin (its source is bound to journaled bytes)", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    // A failed upload leaves a needs_action record whose journaled bundle file is bound to H.
    client.uploadShouldThrow = true;
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({ runId: "run-bnd", sourceSha: H, kind: "issue", branch: "b", generation: 3 });
    await coord.captureAndUpload({ record: rec!, barePath: "/bare", defaultBranch: "main" });
    assert.equal((await coord.inspect("run-bnd"))[0]!.state, "needs_action");
    // A later same-generation pin with a DIFFERENT source must not move a record past `pinned`.
    await coord.pin({ runId: "run-bnd", sourceSha: H_PRIME, kind: "issue", branch: "b", generation: 3 });
    const recs = await coord.inspect("run-bnd");
    assert.equal(recs.length, 1);
    assert.equal(recs[0]!.sourceSha, H, "the record keeps its journaled source, never H'");
    assert.equal(recs[0]!.state, "needs_action");
  });

  it("release names the exact generation and drops the journal only on a real release", async () => {
    const client = new FakeClient();
    const coord = makeCoordinator({ client });
    await coord.pin({ runId: "run-rel", sourceSha: H, kind: "issue", branch: "b", generation: 9 });
    await coord.release("run-rel", 9);
    assert.deepEqual(client.releaseCalls, ["run-rel"]);
    assert.deepEqual(client.releaseGenerations, [9], "the EXACT generation is released");
    assert.deepEqual(await coord.inspect("run-rel"), [], "a real release drops the run journal");
  });

  it("release(generation) removes ONLY that generation's record+bundle; a sibling generation SURVIVES (no whole-dir wipe)", async () => {
    // PRD #1349 M2 (D1) regression: on a same-worker affinity resume, generation N's retained
    // needs_action record + its on-disk bundle (possibly the last local copy of N's unpublished
    // committed work) coexist with generation N+1's record in the SAME run dir. A clean release of
    // N+1 must NOT wipe the whole run dir — that would take N's work with it.
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoordinator({ client, git });
    const runId = "run-sibling";
    // Gen N: drive to a RETAINED needs_action WITH an on-disk bundle (the upload fails, so a
    // verified bundle file is journaled and the source is retained).
    client.uploadShouldThrow = true;
    const genN = await coord.pin({ runId, sourceSha: H, kind: "issue", branch: "b", generation: 10 });
    await coord.captureAndUpload({ record: genN!, barePath: "/bare", defaultBranch: "main" });
    const nRec = (await coord.inspect(runId)).find((r) => r.generation === 10)!;
    assert.equal(nRec.state, "needs_action", "gen N is retained (needs_action)");
    const nBundle = nRec.bundlePath!;
    assert.ok(nBundle && fs.existsSync(nBundle), "gen N's verified bundle is on disk");
    // Gen N+1: a fresh record for the next generation, in the SAME run dir.
    client.uploadShouldThrow = false;
    await coord.pin({ runId, sourceSha: H_PRIME, kind: "issue", branch: "b", generation: 11 });
    assert.equal((await coord.inspect(runId)).length, 2, "gen 10 + gen 11 coexist in one run dir");
    // A clean release of gen N+1 (server released:true / retained:false).
    await coord.release(runId, 11);
    assert.deepEqual(client.releaseGenerations, [11], "released the exact generation N+1");
    const remaining = await coord.inspect(runId);
    assert.equal(remaining.length, 1, "ONLY gen 11's record was removed");
    assert.equal(remaining[0]!.generation, 10, "gen 10's record SURVIVES");
    assert.equal(remaining[0]!.state, "needs_action");
    assert.ok(
      fs.existsSync(nBundle),
      "gen 10's bundle SURVIVES — releasing a sibling generation never wipes the run dir",
    );
  });

  it("a server-RETAINED release keeps the local journal (the source stays protected)", async () => {
    const client = new FakeClient();
    client.releaseRetained = true;
    const coord = makeCoordinator({ client });
    await coord.pin({ runId: "run-ret", sourceSha: H, kind: "issue", branch: "b", generation: 2 });
    await coord.release("run-ret", 2);
    assert.deepEqual(client.releaseGenerations, [2]);
    assert.equal(
      (await coord.inspect("run-ret")).length,
      1,
      "a RETAINED hold keeps the local journal for owner attention rather than deleting it",
    );
  });

  it("already-published releases the record's EXACT generation and archives nothing", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    git.alreadyPublished = true;
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({ runId: "run-ap", sourceSha: H, kind: "issue", branch: "b", generation: 4 });
    const outcome = await coord.captureAndUpload({ record: rec!, barePath: "/bare", defaultBranch: "main" });
    assert.equal(outcome.reason, "already_published");
    assert.deepEqual(client.releaseGenerations, [4], "released the record's exact generation");
    assert.equal(client.uploadCalls.length, 0);
  });

  it("the reserve carries the exact generation so it binds the right hold", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({ runId: "run-res", sourceSha: H, kind: "issue", branch: "b", generation: 11 });
    await coord.captureAndUpload({ record: rec!, barePath: "/bare", defaultBranch: "main" });
    assert.equal(client.reserveCalls.length, 1);
    assert.equal(client.reserveCalls[0]!.req.generation, 11, "reserve binds the exact generation");
  });

  it("inventoryHolds observes the server's holds and NEVER releases; disabled ⇒ []", async () => {
    const client = new FakeClient();
    client.holds = [
      { hold_id: "h1", generation: 3, has_available_capture: false },
      { hold_id: "h2", generation: 4, has_available_capture: true, capture_state: "available" },
    ];
    const coord = makeCoordinator({ client });
    const holds = await coord.inventoryHolds("run-inv");
    assert.equal(holds.length, 2);
    assert.deepEqual(holds.map((h) => h.generation), [3, 4]);
    assert.deepEqual(client.listCalls, ["run-inv"]);
    assert.equal(client.releaseCalls.length, 0, "the inventory NEVER releases a hold");
    // A transport failure is best-effort: [] and no throw.
    client.listRecoveryHolds = async () => {
      throw new Error("network down");
    };
    assert.deepEqual(await coord.inventoryHolds("run-inv"), []);
    // A token-less coordinator is a no-op inventory.
    const disabled = makeCoordinator({ token: undefined });
    assert.deepEqual(await disabled.inventoryHolds("run-inv"), []);
  });

  it("cancel-during-capture retains: an aborted upload marks needs_action and NEVER releases", async () => {
    const client = new FakeClient();
    const git = new FakeGit();
    // Model a cancellation arriving during the upload: the client throws when the signal fired.
    client.uploadRecoveryBundle = (async (
      _runId: string,
      captureId: string,
      manifest: RecoveryUploadManifest,
      bundle: Readable,
      signal?: AbortSignal,
    ) => {
      for await (const _ of bundle) {
        /* drain */
      }
      void captureId;
      void manifest;
      if (signal?.aborted) throw new Error("aborted mid-capture");
      return { capture_id: captureId, state: "available", manifest_bound: true };
    }) as typeof client.uploadRecoveryBundle;
    const coord = makeCoordinator({ client, git });
    const rec = await coord.pin({ runId: "run-cancel", sourceSha: H, kind: "issue", branch: "b", generation: 6 });
    const ac = new AbortController();
    ac.abort();
    const outcome = await coord.captureAndUpload({
      record: rec!,
      barePath: "/bare",
      defaultBranch: "main",
      signal: ac.signal,
    });
    assert.equal(outcome.state, "needs_action", "a cancelled capture retains (needs_action)");
    assert.equal(client.releaseCalls.length, 0, "a cancelled capture NEVER releases custody");
    assert.equal((await coord.inspect("run-cancel"))[0]!.state, "needs_action", "the source is retained");
  });
});
