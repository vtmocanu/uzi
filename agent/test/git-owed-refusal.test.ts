import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, CheckpointSoftDeadlineError, type BoundaryProcessSpawner, type FetchAgentBranchOptions, type PositiveOwedCandidateContext, type RefusalReleaseGate,
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
/** The run's import stages (an in-progress import's private ref), by name. */
const importStageRefs = (): string[] => incomingRefs().map((l) => l.split(" ")[0]!).filter((r) => /\/import-/.test(r));
const foreignImportStage = (tip: string, nonce = OTHER_NONCE, g = 1): string => {
  const ref = `refs/uzi-incoming/${RUN}/${g}/import-${nonce}-${randomUUID()}`;
  gitIn(bare, ["update-ref", ref, tip]);
  return ref;
};
const finishedSet = (c: GitCache = cache): Set<string> => (c as unknown as { finishedImportStages: Set<string> }).finishedImportStages;
/** An import stage that looks like a finished import of the cache's own process (its nonce, recorded as finished). */
const ownFinishedImportStage = (tip: string, g = 1, c: GitCache = cache): string => {
  const nonce = (c as unknown as { processNonce: string }).processNonce;
  const ref = `refs/uzi-incoming/${RUN}/${g}/import-${nonce}-${randomUUID()}`;
  gitIn(bare, ["update-ref", ref, tip]);
  finishedSet(c).add(ref);
  return ref;
};
const pinRefs = (): string[] => gitIn(bare, ["for-each-ref", "--format=%(refname)", `refs/uzi-owed/${RUN}/`]).split("\n").filter(Boolean).sort();
const gate = (head: string, g = 1, c: GitCache = cache, branch?: string): Promise<RefusalReleaseGate> => c.refusalReleaseGate(bare, RUN, g, head, branch);
const evidence = (): string => JSON.stringify({ files: refusalFiles().map((n) => [n, fs.readFileSync(path.join(refDir(), n), "utf8")]), refs: incomingRefs() });
const filesSnapshot = (): string => JSON.stringify(refusalFiles().map((n) => [n, fs.readFileSync(path.join(refDir(), n), "utf8")]));
/** Every candidate, context and other owed metadata file (bare/uzi-owed), so a retain that writes owed metadata is seen. */
const owedSnapshot = (): string => {
  const dir = path.join(bare, "uzi-owed");
  if (!fs.existsSync(dir)) return "[]";
  return JSON.stringify(fs.readdirSync(dir).sort().map((n) => {
    const full = path.join(dir, n);
    return [n, fs.statSync(full).isFile() ? fs.readFileSync(full, "utf8") : "<dir>"];
  }));
};
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
    assert.ok(first.some((r) => new RegExp(`^refs/uzi-incoming/${RUN}/1/import-[A-Za-z0-9-]{8,64}-[0-9a-f-]{36} ${refused}$`).test(r)));
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

  it("an unanchored import stage (its refused ref gone) makes the gate ineligible, and re-anchoring restores eligibility", async () => {
    const { cover, refused } = await eligibleState();
    assert.equal((await gate(cover)).kind, "eligible");
    const refusedRef = `refs/uzi-incoming/${RUN}/1/refused-${refused}`;
    gitIn(bare, ["update-ref", "-d", refusedRef]);
    assert.deepEqual(await gate(cover), { kind: "ineligible", reason: "refusal import stage is unanchored" });
    gitIn(bare, ["update-ref", refusedRef, refused]);
    assert.equal((await gate(cover)).kind, "eligible");
  });

  it("an import stage anchored only at an earlier generation is unanchored at its own generation, not excluded", async () => {
    const { cover, refused } = await eligibleState();
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    // The refused ref and ledger row for this sha exist at generation 1 only; this stage sits at generation 2. It is a
    // finished import of this process, so only its anchoring is in question.
    ownFinishedImportStage(refused, 2);
    assert.deepEqual(await gate(cover, 2), { kind: "ineligible", reason: "refusal import stage is unanchored" });
  });

  it("a live in-process import stage makes the gate ineligible while it is in flight", async () => {
    const { cover } = await eligibleState();
    const [stage] = importStageRefs() as [string];
    const live = (cache as unknown as { liveImportStages: Set<string> }).liveImportStages;
    live.add(stage);
    assert.deepEqual(await gate(cover), { kind: "ineligible", reason: "refusal import in progress" });
    live.delete(stage);
    assert.equal((await gate(cover)).kind, "eligible");
  });

  it("a stage that is neither an import nor a refused ref stays evidence; a short-nonce import-looking name is not an import", async () => {
    const { cover, refused } = await eligibleState();
    gitIn(bare, ["update-ref", `refs/uzi-incoming/${RUN}/1/import-short-${randomUUID()}`, refused]);
    // The nonce is shorter than the 8 characters an import stage carries: it is plain evidence, here ledgered, so it is
    // covered and eligible, and it enters the digest (the digest differs from the one without it).
    const withExtra = await gate(cover);
    assert.equal(withExtra.kind, "eligible");
    gitIn(bare, ["update-ref", "-d", incomingRefs().map((l) => l.split(" ")[0]!).find((r) => r.includes("/import-short-"))!]);
    const without = await gate(cover);
    assert.equal(without.kind, "eligible");
    assert.notEqual((without as { evidenceDigest: string }).evidenceDigest, (withExtra as { evidenceDigest: string }).evidenceDigest);
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
    // retainCurrentOwedCandidate is not stopped by the in-process fence; only a seal of the generation it would pin stops it.
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
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "ownership_unknown" });
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
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "ownership_unknown" });
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
      // The durable marker is written whenever the directory is writable (not in the chmod ledger-only variant).
      if (!failLedger) assert.ok(fs.existsSync(path.join(refDir(), `incomplete-${RUN}.json`)), "the incomplete marker exists");
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
    assert.ok(refusalFiles().includes(`incomplete-${RUN}.json`), "the failed seal check left the durable incomplete marker");
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

  it("a sealed, covered generation is cleaned up: stages, ledger, final and cleanup go; the admission stays", async () => {
    const { cover } = await sealed();
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), []);
    assert.deepEqual(pinRefs(), []);
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`]);
  });

  it("generation 1 cleanup, then generation 2 of the same process at the cap is eligible", async () => {
    const { cover } = await sealed();
    assert.equal(await cleanup(cover), "removed");
    for (let i = pinRefs().length; i < CAP; i++) gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${String(i).padStart(40, "d")}`, base]);
    assert.equal(pinRefs().length, CAP);
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    const tracking = gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]);
    assert.equal((await gate(coverOf([tracking]), 2)).kind, "eligible");
  });

  it("cleanup never deletes an admission, of its own or of an earlier generation", async () => {
    const kept = await rewriteTimes(3);
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    const cover = coverOf(kept);
    assert.equal(await cache.cleanupRecoveryGeneration(bare, { ...ctx, generation: 2 }, [], [], [], cover), "removed");
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`, `admission-${RUN}-2.json`]);
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
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`]);
  });

  /** A sealed generation with its cleanup intent written and nothing deleted yet. */
  async function intentWritten() {
    const state = await sealed();
    let opens = 0;
    await assert.rejects(cleanup(state.cover, () => ++opens <= 1), /source protection changed/);
    assert.ok(fs.existsSync(path.join(refDir(), `cleanup-${RUN}-1.json`)));
    return state;
  }

  it("F2: a head ledgered at G and again at G+1 after the intent was written: G's cleanup removes only the (G, head) row", async () => {
    const { cover, refused } = await intentWritten();
    const ledger = readJson(ledgerName);
    const row = ledger.entries.find((e: any) => e.sha === refused);
    writeRaw(ledgerName, { ...ledger, entries: [...ledger.entries, { ...row, generation: 2 }] });
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), [], "the listed stages are gone");
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => [e.generation, e.sha]), [[2, refused]], "only (G, head) was removed");
    assert.deepEqual(refusalFiles().filter((n) => n.startsWith("final-") || n.startsWith("cleanup-")), []);
    // The later generation's evidence is untouched and its own gate still demands that head be covered.
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    const uncovered = await gate(base, 2);
    assert.equal(uncovered.kind, "ineligible");
    assert.match((uncovered as { reason: string }).reason, new RegExp(`refused head ${refused} is not covered`));
    assert.match((await gate(coverOf([gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`])]), 2) as { reason: string }).reason, new RegExp(`refused head ${refused} is not covered`));
    assert.equal((await gate(coverOf([gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), refused]), 2)).kind, "eligible");
  });

  it("a crash in G's cleanup tail after the ledger rewrite is not permanent when the head is also ledgered at G+1", async () => {
    const { cover, refused } = await intentWritten();
    const ledger = readJson(ledgerName);
    const row = ledger.entries.find((e: any) => e.sha === refused);
    writeRaw(ledgerName, { ...ledger, entries: [...ledger.entries, { ...row, generation: 2 }] });
    // The crashed pass got as far as the ledger rewrite: stages deleted, the (G, head) row removed, intent still on disk.
    await cache["applyRefusalCleanup"](bare, readJson(`cleanup-${RUN}-1.json`), () => undefined);
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => [e.generation, e.sha]), [[2, refused]]);
    assert.ok(fs.existsSync(path.join(refDir(), `cleanup-${RUN}-1.json`)));
    assert.equal(await cleanup(cover), "removed", "the resumed G cleanup completes");
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => [e.generation, e.sha]), [[2, refused]], "the G+1 row is intact");
    assert.deepEqual(refusalFiles().filter((n) => n.startsWith("final-") || n.startsWith("cleanup-")), []);
  });

  it("F5: an unrelated ref appended to the cleanup intent is retained and not deleted", async () => {
    const { cover, refused } = await intentWritten();
    const unrelated = `refs/heads/precious-${seq}`;
    gitIn(bare, ["update-ref", unrelated, refused]);
    const intent = readJson(`cleanup-${RUN}-1.json`);
    writeRaw(`cleanup-${RUN}-1.json`, { ...intent, stages: [...intent.stages, { ref: unrelated, generation: 1, tip: refused }] });
    assert.equal(await cleanup(cover), "retained");
    assert.equal(gitIn(bare, ["rev-parse", unrelated]), refused);
    assert.equal(incomingRefs().length, 2);
  });

  it("F5: a self-consistent intent that lists a ref outside this run's incoming namespace is retained", async () => {
    const { cover, refused } = await intentWritten();
    const unrelated = `refs/heads/precious-${seq}`;
    gitIn(bare, ["update-ref", unrelated, refused]);
    const intent = readJson(`cleanup-${RUN}-1.json`);
    const stages = [...intent.stages, { ref: unrelated, generation: 1, tip: refused }];
    // Forge the whole intent so its digest binds the unrelated ref too, and drop the FINAL: only the namespace rule can refuse it.
    const lines = [...intent.ledgerEntries.map((e: any) => `L ${e.generation} ${e.sha}`), ...stages.map((x: any) => `S ${x.ref} ${x.tip}`)].sort();
    const finalDigest = createHash("sha256").update(lines.join("\n")).digest("hex");
    writeRaw(`cleanup-${RUN}-1.json`, { ...intent, stages, finalDigest });
    fs.rmSync(path.join(refDir(), `final-${RUN}-1.json`));
    assert.equal(await cleanup(cover), "retained");
    assert.equal(gitIn(bare, ["rev-parse", unrelated]), refused);
    assert.equal(incomingRefs().length, 2);
  });

  /** Rewrites the intent with its digest recomputed so every hash rule holds, and drops the FINAL. */
  function forgeIntent(change: (intent: any) => { stages: any[]; ledgerEntries: any[] }): void {
    const intent = readJson(`cleanup-${RUN}-1.json`);
    const { stages, ledgerEntries } = change(intent);
    const lines = [...ledgerEntries.map((e: any) => `L ${e.generation} ${e.sha}`), ...stages.map((x: any) => `S ${x.ref} ${x.tip}`)].sort();
    writeRaw(`cleanup-${RUN}-1.json`, { ...intent, stages, ledgerEntries, finalDigest: createHash("sha256").update(lines.join("\n")).digest("hex") });
    fs.rmSync(path.join(refDir(), `final-${RUN}-1.json`));
  }

  it("S2: a self-consistent intent listing a ledger entry at generation G+1 is retained", async () => {
    const { cover } = await intentWritten();
    forgeIntent((i) => ({ stages: i.stages, ledgerEntries: [...i.ledgerEntries, { generation: 2, sha: base }] }));
    assert.equal(await cleanup(cover), "retained");
    assert.equal(incomingRefs().length, 2);
  });

  it("S3: a self-consistent intent whose stage tip is not among its ledger entries is retained", async () => {
    const { cover } = await intentWritten();
    forgeIntent((i) => ({ stages: [...i.stages, { ref: `refs/uzi-incoming/${RUN}/1/extra`, generation: 1, tip: base }], ledgerEntries: i.ledgerEntries }));
    assert.equal(await cleanup(cover), "retained");
    assert.equal(incomingRefs().length, 2);
  });

  it("F5: an intent whose lists no longer hash to the sealed digest is retained even without the FINAL", async () => {
    const { cover } = await intentWritten();
    const intent = readJson(`cleanup-${RUN}-1.json`);
    // An extra listed entry keeps every other rule satisfied (current evidence is still a subset), so only the digest recompute refuses.
    writeRaw(`cleanup-${RUN}-1.json`, { ...intent, ledgerEntries: [...intent.ledgerEntries, { generation: 1, sha: base }] });
    fs.rmSync(path.join(refDir(), `final-${RUN}-1.json`));
    assert.equal(await cleanup(cover), "retained");
    assert.equal(incomingRefs().length, 2);
  });

  it("an anchored stopped import stage is no evidence: deleting it after the seal still matches", async () => {
    const { cover } = await sealed();
    const [stage] = importStageRefs() as [string];
    gitIn(bare, ["update-ref", "-d", stage]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "matches");
  });

  it("a finished, anchored import stage left by this process is excluded from the seal and deleted by the sealed cleanup", async () => {
    const { cover, refused } = await sealed();
    const foreign = ownFinishedImportStage(refused);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "matches", "the leftover stage is not evidence");
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), []);
    assert.equal(gitIn(bare, ["for-each-ref", foreign]), "");
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`]);
  });

  it("the intent lists the anchored import stage apart from the evidence stages and the sealed digest", async () => {
    const { refused } = await intentWritten();
    const intent = readJson(`cleanup-${RUN}-1.json`);
    assert.deepEqual(intent.stages.map((x: any) => x.ref), [`refs/uzi-incoming/${RUN}/1/refused-${refused}`]);
    assert.equal(intent.importStages.length, 1);
    assert.match(intent.importStages[0].ref, new RegExp(`^refs/uzi-incoming/${RUN}/1/import-[A-Za-z0-9-]{8,64}-[0-9a-f-]{36}$`));
    assert.equal(intent.importStages[0].tip, refused);
    assert.equal(intent.finalDigest, readJson(`final-${RUN}-1.json`).evidenceDigest);
  });

  it("interrupted between the intent and the import deletion, the cleanup resumes and completes", async () => {
    const { cover } = await intentWritten();
    assert.equal(incomingRefs().length, 2, "nothing was deleted yet");
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), []);
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`]);
  });

  it("interrupted after the import and refused-ref deletions (ledger not yet rewritten), the cleanup resumes and completes", async () => {
    const { cover } = await intentWritten();
    for (const line of incomingRefs()) gitIn(bare, ["update-ref", "-d", line.split(" ")[0]!]);
    assert.ok(readJson(ledgerName).entries.length > 0, "the ledger still lists the head");
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(refusalFiles(), [`admission-${RUN}-1.json`]);
  });

  it("interrupted after only the import deletion, the cleanup resumes and completes", async () => {
    const { cover } = await intentWritten();
    for (const ref of importStageRefs()) gitIn(bare, ["update-ref", "-d", ref]);
    assert.equal(incomingRefs().length, 1);
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(incomingRefs(), []);
  });

  it("an import stage the intent does not list, appearing after the intent was written, is retained", async () => {
    const { cover, refused } = await intentWritten();
    const late = foreignImportStage(refused);
    const before = evidence() + JSON.stringify(pinRefs());
    assert.equal(await cleanup(cover), "retained");
    assert.equal(evidence() + JSON.stringify(pinRefs()), before, "nothing was deleted");
    gitIn(bare, ["update-ref", "-d", late]);
    assert.equal(await cleanup(cover), "removed");
  });

  it("an import stage whose tip moved since the intent is retained", async () => {
    const { cover } = await intentWritten();
    const [stage] = importStageRefs() as [string];
    const moved = commitTree("moved import", false);
    toBare(moved);
    gitIn(bare, ["update-ref", stage, moved]);
    assert.equal(await cleanup(cover), "retained");
    assert.equal(gitIn(bare, ["rev-parse", stage]), moved);
  });

  it("a live in-process import retains the sealed cleanup", async () => {
    const { cover } = await sealed();
    const [stage] = importStageRefs() as [string];
    const live = (cache as unknown as { liveImportStages: Set<string> }).liveImportStages;
    live.add(stage);
    const before = evidence() + JSON.stringify(pinRefs());
    assert.equal(await cleanup(cover), "retained");
    assert.equal(evidence() + JSON.stringify(pinRefs()), before);
    live.delete(stage);
    assert.equal(await cleanup(cover), "removed");
  });

  it("an unanchored import stage retains the sealed cleanup plan", async () => {
    const { cover, refused } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    foreignImportStage(refused, OTHER_NONCE, 2);
    gitIn(bare, ["update-ref", "-d", `refs/uzi-incoming/${RUN}/1/refused-${refused}`]);
    assert.equal(await cleanup(cover), "retained");
  });

  it("the ledger half of the exact-pair subset check: a ledger row added after the intent, with no stage, is retained", async () => {
    const { cover } = await intentWritten();
    const ledger = readJson(ledgerName);
    writeRaw(ledgerName, { ...ledger, entries: [...ledger.entries, { generation: 1, sha: base, path: "P2" }] });
    const before = evidence() + JSON.stringify(pinRefs());
    assert.equal(await cleanup(cover), "retained");
    assert.equal(evidence() + JSON.stringify(pinRefs()), before, "nothing was deleted");
  });

  it("refusal-directory writes need a valid run id", async () => {
    const write = cache["writeOwedFile"].bind(cache) as (b: string, n: string, v: unknown, d?: string, r?: string) => Promise<void>;
    await assert.rejects(write(bare, "x.json", {}, "info/uzi-refusals"), /valid run id/);
    await assert.rejects(write(bare, "x.json", {}, "info/uzi-refusals", "not a run id!"), /valid run id/);
    assert.equal(refusalFiles().includes("x.json"), false);
  });
});

describe("issue4481 durable incomplete marker and seal comparison", { timeout: 300_000 }, () => {
  it("F1: a double recording failure leaves a durable marker; a new process below the cap is ineligible, not none", async () => {
    const kept = await atCap();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const head = commitTree("unseen head", false);
    toBare(head);
    const internals = cache as unknown as {
      runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string>;
      writeOwedFile: (b: string, name: string, value: unknown, dir?: string, runId?: string) => Promise<void>;
    };
    const runGit = internals.runGit.bind(cache), write = internals.writeOwedFile.bind(cache);
    internals.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "update-ref" && String(args[1]).includes("/refused-")) throw new Error("injected ref failure");
      return runGit(cwd, args, ...rest);
    };
    internals.writeOwedFile = async (b, name, value, dir, runId) => {
      if (name.startsWith("refusals-")) throw new Error("injected ledger failure");
      return write(b, name, value, dir, runId);
    };
    try { await assert.rejects(cache["pinOwedUnderLock"](ctx, head, "P4"), /owed candidate limit/); }
    finally { internals.runGit = runGit; internals.writeOwedFile = write; }
    assert.deepEqual(readJson(`incomplete-${RUN}.json`), { version: 1, runId: RUN, branch: BRANCH, barePath: bare });
    // Free the cap entirely: nothing but the marker remains to say the history is incomplete.
    for (const sha of kept) gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${sha}`]);
    const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP, processNonce: OTHER_NONCE }));
    assert.deepEqual(await fresh.refusalReleaseGate(bare, RUN, 1, base), { kind: "ineligible", reason: "refusal history incomplete" });
    assert.equal(await fresh.sealRefusalRelease(bare, RUN, 1, base), "ineligible");
    // An unparseable marker counts too, and cleanup never deletes it.
    writeRaw(`incomplete-${RUN}.json`, "{garbage");
    assert.equal((await fresh.refusalReleaseGate(bare, RUN, 1, base)).kind, "ineligible");
  });

  it("F1: an ordinary cleanup never deletes the incomplete marker", async () => {
    const kept = await rewriteTimes(2);
    fs.mkdirSync(refDir(), { recursive: true, mode: 0o700 });
    writeRaw(`incomplete-${RUN}.json`, { version: 1, runId: RUN, branch: BRANCH, barePath: bare });
    assert.equal(await cleanup(coverOf(kept)), "removed");
    assert.ok(fs.existsSync(path.join(refDir(), `incomplete-${RUN}.json`)));
  });

  it("F3: a seal whose evidence has vanished compares as a mismatch instead of reporting none", async () => {
    const kept = await atCap();
    await cache.admitRefusalProvenance(ctx);
    const refused = await refuse();
    const cover = coverOf([...kept, refused]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    fs.rmSync(path.join(refDir(), ledgerName));
    for (const line of incomingRefs()) gitIn(bare, ["update-ref", "-d", line.split(" ")[0]!]);
    for (const sha of kept) gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${sha}`]);
    // The cap-filling pin left an owed-path marker, so an empty-looking run is proven by this process's admission
    // (eligible with the empty-evidence digest), not reported as none.
    assert.deepEqual(await gate(cover), { kind: "eligible", evidenceDigest: createHash("sha256").update("").digest("hex") });
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "mismatch");
  });

  it("R1: updated -> older -> updated at the cap, the middle generation unadmitted (old worker), is ineligible", async () => {
    const kept = await atCap();
    // Generation 1 (current worker) admitted. Generation 2 ran on an old worker: no admission, and its refusal
    // stage was deleted as old code does. Generation 3 (this worker) displaces and refuses at the cap.
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const oldWorkerHead = commitTree("generation two on an old worker", true);
    toBare(oldWorkerHead);
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 3 }), "written");
    const third = withContext(3);
    const refused = commitTree("generation three refused", true);
    assert.deepEqual(await fetchHead(third), { kind: "not_updated", reason: "owed_limit", refusedSha: refused });
    const cover = coverOf([...kept, oldWorkerHead, refused]);
    assert.deepEqual(await gate(cover, 3), { kind: "ineligible", reason: "admission for generation 2 absent" });
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 3, cover), "ineligible");
  });
});

describe("issue4481 owed-path marker (restart residual)", { timeout: 300_000 }, () => {
  const markerName = `owedpath-${RUN}-1.json`;
  const restarted = (): GitCache => new GitCache(fx.dataDir, nullLogger(), undefined,
    testGitCacheOptions({ maxOwedCandidates: CAP, processNonce: OTHER_NONCE }));
  type Internals = {
    runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string>;
    writeOwedFile: (b: string, name: string, value: unknown, dir?: string, runId?: string) => Promise<void>;
  };
  /** A cap refusal of an unseen head in which the refused ref, ledger and incomplete-marker writes all fail. */
  async function refuseWithEverythingFailing(): Promise<{ kept: string[]; head: string }> {
    const kept = await atCap();
    const head = commitTree("unseen head", false);
    toBare(head);
    const internals = cache as unknown as Internals;
    const runGit = internals.runGit.bind(cache), write = internals.writeOwedFile.bind(cache);
    internals.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "update-ref" && String(args[1]).includes("/refused-")) throw new Error("injected ref failure");
      return runGit(cwd, args, ...rest);
    };
    internals.writeOwedFile = async (b, name, value, dir, runId) => {
      if (name.startsWith("refusals-") || name.startsWith("incomplete-")) throw new Error("injected write failure");
      return write(b, name, value, dir, runId);
    };
    try { await assert.rejects(cache["pinOwedUnderLock"](ctx, head, "P4"), /owed candidate limit/); }
    finally { internals.runGit = runGit; internals.writeOwedFile = write; }
    return { kept, head };
  }

  it("the cap-filling pin leaves a write-once marker; fewer pins leave none", async () => {
    await rewriteTimes(CAP - 1);
    assert.equal(refusalFiles().includes(markerName), false, "below the cap there is no marker");
    assert.deepEqual(await gate(base), { kind: "none" });
    await rewriteTimes(1);
    assert.deepEqual(readJson(markerName), { version: 1, runId: RUN, generation: 1, branch: BRANCH, barePath: bare });
    const before = fs.readFileSync(path.join(refDir(), markerName), "utf8");
    await rewriteTimes(0);
    const internals = cache as unknown as Internals;
    const write = internals.writeOwedFile.bind(cache);
    const markerWrites: string[] = [];
    internals.writeOwedFile = async (b, name, value, dir, runId) => {
      if (name === markerName) markerWrites.push(name);
      return write(b, name, value, dir, runId);
    };
    try { await assert.rejects(cache["pinOwedUnderLock"](ctx, base, "P4"), /owed candidate limit/); }
    finally { internals.writeOwedFile = write; }
    assert.deepEqual(markerWrites, [], "an existing marker is never rewritten");
    assert.equal(fs.readFileSync(path.join(refDir(), markerName), "utf8"), before, "write-once");
  });

  it("R2(i): a restarted process cannot call a run none after every refusal write failed (admission is another process's)", async () => {
    const { kept } = await refuseWithEverythingFailing();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    assert.equal(refusalFiles().some((n) => n.startsWith("refusals-") || n.startsWith("incomplete-")), false);
    assert.equal(incomingRefs().some((r) => r.includes("/refused-")), false);
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${kept[0]}`]);
    assert.ok(pinRefs().length < CAP);
    const verdict = await gate(coverOf(kept), 1, restarted());
    assert.equal(verdict.kind, "ineligible", JSON.stringify(verdict));
    assert.match((verdict as { reason: string }).reason, /another process/);
  });

  it("R2(ii): the same with no admission ever written is ineligible", async () => {
    const { kept } = await refuseWithEverythingFailing();
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${kept[0]}`]);
    assert.deepEqual(await gate(coverOf(kept), 1, restarted()), { kind: "ineligible", reason: "admission for generation 1 absent" });
  });

  it("R2(iii): a run that never filled the cap stays none after a restart", async () => {
    await rewriteTimes(CAP - 1);
    assert.deepEqual(await gate(base, 1, restarted()), { kind: "none" });
    assert.equal(refusalFiles().some((n) => n.startsWith("owedpath-")), false);
  });

  it("the refusal branch writes the marker when pins were made outside the pin path", async () => {
    for (let i = 0; i < CAP; i++) gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${String(i).padStart(40, "e")}`, base]);
    const head = commitTree("direct", false);
    toBare(head);
    await assert.rejects(cache["pinOwedUnderLock"](ctx, head, "P4"), /owed candidate limit/);
    assert.ok(refusalFiles().includes(markerName));
  });

  it("R2(iv): a failing marker write refuses the cap-filling pin as preservation_failed, and no pin ref appears", async () => {
    await rewriteTimes(CAP - 1);
    const pinsBefore = pinRefs();
    const internals = cache as unknown as Internals;
    const write = internals.writeOwedFile.bind(cache);
    internals.writeOwedFile = async (b, name, value, dir, runId) => {
      if (name.startsWith("owedpath-")) throw new Error("injected marker failure");
      return write(b, name, value, dir, runId);
    };
    try {
      commitTree("filler", true);
      assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "preservation_failed" });
    } finally { internals.writeOwedFile = write; }
    assert.deepEqual(pinRefs(), pinsBefore);
    assert.equal(refusalFiles().includes(markerName), false);
  });

  it("cleanup of a marker-only generation removes the marker, nothing else", async () => {
    const kept = await atCap();
    assert.ok(refusalFiles().includes(markerName));
    fs.writeFileSync(path.join(refDir(), `incomplete-${RUN}.json`), JSON.stringify({ version: 1, runId: RUN, branch: BRANCH, barePath: bare }) + "\n", { mode: 0o600 });
    assert.equal(await cleanup(coverOf(kept)), "removed");
    assert.deepEqual(refusalFiles(), [`incomplete-${RUN}.json`]);
  });
});

