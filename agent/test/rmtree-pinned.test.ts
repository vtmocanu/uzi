import { describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { mkdirSync, renameSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { restoreTreeWritability, rmTreePinned, rmRunnerTeardownTree, type CommandWrapper } from "../src/rmtree.js";
import { noProcFd, RACED_FILES, seedRacedTree, startSwapRacer } from "./swap-racer.js";

/**
 * PRD #1809 M3 (audit fix): the whole-tree removal the running disk reclaim uses. The
 * reclaim deletes while other runs' same-uid agent processes are live, so a path-based walk
 * (`fs.rm`, what rmHomeTree does) can be redirected by an intermediate directory swapped for
 * a symlink mid-walk. These tests run the real pinned-descriptor script single-uid (the
 * cross-uid proof needs the real split, `e2e/home-uid-split/`), and skip on a host without
 * `/proc/self/fd`, where the walk refuses by design.
 */

const NO_PROC_FD = "no /proc/self/fd on this host: the pinned walk refuses here by design";
const asRoot = process.getuid?.() === 0;

async function mktmp(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "uzi-rmtree-pinned-"));
}

async function forceCleanup(root: string): Promise<void> {
  await restoreTreeWritability(root).catch(() => undefined);
  await fs.rm(root, { recursive: true, force: true }).catch(() => undefined);
}

async function exists(p: string): Promise<boolean> {
  return fs.lstat(p).then(
    () => true,
    () => false,
  );
}

async function countFiles(dir: string): Promise<number> {
  return (await fs.readdir(dir)).length;
}

