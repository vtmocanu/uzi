import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, type CheckpointPublicationContext, type PublicationCandidate } from "../src/git.js";
import { nullLogger } from "./helpers.js";

function fixture() {
  const scratch = path.resolve("../.uzi/scratch");
  fs.mkdirSync(scratch, { recursive: true });
  const root = fs.mkdtempSync(path.join(scratch, "admission-"));
  const bare = path.join(root, "repo.git"), remote = path.join(root, "remote.git");
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_AUTHOR_NAME: "Test", GIT_AUTHOR_EMAIL: "test@example.org",
    GIT_COMMITTER_NAME: "Test", GIT_COMMITTER_EMAIL: "test@example.org" };
  const git = (dir: string, args: string[], input?: string | Buffer) =>
    execFileSync("git", ["-C", dir, ...args], { env, input, encoding: "utf8", stdio: "pipe" }).trim();
  for (const dir of [bare, remote]) execFileSync("git", ["init", "--bare", dir], { env, stdio: "pipe" });
  const tree = git(bare, ["mktree"], "");
  const commit = (parents: string[], message: string) =>
    git(bare, ["commit-tree", tree, ...parents.flatMap(p => ["-p", p]), "-m", message]);
  const floor = commit([], "floor"), fresh = commit([floor], "fresh"), candidate = commit([fresh], "candidate");
  const branch = "agent/admission";
  git(bare, ["remote", "add", "origin", remote]);
  git(bare, ["update-ref", "refs/remotes/origin/main", floor]);
  git(bare, ["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main"]);
  git(bare, ["update-ref", "refs/remotes/origin/" + branch, floor]);
  git(bare, ["update-ref", "refs/uzi-runner/" + branch, candidate]);
  const cache = new GitCache(root, nullLogger());
  let producers = 0, spawns = 0;
  const producerInputs: unknown[] = [];
  const seam = cache as unknown as { spawnGit: (...args: unknown[]) => Promise<unknown>;
    runGit: (dir: string, args: string[], ...rest: unknown[]) => Promise<string> };
  const spawn = seam.spawnGit.bind(cache);
  seam.spawnGit = async (...args) => {
    spawns++;
    if ((args[1] as string[]).includes("pack-objects")) { producers++; producerInputs.push(args[2]); }
    return spawn(...args);
  };
  const drain = async (p: { pack: NodeJS.ReadableStream; exited: Promise<number> }) => {
    const chunks: Buffer[] = [];
    for await (const chunk of p.pack) chunks.push(Buffer.from(chunk));
    assert.equal(await p.exited, 0);
    return Buffer.concat(chunks);
  };
  const receive = async (p: { pack: NodeJS.ReadableStream; exited: Promise<number> }, sha: string) => {
    git(remote, ["index-pack", "--stdin", "--fix-thin"], await drain(p));
    git(remote, ["update-ref", "refs/heads/received", sha]);
  };
  return { root, bare, remote, git, commit, floor, fresh, candidate, branch, cache, seam, drain, receive,
    producers: () => producers, spawns: () => spawns, producerInputs, cleanup: () => fs.rmSync(root, { recursive: true, force: true }) };
}

for (const pinned of [false, true]) it(`checkpoint admission captures actual ${pinned ? "pinned" : "unpinned"} inputs and rejudges replay`, async () => {
  const f = fixture();
  try {
    const contexts: CheckpointPublicationContext[] = [];
    const p = await f.cache.checkpointPack(f.bare, f.branch, undefined,
      pinned ? { tipSha: f.candidate, excludeSha: f.floor, defaultBlobSha: f.floor } : undefined,
      undefined, f.candidate, async (sha, ctx) => {
        assert.ok(ctx);
        assert.equal(f.producers(), contexts.length);
        assert.equal(sha, f.candidate);
        assert.deepEqual(ctx, { sourceSha: f.candidate, candidateSha: f.candidate,
          excludeSha: f.floor, packExcludeShas: [f.floor], defaultBlobSha: f.floor });
        assert.ok(Object.isFrozen(ctx)); assert.ok(Object.isFrozen(ctx.packExcludeShas));
        contexts.push(ctx);
      }, undefined, { defaultBranch: "main" });
    assert.ok(p);
    // Give the receiver only the excluded floor, then prove the received ancestry.
    f.git(f.bare, ["push", "origin", f.floor + ":refs/heads/base"]);
    await f.receive(p, p.tipOid);
    assert.equal(f.git(f.remote, ["rev-parse", "received^"]), f.fresh);
    const replay = await p.replay();
    await f.drain(replay);
    assert.deepEqual(contexts[1], contexts[0]);
    assert.equal(contexts.length, 2);
  } finally { f.cleanup(); }
});