describe("issue4481 ledger keyed by generation and head", { timeout: 300_000 }, () => {
  it("generations 1 and 2 both refuse X: sealing and cleaning generation 1 keeps generation 2's row, which is then eligible", async () => {
    const kept = await atCap();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const X = await refuse("shared head");
    const cover1 = coverOf([...kept, X]);
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover1), "sealed");
    const ctx2 = { ...ctx, generation: 2 };
    await assert.rejects(cache["pinOwedUnderLock"](ctx2, X, "P4"), /owed candidate limit/);
    assert.deepEqual(readJson(ledgerName).entries.map((e: any) => [e.generation, e.sha]), [[1, X], [2, X]]);
    assert.ok(incomingRefs().some((r) => r.startsWith(`refs/uzi-incoming/${RUN}/1/refused-${X} `)));
    assert.ok(incomingRefs().some((r) => r.startsWith(`refs/uzi-incoming/${RUN}/2/refused-${X} `)));
    assert.equal(await cleanup(cover1), "removed");
    assert.deepEqual(readJson(ledgerName).entries, [{ generation: 2, sha: X, path: "P4" }]);
    assert.deepEqual(incomingRefs(), [`refs/uzi-incoming/${RUN}/2/refused-${X} ${X}`]);
    assert.equal(await cache.admitRefusalProvenance(ctx2), "written");
    const verdict = await gate(coverOf([...kept, X]), 2);
    assert.equal(verdict.kind, "eligible", JSON.stringify(verdict));
  });

  it("the same (generation, head) pair is still a single row; a malformed duplicate pair is rejected", async () => {
    const kept = await atCap();
    await cache.admitRefusalProvenance(ctx);
    const X = await refuse("dup");
    await assert.rejects(cache["pinOwedUnderLock"](ctx, X, "P4"), /owed candidate limit/);
    assert.equal(readJson(ledgerName).entries.length, 1);
    const good = readJson(ledgerName);
    writeRaw(ledgerName, { ...good, entries: [...good.entries, ...good.entries] });
    assert.equal((await gate(coverOf([...kept, X]))).kind, "ineligible");
  });
});

