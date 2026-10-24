import { beforeEach, afterEach, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { GitCache, type PositiveOwedCandidateContext } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking, fixtureTrackingOptions } from "./runner-tracking-fixture.js";
import { TickSpawner } from "../src/tick-spawner.js";

/** Capture through the production boundary and settle every owned child, including on refusal. */
async function captureRecovered(cache: GitCache, clone: { path: string; baseCommit: string }, bare: string): Promise<string> {
  const controller = new AbortController();
  const owner = new TickSpawner({ signal: controller.signal });
  try {
    return (await cache.withBoundaryProcessSpawner(owner.spawn, controller.signal,
      () => cache.capturePlanningDiff(clone.path, clone.baseCommit, bare))).toString("utf8");
  } finally {
    controller.abort();
    await owner.settled();
    assert.deepEqual(owner.survivors(), []);
  }
}

/** Independent expected bytes for the single-line new files used by these lifecycle fixtures. */
function addedFilePatch(file: string, line: string): string {
  const content = line + "\n";
  const oid = createHash("sha1").update("blob " + Buffer.byteLength(content) + "\0").update(content).digest("hex").slice(0, 7);
  return `diff --git b/${file} b/${file}\nnew file mode 100644\nindex 0000000..${oid}\n--- /dev/null\n+++ b/${file}\n@@ -0,0 +1 @@\n+${line}\n`;
}


// issue #3239 M3: a retained-recovery successor must never adopt a `wip(park):` marker that is buried
// under a recovery-coverage wrapper. Real git fixtures; genuine wrappers come from buildRecoveryCoverage.

const branch = "agent/issue-2512";
const key = "issue-2512";
const runId = "run-2512";
const aid = (n: number) => `20261008T171401Z-g${n}-0123456789abcdef`;
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"];
let fx: Fixture;
let cache: GitCache;
let bare: string;
let source: { runId: string; clonePath: string };

function git(dir: string, args: string[], env: NodeJS.ProcessEnv = {}, input?: string): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", input, env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", ...env },
    stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}
function journal(): any { return JSON.parse(git(bare, ["config", `uzi-recovery.${branch}.clone`])); }
function writeJournal(value: unknown): void { git(bare, ["config", `uzi-recovery.${branch}.clone`, JSON.stringify(value)]); }
function ledger(): string {
  try { return git(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]); } catch { return ""; }
}
const sha = (rev: string, dir = bare) => git(dir, ["rev-parse", rev]);
function ctx(): Promise<PositiveOwedCandidateContext> {
  return fixtureTrackingOptions(cache, bare, branch, runId).then(o => o.context as PositiveOwedCandidateContext);
}
async function genuine(roots: string[], current: string) {
  return cache.buildRecoveryCoverage(bare, await ctx(), roots, current);
}
/** A WIP marker made by the real commitWipMarker in `dir`, then mirrored into the tracking ref. */
async function wipMarker(dir: string, file: string): Promise<string> {
  fs.writeFileSync(path.join(dir, file), `${file}\n`);
  git(dir, ["add", file]);
  assert.equal(await cache.commitWipMarker(dir), true);
  await fixtureFetchTracking(cache, bare, dir, branch, runId);
  return sha("HEAD", dir);
}
function sibling(parent: string, message: string): string {
  return git(bare, [...IDENT, "commit-tree", `${parent}^{tree}`, "-p", parent, "-m", message]);
}
function lookalike(marker: string, o: { author?: string; committer?: string; date?: string; fp?: string; tree?: string; pin: boolean }): string {
  const realTree = sha(`${marker}^{tree}`);
  const real = createHash("sha256").update(JSON.stringify({ roots: [marker], currentSha: marker, tree: realTree })).digest("hex");
  const fp = o.fp ?? real;
  const date = o.date ?? "2000-01-01T00:00:00Z";
  const author = o.author ?? "uzi-agent";
  const committer = o.committer ?? "uzi-agent";
  const wrapper = git(bare, ["--no-replace-objects", "-c", "commit.gpgsign=false", "commit-tree", o.tree ?? realTree, "-p", marker,
    "-m", `uzi recovery coverage ${fp} round 0 batch 0`], {
    GIT_AUTHOR_NAME: author, GIT_AUTHOR_EMAIL: "uzi-agent@uzi.local", GIT_COMMITTER_NAME: committer,
    GIT_COMMITTER_EMAIL: "uzi-agent@uzi.local", GIT_AUTHOR_DATE: date, GIT_COMMITTER_DATE: date,
  });
  if (o.pin) git(bare, ["update-ref", `refs/uzi-coverage/${runId}/1/${fp}`, wrapper]);
  return wrapper;
}
/** A commit whose message is the raw bytes `msg`. It is written with `hash-object --literally` because
 *  `commit-tree` re-encodes a non-UTF-8 message to UTF-8. */
