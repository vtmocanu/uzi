// issue #3239 — GitCache.commitWipMarker never commits a core dump or an oversized file, and a
// post-commit backstop rolls back (CAS on the exact pre-capture branch) or refuses when one slips in anyway.
import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, WIP_PARK_COMMIT_PREFIX, type GitCacheOptions, type WipIncompleteInfo } from "../src/git.js";
import { nullLogger } from "./helpers.js";

const CAP = 8 * 1024 * 1024;
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const gitEnv = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

async function fixture(t: test.TestContext, wipCapture?: GitCacheOptions["wipCapture"]) {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "wip-omit-"));
  const root = path.join(dir, "clone");
  await fs.mkdir(root);
  const g = (...args: string[]) => execFileSync("/usr/bin/git", ["-C", root, ...args],
    { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  g("init", "-q", "-b", "main");
  for (const [k, v] of [["user.name", "fixture"], ["user.email", "fixture@example.com"], ["commit.gpgsign", "false"],
    ["gc.auto", "0"], ["core.fsmonitor", "false"]]) g("config", k!, v!);
  await fs.writeFile(path.join(root, "tracked.txt"), "original\n");
    g("add", "-A"); g("commit", "-q", "-m", "base");
  const git = new GitCache(dir, nullLogger(), undefined, wipCapture ? { wipCapture } : {});
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const write = (rel: string, size: number | string) =>
    fs.writeFile(path.join(root, rel), typeof size === "number" ? Buffer.alloc(size, 0x61) : size);
  const calls: WipIncompleteInfo[] = [];
  const onIncomplete = (info: WipIncompleteInfo) => { calls.push(info); };
  const exists = (rel: string) => fs.stat(path.join(root, rel)).then((s) => s.size, () => -1);
  return { dir, root, g, git, write, calls, onIncomplete, exists, head: () => g("rev-parse", "HEAD") };
}

const reasons = (c: WipIncompleteInfo) => Object.fromEntries(c.omitted.map((o) => [o.path, o.reason]));

describe("commitWipMarker omissions (issue #3239)", () => {
  test("(a) refuse mode makes no marker; exclude mode commits the rest", async (t) => {
    const f = await fixture(t);
    await f.write("big.bin", CAP + 1);
    await f.write("core.1039053", "dump");
    await f.write("small-wip.txt", "ordinary\n");
    const pre = f.head();

    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.head(), pre);
    assert.equal(f.g("diff", "--cached", "--name-only"), "", "nothing staged");
    assert.equal(f.calls.length, 1);
    assert.deepEqual(reasons(f.calls[0]!), { "big.bin": "oversized", "core.1039053": "core_file" });
    assert.deepEqual(f.calls[0]!.omitted.find((o) => o.path === "big.bin")?.bytes, CAP + 1);
    assert.equal(f.calls[0]!.markerCreated, false);
    assert.equal(await f.exists("big.bin"), CAP + 1);
    assert.equal(await f.exists("core.1039053"), 4);

    f.calls.length = 0;
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), true);
    assert.ok(f.g("log", "-1", "--format=%s").startsWith(WIP_PARK_COMMIT_PREFIX));
    assert.equal(f.g("rev-parse", "HEAD^"), pre);
    const tree = f.g("ls-tree", "-r", "--name-only", "HEAD").split("\n");
    assert.ok(tree.includes("small-wip.txt"));
    assert.ok(!tree.includes("big.bin") && !tree.includes("core.1039053"));
    assert.equal(await f.exists("big.bin"), CAP + 1);
    assert.equal(await f.exists("core.1039053"), 4);
    assert.equal(f.calls.length, 1);
    assert.deepEqual(reasons(f.calls[0]!), { "big.bin": "oversized", "core.1039053": "core_file" });
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [true, false]);
  });

  test("(b) an oversized STAGED new file is unstaged and left out", async (t) => {
    const f = await fixture(t);
    await f.write("staged-big.bin", CAP + 1);
    f.g("add", "staged-big.bin");
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), true);
    const tree = f.g("ls-tree", "-r", "--name-only", "HEAD").split("\n");
    assert.ok(tree.includes("small-wip.txt") && !tree.includes("staged-big.bin"));
    assert.equal(await f.exists("staged-big.bin"), CAP + 1);
    assert.deepEqual(reasons(f.calls[0]!), { "staged-big.bin": "oversized" });
  });

  test("(b2) a staged blob that was shrunk in the worktree after git add is still caught", async (t) => {
    const f = await fixture(t);
    await f.write("staged-big.bin", CAP + 1);
    f.g("add", "staged-big.bin");
    await f.write("staged-big.bin", "now small\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.deepEqual(reasons(f.calls[0]!), { "staged-big.bin": "oversized" });
  });

  test("(c) an oversized TRACKED modification keeps the old blob in the marker", async (t) => {
    const f = await fixture(t);
    await f.write("tracked.txt", CAP + 1);
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), true);
    assert.equal(f.g("show", "HEAD:tracked.txt"), "original");
    assert.equal(await f.exists("tracked.txt"), CAP + 1);
    assert.deepEqual(reasons(f.calls[0]!), { "tracked.txt": "oversized" });
  });

  test("(c2) exclude mode with only omitted files makes no marker", async (t) => {
    const f = await fixture(t);
    const pre = f.head();
    await f.write("core", "dump");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), false);
    assert.equal(f.head(), pre);
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [false, false]);
  });

  test("(d) control: exactly 8 MiB and a file named score.1 commit normally", async (t) => {
    const f = await fixture(t);
    await f.write("exact.bin", CAP);
    await f.write("score.1", "ok\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), true);
    const tree = f.g("ls-tree", "-r", "--name-only", "HEAD").split("\n");
    assert.ok(tree.includes("exact.bin") && tree.includes("score.1"));
    assert.equal(f.calls.length, 0);
  });

  test("(d2) clean tree and legacy no-options call still behave as before", async (t) => {
    const f = await fixture(t);
    assert.equal(await f.git.commitWipMarker(f.root), false);
    await f.write("small-wip.txt", "x\n");
    assert.equal(await f.git.commitWipMarker(f.root), true);
  });

  // The race: the file is small at the omission check and over the cap by the time `add -A` runs.
  const grow = (root: string) => fs.writeFile(path.join(root, "racy.txt"), Buffer.alloc(CAP + 1, 0x62));

  test("(e) an intervening commit on top of the marker refuses the rollback", async (t) => {
    let extra = "";
    const f = await fixture(t, {
      afterOmissionCheck: (p) => grow(p),
      beforeBackstop: async (p) => {
        const g = (...a: string[]) => execFileSync("/usr/bin/git", ["-C", p, ...a], { env: gitEnv, encoding: "utf8" }).trim();
        g("commit", "-q", "--allow-empty", "-m", "extra");
        extra = g("rev-parse", "HEAD");
      },
    });
    await f.write("racy.txt", "tiny\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0]!.rollbackRefused, true);
    assert.equal(f.calls[0]!.markerCreated, false);
    assert.equal(f.head(), extra);
    assert.ok(f.g("log", "--format=%s").split("\n").some((s) => s.startsWith(WIP_PARK_COMMIT_PREFIX)));
    assert.equal(await f.exists("racy.txt"), CAP + 1);
  });

  test("(e2) sibling: the same race without an extra commit rolls the branch back to preHead", async (t) => {
    const f = await fixture(t, { afterOmissionCheck: (p) => grow(p) });
    const pre = f.head();
    await f.write("racy.txt", "tiny\n");
    await f.write("small-wip.txt", "ordinary\n");
    const indexBefore = () => f.g("ls-files", "-s");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.g("rev-parse", "refs/heads/main"), pre);
    assert.equal(f.head(), pre);
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [false, false]);
    assert.equal(reasons(f.calls[0]!)["racy.txt"], "oversized");
    assert.equal(await f.exists("racy.txt"), CAP + 1);
    assert.equal(await f.exists("small-wip.txt"), 9);
    assert.match(indexBefore(), /small-wip\.txt/, "index left as the failed capture staged it");
    assert.match(f.g("reflog", "-1", "refs/heads/main"), /wip capture rollback/);
  });

  test("(f) same-commit branch switch refuses; both branches stay at the marker", async (t) => {
    const f = await fixture(t, {
      afterOmissionCheck: (p) => grow(p),
      beforeBackstop: async (p, marker) => {
        const g = (...a: string[]) => execFileSync("/usr/bin/git", ["-C", p, ...a], { env: gitEnv, encoding: "utf8" }).trim();
        g("branch", "B", marker);
        g("symbolic-ref", "HEAD", "refs/heads/B");
      },
    });
    await f.write("racy.txt", "tiny\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls[0]!.rollbackRefused, true);
    const a = f.g("rev-parse", "refs/heads/main");
    assert.equal(f.g("rev-parse", "refs/heads/B"), a);
    assert.notEqual(a, f.g("rev-parse", "main^"));
    assert.equal(f.g("symbolic-ref", "HEAD"), "refs/heads/B");
  });

  test("(h) attached: HEAD switched to B after the final recheck moves only branch A back", async (t) => {
    let pre = "";
    const f = await fixture(t, {
      afterOmissionCheck: (p) => grow(p),
      beforeRollbackWrite: async (p, marker) => {
        const g = (...a: string[]) => execFileSync("/usr/bin/git", ["-C", p, ...a], { env: gitEnv, encoding: "utf8" }).trim();
        g("branch", "B", marker);
        g("symbolic-ref", "HEAD", "refs/heads/B");
      },
    });
    pre = f.head();
    await f.write("racy.txt", "tiny\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.g("rev-parse", "refs/heads/main"), pre, "only branch A moved back");
    assert.notEqual(f.g("rev-parse", "refs/heads/B"), pre, "B untouched at the marker");
    assert.equal(f.g("rev-parse", "refs/heads/B^"), pre);
    assert.equal(f.g("symbolic-ref", "HEAD"), "refs/heads/B", "HEAD stays on B");
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [false, false]);
  });

  test("(g1) detached HEAD: the backstop refuses without writing; HEAD and all refs unchanged", async (t) => {
    let refsAtBackstop = "";
    let wrote = false;
    const snapshot = (p: string) => execFileSync("/usr/bin/git", ["-C", p, "for-each-ref", "--format=%(refname) %(objectname)"],
      { env: gitEnv, encoding: "utf8" }) + "HEAD " + execFileSync("/usr/bin/git", ["-C", p, "rev-parse", "HEAD"], { env: gitEnv, encoding: "utf8" });
    const f = await fixture(t, {
      afterOmissionCheck: (p) => grow(p),
      beforeBackstop: async (p) => { refsAtBackstop = snapshot(p); },
      beforeRollbackWrite: async () => { wrote = true; },
    });
    const pre = f.head();
    f.g("checkout", "-q", "--detach");
    await f.write("racy.txt", "tiny\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [false, true]);
    assert.equal(f.calls[0]!.marker, undefined, "a detached capture never reports a marker to undo");
    assert.equal(wrote, false, "no rollback write was attempted");
    assert.equal(snapshot(f.root), refsAtBackstop, "HEAD and every ref unchanged by the backstop");
    assert.equal(f.g("rev-parse", "HEAD^"), pre);
    assert.equal(f.g("rev-parse", "refs/heads/main"), pre, "no branch ref moved");
    assert.throws(() => f.g("symbolic-ref", "-q", "HEAD"), "HEAD is still detached");
    assert.equal(await f.exists("racy.txt"), CAP + 1);
  });

  test("(g2) attached, then HEAD detached at the marker: refuses, no ref moved", async (t) => {
    const f = await fixture(t, {
      afterOmissionCheck: (p) => grow(p),
      beforeBackstop: async (p) => {
        execFileSync("/usr/bin/git", ["-C", p, "checkout", "-q", "--detach"], { env: gitEnv });
      },
    });
    await f.write("racy.txt", "tiny\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls[0]!.rollbackRefused, true);
    assert.equal(f.g("rev-parse", "refs/heads/main"), f.head(), "main still at the marker");
    assert.equal(f.g("rev-parse", "main^"), f.g("rev-parse", "HEAD^"));
  });

  test("(o) a commit call that rejects after git wrote the ref reports an unvetted marker", async (t) => {
    const f = await fixture(t, { commitRejectsAfterWrite: true });
    const pre = f.head();
    await f.write("wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls.length, 1);
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [false, true]);
    assert.ok(f.g("log", "-1", "--format=%s").startsWith(WIP_PARK_COMMIT_PREFIX), "git did write the marker");
    assert.equal(f.g("rev-parse", "HEAD^"), pre);
    assert.equal(f.calls[0]!.marker, f.head(), "the written marker is recovered as this capture's own commit");
  });

  test("(g3) detached, then HEAD attached to a branch at the marker: refuses, no ref moved", async (t) => {
    const f = await fixture(t, {
      afterOmissionCheck: (p) => grow(p),
      beforeBackstop: async (p, marker) => {
        const g = (...a: string[]) => execFileSync("/usr/bin/git", ["-C", p, ...a], { env: gitEnv, encoding: "utf8" }).trim();
        g("branch", "B", marker);
        g("symbolic-ref", "HEAD", "refs/heads/B");
      },
    });
    const pre = f.head();
    f.g("checkout", "-q", "--detach");
    await f.write("racy.txt", "tiny\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls[0]!.rollbackRefused, true);
    assert.equal(f.g("rev-parse", "refs/heads/main"), pre);
    assert.equal(f.g("rev-parse", "refs/heads/B^"), pre);
    assert.equal(f.g("symbolic-ref", "HEAD"), "refs/heads/B");
  });

  const gIn = (p: string) => (...a: string[]) => execFileSync("/usr/bin/git", ["-C", p, ...a], { env: gitEnv, encoding: "utf8" }).trim();
  const slipCore = (p: string) => fs.writeFile(path.join(p, "core"), "tiny\n");

  test("(i) a small core file that appears after the scan is caught by the backstop and rolled back", async (t) => {
    const f = await fixture(t, { afterOmissionCheck: (p) => slipCore(p) });
    const pre = f.head();
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), false);
    assert.equal(f.g("rev-parse", "refs/heads/main"), pre);
    assert.equal(f.calls.length, 1);
    assert.equal(reasons(f.calls[0]!)["core"], "core_file");
    assert.deepEqual([f.calls[0]!.markerCreated, f.calls[0]!.rollbackRefused], [false, false]);
    assert.equal(await f.exists("core"), 5);
  });

  test("(j) HEAD swapped to a pre-planted clean wip branch: the commit written to the branch is vetted, not HEAD", async (t) => {
    const f = await fixture(t, {
      afterOmissionCheck: (p) => slipCore(p),
      afterCommit: async (p) => {
        const g = gIn(p);
        const planted = g("commit-tree", "HEAD~1^{tree}", "-p", "HEAD~1", "-m", `${WIP_PARK_COMMIT_PREFIX} planted`);
        g("branch", "B", planted);
        g("symbolic-ref", "HEAD", "refs/heads/B");
      },
    });
    const pre = f.head();
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls.length, 1);
    assert.equal(f.calls[0]!.rollbackRefused, true);
    assert.equal(reasons(f.calls[0]!)["core"], "core_file", "the core blob in branch A's commit is reported");
    assert.equal(f.g("rev-parse", "refs/heads/main^"), pre);
    assert.equal(f.g("symbolic-ref", "HEAD"), "refs/heads/B");
  });

  test("(n) a replace ref planted over an oversized blob cannot hide it from the backstop", async (t) => {
    let big = "";
    const f = await fixture(t, {
      afterOmissionCheck: async (p) => {
        // Slips past the scan: written after it, so the scan never saw it.
        await fs.writeFile(path.join(p, "racy.txt"), Buffer.alloc(CAP + 1, 0x62));
      },
      afterCommit: async (p) => {
        const g = gIn(p);
        big = g("rev-parse", "HEAD:racy.txt");
        const tiny = execFileSync("/usr/bin/git", ["-C", p, "hash-object", "-w", "--stdin"],
          { env: gitEnv, encoding: "utf8", input: "tiny\n" }).trim();
        g("replace", "-f", big, tiny);
      },
    });
    const pre = f.head();
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls.length, 1);
    assert.equal(reasons(f.calls[0]!)["racy.txt"], "oversized");
    assert.equal(f.calls[0]!.markerCreated, false);
    assert.equal(f.g("rev-parse", "refs/heads/main"), pre, "rolled back despite the replace ref");
  });

  test("(k) a preRef turned into a symref after the recheck never moves its target", async (t) => {
    let marker = "";
    const f = await fixture(t, {
      afterOmissionCheck: (p) => slipCore(p),
      beforeRollbackWrite: async (p, m) => {
        marker = m;
        const g = gIn(p);
        g("update-ref", "refs/remotes/origin/main", m);
        g("symbolic-ref", "refs/heads/main", "refs/remotes/origin/main");
      },
    });
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onIncomplete: f.onIncomplete }), false);
    assert.equal(f.g("rev-parse", "refs/remotes/origin/main"), marker, "the symref target did not move");
  });

  test("(l) deleting a committed core file does not block a refuse-mode capture", async (t) => {
    const f = await fixture(t);
    await f.write("core", "old dump\n");
    f.g("add", "core"); f.g("commit", "-q", "-m", "oops core");
    await fs.rm(path.join(f.root, "core"));
    await f.write("small-wip.txt", "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), true);
    assert.equal(f.calls.length, 0);
    const tree = f.g("ls-tree", "-r", "--name-only", "HEAD").split("\n");
    assert.ok(tree.includes("small-wip.txt") && !tree.includes("core"));
  });

  test("(m) the omission report is bounded to 100 paths plus the total", async (t) => {
    const f = await fixture(t);
    for (let i = 0; i < 105; i++) await f.write(`core.${i}`, "d");
    assert.equal(await f.git.commitWipMarker(f.root, { onIncomplete: f.onIncomplete }), false);
    assert.equal(f.calls[0]!.omitted.length, 100);
    assert.equal(f.calls[0]!.omittedTotal, 105);
  });
});

