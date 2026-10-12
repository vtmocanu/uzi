import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";

// issue #2779 — GitCache.publishedCommitMessages over REAL repos: the commit messages a push of
// a tip would publish, read from the worker bare against its default ref. Any failure to read the
// range is null (callers refuse), never an empty list.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8" }).trim();
}

function commit(repo: string, file: string, message: string): string {
  fs.writeFileSync(path.join(repo, file), `${message}\n${Math.random()}\n`);
  git(repo, "add", ".");
  git(repo, ...IDENT, "commit", "-m", message);
  return git(repo, "rev-parse", "HEAD");
}

let fx: Fixture;
let gc: GitCache;

beforeEach(() => {
  fx = makeFixture();
  gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
});
afterEach(() => fx.cleanup());

/** Bare clone of origin, then pull `branch` from origin into a throwaway ref and return its tip. */
async function bareWith(branch: string): Promise<{ bare: string; tip: string }> {
  const bare = await gc.ensureClone(fx.originPath);
  execFileSync("git", ["-C", bare, "fetch", "--refmap=", "origin", `+refs/heads/${branch}:refs/uzi-test/${branch}`], { env: ENV });
  return { bare, tip: git(bare, "rev-parse", `refs/uzi-test/${branch}`) };
}

describe("publishedCommitMessages (issue #2779)", () => {
  it("returns every branch commit with its full multi-line message, newest first", async () => {
    git(fx.originPath, "checkout", "-b", "work");
    const a = commit(fx.originPath, "a.txt", "first subject\n\nbody line one\n\nbody para two\nFixes #2172");
    const b = commit(fx.originPath, "b.txt", "second subject");
    git(fx.originPath, "checkout", "main");
    const { bare, tip } = await bareWith("work");
    const out = await gc.publishedCommitMessages(bare, tip);
    assert.ok(out);
    assert.deepStrictEqual(out.map((c) => c.sha), [b, a]);
    assert.ok(out[1]!.message.startsWith("first subject\n\nbody line one\n\nbody para two\nFixes #2172"));
    assert.ok(out[0]!.message.startsWith("second subject"));
  });

  it("includes a merge commit's own message", async () => {
    git(fx.originPath, "checkout", "-b", "work");
    const w1 = commit(fx.originPath, "w.txt", "work one");
    git(fx.originPath, "checkout", "-b", "side", "main");
    const s1 = commit(fx.originPath, "s.txt", "side one");
    git(fx.originPath, "checkout", "work");
    git(fx.originPath, ...IDENT, "merge", "--no-ff", "-m", "Merge side\n\nCloses #2172", "side");
    const merge = git(fx.originPath, "rev-parse", "HEAD");
    git(fx.originPath, "checkout", "main");
    const { bare, tip } = await bareWith("work");
    const out = await gc.publishedCommitMessages(bare, tip);
    assert.ok(out);
    assert.deepStrictEqual(new Set(out.map((c) => c.sha)), new Set([merge, w1, s1]));
    assert.ok(out.find((c) => c.sha === merge)!.message.includes("Closes #2172"));
  });

  it("returns [] (not null) for an empty range", async () => {
    const bare = await gc.ensureClone(fx.originPath);
    const tip = git(bare, "rev-parse", "refs/remotes/origin/main");
    assert.deepStrictEqual(await gc.publishedCommitMessages(bare, tip), []);
  });

  it("returns null for an invalid tip, an absent object, and an unresolvable repo", async () => {
    const bare = await gc.ensureClone(fx.originPath);
    assert.strictEqual(await gc.publishedCommitMessages(bare, "not-a-sha"), null);
    assert.strictEqual(await gc.publishedCommitMessages(bare, "refs/heads/main"), null);
    assert.strictEqual(await gc.publishedCommitMessages(bare, "f".repeat(40)), null);
    assert.strictEqual(await gc.publishedCommitMessages(path.join(fx.dataDir, "no-such-repo"), "a".repeat(40)), null);
  });

  it("returns null for a commit whose message hides text behind a NUL byte", async () => {
    git(fx.originPath, "checkout", "-b", "work");
    commit(fx.originPath, "a.txt", "clean one");
    const parent = git(fx.originPath, "rev-parse", "HEAD");
    const tree = git(fx.originPath, "rev-parse", "HEAD^{tree}");
    // A crafted commit object: git log's %B stops at the NUL, so "Fixes #7" would be invisible.
    const body = Buffer.from(
      `tree ${tree}\nparent ${parent}\nauthor t <t@t> 0 +0000\ncommitter t <t@t> 0 +0000\n\nhead\u0000Fixes #7\n`,
    );
    const crafted = execFileSync("git", ["-C", fx.originPath, "hash-object", "-t", "commit", "-w", "--literally", "--stdin"],
      { env: ENV, input: body, encoding: "utf8" }).trim();
    git(fx.originPath, "update-ref", "refs/heads/nul", crafted);
    git(fx.originPath, "checkout", "main");
    const { bare, tip } = await bareWith("nul");
    assert.strictEqual(tip, crafted);
    assert.strictEqual(await gc.publishedCommitMessages(bare, tip), null);
  });

  /** Write a commit object on top of `main` with an `encoding` header and the given body. */
  function craftEncoded(branch: string, encoding: string, msg: string): string {
    const parent = git(fx.originPath, "rev-parse", "main");
    const tree = git(fx.originPath, "rev-parse", "main^{tree}");
    const body = Buffer.from(
      `tree ${tree}\nparent ${parent}\nauthor t <t@t> 0 +0000\ncommitter t <t@t> 0 +0000\nencoding ${encoding}\n\n${msg}\n`,
    );
    const sha = execFileSync("git", ["-C", fx.originPath, "hash-object", "-t", "commit", "-w", "--literally", "--stdin"],
      { env: ENV, input: body, encoding: "utf8" }).trim();
    git(fx.originPath, "update-ref", `refs/heads/${branch}`, sha);
    return sha;
  }

  it("returns null for a commit with a non-UTF-8 encoding header (%B would print an empty or garbled body)", async () => {
    const crafted = craftEncoded("enc16", "UTF-16", "Fixes #12");
    const { bare, tip } = await bareWith("enc16");
    assert.strictEqual(tip, crafted);
    assert.strictEqual(await gc.publishedCommitMessages(bare, tip), null);
  });

  it("still reads a commit that declares encoding UTF-8", async () => {
    const crafted = craftEncoded("enc8", "UTF-8", "clean subject");
    const { bare, tip } = await bareWith("enc8");
    const out = await gc.publishedCommitMessages(bare, tip);
    assert.ok(out);
    assert.deepStrictEqual(out.map((c) => c.sha), [crafted]);
    assert.ok(out[0]!.message.startsWith("clean subject"));
  });

  it("returns null for a commit with a non-UTF-8 encoding header such as ISO-8859-1", async () => {
    craftEncoded("enc1", "ISO-8859-1", "Fixes #12");
    const { bare, tip } = await bareWith("enc1");
    assert.strictEqual(await gc.publishedCommitMessages(bare, tip), null);
  });

  it("returns null when a newer message ends in a forged cat-file frame hiding a UTF-16 parent", async () => {
    // Parent: raw ASCII body under `encoding UTF-16`. Child: a message with no trailing newline
    // ending in a forged `<40 zeros> commit 1` frame plus `encoding UTF-8`, which swallowed the
    // parent's real header in a frame-parsing implementation.
    const parent = craftEncoded("forge", "UTF-16", "Fixes #12");
    const tree = git(fx.originPath, "rev-parse", "main^{tree}");
    const msg = `chore: tidy\n\n${"0".repeat(40)} commit 1\nencoding UTF-8`;
    const body = Buffer.from(`tree ${tree}\nparent ${parent}\nauthor t <t@t> 0 +0000\ncommitter t <t@t> 0 +0000\n\n${msg}`);
    const child = execFileSync("git", ["-C", fx.originPath, "hash-object", "-t", "commit", "-w", "--literally", "--stdin"],
      { env: ENV, input: body, encoding: "utf8" }).trim();
    git(fx.originPath, "update-ref", "refs/heads/forge", child);
    const { bare, tip } = await bareWith("forge");
    assert.strictEqual(tip, child);
    assert.strictEqual(await gc.publishedCommitMessages(bare, tip), null);
  });

  for (const answer of ["missing", "malformed"] as const) {
    it(`refuses when the raw commit read is ${answer} even though git exits zero`, async () => {
      git(fx.originPath, "checkout", "-b", "work");
      const c = commit(fx.originPath, "a.txt", "plain subject");
      git(fx.originPath, "checkout", "main");
      const { bare, tip } = await bareWith("work");
      assert.ok(await gc.publishedCommitMessages(bare, tip), "control: the clean commit reads");
      const original = Reflect.get(gc, "execScoped").bind(gc);
      Reflect.set(gc, "execScoped", async (command: string, args: string[], options: unknown) => {
        if (command === "git" && args.includes("cat-file")) {
          return { stdout: answer === "missing" ? `${c} missing\n` : "author t <t@t> 0 +0000\n\nplain subject\n", stderr: "" };
        }
        return original(command, args, options);
      });
      assert.strictEqual(await gc.publishedCommitMessages(bare, tip), null);
    });
  }
});
