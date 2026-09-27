import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import {
  PURGE_CHILDREN_SCRIPT,
  SUBTREE_CHAIN_SCRIPT,
  type HomeRemovalDeps,
  type SubtreeRemovalDeps,
  measureHomeSubtrees,
  rmHomeSubtree,
  rmHomeTree,
  rmTreeForce,
  restoreTreeWritability,
} from "../src/rmtree.js";

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

/**
 * Issue #1607. The cross-uid proof (worker cleanup of a runner-written HOME) needs the
 * real uid split and lives in `e2e/home-uid-split/` (`task test:home-uid-split`). These
 * host tests pin the decision logic around it and the purge script's own behaviour,
 * which is uid-independent.
 */
describe("rmHomeTree (#1607)", () => {
  const eacces = () => Object.assign(new Error("EACCES: permission denied, scandir"), { code: "EACCES" });

  /** Fake deps recording calls; removeTree fails EACCES `failures` times, then really removes. */
  function fakeDeps(splitActive: boolean, failures: number, firstError: () => Error = eacces) {
    const calls: string[] = [];
    let left = failures;
    const deps: HomeRemovalDeps = {
      splitActive,
      removeTree: async (t) => {
        calls.push("removeTree");
        if (left > 0) {
          left -= 1;
          throw firstError();
        }
        await rmTreeForce(t);
      },
      purgeChildrenAsAgents: async () => {
        calls.push("purge");
      },
    };
    return { deps, calls };
  }

  it("single-uid: a removal that succeeds never reaches the runner helper", async () => {
    const root = await mktmp();
    const { deps, calls } = fakeDeps(false, 0);
    await rmHomeTree(root, deps);
    assert.deepStrictEqual(calls, ["removeTree"]);
    assert.strictEqual(await exists(root), false);
  });

  it("single-uid: a permission failure propagates unchanged (no helper to fall back to)", async () => {
    const root = await mktmp();
    try {
      const { deps, calls } = fakeDeps(false, 1);
      await assert.rejects(rmHomeTree(root, deps), { code: "EACCES" });
      assert.deepStrictEqual(calls, ["removeTree"]);
    } finally {
      await forceCleanup(root);
    }
  });

  it("split: a permission failure purges as runner, widens the root for group runner, then retries as the worker", async () => {
    const root = await mktmp();
    await fs.chmod(root, 0o700);
    let modeAtPurge = -1;
    const { deps, calls } = fakeDeps(true, 1);
    deps.purgeChildrenAsAgents = async (t) => {
      calls.push("purge");
      modeAtPurge = (await fs.lstat(t)).mode & 0o777;
    };
    await rmHomeTree(root, deps);
    assert.deepStrictEqual(calls, ["removeTree", "purge", "removeTree"]);
    assert.strictEqual(modeAtPurge & 0o070, 0o070, "group runner must be able to traverse the root when the helper runs");
    assert.strictEqual(await exists(root), false);
  });

  it("split: the helper's own failure is not the verdict; the worker's final pass is", async () => {
    const root = await mktmp();
    const { deps, calls } = fakeDeps(true, 1);
    deps.purgeChildrenAsAgents = async () => {
      calls.push("purge");
      throw new Error("helper exited 1");
    };
    await rmHomeTree(root, deps);
    assert.deepStrictEqual(calls, ["removeTree", "purge", "removeTree"]);
    assert.strictEqual(await exists(root), false);
  });

  it("split: a non-permission error propagates without reaching the helper", async () => {
    const root = await mktmp();
    try {
      const { deps, calls } = fakeDeps(true, 1, () => Object.assign(new Error("ENOSPC"), { code: "ENOSPC" }));
      await assert.rejects(rmHomeTree(root, deps), { code: "ENOSPC" });
      assert.deepStrictEqual(calls, ["removeTree"]);
    } finally {
      await forceCleanup(root);
    }
  });

  it("split: refuses a symlinked root and never runs the helper through it", async () => {
    const outside = await mktmp();
    const parent = await mktmp();
    const link = path.join(parent, "home");
    try {
      await fs.symlink(outside, link, "dir");
      const modeBefore = (await fs.lstat(outside)).mode;
      const { deps, calls } = fakeDeps(true, 1);
      await assert.rejects(rmHomeTree(link, deps));
      assert.deepStrictEqual(calls, ["removeTree"]);
      assert.strictEqual((await fs.lstat(outside)).mode, modeBefore, "the symlink target's mode is untouched");
    } finally {
      await forceCleanup(parent);
      await forceCleanup(outside);
    }
  });

  it("refuses a non-absolute path before touching anything", async () => {
    const { deps, calls } = fakeDeps(true, 0);
    await assert.rejects(rmHomeTree("relative/home", deps), /non-absolute/);
    assert.deepStrictEqual(calls, []);
  });
});