describe("undoWipMarkerOnBranch (issue #3239)", () => {
  async function withMarker(t: test.TestContext) {
    const f = await fixture(t);
    const parent = f.head();
    await f.write("wip.txt", "dirty\n");
    let marker = "";
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude", onCommitted: (m) => { marker = m; } }), true);
    assert.equal(marker, f.head(), "onCommitted reported the commit the capture wrote");
    return { f, parent, marker };
  }

  test("(a) CAS succeeds: branch at parent, index kept, worktree file remains staged", async (t) => {
    const { f, parent, marker } = await withMarker(t);
    assert.equal(await f.git.undoWipMarkerOnBranch(f.root, "main", marker), true);
    assert.equal(f.g("rev-parse", "refs/heads/main"), parent);
    assert.equal(f.g("diff", "--cached", "--name-only"), "wip.txt", "the capture remains staged");
    assert.equal(await f.exists("wip.txt"), 6);
    assert.match(f.g("status", "--porcelain"), /^A  wip\.txt$/m);
  });

  test("(b) branch moved past the marker: refused, branch untouched", async (t) => {
    const { f, marker } = await withMarker(t);
    f.g("commit", "-q", "--allow-empty", "-m", "later");
    const tip = f.head();
    assert.equal(await f.git.undoWipMarkerOnBranch(f.root, "main", marker), false);
    assert.equal(f.g("rev-parse", "refs/heads/main"), tip);
  });

  test("(c) HEAD switched to B at the marker: only the named branch is rewound, HEAD stays on B", async (t) => {
    const { f, parent, marker } = await withMarker(t);
    f.g("branch", "B", marker);
    f.g("symbolic-ref", "HEAD", "refs/heads/B");
    assert.equal(await f.git.undoWipMarkerOnBranch(f.root, "main", marker), true);
    assert.equal(f.g("rev-parse", "refs/heads/main"), parent);
    assert.equal(f.g("rev-parse", "refs/heads/B"), marker, "B untouched");
    assert.equal(f.g("symbolic-ref", "HEAD"), "refs/heads/B");
    assert.equal(f.g("diff", "--cached", "--name-only"), "", "index not reset (still B's tree)");
  });

  test("(d) a commit that is not a wip(park) marker is refused", async (t) => {
    const f = await fixture(t);
    f.g("commit", "-q", "--allow-empty", "-m", "plain");
    const tip = f.head();
    assert.equal(await f.git.undoWipMarkerOnBranch(f.root, "main", tip), false);
    assert.equal(f.head(), tip);
  });

  test("(e) staged-only content survives the HEAD-switch index-restore window", async (t) => {
    const { f, parent, marker } = await withMarker(t);
    f.g("branch", "B", marker);
    await f.write("index-only.txt", "only staged version\n");
    f.g("add", "index-only.txt");
    await f.write("index-only.txt", "different working version\n");
    const staged = f.g("ls-files", "--stage", "--", "index-only.txt");
    const index = await fs.readFile(path.join(f.root, ".git", "index"));
    const runGit = f.git["runGitAsRunner"].bind(f.git);
    let indexWrites = 0;
    f.git["runGitAsRunner"] = async (cwd, args, opts) => {
      // Reproduce the old race after its HEAD check and immediately before its
      // read-tree. A branch-only undo never opens this index-write window.
      if (args[0] === "read-tree") {
        indexWrites++;
        f.g("symbolic-ref", "HEAD", "refs/heads/B");
      }
      return runGit(cwd, args, opts);
    };

    assert.equal(await f.git.undoWipMarkerOnBranch(f.root, "main", marker), true);
    assert.equal(f.g("rev-parse", "refs/heads/main"), parent);
    assert.equal(f.g("rev-parse", "refs/heads/B"), marker, "B is not rewound");
    assert.equal(f.g("ls-files", "--stage", "--", "index-only.txt"), staged, "staged-only entry survives");
    assert.equal(f.g("show", ":index-only.txt"), "only staged version", "staged-only bytes survive");
    assert.deepEqual(await fs.readFile(path.join(f.root, ".git", "index")), index, "index is byte-identical");
    assert.equal(await fs.readFile(path.join(f.root, "index-only.txt"), "utf8"), "different working version\n");
    assert.equal(indexWrites, 0, "undo never writes the shared index");
  });
});