/** A terminal run HOME: a `0555` module cache, a private dir, files at several depths. */
async function seedTree(root: string): Promise<void> {
  const mod = path.join(root, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
  await fs.mkdir(mod, { recursive: true });
  await fs.writeFile(path.join(mod, "dec.go"), "package inf\n");
  await fs.mkdir(path.join(root, ".claude", "projects"), { recursive: true, mode: 0o700 });
  await fs.writeFile(path.join(root, ".claude", "projects", "s.jsonl"), "{}\n");
  await fs.writeFile(path.join(root, ".claude.json"), "{}\n");
  await fs.symlink("/nonexistent-target", path.join(root, "dangling"));
  await fs.chmod(mod, 0o555);
  await fs.chmod(path.dirname(mod), 0o555);
}

/** A victim OUTSIDE the tree, shaped so a redirected walk has something to delete. */
async function seedVictim(dir: string, n: number): Promise<void> {
  for (let i = 0; i < n; i++) await fs.writeFile(path.join(dir, `precious-${i}`), "keep\n");
}

describe("rmTreePinned (PRD #1809 M3)", () => {
  it("removes a whole tree with 0555 dirs and a dangling symlink; absent when nothing is there", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    try {
      await fs.mkdir(path.join(parent, "run"));
      await seedTree(path.join(parent, "run"));
      await fs.writeFile(path.join(parent, "sibling"), "keep\n");
      assert.equal(await rmTreePinned(parent, "run"), "removed");
      assert.equal(await exists(path.join(parent, "run")), false);
      assert.ok(await exists(path.join(parent, "sibling")), "a sibling of the tree is kept");
      assert.equal(await rmTreePinned(parent, "run"), "absent");
      assert.equal(await rmTreePinned(path.join(parent, "no-such-parent"), "run"), "absent");
    } finally {
      await forceCleanup(parent);
    }
  });

  it("refuses a symlinked or non-directory root, a bad name and a relative parent, touching nothing", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    const victim = await mktmp();
    try {
      await seedVictim(victim, 3);
      await fs.symlink(victim, path.join(parent, "link"), "dir");
      await fs.writeFile(path.join(parent, "file"), "x");
      await assert.rejects(rmTreePinned(parent, "link"), { code: "ELOOP", message: /refusing/ });
      await assert.rejects(rmTreePinned(parent, "file"), { code: "ELOOP", message: /refusing/ });
      for (const bad of ["..", ".", "a/b", "-rf", ""]) {
        await assert.rejects(rmTreePinned(parent, bad), /not one path component/, bad);
      }
      await assert.rejects(rmTreePinned("relative", "x"), /non-absolute/);
      assert.equal(await countFiles(victim), 3, "the symlink's target is untouched");
      assert.ok(await exists(path.join(parent, "link")) && (await exists(path.join(parent, "file"))));
    } finally {
      await forceCleanup(parent);
      await forceCleanup(victim);
    }
  });

  it("refuses a root the worker does not own", { skip: asRoot ? false : "needs root to chown a fixture" }, async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    try {
      await fs.mkdir(path.join(parent, "foreign"));
      await fs.writeFile(path.join(parent, "foreign", "f"), "x");
      await fs.chown(path.join(parent, "foreign"), 12345, 12345);
      await assert.rejects(rmTreePinned(parent, "foreign"), { code: "EPERM", message: /not owned by this worker/ });
      assert.ok(await exists(path.join(parent, "foreign", "f")));
    } finally {
      await forceCleanup(parent);
    }
  });

  it("refuses a root owned by another uid before any chmod or helper (non-root, via the uid seam)", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    try {
      const root = path.join(parent, "foreign");
      await fs.mkdir(root, { mode: 0o700 });
      await fs.chmod(root, 0o700);
      await fs.writeFile(path.join(root, "f"), "x");
      const self = process.getuid?.() ?? 0;
      let helperRan = false;
      await assert.rejects(
        rmTreePinned(parent, "foreign", {
          getuid: () => self + 1,
          splitActive: true, // the path that chmods the root, had the owner check passed
          wrappers: [
            (c, a) => {
              helperRan = true;
              return { command: c, args: [...a] };
            },
          ],
        }),
        { code: "EPERM", message: /not owned by this worker/ },
      );
      assert.equal(helperRan, false, "no helper pass ran");
      assert.equal((await fs.stat(root)).mode & 0o7777, 0o700, "the root was not chmodded");
      assert.ok(await exists(path.join(root, "f")), "nothing was removed");
    } finally {
      await forceCleanup(parent);
    }
  });

  it("a child dir swapped for a symlink after it was emptied is unlinked (ENOTDIR), never followed", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    const victim = await mktmp();
    const syncDir = await mktmp();
    try {
      const root = path.join(parent, "run");
      await fs.mkdir(path.join(root, "x"), { recursive: true });
      await fs.writeFile(path.join(root, "x", "f"), "");
      await seedVictim(victim, 5);
      const sync = path.join(syncDir, "sync");
      const pending = rmTreePinned(parent, "run", {
        wrappers: [(c, a) => ({ command: c, args: [...a, sync, "emptied"] })],
      }).then(
        (v) => ({ v }),
        (e: Error) => ({ e }),
      );
      const until = Date.now() + 10_000;
      while (!(await exists(`${sync}.emptied`))) {
        if (Date.now() > until) throw new Error("the script never reached its emptied pause");
        await new Promise((r) => setTimeout(r, 10));
      }
      // `x` is empty and about to be rmdir'ed by name: swap it for a symlink to the victim.
      await fs.rename(path.join(root, "x"), path.join(root, "x.moved"));
      await fs.symlink(victim, path.join(root, "x"), "dir");
      await fs.writeFile(`${sync}.go`, "");
      const out = await pending;
      assert.equal(await countFiles(victim), 5, "the symlink's target is untouched");
      assert.deepEqual(out, { v: "removed" }, "the symlink was unlinked and the moved dir removed on the re-read");
      assert.equal(await exists(root), false);
    } finally {
      await forceCleanup(parent);
      await forceCleanup(victim);
      await forceCleanup(syncDir);
    }
  });

  it("refuses when the name is swapped for another directory between the worker's pin and the helper's", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    try {
      const root = path.join(parent, "run");
      await fs.mkdir(root);
      await fs.writeFile(path.join(root, "f"), "x");
      // The wrapper runs right before the helper is spawned: swap the name for a planted dir.
      const swapping: CommandWrapper = (command, args) => {
        renameSync(root, `${root}.moved`);
        mkdirSync(root);
        writeFileSync(path.join(root, "planted"), "keep\n");
        return { command, args: [...args] };
      };
      await assert.rejects(rmTreePinned(parent, "run", { wrappers: [swapping] }), { code: "ENOTEMPTY" });
      assert.ok(await exists(path.join(root, "planted")), "the planted dir the name now names is untouched");
      assert.ok(await exists(path.join(`${root}.moved`, "f")), "nothing was deleted by name");
    } finally {
      await forceCleanup(parent);
    }
  });

  it("stops at its entry budget and reports it, leaving the rest", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    try {
      const root = path.join(parent, "run");
      await fs.mkdir(root);
      for (let i = 0; i < 300; i++) await fs.writeFile(path.join(root, `f${i}`), "");
      await assert.rejects(rmTreePinned(parent, "run", { maxEntries: 50 }), { message: /ran out of its budget \(50 entries/ });
      assert.equal(await countFiles(root), 250);
      await assert.rejects(rmTreePinned(parent, "run", { deadline: Date.now() - 1 }), /deadline passed/);
      assert.equal(await rmTreePinned(parent, "run"), "removed");
    } finally {
      await forceCleanup(parent);
    }
  });

  /**
   * The audit's attack, made deterministic with the script's `listed` seam: the helper
   * pauses once the tree root's first batch of names is read, the test swaps an
   * INTERMEDIATE directory (`run/x/d`, two levels down) for a symlink to a victim, and the
   * walk continues. Every descent re-pins `O_NOFOLLOW` through its parent's descriptor, so
   * the symlink is unlinked, never followed.
   */
  it("TOCTOU: an intermediate dir swapped for a symlink mid-walk never reaches the victim", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    const victim = await mktmp();
    const syncDir = await mktmp();
    try {
      const root = path.join(parent, "run");
      await fs.mkdir(path.join(root, "x", "d"), { recursive: true });
      for (let i = 0; i < 5; i++) await fs.writeFile(path.join(root, "x", "d", `f${i}`), "");
      await seedVictim(victim, 20);
      const sync = path.join(syncDir, "sync");
      const pending = rmTreePinned(parent, "run", {
        wrappers: [(c, a) => ({ command: c, args: [...a, sync, "listed"] })],
      }).then(
        (v) => ({ v }),
        (e: Error) => ({ e }),
      );
      const until = Date.now() + 10_000;
      while (!(await exists(`${sync}.listed`))) {
        if (Date.now() > until) throw new Error("the script never reached its listed pause");
        await new Promise((r) => setTimeout(r, 10));
      }
      await fs.rename(path.join(root, "x", "d"), path.join(root, "x", "d.moved"));
      await fs.symlink(victim, path.join(root, "x", "d"), "dir");
      await fs.writeFile(`${sync}.go`, "");
      const out = await pending;
      assert.equal(await countFiles(victim), 20, "every victim file outside the tree survives");
      assert.deepEqual(out, { v: "removed" }, "the tree, the moved dir included, is gone");
      assert.equal(await exists(root), false);
    } finally {
      await forceCleanup(parent);
      await forceCleanup(victim);
      await forceCleanup(syncDir);
    }
  });

  /**
   * The audit's racer (swap-racer.ts): a separate same-uid process swaps each `run/d<i>/x`
   * for a symlink to a same-named victim once the removal has started emptying it. Against
   * rmHomeTree (`fs.rm`) this deleted about 330 of the 400 victim files per trial; the
   * pinned walk unlinks through the descriptor it pinned, so the swap cannot move it.
   */
  it("TOCTOU: a concurrent same-uid racer swapping intermediate dirs never gets a victim file deleted", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const parent = await mktmp();
    const victim = await mktmp();
    const scratch = await mktmp();
    try {
      await seedRacedTree(path.join(parent, "run"), victim);
      const racer = await startSwapRacer(path.join(parent, "run"), victim, scratch);
      let out: { v: string } | { e: NodeJS.ErrnoException };
      let swaps: number;
      try {
        out = await rmTreePinned(parent, "run").then(
          (v) => ({ v }),
          (e: NodeJS.ErrnoException) => ({ e }),
        );
      } finally {
        swaps = await racer.stop();
      }
      assert.ok(swaps > 0, "the racer swapped at least one directory mid-walk (the race happened)");
      const names = (await fs.readdir(victim)).sort();
      assert.deepEqual(names, Array.from({ length: RACED_FILES }, (_, i) => `f${i}`).sort(), "every outside name survives");
      for (const name of names) assert.equal(await fs.readFile(path.join(victim, name), "utf8"), "keep\n");
      // Each swapped symlink is unlinked and each moved dir re-listed, so the tree goes; the
      // only other acceptable outcome is the typed not-removed verdict (a swap landing after
      // the final re-read), never a crash or a refusal of the root.
      if ("v" in out) {
        assert.equal(out.v, "removed");
        assert.equal(await exists(path.join(parent, "run")), false);
      } else {
        assert.equal(out.e.code, "ENOTEMPTY", `unexpected failure: ${out.e.message}`);
      }
    } finally {
      await forceCleanup(parent);
      await forceCleanup(victim);
      await forceCleanup(scratch);
    }
  });
});