function rawCommit(parent: string, msg: Buffer, tree = sha(`${parent}^{tree}`)): string {
  const who = "t <t@example.test> 946684800 +0000";
  const body = Buffer.concat([Buffer.from(`tree ${tree}\nparent ${parent}\nauthor ${who}\ncommitter ${who}\n\n`), msg]);
  return execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--literally", "--stdin"], {
    input: body, encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}
/** Assert a refused successor: nothing written, no clone, source untouched. */
async function assertRefused(attempts: number, attempt: string): Promise<void> {
  const head = sha("HEAD", source.clonePath);
  const before = journal();
  const ledgerBefore = ledger();
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, source, attempts, attempt),
    /successor recovery verification failed/);
  assert.equal(journal().recovery.stage, "captured");
  assert.deepEqual(journal(), before, "the journal is unchanged");
  assert.equal(ledger(), ledgerBefore, "no attempt ledger entry");
  assert.ok(!ledger().includes(attempt));
  assert.ok(!fs.readdirSync(path.dirname(source.clonePath)).some(n => n.includes(attempt)), "no successor clone dir");
  assert.equal(sha("HEAD", source.clonePath), head, "the source clone is unchanged");
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId);
  source = { runId, clonePath: clone.path };
  await cache.markRecoveryCapture(bare, clone.path, branch, runId);
});
afterEach(() => fx.cleanup());

test("issue #3239 (a): a genuine wrapper over a WIP marker adopts the marker's content uncommitted, with neither in history", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  const c = (await genuine([m], m)).sha;
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, m);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, r.attempts, aid(2));
  assert.equal(sha("HEAD", successor.path), sha(`${m}^`));
  assert.equal(successor.baseCommit, sha(`${m}^`));
  assert.equal(await captureRecovered(cache, successor, bare), addedFilePatch("work.txt", "work.txt"));
  assert.equal(git(successor.path, ["status", "--porcelain"]), "A  work.txt");
  const history = git(successor.path, ["rev-list", "HEAD"]).split("\n");
  assert.ok(!history.includes(m) && !history.includes(c), "neither the marker nor the wrapper is in history");
  assert.equal(successor.wipRecovered, true);
  assert.equal(successor.adoptedSource, m);
  assert.equal(successor.adoptedContentSource, m);
  assert.deepEqual(journal().peeledMarkers, [m]);
  assert.equal(journal().recovery.stage, "ready-for-model");
});

for (const [name, opts] of [
  ["no pin", { pin: false }],
  ["wrong author only", { pin: true, author: "mallory" }],
  ["wrong committer only", { pin: true, committer: "mallory" }],
  ["wrong date", { pin: true, date: "2000-01-02T00:00:00Z" }],
  ["wrong fingerprint", { pin: true, fp: createHash("sha256").update("other").digest("hex") }],
] as const) {
  test(`issue #3239 (b): a look-alike wrapper (${name}) over a marker is refused and keeps the source`, async () => {
    const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
    const m = await wipMarker(source.clonePath, "work.txt");
    const c = lookalike(m, opts);
    await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, m);
    await assertRefused(r.attempts, aid(2));
  });
}

test("issue #3239 (c): a leaf of committed work outside the marker's history is refused", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  const outside = sibling(sha(`${m}^`), "committed work elsewhere");
  const c = (await genuine([m, outside], m)).sha;
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, m);
  await assertRefused(r.attempts, aid(2));
});

test("issue #3239 (d): a genuine wrapper over a clean milestone still adopts the wrapper and keeps the content source", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  fs.writeFileSync(path.join(source.clonePath, "work.txt"), "milestone\n");
  git(source.clonePath, ["add", "work.txt"]);
  git(source.clonePath, [...IDENT, "commit", "-m", "milestone"]);
  await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
  const t = sha("HEAD", source.clonePath);
  const c = (await genuine([t], t)).sha;
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, t);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, r.attempts, aid(2));
  assert.equal(successor.adoptedSource, c);
  assert.equal(successor.adoptedContentSource, t);
  assert.equal(sha("HEAD", successor.path), c);
  assert.equal(journal().peeledMarkers, undefined);
});

