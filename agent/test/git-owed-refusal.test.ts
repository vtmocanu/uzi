import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext, type RefusalReleaseGate,
  type TrackingUpdateResult } from "../src/git.js";

// issue #4481: refused heads at the owed cap are recorded, kept alive, gated, sealed and cleaned up.
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-4481";
const RUN = "44814481-aaaa-bbbb-cccc-444444444444";
const CAP = 6;
const OTHER_NONCE = "other-process-nonce-0001";
let fx: Fixture;
let cache: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;
let ctx: PositiveOwedCandidateContext;
let seq = 0;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}
function commitTree(label: string, moveBranch: boolean): string {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const sha = gitIn(clone, [...IDENT, "commit-tree", tree, "-p", base, "-m", `${label} ${seq++}`]);
  if (moveBranch) gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
  return sha;
}
/** A commit whose parents are exactly `parents`, present in the bare, standing for a confirmed FINAL. */
function coverOf(parents: string[]): string {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const sha = gitIn(clone, [...IDENT, "commit-tree", tree, ...parents.flatMap((p) => ["-p", p]), "-m", `cover ${seq++}`]);
  gitIn(clone, ["update-ref", `refs/heads/cover-${seq}`, sha]);
  gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, `+refs/heads/cover-${seq}:refs/uzi-test/cover-${seq}`]);
  return sha;
}
function toBare(sha: string): void {
  gitIn(clone, ["update-ref", `refs/heads/stash-${seq}`, sha]);
  gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, `+refs/heads/stash-${seq}:refs/uzi-test/stash-${seq++}`]);
}
const fetchHead = (o: FetchAgentBranchOptions = opts): Promise<TrackingUpdateResult> => cache.fetchAgentBranch(bare, clone, BRANCH, RUN, o);
const refDir = (): string => path.join(bare, "info", "uzi-refusals");
const refusalFiles = (): string[] => fs.existsSync(refDir()) ? fs.readdirSync(refDir()).sort() : [];
const readJson = (name: string): any => JSON.parse(fs.readFileSync(path.join(refDir(), name), "utf8"));
const writeRaw = (name: string, v: unknown): void => {
  fs.writeFileSync(path.join(refDir(), name), typeof v === "string" ? v : JSON.stringify(v) + "\n", { mode: 0o600 });
};
const ledgerName = `refusals-${RUN}.json`;
const incomingRefs = (): string[] => gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/"]).split("\n").filter(Boolean).sort();
const pinRefs = (): string[] => gitIn(bare, ["for-each-ref", "--format=%(refname)", `refs/uzi-owed/${RUN}/`]).split("\n").filter(Boolean).sort();
const gate = (head: string, g = 1, c: GitCache = cache): Promise<RefusalReleaseGate> => c.refusalReleaseGate(bare, RUN, g, head);
const evidence = (): string => JSON.stringify({ files: refusalFiles().map((n) => [n, fs.readFileSync(path.join(refDir(), n), "utf8")]), refs: incomingRefs() });
const filesSnapshot = (): string => JSON.stringify(refusalFiles().map((n) => [n, fs.readFileSync(path.join(refDir(), n), "utf8")]));
const withContext = (generation: number): FetchAgentBranchOptions => ({ ...opts, context: { ...opts.context, generation } as FetchAgentBranchOptions["context"] });

async function rewriteTimes(n: number): Promise<string[]> {
  const heads: string[] = [];
  for (let i = 0; i < n; i++) {
    heads.push(commitTree("rewrite", true));
    const result = await fetchHead();
    assert.equal(result.kind, "updated", `rewrite ${i + 1}: ${JSON.stringify(result)}`);
  }
  return heads;
}
/** Drive the owed pin count to the cap; returns the retained heads (the last is the tracking head). */
const atCap = (): Promise<string[]> => rewriteTimes(CAP);
async function refuse(label = "refused"): Promise<string> {
  const sha = commitTree(label, true);
  assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit", refusedSha: sha });
  return sha;
}
/** At the cap with one admitted refusal covered by a FINAL: the baseline every gate test mutates. */
async function eligibleState(): Promise<{ kept: string[]; refused: string; cover: string }> {
  const kept = await atCap();
  assert.equal(await cache.admitRefusalProvenance(ctx), "written");
  const refused = await refuse();
  return { kept, refused, cover: coverOf([...kept, refused]) };
}
/** Free the tracking head's pin and fill its slot with a placeholder, so pinning the head hits the cap. */
function unpinTrackingHead(kept: string[]): void {
  gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${kept[kept.length - 1]}`]);
  gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${"b".repeat(40)}`, base]);
}
const roots = async () => (await cache.enumerateOwedCandidates(bare, RUN)).map((c) => ({ sha: c.sha, contexts: c.contexts }));
const cleanup = async (covering: string, canDelete?: () => boolean) =>
  cache.cleanupRecoveryGeneration(bare, ctx, await roots(), [], [], covering, canDelete, "archive");

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP }));
  bare = await cache.ensureClone(fx.originPath);
  base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-4481", noProofReseed, RUN)).path;
  ctx = { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } };
  opts = { context: ctx };
});
afterEach(() => {
  if (fs.existsSync(refDir())) fs.chmodSync(refDir(), 0o700);
  fx.cleanup();
});

