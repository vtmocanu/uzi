import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, createHmac } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, type FetchAgentBranchOptions, type OwedCandidate, type PositiveOwedCandidateContext, type RecoveryCoverageClaim } from "../src/git.js";
import { RecoveryCoordinator, canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import type { CompletedPublicationReceipt, RecoveryArchiveRedundancyRequest, RecoveryArchiveRedundancyResponse } from "../src/protocol.js";

// Issue #2625 M2: the worker reads a redundancy claim from a real coverage aggregate (parity with
// fixtures/recovery-archive-redundancy/vectors.json, which the api verifies byte for byte) and
// submits it after the cleanup journal lock is released, without ever gating the cleanup.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const BRANCH = "agent/issue-2625";
const RUN = "26252625-1111-2222-3333-444444444444";

interface VecObject { sha: string; round: number; batch: number; parents: string[]; bodyBase64: string }
interface VecCase {
  name: string; roots: string[]; currentSha: string; tree: string; digest: string; sourceSha: string;
  aggregateObjects: VecObject[]; currentObject: VecObject;
  witnessObject?: { sha: string; bodyBase64: string; tree: string };
}
const vectors = new Map<string, VecCase>(
  (JSON.parse(fs.readFileSync(new URL("../../fixtures/recovery-archive-redundancy/vectors.json", import.meta.url), "utf8")) as
    { cases: VecCase[] }).cases.map(c => [c.name, c]));
const vector = (name: string): VecCase => vectors.get(name)!;
const decode = (b64: string): string => Buffer.from(b64, "base64").toString("utf8");

function gitIn(dir: string, args: string[], env: NodeJS.ProcessEnv = {}, input?: string): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", env: { ...GIT_ENV, ...env }, ...(input === undefined ? {} : { input }),
  }).trim();
}

// ---- real-git fixture ----