/** Episode 1 peels M1 into the attempt A2, whose own WIP marker M2 sits on M1's parent. */
async function secondEpisode() {
  const r1 = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m1 = await wipMarker(source.clonePath, "one.txt");
  await cache.recordRecoveryCapture(bare, branch, key, source, r1.attempts, (await genuine([m1], m1)).sha, m1);
  const a2 = await cache.prepareRecoverySuccessor(bare, branch, key, source, r1.attempts, aid(2));
  const expected = { runId, clonePath: a2.path, attemptId: aid(2) };
  const r2 = await cache.reserveRecoveryIteration(bare, branch, key, expected);
  const m2 = await wipMarker(a2.path, "two.txt");
  assert.equal(sha(`${m2}^`), sha(`${m1}^`), "sanity: the second marker sits on the first marker's parent");
  const c2 = (await genuine([m1, m2], m2)).sha;
  await cache.recordRecoveryCapture(bare, branch, key, expected, r2.attempts, c2, m2);
  return { m1, m2, c2, expected, attempts: r2.attempts };
}

test("issue #3239 (e): a lineage-proven superseded marker leaf is accepted and the lineage carries forward", async () => {
  const { m1, m2, c2, expected, attempts } = await secondEpisode();
  assert.deepEqual(journal().peeledMarkers, [m1], "sanity: episode 1 journaled the peeled marker on A2");
  const a3 = await cache.prepareRecoverySuccessor(bare, branch, key, expected, attempts, aid(3));
  assert.equal(sha("HEAD", a3.path), sha(`${m1}^`));
  const history = git(a3.path, ["rev-list", "HEAD"]).split("\n");
  for (const x of [m1, m2, c2]) assert.ok(!history.includes(x));
  assert.equal(git(a3.path, ["status", "--porcelain"]).split("\n").sort().join("|"), "A  one.txt|A  two.txt");
  assert.deepEqual([...journal().peeledMarkers].sort(), [m1, m2].sort());
});

test("issue #3239 (f): sibling markers on one parent with no lineage for the first are refused", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m1 = await wipMarker(source.clonePath, "one.txt");
  const blob = git(bare, ["hash-object", "-w", "--stdin"], {}, "other content\n");
  const entries = git(bare, ["ls-tree", `${m1}^{tree}`]);
  const tree = git(bare, ["mktree"], {}, `${entries}\n100644 blob ${blob}\tother.txt\n`);
  const m2 = git(bare, [...IDENT, "commit-tree", tree, "-p", sha(`${m1}^`), "-m", "wip(park): second sibling"]);
  assert.notEqual(sha(`${m2}^{tree}`), sha(`${m1}^{tree}`));
  const c = (await genuine([m1, m2], m2)).sha;
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, m2);
  await assertRefused(r.attempts, aid(2));
});

test("issue #3239 (g): a marker leaf absent from the producing attempt's own journal lineage is refused", async () => {
  const { expected, attempts } = await secondEpisode();
  // The producing attempt's journal lost the lineage that names the superseded marker.
  const j = journal();
  delete j.peeledMarkers;
  writeJournal(j);
  const head = sha("HEAD", expected.clonePath);
  await assert.rejects(cache.prepareRecoverySuccessor(bare, branch, key, expected, attempts, aid(3)),
    /successor recovery verification failed/);
  assert.equal(journal().recovery.stage, "captured");
  assert.ok(!ledger().includes(aid(3)));
  assert.equal(sha("HEAD", expected.clonePath), head);
});

test("issue #3239: the journal parser validates peeledMarkers", async () => {
  const ok = ["a".repeat(40), "b".repeat(40)];
  const base = journal();
  writeJournal({ ...base, peeledMarkers: ok });
  await cache.reserveRecoveryIteration(bare, branch, key, source);
  assert.deepEqual(journal().peeledMarkers, ok, "a well-formed list round-trips");
  const stored = journal();
  const bad: Record<string, unknown> = {
    "a non-sha entry": ["not-a-sha"],
    "an uppercase sha": [ok[0]!.toUpperCase()],
    "a non-array": ok[0],
    "duplicates": [ok[0], ok[0]],
    "more than 64 entries": Array.from({ length: 65 }, (_, i) => i.toString(16).padStart(40, "0")),
  };
  for (const [name, value] of Object.entries(bad)) {
    writeJournal({ ...stored, peeledMarkers: value });
    await assert.rejects(cache.reserveRecoveryIteration(bare, branch, key, source), /invalid peeled markers/, name);
  }
  writeJournal({ ...stored, peeledMarkers: Array.from({ length: 64 }, (_, i) => i.toString(16).padStart(40, "0")) });
  await cache.reserveRecoveryIteration(bare, branch, key, source);
});