describe("runner-owned teardown entry point (#2324)", () => {
  it("single-uid removes private and read-only directories with supported clone names", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const saved = process.env.UZI_UID_SPLIT;
    delete process.env.UZI_UID_SPLIT;
    const parent = await mktmp();
    try {
      for (const name of ["ordinary", "ci-fix-a+b", "mr-rework-ș"] ) {
        const leaf = path.join(parent, name);
        await fs.mkdir(path.join(leaf, "private"), { recursive: true, mode: 0o700 });
        await fs.writeFile(path.join(leaf, "private", "file"), "remove");
        await fs.chmod(path.join(leaf, "private"), 0o555);
        if (name !== "ordinary") {
          await assert.rejects(rmTreePinned(parent, name), /not one path component/);
          await assert.rejects(rmRunnerTeardownTree(leaf), /not one path component/);
        }
        await rmRunnerTeardownTree(leaf, { allowCloneName: true });
        assert.equal(await exists(leaf), false);
      }
    } finally {
      if (saved === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = saved;
      await forceCleanup(parent);
    }
  });

  it("refuses unknown single-uid identity and malformed clone components", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const saved = process.env.UZI_UID_SPLIT;
    delete process.env.UZI_UID_SPLIT;
    const parent = await mktmp();
    try {
      const leaf = path.join(parent, "ordinary");
      await fs.mkdir(leaf);
      await fs.writeFile(path.join(leaf, "keep"), "keep");
      const unknown = mock.method(process as { getuid: () => number | undefined }, "getuid", () => undefined);
      try { await assert.rejects(rmRunnerTeardownTree(leaf), /not owned/); }
      finally { unknown.mock.restore(); }
      assert.equal(await fs.readFile(path.join(leaf, "keep"), "utf8"), "keep");
      for (const name of [".hidden", "-option", "a\\b", "a\nb"]) {
        await assert.rejects(rmRunnerTeardownTree(path.join(parent, name), { allowCloneName: true }), /not one path component/);
      }
      await assert.rejects(rmRunnerTeardownTree("relative"), /non-absolute/);
    } finally {
      if (saved === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = saved;
      await forceCleanup(parent);
    }
  });
});