describe("PURGE_CHILDREN_SCRIPT (#1607)", () => {
  const run = promisify(execFile);

  it("removes every child including 0555 dirs, keeps the root, and never follows a symlink out", async (t) => {
    if (asRoot) {
      t.skip("running as uid 0 — root bypasses the 0555 fixture, so the widening walk would not be forced");
      return;
    }
    const home = await mktmp();
    const outside = await mktmp();
    try {
      const keep = path.join(outside, "keep");
      await fs.mkdir(keep);
      await fs.writeFile(path.join(keep, "precious"), "do not touch\n", "utf8");
      await fs.chmod(keep, 0o555);
      const mod = await makeGoModCacheFixture(home);
      await fs.symlink(keep, path.join(home, "escape"), "dir");
      await fs.chmod(path.join(home, ".claude", "projects"), 0o700);
      await assertReadOnlyDir(mod);

      await run(process.execPath, ["-e", PURGE_CHILDREN_SCRIPT, home]);

      assert.deepStrictEqual(await fs.readdir(home), [], "every child is removed");
      assert.ok(await exists(home), "the root itself is left for the worker's final pass");
      assert.ok(await exists(path.join(keep, "precious")), "the symlink target's contents survive");
      assert.strictEqual(((await fs.lstat(keep)).mode & 0o777).toString(8), "555", "the symlink target's mode is untouched");
    } finally {
      await forceCleanup(home);
      await forceCleanup(outside);
    }
  });
});

/**
 * PRD #1809 D3. The subtree purge a process-ending park uses to drop a run's caches. As
 * with rmHomeTree, the real cross-uid proof needs the uid split (`e2e/home-uid-split/`);
 * these host tests pin the decision logic by simulating the split through the injected
 * deps, while the purge and chain scripts run for real (unwrapped, as this uid).
 */