describe("issue4481 run-tagged refusal temps", { timeout: 300_000 }, () => {
  const OTHER_RUN = "44814481-aaaa-bbbb-cccc-555555555555";
  const UUID = "00000000-0000-4000-8000-000000000000";
  const drop = (name: string): void => { fs.writeFileSync(path.join(refDir(), name), "x", { mode: 0o600 }); };

  it("another run's stranded tagged temp does not block this run; its own and unattributed temps do", async () => {
    const { cover } = await eligibleState();
    drop(`.tmp-${OTHER_RUN}-${UUID}`);
    assert.equal((await gate(cover)).kind, "eligible", "run B's temp is ignored by run A");
    drop(`.tmp-short-${UUID}`);
    assert.equal((await gate(cover)).kind, "eligible", "a differently named run's temp is ignored too");
    fs.rmSync(path.join(refDir(), `.tmp-short-${UUID}`));
    drop(`.tmp-${RUN}-${UUID}`);
    assert.deepEqual(await gate(cover), { kind: "ineligible", reason: "temporary refusal metadata present" });
    fs.rmSync(path.join(refDir(), `.tmp-${RUN}-${UUID}`));
    for (const bad of [`.tmp-${UUID}`, ".tmp-garbage", `.tmp-${OTHER_RUN}-not-a-uuid`, `.tmp--${UUID}`, `.tmp-bad/run-${UUID}`.replace("/", "!")]) {
      drop(bad);
      assert.equal((await gate(cover)).kind, "ineligible", bad);
      fs.rmSync(path.join(refDir(), bad));
    }
    assert.equal((await gate(cover)).kind, "eligible");
  });

  it("the cleanup plan retains for this run's or an unattributed temp, and ignores another run's", async () => {
    const { cover } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    drop(`.tmp-${RUN}-${UUID}`);
    assert.equal(await cleanup(cover), "retained");
    fs.rmSync(path.join(refDir(), `.tmp-${RUN}-${UUID}`));
    drop(`.tmp-${UUID}`);
    assert.equal(await cleanup(cover), "retained");
    fs.rmSync(path.join(refDir(), `.tmp-${UUID}`));
    drop(`.tmp-${OTHER_RUN}-${UUID}`);
    assert.equal(await cleanup(cover), "removed");
    assert.deepEqual(refusalFiles(), [`.tmp-${OTHER_RUN}-${UUID}`, `admission-${RUN}-1.json`]);
  });

  it("refusal-directory writers tag their temp with the run; uzi-owed temps stay untagged", async () => {
    const renames: string[] = [];
    const fsp = (await import("node:fs/promises")).default;
    const original = fsp.rename;
    (fsp as { rename: typeof fsp.rename }).rename = (async (from: fs.PathLike, to: fs.PathLike) => {
      renames.push(`${path.basename(path.dirname(String(from)))}/${path.basename(String(from))}`);
      return original(from, to);
    }) as typeof fsp.rename;
    try { await rewriteTimes(CAP); await cache.admitRefusalProvenance(ctx); }
    finally { (fsp as { rename: typeof fsp.rename }).rename = original; }
    const refusal = renames.filter((r) => r.startsWith("uzi-refusals/"));
    const owed = renames.filter((r) => r.startsWith("uzi-owed/"));
    assert.ok(refusal.length >= 2, "the marker and the admission were written");
    for (const r of refusal) assert.match(r, new RegExp(`^uzi-refusals/\\.tmp-${RUN}-[0-9a-f-]{36}$`));
    assert.ok(owed.length > 0);
    for (const r of owed) assert.match(r, /^uzi-owed\/\.tmp-[0-9a-f-]{36}$/);
  });
});

