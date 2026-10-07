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

// An output stream that ended before execScoped attached without anything ever being read from it is a clean
// empty EOF (the Codex supervisor closes its stdio copies at launch, so a fast silent git child's pipe can
// already have ended when the handle is returned). One that emitted data to nobody (child_process resumes unread
// stdio at exit), or was destroyed without ending, may have lost output and is refused.
type Seen = { destroyed: boolean; readableEnded: boolean; readableDidRead: boolean };
const snap = (s: Readable): Seen => ({ destroyed: s.destroyed, readableEnded: s.readableEnded, readableDidRead: s.readableDidRead });

let fx: Fixture;
let cache: GitCache;
beforeEach(() => { fx = makeFixture(); cache = new GitCache(fx.dataDir, nullLogger()); });
afterEach(() => fx.cleanup());

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

/** Real child, handed over only after it exited and its pipes closed (non-delayed otherwise). */
function exitedFirst(seen: Seen[]): BoundaryProcessSpawner {
  return async (request: BoundaryProcessRequest): Promise<BoundaryProcessHandle> => {
    const [command, ...args] = request.argv;
    const child = spawn(command!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
    const [code] = await once(child, "close") as [number | null];
    for (const s of [child.stdout, child.stderr]) seen.push(snap(s));
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => {}, completed: Promise.resolve({ code: code ?? 128 }) };
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
  return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; }, completed };
};

/**
 * The Codex supervisor shape: the git child runs, then the supervisor closes its stdio copies while the
 * handled process stays alive. Streams are handed over only once stderr has ended and been destroyed
 * (nothing read), while stdout still holds its unread bytes.
 */
function supervisorShape(seen: Seen[]): BoundaryProcessSpawner {
  return async (request) => {
    const child = spawn("sh", ["-c", '"$@"; rc=$?; exec >&- 2>&-; sleep 0.3; exit $rc', "sh", ...request.argv],
      { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code) => resolve({ code: code ?? 128 }));
    });
    for (let i = 0; i < 200 && !(child.stderr.readableEnded && child.stderr.destroyed); i++) {
      await new Promise((r) => setTimeout(r, 10));
    }
    seen.push(snap(child.stdout), snap(child.stderr));
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; }, completed };
  };
}

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

/** A command with stdout data and empty stderr (the preflight's tip resolution). */
function execOut(bare: string, sha: string): Promise<{ stdout: string }> {
  return (cache as unknown as { execScoped: (c: string, a: string[], o: object) => Promise<{ stdout: string }> })
    .execScoped("git", ["-C", bare, "rev-parse", sha], { env: GIT_ENV, timeout: 10_000 });
}

/** The preflight's object-walk command, which has empty stdout and stderr. */
function execQuiet(bare: string, sha: string): Promise<{ stdout: string }> {
  return (cache as unknown as { execScoped: (c: string, a: string[], o: object) => Promise<{ stdout: string }> })
    .execScoped("git", ["-C", bare, "rev-list", "--objects", "--quiet", sha], { env: GIT_ENV, timeout: 10_000 });
}

const inBoundary = <T>(spawner: BoundaryProcessSpawner, fn: () => Promise<T>): Promise<T> =>
  cache.withBoundaryProcessSpawner(spawner, new AbortController().signal, fn);

