import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { deflateSync } from "node:zlib";
import { GitCache } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

let fx: Fixture, bare: string, base: string, tip: string, cache: GitCache;
let baseOids: string[], tipOids: string[], uniqueBytes: number, baseBytes: number, limit: number;
function gitEnv(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env };
  for (const key of Object.keys(env)) if (key.startsWith("GIT_")) delete env[key];
  env.GIT_CONFIG_GLOBAL = "/dev/null";
  env.GIT_CONFIG_SYSTEM = "/dev/null";
  env.GIT_TERMINAL_PROMPT = "0";
  return env;
}
function git(dir: string, args: string[], input?: string): string {
  return execFileSync("git", ["-C", dir, ...args], { env: gitEnv(), encoding: "utf8", input, stdio: "pipe" }).trim();
}
function closure(sha: string): string[] {
  return git(bare, ["rev-list", "--objects", "--no-object-names", "--missing=error", sha]).split("\n");
}
function decoded(oids: string[]): number {
  return git(bare, ["cat-file", "--batch-check"], oids.join("\n") + "\n")
    .split("\n").reduce((sum, row) => sum + Number(row.split(" ")[2]), 0);
}
function budget(): { delivered: number; limit: number } {
  return (cache as unknown as { recoveryOperations: { getStore(): { closureBudget: { delivered: number; limit: number } } } })
    .recoveryOperations.getStore().closureBudget;
}
function corruptBlob(dir: string, oid: string): void {
  const size = Number(git(dir, ["cat-file", "-s", oid]));
  const target = path.join(dir, "objects", oid.slice(0, 2), oid.slice(2));
  fs.mkdirSync(path.dirname(target), { recursive: true });
  // Keep the same declared type and size, so only content verification catches this.
  const bytes = Buffer.concat([Buffer.from(`blob ${size}\0`), Buffer.alloc(size, 0x5a)]);
  if (fs.existsSync(target)) fs.chmodSync(target, 0o600);
  fs.writeFileSync(target, deflateSync(bytes));
}
async function operation(action: () => Promise<void>): Promise<void> {
  await cache.withRecoveryOperation(new AbortController().signal, Date.now() + 10_000, action);
}

beforeEach(async () => {
  fx = makeFixture({ "a-shared.txt": "shared-history\n".repeat(10_000) });
  const bootstrap = new GitCache(fx.dataDir, nullLogger());
  bare = await bootstrap.ensureClone(fx.originPath);
  base = git(bare, ["rev-parse", "refs/remotes/origin/main"]);
  fs.writeFileSync(path.join(fx.originPath, "delta.txt"), "small run-local delta\n");
  git(fx.originPath, ["add", "delta.txt"]);
  git(fx.originPath, ["commit", "-m", "small delta"]);
  await bootstrap.ensureClone(fx.originPath);
  tip = git(bare, ["rev-parse", "refs/remotes/origin/main"]);
  baseOids = closure(base);
  tipOids = closure(tip);
  baseBytes = decoded(baseOids);
  uniqueBytes = decoded([...new Set([...baseOids, ...tipOids])]);
  limit = uniqueBytes + 8192;
  assert.ok(uniqueBytes < limit && limit < 6 * baseBytes);
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: limit });
});
afterEach(() => fx.cleanup());

test("N+1 shared-history checks charge only the union of verified decoded bytes", async () => {
  await operation(async () => {
    for (let n = 0; n < 5; n++) assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
    assert.equal(await cache.verifyRecoveryClosure(bare, tip), true);
    assert.equal(budget().delivered, uniqueBytes);
  });
});

test("credential-switch 3k+N shared-closure sequence stays below the union budget", async () => {
  const k = 2, prerequisites = 3;
  assert.ok(uniqueBytes < limit && limit < (3 * k + prerequisites) * baseBytes);
  const sources = [base, tip];
  await operation(async () => {
    let checks = 0;
    for (let proof = 0; proof < 3; proof++) {
      for (const source of sources) { assert.equal(await cache.verifyRecoveryClosure(bare, source), true); checks++; }
    }
    for (let n = 0; n < prerequisites; n++) { assert.equal(await cache.verifyRecoveryClosure(bare, base), true); checks++; }
    assert.equal(checks, 3 * k + prerequisites);
    assert.equal(budget().delivered, uniqueBytes);
  });
});

test("nested operations share verified objects and unique-byte accounting", async () => {
  await operation(async () => {
    const outer = budget();
    assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
    await operation(async () => {
      assert.equal(budget(), outer);
      assert.equal(await cache.verifyRecoveryClosure(bare, tip), true);
      assert.equal(outer.delivered, uniqueBytes);
    });
  });
});

test("a previously hashed missing object is still refused on the next complete walk", async () => {
  // Materialize a loose-only fixture so deleting this object truly removes it.
  const looseSha = git(bare, ["hash-object", "-w", "--stdin"], "loose-only content\n");
  const tree = git(bare, ["mktree"], `100644 blob ${looseSha}\tonly.txt\n`);
  const looseTip = git(bare, ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-m", "loose root"]);
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, looseTip), true);
    fs.unlinkSync(path.join(bare, "objects", looseSha.slice(0, 2), looseSha.slice(2)));
    assert.equal(await cache.verifyRecoveryClosure(bare, looseTip), false);
  });
});

