import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, ScratchPublicationError } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed } from "./helpers.js";

// Every case creates a runner clone, whose scratch provisioning needs Linux procfs;
// the unsupported-platform refusal itself is covered in git.test.ts.
const linuxCloneSkip = process.platform === "linux" ? false : "runner clone scratch provisioning requires Linux procfs";

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
  const rc = await cache.createOrAttachRunnerClone(bare, 1719, noProofReseed);
  return { bare, clone: rc.path };
}
async function track(bare: string, clone: string): Promise<void> {
  await fixtureFetchTracking(cache, bare, clone, branch, "scratch-test");
}
beforeEach(() => { fx = makeFixture(); cache = new GitCache(fx.dataDir, nullLogger()); });
afterEach(() => fx.cleanup());

it("accepts clean history and pushes the pinned candidate", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "clean.txt"), "clean\n");
  const sha = commit(clone, "clean");
  await track(bare, clone);
  assert.equal(await cache.scratchPublicationPreflight(bare, branch), sha);
  await cache.pushBranch(bare, branch, "", fx.originPath);
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), sha);
});

it("packs the pinned WIP candidate after the tracking ref moves, including overlay path", { skip: linuxCloneSkip }, async () => {
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

it("packs unpinned candidate and floor OIDs even when refs move at pack time", { skip: linuxCloneSkip }, async () => {
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

type ScratchShape = "forced staged file" | "add then delete" | "symlink replacing directory" | "file replacing directory";

for (const shape of ["forced staged file", "add then delete", "symlink replacing directory", "file replacing directory"] as const satisfies readonly ScratchShape[]) {
  it(`checkpoint and final push refuse ${shape} without moving the confirmed or remote tip`, { skip: linuxCloneSkip }, async () => {
    const { bare, clone } = await setup();
    fs.writeFileSync(path.join(clone, "clean.txt"), "clean\n");
    const confirmed = commit(clone, "clean confirmed tip");
    await track(bare, clone);
    await cache.pushBranch(bare, branch, "", fx.originPath);
    const ref = `refs/heads/${branch}`;
    const tracking = `refs/remotes/origin/${branch}`;
    assert.equal(git(fx.originPath, "rev-parse", ref), confirmed);

    const scratch = path.join(clone, ".uzi", "scratch");
    if (shape === "forced staged file" || shape === "add then delete") {
      fs.mkdirSync(scratch, { recursive: true });
      fs.writeFileSync(path.join(scratch, "secret"), "value\n");
      git(clone, "add", "-f", ".uzi/scratch/secret");
      git(clone, ...ident, "commit", "-m", "scratch file");
      if (shape === "add then delete") {
        fs.rmSync(scratch, { recursive: true });
        commit(clone, "delete scratch");
      }
    } else {
      fs.rmSync(scratch, { recursive: true });
      if (shape === "symlink replacing directory") fs.symlinkSync("../README.md", scratch);
      else fs.writeFileSync(scratch, "file");
      git(clone, "add", "-f", ".uzi/scratch");
      git(clone, ...ident, "commit", "-m", shape);
    }
    await track(bare, clone);
    assert.notEqual(git(bare, "rev-parse", `refs/uzi-runner/${branch}`), confirmed);
    await assert.rejects(cache.checkpointPack(bare, branch), ScratchPublicationError);
    await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
    assert.equal(git(fx.originPath, "rev-parse", ref), confirmed);
    assert.equal(git(bare, "rev-parse", tracking), confirmed);
  });
}

it("refuses a force staged scratch file and an add then delete", { skip: linuxCloneSkip }, async () => {
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

it("finds scratch on a merged side branch even when merge result is clean", { skip: linuxCloneSkip }, async () => {
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

it("refuses a scratch symlink and a file replacing its directory", { skip: linuxCloneSkip }, async () => {
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

it("refuses a rewound remote floor before pushing", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  commit(clone, "one");
  await track(bare, clone);
  await cache.pushBranch(bare, branch, "", fx.originPath);
  // A new claim captures the published tip; a checkpoint is not its claim floor.
  const captured = await cache.createOrAttachRunnerClone(bare, 1719, noProofReseed);
  const P = git(clone, "rev-parse", "HEAD");
  assert.deepEqual(captured.publicationFloor, { kind: "pinned", oid: P });
  git(fx.originPath, "update-ref", `refs/heads/${branch}`, "refs/heads/main");
  fs.writeFileSync(path.join(clone, "two"), "two");
  const H = commit(clone, "two");
  await track(bare, clone);
  await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath, undefined, {
    floor: captured.publicationFloor!, originalHead: H, candidate: H,
  }), ScratchPublicationError);
});

it("cannot publish when the remote rewinds to scratch history after the floor refresh", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "clean.txt"), "clean\n");
  commit(clone, "clean");
  await track(bare, clone);
  await cache.pushBranch(bare, branch, "", fx.originPath);
  fs.writeFileSync(path.join(clone, "next.txt"), "next\n");
  const candidate = commit(clone, "next");
  await track(bare, clone);

  const scratch = path.join(fx.originPath, ".uzi", "scratch");
  fs.mkdirSync(scratch, { recursive: true });
  fs.writeFileSync(path.join(scratch, "private.txt"), "private\n");
  git(fx.originPath, "add", "-f", ".uzi/scratch/private.txt");
  const scratchTip = commit(fx.originPath, "remote scratch history");
  const seam = cache as unknown as { runGit: (...args: unknown[]) => Promise<string> };
  const original = seam.runGit.bind(cache);
  seam.runGit = async (...args) => {
    const argv = args[1];
    if (Array.isArray(argv) && argv[0] === "push") {
      git(fx.originPath, "update-ref", `refs/heads/${branch}`, scratchTip);
    }
    return original(...args);
  };
  try {
    await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath));
  } finally {
    seam.runGit = original;
  }
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), scratchTip);
  assert.notEqual(scratchTip, candidate);
});