describe("issue4481 refusal stage listing is cross-checked against loose refs", { timeout: 300_000 }, () => {
  const looseDir = (g = 1): string => path.join(bare, "refs", "uzi-incoming", RUN, String(g));
  const putLoose = (name: string, content: string, g = 1): string => {
    fs.mkdirSync(looseDir(g), { recursive: true });
    const p = path.join(looseDir(g), name);
    fs.writeFileSync(p, content);
    return p;
  };

  it("a broken loose stage file with an otherwise empty listing makes the gate ineligible", async () => {
    await rewriteTimes(2);
    assert.deepEqual(await gate(base), { kind: "none" });
    putLoose("garbage", "garbage\n");
    assert.equal((await gate(base)).kind, "ineligible");
  });

  it("a dangling symbolic or symlinked stage added to an eligible setup makes it ineligible; removing it restores eligibility", async () => {
    const { cover } = await eligibleState();
    assert.equal((await gate(cover)).kind, "eligible");
    const sym = putLoose("sym", "ref: refs/does/not/exist\n");
    assert.equal((await gate(cover)).kind, "ineligible");
    fs.rmSync(sym);
    fs.symlinkSync("/does/not/exist", path.join(looseDir(), "link"));
    assert.equal((await gate(cover)).kind, "ineligible");
    fs.rmSync(path.join(looseDir(), "link"));
    putLoose("extra", `${base}\n`);
    assert.equal((await gate(cover)).kind, "ineligible", "a well-formed loose stage that git lists but no ledger entry covers");
    fs.rmSync(path.join(looseDir(), "extra"));
    assert.equal((await gate(cover)).kind, "eligible");
  });

  /** Wrap the one git listing of the run's stage refs so a test can shape what git "said". */
  const stubListing = (shape: (out: { stdout: string; stderr: string }) => { stdout: string; stderr: string }): (() => void) => {
    const internals = cache as unknown as { execScoped: (cmd: string, args: string[], ...rest: unknown[]) => Promise<{ stdout: string; stderr: string }> };
    const original = internals.execScoped;
    internals.execScoped = async function (this: unknown, cmd, args, ...rest) {
      const out = await original.call(cache, cmd, args, ...rest);
      return Array.isArray(args) && args.includes("for-each-ref") && args.some((a) => String(a).startsWith(`refs/uzi-incoming/${RUN}/`)) ? shape(out) : out;
    };
    return () => { internals.execScoped = original; };
  };

  it("a stderr line from the stage listing alone makes the gate ineligible", async () => {
    const { cover } = await eligibleState();
    assert.equal((await gate(cover)).kind, "eligible");
    const restore = stubListing((out) => ({ stdout: out.stdout, stderr: "warning: ignoring broken ref\n" }));
    try { assert.equal((await gate(cover)).kind, "ineligible"); } finally { restore(); }
    assert.equal((await gate(cover)).kind, "eligible");
  });

  it("a listing oid that differs from the loose ref file alone makes the gate ineligible", async () => {
    const kept = await atCap();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    const h0 = await refuse("refused-a"), h1 = await refuse("refused-b");
    const cover = coverOf([...kept, h0, h1]);
    assert.equal((await gate(cover)).kind, "eligible");
    // Both heads stay ledgered and covered; only the listed oids are swapped against the files on disk.
    // Only import-stage lines get their oid field swapped; the refused-ref lines stay untouched.
    assert.ok(incomingRefs().some((l) => l.includes("/import-") && l.endsWith(` ${h0}`)), "an import stage at h0");
    assert.ok(incomingRefs().some((l) => l.includes("/import-") && l.endsWith(` ${h1}`)), "an import stage at h1");
    const swap = (line: string): string => {
      const i = line.lastIndexOf(" ");
      if (i < 0 || !line.slice(0, i).includes("/import-")) return line;
      const oid = line.slice(i + 1);
      return `${line.slice(0, i)} ${oid === h0 ? h1 : oid === h1 ? h0 : oid}`;
    };
    const restore = stubListing((out) => ({ stdout: out.stdout.split("\n").map(swap).join("\n"), stderr: out.stderr }));
    try { assert.equal((await gate(cover)).kind, "ineligible"); } finally { restore(); }
    assert.equal((await gate(cover)).kind, "eligible");
  });

  describe("only the files ref backend is trusted", () => {
    const configPath = (): string => path.join(bare, "config");
    const withConfig = async (extra: string, expected: "eligible" | "ineligible", formatVersion1 = false): Promise<void> => {
      const { cover } = await eligibleState();
      assert.equal((await gate(cover)).kind, "eligible");
      const original = fs.readFileSync(configPath());
      // Git only honours a v1 extension in a version 1 repository, so these config-file cases are git-level fail-closed
      // (git exits 128 on a reftable or valueless refstorage and the gate reads that as ineligible); they do not isolate the guard.
      fs.writeFileSync(configPath(), (formatVersion1 ? original.toString().replace("repositoryformatversion = 0", "repositoryformatversion = 1") : original.toString()) + extra);
      try { assert.equal((await gate(cover)).kind, expected); } finally { fs.writeFileSync(configPath(), original); }
      assert.equal((await gate(cover)).kind, "eligible");
    };

    // Real git rejects most refstorage configs itself, so the guard is isolated by reporting the value to it alone.
    // A reported empty value must be refused too: only an exact `files` is accepted.
    const reportingConfig = async (value: string, expected: "eligible" | "ineligible"): Promise<void> => {
      const { cover } = await eligibleState();
      const internals = cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
      const original = internals.runGit;
      internals.runGit = async function (cwd, args, ...rest) {
        const out = await original.call(cache, cwd, args, ...rest);
        return args[0] === "config" && args.includes("--list") ? `${out}extensions.refstorage\n${value}\0` : out;
      };
      try { assert.equal((await gate(cover)).kind, expected); } finally { internals.runGit = original; }
    };
    it("a reported non-files refstorage value is ineligible", () => reportingConfig("reftable", "ineligible"));
    it("a reported refstorage value of files is accepted", () => reportingConfig("files", "eligible"));
    it("a reported empty refstorage value is ineligible", () => reportingConfig("", "ineligible"));
    it("a reported refstorage value that only differs in case is ineligible", () => reportingConfig("FILES", "ineligible"));

    it("a reftable directory in the bare is ineligible", async () => {
      const { cover } = await eligibleState();
      fs.mkdirSync(path.join(bare, "reftable"));
      assert.equal((await gate(cover)).kind, "ineligible");
      fs.rmdirSync(path.join(bare, "reftable"));
      assert.equal((await gate(cover)).kind, "eligible");
    });
    it("git-level fail-closed: extensions.refstorage=reftable in the config file is ineligible", () => withConfig("[extensions]\n\trefstorage = reftable\n", "ineligible"));
    it("git-level fail-closed: a valueless extensions.refstorage is ineligible", () => withConfig("[extensions]\n\trefstorage\n", "ineligible"));
    it("an explicit extensions.refstorage=files is accepted (git itself rejects other spellings of the value)", () => withConfig("[extensions]\n\trefStorage = files\n", "eligible", true));
  });

  it("a broken loose stage makes the cleanup plan retain", async () => {
    const { cover } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const garbage = putLoose("garbage", "garbage\n");
    const before = evidence() + JSON.stringify(pinRefs());
    assert.equal(await cleanup(cover), "retained");
    assert.equal(evidence() + JSON.stringify(pinRefs()), before);
    fs.rmSync(garbage);
    assert.equal(await cleanup(cover), "removed");
  });
});