test("issue #3239 (b2): an aggregate whose own tree differs from the source's is refused even with the matching fingerprint and a pin", async () => {
  // The fingerprint binds the SOURCE's tree, not the aggregate's own, so only the explicit tree check
  // can refuse this wrapper: it is pinned, has the right identity, and its fingerprint is the real one.
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  const empty = git(bare, ["mktree"], {}, "");
  assert.notEqual(empty, sha(`${m}^{tree}`));
  const c = lookalike(m, { pin: true, tree: empty });
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, m);
  await assertRefused(r.attempts, aid(2));
});

for (const [name, msg] of [
  ["a 70 KB message", Buffer.from(`oversized\n${"x".repeat(70 * 1024)}\n`)],
  ["a non-UTF-8 byte (0xff)", Buffer.concat([Buffer.from("bad byte "), Buffer.from([0xff]), Buffer.from(" here\n")])],
  ["an ordinary message", Buffer.from("ordinary clean source\n")],
] as const) {
  test(`issue #3239 (h): a clean source with ${name} over a buried non-lineage marker leaf is refused`, async () => {
    const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
    const m = await wipMarker(source.clonePath, "work.txt");
    const clean = rawCommit(sha(`${m}^`), msg);
    const c = (await genuine([m, clean], clean)).sha;
    await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, clean);
    await assertRefused(r.attempts, aid(2));
  });
}

test("issue #3239 (h2): a coverage commit that cannot be read within bounds is refused even over a clean source", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const s0 = sha("HEAD", source.clonePath);
  const fp = createHash("sha256").update(JSON.stringify({ roots: [s0], currentSha: s0, tree: sha(`${s0}^{tree}`) })).digest("hex");
  const c = rawCommit(s0, Buffer.from(`uzi recovery coverage ${fp} round 0 batch 0\n\n${"x".repeat(70 * 1024)}\n`));
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, s0);
  await assertRefused(r.attempts, aid(2));
});

test("issue #3239 (h4): a readable tip claiming the coverage subject without its exact shape is refused over a buried marker", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  const clean = sibling(sha(`${m}^`), "ordinary clean source");
  const fp = "0".repeat(64);
  const c = git(bare, [...IDENT, "commit-tree", `${clean}^{tree}`, "-p", m, "-p", clean,
    "-m", `uzi recovery coverage ${fp} round 0 batch 0`, "-m", "extra body"]);
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, clean);
  await assertRefused(r.attempts, aid(2));
});

/** A lineage-listed marker leaf `mx` on `parentOf(m)`-or-elsewhere; the wrapper covers [m, mx] over source m. */
async function leafWithParent(inside: boolean) {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  const outside = sibling(sha(`${m}^`), "committed work elsewhere");
  const mx = sibling(inside ? sha(`${m}^`) : outside, "wip(park): superseded sibling marker");
  const c = (await genuine([m, mx], m)).sha;
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, c, m);
  writeJournal({ ...journal(), peeledMarkers: [mx] });
  return { r, m };
}

test("issue #3239 (i): a lineage-listed marker leaf whose parent is outside the source's history is refused", async () => {
  const { r } = await leafWithParent(false);
  await assertRefused(r.attempts, aid(2));
});

test("issue #3239 (i2): the same lineage-listed marker leaf with its parent inside the source's history is accepted", async () => {
  const { r, m } = await leafWithParent(true);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, r.attempts, aid(2));
  assert.equal(sha("HEAD", successor.path), sha(`${m}^`));
});

test("issue #3239 (j): a tip that is itself a WIP marker journals it as the successor's peeled lineage", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, m, m);
  const successor = await cache.prepareRecoverySuccessor(bare, branch, key, source, r.attempts, aid(2));
  assert.equal(sha("HEAD", successor.path), sha(`${m}^`));
  assert.equal(successor.wipRecovered, true);
  assert.deepEqual(journal().peeledMarkers, [m]);
  assert.equal(journal().recovery.stage, "ready-for-model");
});

test("issue #3239 (k): a plain clean tip journals no peeled markers", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  fs.writeFileSync(path.join(source.clonePath, "work.txt"), "milestone\n");
  git(source.clonePath, ["add", "work.txt"]);
  git(source.clonePath, [...IDENT, "commit", "-m", "milestone"]);
  await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
  const t = sha("HEAD", source.clonePath);
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, t, t);
  await cache.prepareRecoverySuccessor(bare, branch, key, source, r.attempts, aid(2));
  assert.equal(journal().peeledMarkers, undefined);
});

