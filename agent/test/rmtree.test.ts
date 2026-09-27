import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { existsSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import {
  PURGE_CHILDREN_SCRIPT,
  type HomeRemovalDeps,
  measureRunCaches,
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
 * with rmHomeTree, the real cross-uid proof needs the uid split (`e2e/home-uid-split/`).
 * These host tests run the real pinned-descriptor script: through the default wrappers
 * (single-uid here, so the real `runnerCommand` is the identity and the real exit-code
 * mapping decides), and through stand-in wrappers that script the per-uid passes a split
 * worker makes. The script pins through `/proc/self/fd`, so a host without it (macOS)
 * skips rather than passes vacuously.
 */
const noProcFd = !existsSync("/proc/self/fd");
const NO_PROC_FD = "no /proc/self/fd on this host: the pinned walk refuses here by design";

describe("rmHomeSubtree (PRD #1809)", () => {
  /** A HOME with a `0555` module cache, the build cache, and siblings that must survive. */
  async function seedHome(home: string): Promise<string> {
    const mod = await makeGoModCacheFixture(home); // go/pkg/mod/... (0555) + .claude/projects
    await fs.mkdir(path.join(home, "go", "bin"), { recursive: true });
    await fs.writeFile(path.join(home, "go", "bin", "gopls"), "bin\n");
    await fs.mkdir(path.join(home, ".cache", "go-build", "0a"), { recursive: true });
    await fs.writeFile(path.join(home, ".cache", "go-build", "0a", "obj"), "o\n");
    await fs.symlink("/nonexistent-target", path.join(home, ".cache", "go-build", "dangling"));
    await fs.mkdir(path.join(home, ".cache", "other-tool"), { recursive: true });
    await fs.writeFile(path.join(home, ".cache", "other-tool", "state"), "s\n");
    await fs.writeFile(path.join(home, ".claude.json"), "{}\n");
    await fs.chmod(path.dirname(mod), 0o555);
    await fs.chmod(mod, 0o555);
    return mod;
  }

  const SIBLINGS = ["go/bin/gopls", ".cache/other-tool/state", ".claude/projects/session.jsonl", ".claude.json"];

  async function assertSiblingsKept(home: string, rename: Record<string, string> = {}): Promise<void> {
    for (const rel of SIBLINGS) {
      const [top, ...rest] = rel.split("/");
      const p = path.join(home, rename[top!] ?? top!, ...rest);
      assert.ok(await exists(p), `${rel} must survive`);
    }
  }

  /** A stand-in wrapper that records each pass and runs the command as this uid. */
  function recording(calls: string[], name: string, extra: string[] = []) {
    return (command: string, args: readonly string[]) => {
      calls.push(name);
      return { command, args: [...args, ...extra] };
    };
  }

  /** A stand-in wrapper whose pass exits `code` without running the script. */
  function exitsWith(calls: string[], code: number) {
    return () => {
      calls.push(`exit${code}`);
      return { command: process.execPath, args: ["-e", `process.exit(${code})`] };
    };
  }

  it("default wrappers: drops the 0555 module cache and the build cache, nothing else, and leaves the HOME root's mode alone", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      await fs.chmod(home, 0o750);
      const mod = await seedHome(home);
      if (!asRoot) await assertReadOnlyDir(mod);
      assert.strictEqual(await rmHomeSubtree(home, "go/pkg/mod"), "removed");
      assert.strictEqual(await rmHomeSubtree(home, ".cache/go-build"), "removed");
      assert.strictEqual(await exists(path.join(home, "go", "pkg", "mod")), false);
      assert.strictEqual(await exists(path.join(home, ".cache", "go-build")), false);
      assert.ok(await exists(path.join(home, "go", "pkg")), "the parent of a dropped cache stays");
      await assertSiblingsKept(home);
      assert.strictEqual(((await fs.lstat(home)).mode & 0o777).toString(8), "750", "the HOME root's mode is untouched");
    } finally {
      await forceCleanup(home);
    }
  });

  it("default wrappers: a missing subtree, parent or HOME resolves absent", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, "go"));
      assert.strictEqual(await rmHomeSubtree(home, "go/pkg/mod"), "absent");
      assert.strictEqual(await rmHomeSubtree(home, ".npm/_cacache"), "absent");
      assert.strictEqual(await rmHomeSubtree(path.join(home, "no-such-home"), ".cache/go-build"), "absent");
      assert.ok(await exists(path.join(home, "go")));
    } finally {
      await forceCleanup(home);
    }
  });

  for (const [what, rel, link] of [
    ["a symlinked `.cache`", ".cache/go-build", ".cache"],
    ["a symlinked `go/pkg`", "go/pkg/mod", "go/pkg"],
    ["a symlinked leaf", "go/pkg/mod", "go/pkg/mod"],
  ] as const) {
    it(`default wrappers: refuses ${what} as final, and never touches the target outside`, async (t) => {
      if (noProcFd) return t.skip(NO_PROC_FD);
      const home = await mktmp();
      const outside = await mktmp();
      try {
        const leafInOutside = path.join(outside, ...rel.split("/").slice(link.split("/").length));
        await fs.mkdir(leafInOutside, { recursive: true });
        await fs.writeFile(path.join(leafInOutside, "precious"), "keep\n");
        await fs.mkdir(path.dirname(path.join(home, link)), { recursive: true });
        await fs.symlink(outside, path.join(home, link), "dir");
        await assert.rejects(rmHomeSubtree(home, rel), { code: "ELOOP", message: /refusing/ });
        // A refusal is final: no later uid pass runs after it.
        const calls: string[] = [];
        await assert.rejects(rmHomeSubtree(home, rel, { wrappers: [recording(calls, "a"), recording(calls, "b")] }), /refusing/);
        assert.deepStrictEqual(calls, ["a"]);
        assert.ok(await exists(path.join(leafInOutside, "precious")), "the symlink target survives");
      } finally {
        await forceCleanup(home);
        await forceCleanup(outside);
      }
    });
  }

  it("split passes: stops at the first pass that settles it, and a partial or blocked pass hands on", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      await seedHome(home);
      const calls: string[] = [];
      // runner leaves runner-cmd's entries (exit 4), runner-cmd cannot open a component
      // (exit 7), a helper fails outright (exit 1, e.g. setpriv), the second runner pass
      // finishes: the stand-ins script those verdicts.
      const wrappers = [
        exitsWith(calls, 4),
        exitsWith(calls, 7),
        exitsWith(calls, 1),
        recording(calls, "real"),
        recording(calls, "unused"),
      ];
      assert.strictEqual(await rmHomeSubtree(home, "go/pkg/mod", { wrappers }), "removed");
      assert.deepStrictEqual(calls, ["exit4", "exit7", "exit1", "real"]);
      assert.strictEqual(await exists(path.join(home, "go", "pkg", "mod")), false);
      await assertSiblingsKept(home);
    } finally {
      await forceCleanup(home);
    }
  });

  it("split passes: throws when every pass leaves the subtree, and refuses outright without /proc/self/fd", async () => {
    const home = await mktmp();
    try {
      const calls: string[] = [];
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", { wrappers: [exitsWith(calls, 7), exitsWith(calls, 4)] }), {
        code: "ENOTEMPTY",
        message: /still present.*entries remained/,
      });
      assert.deepStrictEqual(calls, ["exit7", "exit4"]);
      calls.length = 0;
      // A helper that ran but failed (setpriv, an uncaught throw) is named as such, not as a
      // tree that kept entries.
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", { wrappers: [exitsWith(calls, 1)] }), {
        code: "ENOTEMPTY",
        message: /helper itself failed \(exit 1\)/,
      });
      calls.length = 0;
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", { wrappers: [exitsWith(calls, 5), exitsWith(calls, 0)] }), /refusing/);
      assert.deepStrictEqual(calls, ["exit5"], "no fallback pass after an unsupported host");
      calls.length = 0;
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", { wrappers: [exitsWith(calls, 8), exitsWith(calls, 0)] }), {
        code: "ENOTEMPTY",
        message: /ran out of its budget/,
      });
      assert.deepStrictEqual(calls, ["exit8"], "an exhausted budget ends the removal: another uid meets the same tree");
      calls.length = 0;
      await assert.rejects(
        rmHomeSubtree(home, ".npm/_cacache", { wrappers: [exitsWith(calls, 0)], deadline: Date.now() - 1 }),
        { code: "ENOTEMPTY", message: /deadline passed/ },
      );
      assert.deepStrictEqual(calls, [], "no pass starts after the caller's deadline");
      await assert.rejects(
        rmHomeSubtree(home, ".npm/_cacache", { wrappers: [() => ({ command: "/nonexistent/uzi-no-such-binary", args: [] })] }),
        { code: "ENOENT" },
        "a helper that cannot run is surfaced, not read as a verdict",
      );
    } finally {
      await forceCleanup(home);
    }
  });

  it("refuses anything not on the cache list, and a non-absolute HOME, before any pass runs", async () => {
    const home = await mktmp();
    try {
      const calls: string[] = [];
      const passes = [recording(calls, "pass")];
      for (const rel of ["go", ".claude", "go/bin", "../x", "/etc", ".cache/go-build/../../.claude", ""]) {
        await assert.rejects(rmHomeSubtree(home, rel, { wrappers: passes }), /not a listed cache subtree/, rel);
      }
      await assert.rejects(rmHomeSubtree("relative/home", "go/pkg/mod", { wrappers: passes }), /non-absolute/);
      assert.deepStrictEqual(calls, []);
    } finally {
      await forceCleanup(home);
    }
  });

  /**
   * B1 (TOCTOU). A live `runner`/`runner-cmd` process, e.g. another run's, can rename a
   * component of the chain and plant a symlink AFTER the walk verified it. The script's
   * test seam pauses once the leaf is pinned; the test swaps a component for a symlink to
   * a victim dir shaped like the subtree, then lets the removal continue. On 1eb02dbe
   * (path-based removal after a one-shot check) the same swap deleted the victim.
   */
  async function removeWithSwap(
    home: string,
    rel: string,
    swap: () => Promise<void>,
    phase: "pinned" | "listed" = "pinned",
  ) {
    const sync = path.join(await mktmp(), "sync");
    const pending = rmHomeSubtree(home, rel, { wrappers: [(c, a) => ({ command: c, args: [...a, sync, phase] })] }).then(
      (v) => ({ v }),
      (e: Error) => ({ e }),
    );
    const deadline = Date.now() + 10_000;
    while (!(await exists(`${sync}.${phase}`))) {
      if (Date.now() > deadline) throw new Error(`the script never reached its ${phase} pause`);
      await new Promise((r) => setTimeout(r, 10));
    }
    await swap();
    await fs.writeFile(`${sync}.go`, "");
    const out = await pending;
    await forceCleanup(path.dirname(sync));
    return out;
  }

  for (const [component, rel, victimShape] of [
    [".cache", ".cache/go-build", "go-build"],
    ["go", "go/pkg/mod", "pkg/mod"],
  ] as const) {
    it(`TOCTOU: \`${component}\` swapped for a symlink after the leaf is pinned removes only the pinned tree`, async (t) => {
      if (noProcFd) return t.skip(NO_PROC_FD);
      const home = await mktmp();
      const victim = await mktmp();
      try {
        await seedHome(home);
        await fs.mkdir(path.join(victim, victimShape), { recursive: true });
        await fs.writeFile(path.join(victim, victimShape, "precious"), "keep\n");
        const moved = `${component}.moved`;
        const out = await removeWithSwap(home, rel, async () => {
          await fs.rename(path.join(home, component), path.join(home, moved));
          await fs.symlink(victim, path.join(home, component), "dir");
        });
        assert.ok(await exists(path.join(victim, victimShape, "precious")), "the victim outside the HOME survives");
        assert.deepStrictEqual(out, { v: "removed" }, "the pinned tree's name is gone from its pinned parent");
        const pinnedLeaf = path.join(home, moved, ...rel.split("/").slice(1));
        assert.strictEqual(await exists(pinnedLeaf), false, "the pinned (now renamed) subtree was removed");
        await assertSiblingsKept(home, { [component]: moved });
      } finally {
        await forceCleanup(home);
        await forceCleanup(victim);
      }
    });
  }

  it("TOCTOU: a leaf swapped for a symlink is emptied through its pinned descriptor, then refused", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    const victim = await mktmp();
    try {
      await seedHome(home);
      await fs.writeFile(path.join(victim, "precious"), "keep\n");
      const out = await removeWithSwap(home, "go/pkg/mod", async () => {
        await fs.rename(path.join(home, "go", "pkg", "mod"), path.join(home, "go", "pkg", "mod.moved"));
        await fs.symlink(victim, path.join(home, "go", "pkg", "mod"), "dir");
      });
      assert.ok(await exists(path.join(victim, "precious")), "the victim outside the HOME survives");
      assert.ok("e" in out && /refusing/.test(out.e.message), `the planted symlink is refused: ${JSON.stringify(out)}`);
      assert.deepStrictEqual(await fs.readdir(path.join(home, "go", "pkg", "mod.moved")), [], "the pinned leaf was emptied");
      await assertSiblingsKept(home);
    } finally {
      await forceCleanup(home);
      await forceCleanup(victim);
    }
  });

  /**
   * A CHILD directory inside the leaf is swapped for a symlink to a victim after the leaf
   * was listed but before the child is pinned. The child pin's O_NOFOLLOW is what refuses
   * it (the symlink itself is then unlinked); a pin that followed it would chmod and empty
   * the victim.
   */
  it("TOCTOU: a child dir of the leaf swapped for a symlink after listing is unlinked, never followed", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    const victim = await mktmp();
    try {
      await seedHome(home);
      await fs.writeFile(path.join(victim, "precious"), "keep\n");
      await fs.chmod(victim, 0o500);
      const child = path.join(home, "go", "pkg", "mod", "gopkg.in");
      const out = await removeWithSwap(
        home,
        "go/pkg/mod",
        async () => {
          await fs.rename(child, `${child}.moved`);
          await fs.symlink(victim, child, "dir");
        },
        "listed",
      );
      assert.ok(await exists(path.join(victim, "precious")), "the victim outside the HOME survives");
      assert.strictEqual(((await fs.lstat(victim)).mode & 0o777).toString(8), "500", "the victim's mode is untouched");
      assert.deepStrictEqual(out, { v: "removed" }, "the leaf, the moved child included, is gone");
      assert.strictEqual(await exists(path.join(home, "go", "pkg", "mod")), false);
      await assertSiblingsKept(home);
    } finally {
      await forceCleanup(home);
      await fs.chmod(victim, 0o700).catch(() => undefined);
      await forceCleanup(victim);
    }
  });

  it("a flat directory with more names than the pass's entry budget stops the removal and reports it", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      const leaf = path.join(home, ".npm", "_cacache");
      await fs.mkdir(leaf, { recursive: true });
      for (let i = 0; i < 300; i++) await fs.writeFile(path.join(leaf, `f${i}`), "");
      await assert.rejects(rmHomeSubtree(home, ".npm/_cacache", { maxEntries: 50 }), {
        code: "ENOTEMPTY",
        message: /ran out of its budget \(50 entries/,
      });
      const left = (await fs.readdir(leaf)).length;
      assert.strictEqual(left, 250, `exactly the budget's worth of names was removed: ${left} left`);
      assert.strictEqual(await rmHomeSubtree(home, ".npm/_cacache"), "removed", "an unbounded pass finishes it");
    } finally {
      await forceCleanup(home);
    }
  });
});