let fx: Fixture;
let cache: GitCache;
let bare: string;
let context: PositiveOwedCandidateContext;

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  const base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-2625", noProofReseed, RUN)).path;
  const opts: FetchAgentBranchOptions = { context: { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const sha = gitIn(clone, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", base, "-m", "source"]);
  gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
  assert.equal((await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
  context = opts.context as PositiveOwedCandidateContext;
});
afterEach(() => fx.cleanup());

/** A commit in the bare repo with the fixture identity; the tree holds one file named by `treeLabel`. */
function tree(label: string): string {
  const blob = gitIn(bare, ["hash-object", "-w", "--stdin"], {}, label + "\n");
  return gitIn(bare, ["mktree"], {}, `100644 blob ${blob}\tf\n`);
}
function commit(treeSha: string, parents: string[], message: string, date = 1578096000): string {
  const args = ["commit-tree", treeSha, "-m", message];
  for (const p of parents) args.push("-p", p);
  return gitIn(bare, args, {
    GIT_AUTHOR_NAME: "Fixture Author", GIT_AUTHOR_EMAIL: "fixture@example.invalid", GIT_AUTHOR_DATE: `${date} +0000`,
    GIT_COMMITTER_NAME: "Fixture Author", GIT_COMMITTER_EMAIL: "fixture@example.invalid", GIT_COMMITTER_DATE: `${date} +0000`,
  });
}
const b64Body = (sha: string): string =>
  execFileSync("git", ["-C", bare, "cat-file", "commit", sha], { env: GIT_ENV }).toString("base64");

/** The history the vectors were built from: c1 <- c2 <- c3 (linear), plus wip commits per case. */
function history() {
  const [t1, t2, t3] = [tree("t1"), tree("t2"), tree("t3")] as [string, string, string];
  const c1 = commit(t1, [], "c1", 1577836800);
  const c2 = commit(t2, [c1], "c2", 1577923200);
  const c3 = commit(t3, [c2], "c3");
  return { c1, c2, c3, t3 };
}

/** Rewrite every sha of `text` to its role in the vector, then sort `parent` lines like git's sorted heads. */
function normalized(body: string, roles: Map<string, string>): string {
  const mapped = body.replace(/\b[0-9a-f]{40}\b|\b[0-9a-f]{64}\b/g, h => roles.get(h) ?? h);
  const lines = mapped.split("\n");
  const first = lines.findIndex(l => l.startsWith("parent "));
  if (first < 0) return mapped;
  let last = first;
  while (lines[last + 1]?.startsWith("parent ")) last++;
  lines.splice(first, last - first + 1, ...lines.slice(first, last + 1).sort());
  return lines.join("\n");
}

async function coverageClaim(roots: string[], current: string, witnessHead?: string): Promise<{
  claim: RecoveryCoverageClaim | undefined; cov: Awaited<ReturnType<GitCache["buildRecoveryCoverage"]>>;
}> {
  const cov = await cache.buildRecoveryCoverage(bare, context, roots, current);
  const claim = await cache.readRecoveryCoverageClaim(bare, {
    sourceSha: cov.sha, fingerprint: cov.fingerprint, roots: cov.originalShas, currentSha: current,
    ...(witnessHead === undefined ? {} : { witnessHead }),
  });
  return { claim, cov };
}

describe("readRecoveryCoverageClaim parity with vectors.json (real git)", () => {
  it("small: aggregate and current bodies equal the vector modulo object ids", async () => {
    const v = vector("small");
    const { c1, c2, c3, t3 } = history();
    const { claim, cov } = await coverageClaim([c1, c2], c3);
    assert.ok(claim);
    const c2v = decode(v.currentObject.bodyBase64).match(/^parent (\w+)$/m)![1]!;
    const c1v = v.roots.find(r => r !== c2v)!;
    const roles = new Map([[c1, c1v], [c2, c2v], [c3, v.currentSha], [t3, v.tree], [cov.fingerprint, v.digest], [cov.sha, v.sourceSha]]);
    assert.equal(claim.tree, t3);
    assert.equal(claim.coverageDigest, cov.fingerprint);
    assert.deepEqual(claim.roots, [c1, c2].sort());
    assert.equal(claim.aggregateObjects.length, 1);
    assert.equal(normalized(decode(claim.aggregateObjects[0]!), roles), decode(v.aggregateObjects[0]!.bodyBase64));
    assert.equal(normalized(decode(claim.currentObject), roles), decode(v.currentObject.bodyBase64));
    assert.equal(claim.witnessObject, undefined);
    assert.equal(createHash("sha256").update(JSON.stringify({ roots: claim.roots, currentSha: c3, tree: t3 })).digest("hex"), claim.coverageDigest);
  });

  it("two_rounds: 32+8 round-0 batches then one round-1 commit, in vector order", async () => {
    const v = vector("two_rounds");
    const shared = tree("tr");
    const roots: string[] = [];
    for (let i = 0; i < v.roots.length; i++) roots.push(commit(shared, [], `r${i}`, 1577836800 + i));
    const current = commit(tree("tip"), [roots[0]!], "tip", 1579132800);
    const { claim } = await coverageClaim(roots, current);
    assert.ok(claim);
    assert.equal(claim.aggregateObjects.length, v.aggregateObjects.length);
    const shape = (b64: string) => {
      const text = decode(b64);
      return [text.match(/round (\d+) batch (\d+)\n$/)!.slice(1).join("/"), (text.match(/^parent /gm) ?? []).length];
    };
    assert.deepEqual(claim.aggregateObjects.map(shape), v.aggregateObjects.map(o => [`${o.round}/${o.batch}`, o.parents.length]));
    assert.equal(normalizedMessage(claim.currentObject), normalizedMessage(v.currentObject.bodyBase64));
  });

  for (const name of ["wip", "wip_unrooted_parent"] as const) {
    it(`${name}: aggregate and current bodies equal the vector modulo object ids`, async () => {
      const v = vector(name);
      const { c1, c2, c3 } = history();
      const wip = name === "wip";
      const wipTree = tree("wip-" + name);
      const date = wip ? 1579219200 : 1579392000;
      const message = wip ? "wip(park): work in progress" : "wip(park): second work in progress";
      const current = commit(wipTree, [wip ? c3 : c2], message, date);
      const roots = wip ? [c1, c3] : [c1];
      const { claim, cov } = await coverageClaim(roots, current);
      assert.ok(claim);
      const parent = decode(v.currentObject.bodyBase64).match(/^parent (\w+)$/m)![1]!;
      const c1v = v.roots.find(r => r !== parent)!;
      const roles = new Map<string, string>([[c1, wip ? c1v : v.roots[0]!], [current, v.currentSha], [wipTree, v.tree],
        [cov.fingerprint, v.digest], [cov.sha, v.sourceSha]]);
      if (wip) { roles.set(c3, parent); roles.set(c2, decode(v.witnessObject!.bodyBase64).match(/^parent (\w+)$/m)![1]!); }
      else roles.set(c2, parent);
      assert.equal(normalized(decode(claim.aggregateObjects[0]!), roles), decode(v.aggregateObjects[0]!.bodyBase64));
      assert.equal(normalized(decode(claim.currentObject), roles), decode(v.currentObject.bodyBase64));
    });
  }
});

function normalizedMessage(b64: string): string { return decode(b64).split("\n\n")[1]!; }

describe("readRecoveryCoverageClaim", () => {
  it("is read-only: no refs and no objects are written", async () => {
    const { c1, c2, c3 } = history();
    const cov = await cache.buildRecoveryCoverage(bare, context, [c1, c2], c3);
    const snapshot = () => [gitIn(bare, ["for-each-ref"]), gitIn(bare, ["cat-file", "--batch-all-objects", "--batch-check"])].join("\n--\n");
    const before = snapshot();
    const claim = await cache.readRecoveryCoverageClaim(bare, { sourceSha: cov.sha, fingerprint: cov.fingerprint, roots: cov.originalShas, currentSha: c3 });
    assert.ok(claim);
    assert.equal(snapshot(), before);
  });

  it("carries the raw git bytes of the aggregate and the current commit", async () => {
    const { c1, c2, c3 } = history();
    const cov = await cache.buildRecoveryCoverage(bare, context, [c1, c2], c3);
    const claim = await cache.readRecoveryCoverageClaim(bare, { sourceSha: cov.sha, fingerprint: cov.fingerprint, roots: cov.originalShas, currentSha: c3 });
    assert.equal(claim!.aggregateObjects[0], b64Body(cov.sha));
    assert.equal(claim!.currentObject, b64Body(c3));
  });

  it("no claim when the fingerprint, roots, source or current do not agree", async () => {
    const { c1, c2, c3 } = history();
    const cov = await cache.buildRecoveryCoverage(bare, context, [c1, c2], c3);
    const ok = { sourceSha: cov.sha, fingerprint: cov.fingerprint, roots: cov.originalShas, currentSha: c3 };
    const read = (o: Partial<typeof ok>) => cache.readRecoveryCoverageClaim(bare, { ...ok, ...o });
    assert.ok(await read({}));
    assert.equal(await read({ fingerprint: "e".repeat(64) }), undefined);
    assert.equal(await read({ roots: [c1] }), undefined);
    assert.equal(await read({ roots: [...ok.roots].reverse() }), undefined, "unsorted roots");
    assert.equal(await read({ sourceSha: c3 }), undefined, "source that is not an aggregate");
    assert.equal(await read({ sourceSha: "9".repeat(40) }), undefined);
    assert.equal(await read({ currentSha: c2 }), undefined);
  });

  it("no claim above the 64-head cap", async () => {
    const shared = tree("cap");
    const roots = Array.from({ length: 65 }, (_, i) => commit(shared, [], `cap${i}`, 1577836800 + i)).sort();
    const current = commit(tree("capcur"), [roots[0]!], "cur");
    assert.equal(await cache.readRecoveryCoverageClaim(bare, { sourceSha: current, fingerprint: "a".repeat(64), roots, currentSha: current }), undefined);
    const sixtyThree = roots.slice(0, 63);
    const { claim } = await coverageClaim(sixtyThree, current);
    assert.ok(claim, "63 roots + current is exactly 64 heads");
  });

  describe("witness", () => {
    it("is attached only when the witness has the tree and covers every parent of the current commit", async () => {
      const { c1, c3 } = history();
      const wipTree = tree("w");
      const wip = commit(wipTree, [c3], "wip(park): w", 1579219200);
      const witness = commit(wipTree, [c3], "published rewrite", 1579305600);
      const { claim } = await coverageClaim([c1, c3], wip, witness);
      assert.ok(claim);
      assert.equal(claim.witnessObject, b64Body(witness));
    });

    it("is omitted when the current commit is already an ancestor of the witness head", async () => {
      const { c1, c2, c3 } = history();
      const later = commit(tree("later"), [c3], "later", 1579305600);
      const { claim } = await coverageClaim([c1, c2], c3, later);
      assert.ok(claim);
      assert.equal(claim.witnessObject, undefined);
      const { claim: same } = await coverageClaim([c1, c2], c3, c3);
      assert.equal(same!.witnessObject, undefined);
    });

    it("makes the whole claim vanish when the tree differs, a parent is uncovered or the head is unknown", async () => {
      const { c1, c2, c3 } = history();
      const wipTree = tree("w2");
      const wip = commit(wipTree, [c3], "wip(park): w2", 1579219200);
      const wrongTree = commit(tree("other"), [c3], "wrong tree", 1579305600);
      const uncovered = commit(wipTree, [c2], "does not contain the parent", 1579305600);
      assert.equal((await coverageClaim([c1, c3], wip, wrongTree)).claim, undefined);
      assert.equal((await coverageClaim([c1, c3], wip, uncovered)).claim, undefined);
      assert.equal((await coverageClaim([c1, c3], wip, "7".repeat(40))).claim, undefined);
    });
  });
});

// ---- coordinator: build under the lock, submit after it, never gate the cleanup ----

const H = "a".repeat(40);
const EMPTY_DIGEST = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
const FEATURE = "recovery_archive_redundancy_v1";
const TOKEN = "local-worker-fixture";
const FINAL_HEAD = "f".repeat(40);

const receipt = (generation: number): CompletedPublicationReceipt => ({
  hold_id: "11111111-1111-4111-8111-111111111111", run_id: "22222222-2222-4222-8222-222222222222",
  owner_id: "33333333-3333-4333-8333-333333333333", worker_id: "44444444-4444-4444-8444-444444444444", generation,
  final_head: FINAL_HEAD, repo_id: "55555555-5555-4555-8555-555555555555",
  connection_id: "66666666-6666-4666-8666-666666666666", project_id: 7, forge_type: "github",
  base_url: "https://example.invalid", branch: "task", mr_iid: 3, observed_branch_head: FINAL_HEAD,
});

async function harness(opts: { features?: string[]; redundancyDeadlineMs?: number } = {}) {
  const root = await fsp.mkdtemp(path.join(os.tmpdir(), "redundancy-"));
  await fsp.chmod(root, 0o700);
  const runId = "22222222-2222-4222-8222-222222222222";
  const barePath = path.join(root, "repo.git");
  const ctx: PositiveOwedCandidateContext = { runId, generation: 7, kind: "issue", branch: "task", barePath,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: "d".repeat(40) } };
  const state = {
    features: new Set(["recovery_inventory_v1", ...(opts.features ?? [FEATURE])]),
    releaseError: undefined as Error | undefined,
    cleanup: "removed" as "removed" | "retained",
    cleanupCalls: 0,
    claimResult: "ok" as "ok" | "none" | "throw" | "hang",
    claimReads: [] as Array<Record<string, unknown>>,
    // when set, the claim read takes this long (a slow repo read)
    claimDelayMs: undefined as number | undefined,
    // behaviour of the redundancy RPC
    prove: async (): Promise<RecoveryArchiveRedundancyResponse> => ({ capture_id: "", outcome: "retained", reason: "not_ancestor" }),
    // when set, called synchronously from the client method before any await (a client
    // that aborts the caller in its synchronous part)
    syncProve: undefined as ((signal?: AbortSignal) => Promise<RecoveryArchiveRedundancyResponse>) | undefined,
    proveCalls: [] as Array<{ runId: string; captureId: string; req: RecoveryArchiveRedundancyRequest; signal?: AbortSignal;
      lockFree: boolean; cleanupCallsAtSend: number }>,
  };
  const captures = new Map<string, { id: string; request: { source_sha: string; coverage_digest: string }; manifest?: { checksum: string; byte_size: number } }>();
  const client = {
    hasFeature: (f: string) => state.features.has(f),
    getRunOwnership: async () => ({ status: "completed", claim_generation: 7, inventory_guarded: true }),
    listRecoveryHolds: async () => ({ run_id: runId, holds: [{ hold_id: "h7", generation: 7, inventory_guarded: true, has_available_capture: false }] }),
    reserveRecoveryCapture: async (_run: string, request: { idempotency_key: string; source_sha: string; coverage_digest: string }) => {
      let c = captures.get(request.idempotency_key);
      if (!c) { c = { id: "server-" + (captures.size + 1), request }; captures.set(request.idempotency_key, c); }
      return { capture_id: c.id, state: "preparing" };
    },
    getRecoveryCaptureStatus: async (_run: string, id: string) => {
      const c = [...captures.values()].find(x => x.id === id)!;
      return { capture_id: id, state: c.manifest ? "available" : "preparing", manifest_bound: !!c.manifest,
        checksum: c.manifest?.checksum, byte_size: c.manifest?.byte_size, expires_at: "2099-01-01T00:00:00Z" };
    },
    uploadRecoveryBundle: async (_run: string, id: string, m: { checksum: string; byte_size: number }, stream: AsyncIterable<unknown>) => {
      for await (const chunk of stream) assert.ok(chunk);
      [...captures.values()].find(x => x.id === id)!.manifest = structuredClone(m);
      return { capture_id: id, state: "available", manifest_bound: true };
    },
    releaseRecoveryCustody: async (_run: string, generation: number) => {
      if (state.releaseError) throw state.releaseError;
      return { run_id: runId, generation, released: true, holds_released: 1 };
    },
    proveRecoveryArchiveRedundancy: (run: string, captureId: string, req: RecoveryArchiveRedundancyRequest, signal?: AbortSignal): Promise<RecoveryArchiveRedundancyResponse> => {
      if (state.syncProve) {
        state.proveCalls.push({ runId: run, captureId, req, ...(signal ? { signal } : {}), lockFree: true, cleanupCallsAtSend: state.cleanupCalls });
        return state.syncProve(signal);
      }
      return (async () => {
        state.proveCalls.push({ runId: run, captureId, req, ...(signal ? { signal } : {}), lockFree: await journalLockFree(), cleanupCallsAtSend: state.cleanupCalls });
        return { ...(await state.prove()), capture_id: captureId };
      })();
    },
  };
  const cloneClaim: RecoveryCoverageClaim = {
    coverageDigest: "", roots: [H], currentSha: H, tree: "1".repeat(40),
    aggregateObjects: [Buffer.from("aggregate body").toString("base64")], currentObject: Buffer.from("current body").toString("base64"),
  };
  const git = {
    recoverySettlementRoot: path.join(root, "settlement"),
    deleteRecoveryPin: async () => undefined,
    readInventoryCloneHeads: async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] }),
    ancestry: async () => "ancestor",
    enumerateOwedCandidates: async () => [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(ctx)] }] as OwedCandidate[],
    discoverOwedCandidates: async () => [{ context: ctx, candidates: [{ sha: H, pinRef: "refs/owed/a", contexts: [structuredClone(ctx)] }] }],
    resolveRecoveryBareDir: async () => barePath,
    buildRecoveryCoverage: async (_b: string, _c: unknown, roots: string[], currentSha: string) => {
      const fingerprint = createHash("sha256").update(JSON.stringify({ roots: [...roots].sort(), currentSha })).digest("hex");
      return { sha: fingerprint.slice(0, 40), fingerprint };
    },
    fetchDefaultTip: async () => { throw new Error("guarded capture must be credential free"); },
    produceRecoveryBundle: async (_b: string, o: { outPath: string; sourceSha: string }) => {
      const pack = Buffer.concat([Buffer.from("PACK"), Buffer.from([0, 0, 0, 2, 0, 0, 0, 0])]);
      const bytes = Buffer.concat([Buffer.from(`# v2 git bundle\n${o.sourceSha} refs/heads/recovered-source\n\n`), pack, createHash("sha1").update(pack).digest()]);
      await fsp.writeFile(o.outPath, bytes);
      return { bundlePath: o.outPath, byteSize: bytes.length, checksum: createHash("sha256").update(bytes).digest("hex"),
        chunkCount: 1, prerequisiteShas: [], selfContained: true, alreadyPublished: false };
    },
  };
  const wiring = {
    cleanupRecoveryGeneration: async (...args: unknown[]) => {
      state.cleanupCalls++;
      return args[7] === "completion-check" ? "verified" : state.cleanup;
    },
    readRecoveryCoverageClaim: async (_b: string, o: Record<string, unknown>) => {
      state.claimReads.push(o);
      if (state.claimDelayMs) await new Promise(r => setTimeout(r, state.claimDelayMs));
      if (state.claimResult === "throw") throw new Error("read failed");
      if (state.claimResult === "hang") return new Promise<RecoveryCoverageClaim | undefined>(() => undefined);
      return state.claimResult === "none" ? undefined : { ...cloneClaim, coverageDigest: o.fingerprint as string };
    },
  };
  const make = () => new RecoveryCoordinator({
    recoveryRoot: path.join(root, "journal"), workerToken: TOKEN, log: nullLogger(), client: client as never, git: git as never,
    withCompletionSourceBoundary: async (_s, action) => { await action(() => true); return "passed"; },
    ...(opts.redundancyDeadlineMs === undefined ? {} : { testHooks: { redundancyDeadlineMs: opts.redundancyDeadlineMs } }),
  });
  const coordinator = make();
  /** The cleanup journal lock is free iff an unrelated call that takes it finishes at once. */
  async function journalLockFree(): Promise<boolean> {
    let timer: NodeJS.Timeout | undefined;
    const probe = coordinator.persistCompletionReceipt({ ...receipt(99), run_id: runId });
    const outcome = await Promise.race([probe.then(() => true), new Promise<boolean>(r => { timer = setTimeout(() => r(false), 750); })]);
    clearTimeout(timer);
    return outcome;
  }
  const journalDir = path.join(root, "journal", runId);
  const sign = async (record: RecoveryRecord): Promise<void> => {
    const key = createHmac("sha256", TOKEN).update("uzi-recovery-journal-v1").digest();
    const mac = createHmac("sha256", key).update(canonicalJson(record)).digest("hex");
    await fsp.writeFile(path.join(journalDir, record.captureId + ".json"), JSON.stringify({ ...record, mac }));
  };
  /** A pinned source record plus an uploaded (final ACK pending or acknowledged) coverage record. */
  async function uploaded(acknowledge: boolean) {
    await coordinator.pin({ runId, generation: 7, kind: "issue", branch: "task", sourceSha: H, inventoryGuarded: true });
    const frozen = await coordinator.freezeInventory({ context: ctx, currentSha: H, originalSourceSha: H, defaultBranch: "main" });
    assert.ok(frozen);
    if (!acknowledge) state.releaseError = new Error("FINAL not accepted yet");
    await coordinator.captureAndUpload({ record: frozen, barePath, defaultBranch: "main" });
    state.releaseError = undefined;
    const records = await coordinator.inspect(runId);
    const archive = records.find(r => r.coverageDigest)!;
    const source = records.find(r => !r.coverageDigest)!;
    assert.equal(archive.state, "uploaded");
    assert.ok(archive.serverCaptureId);
    assert.equal(!!archive.finalAcknowledged, acknowledge);
    Object.assign(git, wiring);
    return { archive, source };
  }
  /** Put the completion source on every record and the receipt on the NON-uploaded sibling. */
  async function receipted(rec: { archive: RecoveryRecord; source: RecoveryRecord }) {
    assert.equal(await coordinator.bindCompletionSource({ context: ctx, clonePath: path.join(root, "clone"), expectedHead: H, roots: [{ sha: H, contexts: [ctx] }] }), true);
    const records = await coordinator.inspect(runId);
    const sibling = records.find(r => r.captureId === rec.source.captureId)!;
    await sign({ ...sibling, completionReceipt: receipt(7) });
    return (await coordinator.inspect(runId)).find(r => r.captureId === rec.source.captureId)!;
  }
  return { root, ctx, runId, state, git, coordinator, make, uploaded, receipted, sign, close: () => fsp.rm(root, { recursive: true, force: true }) };
}