it("refuses a candidate with a missing ancestor object", { skip: linuxCloneSkip }, async () => {
  const { bare } = await setup();
  const tree = git(bare, "rev-parse", "refs/heads/main^{tree}");
  const missing = "a".repeat(40);
  const body = `tree ${tree}\nparent ${missing}\nauthor Test <test@example.org> 1 +0000\ncommitter Test <test@example.org> 1 +0000\n\nbroken ancestry\n`;
  const candidate = execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--stdin"], {
    encoding: "utf8", input: body,
  }).trim();
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch, candidate), ScratchPublicationError);
});

it("refuses a candidate with a missing tree object", { skip: linuxCloneSkip }, async () => {
  const { bare } = await setup();
  const missing = "b".repeat(40);
  const body = `tree ${missing}\nparent ${git(bare, "rev-parse", "refs/heads/main")}\nauthor Test <test@example.org> 1 +0000\ncommitter Test <test@example.org> 1 +0000\n\nbroken tree\n`;
  const candidate = execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--stdin"], {
    encoding: "utf8", input: body,
  }).trim();
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch, candidate), ScratchPublicationError);
});

it("refuses timeout and output overflow from either bounded history walk", { skip: linuxCloneSkip }, async () => {
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

it("refuses unavailable and divergent checkpoint floors", { skip: linuxCloneSkip }, async () => {
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

it("refuses shallow or unresolved candidate history", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const sha = commit(clone, "one");
  await track(bare, clone);
  fs.writeFileSync(path.join(bare, "shallow"), `${sha}\n`);
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch), ScratchPublicationError);
  fs.rmSync(path.join(bare, "shallow"));
  await assert.rejects(cache.scratchPublicationPreflight(bare, branch, "a".repeat(40)), ScratchPublicationError);
});

it("refuses a checkpoint whose floor cannot be resolved with the typed reason", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  const floor = git(bare, "rev-parse", "refs/remotes/origin/main");
  fs.writeFileSync(path.join(clone, "wip.txt"), "wip\n");
  const candidate = commit(clone, "wip");
  await track(bare, clone);
  // A malformed pinned range is a range that cannot be established.
  await assert.rejects(cache.checkpointPack(bare, branch, undefined, {
    tipSha: "not-a-sha", excludeSha: floor,
  }), ScratchPublicationError);
  await assert.rejects(cache.checkpointPack(bare, branch, undefined, {
    tipSha: candidate, excludeSha: "short",
  }), ScratchPublicationError);
  // No origin branch and a tracking tip disjoint from the default: merge-base exits
  // non-zero, which must surface as the typed refusal, not a generic git error.
  const orphan = git(bare, ...ident, "commit-tree", `${floor}^{tree}`, "-m", "disjoint root");
  git(bare, "update-ref", `refs/uzi-runner/${branch}`, orphan);
  assert.throws(() => git(bare, "rev-parse", "--verify", `refs/remotes/origin/${branch}`));
  await assert.rejects(cache.checkpointPack(bare, branch), ScratchPublicationError);
});

