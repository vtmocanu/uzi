import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";
import {
  type GoWorkFsOps,
  GO_WORK_PARENT,
  goWorkLeafName,
  isGoWorkClean,
  leafGeneration,
  prepareManagedGoWork,
  removeGoWorkLeaves,
} from "../src/go-work.js";
import { RUNNER_UID, uidSplitActive } from "../src/runner-uid.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { recordingLogger } from "./helpers.js";

// Issue #3870 M1: the managed Go work dir primitives. These host tests run the real descriptor-relative
// steps single-uid; the split shape (2770, runner group) is asserted inline only where the test env has
// the split active, and proven for real by e2e/home-uid-split.

const noProcFd = !existsSync("/proc/self/fd");
const NO_PROC_FD = "no /proc/self/fd on this host: the descriptor-relative steps refuse here by design";
const split = uidSplitActive();

let home: string;
beforeEach(() => {
  home = makeScratchRoot("uzi-go-work-");
});
afterEach(async () => {
  await fs.rm(home, { recursive: true, force: true });
});

const parentDir = () => path.join(home, GO_WORK_PARENT);
const warnings = (lines: unknown[]) => (lines as Array<{ level: string; msg: string; reason?: string }>).filter((l) => l.level === "warn");
const UNAVAILABLE = "managed Go work dir unavailable; launching without GOTMPDIR";

async function seedLeaves(gens: number[]): Promise<void> {
  for (const g of gens) {
    await fs.mkdir(path.join(parentDir(), `g${g}`, "go-build1"), { recursive: true });
    await fs.writeFile(path.join(parentDir(), `g${g}`, "go-build1", "x"), "x\n");
  }
}

async function prepareParent(): Promise<void> {
  const { logger } = recordingLogger();
  const r = await prepareManagedGoWork(home, 1, logger);
  assert.ok("dir" in r, JSON.stringify(r));
  await fs.rm(path.join(parentDir(), "g1"), { recursive: true });
}

describe("leaf grammar", () => {
  it("accepts g<positive safe integer> only", () => {
    assert.strictEqual(leafGeneration("g1"), 1);
    assert.strictEqual(leafGeneration("g123"), 123);
    assert.strictEqual(leafGeneration("g9007199254740991"), 9007199254740991);
    for (const bad of ["g0", "g01", "gx", "foo", "g3x", "g", "G3", "g-1", "g1.5", "g12345678901234567", "g9007199254740993", " g3", "g3\n"]) {
      assert.strictEqual(leafGeneration(bad), undefined, bad);
    }
    assert.strictEqual(goWorkLeafName(7), "g7");
    assert.throws(() => goWorkLeafName(0));
  });
});

