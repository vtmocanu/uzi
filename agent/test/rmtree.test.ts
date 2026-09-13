import { describe, it, mock } from "node:test";
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

  // issue #1308 m1. `restoreTreeWritability`'s chmod walk targets EACCES/EPERM — a
  // permission problem — and does nothing for ENOTEMPTY/EBUSY, a lingering-writer
  // problem (a `git maintenance` child, `web/node_modules/.vite`). Node's own
  // `fs.rm(recursive, { maxRetries, retryDelay })` retries THOSE transient codes
  // internally, inside a single call — invisible to rmTreeForce's own try/catch, which
  // never sees more than one rejection either way. So the fix IS the `RM_RETRY` options
  // object reaching every `fs.rm` call: drop it and a transient ENOTEMPTY/EBUSY that a
  // real retry would have ridden out instead propagates straight out, unfixed.
  //
  // A REAL race (an actual concurrent writer) would make this flaky and timing-bound —
  // exactly what this repo's rules forbid asserting on. So this pins the same contract
  // deterministically: fs.rm is mocked to fail the underlying removal once with a
  // synthetic ENOTEMPTY and only succeed on a second attempt IF it was handed a retry
  // budget (opts.maxRetries) — mirroring, at the call-argument level, exactly what
  // Node's real implementation conditions its internal retry on. No real timer is
  // exercised (the retry is immediate), so this is a call-count / completion assertion,
  // never a wall-clock one.
  it("resolves through a transient ENOTEMPTY when fs.rm carries a retry budget (deterministic, no timing assertion)", async () => {
    const root = await mktmp();
    try {
      const nested = path.join(root, "nested");
      await fs.mkdir(nested, { recursive: true });
      await fs.writeFile(path.join(nested, "f.txt"), "x", "utf8");
      const realRm = fs.rm.bind(fs);
      let attempts = 0;
      const m = mock.method(
        fs,
        "rm",
        async (p: Parameters<typeof fs.rm>[0], opts?: { maxRetries?: number }) => {
          attempts++;
          if (attempts === 1) {
            if (!opts?.maxRetries) {
              // No retry budget was passed — exactly the unfixed #1308 m1 shape: one
              // attempt, no retry, the transient error propagates.
              const err: NodeJS.ErrnoException = new Error("ENOTEMPTY: directory not empty");
              err.code = "ENOTEMPTY";
              throw err;
            }
            // A retry budget IS present: mirror Node's own internal retry — the
            // caller's single `await fs.rm(...)` still resolves, having ridden out one
            // transient failure underneath. Counted as a second attempt.
            attempts++;
            return realRm(p, opts);
          }
          return realRm(p, opts);
        },
      );
      try {
        await rmTreeForce(root);

        assert.strictEqual(await exists(root), false, "the tree is eventually removed");
        assert.ok(attempts >= 2, `the retry budget must be exercised at least twice, got ${attempts}`);
      } finally {
        m.mock.restore();
      }
    } finally {
      await forceCleanup(root);
    }
  });

  it("never follows a symlink out of the tree while restoring permissions", async (t) => {
    // This test was VACUOUS until 2026-07-21 and the fixture is the whole point.
    //
    // The original built a tree with NO read-only directory inside `root`, so
    // rmTreeForce's fast-path `fs.rm` SUCCEEDED and `restoreTreeWritability` — the
    // only thing that chmods, and therefore the only thing that could follow a
    // symlink — never ran at all. Every assertion below passed without the code
    // under test executing. Proven by mutation, not by reading: deliberately
    // dropping O_NOFOLLOW and descending into symlink entries left all four tests
    // in this file GREEN.
    //
    // So the read-only directory below is LOAD-BEARING. It forces the fast path to
    // fail with EACCES, which is the only way the permission-restoring walk runs
    // and the only way this test can observe the guard it claims to test.
    if (asRoot) {
      t.skip("running as uid 0 — root bypasses the 0555 fixture, so the walk would not be forced");
      return;
    }
    const root = await mktmp();
    const outside = await mktmp();
    try {
      const keep = path.join(outside, "keep");
      await fs.mkdir(keep, { recursive: true });
      await fs.writeFile(path.join(keep, "precious"), "do not touch\n", "utf8");

      const home = path.join(root, "home");
      // The read-only directory that forces the walk. Without this the fast path
      // succeeds and nothing below is exercised.
      const readOnly = path.join(home, "go", "pkg", "mod", "example.com", "dep@v1.0.0");
      await fs.mkdir(readOnly, { recursive: true });
      await fs.writeFile(path.join(readOnly, "dep_test.go"), "package dep\n", "utf8");
      await fs.symlink(keep, path.join(home, "escape"), "dir");
      // chmod last, so a later mkdir cannot undo it.
      await fs.chmod(keep, 0o555);
      await fs.chmod(readOnly, 0o555);
      await assertReadOnlyDir(readOnly);
      await assertReadOnlyDir(keep);

      // Drive the walk DIRECTLY rather than through rmTreeForce. The guard lives in
      // restoreTreeWritability, so calling it is what makes this test about the
      // guard; going through rmTreeForce would leave the assertions hostage to
      // whether the fast path happened to fail. (A destructive premise-check does
      // not work here either: `fs.rm` removes what it reaches before it fails, and
      // the symlink is one of those things — it would delete the very entry this
      // test exists to observe.)
      await restoreTreeWritability(root);

      // POSITIVE CONTROL, non-destructive: the walk really ran, proven by its own
      // effect INSIDE the tree. Without this the mode assertion below is free.
      const walked = (await fs.lstat(readOnly)).mode & 0o700;
      assert.strictEqual(walked, 0o700, "restoreTreeWritability must have widened the read-only dir it walked");

      // The guard: the symlink's TARGET, outside the tree, is untouched.
      assert.strictEqual(
        ((await fs.lstat(keep)).mode & 0o777).toString(8),
        "555",
        "the symlink target's mode is untouched — the walk must not chmod through a symlink",
      );

      await rmTreeForce(root);

      assert.strictEqual(await exists(root), false, "the tree itself is removed");
      assert.ok(await exists(path.join(keep, "precious")), "the symlink target's contents survive");
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