describe("issue4481 sealed cleanup registers its stage deletions with the authority", { timeout: 300_000 }, () => {
  const hooks = (events: string[], refuse = false) => ({
    admitSourceUse: async () => undefined,
    assertBareClear: async () => undefined,
    beginDestructive: async () => ({ settle: async () => undefined }),
    beginBareMutation: async (_b: string, op: string, footprint: readonly string[]) => {
      if (refuse && op === "refusal-cleanup-delete-ref") throw new Error("authority refused");
      events.push(`bare:${op}:${footprint.join(",")}`);
      return { settle: async (outcome: unknown) => { events.push(`settle:${op}:${JSON.stringify(outcome)}`); } };
    },
  });

  it("each stage deletion is registered with its ref footprint and settled ok", async () => {
    const { cover } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const stages = incomingRefs().map((l) => l.split(" ")[0]!).sort();
    assert.equal(stages.length, 2);
    const events: string[] = [];
    const hooked = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ maxOwedCandidates: CAP, authority: hooks(events) as never }));
    assert.equal(await hooked.cleanupRecoveryGeneration(bare, ctx, await roots(), [], [], cover, undefined, "archive"), "removed");
    const mine = events.filter((e) => e.includes("refusal-cleanup-delete-ref"));
    assert.deepEqual(mine.filter((e) => e.startsWith("bare:")).sort(), stages.map((s) => `bare:refusal-cleanup-delete-ref:${s}`).sort());
    assert.equal(mine.filter((e) => e === 'settle:refusal-cleanup-delete-ref:{"ok":true}').length, 2);
    for (let i = 0; i < mine.length; i += 2) {
      assert.match(mine[i]!, /^bare:/);
      assert.equal(mine[i + 1], 'settle:refusal-cleanup-delete-ref:{"ok":true}');
    }
    assert.deepEqual(incomingRefs(), []);
  });

  it("a refused registration deletes nothing", async () => {
    const { cover } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, cover), "sealed");
    const before = incomingRefs();
    const hooked = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ maxOwedCandidates: CAP, authority: hooks([], true) as never }));
    await assert.rejects(hooked.cleanupRecoveryGeneration(bare, ctx, await roots(), [], [], cover, undefined, "archive"), /authority refused/);
    assert.deepEqual(incomingRefs(), before);
  });
});

