// PRD #1798 M5: GitCache.readBare, the bounded bare read behind the PR-description context
// (pr-description-context.ts). Real git over a real bare repository.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { PassThrough } from "node:stream";
import { setTimeout as delay } from "node:timers/promises";

import { GitCache, type BoundaryProcessSpawner } from "../src/git.js";
import { resolvePrdInputFromBare } from "../src/prd-link.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

function git(cwd: string, ...args: string[]): string {
  const env: NodeJS.ProcessEnv = {};
  for (const [k, v] of Object.entries(process.env)) if (!k.startsWith("GIT_")) env[k] = v;
  return execFileSync("git", args, {
    cwd,
    encoding: "utf8",
    env: {
      ...env,
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_AUTHOR_NAME: "t",
      GIT_AUTHOR_EMAIL: "t@t",
      GIT_COMMITTER_NAME: "t",
      GIT_COMMITTER_EMAIL: "t@t",
    },
  });
}

/** A bare repository whose HEAD commit holds `files`. */
function withBare(files: Record<string, string>, fn: (gc: GitCache, bare: string) => Promise<void>): Promise<void> {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-readbare-"));
  const work = path.join(root, "work");
  fs.mkdirSync(work);
  git(work, "init", "-q", "-b", "main");
  for (const [p, content] of Object.entries(files)) fs.writeFileSync(path.join(work, p), content);
  git(work, "add", "-A");
  git(work, "commit", "-qm", "one");
  const bare = path.join(root, "bare.git");
  git(root, "clone", "-q", "--bare", work, bare);
  const gc = new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
  return fn(gc, bare).finally(() => fs.rmSync(root, { recursive: true, force: true }));
}

describe("GitCache.readBare (PRD #1798 M5)", () => {
  it("returns a small output whole, untruncated", async () => {
    await withBare({ "a.txt": "hello\n" }, async (gc, bare) => {
      assert.deepEqual(await gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes: 1024, timeoutMs: 30_000 }), {
        text: "hello\n",
        truncated: false,
      });
    });
  });

  it("an output of exactly maxBytes is not truncated", async () => {
    await withBare({ "a.txt": "x".repeat(100) }, async (gc, bare) => {
      const r = await gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes: 100 });
      assert.equal(r.truncated, false);
      assert.equal(r.text.length, 100);
    });
  });

  it("stops at maxBytes on a large output and reports truncated instead of throwing", async () => {
    await withBare({ "big.txt": "y".repeat(2 * 1024 * 1024) }, async (gc, bare) => {
      const r = await gc.readBare(bare, ["show", "HEAD:big.txt"], { maxBytes: 4096, timeoutMs: 30_000 });
      assert.equal(r.truncated, true);
      assert.equal(r.text, "y".repeat(4096));
    });
  });

  it("a cut through a multi-byte character drops it rather than emitting U+FFFD", async () => {
    await withBare({ "u.txt": "é".repeat(1000) }, async (gc, bare) => {
      const r = await gc.readBare(bare, ["show", "HEAD:u.txt"], { maxBytes: 101 });
      assert.equal(r.truncated, true);
      assert.equal(r.text, "é".repeat(50));
      assert.equal(r.text.includes("�"), false);
    });
  });

  it("rejects on a non-zero git exit", async () => {
    await withBare({ "a.txt": "a" }, async (gc, bare) => {
      await assert.rejects(gc.readBare(bare, ["show", "HEAD:missing.txt"], { maxBytes: 1024 }), /missing\.txt/);
    });
  });

  it("rejects without spawning when the time is already spent or the signal already aborted", async () => {
    await withBare({ "a.txt": "a" }, async (gc, bare) => {
      await assert.rejects(gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes: 1024, timeoutMs: 0 }), /no time left/);
      const ac = new AbortController();
      ac.abort();
      await assert.rejects(gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes: 1024, signal: ac.signal }), /aborted/);
    });
  });

  it("rejects at timeoutMs on a read that does not finish, and on a mid-read abort", async () => {
    await withBare({ "a.txt": "a" }, async (gc, bare) => {
      // A literal, inert alias that just waits: the read must not outlive its bound.
      const slow = ["-c", "alias.wait=!sleep 3", "wait"];
      let started = Date.now();
      await assert.rejects(gc.readBare(bare, slow, { maxBytes: 1024, timeoutMs: 100 }), /exceeded 100ms/);
      assert.ok(Date.now() - started < 2_500, "settled at the timeout");
      const ac = new AbortController();
      setTimeout(() => ac.abort(), 100);
      started = Date.now();
      await assert.rejects(gc.readBare(bare, slow, { maxBytes: 1024, signal: ac.signal }), /aborted/);
      assert.ok(Date.now() - started < 2_500, "settled at the abort");
    });
  });

  it("a timeout kills the child: the aliased sleep is gone well before it would have finished", async () => {
    await withBare({ "a.txt": "a" }, async (gc, bare) => {
      const pidFile = path.join(path.dirname(bare), "alias.pid");
      // A literal, inert alias: record the shell's pid, then become a long sleep under that pid.
      const slow = ["-c", `alias.wait=!echo $$ > '${pidFile}'; exec sleep 10`, "wait"];
      const started = Date.now();
      await assert.rejects(gc.readBare(bare, slow, { maxBytes: 1024, timeoutMs: 500 }), /exceeded 500ms/);
      // The shell writes the pid file itself, so poll for a complete line rather than assume it
      // is on disk the moment the read rejects.
      let recorded = "";
      while (!recorded.endsWith("\n") && Date.now() - started < 3_000) {
        recorded = fs.existsSync(pidFile) ? fs.readFileSync(pidFile, "utf8") : "";
        if (!recorded.endsWith("\n")) await delay(25);
      }
      const pid = Number(recorded.trim());
      assert.ok(pid > 0, "the alias recorded its pid");
      // Gone within 3 s of the start; the sleep alone lasts 10 s.
      while (processAlive(pid) && Date.now() - started < 3_000) await delay(25);
      assert.equal(processAlive(pid), false, "the child's sleep was terminated at the timeout");
    });
  });

  it("keeps at most 64 KiB of git's stderr in a failure message", async () => {
    await withBare({ "a.txt": "a" }, async (gc, bare) => {
      // A literal, inert alias: 300,000 bytes of stderr, then a non-zero exit.
      const noisy = ["-c", "alias.noisy=!head -c 300000 /dev/zero | tr '\\0' e >&2; exit 3", "noisy"];
      const err = await gc.readBare(bare, noisy, { maxBytes: 1024 }).then(
        () => assert.fail("a non-zero exit rejects"),
        (e: unknown) => e as Error,
      );
      assert.match(err.message, /exited 3: e{1000}/);
      assert.ok(err.message.length <= 64 * 1024 + 200, `stderr capped (${err.message.length})`);
    });
  });

  it("throws up front on a non-finite or negative maxBytes and a non-finite timeoutMs", async () => {
    await withBare({ "a.txt": "a" }, async (gc, bare) => {
      for (const maxBytes of [Number.NaN, -1, Number.POSITIVE_INFINITY]) {
        await assert.rejects(gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes }), /maxBytes must be/, String(maxBytes));
      }
      await assert.rejects(
        gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes: 1, timeoutMs: Number.POSITIVE_INFINITY }),
        /timeoutMs must be finite/,
      );
      await assert.rejects(gc.readBare(bare, ["show", "HEAD:a.txt"], { maxBytes: 1, timeoutMs: Number.NaN }), /no time left/);
    });
  });

  it("inside a boundary scope, forwards the read's timeout (default GIT_TIMEOUT_MS) to the spawner", async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-readbare-b-"));
    const gc = new GitCache(root, nullLogger(), undefined, testGitCacheOptions());
    const requests: BoundaryProcessRequest[] = [];
    const spawner: BoundaryProcessSpawner = async (request) => {
      requests.push(request);
      const stdout = new PassThrough();
      stdout.end("hello");
      return { stdin: null, stdout, stderr: null, cancel: async () => {}, completed: Promise.resolve({ code: 0 }) };
    };
    const ac = new AbortController();
    try {
      const reads = await gc.withBoundaryProcessSpawner(spawner, ac.signal, async () => [
        await gc.readBare("/bare.git", ["show", "HEAD:a.txt"], { maxBytes: 1024, timeoutMs: 1_234 }),
        await gc.readBare("/bare.git", ["show", "HEAD:a.txt"], { maxBytes: 1024 }),
      ]);
      assert.deepEqual(reads, [
        { text: "hello", truncated: false },
        { text: "hello", truncated: false },
      ]);
      assert.deepEqual(
        requests.map((r) => [r.timeoutMs, r.identity, r.argv.slice(-2)]),
        [
          [1_234, "worker_pat", ["show", "HEAD:a.txt"]],
          [10 * 60_000, "worker_pat", ["show", "HEAD:a.txt"]],
        ],
      );
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
});

