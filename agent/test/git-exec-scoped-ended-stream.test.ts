import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { once } from "node:events";
import { PassThrough, Readable, Writable } from "node:stream";
import { GitCache, ScratchPublicationError, type BoundaryProcessSpawner } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

// Output streams already ended or destroyed when execScoped attaches may have lost data (child_process
// resumes unread stdio at exit), so they are refused rather than read as empty output.

let fx: Fixture;
let cache: GitCache;
beforeEach(() => { fx = makeFixture(); cache = new GitCache(fx.dataDir, nullLogger()); });
afterEach(() => fx.cleanup());

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

/** Real child, handed over only after it exited and its pipes closed (non-delayed otherwise). */
function exitedFirst(seen: Array<{ destroyed: boolean; readableEnded: boolean }>): BoundaryProcessSpawner {
  return async (request: BoundaryProcessRequest): Promise<BoundaryProcessHandle> => {
    const [command, ...args] = request.argv;
    const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
    const [code] = await once(child, "close") as [number | null];
    for (const s of [child.stdout, child.stderr]) seen.push({ destroyed: s.destroyed, readableEnded: s.readableEnded });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed: Promise.resolve({ code: code ?? 128 }) };
  };
}

/** Real child, handed over immediately (the plain in-boundary shape). */
const realPipes: BoundaryProcessSpawner = async (request) => {
  const [command, ...args] = request.argv;
  const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
  const completed = new Promise<{ code: number }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code) => resolve({ code: code ?? 128 }));
  });
  return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed };
};

async function endedStream(): Promise<PassThrough> {
  const s = new PassThrough();
  s.end();
  s.resume();
  await once(s, "close");
  return s;
}

async function candidate(): Promise<{ bare: string; sha: string }> {
  const bare = await cache.ensureClone(fx.originPath);
  return { bare, sha: git(bare, "rev-parse", "refs/remotes/origin/main") };
}

/** The preflight's object-walk command, which has empty stdout and stderr. */
function execQuiet(bare: string, sha: string): Promise<{ stdout: string }> {
  return (cache as unknown as { execScoped: (c: string, a: string[], o: object) => Promise<{ stdout: string }> })
    .execScoped("git", ["-C", bare, "rev-list", "--objects", "--quiet", sha], { env: GIT_ENV, timeout: 10_000 });
}

const inBoundary = <T>(spawner: BoundaryProcessSpawner, fn: () => Promise<T>): Promise<T> =>
  cache.withBoundaryProcessSpawner(spawner, new AbortController().signal, fn);

