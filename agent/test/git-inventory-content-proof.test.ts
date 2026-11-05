// issue #2969 D5/D6: GitCache.inventorySourceContentProof (strict, byte-exact FINAL source proof) and
// GitCache.inventoryIgnoredResidue (predecessor-only ignored-file gate), against REAL git through the runner-git
// path (runGitAsRunnerBytes). Faults are injected at that transport seam, never by editing the repository.
import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, type InventoryContentProofBounds } from "../src/git.js";
import { nullLogger } from "./helpers.js";

const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const GIT_ENV = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

type BytesResult = { code: number; stdout: Buffer; stderr: Buffer; exceeded?: true };
interface Seam {
  runGitAsRunnerBytes(cwd: string, args: string[], opts?: { input?: Buffer }): Promise<BytesResult>;
}
interface Call { args: string[]; input?: Buffer }

function fixture(t: test.TestContext) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "content-proof-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const root = path.join(dir, "clone");
  fs.mkdirSync(root);
  const g = (...args: string[]) => execFileSync("git", ["-C", root, ...args],
    { env: GIT_ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  g("init", "-q", "-b", "main");
  for (const [k, v] of [["user.name", "fixture"], ["user.email", "fixture@example.com"], ["commit.gpgsign", "false"],
    ["gc.auto", "0"], ["core.fsmonitor", "false"]]) g("config", k!, v!);
  const git = new GitCache(dir, nullLogger());
  const seam = git as unknown as Seam;
  const real = seam.runGitAsRunnerBytes.bind(git);
  const calls: Call[] = [];
  let override: ((args: string[], real: () => Promise<BytesResult>) => Promise<BytesResult> | undefined) | undefined;
  seam.runGitAsRunnerBytes = async (cwd, args, opts) => {
    calls.push({ args, ...(opts?.input ? { input: opts.input } : {}) });
    return override?.(args, () => real(cwd, args, opts)) ?? real(cwd, args, opts);
  };
  const commitAll = () => { g("add", "-A"); g("commit", "-q", "-m", "base"); };
  return {
    dir, root, g, git, calls, commitAll,
    inject: (fn: typeof override) => { override = fn; },
    hashCalls: () => calls.filter(c => c.args[0] === "hash-object"),
    proof: (bounds?: InventoryContentProofBounds) => git.inventorySourceContentProof(root, bounds),
  };
}

const lines = (buf: Buffer | undefined): string[] => buf === undefined ? [] :
  buf.toString("latin1").split("\n").slice(0, -1);

describe("inventorySourceContentProof (issue #2969 D5)", () => {
  test("tab, leading-dash and non-UTF-8 names are each hashed exactly once and come out clean", async t => {
    const f = fixture(t);
    const raw = Buffer.concat([Buffer.from(f.root + "/bad-"), Buffer.from([0xff, 0xfe, 0x80]), Buffer.from(".bin")]);
    fs.writeFileSync(path.join(f.root, "tab\tname.txt"), "tab\n");
    fs.writeFileSync(path.join(f.root, "-n"), "dash n\n");
    fs.writeFileSync(path.join(f.root, "--help"), "dash help\n");
    fs.writeFileSync(raw, "raw\n");
    fs.mkdirSync(path.join(f.root, "sub dir"));
    fs.writeFileSync(path.join(f.root, "sub dir", "inner.txt"), "inner\n");
    fs.writeFileSync(path.join(f.root, "run.sh"), "#!/bin/sh\n", { mode: 0o755 });
    fs.symlinkSync("-n", path.join(f.root, "link"));
    f.commitAll();
    assert.equal(await f.proof(), "clean");
    const hashes = f.hashCalls();
    assert.equal(hashes.length, 1, "one hash-object dispatch");
    assert.deepEqual(hashes[0]!.args, ["hash-object", "--no-filters", "--stdin-paths"], "paths ride stdin, never argv");
    const sent = lines(hashes[0]!.input);
    assert.equal(sent.length, 6, "every regular file once; the symlink is covered by status");
    assert.deepEqual(sent.slice().sort(), ["--help", "-n", "bad-\xff\xfe\x80.bin", "run.sh", "sub dir/inner.txt", "tab\tname.txt"].sort());
    for (const call of f.calls) for (const arg of call.args) assert.ok(!arg.includes("bad-") && arg !== "-n" && arg !== "--help", "no path in argv");
  });

  test("the quote collision refuses as unreadable and the colliding path is never sent", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, '"x"'), "quoted\n");
    fs.writeFileSync(path.join(f.root, "x"), "plain\n");
    f.commitAll();
    assert.equal(await f.proof(), "unreadable");
    assert.equal(f.hashCalls().length, 0, "refused before any hash-object dispatch");
  });

  test("the trailing-CR collision refuses as unreadable and the colliding path is never sent", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "y\r"), "cr\n");
    fs.writeFileSync(path.join(f.root, "y"), "plain\n");
    f.commitAll();
    assert.equal(await f.proof(), "unreadable");
    assert.equal(f.hashCalls().length, 0);
  });

  test("a name with a newline refuses and is never sent", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "two\nlines"), "nl\n");
    fs.writeFileSync(path.join(f.root, "ok.txt"), "ok\n");
    f.commitAll();
    assert.equal(await f.proof(), "unreadable");
    assert.equal(f.hashCalls().length, 0);
  });

  test("an unreadable tracked file is unreadable", {
    skip: process.getuid?.() === 0 && "root reads every file; the unreadable case needs the real UID lane (M4)",
  }, async t => {
    const f = fixture(t);
    f.g("config", "core.trustctime", "false");
    const file = path.join(f.root, "secret.txt");
    fs.writeFileSync(file, "s\n");
    const past = new Date(Date.now() - 3_600_000);
    fs.utimesSync(file, past, past);
    f.commitAll();
    fs.chmodSync(file, 0);
    // git's stat cache still calls it clean (the readable-bit change is invisible to it), so only the content read fails.
    assert.equal(f.g("status", "--porcelain"), "", "precondition: status does not read the file");
    assert.equal(await f.proof(), "unreadable");
  });

  test("content that differs only in its hash, with the same stat, is dirty", async t => {
    const f = fixture(t);
    f.g("config", "core.trustctime", "false");
    const file = path.join(f.root, "data.txt");
    fs.writeFileSync(file, "AAAA\n");
    const past = new Date(Date.now() - 3_600_000);
    fs.utimesSync(file, past, past);
    f.commitAll();
    // Same size, same inode, same mtime; only the bytes change.
    fs.writeFileSync(file, "BBBB\n");
    fs.utimesSync(file, past, past);
    assert.equal(f.g("status", "--porcelain"), "", "precondition: git's stat cache calls the tree clean");
    assert.deepEqual(await f.git.worktreeStatus(f.root), [], "the old worktreeStatus proof is blind to it");
    assert.equal(await f.proof(), "dirty");
  });

  test("a plain modification and an untracked file are dirty", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    assert.equal(await f.proof(), "clean");
    fs.writeFileSync(path.join(f.root, "untracked.txt"), "u\n");
    assert.equal(await f.proof(), "dirty");
    fs.rmSync(path.join(f.root, "untracked.txt"));
    fs.writeFileSync(path.join(f.root, "a.txt"), "changed\n");
    assert.equal(await f.proof(), "dirty");
  });

  for (const flag of ["--assume-unchanged", "--skip-worktree"]) {
    test(`${flag} is unreadable`, async t => {
      const f = fixture(t);
      fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
      fs.writeFileSync(path.join(f.root, "b.txt"), "b\n");
      f.commitAll();
      f.g("update-index", flag, "a.txt");
      assert.equal(await f.proof(), "unreadable");
    });
  }

  test("a WIP-omitted oversized file stays visible and is dirty", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "tracked.txt"), "original\n");
    f.commitAll();
    fs.writeFileSync(path.join(f.root, "big.bin"), Buffer.alloc(8 * 1024 * 1024 + 1, 0x61));
    fs.writeFileSync(path.join(f.root, "small-wip.txt"), "ordinary\n");
    assert.equal(await f.git.commitWipMarker(f.root, { mode: "exclude" }), true);
    assert.equal(await f.proof(), "dirty", "the omitted file is still an untracked file");
  });

  describe("transport faults at the runGitAsRunnerBytes seam", () => {
    const setup = (t: test.TestContext) => {
      const f = fixture(t);
      fs.writeFileSync(path.join(f.root, "one.txt"), "1\n");
      fs.writeFileSync(path.join(f.root, "two.txt"), "2\n");
      fs.writeFileSync(path.join(f.root, "three.txt"), "3\n");
      f.commitAll();
      return f;
    };
    const faultHash = (mutate: (out: Buffer) => Buffer) => (args: string[], real: () => Promise<BytesResult>) =>
      args[0] === "hash-object" ? real().then(r => ({ ...r, stdout: mutate(r.stdout) })) : undefined;

    test("the unmodified transport is clean (control)", async t => {
      const f = setup(t);
      assert.equal(await f.proof(), "clean");
    });
    test("truncated output", async t => {
      const f = setup(t);
      f.inject(faultHash(out => out.subarray(0, out.length - 7)));
      assert.equal(await f.proof(), "unreadable");
    });
    test("a missing line", async t => {
      const f = setup(t);
      f.inject(faultHash(out => Buffer.from(lines(out).slice(1).join("\n") + "\n")));
      assert.equal(await f.proof(), "unreadable");
    });
    test("an extra line", async t => {
      const f = setup(t);
      f.inject(faultHash(out => Buffer.concat([out, Buffer.from("0".repeat(40) + "\n")])));
      assert.equal(await f.proof(), "unreadable");
    });
    test("a malformed line", async t => {
      const f = setup(t);
      f.inject(faultHash(out => Buffer.concat([out.subarray(0, 39), Buffer.from("Z\n"), out.subarray(41)])));
      assert.equal(await f.proof(), "unreadable");
    });
    test("an out-of-order result is dirty, never clean", async t => {
      const f = setup(t);
      f.inject(faultHash(out => Buffer.from(lines(out).reverse().join("\n") + "\n")));
      assert.equal(await f.proof(), "dirty");
    });
    for (const step of ["status", "ls-files", "rev-parse", "hash-object"]) {
      test(`stderr on exit 0 from ${step} is unreadable`, async t => {
        const f = setup(t);
        f.inject((args, real) => args[0] === step ? real().then(r => ({ ...r, stderr: Buffer.from("warning: boo\n") })) : undefined);
        assert.equal(await f.proof(), "unreadable");
      });
      test(`a non-zero exit from ${step} is unreadable`, async t => {
        const f = setup(t);
        f.inject((args, real) => args[0] === step ? real().then(r => ({ ...r, code: 128 })) : undefined);
        assert.equal(await f.proof(), "unreadable");
      });
    }
  });

  describe("caps", () => {
    test("more entries than maxEntries is over_bound", async t => {
      const f = fixture(t);
      for (const n of ["a", "b", "c"]) fs.writeFileSync(path.join(f.root, n), n);
      f.commitAll();
      assert.equal(await f.proof({ maxEntries: 2, maxBytes: 1 << 20 }), "over_bound");
      assert.equal(await f.proof({ maxEntries: 3, maxBytes: 1 << 20 }), "clean");
      assert.equal(f.hashCalls().length, 1, "only the in-bound proof dispatched hash-object");
    });
    test("output past maxBytes is over_bound", async t => {
      const f = fixture(t);
      for (let i = 0; i < 8; i++) fs.writeFileSync(path.join(f.root, `file-number-${i}.txt`), String(i));
      f.commitAll();
      assert.equal(await f.proof({ maxEntries: 100, maxBytes: 64 }), "over_bound");
    });
  });
});