describe("rmHomeSubtree (PRD #1809)", () => {
  const run = promisify(execFile);
  const eacces = () => Object.assign(new Error("EACCES: permission denied, unlink"), { code: "EACCES" });

  /** Deps whose removeTree fails EACCES `failures` times before really removing (the
   *  worker blocked by agent-owned dirs), and whose agent-uid helpers run the real scripts. */
  function subtreeDeps(splitActive: boolean, failures: number) {
    const calls: string[] = [];
    let left = failures;
    const deps: SubtreeRemovalDeps = {
      splitActive,
      removeTree: async (t) => {
        calls.push("removeTree");
        if (left > 0) {
          left -= 1;
          throw eacces();
        }
        await rmTreeForce(t);
      },
      purgeChildrenAsAgents: async (t) => {
        calls.push("purge");
        await run(process.execPath, ["-e", PURGE_CHILDREN_SCRIPT, t]).catch(() => undefined);
      },
      chainAsAgents: async (home, rel, mode) => {
        calls.push(`chain:${mode}`);
        const code = await run(process.execPath, ["-e", SUBTREE_CHAIN_SCRIPT, home, rel, mode]).then(
          () => 0,
          (e: { code?: unknown }) => e.code,
        );
        return code === 0 ? "ok" : code === 2 ? "absent" : code === 3 ? "refused" : "failed";
      },
    };
    return { deps, calls };
  }

  /** A HOME with a `0555` module cache, the build cache, and siblings that must survive. */
  async function seedHome(home: string): Promise<string> {
    const mod = await makeGoModCacheFixture(home); // go/pkg/mod/... (0555) + .claude/projects
    await fs.mkdir(path.join(home, "go", "bin"), { recursive: true });
    await fs.writeFile(path.join(home, "go", "bin", "gopls"), "bin\n");
    await fs.mkdir(path.join(home, ".cache", "go-build", "0a"), { recursive: true });
    await fs.writeFile(path.join(home, ".cache", "go-build", "0a", "obj"), "o\n");
    await fs.mkdir(path.join(home, ".cache", "other-tool"), { recursive: true });
    await fs.writeFile(path.join(home, ".cache", "other-tool", "state"), "s\n");
    await fs.writeFile(path.join(home, ".claude.json"), "{}\n");
    await fs.chmod(mod, 0o555);
    return mod;
  }

  async function assertSiblingsKept(home: string): Promise<void> {
    for (const rel of ["go/bin/gopls", ".cache/other-tool/state", ".claude/projects/session.jsonl", ".claude.json"]) {
      assert.ok(await exists(path.join(home, rel)), `${rel} must survive`);
    }
  }

  it("single-uid: drops the 0555 module cache and nothing else, leaving the HOME root's mode alone", async () => {
    const home = await mktmp();
    try {
      await fs.chmod(home, 0o750);
      const mod = await seedHome(home);
      if (!asRoot) await assertReadOnlyDir(mod);
      const { deps, calls } = subtreeDeps(false, 0);
      await rmHomeSubtree(home, "go/pkg/mod", deps);
      await rmHomeSubtree(home, ".cache/go-build", deps);
      assert.deepStrictEqual(calls, ["removeTree", "removeTree"]);
      assert.strictEqual(await exists(path.join(home, "go", "pkg", "mod")), false);
      assert.strictEqual(await exists(path.join(home, ".cache", "go-build")), false);
      assert.ok(await exists(path.join(home, "go", "pkg")), "the parent of a dropped cache stays");
      await assertSiblingsKept(home);
      assert.strictEqual(((await fs.lstat(home)).mode & 0o777).toString(8), "750", "the HOME root's mode is untouched");
    } finally {
      await forceCleanup(home);
    }
  });

  it("split: a blocked worker purges as the agents, then rmdirs the leaf as the agent when its parent is agent-owned", async () => {
    const home = await mktmp();
    try {
      await fs.chmod(home, 0o750);
      const mod = await seedHome(home);
      if (!asRoot) await assertReadOnlyDir(mod);
      // Two failures: the first plain removal, and the leaf removal after the purge
      // (the worker cannot unlink from the agent-owned `go/pkg`).
      const { deps, calls } = subtreeDeps(true, 2);
      await rmHomeSubtree(home, "go/pkg/mod", deps);
      assert.deepStrictEqual(calls, ["removeTree", "purge", "removeTree", "chain:rmdir"]);
      assert.strictEqual(await exists(path.join(home, "go", "pkg", "mod")), false);
      await assertSiblingsKept(home);
      assert.strictEqual(((await fs.lstat(home)).mode & 0o777).toString(8), "750", "the HOME root is never widened");
    } finally {
      await forceCleanup(home);
    }
  });

  it("split: throws when the subtree is still there at the end", async () => {
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, ".npm", "_cacache"), { recursive: true });
      const { deps } = subtreeDeps(true, 99);
      deps.chainAsAgents = async (_h, _r, mode) => (mode === "check" ? "ok" : "failed");
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", deps), /still present/);
    } finally {
      await forceCleanup(home);
    }
  });

  it("split: a component the worker cannot open is walked by the agents, whose refusal is final", async () => {
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, ".npm", "_cacache"), { recursive: true });
      const { deps, calls } = subtreeDeps(true, 0);
      // A `0000` `.npm` stands in for a runner-private dir the worker cannot open; the
      // agent walk is answered directly so its refusal is what the helper acts on.
      deps.chainAsAgents = async () => {
        calls.push("chain:check");
        return "refused";
      };
      await fs.chmod(path.join(home, ".npm"), 0o000);
      if (asRoot) return; // root opens a 0000 dir, so the EACCES branch is not reached
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", deps), /agent-uid walk: refused/);
      assert.deepStrictEqual(calls, ["chain:check"], "nothing is removed after a refusal");
    } finally {
      await fs.chmod(path.join(home, ".npm"), 0o700).catch(() => undefined);
      await forceCleanup(home);
    }
  });

  it("refuses a symlinked `.cache` and never touches the target outside", async () => {
    const home = await mktmp();
    const outside = await mktmp();
    try {
      await fs.mkdir(path.join(outside, "go-build"));
      await fs.writeFile(path.join(outside, "go-build", "precious"), "keep\n");
      await fs.symlink(outside, path.join(home, ".cache"), "dir");
      for (const split of [false, true]) {
        const { deps, calls } = subtreeDeps(split, 0);
        await assert.rejects(rmHomeSubtree(home, ".cache/go-build", deps), /refusing/);
        assert.deepStrictEqual(calls, [], "nothing ran through the symlink");
      }
      assert.ok(await exists(path.join(outside, "go-build", "precious")), "the symlink target survives");
    } finally {
      await forceCleanup(home);
      await forceCleanup(outside);
    }
  });

  it("refuses a symlinked leaf", async () => {
    const home = await mktmp();
    const outside = await mktmp();
    try {
      await fs.writeFile(path.join(outside, "precious"), "keep\n");
      await fs.mkdir(path.join(home, "go", "pkg"), { recursive: true });
      await fs.symlink(outside, path.join(home, "go", "pkg", "mod"), "dir");
      const { deps, calls } = subtreeDeps(true, 0);
      await assert.rejects(rmHomeSubtree(home, "go/pkg/mod", deps), /refusing/);
      assert.deepStrictEqual(calls, []);
      assert.ok(await exists(path.join(outside, "precious")));
    } finally {
      await forceCleanup(home);
      await forceCleanup(outside);
    }
  });

  it("refuses anything not on the cache list, and a non-absolute HOME, before touching anything", async () => {
    const home = await mktmp();
    try {
      const { deps, calls } = subtreeDeps(true, 0);
      for (const rel of ["go", ".claude", "go/bin", "../x", "/etc", ".cache/go-build/../../.claude", ""]) {
        await assert.rejects(rmHomeSubtree(home, rel, deps), /not a listed cache subtree/, rel);
      }
      await assert.rejects(rmHomeSubtree("relative/home", "go/pkg/mod", deps), /non-absolute/);
      assert.deepStrictEqual(calls, []);
    } finally {
      await forceCleanup(home);
    }
  });

  it("a missing subtree (or a missing parent) is a successful no-op", async () => {
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, "go"));
      const { deps, calls } = subtreeDeps(true, 0);
      await rmHomeSubtree(home, "go/pkg/mod", deps);
      await rmHomeSubtree(home, ".npm/_cacache", deps);
      await rmHomeSubtree(path.join(home, "no-such-home"), ".cache/go-build", deps);
      assert.deepStrictEqual(calls, []);
      assert.ok(await exists(path.join(home, "go")));
    } finally {
      await forceCleanup(home);
    }
  });
});

