import test, { type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { BareCacheIdentityError, GitCache } from "../src/git.js";
import { nullLogger } from "./helpers.js";

const linux = { skip: process.platform !== "linux" };
const env = {
  PATH: "/usr/bin:/bin", GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0", GIT_AUTHOR_NAME: "fixture", GIT_AUTHOR_EMAIL: "fixture@example.invalid",
  GIT_COMMITTER_NAME: "fixture", GIT_COMMITTER_EMAIL: "fixture@example.invalid",
  GIT_AUTHOR_DATE: "2000-01-01T00:00:00Z", GIT_COMMITTER_DATE: "2000-01-01T00:00:00Z",
};
function git(cwd: string, args: string[], input?: string): string {
  return execFileSync("/usr/bin/git", ["--no-replace-objects", "-C", cwd, ...args],
    { env, input, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"], timeout: 5000 }).trim();
}
async function fixture(t: TestContext, hashed = false) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "canonical-probe-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const cache = new GitCache(path.join(root, "data"), nullLogger());
  const url = "https://example.invalid/" + (hashed ? "a".repeat(300) : "team") + "/repo.git";
  const bare = cache.barePathFor(url);
  await fs.mkdir(path.dirname(bare), { recursive: true, mode: 0o700 });
  git(root, ["init", "--bare", "-q", "-b", "main", bare]);
  await fs.chmod(bare, 0o700);
  git(bare, ["config", "remote.origin.url", url]);
  git(bare, ["config", "uzi.cacheOrigin", url]);
  const tree = git(bare, ["mktree"], "");
  const head = git(bare, ["commit-tree", tree, "-m", "fixture"]);
  git(bare, ["update-ref", "refs/heads/main", head]);
  assert.equal(path.basename(bare).startsWith("uzi-sha256-"), hashed);
  return { root, cache, url, bare, head };
}

type Fixture = Awaited<ReturnType<typeof fixture>>;
type Probe = "owed" | "recovery" | "resolver" | "hashed resolver" | "hashed identity" | "epoch discovery";
const probes: Probe[] = ["owed", "recovery", "resolver", "hashed resolver", "hashed identity", "epoch discovery"];

function prohibitGit(t: TestContext, cache: GitCache): string[][] {
  const calls: string[][] = [];
  const native = cache as unknown as { runGit(cwd: string | undefined, args: string[]): Promise<string> };
  t.mock.method(native, "runGit", async (_cwd: string | undefined, args: string[]) => {
    calls.push([...args]);
    throw new Error("unsafe metadata Git was dispatched");
  });
  return calls;
}

async function assertRefused(f: Fixture, probe: Probe): Promise<void> {
  if (probe === "owed") {
    await assert.rejects(f.cache.produceRecoveryBundle(f.bare, {
      sourceSha: f.head, outPath: path.join(f.root, "refused.bundle"), guardedDefaultBranch: "main",
    }), { message: "maintenance_source_layout_unsupported" });
  } else if (probe === "recovery") {
    await assert.rejects(f.cache.maintenanceOriginDigest(f.bare, f.url), { message: "invalid recovery bare" });
  } else if (probe === "hashed identity") {
    await assert.rejects(f.cache.existingBarePathFor(f.url), BareCacheIdentityError);
  } else if (probe === "epoch discovery") {
    const result = await f.cache.discoverEpochRetainedSources();
    assert.equal(result.inventoryComplete, false);
    assert.ok(result.unreadable > 0);
  } else {
    assert.equal(await f.cache.resolveRecoveryBareDir(path.basename(f.bare)), undefined);
  }
}

for (const probe of probes) for (const kind of ["FIFO", "regular with nested FIFO"] as const) {
  test(`${probe} refuses ${kind} before metadata Git`, linux, async t => {
    const f = await fixture(t, probe.startsWith("hashed"));
    const control = path.join(f.bare, "objects", "info", "alternates");
    const target = path.join(f.root, "foreign-objects");
    await fs.mkdir(path.dirname(control), { recursive: true });
    if (kind === "FIFO") execFileSync("/usr/bin/mkfifo", [control]);
    else {
      await fs.mkdir(path.join(target, "info"), { recursive: true });
      execFileSync("/usr/bin/mkfifo", [path.join(target, "info", "alternates")]);
      await fs.writeFile(control, target + "\n");
    }
    const before = await fs.lstat(control, { bigint: true });
    const forbidden = (file: unknown) => typeof file === "string" &&
      (file === control || file === target || file.startsWith(target + "/"));
    const open = fs.open.bind(fs), read = fs.readFile.bind(fs), lstat = fs.lstat.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      assert.equal(forbidden(args[0]), false, "no alternate control or target is opened");
      return open(...args);
    });
    t.mock.method(fs, "readFile", async (...args: Parameters<typeof fs.readFile>) => {
      assert.equal(forbidden(args[0]), false, "no alternate control or target is read");
      return read(...args);
    });
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
      assert.equal(typeof args[0] === "string" && (args[0] === target || args[0].startsWith(target + "/")), false,
        "the referenced store is not traversed");
      return lstat(...args);
    });
    const calls = prohibitGit(t, f.cache);
    await assertRefused(f, probe);
    assert.deepEqual(calls, [], "unsupported controls must be refused before Git initializes its object database");
    const after = await fs.lstat(control, { bigint: true });
    assert.deepEqual([after.dev, after.ino, after.mode], [before.dev, before.ino, before.mode]);
    await assert.rejects(fs.lstat(path.join(f.root, "refused.bundle")), { code: "ENOENT" });
  });
}

