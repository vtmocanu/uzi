// PRD #1798 M5: GitCache.readBare, the bounded bare read behind the PR-description context
// (pr-description-context.ts). Real git over a real bare repository.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { GitCache } from "../src/git.js";
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
});