it("chain admission sees the transformed candidate and exact raw source", async () => {
  const f = fixture();
  try {
    const wrapper = f.commit([f.floor, f.candidate], "ckpt(overlay): previous wrapper");
    let judged = "";
    const p = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined, undefined, undefined,
      async (sha, ctx) => {
        assert.ok(ctx);
        judged = sha;
        assert.equal(ctx.sourceSha, f.candidate);
        assert.equal(ctx.candidateSha, wrapper);
        assert.deepEqual(ctx.packExcludeShas, [f.floor]);
      }, [wrapper]);
    assert.ok(p); assert.equal(p.tipOid, wrapper); assert.equal(judged, wrapper);
    f.git(f.bare, ["push", "origin", f.floor + ":refs/heads/base"]);
    await f.receive(p, wrapper);
    assert.equal(f.git(f.remote, ["rev-parse", "received^2"]), f.candidate);
  } finally { f.cleanup(); }
});

it("zero-range parent exclusions and pinned range are snapshotted before awaits and across replay", async () => {
  const f = fixture();
  try {
    const parents = [f.fresh];
    const range = { tipSha: f.candidate, excludeSha: f.candidate, packExcludeShas: parents };
    let calls = 0;
    const pending = f.cache.checkpointPack(f.bare, f.branch, undefined, range, undefined, undefined,
      async (_sha, ctx) => {
        assert.ok(ctx);
        calls++;
        assert.equal(ctx.excludeSha, f.candidate);
        assert.deepEqual(ctx.packExcludeShas, [f.fresh]);
        assert.throws(() => (ctx.packExcludeShas as string[]).push(f.floor), TypeError);
        parents[0] = f.candidate;
      });
    range.tipSha = f.floor; range.excludeSha = f.floor; parents[0] = f.floor;
    const p = await pending;
    assert.ok(p); assert.equal(p.tipOid, f.candidate);
    f.git(f.bare, ["push", "origin", f.fresh + ":refs/heads/base"]);
    await f.receive(p, f.candidate);
    await f.drain(await p.replay());
    assert.equal(calls, 2);
    assert.deepEqual(f.producerInputs, [1, 2].map(() => f.candidate + "\n^" + f.fresh + "\n"));
  } finally { f.cleanup(); }
});

for (const rejectAt of [1, 2]) it(`checkpoint guard rejection at producer ${rejectAt} prevents its spawn`, async () => {
  const f = fixture();
  try {
    let calls = 0;
    const refusal = new Error("admission refused");
    const start = () => f.cache.checkpointPack(f.bare, f.branch, undefined, undefined, undefined, undefined,
      async () => { if (++calls === rejectAt) throw refusal; }, undefined, { defaultBranch: "main" });
    if (rejectAt === 1) {
      await assert.rejects(start(), e => e === refusal);
      assert.equal(f.spawns(), 0, "admission precedes object-list preparation too");
    }
    else {
      const p = await start(); assert.ok(p);
      await f.drain(p);
      const spawns = f.spawns();
      await assert.rejects(p.replay(), e => e === refusal);
      assert.equal(f.spawns(), spawns, "replay rejection cannot prepare objects");
    }
    assert.equal(f.producers(), rejectAt - 1);
  } finally { f.cleanup(); }
});

it("invalid captured default blocks admission and producer", async () => {
  const f = fixture();
  try {
    let called = false;
    await assert.rejects(f.cache.checkpointPack(f.bare, f.branch, undefined,
      { tipSha: f.candidate, excludeSha: f.floor, defaultBlobSha: "0".repeat(40) },
      undefined, undefined, async () => { called = true; }));
    assert.equal(called, false); assert.equal(f.producers(), 0);
  } finally { f.cleanup(); }
});