describe("issue4481 refusal ledger", { timeout: 300_000 }, () => {
  it("keeps the refused head, reads the ledger back, dedupes and releases the duplicate stage", async () => {
    await atCap();
    const refused = await refuse();
    const ledger = readJson(ledgerName);
    assert.deepEqual(ledger, { version: 1, runId: RUN, branch: BRANCH, barePath: bare,
      entries: [{ generation: 1, sha: refused, path: "P2" }] });
    const first = incomingRefs();
    assert.equal(first.length, 2, "an attributed stage plus the refused ref");
    assert.ok(first.includes(`refs/uzi-incoming/${RUN}/1/refused-${refused} ${refused}`));
    assert.ok(first.some((r) => new RegExp(`^refs/uzi-incoming/${RUN}/1/[0-9a-f-]{36} ${refused}$`).test(r)));
    // The same head refused again: still one ledger entry, and the duplicate stage is deleted.
    assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit", refusedSha: refused });
    assert.deepEqual(readJson(ledgerName).entries, ledger.entries);
    assert.deepEqual(incomingRefs(), first, "no second stage survives for an already-ledgered head");
    // A different head adds an entry and keeps its own stage.
    const other = await refuse("other");
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => e.sha), [refused, other]);
    assert.equal(incomingRefs().length, 4);
  });

  it("the gate says none below the cap, and ineligible at the cap without admissions", async () => {
    await rewriteTimes(2);
    assert.deepEqual(await gate(base), { kind: "none" });
    const kept = await rewriteTimes(CAP - 2);
    assert.deepEqual(await gate(base), { kind: "ineligible", reason: "admission for generation 1 absent" });
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const cover = coverOf(kept);
    const eligible = await gate(cover);
    assert.equal(eligible.kind, "eligible", JSON.stringify(eligible));
  });

  it("an admitted, covered refusal is eligible and the digest is stable", async () => {
    const { cover } = await eligibleState();
    const a = await gate(cover);
    assert.equal(a.kind, "eligible", JSON.stringify(a));
    assert.deepEqual(await gate(cover), a);
  });

  it("a divergent earlier refused head makes the gate ineligible", async () => {
    const kept = await atCap();
    await cache.admitRefusalProvenance(ctx);
    const first = await refuse("first");
    const second = await refuse("second");
    const onlySecond = coverOf([...kept, second]);
    const verdict = await gate(onlySecond);
    assert.equal(verdict.kind, "ineligible");
    assert.match((verdict as { reason: string }).reason, new RegExp(first));
    assert.equal((await gate(coverOf([...kept, first, second]))).kind, "eligible");
  });

  it("an unledgered attributed stage makes the gate ineligible", async () => {
    const { cover, kept } = await eligibleState();
    const stray = commitTree("stray", false);
    toBare(stray);
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/1/stray`, stray]);
    const verdict = await gate(coverOf([...kept, stray, cover]));
    assert.deepEqual(verdict, { kind: "ineligible", reason: "refusal stage is not in the ledger" });
  });

  it("a stage of a later generation, malformed, unbound or over-long ledgers and tmp residue are ineligible", async () => {
    const { cover, refused } = await eligibleState();
    const good = fs.readFileSync(path.join(refDir(), ledgerName), "utf8");
    const expectIneligible = async (label: string): Promise<void> => {
      assert.equal((await gate(cover)).kind, "ineligible", label);
      writeRaw(ledgerName, good);
      assert.equal((await gate(cover)).kind, "eligible", `${label}: restored`);
    };
    const parsed = JSON.parse(good);
    writeRaw(ledgerName, "{not json");
    await expectIneligible("garbage");
    writeRaw(ledgerName, { ...parsed, extra: 1 });
    await expectIneligible("extra key");
    writeRaw(ledgerName, { ...parsed, runId: "44814481-aaaa-bbbb-cccc-555555555555" });
    await expectIneligible("other run");
    writeRaw(ledgerName, { ...parsed, barePath: "/elsewhere" });
    await expectIneligible("other bare");
    writeRaw(ledgerName, { ...parsed, entries: [{ generation: 1, sha: refused.toUpperCase(), path: "P2" }] });
    await expectIneligible("uppercase sha");
    writeRaw(ledgerName, { ...parsed, entries: [{ generation: 1, sha: refused, path: "P9" }] });
    await expectIneligible("unknown path label");
    writeRaw(ledgerName, { ...parsed, entries: [...parsed.entries, ...parsed.entries] });
    await expectIneligible("duplicate sha");
    writeRaw(ledgerName, { ...parsed, entries: Array.from({ length: 65 }, (_, i) => ({ generation: 1, sha: String(i).padStart(40, "c"), path: "P2" })) });
    await expectIneligible("over-length");
    writeRaw(".tmp-00000000-0000-0000-0000-000000000000", "x");
    assert.equal((await gate(cover)).kind, "ineligible", "tmp residue");
    fs.rmSync(path.join(refDir(), ".tmp-00000000-0000-0000-0000-000000000000"));
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/2/refused-${refused}`, refused]);
    assert.deepEqual(await gate(cover), { kind: "ineligible", reason: "refusal stage of a later generation" });
  });

  it("admission gaps and other-process admissions are ineligible; an admission is write-once", async () => {
    const { cover } = await eligibleState();
    assert.equal((await gate(cover, 2)).kind, "ineligible", "generation 2 was never admitted");
    const ctx2 = { ...ctx, generation: 2 };
    assert.equal(await cache.admitRefusalProvenance(ctx2), "written");
    assert.equal((await gate(cover, 2)).kind, "eligible");
    // Another process cannot be trusted, and it cannot overwrite either.
    const other = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP, processNonce: OTHER_NONCE }));
    const before = fs.readFileSync(path.join(refDir(), `admission-${RUN}-1.json`), "utf8");
    assert.equal(await other.admitRefusalProvenance(ctx), "existing");
    assert.equal(await cache.admitRefusalProvenance(ctx), "existing");
    assert.equal(fs.readFileSync(path.join(refDir(), `admission-${RUN}-1.json`), "utf8"), before);
    assert.equal(await other.admitRefusalProvenance({ ...ctx, generation: 3 }), "written");
    assert.equal(readJson(`admission-${RUN}-3.json`).process, OTHER_NONCE);
    assert.equal((await gate(cover, 3)).kind, "ineligible", "generation 3 was admitted by another process");
    assert.match((await gate(cover, 3) as { reason: string }).reason, /another process/);
    assert.equal((await gate(cover, 1, other)).kind, "ineligible", "the other process does not trust our admission");
    // Malformed admission.
    writeRaw(`admission-${RUN}-1.json`, { version: 1, runId: RUN, generation: 1, branch: BRANCH, barePath: bare, protocol: 2, process: "x".repeat(10) });
    assert.equal((await gate(cover)).kind, "ineligible");
  });

  it("a closed generation refuses fetch, updateTrackingRef and updateRemediationTrackingRef before any write", async () => {
    await atCap();
    const tip = gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]);
    const next = commitTree("after close", true);
    toBare(next);
    cache.closeRefusalGeneration(RUN, 1);
    const refs = incomingRefs(), pins = pinRefs(), files = refusalFiles();
    assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(await cache.updateTrackingRef(bare, BRANCH, next, opts), { kind: "not_updated", reason: "ownership_unknown" });
    const admission = { trusted: true, admit: async () => true };
    assert.deepEqual(await cache.updateRemediationTrackingRef(bare, BRANCH, next, opts,
      { runId: RUN, clonePath: clone, attemptId: "attempt-1" }, tip, admission), { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(incomingRefs(), refs);
    assert.deepEqual(pinRefs(), pins);
    assert.deepEqual(refusalFiles(), files);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), tip);
    // A later generation of the same run is not fenced.
    assert.notDeepEqual(await fetchHead(withContext(2)), { kind: "not_updated", reason: "ownership_unknown" });
    // retainCurrentOwedCandidate is sealed by the chokepoint, not fenced.
    assert.equal((await cache.retainCurrentOwedCandidate(bare, opts)).kind, "updated");
  });

  it("P1 (unobserved tracking) records, and honours the seal", async () => {
    const { kept, refused } = await eligibleState();
    const key = `uzi-trackowner.${BRANCH}.receiptversion`;
    gitIn(bare, ["config", "--local", key, "2"]);
    const p1 = commitTree("p1", true);
    assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit", refusedSha: p1 });
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => [e.sha, e.path]), [[refused, "P2"], [p1, "P1"]]);
    gitIn(bare, ["config", "--local", key, "1"]);
    const cover = coverOf([...kept, refused, p1]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    gitIn(bare, ["config", "--local", key, "2"]);
    const before = filesSnapshot();
    const later = commitTree("p1 after seal", true);
    assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit", refusedSha: later });
    assert.equal(filesSnapshot(), before, "the ledger and every record are untouched");
    assert.equal(incomingRefs().some((r) => r.endsWith(` ${later}`) && r.includes("/refused-")), false, "no refused ref after the seal");
    assert.equal(readJson(ledgerName).entries.some((e: any) => e.sha === later), false);
  });

  it("P2 (updateTrackingRef) records its refusal; after the seal nothing is recorded", async () => {
    const { kept, refused } = await eligibleState();
    const viaUpdate = commitTree("p2", false);
    toBare(viaUpdate);
    const result = await cache.updateTrackingRef(bare, BRANCH, viaUpdate, opts);
    assert.deepEqual(result, { kind: "not_updated", reason: "owed_limit" });
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => [e.sha, e.path]), [[refused, "P2"], [viaUpdate, "P2"]]);
    assert.ok(incomingRefs().includes(`refs/uzi-incoming/${RUN}/1/refused-${viaUpdate} ${viaUpdate}`));
    const cover = coverOf([...kept, refused, viaUpdate]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const before = evidence();
    const sealedTry = commitTree("p2 after seal", false);
    toBare(sealedTry);
    assert.deepEqual(await cache.updateTrackingRef(bare, BRANCH, sealedTry, opts), { kind: "not_updated", reason: "owed_limit" });
    assert.equal(evidence(), before);
  });

  /** Seven real pins from a cap-7 cache, then the tracking head's pin removed: a cap-6 cache sits at its cap
   *  with an unpinned tracking head, so displacing that head refuses at P3. */
  async function displacementRig(): Promise<{ kept: string[]; tracking: string }> {
    const big = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP + 1 }));
    const kept: string[] = [];
    for (let i = 0; i < CAP + 1; i++) {
      kept.push(commitTree("rig", true));
      assert.equal((await big.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
    }
    const tracking = kept[kept.length - 1]!;
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${tracking}`]);
    assert.equal(pinRefs().length, CAP);
    return { kept, tracking };
  }

  it("P3 (displaced head) is recorded under the displaced generation, writes no admission, and honours the seal", async () => {
    const { kept, tracking } = await displacementRig();
    // Generation 2 is the only admitted generation, so generation 1's admission is a gap.
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    const next = commitTree("displacer", true);
    const result = await fetchHead(withContext(2));
    assert.deepEqual(result, { kind: "not_updated", reason: "owed_limit", refusedSha: next });
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), tracking, "the promotion aborted; tracking is unchanged");
    const entries = readJson(ledgerName).entries;
    assert.deepEqual(entries.filter((e: any) => e.sha === tracking), [{ generation: 1, sha: tracking, path: "P3" }]);
    assert.deepEqual(entries.filter((e: any) => e.sha === next), [{ generation: 2, sha: next, path: "P2" }]);
    assert.ok(incomingRefs().includes(`refs/uzi-incoming/${RUN}/1/refused-${tracking} ${tracking}`));
    assert.deepEqual(refusalFiles().filter((n) => n.startsWith("admission-")), [`admission-${RUN}-2.json`],
      "no admission is written for the displaced generation");
    const cover = coverOf([...kept, next]);
    assert.deepEqual(await gate(cover, 2), { kind: "ineligible", reason: "admission for generation 1 absent" });
    // Admit generation 1 as the runner would have, cover everything, seal; then nothing more is recorded.
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 2, cover), "sealed");
    const before = filesSnapshot();
    const again = commitTree("displacer again", true);
    assert.deepEqual(await fetchHead(withContext(2)), { kind: "not_updated", reason: "owed_limit", refusedSha: again });
    assert.equal(filesSnapshot(), before);
    assert.equal(incomingRefs().some((r) => r.includes("/refused-") && r.endsWith(` ${again}`)), false);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), tracking);
  });

  it("updated -> older -> updated through real displacement never writes an admission", async () => {
    await rewriteTimes(2);
    const second = withContext(2);
    commitTree("generation two", true);
    assert.equal((await fetchHead(second)).kind, "updated", "a later generation displaces the older tracking head");
    commitTree("generation one again", true);
    assert.equal((await fetchHead()).kind, "updated", "the older generation updates again");
    commitTree("generation two again", true);
    assert.equal((await fetchHead(second)).kind, "updated");
    assert.deepEqual(refusalFiles(), [], "pinning and displacement never write admissions");
    assert.deepEqual(await gate(base, 2), { kind: "none" });
  });

  it("P4 (conflicting ancestor tracking ref) records its refusal under the legacy generation and honours the seal", async () => {
    // A legacy flat ref owned by the run blocks the real tracking ref's directory.
    const ancestorTip = commitTree("ancestor", false);
    toBare(ancestorTip);
    gitIn(bare, ["update-ref", "refs/uzi-runner/agent", ancestorTip]);
    gitIn(bare, ["config", "--local", "uzi-trackowner.agent.owner", RUN]);
    for (let i = 0; i < CAP; i++) gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${String(i).padStart(40, "a")}`, base]);
    const incoming = commitTree("p4", true);
    // A confirmed incoming head needs no pin, so the first pin attempt is the ancestor's.
    const confirmed: FetchAgentBranchOptions = { ...opts, remotelyConfirmedSha: incoming };
    assert.deepEqual(await fetchHead(confirmed), { kind: "not_updated", reason: "owed_limit", refusedSha: incoming });
    assert.deepEqual(readJson(ledgerName).entries, [{ generation: 0, sha: ancestorTip, path: "P4" }]);
    assert.ok(incomingRefs().includes(`refs/uzi-incoming/${RUN}/0/refused-${ancestorTip} ${ancestorTip}`));
    // Once a seal exists nothing more is recorded, even for a different ancestor head.
    writeRaw(`final-${RUN}-1.json`, { version: 1, runId: RUN, generation: 1, sourceSha: incoming, evidenceDigest: "d".repeat(64), process: OTHER_NONCE });
    const newer = commitTree("ancestor again", false);
    toBare(newer);
    gitIn(bare, ["update-ref", "refs/uzi-runner/agent", newer]);
    const before = filesSnapshot();
    assert.equal((await fetchHead(confirmed)).kind, "not_updated");
    assert.equal(filesSnapshot(), before);
    assert.equal(incomingRefs().some((r) => r.includes(`refused-${newer}`)), false);
  });

  /** Hold the bare lock, start `call`, close the generation while it waits, then release. */
  async function closeWhileWaiting<T>(call: () => Promise<T>): Promise<T> {
    let release!: () => void;
    const held = cache["withLock"](bare, () => new Promise<void>((resolve) => { release = resolve; }));
    const pending = call();
    await new Promise((resolve) => setTimeout(resolve, 600));
    cache.closeRefusalGeneration(RUN, 1);
    release();
    await held;
    return pending;
  }

  it("a close that lands while fetchAgentBranch waits for the bare lock stages, pins and moves nothing", async () => {
    await rewriteTimes(2);
    const tip = gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]);
    const pins = pinRefs();
    commitTree("raced", true);
    const git = cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = git.runGit.bind(cache);
    const transports: string[][] = [];
    git.runGit = async (cwd, args, ...rest) => { if (args.includes("fetch")) transports.push(args); return original(cwd, args, ...rest); };
    try {
      assert.deepEqual(await closeWhileWaiting(() => fetchHead()), { kind: "not_updated", reason: "ownership_unknown" });
    } finally { git.runGit = original; }
    assert.deepEqual(transports, [], "the closed generation never starts the transport that would create its stage");
    assert.deepEqual(incomingRefs(), []);
    assert.deepEqual(pinRefs(), pins);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), tip);
  });

  it("a close that lands while updateTrackingRef waits for the bare lock pins and moves nothing", async () => {
    await rewriteTimes(2);
    const tip = gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]);
    const pins = pinRefs();
    const next = commitTree("raced update", false);
    toBare(next);
    assert.deepEqual(await closeWhileWaiting(() => cache.updateTrackingRef(bare, BRANCH, next, opts)),
      { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(pinRefs(), pins);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), tip);
  });

  it("P7 after the seal records a tracking head that was never ledgered: nothing", async () => {
    const kept = await atCap();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, coverOf(kept)), "sealed");
    unpinTrackingHead(kept);
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "owed_limit" });
    assert.equal(fs.existsSync(path.join(refDir(), ledgerName)), false);
    assert.deepEqual(incomingRefs(), []);
  });

  it("a legacy (null generation) import refused at the cap leaves no flat stage, only the generation-0 refused ref", async () => {
    const legacy: FetchAgentBranchOptions = { context: { ...ctx, generation: null, legacy: true } as FetchAgentBranchOptions["context"] };
    for (let i = 0; i < CAP; i++) {
      commitTree("legacy", true);
      assert.equal((await fetchHead(legacy)).kind, "updated");
    }
    const refused = commitTree("legacy refused", true);
    assert.deepEqual(await fetchHead(legacy), { kind: "not_updated", reason: "owed_limit", refusedSha: refused });
    assert.deepEqual(incomingRefs(), [`refs/uzi-incoming/${RUN}/0/refused-${refused} ${refused}`]);
  });

  it("P7 (retainCurrentOwedCandidate) records its refusal and honours the seal", async () => {
    const { kept, refused } = await eligibleState();
    const tracking = kept[kept.length - 1]!;
    unpinTrackingHead(kept);
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "owed_limit" });
    assert.ok(readJson(ledgerName).entries.some((e: any) => e.sha === tracking && e.path === "P7"));
    const cover = coverOf([...kept, refused]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const before = evidence();
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "owed_limit" });
    assert.equal(evidence(), before);
  });

  it("a failed recording still reports owed_limit; the unledgered refused ref keeps the gate ineligible", async () => {
    const kept = await atCap();
    await cache.admitRefusalProvenance(ctx);
    unpinTrackingHead(kept);
    fs.chmodSync(refDir(), 0o500);
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "owed_limit" });
    fs.chmodSync(refDir(), 0o700);
    assert.equal(fs.existsSync(path.join(refDir(), ledgerName)), false, "no ledger claim was made");
    const tracking = kept[kept.length - 1]!;
    const verdict = await gate(coverOf([...kept]));
    assert.deepEqual(verdict, { kind: "ineligible", reason: "refusal recording failed in this process" });
    assert.ok(incomingRefs().some((r) => r.includes(`refused-${tracking}`)));
  });

  it("the tracking head must be covered by the gate head or owed-pinned", async () => {
    const { kept, refused } = await eligibleState();
    const tracking = kept[kept.length - 1]!;
    const withoutTracking = coverOf([...kept.slice(0, -1), refused]);
    assert.equal((await gate(withoutTracking)).kind, "eligible", "the tracking head is pinned, so it needs no coverage");
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${tracking}`]);
    gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${"b".repeat(40)}`, base]);
    assert.deepEqual(await gate(withoutTracking), { kind: "ineligible", reason: "tracking head is not covered" });
    assert.equal((await gate(coverOf([...kept, refused]))).kind, "eligible");
  });

  it("sealRefusalRelease writes once, then matches, then reports a mismatch after the evidence changes", async () => {
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, base), "none");
    assert.equal(refusalFiles().some((n) => n.startsWith("final-")), false, "none writes nothing");
    const { kept, refused, cover } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, "f".repeat(40)), "ineligible");
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const final = readJson(`final-${RUN}-1.json`);
    assert.equal(final.sourceSha, cover);
    assert.equal((await gate(cover) as { evidenceDigest: string }).evidenceDigest, final.evidenceDigest);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "matches");
    assert.deepEqual(readJson(`final-${RUN}-1.json`), final, "write-once");
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, coverOf([cover])), "mismatch", "another source sha");
    const other = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP, processNonce: OTHER_NONCE }));
    assert.equal(await other.sealRefusalRelease(bare, RUN, 1, cover), "ineligible", "admissions belong to our process");
    // Evidence changes after the seal: a new covered stage alters the digest.
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/1/refused-${refused}-extra`, refused]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "mismatch");
    void kept;
  });
});

describe("issue4481 recording failure latch", { timeout: 300_000 }, () => {
  /** Drive the chokepoint directly for a non-tracking head (the P4/P5/P6 shape the tracking-tip rule cannot see). */
  async function refuseWithFailure(failRef: boolean, failLedger: boolean): Promise<{ kept: string[]; head: string }> {
    const kept = await atCap();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const head = commitTree("unseen head", false);
    toBare(head);
    const internals = cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = internals.runGit.bind(cache);
    if (failRef) internals.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "update-ref" && String(args[1]).includes("/refused-")) throw new Error("injected refused-ref failure");
      return original(cwd, args, ...rest);
    };
    if (failLedger) fs.chmodSync(refDir(), 0o500);
    try {
      await assert.rejects(cache["pinOwedUnderLock"](ctx, head, "P4"), /owed candidate limit/);
    } finally {
      internals.runGit = original;
      fs.chmodSync(refDir(), 0o700);
    }
    return { kept, head };
  }
  for (const [name, failRef, failLedger] of [["refused-ref write", true, false], ["ledger write", false, true], ["both writes", true, true]] as const) {
    it(`a failed ${name} latches the run: the gate and the seal stay ineligible`, async () => {
      const { kept, head } = await refuseWithFailure(failRef, failLedger);
      // The surviving evidence alone would look covered: the head is an ancestor of the gate head.
      const cover = coverOf([...kept, head]);
      assert.deepEqual(await gate(cover), { kind: "ineligible", reason: "refusal recording failed in this process" });
      assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "ineligible");
      assert.equal(refusalFiles().some((n) => n.startsWith("final-")), false);
      // The latch is per run: another run on the same cache is unaffected.
      assert.deepEqual(await cache.refusalReleaseGate(bare, "other-run", 1, cover), { kind: "none" });
    });
  }
  /** A cap refusal of an unseen head with `inject` active only while the chokepoint runs. */
  async function refuseWith(inject: (restore: () => void) => () => void): Promise<{ kept: string[]; head: string }> {
    const kept = await atCap();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const head = commitTree("unseen head", false);
    toBare(head);
    const restore = inject(() => {});
    try { await assert.rejects(cache["pinOwedUnderLock"](ctx, head, "P4"), /owed candidate limit/); }
    finally { restore(); }
    return { kept, head };
  }
  it("a one-shot failure of the seal check latches the run for the process lifetime", async () => {
    const internals = cache as unknown as { refusalNames: (barePath: string) => Promise<string[]> };
    const { kept, head } = await refuseWith(() => {
      const original = internals.refusalNames.bind(cache);
      let calls = 0;
      internals.refusalNames = async (barePath) => { if (calls++ === 0) throw new Error("injected seal-check failure"); return original(barePath); };
      return () => { internals.refusalNames = original; };
    });
    assert.equal(refusalFiles().some((n) => n.startsWith("refusals-")), false, "the failed seal check recorded nothing");
    assert.deepEqual(await gate(coverOf([...kept, head])), { kind: "ineligible", reason: "refusal recording failed in this process" });
  });
  it("a refused-ref write that does not read back is a failed write", async () => {
    const internals = cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const { kept, head } = await refuseWith(() => {
      const original = internals.runGit.bind(cache);
      // The write "succeeds" but leaves nothing behind, so the read-back finds no ref.
      internals.runGit = async (cwd, args, ...rest) =>
        args[0] === "update-ref" && String(args[1]).includes("/refused-") ? "" : original(cwd, args, ...rest);
      return () => { internals.runGit = original; };
    });
    assert.equal(incomingRefs().some((r) => r.includes("/refused-")), false);
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => e.sha), [head], "the ledger entry alone was written");
    assert.deepEqual(await gate(coverOf([...kept, head])), { kind: "ineligible", reason: "refusal recording failed in this process" });
  });
  it("the writes are independent: a failed ref write still ledgers the head", async () => {
    const { head } = await refuseWithFailure(true, false);
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => e.sha), [head]);
    assert.equal(incomingRefs().some((r) => r.includes("/refused-")), false);
  });
  it("a successful recording does not latch", async () => {
    const { kept, head } = await refuseWithFailure(false, false);
    assert.equal((await gate(coverOf([...kept, head]))).kind, "eligible");
  });
});

describe("issue4481 cleanup", { timeout: 300_000 }, () => {
  const sealed = async () => {
    const state = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, state.cover), "sealed");
    return state;
  };

  it("without refusal evidence cleanup is unchanged", async () => {
    const kept = await rewriteTimes(3);
    assert.equal(await cleanup(coverOf(kept)), "removed");
    assert.deepEqual(pinRefs(), []);
    assert.deepEqual(refusalFiles(), []);
  });

  it("a sealed, covered generation is cleaned up: stages, ledger, final, cleanup and admissions go", async () => {
    const { cover } = await sealed();
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), []);
    assert.deepEqual(pinRefs(), []);
    assert.deepEqual(refusalFiles(), []);
  });

  it("a removed generation deletes only its own admission", async () => {
    const kept = await rewriteTimes(3);
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    const cover = coverOf(kept);
    assert.equal(await cache.cleanupRecoveryGeneration(bare, { ...ctx, generation: 2 }, [], [], [], cover), "removed");
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`], "an earlier generation's admission stays");
  });

  it("an aborted first-mutation boundary leaves the admission untouched", async () => {
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const before = filesSnapshot();
    await assert.rejects(cache.cleanupRecoveryGeneration(bare, ctx, [], [], [], base, () => true, "archive",
      () => { throw new Error("client aborted"); }), /client aborted/);
    assert.equal(filesSnapshot(), before);
    assert.equal(await cache.cleanupRecoveryGeneration(bare, ctx, [], [], [], base), "removed");
    assert.deepEqual(refusalFiles(), []);
  });

  it("the number of admission files never bounds the ordinary gate", async () => {
    fs.mkdirSync(refDir(), { recursive: true, mode: 0o700 });
    for (let i = 0; i < 4200; i++) {
      const other = `${String(i).padStart(8, "0")}-aaaa-bbbb-cccc-444444444444`;
      fs.writeFileSync(path.join(refDir(), `admission-${other}-1.json`), "{}\n", { mode: 0o600 });
    }
    assert.deepEqual(await gate(base), { kind: "none" });
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, base), "none");
  });

  it("evidence without a seal, or an uncovered change after the seal, retains everything", async () => {
    const kept = await atCap();
    await cache.admitRefusalProvenance(ctx);
    const refused = await refuse();
    const cover = coverOf([...kept, refused]);
    const snapshot = (): string => evidence() + JSON.stringify(pinRefs());
    const unsealed = snapshot();
    assert.equal(await cleanup(cover), "retained", "no final");
    assert.equal(snapshot(), unsealed);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const afterSeal = snapshot();
    assert.equal(await cleanup(coverOf([cover])), "retained", "the acked FINAL is not the sealed source");
    assert.equal(snapshot(), afterSeal);
    // A new, uncovered refusal after the final.
    const late = commitTree("late", false);
    toBare(late);
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/1/late`, late]);
    const changed = snapshot();
    assert.equal(await cleanup(cover), "retained");
    assert.equal(snapshot(), changed, "nothing was deleted");
  });

  it("a covered change after the final is also retained (the digest binds the evidence)", async () => {
    const { cover, refused } = await sealed();
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/1/extra`, refused]);
    const before = evidence() + JSON.stringify(pinRefs());
    assert.equal(await cleanup(cover), "retained");
    assert.equal(evidence() + JSON.stringify(pinRefs()), before);
  });

  it("resumes after a partial deletion and retains a rebound generation", async () => {
    const { cover } = await sealed();
    let opens = 0;
    // open() runs before the cleanup record, then before each stage deletion: fail after the first stage.
    await assert.rejects(cleanup(cover, () => ++opens <= 2), /source protection changed/);
    assert.ok(fs.existsSync(path.join(refDir(), `cleanup-${RUN}-1.json`)));
    assert.ok(fs.existsSync(path.join(refDir(), `final-${RUN}-1.json`)));
    assert.equal(incomingRefs().length, 1, "one stage was deleted before the failure");
    // A rebound generation: a new stage that the cleanup record never listed.
    const late = commitTree("rebound", false);
    toBare(late);
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/1/rebound`, late]);
    const frozen = evidence();
    assert.equal(await cleanup(cover), "retained");
    assert.equal(evidence(), frozen);
    gitIn(bare, ["update-ref", "-d", `refs/uzi-incoming/${RUN}/1/rebound`]);
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), []);
    assert.deepEqual(refusalFiles(), []);
  });
});