test("issue #3239: markRecoveryCapture unions and dedups peeled markers, keeps them across a capture, and writes none when none is given", async () => {
  const a = "a".repeat(40), b = "b".repeat(40);
  assert.equal(journal().peeledMarkers, undefined, "no marker given: the field is not written");
  await cache.markRecoveryCapture(bare, source.clonePath, branch, runId, undefined, a);
  assert.deepEqual(journal().peeledMarkers, [a]);
  await cache.markRecoveryCapture(bare, source.clonePath, branch, runId, undefined, b);
  await cache.markRecoveryCapture(bare, source.clonePath, branch, runId, undefined, a);
  assert.deepEqual([...journal().peeledMarkers].sort(), [a, b], "both markers, duplicates dedup");
  await cache.markRecoveryCapture(bare, source.clonePath, branch, runId);
  assert.deepEqual([...journal().peeledMarkers].sort(), [a, b], "a call without a marker leaves the lineage alone");
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, m, m);
  assert.deepEqual([...journal().peeledMarkers].sort(), [a, b], "preserved across recordRecoveryCapture");
  await assert.rejects(cache.markRecoveryCapture(bare, source.clonePath, branch, runId, undefined, "nope"), /invalid peeled marker/);
});

test("issue #3239 (h3): the non-UTF-8 fixture really is not valid UTF-8 and survives fsck", async () => {
  const bytes = Buffer.concat([Buffer.from("bad byte "), Buffer.from([0xff]), Buffer.from(" here\n")]);
  const c = rawCommit(sha("HEAD", source.clonePath), bytes);
  const body = execFileSync("git", ["-C", bare, "cat-file", "commit", c]);
  assert.ok(body.includes(0xff), "the raw 0xff byte is stored");
  assert.notDeepEqual(Buffer.from(body.toString("utf8"), "utf8"), body, "the body does not round-trip through UTF-8");
  git(bare, ["fsck", "--strict", "--no-dangling"]);
});

test("issue #3239 (l): a thrown read of an inner aggregate in a multi-round wrapper refuses instead of burying its marker", async () => {
  const r = await cache.reserveRecoveryIteration(bare, branch, key, source);
  const m = await wipMarker(source.clonePath, "work.txt");
  const base = sha(`${m}^`);
  const clean = sibling(base, "clean source");
  // 34 distinct roots force a second coverage round (parent batches hold at most 32 roots).
  const roots = [m, clean, ...Array.from({ length: 32 }, (_, i) => sibling(base, `filler ${i}`))];
  const c1 = (await genuine(roots, clean)).sha;
  const inner = git(bare, ["rev-list", "--parents", "-n", "1", c1]).split(" ").slice(1);
  assert.ok(inner.length >= 2, "sanity: c1 is a round-1 aggregate over round-0 aggregates");
  const holder = inner.find(a => git(bare, ["rev-list", "--parents", "-n", "1", a]).split(" ").includes(m));
  assert.ok(holder, "sanity: one round-0 aggregate carries the marker");
  // The tip is one level above c1, so the unreadable aggregate is two levels down: the tip's own
  // subject read does not touch it and only the walk's bounded read can notice.
  const tip = git(bare, ["--no-replace-objects", "-c", "commit.gpgsign=false", "commit-tree", sha(`${clean}^{tree}`), "-p", c1,
    "-m", `uzi recovery coverage ${"0".repeat(64)} round 2 batch 0`], {
    GIT_AUTHOR_NAME: "uzi-agent", GIT_AUTHOR_EMAIL: "uzi-agent@uzi.local", GIT_COMMITTER_NAME: "uzi-agent",
    GIT_COMMITTER_EMAIL: "uzi-agent@uzi.local", GIT_AUTHOR_DATE: "2000-01-01T00:00:00Z", GIT_COMMITTER_DATE: "2000-01-01T00:00:00Z",
  });
  await cache.recordRecoveryCapture(bare, branch, key, source, r.attempts, tip, clean);
  // Corrupt the loose object so cat-file throws (not a bounded-read refusal).
  const obj = path.join(bare, "objects", holder.slice(0, 2), holder.slice(2));
  fs.chmodSync(obj, 0o600);
  fs.writeFileSync(obj, "garbage");
  await assertRefused(r.attempts, aid(2));
});