test("a failed verification publishes no trust for objects hashed before the corrupt frame", async () => {
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: uniqueBytes * 4 });
  fs.writeFileSync(path.join(fx.originPath, "x-bad.txt"), "later object\n");
  git(fx.originPath, ["add", "x-bad.txt"]);
  git(fx.originPath, ["commit", "-m", "later corrupt object"]);
  await cache.ensureClone(fx.originPath);
  const badTip = git(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const shared = git(bare, ["rev-parse", `${badTip}:a-shared.txt`]);
  const bad = git(bare, ["rev-parse", `${badTip}:x-bad.txt`]);
  const ordering = closure(badTip);
  assert.ok(ordering.indexOf(shared) < ordering.indexOf(bad), "A must be hashed before corrupt X");
  corruptBlob(bare, bad);
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, badTip), false);
    corruptBlob(bare, shared);
    assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
  });
});

test("separate bares never share verified objects, even with identical shared object ids", async () => {
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: uniqueBytes * 4 });
  const other = bare + ".other";
  fs.cpSync(bare, other, { recursive: true });
  const shared = git(other, ["rev-parse", `${base}:a-shared.txt`]);
  corruptBlob(other, shared);
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
    assert.equal(await cache.verifyRecoveryClosure(other, base), false);
  });
});

test("replacing a bare at the same pathname invalidates its verified-object cache", async () => {
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: uniqueBytes * 4 });
  const shared = git(bare, ["rev-parse", `${base}:a-shared.txt`]);
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
    const old = bare + ".old";
    fs.renameSync(bare, old);
    fs.cpSync(old, bare, { recursive: true });
    corruptBlob(bare, shared);
    assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
  });
});

test("a fresh operation starts empty and detects corruption after the previous operation", async () => {
  await operation(async () => { assert.equal(await cache.verifyRecoveryClosure(bare, base), true); });
  corruptBlob(bare, git(bare, ["rev-parse", `${base}:a-shared.txt`]));
  await operation(async () => { assert.equal(await cache.verifyRecoveryClosure(bare, base), false); });
});

for (const mismatch of ["size", "type"] as const) {
  test(`cached ${mismatch} mismatch refuses without trusting changed object headers`, async () => {
    await operation(async () => {
      assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
      const shared = git(bare, ["rev-parse", `${base}:a-shared.txt`]);
      const target = path.join(bare, "objects", shared.slice(0, 2), shared.slice(2));
      if (fs.existsSync(target)) fs.chmodSync(target, 0o600);
      fs.writeFileSync(target, deflateSync(Buffer.from(mismatch === "size" ? "blob 1\0x" : "tree 0\0")));
      assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
    });
  });
}

test("identity replacement during producer settlement refuses and publishes no entries", async () => {
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const spawn = seam.spawnGit.bind(cache);
  let replaced = false;
  seam.spawnGit = async (...args) => {
    const producer = await spawn(...args);
    const exited = producer.exited;
    producer.exited = exited.then((status: number) => {
      if (!replaced) {
        replaced = true;
        const old = bare + ".pre-exit";
        fs.renameSync(bare, old);
        fs.cpSync(old, bare, { recursive: true });
        corruptBlob(bare, git(bare, ["rev-parse", `${base}:a-shared.txt`]));
      }
      return status;
    });
    return producer;
  };
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
    assert.equal(replaced, true);
    const scope = (cache as any).recoveryOperations.getStore();
    assert.equal(scope.closureCache.entries, 0);
  });
});

test("total operation cache saturation stops caching and still re-hashes misses", async () => {
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: uniqueBytes * 4 });
  await operation(async () => {
    const scope = (cache as any).recoveryOperations.getStore();
    // Simulate earlier identities occupying all but one entry, without allocating a huge fixture.
    const cap = Math.floor(64 * 1024 * 1024 / 41);
    scope.closureCache.entries = cap - 1;
    assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
    assert.equal(scope.closureCache.entries, cap);
    const entries = [...scope.closureCache.identities.values()][0] as Map<string, unknown>;
    assert.equal(entries.size, 1);
    const shared = git(bare, ["rev-parse", `${base}:a-shared.txt`]);
    assert.equal(entries.has(shared), false, "uncached blob is not verification authority");
    corruptBlob(bare, shared);
    assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
    assert.equal(scope.closureCache.entries, cap);
  });
});

test("unique content over the remaining decoded budget still fails closed", async () => {
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: baseBytes + 1 });
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, base), true);
    await assert.rejects(cache.verifyRecoveryClosure(bare, tip), { message:
      `reachable recovery history exceeds the ${baseBytes + 1} bytes decoded verification limit` });
    assert.equal(budget().delivered, baseBytes, "declared-size refusal adds no unread bytes");
  });
});

test("fully verified frames with a failed producer exit publish no cached trust", async () => {
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, { recoveryDecodedLimitBytes: uniqueBytes * 4 });
  const seam = cache as unknown as { spawnGit: (...args: any[]) => Promise<any> };
  const spawn = seam.spawnGit.bind(cache);
  let first = true;
  seam.spawnGit = async (...args) => {
    const producer = await spawn(...args);
    if (first) { first = false; producer.exited = producer.exited.then(() => 1); }
    return producer;
  };
  await operation(async () => {
    assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
    assert.equal((cache as any).recoveryOperations.getStore().closureCache.entries, 0);
    corruptBlob(bare, git(bare, ["rev-parse", `${base}:a-shared.txt`]));
    assert.equal(await cache.verifyRecoveryClosure(bare, base), false);
  });
});