describe("measureRunCaches (PRD #1809)", () => {
  const identity = (command: string, args: readonly string[]) => ({ command, args: [...args] });

  it("counts only the cache subtrees, without following a symlink out", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    const outside = await mktmp();
    try {
      await fs.mkdir(path.join(home, ".cache", "go-build"), { recursive: true });
      await fs.writeFile(path.join(home, ".cache", "go-build", "obj"), Buffer.alloc(64 * 1024, 1));
      await fs.writeFile(path.join(home, "notes"), Buffer.alloc(1024 * 1024, 1));
      await fs.mkdir(path.join(outside, "_cacache"));
      await fs.writeFile(path.join(outside, "big"), Buffer.alloc(1024 * 1024, 1));
      await fs.writeFile(path.join(outside, "_cacache", "big"), Buffer.alloc(1024 * 1024, 1));
      await fs.symlink(outside, path.join(home, ".cache", "go-build", "escape"), "dir");
      await fs.symlink(outside, path.join(home, ".npm"), "dir");
      const r = await measureRunCaches(home);
      assert.ok(r.cacheBytes >= 64 * 1024, `cache bytes counted: ${r.cacheBytes}`);
      assert.ok(r.cacheBytes < 1024 * 1024, `neither the non-cache file nor a symlink target is counted: ${r.cacheBytes}`);
      assert.strictEqual(r.truncated, false);
    } finally {
      await forceCleanup(home);
      await forceCleanup(outside);
    }
  });

  it("stops at its entry ceiling and flags the reading truncated", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, "go", "pkg", "mod"), { recursive: true });
      for (let i = 0; i < 5; i++) await fs.writeFile(path.join(home, "go", "pkg", "mod", `m${i}`), Buffer.alloc(8192, 1));
      const full = await measureRunCaches(home, { wrappers: [identity] });
      const capped = await measureRunCaches(home, { wrappers: [identity], maxEntries: 2 });
      assert.strictEqual(full.truncated, false);
      assert.strictEqual(full.entries, 5);
      assert.strictEqual(capped.truncated, true);
      assert.strictEqual(capped.entries, 2);
      assert.ok(capped.cacheBytes < full.cacheBytes, `a truncated reading is a lower bound: ${capped.cacheBytes} < ${full.cacheBytes}`);
    } finally {
      await forceCleanup(home);
    }
  });

  it("streams a flat directory with far more names than its ceiling: counts exactly the ceiling, flags truncated", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      const leaf = path.join(home, "go", "pkg", "mod");
      await fs.mkdir(leaf, { recursive: true });
      for (let i = 0; i < 2000; i++) await fs.writeFile(path.join(leaf, `m${i}`), "");
      const r = await measureRunCaches(home, { wrappers: [identity], maxEntries: 10 });
      assert.strictEqual(r.truncated, true);
      assert.strictEqual(r.entries, 10, "every dirent read is counted, and the walk stops at the ceiling");
      await assert.rejects(measureRunCaches(home, { wrappers: [identity], deadline: Date.now() - 1 }), /deadline passed/);
    } finally {
      await forceCleanup(home);
    }
  });

  it("keeps the larger reading across uid passes, and rejects only when none ran", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const home = await mktmp();
    try {
      await fs.mkdir(path.join(home, "go", "pkg", "mod"), { recursive: true });
      await fs.writeFile(path.join(home, "go", "pkg", "mod", "m"), Buffer.alloc(8192, 1));
      const broken = () => ({ command: "/nonexistent/uzi-no-such-binary", args: [] });
      const single = await measureRunCaches(home, { wrappers: [identity] });
      assert.deepStrictEqual(await measureRunCaches(home, { wrappers: [broken, identity, identity] }), single);
      await assert.rejects(measureRunCaches(home, { wrappers: [broken] }));
      await assert.rejects(measureRunCaches("relative", { wrappers: [identity] }), /non-absolute/);
    } finally {
      await forceCleanup(home);
    }
  });
});
