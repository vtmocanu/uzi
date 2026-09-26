import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, ScratchPublicationError } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

let fx: Fixture;
let cache: GitCache;
const branch = "agent/issue-1719";
const ident = ["-c", "user.name=Test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false"];
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: {
    ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  } }).trim();
}
function commit(dir: string, name: string): string {
  git(dir, "add", "-A");
  git(dir, ...ident, "commit", "-m", name);
  return git(dir, "rev-parse", "HEAD");
}
async function setup(): Promise<{ bare: string; clone: string }> {
  const bare = await cache.ensureClone(fx.originPath);
  const rc = await cache.createOrAttachRunnerClone(bare, 1719);
  return { bare, clone: rc.path };
}
async function track(bare: string, clone: string): Promise<void> {
  await cache.fetchAgentBranch(bare, clone, branch, "scratch-test");
}
beforeEach(() => { fx = makeFixture(); cache = new GitCache(fx.dataDir, nullLogger()); });
afterEach(() => fx.cleanup());

it("accepts clean history and pushes the pinned candidate", async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "clean.txt"), "clean\n");
  const sha = commit(clone, "clean");
  await track(bare, clone);
  assert.equal(await cache.scratchPublicationPreflight(bare, branch), sha);
  await cache.pushBranch(bare, branch, "", fx.originPath);
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), sha);
});

it("packs the pinned WIP candidate after the tracking ref moves, including overlay path", async () => {
  const { bare, clone } = await setup();
  const floor = git(bare, "rev-parse", "refs/remotes/origin/main");
  fs.writeFileSync(path.join(clone, "wip.txt"), "work in progress\n");
  const wip = commit(clone, "WIP");
  await track(bare, clone);
  fs.writeFileSync(path.join(clone, "later.txt"), "later\n");
  commit(clone, "later");
  await track(bare, clone);
  for (const overlay of [undefined, { defaultBranch: "main" }]) {
    const packed = await cache.checkpointPack(bare, branch, overlay, { tipSha: wip, excludeSha: floor });
    assert.ok(packed);
    assert.equal(packed.tipOid, wip);
    const chunks: Buffer[] = [];
    for await (const chunk of packed.pack) chunks.push(Buffer.from(chunk));
    assert.equal(Buffer.concat(chunks).subarray(0, 4).toString(), "PACK");
  }
});

it("packs unpinned candidate and floor OIDs even when refs move at pack time", async () => {
  const { bare, clone } = await setup();
  const floor = git(bare, "rev-parse", "refs/remotes/origin/main");
  fs.writeFileSync(path.join(clone, "wip.txt"), "wip\n");
  const candidate = commit(clone, "wip");
  await track(bare, clone);
  fs.writeFileSync(path.join(clone, "later.txt"), "later\n");
  const later = commit(clone, "later");
  await track(bare, clone);
  git(bare, "update-ref", `refs/uzi-runner/${branch}`, candidate);
  const seam = cache as unknown as { spawnGit: (
    cwd: string, args: string[], stdin?: string,
  ) => Promise<{ stdout: NodeJS.ReadableStream }> };
  const original = seam.spawnGit.bind(cache);
  seam.spawnGit = async (cwd, args, stdin) => {
    assert.equal(stdin, `${candidate}\n^${floor}\n`);
    git(bare, "update-ref", `refs/uzi-runner/${branch}`, later);
    git(bare, "update-ref", "refs/remotes/origin/main", candidate);
    return original(cwd, args, stdin);
  };
  const packed = await cache.checkpointPack(bare, branch);
  assert.ok(packed);
  assert.equal(packed.tipOid, candidate);
  const chunks: Buffer[] = [];
  for await (const chunk of packed.pack) chunks.push(Buffer.from(chunk));
  assert.equal(Buffer.concat(chunks).subarray(0, 4).toString(), "PACK");
});

it("refuses a force staged scratch file and an add then delete", async () => {
  const { bare, clone } = await setup();
  fs.mkdirSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  fs.writeFileSync(path.join(clone, ".uzi", "scratch", "secret"), "value\n");
  git(clone, "add", "-f", ".uzi/scratch/secret");
  git(clone, ...ident, "commit", "-m", "add scratch");
  fs.rmSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  commit(clone, "delete scratch");
  await track(bare, clone);
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch), ScratchPublicationError);
  await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
  assert.throws(() => git(fx.originPath, "rev-parse", `refs/heads/${branch}`));
});

it("finds scratch on a merged side branch even when merge result is clean", async () => {
  const { bare, clone } = await setup();
  git(clone, "checkout", "-b", "side");
  fs.mkdirSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  fs.writeFileSync(path.join(clone, ".uzi", "scratch", "x"), "x");
  git(clone, "add", "-f", ".uzi/scratch/x");
  git(clone, ...ident, "commit", "-m", "side scratch");
  git(clone, "checkout", branch);
  git(clone, ...ident, "merge", "--no-ff", "side", "-m", "merge side");
  fs.rmSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  commit(clone, "remove merged scratch");
  await track(bare, clone);
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch), ScratchPublicationError);
});