describe("inventoryIgnoredResidue (issue #2969 D6, predecessor only)", () => {
  const setup = (t: test.TestContext, exclude: string[]) => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    fs.appendFileSync(path.join(f.root, ".git", "info", "exclude"), exclude.map(e => e + "\n").join(""));
    return f;
  };
  test("scratch-only content is clean", async t => {
    const f = setup(t, [".uzi/scratch/"]);
    fs.mkdirSync(path.join(f.root, ".uzi", "scratch", "deep"), { recursive: true });
    fs.writeFileSync(path.join(f.root, ".uzi", "scratch", "deep", "log.txt"), "s\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "clean");
    assert.equal(await f.proof(), "clean");
  });
  test("a clone with no .uzi directory is clean", async t => {
    const f = setup(t, [".uzi/scratch/"]);
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "clean");
  });
  test("an ignored build/report.log is residue while the ordinary content proof stays clean", async t => {
    const f = setup(t, [".uzi/scratch/", "build/"]);
    fs.mkdirSync(path.join(f.root, "build"));
    fs.writeFileSync(path.join(f.root, "build", "report.log"), "r\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
    assert.equal(await f.proof(), "clean", "the ordinary path is unchanged for ignored files");
  });
  test("an ignored node_modules/ is residue", async t => {
    const f = setup(t, [".uzi/scratch/", "node_modules/"]);
    fs.mkdirSync(path.join(f.root, "node_modules", "pkg"), { recursive: true });
    fs.writeFileSync(path.join(f.root, "node_modules", "pkg", "index.js"), "m\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
  });
  test("ignored content under .uzi but outside scratch is residue", async t => {
    const f = setup(t, [".uzi/"]);
    fs.mkdirSync(path.join(f.root, ".uzi", "scratch"), { recursive: true });
    fs.writeFileSync(path.join(f.root, ".uzi", "scratch", "ok.txt"), "s\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "clean", ".uzi/ holding only scratch is clean");
    fs.writeFileSync(path.join(f.root, ".uzi", "cache.bin"), "c\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
  });
  test("stderr on exit 0, a non-zero exit and an exception are all residue", async t => {
    const f = setup(t, [".uzi/scratch/"]);
    f.inject((args, real) => args[0] === "ls-files" ? real().then(r => ({ ...r, stderr: Buffer.from("w\n") })) : undefined);
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
    f.inject((args, real) => args[0] === "ls-files" ? real().then(r => ({ ...r, code: 128 })) : undefined);
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
    f.inject(() => Promise.reject(new Error("transport down")));
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
  });
  test("output past maxBytes is over_bound", async t => {
    const f = setup(t, ["junk-*"]);
    for (let i = 0; i < 20; i++) fs.writeFileSync(path.join(f.root, `junk-${i}-with-a-long-name.log`), "j");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root, { maxEntries: 5, maxBytes: 32 }), "over_bound");
  });
});
