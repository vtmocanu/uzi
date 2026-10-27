import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { recordingLogger, testGitCacheOptions } from "./helpers.js";
import { BareCacheShallowError, GitCache, gitEnv } from "../src/git.js";
import { classifyForgeError, withForgeRetry } from "../src/forge-retry.js";

// Issue #3450: the bare cache refuses a shallow repository, both when `cloneBare` creates it
// from a shallow source and when `ensureClone` would reuse an existing shallow bare.

let fx: Fixture;
let shallowSource: string;

function gitIn(cwd: string, args: string[]): string {
  return execFileSync("git", args, { cwd, env: gitEnv(), encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
}

beforeEach(() => {
  fx = makeFixture();
  // A second commit, so a depth-1 clone really truncates history (a lone root commit is not shallow).
  fs.writeFileSync(path.join(fx.originPath, "second.txt"), "second\n");
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, ["-c", "user.email=fixture@uzi.local", "-c", "user.name=fixture", "commit", "-m", "second"]);
  shallowSource = path.join(path.dirname(fx.originPath), "shallow-src");
  gitIn(path.dirname(fx.originPath), ["clone", "--no-local", "--depth", "1", "--", fx.originPath, shallowSource]);
  gitIn(shallowSource, ["config", "maintenance.auto", "false"]);
  assert.equal(gitIn(shallowSource, ["rev-parse", "--is-shallow-repository"]), "true");
});

afterEach(() => fx.cleanup());

/** Every file under `root` with its size and sha256, sorted, plus the bare's refs. */
function snapshot(root: string): string {
  const out: string[] = [];
  const walk = (dir: string): void => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const full = path.join(dir, entry.name);
      const rel = path.relative(root, full);
      if (entry.isDirectory()) {
        out.push(`d ${rel}`);
        walk(full);
      } else {
        const data = fs.readFileSync(full);
        out.push(`f ${rel} ${data.length} ${createHash("sha256").update(data).digest("hex")}`);
      }
    }
  };
  walk(root);
  out.push(gitIn(root, ["for-each-ref"]));
  return out.join("\n");
}

it("refuses to clone a bare cache from a shallow source and leaves no destination", async () => {
  const { logger } = recordingLogger();
  let sleeps = 0;
  const git = new GitCache(fx.dataDir, logger, { schedule: [1, 1], sleep: async () => { sleeps++; } }, testGitCacheOptions());
  const bare = git.barePathFor(shallowSource);
  await assert.rejects(git.ensureClone(shallowSource), (err: unknown) => {
    assert.ok(err instanceof BareCacheShallowError, `expected BareCacheShallowError, got ${String(err)}`);
    assert.equal(err.message, "repo cache refused: shallow repository");
    return true;
  });
  assert.equal(fs.existsSync(bare), false, "the refused bare must be removed");
  assert.equal(sleeps, 0, "a shallow refusal is not retried");
});

it("refuses to reuse an existing shallow bare and leaves it exactly as found, with no fetch", async () => {
  const { logger, lines } = recordingLogger();
  const git = new GitCache(fx.dataDir, logger, undefined, testGitCacheOptions());
  const bare = git.barePathFor(shallowSource);
  fs.mkdirSync(path.dirname(bare), { recursive: true });
  gitIn(fx.dataDir, ["clone", "--bare", "--", shallowSource, bare]);
  assert.ok(fs.existsSync(path.join(bare, "shallow")), "fixture bare must carry a shallow file");
  const before = snapshot(bare);

  await assert.rejects(git.ensureClone(shallowSource), BareCacheShallowError);

  assert.equal(snapshot(bare), before);
  const records = lines as { level?: string; msg?: string; args?: string[] }[];
  assert.equal(records.some((r) => r.msg === "repo cache: fetching"), false);
  const gitCalls = records.filter((r) => r.msg === "git" && Array.isArray(r.args));
  assert.equal(gitCalls.some((r) => r.args!.includes("fetch")), false, "no fetch may be attempted");
});

it("a non-shallow source still clones and reuses", async () => {
  const git = new GitCache(fx.dataDir, recordingLogger().logger, undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fx.originPath);
  assert.equal(gitIn(bare, ["rev-parse", "--is-shallow-repository"]), "false");
  assert.equal(await git.ensureClone(fx.originPath), bare);
});

it("BareCacheShallowError is permanent and withForgeRetry makes one attempt", async () => {
  assert.equal(classifyForgeError(new BareCacheShallowError()), "permanent");
  let attempts = 0;
  let sleeps = 0;
  await assert.rejects(
    withForgeRetry(async () => {
      attempts++;
      throw new BareCacheShallowError();
    }, { schedule: [1, 1], sleep: async () => { sleeps++; } }),
    BareCacheShallowError,
  );
  assert.equal(attempts, 1);
  assert.equal(sleeps, 0);
});