describe("measureHomeSubtrees (PRD #1809)", () => {
  it("counts the HOME and its cache bytes, without following a symlink out", async () => {
    const home = await mktmp();
    const outside = await mktmp();
    try {
      await fs.mkdir(path.join(home, ".cache", "go-build"), { recursive: true });
      await fs.writeFile(path.join(home, ".cache", "go-build", "obj"), Buffer.alloc(64 * 1024, 1));
      await fs.writeFile(path.join(home, "notes"), Buffer.alloc(16 * 1024, 1));
      await fs.writeFile(path.join(outside, "big"), Buffer.alloc(1024 * 1024, 1));
      await fs.symlink(outside, path.join(home, "escape"), "dir");
      const r = await measureHomeSubtrees(home);
      assert.ok(r.cacheBytes >= 64 * 1024, `cache bytes counted: ${r.cacheBytes}`);
      assert.ok(r.homeBytes >= r.cacheBytes + 16 * 1024, `home bytes include non-cache files: ${r.homeBytes}`);
      assert.ok(r.homeBytes < 1024 * 1024, `the symlink target is not counted: ${r.homeBytes}`);
    } finally {
      await forceCleanup(home);
      await forceCleanup(outside);
    }
  });

  it("keeps the larger reading per field across uid passes, and rejects only when none ran", async () => {
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, "go", "pkg", "mod"), { recursive: true });
      await fs.writeFile(path.join(home, "go", "pkg", "mod", "m"), Buffer.alloc(8192, 1));
      const identity = (command: string, args: readonly string[]) => ({ command, args: [...args] });
      const broken = () => ({ command: "/nonexistent/uzi-no-such-binary", args: [] });
      const single = await measureHomeSubtrees(home, [identity]);
      assert.deepStrictEqual(await measureHomeSubtrees(home, [broken, identity, identity]), single);
      await assert.rejects(measureHomeSubtrees(home, [broken]));
      await assert.rejects(measureHomeSubtrees("relative", [identity]), /non-absolute/);
    } finally {
      await forceCleanup(home);
    }
  });
});
