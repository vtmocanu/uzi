import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough, type Readable } from "node:stream";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, type BoundaryProcessSpawner } from "../src/git.js";

// spawnGit STREAMS a git child's stdout (the checkpoint pack upload). Node ends a child's
// stdout before it emits `close`, so a consumer of the raw stream could read a clean
// (empty or truncated) EOF from a FAILED child, and the later destroy(err) was a no-op: a
// failed `pack-objects` could be uploaded as if it were a valid pack. The broker then
// forwards a pack-less create, which origin rejects with "eof before pack header" (the
// symptom seen in the #1739 nightly; that run's git stderr is not available, so its
// underlying pack-objects failure is unproven).

type SpawnGit = (cwd: string, args: string[], stdin?: string) => Promise<{ child?: ChildProcess; stdout: Readable }>;

function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(c));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

function withTmp<T>(fn: (root: string) => Promise<T>): Promise<T> {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "spawngit-"));
  return fn(root).finally(() => fs.rmSync(root, { recursive: true, force: true }));
}

function cache(root: string): { git: GitCache; spawnGit: SpawnGit } {
  const git = new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
  return { git, spawnGit: (git as unknown as { spawnGit: SpawnGit }).spawnGit.bind(git) };
}

function bare(root: string): string {
  const dir = path.join(root, "repo.git");
  execFileSync("git", ["init", "--bare", "-q", dir]);
  return dir;
}

/** A controllable supervised process: the test writes stdout and settles `completed`. */
function fakeProcess(): {
  spawner: BoundaryProcessSpawner;
  stdout: PassThrough;
  settle: (r: { code: number } | Error) => void;
} {
  const stdout = new PassThrough();
  let settle!: (r: { code: number } | Error) => void;
  const completed = new Promise<{ code: number }>((resolve, reject) => {
    settle = (r) => (r instanceof Error ? reject(r) : resolve(r));
  });
  completed.catch(() => undefined);
  const spawner: BoundaryProcessSpawner = () =>
    Promise.resolve({ stdin: new PassThrough(), stdout, stderr: new PassThrough(), cancel: async () => { await completed; }, completed });
  return { spawner, stdout, settle };
}

describe("spawnGit stream failure, plain spawn", () => {
  it("a git child that exits nonzero errors the stream instead of ending it cleanly", () =>
    withTmp(async (root) => {
      const { spawnGit } = cache(root);
      // An unknown revision makes pack-objects exit 128 before writing a pack.
      const { stdout } = await spawnGit(bare(root), ["pack-objects", "--revs", "--stdout"], `${"d".repeat(40)}\n`);
      await assert.rejects(drain(stdout), /exited 128/);
    }));

  it("a successful git child still streams its full output and ends", () =>
    withTmp(async (root) => {
      const { spawnGit } = cache(root);
      // An empty revision list is a valid empty pack: header + checksum, 32 bytes.
      const { stdout } = await spawnGit(bare(root), ["pack-objects", "--revs", "--stdout"], "");
      const pack = await drain(stdout);
      assert.equal(pack.length, 32);
      assert.equal(pack.subarray(0, 4).toString(), "PACK");
    }));

  it("a consumer that abandons the stream early does not leave the child blocked", () =>
    withTmp(async (root) => {
      const { spawnGit } = cache(root);
      // A pack far larger than the pipe and stream buffers, so the child is still writing
      // when the consumer walks away.
      const work = path.join(root, "work");
      execFileSync("git", ["init", "-q", work]);
      fs.writeFileSync(path.join(work, "big.bin"), randomBytes(8 * 1024 * 1024));
      execFileSync("git", ["-C", work, "add", "big.bin"]);
      execFileSync("git", ["-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "big"]);
      const head = execFileSync("git", ["-C", work, "rev-parse", "HEAD"]).toString().trim();
      const { child, stdout } = await spawnGit(work, ["pack-objects", "--revs", "--stdout", "--compression=0"], `${head}\n`);
      assert.ok(child, "the plain path returns its child");
      const exited = new Promise<void>((resolve) => child!.once("close", () => resolve()));
      await new Promise<void>((resolve) => stdout.once("data", () => resolve()));
      stdout.destroy();
      const timeout = new Promise<never>((_, reject) =>
        setTimeout(() => reject(new Error("child still alive 10s after the consumer abandoned the stream")), 10_000).unref(),
      );
      await Promise.race([exited, timeout]);
    }));
});

describe("spawnGit stream failure, supervised (boundary) process", () => {
  it("nonzero completion after stdout ended errors the stream", () =>
    withTmp(async (root) => {
      const { git, spawnGit } = cache(root);
      const p = fakeProcess();
      const { stdout } = await git.withBoundaryProcessSpawner(p.spawner, new AbortController().signal, () =>
        spawnGit(root, ["pack-objects", "--revs", "--stdout"]),
      );
      const drained = drain(stdout);
      p.stdout.end(); // stdout ends BEFORE the completion settles, as with a real child
      await new Promise((r) => setImmediate(r));
      p.settle({ code: 128 });
      await assert.rejects(drained, /exited 128/);
    }));

  it("a rejected completion errors the stream", () =>
    withTmp(async (root) => {
      const { git, spawnGit } = cache(root);
      const p = fakeProcess();
      const { stdout } = await git.withBoundaryProcessSpawner(p.spawner, new AbortController().signal, () =>
        spawnGit(root, ["pack-objects", "--revs", "--stdout"]),
      );
      const drained = drain(stdout);
      p.stdout.end();
      await new Promise((r) => setImmediate(r));
      p.settle(new Error("supervisor reap failed"));
      await assert.rejects(drained, /supervisor reap failed/);
    }));

  it("a clean completion delivers every byte and ends", () =>
    withTmp(async (root) => {
      const { git, spawnGit } = cache(root);
      const p = fakeProcess();
      const { stdout } = await git.withBoundaryProcessSpawner(p.spawner, new AbortController().signal, () =>
        spawnGit(root, ["pack-objects", "--revs", "--stdout"]),
      );
      const drained = drain(stdout);
      p.stdout.end(Buffer.from("PACK-bytes"));
      await new Promise((r) => setImmediate(r));
      p.settle({ code: 0 });
      assert.equal((await drained).toString(), "PACK-bytes");
    }));

  it("abandoning the stream tears down the supervised stdout", () =>
    withTmp(async (root) => {
      const { git, spawnGit } = cache(root);
      const p = fakeProcess();
      const { stdout } = await git.withBoundaryProcessSpawner(p.spawner, new AbortController().signal, () =>
        spawnGit(root, ["pack-objects", "--revs", "--stdout"]),
      );
      stdout.destroy();
      await new Promise((r) => setImmediate(r));
      assert.equal(p.stdout.destroyed, true, "the producer's stdout is destroyed so its writer cannot block");
      p.settle({ code: 0 });
    }));
});
