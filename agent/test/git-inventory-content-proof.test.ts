// issue #2969 D5/D6: GitCache.inventorySourceContentProof (strict, byte-exact FINAL source proof) and
// GitCache.inventoryIgnoredResidue (predecessor-only ignored-file gate), against REAL git through the runner-git
// path (runGitAsRunnerBytes). Faults are injected at that transport seam, never by editing the repository.
import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, runnerGitCarriesWorkerMark, type InventoryContentProofBounds } from "../src/git.js";
import { nullLogger } from "./helpers.js";

const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const GIT_ENV = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };

type BytesResult = { code: number; stdout: Buffer; stderr: Buffer; exceeded?: true };
interface Seam {
  runGitAsRunnerBytes(cwd: string, args: string[], opts?: { input?: Buffer; timeoutMs?: number }): Promise<BytesResult>;
}
// `args` is the subcommand argv with the leading --git-dir/--work-tree pins split off into `pins`.
interface Call { args: string[]; pins: string[]; config: string[]; input?: Buffer; timeoutMs?: number }
const isPin = (a: string): boolean => a.startsWith("--git-dir=") || a.startsWith("--work-tree=");

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
    // Strip the pins and any leading `-c key=value` pairs; the pins stay recorded, the -c pairs in `config`.
    const noPins = args.filter(a => !isPin(a));
    const config: string[] = [];
    while (noPins[0] === "-c") config.push(...noPins.splice(0, 2).slice(1));
    const bare = noPins;
    calls.push({ args: bare, pins: args.filter(isPin), config, ...(opts?.input ? { input: opts.input } : {}),
      ...(opts?.timeoutMs === undefined ? {} : { timeoutMs: opts.timeoutMs }) });
    return override?.(bare, () => real(cwd, args, opts)) ?? real(cwd, args, opts);
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
    skip: process.getuid?.() === 0 && "root reads every file; this case needs a non-root run (the real UID lane)",
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

  test("--git-dir/--work-tree pins keep the worker mark of ls-files and leave status unmarked", () => {
    assert.equal(runnerGitCarriesWorkerMark(["--git-dir=a", "--work-tree=b", "ls-files"]), true);
    assert.equal(runnerGitCarriesWorkerMark(["--git-dir=a", "--work-tree=b", "-c", "k=v", "rev-parse"]), true);
    assert.equal(runnerGitCarriesWorkerMark(["--git-dir=a", "--work-tree=b", "status"]), false);
  });

  test("a replace ref over a large blob never yields a clean proof or a large read", async t => {
    const f = fixture(t);
    const big = path.join(f.root, "big.bin");
    fs.writeFileSync(big, "");
    fs.truncateSync(big, 8 * 1024 * 1024);
    f.commitAll();
    const bigOid = f.g("rev-parse", "HEAD:big.bin");
    const tiny = execFileSync("git", ["-C", f.root, "hash-object", "-w", "--stdin"], { env: GIT_ENV, input: "tiny\n", encoding: "utf8" }).trim();
    f.g("replace", bigOid, tiny);
    // With replacement honoured the index would report 5 bytes for an 8 MiB blob; the pin makes the real size visible.
    const result = await f.proof({ maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 1024 * 1024 });
    assert.equal(result, "over_bound");
    assert.equal(f.hashCalls().length, 0);
    assert.ok(f.calls.length > 0);
    assert.equal(await f.proof(), "clean", "with the default cap the genuine content still proves clean");
  });

  test("a path with a filter attribute is unreadable with zero hash dispatch", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, ".gitattributes"), "*.lfs filter=lfs\n");
    fs.writeFileSync(path.join(f.root, "pointer.lfs"), "tiny pointer\n");
    fs.writeFileSync(path.join(f.root, "plain.txt"), "p\n");
    f.commitAll();
    assert.equal(await f.proof(), "unreadable");
    assert.equal(f.hashCalls().length, 0);
    // filter=unset / no attribute is fine
    fs.writeFileSync(path.join(f.root, ".gitattributes"), "*.lfs -filter\n");
    f.commitAll();
    assert.equal(await f.proof(), "clean");
  });

  test("core.checkStat=minimal in .git/config does not stop the status rescan (the pin overrides it)", async t => {
    const f = fixture(t);
    f.g("config", "core.trustctime", "false");
    f.g("config", "core.checkStat", "minimal");
    const file = path.join(f.root, "data.txt");
    fs.writeFileSync(file, "AAAA\n");
    const past = new Date(Date.now() - 3_600_000);
    fs.utimesSync(file, past, past);
    f.commitAll();
    // ctime moves on rewrite; only a pinned trustctime=true notices it when the config says otherwise.
    fs.writeFileSync(file, "BBBB\n");
    fs.utimesSync(file, past, past);
    assert.equal(f.g("status", "--porcelain"), "", "precondition: the repository's own config calls it clean");
    const status = f.calls.length;
    assert.equal(await f.proof(), "dirty");
    const statusCall = f.calls.slice(status).find(c => c.args[0] === "status")!;
    assert.ok(statusCall.config.includes("core.checkStat=default") && statusCall.config.includes("core.trustctime=true") &&
      statusCall.config.includes("core.untrackedCache=false"));
  });

  test("the whole proof shares one time budget", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    const timeouts: number[] = [];
    const seam = f.git as unknown as Seam;
    const inner = seam.runGitAsRunnerBytes;
    seam.runGitAsRunnerBytes = async (cwd, args, opts) => {
      timeouts.push(opts?.timeoutMs ?? -1);
      await new Promise(resolve => setTimeout(resolve, 120));
      return inner(cwd, args, opts);
    };
    assert.equal(await f.proof({ maxEntries: 10, maxBytes: 1 << 20, deadlineAt: Date.now() + 300 }), "unreadable");
    assert.ok(timeouts.length >= 2 && timeouts.length < 6, "stopped dispatching once the budget was gone: " + timeouts.length);
    for (let i = 1; i < timeouts.length; i++) assert.ok(timeouts[i]! < timeouts[i - 1]!, "each call gets only what is left");
    assert.ok(timeouts[0]! <= 300);
  });

  test("an add-time-only filter cannot hide a huge file: the real size is charged before any hashing", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, ".git", "info", "attributes"), "*.big filter=shrink\n");
    f.g("config", "filter.shrink.clean", "echo tiny");
    f.g("config", "filter.shrink.required", "true");
    const big = path.join(f.root, "data.big");
    fs.writeFileSync(big, "");
    fs.truncateSync(big, 64 * 1024 * 1024);
    f.commitAll();
    assert.equal(f.g("cat-file", "-s", "HEAD:data.big"), "5", "the index declares a tiny blob");
    // Remove the filter and the attribute: status stays clean (size/mtime unchanged), the worktree file is 64 MiB.
    fs.rmSync(path.join(f.root, ".git", "info", "attributes"));
    f.g("config", "--remove-section", "filter.shrink");
    const bounds = { maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 32 * 1024 * 1024 };
    assert.equal(await f.proof(bounds), "over_bound");
    assert.equal(f.hashCalls().length, 0);
    assert.equal(f.calls.filter(c => c.args[0] === "status" || c.args[0] === "check-attr").length, 0, "refused before status and check-attr");
  });

  test("ident expansion past the cap is over_bound", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, ".gitattributes"), "*.txt ident\n");
    fs.writeFileSync(path.join(f.root, "a.txt"), "$Id$\n");
    f.commitAll();
    const file = path.join(f.root, "a.txt");
    fs.writeFileSync(file, "$Id: 0123456789012345678901234567890123456789 $\n".repeat(200));
    assert.equal(await f.proof({ maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 1024 }), "over_bound");
    assert.equal(f.hashCalls().length, 0);
  });

  test("a symlink where the index has a regular file is unreadable", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    fs.writeFileSync(path.join(f.root, "target.txt"), "t\n");
    f.commitAll();
    fs.rmSync(path.join(f.root, "a.txt"));
    fs.symlinkSync("target.txt", path.join(f.root, "a.txt"));
    assert.equal(await f.proof(), "unreadable");
    assert.equal(f.hashCalls().length, 0);
  });

  test("a missing tracked file is dirty", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    fs.rmSync(path.join(f.root, "a.txt"));
    assert.equal(await f.proof(), "dirty");
    assert.equal(f.hashCalls().length, 0);
  });

  test("a non-UTF-8 path is lstatted by its bytes: its real size is what trips the cap", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, ".git", "info", "attributes"), "*.big filter=shrink\n");
    f.g("config", "filter.shrink.clean", "echo tiny");
    const raw = Buffer.concat([Buffer.from(f.root + "/odd-"), Buffer.from([0xff, 0xfe]), Buffer.from(".big")]);
    fs.writeFileSync(raw, "");
    fs.truncateSync(raw, 8 * 1024 * 1024);
    f.commitAll();
    fs.rmSync(path.join(f.root, ".git", "info", "attributes"));
    f.g("config", "--remove-section", "filter.shrink");
    const bounds = { maxEntries: 10, maxBytes: 1 << 20 };
    assert.equal(await f.proof({ ...bounds, maxContentBytes: 1024 * 1024 }), "over_bound", "declared size is 5 bytes");
    assert.equal(f.hashCalls().length, 0);
    assert.equal(await f.proof({ ...bounds, maxContentBytes: 16 * 1024 * 1024 }), "dirty", "within the cap it is hashed, and the content differs from the blob");
  });

  test("every git call pins --git-dir and --work-tree to the clone", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    await f.proof();
    await f.git.inventoryIgnoredResidue(f.root);
    assert.ok(f.calls.length >= 6);
    for (const c of f.calls) assert.deepEqual(c.pins, [`--git-dir=${f.root}/.git`, `--work-tree=${f.root}`]);
  });

  test("core.worktree in the agent-writable config cannot redirect status or the listings", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    fs.appendFileSync(path.join(f.root, ".git", "info", "exclude"), "build/\n");
    const elsewhere = path.join(f.dir, "elsewhere");
    fs.mkdirSync(elsewhere);
    fs.writeFileSync(path.join(elsewhere, "a.txt"), "a\n");
    // Fixture data: the config an agent could write.
    fs.appendFileSync(path.join(f.root, ".git", "config"), `[core]\n\tworktree = ${elsewhere}\n`);
    assert.equal(await f.proof(), "clean");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "clean");
    fs.writeFileSync(path.join(f.root, "untracked.txt"), "u\n");
    assert.equal(await f.proof(), "dirty", "untracked files in the clone are still seen");
    fs.rmSync(path.join(f.root, "untracked.txt"));
    fs.mkdirSync(path.join(f.root, "build"));
    fs.writeFileSync(path.join(f.root, "build", "o.log"), "o\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue", "ignored files in the clone are still seen");
  });

  test("a .git that is a gitfile or a symlink is unreadable", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    const real = path.join(f.dir, "real-git");
    fs.renameSync(path.join(f.root, ".git"), real);
    fs.writeFileSync(path.join(f.root, ".git"), `gitdir: ${real}\n`);
    assert.equal(await f.proof(), "unreadable");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
    fs.rmSync(path.join(f.root, ".git"));
    fs.symlinkSync(real, path.join(f.root, ".git"));
    assert.equal(await f.proof(), "unreadable");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
    assert.equal(f.calls.length, 0, "no git ran against a redirectable .git");
  });

  test("a passed deadline is unreadable and every call timeout is clipped to it", async t => {
    const f = fixture(t);
    fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
    f.commitAll();
    assert.equal(await f.proof({ maxEntries: 10, maxBytes: 1 << 20, deadlineAt: Date.now() - 1 }), "unreadable");
    assert.equal(f.calls.length, 0);
    assert.equal(await f.proof({ maxEntries: 10, maxBytes: 1 << 20, deadlineAt: Date.now() + 5_000 }), "clean");
    assert.ok(f.calls.length > 0 && f.calls.every(c => c.timeoutMs !== undefined && c.timeoutMs <= 5_000 && c.timeoutMs > 0));
    f.calls.length = 0;
    assert.equal(await f.proof(), "clean");
    assert.ok(f.calls.every(c => c.timeoutMs! <= 60_000 && c.timeoutMs! > 59_000), "the 60s total budget bounds each call without a deadline");
  });

  describe("caps", () => {
    test("a sparse large tracked file over maxContentBytes is over_bound with zero hash-object dispatch", async t => {
      const f = fixture(t);
      const big = path.join(f.root, "sparse.bin");
      fs.writeFileSync(big, "");
      fs.truncateSync(big, 64 * 1024 * 1024);
      fs.writeFileSync(path.join(f.root, "small.txt"), "s\n");
      f.commitAll();
      assert.equal(await f.proof({ maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 32 * 1024 * 1024 }), "over_bound");
      assert.equal(f.hashCalls().length, 0);
      assert.equal(await f.proof({ maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 128 * 1024 * 1024 }), "clean");
      assert.equal(f.hashCalls().length, 1);
    });
    test("many files whose sizes sum over maxContentBytes are over_bound before hashing", async t => {
      const f = fixture(t);
      for (let i = 0; i < 3; i++) fs.writeFileSync(path.join(f.root, `f${i}.txt`), `${i}`.repeat(600));
      f.commitAll();
      assert.equal(await f.proof({ maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 1000 }), "over_bound");
      assert.equal(f.hashCalls().length, 0);
      assert.equal(await f.proof({ maxEntries: 100, maxBytes: 1 << 20, maxContentBytes: 1800 }), "clean");
    });
    for (const [name, match] of [["status", (a: string[]) => a[0] === "status"], ["ls-files -v", (a: string[]) => a.includes("-v")],
      ["ls-files staged listing", (a: string[]) => a.some(x => x.startsWith("--format"))], ["hash-object", (a: string[]) => a[0] === "hash-object"]] as const) {
      test(`${name} output past its cap is over_bound`, async t => {
        const f = fixture(t);
        fs.writeFileSync(path.join(f.root, "a.txt"), "a\n");
        f.commitAll();
        f.inject((args, real) => match(args) ? real().then(r => ({ ...r, code: -1, exceeded: true as const })) : undefined);
        assert.equal(await f.proof(), "over_bound");
      });
    }
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
  test("a nested repository at .uzi hides nothing: it is residue", async t => {
    const f = setup(t, [".uzi/"]);
    fs.mkdirSync(path.join(f.root, ".uzi", "build"), { recursive: true });
    fs.writeFileSync(path.join(f.root, ".uzi", "build", "out.o"), "o\n");
    execFileSync("git", ["init", "-q", path.join(f.root, ".uzi")], { env: GIT_ENV, stdio: "ignore" });
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue");
    fs.rmSync(path.join(f.root, ".uzi", ".git"), { recursive: true });
    fs.writeFileSync(path.join(f.root, ".uzi", ".git"), "gitdir: /nowhere\n");
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "residue", "a .git of any type");
  });
  test("an empty ignored directory is not residue", async t => {
    const f = setup(t, [".uzi/scratch/", "emptydir/"]);
    fs.mkdirSync(path.join(f.root, "emptydir"));
    assert.equal(await f.git.inventoryIgnoredResidue(f.root), "clean");
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