// A branch created on the remote after the worker's last fetch: the bare has no
// refs/remotes/origin/<branch>, so there is no prior floor to rewind-check.
function seedRemoteBranch(sourceRepo: string, sha: string): void {
  git(fx.originPath, "fetch", "--no-tags", sourceRepo, `${sha}:refs/heads/${branch}`);
}
function assertNoPriorFloor(bare: string): void {
  assert.throws(() => git(bare, "rev-parse", "--verify", `refs/remotes/origin/${branch}`));
}

it("fast-forwards onto a remote branch created after the last fetch", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const base = commit(clone, "one");
  seedRemoteBranch(clone, base);
  fs.writeFileSync(path.join(clone, "two"), "two");
  const candidate = commit(clone, "two");
  await track(bare, clone);
  assertNoPriorFloor(bare);
  await cache.pushBranch(bare, branch, "", fx.originPath);
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), candidate);
});

it("refuses a divergent remote branch created after the last fetch", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  git(fx.originPath, "checkout", "-q", "-b", "elsewhere");
  fs.writeFileSync(path.join(fx.originPath, "remote-only"), "remote");
  const remoteTip = commit(fx.originPath, "remote only");
  git(fx.originPath, "checkout", "-q", "main");
  git(fx.originPath, "update-ref", `refs/heads/${branch}`, remoteTip);
  fs.writeFileSync(path.join(clone, "local"), "local");
  commit(clone, "local");
  await track(bare, clone);
  assertNoPriorFloor(bare);
  await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), remoteTip);
});

it("refuses scratch history onto a remote branch created after the last fetch", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const base = commit(clone, "one");
  seedRemoteBranch(clone, base);
  fs.mkdirSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  fs.writeFileSync(path.join(clone, ".uzi", "scratch", "secret"), "value\n");
  git(clone, "add", "-f", ".uzi/scratch/secret");
  git(clone, ...ident, "commit", "-m", "scratch");
  await track(bare, clone);
  assertNoPriorFloor(bare);
  await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), base);
});

it("refuses unproven ancestry onto a remote branch created after the last fetch", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const base = commit(clone, "one");
  seedRemoteBranch(clone, base);
  fs.writeFileSync(path.join(clone, "two"), "two");
  commit(clone, "two");
  await track(bare, clone);
  assertNoPriorFloor(bare);
  const seam = cache as unknown as { ancestry: (...args: unknown[]) => Promise<string> };
  const original = seam.ancestry.bind(cache);
  seam.ancestry = async () => "unknown";
  try {
    await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
  } finally {
    seam.ancestry = original;
  }
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), base);
});

it("refuses a tracking floor ref that names no commit", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const base = commit(clone, "one");
  seedRemoteBranch(clone, base);
  fs.writeFileSync(path.join(clone, "two"), "two");
  commit(clone, "two");
  await track(bare, clone);
  // update-ref refuses a missing object, so write the dangling loose ref directly.
  const loose = path.join(bare, "refs", "remotes", "origin", ...branch.split("/"));
  fs.mkdirSync(path.dirname(loose), { recursive: true });
  fs.writeFileSync(loose, `${"c".repeat(40)}\n`);
  await assert.rejects(cache.pushBranch(bare, branch, "", fx.originPath), ScratchPublicationError);
  assert.equal(git(fx.originPath, "rev-parse", `refs/heads/${branch}`), base);
});

// --- classified refusals (issue #2054): every refusal carries kind, step and a one-line detail ---

type ExecSeam = { execScoped: (...args: unknown[]) => Promise<{ stdout: string; stderr: string }> };

async function refusal(promise: Promise<unknown>): Promise<ScratchPublicationError> {
  try {
    await promise;
  } catch (e) {
    assert.ok(e instanceof ScratchPublicationError, `expected ScratchPublicationError, got ${String(e)}`);
    return e;
  }
  assert.fail("expected a ScratchPublicationError");
}
function assertDetail(e: ScratchPublicationError): string {
  assert.ok(e.detail && e.detail.length > 0, "detail must be non-empty");
  assert.ok(e.detail.length <= 200);
  assert.ok(!Array.from(e.detail).some((c) => c.charCodeAt(0) < 0x20), "detail must not carry control characters");
  return e.detail;
}
/** Make the git call whose argv includes every marker in `markers` fail with `err`. */
function failExec(markers: string[], err: unknown): () => void {
  const seam = cache as unknown as ExecSeam;
  const original = seam.execScoped;
  seam.execScoped = async (...args) => {
    const argv = args[1];
    if (Array.isArray(argv) && markers.every((m) => argv.includes(m))) throw err;
    return original.apply(cache, args);
  };
  return () => { seam.execScoped = original; };
}
async function trackedClean(): Promise<{ bare: string; sha: string }> {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "clean.txt"), "clean\n");
  const sha = commit(clone, "clean");
  await track(bare, clone);
  return { bare, sha };
}