for (const kind of ["symlink", "directory", "empty regular", "http-alternates"] as const) {
  test(`owed refuses ${kind} control before metadata Git`, linux, async t => {
    const f = await fixture(t);
    const control = path.join(f.bare, "objects", "info", kind === "http-alternates" ? kind : "alternates");
    await fs.mkdir(path.dirname(control), { recursive: true });
    if (kind === "symlink") await fs.symlink(path.join(f.root, "absent"), control);
    else if (kind === "directory") await fs.mkdir(control);
    else await fs.writeFile(control, "");
    const calls = prohibitGit(t, f.cache);
    await assertRefused(f, "owed");
    assert.deepEqual(calls, []);
  });
}

test("canonical control stat errors deny metadata Git", linux, async t => {
  const f = await fixture(t);
  const control = path.join(f.bare, "objects", "info", "alternates");
  const lstat = fs.lstat.bind(fs);
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (args[0] === control) throw Object.assign(new Error("denied"), { code: "EACCES" });
    return lstat(...args);
  });
  const calls = prohibitGit(t, f.cache);
  await assert.rejects(f.cache.produceRecoveryBundle(f.bare, {
    sourceSha: f.head, outPath: path.join(f.root, "refused.bundle"), guardedDefaultBranch: "main",
  }), { code: "EACCES" });
  assert.deepEqual(calls, []);
});

for (const hashed of [false, true]) {
  test(`valid canonical cache still supports metadata and capture, hashed=${hashed}`, linux, async t => {
    const f = await fixture(t, hashed);
    assert.equal(await f.cache.resolveRecoveryBareDir(path.basename(f.bare)), f.bare);
    assert.equal(await f.cache.existingBarePathFor(f.url), f.bare);
    assert.match((await f.cache.maintenanceOriginDigest(f.bare, f.url))!, /^[a-f0-9]{64}$/);
    const result = await f.cache.produceRecoveryBundle(f.bare, {
      sourceSha: f.head, outPath: path.join(f.root, "valid.bundle"), guardedDefaultBranch: "main", maxBytes: 4096,
    });
    assert.equal(result.selfContained, true);
    assert.equal(result.sourceSha, f.head);
  });
}