/** Is `pid` still a live process? (Signal 0 probes without sending anything.) */
function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

// PRD #1798 finalize (CodeRabbit on #1825): the delivery context reads the linked PRD from the bare
// snapshot at the described head, never from the agent-writable worktree, where a FIFO at the PRD
// path would block an open forever.
describe("resolvePrdInputFromBare", () => {
  it("reads the linked PRD at the given head from the bare, bounded, and never touches a worktree", async () => {
    await withBare({ "prds.md": "x" }, async (gc, bare) => {
      // A second commit adds the PRD, so the read is pinned to a real sha, not HEAD by accident.
      const work = path.join(path.dirname(bare), "work");
      fs.mkdirSync(path.join(work, "prds"));
      fs.writeFileSync(path.join(work, "prds", "1798-x.md"), "# The PRD\n");
      git(work, "add", "-A");
      git(work, "commit", "-qm", "two");
      const head = git(work, "rev-parse", "HEAD").trim();
      git(path.dirname(bare), "--git-dir", bare, "fetch", "-q", work, "main:main");
      // A worktree whose PRD path is a FIFO: opening it would block. The bare read never opens it.
      const wt = path.join(path.dirname(bare), "wt");
      fs.mkdirSync(path.join(wt, "prds"), { recursive: true });
      execFileSync("mkfifo", [path.join(wt, "prds", "1798-x.md")]);

      const r = await resolvePrdInputFromBare(gc, bare, head, "implements prds/1798-x.md", 1798, { timeoutMs: 30_000 });
      assert.deepEqual(r, { prdPath: "prds/1798-x.md", prdText: "# The PRD\n" });

      // No PRD link, a non-sha head, a missing file, or no time left: the nulls fallback, no throw.
      const none = { prdPath: null, prdText: null };
      assert.deepEqual(await resolvePrdInputFromBare(gc, bare, head, "no link here", 1798, { timeoutMs: 30_000 }), none);
      assert.deepEqual(await resolvePrdInputFromBare(gc, bare, "HEAD", "implements prds/1798-x.md", 1798, { timeoutMs: 30_000 }), none);
      assert.deepEqual(await resolvePrdInputFromBare(gc, bare, head, "implements prds/1798-missing.md", 1798, { timeoutMs: 30_000 }), none);
      assert.deepEqual(await resolvePrdInputFromBare(gc, bare, head, "implements prds/1798-x.md", 1798, { timeoutMs: 0 }), none);
    });
  });
});