for (const present of [false, true]) it(`push admission sees fresh ${present ? "pinned" : "absent"} floor and immutable inputs`, async () => {
  const f = fixture();
  try {
    if (present) f.git(f.bare, ["push", "origin", f.fresh + ":refs/heads/" + f.branch]);
    const group = { candidate: f.candidate, originalHead: f.candidate,
      floor: present ? { kind: "pinned" as const, oid: f.floor } : { kind: "absent" as const } };
    let called = 0;
    const pending = f.cache.pushBranch(f.bare, f.branch, "", f.remote, undefined, group,
      async (sha, ctx) => {
        assert.ok(ctx);
        called++;
        assert.equal(sha, f.candidate); assert.equal(ctx.candidateSha, f.candidate);
        assert.equal(ctx.originalHead, f.candidate);
        assert.deepEqual(ctx.originalFloor, present ? { kind: "pinned", oid: f.floor } : { kind: "absent" });
        assert.deepEqual(ctx.remoteFloor, present ? { kind: "pinned", oid: f.fresh } : { kind: "absent" });
        for (const obj of [ctx, ctx.originalFloor, ctx.remoteFloor]) assert.ok(Object.isFrozen(obj));
        assert.throws(() => Object.assign(ctx, { candidateSha: f.floor }), TypeError);
        assert.equal(f.git(f.bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-publication-floor"]), "");
      });
    group.candidate = f.floor; group.originalHead = f.floor;
    if (group.floor.kind === "pinned") group.floor.oid = f.candidate;
    await pending;
    assert.equal(called, 1);
    assert.equal(f.git(f.remote, ["rev-parse", "refs/heads/" + f.branch]), f.candidate);
    assert.equal(f.git(f.remote, ["rev-parse", f.branch + "^"]), f.fresh);
  } finally { f.cleanup(); }
});

it("push guard rejection propagates unchanged without push or NFF refresh", async () => {
  const f = fixture();
  try {
    let pushes = 0, observations = 0;
    const run = f.seam.runGit.bind(f.cache);
    f.seam.runGit = async (dir, args, ...rest) => {
      if (args[0] === "push") pushes++;
      if (args[0] === "ls-remote") observations++;
      return run(dir, args, ...rest);
    };
    const refusal = new Error("non-fast-forward rejected");
    await assert.rejects(f.cache.pushBranch(f.bare, f.branch, "", f.remote, undefined, undefined,
      async () => { throw refusal; }), e => e === refusal);
    assert.equal(pushes, 0); assert.equal(observations, 2);
    assert.equal(f.git(f.remote, ["for-each-ref", "--format=%(refname)", "refs/heads/" + f.branch]), "");
  } finally { f.cleanup(); }
});

for (const failure of ["floor", "cleanup"] as const) it(`push ${failure} failure blocks callback and push`, async () => {
  const f = fixture();
  try {
    let called = false, pushes = 0;
    const run = f.seam.runGit.bind(f.cache);
    f.seam.runGit = async (dir, args, ...rest) => {
      if (args[0] === "push") pushes++;
      if (failure === "cleanup" && args[0] === "update-ref" && args[1] === "-d" &&
          args[2]?.startsWith("refs/uzi-publication-floor/")) throw new Error("cleanup failed");
      return run(dir, args, ...rest);
    };
    const group: PublicationCandidate = { candidate: f.candidate, originalHead: f.candidate,
      floor: failure === "floor" ? { kind: "unverified" } : { kind: "absent" } };
    await assert.rejects(f.cache.pushBranch(f.bare, f.branch, "", f.remote, undefined, group,
      async () => { called = true; }));
    assert.equal(called, false); assert.equal(pushes, 0);
  } finally { f.cleanup(); }
});

for (const root of [false, true]) it(`unpinned zero range exposes actual ${root ? "root" : "parent"} exclusions`, async () => {
  const f = fixture();
  try {
    const tip = root ? f.floor : f.fresh;
    f.git(f.bare, ["update-ref", "-d", "refs/remotes/origin/" + f.branch]);
    f.git(f.bare, ["update-ref", "refs/remotes/origin/main", tip]);
    f.git(f.bare, ["update-ref", "refs/uzi-runner/" + f.branch, tip]);
    const p = await f.cache.checkpointPack(f.bare, f.branch, undefined, undefined, undefined, undefined,
      async (sha, ctx) => {
        assert.ok(ctx);
        assert.equal(sha, tip); assert.equal(ctx.sourceSha, tip); assert.equal(ctx.excludeSha, tip);
        assert.deepEqual(ctx.packExcludeShas, root ? [] : [f.floor]);
      });
    assert.ok(p);
    if (!root) f.git(f.bare, ["push", "origin", f.floor + ":refs/heads/base"]);
    await f.receive(p, tip);
    assert.equal(f.git(f.remote, ["rev-parse", "received"]), tip);
    assert.deepEqual(f.producerInputs, [tip + "\n" + (root ? "" : "^" + f.floor + "\n")]);
  } finally { f.cleanup(); }
});

it("NFF proof refresh does not admit or execute a second push", async () => {
  const f = fixture();
  try {
    let calls = 0, pushes = 0, observations = 0;
    const run = f.seam.runGit.bind(f.cache);
    const rejection = new Error("non-fast-forward");
    f.seam.runGit = async (dir, args, ...rest) => {
      if (args[0] === "push") { pushes++; throw rejection; }
      if (args[0] === "ls-remote") observations++;
      return run(dir, args, ...rest);
    };
    await assert.rejects(f.cache.pushBranch(f.bare, f.branch, "", f.remote, undefined, undefined,
      async () => { calls++; }), e => e === rejection);
    assert.equal(calls, 1); assert.equal(pushes, 1); assert.equal(observations, 4);
  } finally { f.cleanup(); }
});
