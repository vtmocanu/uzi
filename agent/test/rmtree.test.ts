import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { rmTreeForce, restoreTreeWritability } from "../src/rmtree.js";

/**
 * PRD #108 M6. `fs.rm(recursive, force)` cannot delete a tree the Go module cache
 * wrote — `force` suppresses ENOENT, not EACCES — so every Go-touching run
 * stranded its whole $HOME (167.3 MB measured for one run).
 *
 * The first test here is the POSITIVE CONTROL: it pins the unfixed behaviour, so
 * the fix cannot be "verified" against a fixture that was never hostile.
 */

/** Running as root defeats the entire fixture: root ignores the permission bits
 *  and `fs.rm` would just succeed, so the control would pass vacuously and the
 *  fix's test would prove nothing about EACCES. Report it rather than skip
 *  silently — a suite that quietly stops testing the thing is worse than a red. */
const asRoot = process.getuid?.() === 0;

/** `<root>/go/pkg/mod/gopkg.in/inf.v0@v0.9.1/benchmark_test.go` inside a `0555`
 *  directory — the exact shape from the incident log. Returns the read-only dir. */
async function makeGoModCacheFixture(root: string): Promise<string> {
  const mod = path.join(root, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
  await fs.mkdir(mod, { recursive: true });
  await fs.writeFile(path.join(mod, "benchmark_test.go"), "package inf\n", "utf8");
  await fs.mkdir(path.join(root, ".claude", "projects"), { recursive: true });
  await fs.writeFile(path.join(root, ".claude", "projects", "session.jsonl"), "{}\n", "utf8");
  // Last, so the chmod is not undone by a later mkdir under it.
  await fs.chmod(mod, 0o555);
  return mod;
}

/** Assert the fixture is actually hostile AT TEST TIME. A `0555` that a umask,
 *  an ACL or a copy step quietly widened would make every assertion below pass
 *  for the wrong reason. */
async function assertReadOnlyDir(dir: string): Promise<void> {
  const st = await fs.lstat(dir);
  assert.strictEqual(
    (st.mode & 0o777).toString(8),
    "555",
    `fixture directory ${dir} must be mode 0555 when the code under test runs`,
  );
}

async function mktmp(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "uzi-rmtree-"));
}

/** Make a tree removable again so the test's own cleanup cannot leak it. */
async function forceCleanup(root: string): Promise<void> {
  await restoreTreeWritability(root).catch(() => undefined);
  await fs.rm(root, { recursive: true, force: true }).catch(() => undefined);
}

describe("rmTreeForce (PRD #108 M6)", () => {
  it("POSITIVE CONTROL: plain fs.rm(recursive, force) fails EACCES on a 0555 directory and strands it", async (t) => {
    if (asRoot) {
      t.skip("running as uid 0 — root bypasses the 0555 fixture, so this control cannot be exercised");
      return;
    }
    const root = await mktmp();
    try {
      const mod = await makeGoModCacheFixture(root);
      await assertReadOnlyDir(mod);

      const err = await fs.rm(root, { recursive: true, force: true }).then(
        () => undefined,
        (e: NodeJS.ErrnoException) => e,
      );

      assert.ok(err, "unfixed fs.rm must reject on a 0555 directory (force suppresses ENOENT, not EACCES)");
      // The errno is the assertion, not the prose: EACCES on `unlink`, which is
      // verbatim what the incident logged.
      assert.strictEqual(err.code, "EACCES", `expected EACCES, got ${err.code}: ${err.message}`);
      assert.strictEqual(err.syscall, "unlink");
      assert.match(err.message, /benchmark_test\.go/);

      // What survives, MEASURED rather than assumed: the read-only directory and
      // everything under it, plus every ancestor up to the run HOME (a directory
      // cannot be removed while a child remains). Siblings fs.rm reached before
      // giving up ARE deleted — so the leak is not "the whole tree untouched",
      // it is "the module cache plus a hollow skeleton", which is where the
      // incident's 167.3 MB lived.
      assert.ok(await exists(root), "the run HOME survives the failed cleanup");
      assert.ok(await exists(path.join(mod, "benchmark_test.go")), "the read-only directory's contents survive");
      assert.ok(await exists(path.join(root, "go", "pkg", "mod")), "its ancestors survive with it");
    } finally {
      await forceCleanup(root);
    }
  });

  it("removes a tree containing 0555 directories", async () => {
    const root = await mktmp();
    try {
      const mod = await makeGoModCacheFixture(root);
      await assertReadOnlyDir(mod);

      await rmTreeForce(root);

      assert.strictEqual(await exists(root), false, "rmTreeForce must remove the whole tree");
    } finally {
      await forceCleanup(root);
    }
  });

  it("is a no-op for a path that does not exist (ENOENT stays suppressed)", async () => {
    const root = await mktmp();
    await fs.rm(root, { recursive: true, force: true });
    await rmTreeForce(path.join(root, "never-existed"));
  });

  it("never follows a symlink out of the tree while restoring permissions", async () => {
    const root = await mktmp();
    const outside = await mktmp();
    try {
      const keep = path.join(outside, "keep");
      await fs.mkdir(keep, { recursive: true });
      await fs.writeFile(path.join(keep, "precious"), "do not touch\n", "utf8");
      await fs.chmod(keep, 0o555);

      const home = path.join(root, "home");
      await fs.mkdir(home, { recursive: true });
      await fs.symlink(keep, path.join(home, "escape"), "dir");

      await rmTreeForce(root);

      assert.strictEqual(await exists(root), false, "the tree itself is removed");
      assert.ok(await exists(path.join(keep, "precious")), "the symlink target's contents survive");
      if (!asRoot) {
        const st = await fs.lstat(keep);
        assert.strictEqual(
          (st.mode & 0o777).toString(8),
          "555",
          "the symlink target's mode is untouched — the walk must not chmod through a symlink",
        );
      }
    } finally {
      await forceCleanup(root);
      await forceCleanup(outside);
    }
  });
});

async function exists(p: string): Promise<boolean> {
  return fs.lstat(p).then(
    () => true,
    () => false,
  );
}