describe("execScoped boundary collect on already-ended streams fails closed", () => {
  // child_process resumes unread stdio when the child exits, so an exited child's output is dropped
  // while its streams read as destroyed+ended. That must never read as empty output.
  it("refuses a real child that exited before handoff", async () => {
    const { bare, sha } = await candidate();
    const seen: Array<{ destroyed: boolean; readableEnded: boolean }> = [];
    await assert.rejects(inBoundary(exitedFirst(seen), () => execQuiet(bare, sha)), /subprocess output closed before end/);
    assert.ok(seen.some((s) => s.destroyed && s.readableEnded), "no stream was destroyed+ended at handoff");
  });

  it("refuses an ended, resumed and closed PassThrough pair", async () => {
    const { bare, sha } = await candidate();
    const spawner: BoundaryProcessSpawner = async (request) => {
      const real = await realPipes(request);
      await real.completed;
      return { stdin: new Writable({ write: (_c, _e, cb) => cb() }), stdout: (await endedStream()) as Readable,
        stderr: (await endedStream()) as Readable, completed: Promise.resolve({ code: 0 }) };
    };
    await assert.rejects(inBoundary(spawner, () => execQuiet(bare, sha)), /subprocess output closed before end/);
  });

  it("refuses an ended but not destroyed stdout whose data was drained", async () => {
    const { bare, sha } = await candidate();
    let seen: { destroyed: boolean; readableEnded: boolean } | undefined;
    const spawner: BoundaryProcessSpawner = async () => {
      const stdout = new PassThrough({ autoDestroy: false });
      stdout.write("dropped output\n");
      stdout.end();
      stdout.resume();
      await once(stdout, "end");
      seen = { destroyed: stdout.destroyed, readableEnded: stdout.readableEnded };
      const stderr = new PassThrough();
      stderr.end();
      return { stdin: null, stdout, stderr, completed: Promise.resolve({ code: 0 }) };
    };
    await assert.rejects(inBoundary(spawner, () => execQuiet(bare, sha)), /subprocess output closed before end/);
    assert.deepEqual(seen, { destroyed: false, readableEnded: true });
  });

  it("refuses publication when the scratch walk output was dropped (no fail-open)", async () => {
    const scratch = path.join(fx.originPath, ".uzi", "scratch");
    fs.mkdirSync(scratch, { recursive: true });
    fs.writeFileSync(path.join(scratch, "private.txt"), "private\n");
    git(fx.originPath, "add", "-f", ".uzi/scratch/private.txt");
    git(fx.originPath, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "scratch history");
    const { bare, sha } = await candidate();
    const seen: Array<{ destroyed: boolean; readableEnded: boolean }> = [];
    const exited = exitedFirst(seen);
    const spawner: BoundaryProcessSpawner = (request) => request.argv.includes("--full-history") ? exited(request) : realPipes(request);
    // Control: the same history with normal pipes is refused for the scratch path.
    await assert.rejects(inBoundary(realPipes, () => cache.scratchPublicationPreflight(bare, "agent/issue-1", sha)),
      (err: unknown) => err instanceof ScratchPublicationError && err.kind === "scratch_present");
    await assert.rejects(
      inBoundary(spawner, () => cache.scratchPublicationPreflight(bare, "agent/issue-1", sha)),
      (err: unknown) => {
        assert.ok(err instanceof ScratchPublicationError, String(err));
        assert.equal(err.kind, "exec_failed");
        assert.equal(err.step, "scratch_walk");
        return true;
      });
    assert.ok(seen.some((s) => s.destroyed && s.readableEnded), "scratch walk stdout was not destroyed+ended at handoff");
  });
});

describe("scratch publication preflight inside a boundary scope", () => {
  it("passes with real child pipes", async () => {
    const { bare, sha } = await candidate();
    assert.equal(await inBoundary(realPipes, () => cache.scratchPublicationPreflight(bare, "agent/issue-1", sha)), sha);
  });

  const refusals: Array<[string, string, BoundaryProcessSpawner]> = [
    ["command roots live", "codex boundary process refused: command roots live",
      async () => { throw new Error("codex boundary process refused: command roots live"); }],
    ["admission not closed", "codex boundary process refused: admission not closed",
      async () => { throw new Error("codex boundary process refused: admission not closed"); }],
    ["deadline during launch", "boundary process child deadline exceeded during launch",
      async () => {
        const completed = Promise.reject(new Error("boundary process child deadline exceeded during launch"));
        completed.catch(() => undefined);
        const stdout = new PassThrough();
        const stderr = new PassThrough();
        stdout.end();
        stderr.end();
        return { stdin: null, stdout, stderr, completed };
      }],
  ];
  for (const [name, message, spawner] of refusals) {
    it(`surfaces a non-poisoning refusal (${name}) as exec_failed at resolve_tip`, async () => {
      const { bare, sha } = await candidate();
      await assert.rejects(
        inBoundary(spawner, () => cache.scratchPublicationPreflight(bare, "agent/issue-1", sha)),
        (err: unknown) => {
          assert.ok(err instanceof ScratchPublicationError, String(err));
          assert.equal(err.kind, "exec_failed");
          assert.equal(err.step, "resolve_tip");
          assert.ok(err.detail?.includes(message), `detail: ${err.detail}`);
          assert.notEqual(err.detail, "candidate commit is unavailable");
          return true;
        });
    });
  }
});