describe("prepareManagedGoWork", () => {
  it("creates the parent and a leaf with the managed shape", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const { logger, lines } = recordingLogger();
    const r = await prepareManagedGoWork(home, 5, logger);
    assert.deepStrictEqual(r, { dir: path.join(home, GO_WORK_PARENT, "g5") });
    for (const dir of [parentDir(), path.join(parentDir(), "g5")]) {
      const st = await fs.lstat(dir);
      assert.ok(st.isDirectory());
      assert.strictEqual(st.uid, process.getuid?.());
      if (split) {
        assert.strictEqual(st.gid, RUNNER_UID);
        assert.strictEqual((st.mode & 0o7777).toString(8), "2770");
      } else {
        assert.strictEqual((st.mode & 0o777).toString(8), "700");
      }
    }
    assert.deepStrictEqual(warnings(lines), []);
  });

  it("is idempotent for the parent: a second generation reuses it", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const { logger } = recordingLogger();
    assert.ok("dir" in (await prepareManagedGoWork(home, 1, logger)));
    assert.ok("dir" in (await prepareManagedGoWork(home, 2, logger)));
    assert.deepStrictEqual((await fs.readdir(parentDir())).sort(), ["g1", "g2"]);
  });

  for (const gen of [undefined, 0, -1, 1.5, Number.NaN, Number.MAX_SAFE_INTEGER + 2]) {
    it(`falls back with no_generation for ${String(gen)}`, async () => {
      const { logger, lines } = recordingLogger();
      assert.deepStrictEqual(await prepareManagedGoWork(home, gen, logger), { skipped: true, reason: "no_generation" });
      assert.strictEqual(warnings(lines).length, 1);
      assert.strictEqual(warnings(lines)[0]!.msg, UNAVAILABLE);
      assert.strictEqual(existsSync(parentDir()), false, "nothing is created");
    });
  }

  it("an existing leaf is refused as leaf_exists and left as it was", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const { logger, lines } = recordingLogger();
    assert.ok("dir" in (await prepareManagedGoWork(home, 3, logger)));
    await fs.writeFile(path.join(parentDir(), "g3", "mark"), "m\n");
    assert.deepStrictEqual(await prepareManagedGoWork(home, 3, logger), { skipped: true, reason: "leaf_exists" });
    assert.ok(existsSync(path.join(parentDir(), "g3", "mark")));
    assert.strictEqual(warnings(lines).length, 1);
    assert.strictEqual(warnings(lines)[0]!.reason, "leaf_exists");
  });

  it("a symlink parent is refused and its target untouched", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const outside = makeScratchRoot("uzi-go-work-out-");
    try {
      await fs.symlink(outside, parentDir(), "dir");
      const { logger, lines } = recordingLogger();
      assert.deepStrictEqual(await prepareManagedGoWork(home, 1, logger), { skipped: true, reason: "parent_refused" });
      assert.deepStrictEqual(await fs.readdir(outside), []);
      assert.strictEqual(warnings(lines).length, 1);
    } finally {
      await fs.rm(outside, { recursive: true, force: true });
    }
  });

  it("an existing parent with the wrong mode is refused, logged, and never repaired", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await fs.mkdir(parentDir(), { mode: 0o755 });
    await fs.chmod(parentDir(), 0o755);
    const { logger, lines } = recordingLogger();
    assert.deepStrictEqual(await prepareManagedGoWork(home, 1, logger), { skipped: true, reason: "parent_refused" });
    assert.strictEqual(((await fs.lstat(parentDir())).mode & 0o7777).toString(8), "755", "never chmodded");
    assert.deepStrictEqual(await fs.readdir(parentDir()), []);
    const w = warnings(lines);
    assert.strictEqual(w.length, 1);
    assert.strictEqual(w[0]!.msg, UNAVAILABLE);
    assert.strictEqual(w[0]!.reason, "parent_refused");
  });

  it("a non-directory parent is refused", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await fs.writeFile(parentDir(), "file\n");
    const { logger } = recordingLogger();
    assert.deepStrictEqual(await prepareManagedGoWork(home, 1, logger), { skipped: true, reason: "parent_refused" });
  });

  it("a missing or relative HOME is home_unavailable", async () => {
    const { logger } = recordingLogger();
    assert.deepStrictEqual(await prepareManagedGoWork(path.join(home, "nope"), 1, logger), { skipped: true, reason: "home_unavailable" });
    assert.deepStrictEqual(await prepareManagedGoWork("relative/home", 1, logger), { skipped: true, reason: "home_unavailable" });
  });

  describe("injected syscall failures each skip with one diagnostic", () => {
    const boom = (code: string) => Object.assign(new Error("boom"), { code });
    const cases: Array<[string, string, () => GoWorkFsOps]> = [];
    cases.push(["home_unavailable", "open of HOME", () => ({
      open: async (p: string, f: number) => {
        if (p === home) throw boom("EACCES");
        return fs.open(p, f);
      },
    })]);
    cases.push(["parent_create_failed", "mkdir of the parent", () => ({
      mkdir: async (p: string) => {
        if (p.endsWith(`/${GO_WORK_PARENT}`)) throw boom("EROFS");
        return undefined;
      },
    })]);
    cases.push(["parent_refused", "open of the parent", () => ({
      open: async (p: string, f: number) => {
        if (p.endsWith(`/${GO_WORK_PARENT}`)) throw boom("ELOOP");
        return fs.open(p, f);
      },
    })]);
    cases.push(["parent_create_failed", "chmod of the created parent", () => ({
      chmod: async () => {
        throw boom("EPERM");
      },
    })]);
    cases.push(["leaf_create_failed", "mkdir of the leaf", () => ({
      mkdir: async (p: string, o: { mode: number }) => {
        if (p.endsWith("/g9")) throw boom("ENOSPC");
        return fs.mkdir(p, o);
      },
    })]);
    cases.push(["leaf_refused", "open of the leaf", () => ({
      open: async (p: string, f: number) => {
        if (p.endsWith("/g9")) throw boom("ELOOP");
        return fs.open(p, f);
      },
    })]);
    for (const [reason, what, make] of cases) {
      it(`${what} -> ${reason}`, async (t) => {
        if (noProcFd) return t.skip(NO_PROC_FD);
        const { logger, lines } = recordingLogger();
        const r = await prepareManagedGoWork(home, 9, logger, make());
        assert.deepStrictEqual(r, { skipped: true, reason });
        const w = warnings(lines);
        assert.strictEqual(w.length, 1, JSON.stringify(lines));
        assert.strictEqual(w[0]!.msg, UNAVAILABLE);
        assert.strictEqual(w[0]!.reason, reason);
      });
    }

    it("a chmod failure on the created leaf is leaf_create_failed", async (t) => {
      if (noProcFd) return t.skip(NO_PROC_FD);
      await prepareParent();
      const { logger } = recordingLogger();
      const r = await prepareManagedGoWork(home, 9, logger, {
        chmod: async () => {
          throw boom("EPERM");
        },
      });
      assert.deepStrictEqual(r, { skipped: true, reason: "leaf_create_failed" });
    });
  });
});