it("classifies an absent tracking ref as tip_unavailable at resolve_tip", { skip: linuxCloneSkip }, async () => {
  const { bare } = await setup();
  const e = await refusal(cache.pushBranch(bare, branch, "", fx.originPath));
  assert.equal(e.kind, "tip_unavailable");
  assert.equal(e.step, "resolve_tip");
  assertDetail(e);
  assert.match(e.message, /candidate commit is unavailable/);
});

it("classifies shallow history as shallow_history at shallow_check", { skip: linuxCloneSkip }, async () => {
  const { bare, sha } = await trackedClean();
  fs.writeFileSync(path.join(bare, "shallow"), `${sha}\n`);
  const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
  assert.equal(e.kind, "shallow_history");
  assert.equal(e.step, "shallow_check");
  assertDetail(e);
});

it("classifies a deleted loose blob reachable from the candidate as missing_objects", { skip: linuxCloneSkip }, async () => {
  const { bare } = await setup();
  const blob = execFileSync("git", ["-C", bare, "hash-object", "-w", "--stdin"], {
    encoding: "utf8", input: "lost blob\n",
  }).trim();
  const tree = execFileSync("git", ["-C", bare, "mktree"], {
    encoding: "utf8", input: `100644 blob ${blob}\tlost.txt\n`,
  }).trim();
  const candidate = git(bare, ...ident, "commit-tree", tree, "-p", git(bare, "rev-parse", "refs/heads/main"), "-m", "lost blob");
  fs.rmSync(path.join(bare, "objects", blob.slice(0, 2), blob.slice(2)));
  const e = await refusal(cache.scratchPublicationPreflight(bare, branch, candidate));
  assert.equal(e.kind, "missing_objects");
  assert.equal(e.step, "object_walk");
  assert.match(assertDetail(e), /missing|object|unable|corrupt/i);
});

it("classifies a non-missing git failure in the object walk as object_walk_failed", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const err = Object.assign(new Error("subprocess exited 128"), { code: 128, stderr: "fatal: unexpected failure\n", stdout: "" });
  const restore = failExec(["--objects"], err);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.equal(e.kind, "object_walk_failed");
    assert.equal(e.step, "object_walk");
    assert.match(assertDetail(e), /exit 128.*unexpected failure/);
  } finally { restore(); }
});

const execShapes: Array<[string, Error]> = [
  ["timeout", Object.assign(new Error("Command failed"), { killed: true, signal: "SIGTERM", code: null })],
  ["execFile output overflow", Object.assign(new Error("stdout maxBuffer length exceeded"), { code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" })],
  ["boundary output overflow with exit 0", Object.assign(new Error("subprocess output exceeded 4096 bytes"), { code: 0, outputExceeded: true })],
  ["spawn failure", Object.assign(new Error("spawn git ENOENT"), { code: "ENOENT" })],
];
for (const [walk, step] of [["--objects", "object_walk"], ["--full-history", "scratch_walk"]] as const) {
  for (const [name, err] of execShapes) {
    it(`classifies ${name} in the ${step} as exec_failed, never a git exit`, { skip: linuxCloneSkip }, async () => {
      const { bare } = await trackedClean();
      const restore = failExec(["rev-list", walk], err);
      try {
        const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
        assert.equal(e.kind, "exec_failed");
        assert.equal(e.step, step);
        assertDetail(e);
        assert.equal(e.cause, err);
      } finally { restore(); }
    });
  }
}

it("classifies a probe error in the shallow check as exec_failed", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const restore = failExec(["--is-shallow-repository"], new Error("probe exploded"));
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.equal(e.kind, "exec_failed");
    assert.equal(e.step, "shallow_check");
    assert.match(assertDetail(e), /probe exploded/);
  } finally { restore(); }
});

it("classifies scratch in candidate history as scratch_present at scratch_walk", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.mkdirSync(path.join(clone, ".uzi", "scratch"), { recursive: true });
  fs.writeFileSync(path.join(clone, ".uzi", "scratch", "secret"), "value\n");
  git(clone, "add", "-f", ".uzi/scratch/secret");
  git(clone, ...ident, "commit", "-m", "scratch");
  await track(bare, clone);
  const e = await refusal(cache.pushBranch(bare, branch, "", fx.originPath));
  assert.equal(e.kind, "scratch_present");
  assert.equal(e.step, "scratch_walk");
  assertDetail(e);
});