describe("issue4481 a failed import deletes its own attributed stage", { timeout: 300_000 }, () => {
  type Internals = {
    requireOwedCommit: (barePath: string, sha: string) => Promise<void>;
    runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string>;
  };
  const runIncoming = (): string[] => incomingRefs();

  it("an expected-candidate mismatch at a positive generation leaves no attributed stage, and the gate keeps its normal decision", async () => {
    const first = commitTree("first", true);
    assert.equal((await fetchHead()).kind, "updated");
    commitTree("newer", true);
    await assert.rejects(fetchHead({ ...opts, expectedCandidateSha: first }), /checkpoint candidate changed during acquisition/);
    assert.deepEqual(runIncoming(), [], "the stage of the failed import is gone");
    assert.deepEqual(await gate(first), { kind: "none" });
  });

  it("a flat (null generation) import keeps its unattributed stage after the same failure", async () => {
    const legacy: FetchAgentBranchOptions = { context: { ...ctx, generation: null, legacy: true } as FetchAgentBranchOptions["context"] };
    const first = commitTree("legacy first", true);
    assert.equal((await fetchHead(legacy)).kind, "updated");
    const newer = commitTree("legacy newer", true);
    await assert.rejects(fetchHead({ ...legacy, expectedCandidateSha: first }), /checkpoint candidate changed/);
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"]), newer);
  });

  it("an owed-commit check that fails after the fetch leaves no attributed stage", async () => {
    commitTree("first", true);
    assert.equal((await fetchHead()).kind, "updated");
    const head = commitTree("poisoned", true);
    const internals = cache as unknown as Internals;
    const original = internals.requireOwedCommit.bind(cache);
    internals.requireOwedCommit = async (b, sha) => { if (sha === head) throw new Error("injected owed-commit failure"); return original(b, sha); };
    try { await assert.rejects(fetchHead(), /injected owed-commit failure/); }
    finally { internals.requireOwedCommit = original; }
    assert.deepEqual(runIncoming(), []);
  });

  it("a stage that is the ledgered head's sole anchor survives the failure", async () => {
    const first = commitTree("first", true);
    assert.equal((await fetchHead()).kind, "updated");
    const newer = commitTree("newer", true);
    fs.mkdirSync(refDir(), { recursive: true, mode: 0o700 });
    writeRaw(ledgerName, { version: 1, runId: RUN, branch: BRANCH, barePath: bare, entries: [{ generation: 1, sha: newer, path: "P2" }] });
    await assert.rejects(fetchHead({ ...opts, expectedCandidateSha: first }), /checkpoint candidate changed/);
    assert.equal(runIncoming().length, 1, "no refused ref and no pin anchor the head, so the stage stays");
    assert.ok(runIncoming()[0]!.endsWith(` ${newer}`));
  });

  it("a ledgered head that a refused ref anchors does not need its stage", async () => {
    const first = commitTree("first", true);
    assert.equal((await fetchHead()).kind, "updated");
    const newer = commitTree("newer", true);
    fs.mkdirSync(refDir(), { recursive: true, mode: 0o700 });
    writeRaw(ledgerName, { version: 1, runId: RUN, branch: BRANCH, barePath: bare, entries: [{ generation: 1, sha: newer, path: "P2" }] });
    gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, `+refs/heads/${BRANCH}:refs/uzi-incoming/${RUN}/1/refused-${newer}`]);
    await assert.rejects(fetchHead({ ...opts, expectedCandidateSha: first }), /checkpoint candidate changed/);
    assert.deepEqual(runIncoming(), [`refs/uzi-incoming/${RUN}/1/refused-${newer} ${newer}`]);
  });

  describe("a producing-source import", () => {
    const SBRANCH = "agent/issue-3747", SRUN = "run-3747";
    const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
    let sbare: string, sclone: string, sbase: string, sopts: FetchAgentBranchOptions;
    const sincoming = (): string[] => gitIn(sbare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/"]).split("\n").filter(Boolean);
    const scommit = (label: string): string => {
      const sha = gitIn(sclone, [...IDENT, "commit-tree", `${sbase}^{tree}`, "-p", sbase, "-m", label]);
      gitIn(sclone, ["update-ref", `refs/heads/${SBRANCH}`, sha]);
      return sha;
    };
    beforeEach(async () => {
      sbare = bare;
      sclone = (await cache.runnerCloneForBranch(sbare, SBRANCH, "issue-3747", noProofReseed, SRUN, false, undefined,
        { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} })).path;
      await cache.markRecoveryCapture(sbare, sclone, SBRANCH, SRUN, ATTEMPT);
      sbase = base;
      const sctx: PositiveOwedCandidateContext = { runId: SRUN, generation: 1, kind: "issue", branch: SBRANCH, barePath: sbare,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: sbase } };
      sopts = { context: sctx, producingSource: { runId: SRUN, clonePath: sclone, attemptId: ATTEMPT } };
      scommit("first");
      assert.equal((await cache.fetchAgentBranch(sbare, sclone, SBRANCH, SRUN, sopts)).kind, "updated");
    });
    const racing = (phase2: () => void): FetchAgentBranchOptions => {
      let phases = 0;
      return { ...sopts, withSourceAuthority: async <T>(action: () => Promise<T>) => { if (++phases === 2) phase2(); return action(); } };
    };

    it("a stage moved between acquisition and promotion is retained at its new tip", async () => {
      scommit("next head");
      const moved = racing(() => {
        const [line] = sincoming() as [string];
        gitIn(sbare, ["update-ref", line.split(" ")[0]!, sbase, line.split(" ")[1]!]);
      });
      await assert.rejects(cache.fetchAgentBranch(sbare, sclone, SBRANCH, SRUN, moved), /private stage changed/);
      const left = sincoming();
      assert.equal(left.length, 1, "the CAS to the acquired tip failed, so the moved stage stays");
      assert.ok(left[0]!.endsWith(` ${sbase}`));
    });

    it("a failed owed-commit check in the second lock section deletes the stage", async () => {
      const head = scommit("next head");
      const internals = cache as unknown as Internals;
      const original = internals.requireOwedCommit.bind(cache);
      let armed = false;
      internals.requireOwedCommit = async (b, sha) => { if (armed && sha === head) throw new Error("injected owed-commit failure"); return original(b, sha); };
      try {
        await assert.rejects(cache.fetchAgentBranch(sbare, sclone, SBRANCH, SRUN, racing(() => { armed = true; })), /injected owed-commit failure/);
      } finally { internals.requireOwedCommit = original; }
      assert.deepEqual(sincoming(), []);
    });

    it("the real import is a live import between its two lock sections and not after it completes", async () => {
      scommit("next head");
      const gateAt = (): Promise<RefusalReleaseGate> => cache.refusalReleaseGate(sbare, SRUN, 1, sbase);
      const finished = (cache as unknown as { finishedImportStages: Set<string> }).finishedImportStages;
      const finishedBefore = [...finished];
      let between: RefusalReleaseGate | undefined;
      let phases = 0;
      const observing: FetchAgentBranchOptions = { ...sopts, withSourceAuthority: async <T>(action: () => Promise<T>) => {
        if (++phases === 2) between = await gateAt();
        return action();
      } };
      assert.equal((await cache.fetchAgentBranch(sbare, sclone, SBRANCH, SRUN, observing)).kind, "updated");
      assert.deepEqual(between, { kind: "ineligible", reason: "refusal import in progress" });
      const live = (cache as unknown as { liveImportStages: Set<string> }).liveImportStages;
      assert.equal(live.size, 0, "the entry is removed when the import completes");
      // Asserted before the gate runs: the gate itself reconciles a finished import stage, so after it a surviving stage would be gone.
      assert.deepEqual(sincoming(), [], "the completed import's stage is deleted by the import, not by the gate");
      assert.deepEqual([...finished], finishedBefore, "a stage the import deleted is not recorded as a finished producer");
      assert.deepEqual(await gateAt(), { kind: "none" });
    });

    it("a boundary stop after the fetch leaves the attributed stage in place, unlike an ordinary failure", async () => {
      const head = scommit("next head");
      const internals = cache as unknown as Internals;
      const original = internals.requireOwedCommit.bind(cache);
      try {
        internals.requireOwedCommit = async (b, sha) => { if (sha === head) throw new CheckpointSoftDeadlineError(); return original(b, sha); };
        await assert.rejects(cache.fetchAgentBranch(sbare, sclone, SBRANCH, SRUN, sopts), (e: unknown) => e instanceof CheckpointSoftDeadlineError);
      } finally { internals.requireOwedCommit = original; }
      assert.deepEqual(sincoming().map((l) => l.split(" ")[1]), [head], "the soft stop keeps the stage");
    });
  });
});