describe("RecoveryCoordinator redundancy claims", () => {
  it("receipt path: claim goes out after the journal lock is released, receipt on the sibling record still claims the uploaded record", async () => {
    const h = await harness();
    try {
      const rec = await h.uploaded(false);
      const withReceipt = await h.receipted(rec);
      await h.coordinator.resumePending(undefined, [withReceipt]);
      assert.equal(h.state.cleanupCalls, 2, "completion-check preflight and destructive cleanup both ran");
      assert.deepEqual(await h.coordinator.inspect(h.runId), [], "cleanup removed the generation's records");
      assert.equal(h.state.proveCalls.length, 1);
      const call = h.state.proveCalls[0]!;
      assert.equal(call.lockFree, true, "no redundancy RPC under the journal lock");
      assert.equal(call.cleanupCallsAtSend, 2, "the claim is sent after cleanup finished");
      assert.equal(call.captureId, rec.archive.serverCaptureId);
      assert.equal(call.runId, h.runId);
      assert.equal(call.req.generation, 7);
      assert.equal(call.req.coverage_digest, rec.archive.coverageDigest);
      assert.deepEqual(call.req.roots, [H]);
      assert.equal(call.req.current_sha, H);
      assert.equal(call.req.tree, "1".repeat(40));
      assert.deepEqual(call.req.aggregate_objects, [Buffer.from("aggregate body").toString("base64")]);
      assert.equal(call.req.current_object, Buffer.from("current body").toString("base64"));
      assert.deepEqual(h.state.claimReads.map(r => [r.sourceSha, r.fingerprint, r.roots, r.currentSha, r.witnessHead]),
        [[rec.archive.sourceSha, rec.archive.coverageDigest, [H], H, FINAL_HEAD]], "the delivered head is the witness head");
    } finally { await h.close(); }
  });

  it("ACK path: forgetGeneration sends the claim without a witness, after the lock", async () => {
    const h = await harness();
    try {
      const rec = await h.uploaded(true);
      await h.coordinator.forgetGeneration(h.runId, 7);
      assert.deepEqual(await h.coordinator.inspect(h.runId), []);
      assert.equal(h.state.proveCalls.length, 1);
      const call = h.state.proveCalls[0]!;
      assert.equal(call.lockFree, true);
      assert.equal(call.cleanupCallsAtSend, 1);
      assert.equal(call.captureId, rec.archive.serverCaptureId);
      assert.equal(call.req.wip, undefined);
      assert.equal(h.state.claimReads[0]!.witnessHead, undefined);
    } finally { await h.close(); }
  });

  it("a witness from the reader becomes the wip object", async () => {
    const h = await harness();
    try {
      await h.uploaded(true);
      const read = (h.git as unknown as { readRecoveryCoverageClaim: (b: string, o: Record<string, unknown>) => Promise<RecoveryCoverageClaim | undefined> }).readRecoveryCoverageClaim;
      (h.git as unknown as { readRecoveryCoverageClaim: unknown }).readRecoveryCoverageClaim = async (b: string, o: Record<string, unknown>) =>
        ({ ...(await read(b, o))!, witnessObject: "d2l0bmVzcw==" });
      await h.coordinator.forgetGeneration(h.runId, 7);
      assert.deepEqual(h.state.proveCalls[0]!.req.wip, { witness_object: "d2l0bmVzcw==" });
    } finally { await h.close(); }
  });

  describe("no claim", () => {
    it("without the protocol feature", async () => {
      const h = await harness({ features: [] });
      try {
        await h.uploaded(true);
        await h.coordinator.forgetGeneration(h.runId, 7);
        assert.equal(h.state.proveCalls.length, 0);
        assert.equal(h.state.claimReads.length, 0);
        assert.deepEqual(await h.coordinator.inspect(h.runId), [], "cleanup is unchanged");
      } finally { await h.close(); }
    });

    it("for a non-uploaded record (ACK path)", async () => {
      const h = await harness();
      try {
        const rec = await h.uploaded(true);
        await h.sign({ ...rec.archive, state: "bundled" });
        await h.coordinator.forgetGeneration(h.runId, 7);
        assert.equal(h.state.proveCalls.length, 0);
      } finally { await h.close(); }
    });

    // The journal only admits these shapes with a coherent final request, so they run through the
    // receipt path, which does not need an acknowledged archive.
    for (const [name, edit] of [
      ["empty inventory digest", (r: RecoveryRecord): RecoveryRecord => { const { finalRequest: _f, ...rest } = r; return { ...rest, coverageDigest: EMPTY_DIGEST }; }],
      ["settled final disposition", (r: RecoveryRecord): RecoveryRecord => ({ ...r, coverageDigest: EMPTY_DIGEST, originalRoots: [],
        finalRequest: { disposition: { kind: "settled", coverage_digest: EMPTY_DIGEST }, evidence: "publication" } })],
      ["missing server capture id", (r: RecoveryRecord): RecoveryRecord => { const { serverCaptureId: _s, finalRequest: _f, ...rest } = r; return rest as RecoveryRecord; }],
    ] as const) {
      it(`for a ${name} (receipt path)`, async () => {
        const h = await harness();
        try {
          const rec = await h.uploaded(false);
          await h.sign(edit(rec.archive));
          const withReceipt = await h.receipted(rec);
          await h.coordinator.resumePending(undefined, [withReceipt]);
          assert.equal(h.state.cleanupCalls, 2, "the receipt cleanup itself still ran");
          assert.equal(h.state.proveCalls.length, 0);
        } finally { await h.close(); }
      });
    }

    it("when the reader has no claim, throws, or the git/client lacks the optional methods", async () => {
      for (const mode of ["none", "throw"] as const) {
        const h = await harness();
        try {
          await h.uploaded(true);
          h.state.claimResult = mode;
          await h.coordinator.forgetGeneration(h.runId, 7);
          assert.equal(h.state.proveCalls.length, 0);
          assert.deepEqual(await h.coordinator.inspect(h.runId), [], `cleanup unchanged when the reader gives ${mode}`);
        } finally { await h.close(); }
      }
      const h = await harness();
      try {
        await h.uploaded(true);
        delete (h.git as { readRecoveryCoverageClaim?: unknown }).readRecoveryCoverageClaim;
        await h.coordinator.forgetGeneration(h.runId, 7);
        assert.equal(h.state.proveCalls.length, 0);
        assert.deepEqual(await h.coordinator.inspect(h.runId), []);
      } finally { await h.close(); }
    });
  });

  describe("never gates the cleanup", () => {
    for (const [name, prove] of [
      ["retained", async () => ({ capture_id: "", outcome: "retained" as const, reason: "not_ancestor" })],
      ["error", async (): Promise<RecoveryArchiveRedundancyResponse> => { throw new Error("redundancy endpoint down"); }],
    ] as const) {
      it(`${name} answer leaves the receipt cleanup and the ACK cleanup unchanged`, async () => {
        for (const path of ["receipt", "ack"] as const) {
          const h = await harness();
          try {
            const rec = await h.uploaded(path === "ack");
            h.state.prove = prove;
            if (path === "ack") await h.coordinator.forgetGeneration(h.runId, 7);
            else await h.coordinator.resumePending(undefined, [await h.receipted(rec)]);
            assert.equal(h.state.proveCalls.length, 1);
            assert.deepEqual(await h.coordinator.inspect(h.runId), []);
          } finally { await h.close(); }
        }
      });
    }

    it("a hanging request returns control at the deadline and cleanup is already complete", async () => {
      const h = await harness({ redundancyDeadlineMs: 80 });
      try {
        await h.uploaded(true);
        h.state.prove = () => new Promise<RecoveryArchiveRedundancyResponse>(() => undefined);
        const started = Date.now();
        await h.coordinator.forgetGeneration(h.runId, 7);
        const elapsed = Date.now() - started;
        assert.equal(h.state.proveCalls.length, 1);
        assert.ok(elapsed >= 70, `returned only after the deadline, took ${elapsed}ms`);
        assert.ok(elapsed < 5_000, `returned at the deadline, not after a hang, took ${elapsed}ms`);
        assert.equal(h.state.proveCalls[0]!.signal?.aborted, true, "the request was given the deadline signal and aborted");
        assert.deepEqual(await h.coordinator.inspect(h.runId), []);
      } finally { await h.close(); }
    });

    it("the caller's abort ends the wait as well", async () => {
      const h = await harness();
      const abort = new AbortController();
      const timers: NodeJS.Timeout[] = [];
      const guard = <T extends string>(ms: number, value: T) =>
        new Promise<T>(resolve => { const t = setTimeout(() => resolve(value), ms); t.unref(); timers.push(t); });
      try {
        const rec = await h.uploaded(false);
        // #2782: a claim read slower than any fixed abort timer; abort only once the claim is out
        h.state.claimDelayMs = 120;
        let claimStarted!: () => void;
        const started = new Promise<"started">(resolve => { claimStarted = () => resolve("started"); });
        h.state.prove = () => { claimStarted(); return new Promise<RecoveryArchiveRedundancyResponse>(() => undefined); };
        const withReceipt = await h.receipted(rec);
        const running = h.coordinator.resumePending(abort.signal, [withReceipt]);
        assert.equal(await Promise.race([started, guard(10_000, "no-claim")]), "started", "the redundancy claim was never sent");
        abort.abort();
        const abortedAt = Date.now();
        const outcome = await Promise.race([running.then(() => "returned" as const), guard(3_000, "hung")]);
        assert.equal(outcome, "returned", `the caller's abort did not end the wait (${Date.now() - abortedAt}ms)`);
        assert.equal(h.state.proveCalls.length, 1);
        assert.equal(h.state.proveCalls[0]!.signal?.aborted, true, "the claim request was given the caller's abort");
      } finally {
        for (const t of timers) clearTimeout(t);
        abort.abort();
        await h.close();
      }
    });
    it("a claim read that hangs under the journal lock is cut off by the same deadline and cleanup completes", async () => {
      const h = await harness({ redundancyDeadlineMs: 60 });
      try {
        await h.uploaded(true);
        h.state.claimResult = "hang";
        const guard = new Promise<"hung">(resolve => setTimeout(() => resolve("hung"), 3_000).unref());
        const started = Date.now();
        const outcome = await Promise.race([h.coordinator.forgetGeneration(h.runId, 7).then(() => "returned" as const), guard]);
        assert.equal(outcome, "returned", `cleanup stalled behind a hung claim read (${Date.now() - started}ms)`);
        assert.equal(h.state.claimReads.length, 1, "the read was attempted");
        assert.equal(h.state.proveCalls.length, 0, "no claim is sent when its read did not finish");
        assert.deepEqual(await h.coordinator.inspect(h.runId), [], "cleanup is unchanged");
      } finally { await h.close(); }
    });

    it("a client that aborts the caller synchronously and never settles still returns by the deadline", async () => {
      const h = await harness({ redundancyDeadlineMs: 40 });
      try {
        const rec = await h.uploaded(false);
        const abort = new AbortController();
        h.state.syncProve = () => { abort.abort(); return new Promise<RecoveryArchiveRedundancyResponse>(() => undefined); };
        const withReceipt = await h.receipted(rec);
        const started = Date.now();
        const guard = new Promise<"hung">(resolve => setTimeout(() => resolve("hung"), 3_000).unref());
        const outcome = await Promise.race([h.coordinator.resumePending(abort.signal, [withReceipt]).then(() => "returned" as const), guard]);
        assert.equal(outcome, "returned", `submission hung after a synchronous caller abort (${Date.now() - started}ms)`);
        assert.equal(h.state.proveCalls.length, 1);
        assert.equal(h.state.proveCalls[0]!.signal?.aborted, true);
      } finally { await h.close(); }
    });
  });

  describe("memo", () => {
    async function twice(answer: RecoveryArchiveRedundancyResponse | Error) {
      const h = await harness();
      try {
        await h.uploaded(true);
        h.state.cleanup = "retained"; // the journal stays, so a later pass can claim again
        h.state.prove = async () => { if (answer instanceof Error) throw answer; return answer; };
        await h.coordinator.forgetGeneration(h.runId, 7);
        await h.coordinator.forgetGeneration(h.runId, 7);
        return h.state.proveCalls.length;
      } finally { await h.close(); }
    }
    it("an expired answer suppresses repeats", async () => {
      assert.equal(await twice({ capture_id: "", outcome: "expired" }), 1);
    });
    it("a non-transient retained reason suppresses repeats", async () => {
      assert.equal(await twice({ capture_id: "", outcome: "retained", reason: "not_ancestor" }), 1);
    });
    for (const reason of ["ancestry_unknown", "forge_timeout", "cooling_down"]) {
      it(`${reason} is transient and is claimed again`, async () => {
        assert.equal(await twice({ capture_id: "", outcome: "retained", reason }), 2);
      });
    }
    it("an error is transient and is claimed again", async () => {
      assert.equal(await twice(new Error("boom")), 2);
    });
  });
});