it("a tip resolve that fails to execute is exec_failed with the error in detail, not tip_unavailable", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const err = new Error("spawn git EAGAIN");
  const restore = failExec(["rev-parse", "--verify"], err);
  try {
    const e = await refusal(cache.pushBranch(bare, branch, "", fx.originPath));
    assert.equal(e.kind, "exec_failed");
    assert.equal(e.step, "resolve_tip");
    assert.match(assertDetail(e), /EAGAIN/);
    assert.equal(e.cause, err);
  } finally { restore(); }
});

it("classifies a failed remote floor refresh as floor_unverified at floor_refresh", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const seam = cache as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
  const original = seam.runGit;
  seam.runGit = async (cwd, args, ...rest) => {
    if (args.includes("ls-remote")) throw new Error("git ls-remote origin failed: network down");
    return original.call(cache, cwd, args, ...rest);
  };
  try {
    const e = await refusal(cache.pushBranch(bare, branch, "", fx.originPath));
    assert.equal(e.kind, "floor_unverified");
    assert.equal(e.step, "floor_refresh");
    assert.match(e.message, /cannot verify fresh remote floor/);
    assert.match(assertDetail(e), /network down/);
  } finally { seam.runGit = original; }
});

it("classifies checkpoint floor refusals as checkpoint_range at checkpoint_floor", { skip: linuxCloneSkip }, async () => {
  const { bare, sha } = await trackedClean();
  for (const range of [{ tipSha: "not-a-sha", excludeSha: sha }, { tipSha: sha, excludeSha: "a".repeat(40) }]) {
    const e = await refusal(cache.checkpointPack(bare, branch, undefined, range));
    assert.equal(e.kind, "checkpoint_range");
    assert.equal(e.step, "checkpoint_floor");
    assertDetail(e);
  }
});

it("keeps detail to one capped line without control characters", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const long = `bad\u0007thing ${"x".repeat(500)}\nsecond line`;
  const restore = failExec(["--is-shallow-repository"], new Error(long));
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    const detail = assertDetail(e);
    assert.equal(detail.length, 200);
    assert.doesNotMatch(detail, /\n|second line/);
    assert.match(detail, /^bad\?thing x+/);
  } finally { restore(); }
});

async function detailFor(message: string): Promise<string> {
  const { bare } = await trackedClean();
  const restore = failExec(["--is-shallow-repository"], new Error(message));
  try {
    return assertDetail(await refusal(cache.scratchPublicationPreflight(bare, branch)));
  } finally { restore(); }
}

it("does not let bidi, mark or line-separator code points survive in detail", { skip: linuxCloneSkip }, async () => {
  const cp = String.fromCodePoint;
  const detail = await detailFor(`evil${cp(0x202e)}gnp${cp(0x2066)}x${cp(0x200f)}y`);
  for (const c of [0x202e, 0x2066, 0x200f]) assert.ok(!detail.includes(cp(c)), `U+${c.toString(16)} survived`);
  assert.match(detail, /evil.gnp.x.y/);
});

it("splits detail lines on U+2028 and keeps only the first", { skip: linuxCloneSkip }, async () => {
  const cp = String.fromCodePoint;
  const detail = await detailFor(`first${cp(0x2028)}second${cp(0x2029)}third`);
  assert.ok(!detail.includes(cp(0x2028)) && !detail.includes(cp(0x2029)));
  assert.match(detail, /first/);
  assert.doesNotMatch(detail, /second|third/);
});

it("carries the bounded unsplit stderr in rawDetail beside the one-line detail", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const err = Object.assign(new Error("subprocess exited 128"), {
    code: 128, stdout: "", stderr: "fatal: tok\tpart\nsecond line carries more",
  });
  const restore = failExec(["--is-shallow-repository"], err);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.ok(e.rawDetail !== undefined);
    assert.ok(e.rawDetail.includes("tok\tpart"), e.rawDetail);
    assert.ok(e.rawDetail.includes("second line carries more"), e.rawDetail);
    const detail = assertDetail(e);
    assert.match(detail, /tok\?part/);
    assert.doesNotMatch(detail, /second line/);
  } finally { restore(); }
});