describe("issue4481 the incoming head's refusal is recorded before a soft-deadline return", { timeout: 300_000 }, () => {
  const realSpawner: BoundaryProcessSpawner = async (request) => {
    const child = (await import("node:child_process")).spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code) => resolve({ code: code ?? -1 }));
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGTERM"); await completed; } };
  };

  it("a displaced-head refusal with an expired soft deadline still ledgers the incoming head at its generation", async () => {
    const big = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP + 1 }));
    const kept: string[] = [];
    for (let i = 0; i < CAP + 1; i++) {
      kept.push(commitTree("rig", true));
      assert.equal((await big.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
    }
    const tracking = kept[kept.length - 1]!;
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${tracking}`]);
    assert.equal(await cache.admitRefusalProvenance({ ...ctx, generation: 2 }), "written");
    const next = commitTree("displacer", true);
    // The soft deadline expires once the displaced head's refused ref is written, inside the masked chokepoint.
    let clock = 0;
    const internals = cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
    const original = internals.runGit.bind(cache);
    internals.runGit = async (cwd, args, ...rest) => {
      const out = await original(cwd, args, ...rest);
      if (args[0] === "update-ref" && String(args[1]).includes(`/refused-${tracking}`)) clock = 100;
      return out;
    };
    let result: TrackingUpdateResult;
    try {
      result = await cache.withBoundaryProcessSpawner(realSpawner, new AbortController().signal,
        () => cache.fetchAgentBranch(bare, clone, BRANCH, RUN, withContext(2)),
        { softSignal: new AbortController().signal, softDeadlineAt: 100, now: () => clock });
    } catch (cause) {
      assert.ok(!(cause instanceof CheckpointSoftDeadlineError) || clock === 100, String(cause));
      throw cause;
    } finally { internals.runGit = original; }
    assert.equal(clock, 100, "the deadline did expire during the import");
    assert.deepEqual(result, { kind: "not_updated", reason: "owed_limit", refusedSha: next });
    const entries = readJson(ledgerName).entries;
    assert.deepEqual(entries.filter((e: any) => e.sha === tracking).map((e: any) => e.generation), [1]);
    assert.deepEqual(entries.filter((e: any) => e.sha === next).map((e: any) => [e.generation, e.path]), [[2, "P2"]],
      "the incoming head is ledgered at its own generation before the soft-deadline return");
    assert.ok(incomingRefs().includes(`refs/uzi-incoming/${RUN}/2/refused-${next} ${next}`));
  });
});

describe("issue4481 reconciling proven-finished, independently anchored import stages", { timeout: 300_000 }, () => {
  const STAGE_RE = /\/import-/;
  type Internals = { requireOwedCommit: (barePath: string, sha: string) => Promise<void> };
  /** Below the cap: an import interrupted after its stage was fetched, then a successful retry of the same head.
   *  Tracking at the head is the only anchor left (every owed pin is removed). */
  async function interruptedThenRetried(c: GitCache = cache): Promise<{ head: string; stage: string }> {
    const first = commitTree("first", true);
    assert.equal((await c.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
    const head = commitTree("interrupted", true);
    const internals = c as unknown as Internals;
    const original = internals.requireOwedCommit.bind(c);
    let armed = true;
    internals.requireOwedCommit = async (b, sha) => {
      if (armed && sha === head) { armed = false; throw new CheckpointSoftDeadlineError(); }
      return original(b, sha);
    };
    try { await assert.rejects(c.fetchAgentBranch(bare, clone, BRANCH, RUN, opts), (e: unknown) => e instanceof CheckpointSoftDeadlineError); }
    finally { internals.requireOwedCommit = original; }
    assert.equal(incomingRefs().length, 1, "the interrupted import left its attributed stage");
    assert.equal((await c.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
    for (const pin of pinRefs()) gitIn(bare, ["update-ref", "-d", pin]);
    const [stage] = importStageRefs() as [string];
    assert.equal(incomingRefs().length, 1, "only the interrupted stage is left");
    assert.ok(stage && first);
    return { head, stage };
  }
  const stageLeft = (stage: string): boolean => gitIn(bare, ["for-each-ref", stage]) !== "";
  const UNPROVEN = { kind: "ineligible", reason: "refusal import stage producer unproven" } as const;
  const NOT_LEDGERED = { kind: "ineligible", reason: "refusal stage is not in the ledger" } as const;

  it("regression: the leftover stage of an interrupted below-cap import is reconciled when tracking is the sole anchor", async () => {
    const { head, stage } = await interruptedThenRetried();
    assert.deepEqual(await gate(head, 1, cache, BRANCH), { kind: "none" });
    assert.equal(stageLeft(stage), false, "the stage is gone");
    assert.equal(finishedSet().has(stage), false);
  });

  describe("negative controls keep the stage and give an exact verdict", () => {
    it("a live producer", async () => {
      const { head, stage } = await interruptedThenRetried();
      (cache as unknown as { liveImportStages: Set<string> }).liveImportStages.add(stage);
      assert.deepEqual(await gate(head, 1, cache, BRANCH), { kind: "ineligible", reason: "refusal import in progress" });
      assert.equal(stageLeft(stage), true);
    });

    it("a foreign nonce", async () => {
      const { head, stage } = await interruptedThenRetried();
      gitIn(bare, ["update-ref", "-d", stage]);
      const foreign = foreignImportStage(head);
      finishedSet().add(foreign);
      assert.deepEqual(await gate(head, 1, cache, BRANCH), UNPROVEN);
      assert.equal(stageLeft(foreign), true);
    });

    it("a stage this process never recorded as finished", async () => {
      const { head, stage } = await interruptedThenRetried();
      finishedSet().delete(stage);
      assert.deepEqual(await gate(head, 1, cache, BRANCH), UNPROVEN);
      assert.equal(stageLeft(stage), true);
    });

    it("importProducerProven is false for a finished stage that is also live", async () => {
      const { stage } = await interruptedThenRetried();
      const proven = (c: GitCache, ref: string): boolean => (c as unknown as { importProducerProven(r: string): boolean }).importProducerProven(ref);
      assert.equal(finishedSet().has(stage), true);
      assert.equal(proven(cache, stage), true, "control: finished and not live is proven");
      (cache as unknown as { liveImportStages: Set<string> }).liveImportStages.add(stage);
      assert.equal(proven(cache, stage), false);
    });

    it("a sole anchor: tracking moved away, no pins", async () => {
      const { stage } = await interruptedThenRetried();
      assert.equal(await cache.admitRefusalProvenance(ctx), "written");
      const next = commitTree("moved on", true);
      assert.equal((await fetchHead()).kind, "updated");
      for (const pin of pinRefs()) gitIn(bare, ["update-ref", "-d", pin]);
      assert.deepEqual(await gate(next, 1, cache, BRANCH), NOT_LEDGERED);
      assert.equal(stageLeft(stage), true);
    });

    it("an uncovered tip", async () => {
      const { stage } = await interruptedThenRetried();
      assert.equal(await cache.admitRefusalProvenance(ctx), "written");
      assert.deepEqual(await gate(base, 1, cache, BRANCH), NOT_LEDGERED);
      assert.equal(stageLeft(stage), true);
    });

    it("no branch, and a wrong branch", async () => {
      const { head, stage } = await interruptedThenRetried();
      assert.equal(await cache.admitRefusalProvenance(ctx), "written");
      assert.deepEqual(await gate(head), NOT_LEDGERED);
      assert.deepEqual(await gate(head, 1, cache, "agent/issue-9999"), NOT_LEDGERED);
      assert.equal(stageLeft(stage), true);
    });

    it("a ledger with a different branch than the hint", async () => {
      const { head, stage } = await interruptedThenRetried();
      assert.equal(await cache.admitRefusalProvenance(ctx), "written");
      fs.mkdirSync(refDir(), { recursive: true, mode: 0o700 });
      writeRaw(ledgerName, { version: 1, runId: RUN, branch: "agent/other", barePath: bare, entries: [{ generation: 1, sha: base, path: "P2" }] });
      assert.equal((await gate(head, 1, cache, BRANCH)).kind, "ineligible");
      assert.equal(stageLeft(stage), true);
    });
  });

  describe("with an exact refused ref, a ledger row, coverage and complete admissions", () => {
    it("a foreign-nonce import stage holds the gate", async () => {
      const { cover, refused } = await eligibleState();
      assert.equal((await gate(cover)).kind, "eligible");
      foreignImportStage(refused);
      assert.deepEqual(await gate(cover), UNPROVEN);
    });

    it("a this-process stage absent from the finished set holds the gate", async () => {
      const { cover } = await eligibleState();
      const [stage] = importStageRefs() as [string];
      assert.ok(finishedSet().has(stage), "the real import recorded its stage as finished");
      finishedSet().delete(stage);
      assert.deepEqual(await gate(cover), UNPROVEN);
      finishedSet().add(stage);
      assert.equal((await gate(cover)).kind, "eligible");
    });

    it("a real same-process at-cap refusal stays eligible and keeps its stages", async () => {
      const { cover, refused } = await eligibleState();
      const before = incomingRefs();
      const verdict = await gate(cover, 1, cache, BRANCH);
      assert.equal(verdict.kind, "eligible", JSON.stringify(verdict));
      assert.deepEqual(incomingRefs(), before);
      assert.ok(before.some((l) => STAGE_RE.test(l) && l.endsWith(` ${refused}`)));
    });
  });

  it("positive control: an owed-pin anchor at the exact tip reconciles without a branch hint", async () => {
    const { head, stage } = await interruptedThenRetried();
    gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${head}`, head]);
    assert.deepEqual(await gate(head), { kind: "none" });
    assert.equal(stageLeft(stage), false);
  });

  it("positive control: a recovery pin at a generation <= G anchors; one above G does not", async () => {
    const { head, stage } = await interruptedThenRetried();
    assert.equal(await cache.admitRefusalProvenance(ctx), "written");
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/2`, head]);
    assert.deepEqual(await gate(head), NOT_LEDGERED);
    assert.equal(stageLeft(stage), true);
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/1`, head]);
    assert.deepEqual(await gate(head), { kind: "none" });
    assert.equal(stageLeft(stage), false);
  });

  describe("authority ordering", () => {
    const hooks = (events: string[], probe: { refPresent: (ref: string) => boolean; ref: string }, refuse = false) => ({
      admitSourceUse: async () => undefined,
      assertBareClear: async () => undefined,
      beginDestructive: async () => ({ settle: async () => undefined }),
      beginBareMutation: async (_b: string, op: string, footprint: readonly string[]) => {
        if (op !== "refusal-cleanup-delete-ref") return { settle: async () => undefined };
        if (refuse) throw new Error("authority refused");
        events.push(`register:${footprint.join(",")}:stage-present=${probe.refPresent(footprint[0]!)}`);
        return { settle: async (outcome: unknown) => { events.push(`settle:${JSON.stringify(outcome)}:stage-present=${probe.refPresent(probe.ref)}`); } };
      },
    });
    const probe = { refPresent: stageLeft, ref: "" };

    it("registration precedes the delete child and the settlement follows the read-back", async () => {
      const events: string[] = [];
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP,
        authority: hooks(events, probe) as never }));
      const { head, stage } = await interruptedThenRetried(hooked);
      probe.ref = stage;
      assert.deepEqual(events, [], "the imports themselves registered no stage deletion");
      assert.deepEqual(await hooked.refusalReleaseGate(bare, RUN, 1, head, BRANCH), { kind: "none" });
      assert.deepEqual(events, [`register:${stage}:stage-present=true`, 'settle:{"ok":true}:stage-present=false']);
      assert.equal(stageLeft(stage), false);
    });

    it("a refused registration keeps the stage", async () => {
      const hooked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP,
        authority: hooks([], probe, true) as never }));
      const { head, stage } = await interruptedThenRetried(hooked);
      assert.equal(await hooked.admitRefusalProvenance(ctx), "written");
      assert.deepEqual(await hooked.refusalReleaseGate(bare, RUN, 1, head, BRANCH), NOT_LEDGERED);
      assert.equal(stageLeft(stage), true);
    });
  });
});