describe("unsafe WIP history absence proof", () => {
  test("requires successful positive evidence and ignores shallow, graft and replace hiding", async t => {
    const f = await fixture(t);
    const parent = f.head();
    await f.write("wip.txt", "dirty\n");
    assert.equal(await f.git.commitWipMarker(f.root), true);
    const marker = f.head();
    f.g("commit", "-q", "--allow-empty", "-m", "later");
    const tip = f.head();
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, parent, marker), true);
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, tip, marker), false);
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, marker, marker), false);
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, tip, "f".repeat(40)), false, "missing object is unknown");
    await fs.writeFile(path.join(f.root, ".git", "shallow"), tip + "\n");
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, tip, marker), false, "shallow metadata cannot hide the marker");
    await fs.unlink(path.join(f.root, ".git", "shallow"));
    await fs.writeFile(path.join(f.root, ".git", "info", "grafts"), tip + "\n");
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, tip, marker), false, "grafts cannot hide the marker");
    await fs.unlink(path.join(f.root, ".git", "info", "grafts"));
    f.g("replace", tip, parent);
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, tip, marker), false, "replace refs cannot hide the marker");
    f.git["runGitAsRunner"] = async () => { throw Object.assign(new Error("launch refused"), { code: 1 }); };
    assert.equal(await f.git.wipMarkerAbsentFromHistory(f.root, parent, marker), false, "exit 1 is not proof of absence");
  });
});