it("trims leading padding before bounding rawDetail so a later token is not cut", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const token = "tok-" + "z".repeat(40);
  const err = Object.assign(new Error("subprocess exited 128"), {
    code: 128, stdout: "", stderr: `${"\n".repeat(4080)}fatal: ${token}`,
  });
  const restore = failExec(["--is-shallow-repository"], err);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.ok(e.rawDetail?.includes(token), e.rawDetail);
  } finally { restore(); }
});

it("omits an oversized rawDetail rather than cutting a padding-split token to its prefix", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const token = "tok-" + "z".repeat(40);
  const err = Object.assign(new Error("subprocess exited 128"), {
    code: 128, stdout: "", stderr: `fatal: ${token.slice(0, 16)}${"\n".repeat(5000)}${token.slice(16)}`,
  });
  const restore = failExec(["--is-shallow-repository"], err);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.ok(e.rawDetail !== undefined);
    assert.ok(!e.rawDetail.includes(token.slice(0, 8)), e.rawDetail);
    assert.match(e.rawDetail, /characters omitted: over the redaction bound/);
  } finally { restore(); }
});

it("derives detail from a megabyte of stderr without scanning it all", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const err = Object.assign(new Error("subprocess exited 128"), {
    code: 128, stdout: "", stderr: `fatal: first\n${"y".repeat(1024 * 1024)}\n`,
  });
  const restore = failExec(["--is-shallow-repository"], err);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    const detail = assertDetail(e);
    assert.match(detail, /exit 128; fatal: first/);
    assert.doesNotMatch(detail, /y{10}/);
  } finally { restore(); }
});

it("reports output overflow even when git also exited non-zero with a missing-object message", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const err = Object.assign(new Error("subprocess output exceeded limit"), {
    code: 128, outputExceeded: true, stdout: "", stderr: "fatal: missing blob deadbeef\n",
  });
  const restore = failExec(["rev-list", "--objects"], err);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.equal(e.kind, "exec_failed");
    assert.equal(e.step, "object_walk");
    assert.match(assertDetail(e), /output exceeded limit/);
  } finally { restore(); }
});

it("says timed out only for our timeout kill, not for a bare signal", { skip: linuxCloneSkip }, async () => {
  const { bare } = await trackedClean();
  const killed = Object.assign(new Error("Command failed"), { killed: true, signal: "SIGTERM", code: null });
  let restore = failExec(["rev-list", "--objects"], killed);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.equal(e.kind, "exec_failed");
    assert.match(assertDetail(e), /timed out/);
  } finally { restore(); }
  const signalled = Object.assign(new Error("Command failed"), { killed: false, signal: "SIGKILL", code: null });
  restore = failExec(["rev-list", "--objects"], signalled);
  try {
    const e = await refusal(cache.scratchPublicationPreflight(bare, branch));
    assert.equal(e.kind, "exec_failed");
    const detail = assertDetail(e);
    assert.match(detail, /signal SIGKILL/);
    assert.doesNotMatch(detail, /timed out/);
  } finally { restore(); }
});

it("a fresh floor read that fails to execute is floor_unverified at floor_refresh, not a changed branch", { skip: linuxCloneSkip }, async () => {
  const { bare, clone } = await setup();
  fs.writeFileSync(path.join(clone, "one"), "one");
  const base = commit(clone, "one");
  seedRemoteBranch(clone, base);
  fs.writeFileSync(path.join(clone, "two"), "two");
  commit(clone, "two");
  await track(bare, clone);
  const restore = failExec(["rev-parse", `refs/uzi-publication-floor/${branch}^{commit}`], new Error("spawn git EAGAIN"));
  try {
    const e = await refusal(cache.pushBranch(bare, branch, "", fx.originPath));
    assert.equal(e.kind, "floor_unverified");
    assert.equal(e.step, "floor_refresh");
    assert.match(e.message, /cannot verify fresh remote floor/);
    const detail = assertDetail(e);
    assert.match(detail, /EAGAIN/);
    assert.doesNotMatch(detail, /changed during refresh/);
  } finally { restore(); }
});

it("defaults a bare one-argument refusal to exec_failed", () => {
  const e = new ScratchPublicationError("boom");
  assert.equal(e.kind, "exec_failed");
  assert.equal(e.step, undefined);
  assert.equal(e.detail, undefined);
  assert.equal(e.message, "scratch_publication_refused: boom");
});