describe("issue4481 at-cap duplicate stage release requires a verified refused ref", { timeout: 300_000 }, () => {
  const stubRefusedRefWrite = (): (() => void) => {
    const c = cache as unknown as { runGit: (dir: string, args: string[]) => Promise<string> };
    const original = c.runGit;
    c.runGit = function (this: unknown, dir: string, args: string[]) {
      if (args[0] === "update-ref" && args.length === 3 && /\/refused-[0-9a-f]{40}$/.test(args[1]!)) return Promise.reject(new Error("injected refused ref write failure"));
      return original.call(this, dir, args);
    } as typeof c.runGit;
    return () => { c.runGit = original; };
  };

  it("regression: a duplicate of a ledgered head whose refused ref is missing and cannot be rewritten keeps its stage", async () => {
    await atCap();
    const refused = await refuse();
    const first = importStageRefs();
    assert.equal(first.length, 1);
    gitIn(bare, ["update-ref", "-d", `refs/uzi-incoming/${RUN}/1/refused-${refused}`]);
    const restore = stubRefusedRefWrite();
    try {
      assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit", refusedSha: refused });
    } finally { restore(); }
    assert.equal(importStageRefs().length, 2, "the duplicate's stage is kept: no verified refused ref anchors it");
  });

  it("positive control: a duplicate with a verified refused ref deletes its stage", async () => {
    await atCap();
    const refused = await refuse();
    const first = importStageRefs();
    assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit", refusedSha: refused });
    assert.deepEqual(importStageRefs(), first);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-incoming/${RUN}/1/refused-${refused}`]), refused);
  });
});

describe("issue4481 retainCurrentOwedCandidate is fenced by the seal of the generation it would pin", { timeout: 300_000 }, () => {
  const dropTrackingPin = (kept: string[]): void => {
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${kept[kept.length - 1]}`]);
  };

  it("a post-seal retain makes no pin and no candidate file", async () => {
    const { kept, refused } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, coverOf([...kept, refused])), "sealed");
    assert.ok(refusalFiles().includes(`final-${RUN}-1.json`));
    dropTrackingPin(kept);
    const pins = pinRefs(), files = filesSnapshot(), owed = owedSnapshot();
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(pinRefs(), pins);
    assert.equal(filesSnapshot(), files);
    assert.equal(owedSnapshot(), owed, "no candidate or context metadata is written");
  });

  it("a generation-2 caller observing a generation-1 head is refused by generation 1's seal", async () => {
    const { kept, refused } = await eligibleState();
    assert.equal(await cache.sealRefusalRelease(bare, RUN, 1, coverOf([...kept, refused])), "sealed");
    dropTrackingPin(kept);
    const pins = pinRefs(), files = filesSnapshot(), owed = owedSnapshot();
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, withContext(2)), { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(pinRefs(), pins);
    assert.equal(filesSnapshot(), files);
    assert.equal(owedSnapshot(), owed, "no candidate or context metadata is written");
  });

  it("pre-freeze control: the in-process fence alone does not stop P7", async () => {
    const { kept } = await eligibleState();
    dropTrackingPin(kept);
    cache.closeRefusalGeneration(RUN, 1);
    const pins = pinRefs();
    const result = await cache.retainCurrentOwedCandidate(bare, opts);
    assert.equal(result.kind, "updated");
    assert.equal(pinRefs().length, pins.length + 1, "the tracking head is pinned again");
  });
});