describe("execScoped boundary collect tells a clean empty EOF from dropped output", () => {
  // child_process resumes unread stdio when the child exits, so an exited child's output is dropped
  // while its streams read as destroyed+ended. Output read before collect listened must never read as
  // empty or complete; a stream that ended with nothing ever read is genuinely empty.
  it("refuses a real child that exited before handoff with stdout data", async () => {
    const { bare, sha } = await candidate();
    const seen: Seen[] = [];
    await assert.rejects(inBoundary(exitedFirst(seen), () => execOut(bare, sha)), /subprocess output closed before end/);
    assert.ok(seen[0]!.destroyed && seen[0]!.readableEnded && seen[0]!.readableDidRead, JSON.stringify(seen[0]));
  });

  it("reads a real child that exited before handoff with no output as empty", async () => {
    const { bare, sha } = await candidate();
    const seen: Seen[] = [];
    const out = await inBoundary(exitedFirst(seen), () => execQuiet(bare, sha));
    assert.equal(out.stdout, "");
    assert.ok(seen.every((s) => s.destroyed && s.readableEnded && !s.readableDidRead), JSON.stringify(seen));
  });

  it("reads an ended, resumed and closed PassThrough pair with no data as empty", async () => {
    const { bare, sha } = await candidate();
    const spawner: BoundaryProcessSpawner = async (request) => {
      const real = await realPipes(request);
      await real.completed;
      return { stdin: new Writable({ write: (_c, _e, cb) => cb() }), stdout: (await endedStream()) as Readable,
        stderr: (await endedStream()) as Readable, cancel: async () => {}, completed: Promise.resolve({ code: 0 }) };
    };
    assert.equal((await inBoundary(spawner, () => execQuiet(bare, sha))).stdout, "");
  });

  it("refuses an ended and closed stdout PassThrough whose data was drained", async () => {
    const { bare, sha } = await candidate();
    const spawner: BoundaryProcessSpawner = async () => {
      const stdout = new PassThrough();
      stdout.end("dropped output\n");
      stdout.resume();
      await once(stdout, "close");
      const stderr = await endedStream();
      return { stdin: null, stdout, stderr, cancel: async () => {}, completed: Promise.resolve({ code: 0 }) };
    };
    await assert.rejects(inBoundary(spawner, () => execQuiet(bare, sha)), /subprocess output closed before end/);
  });

  it("refuses a stdout partly drained before handoff that ends later (no partial read as complete)", async () => {
    const { bare, sha } = await candidate();
    const spawner: BoundaryProcessSpawner = async () => {
      const stdout = new PassThrough();
      stdout.write("lost head\n");
      stdout.resume();
      await once(stdout, "data");
      setImmediate(() => stdout.end("tail\n"));
      const stderr = new PassThrough();
      stderr.end();
      return { stdin: null, stdout, stderr, cancel: async () => {}, completed: Promise.resolve({ code: 0 }) };
    };
    await assert.rejects(inBoundary(spawner, () => execQuiet(bare, sha)), /subprocess output closed before end/);
  });

  it("refuses a stream that ended empty and then errored", async () => {
    const { bare, sha } = await candidate();
    let seen: (Seen & { errored: boolean }) | undefined;
    const spawner: BoundaryProcessSpawner = async () => {
      const stdout = new PassThrough({ autoDestroy: false });
      stdout.end();
      stdout.resume();
      await once(stdout, "end");
      stdout.on("error", () => undefined);
      stdout.destroy(new Error("late failure"));
      seen = { ...snap(stdout), errored: stdout.errored !== null };
      const stderr = new PassThrough();
      stderr.end();
      return { stdin: null, stdout, stderr, cancel: async () => {}, completed: Promise.resolve({ code: 0 }) };
    };
    await assert.rejects(inBoundary(spawner, () => execQuiet(bare, sha)));
    assert.equal(seen?.readableEnded, true);
    assert.equal(seen?.readableDidRead, false);
    assert.equal(seen?.errored, true);
  });

  it("refuses an ended but not destroyed stdout whose data was drained", async () => {
    const { bare, sha } = await candidate();
    let seen: Seen | undefined;
    const spawner: BoundaryProcessSpawner = async () => {
      const stdout = new PassThrough({ autoDestroy: false });
      stdout.write("dropped output\n");
      stdout.end();
      stdout.resume();
      await once(stdout, "end");
      seen = snap(stdout);
      const stderr = new PassThrough();
      stderr.end();
      return { stdin: null, stdout, stderr, cancel: async () => {}, completed: Promise.resolve({ code: 0 }) };
    };
    await assert.rejects(inBoundary(spawner, () => execQuiet(bare, sha)), /subprocess output closed before end/);
    assert.deepEqual(seen, { destroyed: false, readableEnded: true, readableDidRead: true });
  });

  it("refuses publication when the scratch walk output was dropped (no fail-open)", async () => {
    const scratch = path.join(fx.originPath, ".uzi", "scratch");
    fs.mkdirSync(scratch, { recursive: true });
    fs.writeFileSync(path.join(scratch, "private.txt"), "private\n");
    git(fx.originPath, "add", "-f", ".uzi/scratch/private.txt");
    git(fx.originPath, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "scratch history");
    const { bare, sha } = await candidate();
    const seen: Seen[] = [];
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
    assert.ok(seen.some((s) => s.destroyed && s.readableEnded && s.readableDidRead),
      `scratch walk stdout handoff state: ${JSON.stringify(seen)}`);
  });
});

describe("scratch publication preflight inside a boundary scope", () => {
  it("passes with real child pipes", async () => {
    const { bare, sha } = await candidate();
    assert.equal(await inBoundary(realPipes, () => cache.scratchPublicationPreflight(bare, "agent/issue-1", sha)), sha);
  });

  it("passes when each git child's empty stderr pipe ended before the handle was returned (supervisor shape)", async () => {
    const { bare, sha } = await candidate();
    const seen: Seen[] = [];
    assert.equal(await inBoundary(supervisorShape(seen), () => cache.scratchPublicationPreflight(bare, "agent/issue-1", sha)), sha);
    assert.ok(seen.length >= 2 && seen.length % 2 === 0, `calls: ${seen.length}`);
    for (let i = 0; i < seen.length; i += 2) {
      const e = seen[i + 1]!;
      assert.ok(e.destroyed && e.readableEnded && !e.readableDidRead, `stderr handoff state: ${JSON.stringify(e)}`);
    }
    assert.ok(seen.some((s, i) => i % 2 === 0 && !s.readableEnded), "no stdout was left buffered and unended");
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
        return { stdin: null, stdout, stderr, cancel: async () => { await completed; }, completed };
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
          return true;
        });
    });
  }
});