describe("removeGoWorkLeaves", () => {
  const far = () => Date.now() + 60_000;
  const run = async (select: Parameters<typeof removeGoWorkLeaves>[2]["select"], extra: Partial<Parameters<typeof removeGoWorkLeaves>[2]> = {}) => {
    const { logger, lines } = recordingLogger();
    const r = await removeGoWorkLeaves(home, logger, { select, deadline: far(), reason: "test", ...extra });
    return { r, lines };
  };

  it("olderThan N removes only strictly older generations", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    await seedLeaves([3, 4, 5, 7]);
    const { r } = await run({ kind: "olderThan", generation: 5 });
    assert.deepStrictEqual(r, { parent: "ok", removed: ["g3", "g4"], absent: [], failed: [], skipped: [], truncated: false });
    assert.deepStrictEqual((await fs.readdir(parentDir())).sort(), ["g5", "g7"]);
    assert.ok(isGoWorkClean(r));
  });

  it("all removes every grammar-valid leaf but never the parent", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    await seedLeaves([3, 12]);
    const { r, lines } = await run({ kind: "all" });
    assert.deepStrictEqual(r.removed, ["g3", "g12"]);
    assert.ok(isGoWorkClean(r));
    assert.deepStrictEqual(await fs.readdir(parentDir()), []);
    assert.strictEqual(lines.length, 1, "one bounded log line");
  });

  it("never touches names outside the grammar", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    const keep = ["g0", "g01", "gx", "foo", "g3x"];
    for (const n of keep) {
      await fs.mkdir(path.join(parentDir(), n));
      await fs.writeFile(path.join(parentDir(), n, "f"), "f\n");
    }
    await seedLeaves([2]);
    const { r } = await run({ kind: "all" });
    assert.deepStrictEqual(r.removed, ["g2"]);
    assert.deepStrictEqual((await fs.readdir(parentDir())).sort(), [...keep].sort());
    assert.ok(isGoWorkClean(r));
  });

  it("an absent HOME or parent is absent and clean", async () => {
    const a = await run({ kind: "all" });
    assert.strictEqual(a.r.parent, "absent");
    assert.ok(isGoWorkClean(a.r));
    home = path.join(home, "no-such-home");
    const b = await run({ kind: "all" });
    assert.strictEqual(b.r.parent, "absent");
    assert.ok(isGoWorkClean(b.r));
  });

  it("a deadline already passed skips every selected leaf and is not clean", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    await seedLeaves([1, 2]);
    const { r } = await run({ kind: "all" }, { deadline: Date.now() - 1 });
    assert.deepStrictEqual(r, { parent: "ok", removed: [], absent: [], failed: [], skipped: ["g1", "g2"], truncated: false });
    assert.strictEqual(isGoWorkClean(r), false);
    assert.ok(existsSync(path.join(parentDir(), "g1")), "nothing removed");
  });

  it("a symlink parent is refused, not clean, and its target is untouched", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    const outside = makeScratchRoot("uzi-go-work-out-");
    try {
      await fs.mkdir(path.join(outside, "g1"));
      await fs.symlink(outside, parentDir(), "dir");
      const { r } = await run({ kind: "all" });
      assert.strictEqual(r.parent, "refused");
      assert.strictEqual(isGoWorkClean(r), false);
      assert.ok(existsSync(path.join(outside, "g1")));
    } finally {
      await fs.rm(outside, { recursive: true, force: true });
    }
  });

  it("a parent with the wrong mode is refused", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await fs.mkdir(path.join(parentDir(), "g1"), { recursive: true });
    await fs.chmod(parentDir(), 0o755);
    const { r } = await run({ kind: "all" });
    assert.strictEqual(r.parent, "refused");
    assert.ok(existsSync(path.join(parentDir(), "g1")));
  });

  it("an unreadable parent (readdir denied) is refused, not clean", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    if (process.getuid?.() === 0) return t.skip("root reads any directory");
    await prepareParent();
    await seedLeaves([1]);
    await fs.chmod(parentDir(), 0o300);
    try {
      const { r } = await run({ kind: "all" });
      assert.strictEqual(r.parent, "refused");
      assert.strictEqual(isGoWorkClean(r), false);
    } finally {
      await fs.chmod(parentDir(), 0o700);
    }
  });

  it("stops enumerating at maxEntries and is not clean", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    await seedLeaves([1, 2, 3, 4]);
    const { r } = await run({ kind: "all" }, { maxEntries: 2 });
    assert.strictEqual(r.truncated, true);
    assert.strictEqual(r.removed.length, 2, "only what was enumerated is removed");
    assert.strictEqual(isGoWorkClean(r), false);
    assert.strictEqual((await fs.readdir(parentDir())).length, 2);
    const exact = await run({ kind: "all" }, { maxEntries: 2 });
    assert.strictEqual(exact.r.truncated, false, "exactly maxEntries entries is a complete enumeration");
    assert.ok(isGoWorkClean(exact.r));
  });

  it("a removal that throws is failed and not clean; siblings still get their turn", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    await seedLeaves([1, 2]);
    const { r } = await run(
      { kind: "all" },
      {
        removeSubtree: async (_h: string, rel: string) => {
          if (rel.endsWith("/g1")) throw new Error("stuck");
          return "removed";
        },
      },
    );
    assert.deepStrictEqual(r.failed, ["g1"]);
    assert.deepStrictEqual(r.removed, ["g2"]);
    assert.strictEqual(isGoWorkClean(r), false);
  });

  it("a leaf that is a symlink is failed (refused by the pinned walk), never followed", async (t) => {
    if (noProcFd) return t.skip(NO_PROC_FD);
    await prepareParent();
    const outside = makeScratchRoot("uzi-go-work-out-");
    try {
      await fs.writeFile(path.join(outside, "precious"), "keep\n");
      await fs.symlink(outside, path.join(parentDir(), "g4"), "dir");
      const { r } = await run({ kind: "all" });
      assert.deepStrictEqual(r.failed, ["g4"]);
      assert.ok(existsSync(path.join(outside, "precious")));
    } finally {
      await fs.rm(outside, { recursive: true, force: true });
    }
  });
});

describe("isGoWorkClean", () => {
  const base = { removed: [], absent: [], failed: [], skipped: [], truncated: false };
  it("is true only for an absent parent or a complete, fully removed ok parent", () => {
    assert.strictEqual(isGoWorkClean({ ...base, parent: "absent" }), true);
    assert.strictEqual(isGoWorkClean({ ...base, parent: "ok" }), true);
    assert.strictEqual(isGoWorkClean({ ...base, parent: "refused" }), false);
    assert.strictEqual(isGoWorkClean({ ...base, parent: "ok", truncated: true }), false);
    assert.strictEqual(isGoWorkClean({ ...base, parent: "ok", failed: ["g1"] }), false);
    assert.strictEqual(isGoWorkClean({ ...base, parent: "ok", skipped: ["g1"] }), false);
  });
});