it("refuses a scratch symlink and a file replacing its directory", async () => {
  const { bare, clone } = await setup();
  fs.rmSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  fs.symlinkSync("../README.md", path.join(clone, ".uzi", "scratch"));
  git(clone, "add", "-f", ".uzi/scratch");
  git(clone, ...ident, "commit", "-m", "symlink");
  fs.rmSync(path.join(clone, ".uzi", "scratch"));
  fs.writeFileSync(path.join(clone, ".uzi", "scratch"), "file");
  git(clone, "add", "-f", ".uzi/scratch");
  git(clone, ...ident, "commit", "-m", "file");
  await track(bare, clone);
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch), ScratchPublicationError);
});

it("refuses a rewound remote floor before pushing", async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  commit(clone, "one");
  await track(bare, clone);
  await cache.pushBranch(bare, branch, "", fx.originPath);
  git(fx.originPath, "update-ref", `refs/heads/${branch}`, "refs/heads/main");
  fs.writeFileSync(path.join(clone, "two"), "two");
  commit(clone, "two");
  await track(bare, clone);
  await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
});

it("refuses a candidate with a missing ancestor object", async () => {
  const { bare } = await setup();
  const tree = git(bare, "rev-parse", "refs/heads/main^{tree}");
  const missing = "a".repeat(40);
  const body = `tree ${tree}\nparent ${missing}\nauthor Test <test@example.org> 1 +0000\ncommitter Test <test@example.org> 1 +0000\n\nbroken ancestry\n`;
  const candidate = execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--stdin"], {
    encoding: "utf8", input: body,
  }).trim();
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch, candidate), ScratchPublicationError);
});

it("refuses a candidate with a missing tree object", async () => {
  const { bare } = await setup();
  const missing = "b".repeat(40);
  const body = `tree ${missing}\nparent ${git(bare, "rev-parse", "refs/heads/main")}\nauthor Test <test@example.org> 1 +0000\ncommitter Test <test@example.org> 1 +0000\n\nbroken tree\n`;
  const candidate = execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--stdin"], {
    encoding: "utf8", input: body,
  }).trim();
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch, candidate), ScratchPublicationError);
});

it("refuses timeout and output overflow from either bounded history walk", async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "clean.txt"), "clean\n");
  commit(clone, "clean");
  await track(bare, clone);
  const seam = cache as unknown as { execScoped: (...args: unknown[]) => Promise<{ stdout: string; stderr: string }> };
  const original = seam.execScoped.bind(cache);
  for (const walk of ["--objects", "--full-history"]) {
    for (const failure of ["timeout", "output overflow"]) {
      seam.execScoped = async (...args) => {
        const argv = args[1];
        if (Array.isArray(argv) && argv.includes("rev-list") && argv.includes(walk)) {
          assert.deepEqual(args[2] && {
            timeout: (args[2] as { timeout: number }).timeout,
            maxBuffer: (args[2] as { maxBuffer: number }).maxBuffer,
          }, { timeout: 10_000, maxBuffer: 4_096 });
          throw new Error(failure);
        }
        return original(...args);
      };
      await assert.rejects(cache.scratchPublicationPreflight(bare, branch), ScratchPublicationError);
    }
  }
  seam.execScoped = original;
});

it("refuses unavailable and divergent checkpoint floors", async () => {
  const { bare, clone } = await setup();
  const floor = git(bare, "rev-parse", "refs/remotes/origin/main");
  fs.writeFileSync(path.join(clone, "wip.txt"), "wip\n");
  const candidate = commit(clone, "wip");
  await track(bare, clone);
  await assert.rejects(cache.checkpointPack(bare, branch, undefined, {
    tipSha: candidate, excludeSha: "a".repeat(40),
  }), ScratchPublicationError);
  const unrelated = git(bare, "-c", "user.name=Test", "-c", "user.email=test@example.org",
    "commit-tree", `${floor}^{tree}`, "-m", "unrelated root");
  await assert.rejects(cache.checkpointPack(bare, branch, undefined, {
    tipSha: candidate, excludeSha: unrelated,
  }), ScratchPublicationError);
  git(bare, "update-ref", `refs/remotes/origin/${branch}`, unrelated);
  await assert.rejects(cache.checkpointPack(bare, branch), ScratchPublicationError);
});

it("refuses shallow or unresolved candidate history", async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const sha = commit(clone, "one");
  await track(bare, clone);
  fs.writeFileSync(path.join(bare, "shallow"), `${sha}\n`);
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch), ScratchPublicationError);
  fs.rmSync(path.join(bare, "shallow"));
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch, "a".repeat(40)), ScratchPublicationError);
});
