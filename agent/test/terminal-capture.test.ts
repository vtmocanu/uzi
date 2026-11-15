// Terminal-preservation capture primitives of GitCache (production-dark): captureTerminalSnapshot,
// buildTerminalAggregate, terminalScanDigest, terminalTransportDigest and changedCiPathsPerCommit,
// exercised against real git fixtures (a real clone and a real bare).
import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { GitCache, TERMINAL_INDEX_HASH_SCRIPT, type CheckpointRange, type TerminalSnapshotOptions } from "../src/git.js";
import { CheckpointUnsafeCleanupError, GitBoundaryAbortError } from "../src/checkpoint-stop.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import type { CodexRootHandle } from "../src/codex/launcher.js";
import { credentialFreeGitFixture } from "./credential-free-git-fixture.js";
import { nullLogger } from "./helpers.js";

const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
const gitEnv = { ...cleanEnv, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const MIB = 1024 * 1024;

function run(cwd: string, ...args: string[]): string {
  return execFileSync("/usr/bin/git", ["-C", cwd, ...args], { env: gitEnv, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
}

async function fixture(t: test.TestContext, opts: { ignore?: string } = {}) {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "term-capture-"));
  const tmp = path.join(dir, "runner-tmp");
  await fs.mkdir(tmp);
  const previousTmp = process.env.UZI_RUNNER_TMPDIR;
  process.env.UZI_RUNNER_TMPDIR = tmp;
  // GitCache(dir) roots runner clones at <dir>/runner.
  const root = path.join(dir, "runner", "repo", "clone");
  await fs.mkdir(root, { recursive: true });
  const g = (...args: string[]) => run(root, ...args);
  g("init", "-q", "-b", "main");
  for (const [k, v] of [["user.name", "fixture"], ["user.email", "fixture@example.com"], ["commit.gpgsign", "false"],
    ["gc.auto", "0"], ["core.fsmonitor", "false"]]) g("config", k!, v!);
  await fs.writeFile(path.join(root, "tracked.txt"), "original\n");
  await fs.writeFile(path.join(root, "gone.txt"), "to be deleted\n");
  await fs.writeFile(path.join(root, ".gitignore"), opts.ignore ?? "ignored.log\n.uzi/scratch/\n");
  g("add", "-A");
  g("commit", "-q", "-m", "base");
  const bare = path.join(dir, "bare.git");
  execFileSync("/usr/bin/git", ["clone", "-q", "--bare", root, bare], { env: gitEnv });
  // Refs a capture must never move.
  const head = g("rev-parse", "HEAD");
  g("update-ref", "refs/uzi-runner/main", head);
  g("update-ref", "refs/uzi-checkpoints/agent/issue-1", head);
  // Production shape after fetchAgentBranch: the clone HEAD is reachable from the bare's runner ref.
  run(bare, "update-ref", "refs/uzi-runner/main", head);
  const git = new GitCache(dir, nullLogger());
  t.after(async () => {
    if (previousTmp === undefined) delete process.env.UZI_RUNNER_TMPDIR; else process.env.UZI_RUNNER_TMPDIR = previousTmp;
    await fs.rm(dir, { recursive: true, force: true });
  });
  const write = (rel: string, content: string | Buffer) => fs.mkdir(path.dirname(path.join(root, rel)), { recursive: true })
    .then(() => fs.writeFile(path.join(root, rel), content));
  const indexBytes = () => fs.readFile(path.join(root, ".git", "index"));
  const stray = async () => (await fs.readdir(tmp)).filter((n) => n.startsWith("uzi-term-idx-"));
  const refsOf = (cwd: string) => run(cwd, "for-each-ref", "--format=%(refname) %(objectname)");
  const state = async () => ({
    head: g("rev-parse", "HEAD"), branch: g("rev-parse", "refs/heads/main"),
    runner: g("rev-parse", "refs/uzi-runner/main"), checkpoint: g("rev-parse", "refs/uzi-checkpoints/agent/issue-1"),
    index: (await indexBytes()).toString("hex"),
  });
  const calls: string[] = [];
  const capture = (extra: Partial<TerminalSnapshotOptions> = {}) =>
    git.captureTerminalSnapshot(root, { runId: "run-1", generation: 2, deadlineMs: 60_000, barePath: bare, ...extra });
  return { dir, root, bare, tmp, g, git, write, indexBytes, stray, refsOf, state, capture, calls, head,
    ref: "refs/uzi-terminal-local/run-1/g2" };
}

describe("captureTerminalSnapshot", () => {
  test("captures dirty and untracked work into a snapshot tree and leaves the clone untouched", async (t) => {
    const f = await fixture(t);
    await f.write("tracked.txt", "changed\n");
    await fs.rm(path.join(f.root, "gone.txt"));
    await f.write("new.txt", "brand new\n");
    await f.write("dir/run.sh", "#!/bin/sh\n");
    await fs.chmod(path.join(f.root, "dir/run.sh"), 0o755);
    await fs.symlink("tracked.txt", path.join(f.root, "link"));
    await f.write("ignored.log", "0123456789");
    await f.write(".uzi/scratch/out.txt", "12345");
    const before = await f.state();
    const result = await f.capture();
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind !== "captured") return;
    assert.equal(result.parent, before.head);
    assert.deepEqual(result.ignored, { files: 2, bytes: 15 });
    assert.deepEqual(await f.state(), before, "HEAD, branch, runner and checkpoint refs and the real index bytes are unchanged");
    assert.equal(run(f.bare, "rev-parse", `${result.root}^{tree}`), result.tree);
    assert.equal(run(f.bare, "rev-parse", `${result.root}^`), before.head);
    assert.equal(run(f.bare, "show", `${result.root}:tracked.txt`), "changed");
    assert.equal(run(f.bare, "show", `${result.root}:new.txt`), "brand new");
    const tree = run(f.bare, "ls-tree", "-r", result.root);
    assert.match(tree, /^100755 blob [0-9a-f]{40}\tdir\/run\.sh$/m);
    assert.match(tree, /^120000 blob [0-9a-f]{40}\tlink$/m);
    assert.ok(!/gone\.txt/.test(tree), "deleted file absent from the snapshot");
    assert.ok(!/ignored\.log|scratch/.test(tree), "ignored files are counted, never captured");
    assert.deepEqual(await f.stray(), [], "the private index is removed");
    // Deterministic: the same work captured again names the same commit.
    const again = await f.capture({ onPhase: async () => undefined });
    assert.equal(again.kind, "captured");
    if (again.kind === "captured") assert.equal(again.root, result.root);
  });

  test("a clean tree reports the HEAD commit and writes no ref or index", async (t) => {
    const f = await fixture(t);
    await f.write("ignored.log", "abc");
    const before = await f.state();
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "clean", root: before.head, tree: run(f.root, "rev-parse", "HEAD^{tree}"), ignored: { files: 1, bytes: 3 } });
    assert.deepEqual(f.calls, []);
    assert.deepEqual(await f.state(), before);
    assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false);
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
    assert.deepEqual(await f.stray(), []);
  });

  test("a scratch directory that is not ignored is refused before staging", async (t) => {
    const f = await fixture(t, { ignore: "ignored.log\n" });
    await f.write(".uzi/scratch/out.txt", "x");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "scratch_not_ignored" });
    assert.deepEqual(f.calls, []);
  });

  for (const name of [".netrc", "auth.json", ".credentials.json", ".claude.json", ".git-credentials", "id_rsa", "id_ed25519", "cert.pem", "deploy.key", "sub/.netrc",
    ".npmrc", ".pypirc", ".env", "auth.json.bak", ".docker/config.json", ".kube/config", ".config/gh/hosts.yml"]) {
    test(`credential path ${name} is refused before any staging`, async (t) => {
      const f = await fixture(t);
      await f.write("tracked.txt", "changed\n");
      await f.write(name, "secret\n");
      const before = await f.state();
      const result = await f.capture({ onPhase: async (p) => { f.calls.push(p); }, seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
      assert.deepEqual(result, { kind: "refused", reason: "credential_path" });
      assert.deepEqual(f.calls, [], "neither staging nor a phase was reached");
      assert.deepEqual(await f.stray(), []);
      assert.deepEqual(await f.state(), before);
      assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false);
    });
  }

  test("the worker token file is refused by basename", async (t) => {
    const f = await fixture(t);
    await f.write("uzi-worker-token", "tok\n");
    const result = await f.capture({ workerTokenFile: "/run/secrets/uzi-worker-token" });
    assert.deepEqual(result, { kind: "refused", reason: "credential_path" });
  });

  test("an over-limit file is refused before staging", async (t) => {
    const f = await fixture(t);
    await f.write("big.bin", Buffer.alloc(8 * MIB + 1, 0x61));
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "oversized" });
    assert.deepEqual(f.calls, []);
    assert.deepEqual(await f.stray(), []);
  });

  test("a core dump is refused", async (t) => {
    const f = await fixture(t);
    await f.write("core.123", "dump");
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "core_file" });
  });

  test("a file that grows past the cap between enumeration and staging is refused at staged validation", async (t) => {
    const f = await fixture(t);
    await f.write("grower.txt", "small\n");
    const before = await f.state();
    const result = await f.capture({ seam: { beforeStaging: async () => { await f.write("grower.txt", Buffer.alloc(8 * MIB + 1, 0x62)); } } });
    assert.deepEqual(result, { kind: "refused", reason: "staged_oversized" });
    assert.deepEqual(await f.state(), before);
    assert.deepEqual(await f.stray(), [], "the private index is removed on refusal");
    assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false);
  });

  test("a file that changes size between enumeration and staging is refused", async (t) => {
    const f = await fixture(t);
    await f.write("grower.txt", "small\n");
    const result = await f.capture({ seam: { beforeStaging: async () => { await f.write("grower.txt", "a different and longer body\n"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "staged_size_changed" });
  });

  test("a path that appears after enumeration is refused", async (t) => {
    const f = await fixture(t);
    await f.write("tracked.txt", "changed\n");
    const result = await f.capture({ seam: { beforeStaging: async () => { await f.write("late.txt", "late\n"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "staged_set_mismatch" });
  });

  test("an in-tree filter attribute with no filter configured is ignored: the raw work-tree bytes are captured", async (t) => {
    const f = await fixture(t);
    await f.write(".gitattributes", "*.dat filter=lfsx\n");
    await f.write("a.dat", "payload\n");
    const result = await f.capture();
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind !== "captured") return;
    assert.equal(run(f.bare, "show", `${result.root}:a.dat`), "payload");
    assert.equal(run(f.bare, "show", `${result.root}:.gitattributes`), "*.dat filter=lfsx");
  });

  // An agent-controlled clean filter wired through local config and .git/info/attributes (which git always reads)
  // rewrites a staged edit back to HEAD's bytes, hiding it from the verdict or the snapshot.
  async function plantHidingFilter(f: Awaited<ReturnType<typeof fixture>>) {
    const original = path.join(f.dir, "head-bytes");
    await fs.writeFile(original, "original\n");
    f.g("config", "filter.h.clean", `cat ${original}`);
    await fs.writeFile(path.join(f.root, ".git", "info", "attributes"), "tracked.txt filter=h\n");
    await f.write("tracked.txt", "edited\n");
    f.g("add", "tracked.txt");
  }

  test("a clean filter hiding a staged edit is never reported clean", async (t) => {
    const f = await fixture(t);
    await plantHidingFilter(f);
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.equal(result.kind, "refused", JSON.stringify(result));
    assert.deepEqual(f.calls, []);
  });

  test("a clean filter never makes a capture carry HEAD's bytes for an edited file", async (t) => {
    const f = await fixture(t);
    await plantHidingFilter(f);
    await f.write("new.txt", "real new file\n");
    const result = await f.capture();
    assert.equal(result.kind, "refused", JSON.stringify(result));
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  test("a filter.* local config key is refused even with only an in-tree .gitattributes", async (t) => {
    const f = await fixture(t);
    f.g("config", "filter.h.clean", "cat /dev/null");
    await f.write(".gitattributes", "tracked.txt filter=h\n");
    await f.write("tracked.txt", "edited\n");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "config_refused" });
    assert.deepEqual(f.calls, []);
  });

  // Line-ending conversion configured in the agent-writable local config or attributes file must never rewrite the
  // captured bytes or hide a CRLF-only edit: the verdict is either refused or the CRLF bytes themselves.
  const rawShow = (f: Awaited<ReturnType<typeof fixture>>, root: string, rel: string) =>
    execFileSync("/usr/bin/git", ["-C", f.bare, "show", `${root}:${rel}`], { env: gitEnv });
  const CRLF = Buffer.from("original\r\n");

  test("core.autocrlf=input with a staged CRLF-only edit is never reported clean", async (t) => {
    const f = await fixture(t);
    f.g("config", "core.autocrlf", "input");
    f.g("config", "core.safecrlf", "false");
    await f.write("tracked.txt", CRLF);
    f.g("add", "tracked.txt");
    const result = await f.capture();
    assert.notEqual(result.kind, "clean", JSON.stringify(result));
    if (result.kind === "captured") assert.deepEqual(rawShow(f, result.root, "tracked.txt"), CRLF);
  });

  test("core.autocrlf=input with an unstaged CRLF-only edit and a new file never captures HEAD's bytes for the edit", async (t) => {
    const f = await fixture(t);
    f.g("config", "core.autocrlf", "input");
    f.g("config", "core.safecrlf", "false");
    await f.write("tracked.txt", CRLF);
    await f.write("new.txt", "brand new\n");
    const result = await f.capture();
    assert.notEqual(result.kind, "clean", JSON.stringify(result));
    if (result.kind === "captured") assert.deepEqual(rawShow(f, result.root, "tracked.txt"), CRLF);
  });

  test("a local core.attributesFile with a text rule never normalizes a CRLF-only edit into a clean verdict", async (t) => {
    const f = await fixture(t);
    const attrs = path.join(f.dir, "evil-attributes");
    await fs.writeFile(attrs, "* text\n");
    f.g("config", "core.attributesFile", attrs);
    f.g("config", "core.safecrlf", "false");
    await f.write("tracked.txt", CRLF);
    f.g("add", "tracked.txt");
    const result = await f.capture();
    assert.notEqual(result.kind, "clean", JSON.stringify(result));
    if (result.kind === "captured") assert.deepEqual(rawShow(f, result.root, "tracked.txt"), CRLF);
  });

  test("core.symlinks=false never hides a symlink replaced by a regular file with the same bytes", async (t) => {
    const f = await fixture(t);
    await fs.symlink("tracked.txt", path.join(f.root, "link"));
    f.g("add", "link"); f.g("commit", "-q", "-m", "adds link");
    const head = f.g("rev-parse", "HEAD");
    f.g("push", "-q", f.bare, `${head}:refs/uzi-runner/main`);
    f.g("update-ref", "refs/uzi-runner/main", head);
    f.g("config", "core.symlinks", "false");
    await fs.rm(path.join(f.root, "link"));
    await f.write("link", "tracked.txt");
    const result = await f.capture();
    assert.notEqual(result.kind, "clean", JSON.stringify(result));
    if (result.kind === "captured") assert.equal(run(f.bare, "ls-tree", result.root, "link").split(" ")[0], "100644");
  });

  test("a .git/info/attributes file alone is refused", async (t) => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, ".git", "info", "attributes"), "# nothing\n");
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "config_refused" });
  });

  test("an unreadable untracked directory is refused as an enumeration warning", async (t) => {
    if (process.getuid?.() === 0) { t.skip("running as root: chmod 000 does not hide a directory from git"); return; }
    const f = await fixture(t);
    await f.write("hidden/secret.txt", "work the enumeration cannot see\n");
    const hidden = path.join(f.root, "hidden");
    await fs.chmod(hidden, 0o000);
    try {
      const before = await f.state();
      const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
      assert.deepEqual(result, { kind: "refused", reason: "enumeration_warning" });
      assert.deepEqual(f.calls, []);
      assert.deepEqual(await f.state(), before);
    } finally {
      await fs.chmod(hidden, 0o755);
    }
  });

  for (const flag of ["--assume-unchanged", "--skip-worktree"]) {
    test(`an edited tracked file hidden by ${flag} is refused as index_flags`, async (t) => {
      const f = await fixture(t);
      f.g("update-index", flag, "tracked.txt");
      await f.write("tracked.txt", "edited behind the index\n");
      const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
      assert.deepEqual(result, { kind: "refused", reason: "index_flags" });
      assert.deepEqual(f.calls, []);
      assert.deepEqual(await f.stray(), []);
    });
  }

  test("a nested repository is refused", async (t) => {
    const f = await fixture(t);
    const nested = path.join(f.root, "sub");
    await fs.mkdir(nested);
    run(nested, "init", "-q");
    await fs.writeFile(path.join(nested, "x"), "x");
    run(nested, "-c", "user.name=a", "-c", "user.email=a@b.c", "add", "x");
    run(nested, "-c", "user.name=a", "-c", "user.email=a@b.c", "commit", "-q", "-m", "n");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "unsafe_path" });
    assert.deepEqual(f.calls, []);
  });

  test("a tracked gitlink with worktree changes is refused", async (t) => {
    const f = await fixture(t);
    const nested = path.join(f.root, "sub2");
    await fs.mkdir(nested);
    run(nested, "init", "-q", "-b", "main");
    await fs.writeFile(path.join(nested, "x"), "x");
    run(nested, "-c", "user.name=a", "-c", "user.email=a@b.c", "add", "x");
    run(nested, "-c", "user.name=a", "-c", "user.email=a@b.c", "commit", "-q", "-m", "n");
    run(f.root, "add", "sub2");
    run(f.root, "commit", "-q", "-m", "gitlink");
    await fs.writeFile(path.join(nested, "x"), "dirty");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "unsafe_type" });
    assert.deepEqual(f.calls, []);
  });

  test("a tracked file replaced by a fifo is refused as an unsafe type", async (t) => {
    const f = await fixture(t);
    await fs.rm(path.join(f.root, "tracked.txt"));
    execFileSync("mkfifo", [path.join(f.root, "tracked.txt")]);
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "unsafe_type" });
    assert.deepEqual(f.calls, []);
  });

  test("lowered entry and total-byte caps refuse before staging", async (t) => {
    const f = await fixture(t);
    await f.write("a.txt", "aaaaaaaaaaaaaaaaaaaa");
    await f.write("b.txt", "b");
    await f.write("c.txt", "c");
    const seam = { beforeStaging: async () => { f.calls.push("staging"); } };
    assert.deepEqual(await f.capture({ seam, caps: { maxEntries: 2 } }), { kind: "refused", reason: "entry_cap" });
    assert.deepEqual(await f.capture({ seam, caps: { maxTotalBytes: 10 } }), { kind: "refused", reason: "total_bytes" });
    assert.deepEqual(f.calls, []);
  });

  test("a valueless boolean core.sparseCheckout key is refused as configuration", async (t) => {
    const f = await fixture(t);
    await fs.appendFile(path.join(f.root, ".git", "config"), "[core]\n\tsparseCheckout\n");
    await f.write("new.txt", "x");
    assert.deepEqual(await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } }), { kind: "refused", reason: "config_refused" });
    assert.deepEqual(f.calls, []);
  });

  test("a config.worktree core.worktree decoy is refused, never reported clean", async (t) => {
    const f = await fixture(t);
    const decoy = path.join(f.dir, "decoy");
    await fs.mkdir(decoy);
    await fs.writeFile(path.join(decoy, "tracked.txt"), "original\n");
    await fs.writeFile(path.join(decoy, "gone.txt"), "to be deleted\n");
    await fs.writeFile(path.join(decoy, ".gitignore"), "ignored.log\n.uzi/scratch/\n");
    await fs.appendFile(path.join(f.root, ".git", "config"), "[extensions]\n\tworktreeConfig = true\n");
    await fs.writeFile(path.join(f.root, ".git", "config.worktree"), `[core]\n\tworktree = ${decoy}\n`);
    await f.write("tracked.txt", "edited in the real clone\n");
    await f.write("new.txt", "unpreserved\n");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.equal(result.kind, "refused", JSON.stringify(result));
    assert.deepEqual(f.calls, []);
  });

  test("a config.worktree file alone (no extension key) is refused", async (t) => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, ".git", "config.worktree"), "[core]\n\tbare = false\n");
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "config_refused" });
  });

  test("a forged index (stat cache plus oid patched back to HEAD) is never reported clean", async (t) => {
    const f = await fixture(t);
    await f.write("tracked.txt", "ORIGINAL\n");
    f.g("update-index", "tracked.txt");
    await new Promise((r) => setTimeout(r, 1200));
    const idx = await f.indexBytes();
    const headOid = Buffer.from(f.g("rev-parse", "HEAD:tracked.txt"), "hex");
    const needle = Buffer.concat([Buffer.from("tracked.txt"), Buffer.from([0])]);
    const pathPos = idx.indexOf(needle);
    assert.ok(pathPos > 22, "entry located");
    headOid.copy(idx, pathPos - 2 - 20);
    const body = idx.subarray(0, idx.length - 20);
    const forged = Buffer.concat([body, createHash("sha1").update(body).digest()]);
    await fs.writeFile(path.join(f.root, ".git", "index"), forged);
    assert.equal(f.g("status", "--porcelain"), "", "the forgery hides the edit from status");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.notEqual(result.kind, "clean", JSON.stringify(result));
    assert.deepEqual(await f.stray(), []);
  });

  test("a clone outside the runner root never reaches the transport", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const elsewhere = new GitCache(path.join(f.dir, "elsewhere"), nullLogger());
    const result = await elsewhere.captureTerminalSnapshot(f.root, { runId: "run-1", generation: 2, deadlineMs: 60_000, barePath: f.bare });
    assert.deepEqual(result, { kind: "refused", reason: "clone_outside_runner_root" });
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  const foreignStore = async (f: Awaited<ReturnType<typeof fixture>>) => {
    const foreign = path.join(f.dir, "foreign");
    await fs.mkdir(foreign);
    run(foreign, "init", "-q", "--bare");
    return foreign;
  };

  test("a clone --shared runner clone (alternates naming the bare's objects) captures", async (t) => {
    const f = await fixture(t);
    const shared = path.join(f.dir, "runner", "repo", "shared");
    execFileSync("/usr/bin/git", ["clone", "-q", "--shared", f.bare, shared], { env: gitEnv });
    assert.equal((await fs.readFile(path.join(shared, ".git", "objects", "info", "alternates"), "utf8")).trim().length > 0, true);
    await fs.writeFile(path.join(shared, "new.txt"), "shared work\n");
    const result = await f.git.captureTerminalSnapshot(shared, { runId: "run-1", generation: 2, deadlineMs: 60_000, barePath: f.bare });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind === "captured") assert.equal(run(f.bare, "show", `${result.root}:new.txt`), "shared work");
  });

  test("alternates naming a foreign store are refused before any phase or ref", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const foreign = await foreignStore(f);
    await fs.writeFile(path.join(f.root, ".git", "objects", "info", "alternates"), `${path.join(foreign, "objects")}\n`);
    const result = await f.capture({ onPhase: async (p) => { f.calls.push(p); } });
    assert.deepEqual(result, { kind: "refused", reason: "alternates_present" });
    assert.deepEqual(f.calls, []);
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  test("a planted commondir is refused as a clone shape", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    await fs.writeFile(path.join(f.root, ".git", "commondir"), "../../elsewhere\n");
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "clone_shape" });
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  test("a symlinked objects directory is refused, so a relative alternates line cannot resolve elsewhere", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const objects = path.join(f.root, ".git", "objects");
    const moved = path.join(path.dirname(f.root), "moved-objects", "deep", "objects");
    await fs.mkdir(path.dirname(moved), { recursive: true });
    await fs.rename(objects, moved);
    await fs.symlink(moved, objects);
    const result = await f.capture({ onPhase: async (p) => { f.calls.push(p); } });
    assert.deepEqual(result, { kind: "refused", reason: "alternates_present" });
    assert.deepEqual(f.calls, []);
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  test("alternates with an extra line or an http-alternates entry are refused", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const foreign = await foreignStore(f);
    const info = path.join(f.root, ".git", "objects", "info");
    await fs.writeFile(path.join(info, "alternates"), `${path.join(f.bare, "objects")}\n${path.join(foreign, "objects")}\n`);
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "alternates_present" });
    await fs.rm(path.join(info, "alternates"));
    await fs.writeFile(path.join(info, "http-alternates"), "https://example.invalid/objects\n");
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "alternates_present" });
  });

  test("a HEAD that is not reachable from the bare's runner ref is refused before any fetch or ref", async (t) => {
    const f = await fixture(t);
    run(f.bare, "update-ref", "-d", "refs/uzi-runner/main");
    await f.write("new.txt", "x");
    const result = await f.capture({ onPhase: async (p) => { f.calls.push(p); } });
    assert.deepEqual(result, { kind: "refused", reason: "parent_not_in_bare" });
    assert.deepEqual(f.calls, []);
    assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false);
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
    assert.equal(f.refsOf(f.bare).includes("uzi-incoming"), false);
  });

  test("a forged HEAD carrying a foreign blob through a symlinked loose object is refused and nothing foreign lands", async (t) => {
    const f = await fixture(t);
    const foreign = await foreignStore(f);
    const blob = execFileSync("/usr/bin/git", ["-C", foreign, "hash-object", "-w", "--stdin"], { env: gitEnv, input: "foreign secret\n", encoding: "utf8" }).trim();
    const loose = path.join(foreign, "objects", blob.slice(0, 2), blob.slice(2));
    await fs.mkdir(path.join(f.root, ".git", "objects", blob.slice(0, 2)), { recursive: true });
    await fs.symlink(loose, path.join(f.root, ".git", "objects", blob.slice(0, 2), blob.slice(2)));
    const baseTree = f.g("rev-parse", "HEAD^{tree}");
    const tree = execFileSync("/usr/bin/git", ["-C", f.root, "mktree"], {
      env: gitEnv, encoding: "utf8", input: f.g("ls-tree", baseTree) + `\n100644 blob ${blob}\tevil.txt\n`,
    }).trim();
    const forged = f.g("commit-tree", tree, "-p", f.head, "-m", "forged");
    f.g("update-ref", "refs/heads/main", forged);
    f.g("reset", "-q", "--hard", forged);
    await f.write("new.txt", "x");
    const result = await f.capture();
    assert.deepEqual(result, { kind: "refused", reason: "parent_not_in_bare" });
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false, "the final ref is absent");
    assert.equal(f.refsOf(f.bare).includes("uzi-incoming"), false);
    assert.throws(() => run(f.bare, "cat-file", "-e", blob), "the foreign blob never reached the bare");
  });

  test("a clone ref swapped to a commit with an extra blob lands nothing", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const result = await f.capture({ seam: { afterCloneRef: async () => {
      await fs.writeFile(path.join(f.root, "extra.txt"), "extra\n");
      f.g("add", "extra.txt");
      const extraTree = f.g("write-tree");
      f.g("reset", "-q", "extra.txt");
      await fs.rm(path.join(f.root, "extra.txt"));
      run(f.root, "update-ref", f.ref, f.g("commit-tree", extraTree, "-p", f.head, "-m", "swapped"));
    } } });
    assert.equal(result.kind, "refused", JSON.stringify(result));
    assert.match((result as { reason: string }).reason, /^(foreign_object|bare_transfer_mismatch)$/);
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false, "the final ref is absent");
    assert.equal(f.refsOf(f.bare).includes("uzi-incoming"), false);
  });

  test("an empty alternates file is harmless", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    await fs.writeFile(path.join(f.root, ".git", "objects", "info", "alternates"), "");
    assert.equal((await f.capture()).kind, "captured");
  });

  test("alternates planted after the clone ref exists are refused and no bare ref lands", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const foreign = await foreignStore(f);
    const result = await f.capture({ seam: { afterCloneRef: async () => {
      await fs.writeFile(path.join(f.root, ".git", "objects", "info", "alternates"), `${path.join(foreign, "objects")}\n`);
    } } });
    assert.deepEqual(result, { kind: "refused", reason: "alternates_present" });
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
    assert.equal(f.refsOf(f.bare).includes("uzi-incoming"), false);
  });

  test("a clone ref swapped after validation lands nothing in the final bare ref and leaves no staging ref", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "x");
    const result = await f.capture({ seam: { afterCloneRef: async () => { run(f.root, "update-ref", f.ref, f.head); } } });
    assert.deepEqual(result, { kind: "refused", reason: "bare_transfer_mismatch" });
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false, "the final ref was never created");
    assert.equal(f.refsOf(f.bare).includes("uzi-incoming"), false, "only our own staging ref was dropped");
  });

  test("record-first: capturing precedes the clone ref, captured follows the transfer", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const seen: Array<{ phase: string; cloneRefExists: boolean; bareRefExists: boolean; commit?: string }> = [];
    const result = await f.capture({
      onPhase: async (phase, info) => {
        assert.equal(info.cloneRef, f.ref);
        assert.equal(info.bareRef, f.ref);
        seen.push({ phase, cloneRefExists: f.refsOf(f.root).includes(f.ref), bareRefExists: f.refsOf(f.bare).includes(f.ref), commit: info.commit });
      },
    });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind !== "captured") return;
    assert.deepEqual(seen.map((s) => [s.phase, s.cloneRefExists, s.bareRefExists]), [["capturing", false, false], ["captured", false, true]]);
    assert.equal(seen[0]!.commit, result.root);
    assert.equal(run(f.bare, "rev-parse", f.ref), result.root);
    assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false, "the clone ref is deleted after the transfer");
    assert.equal(f.refsOf(f.bare).includes("uzi-incoming"), false, "the staging ref is deleted");
  });

  test("a failure after the clone ref exists is refused and leaves the refs as they are", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const result = await f.capture({ seam: { afterCloneRef: async () => { throw new Error("injected"); } } });
    assert.equal(result.kind, "refused");
    assert.match(f.refsOf(f.root), /refs\/uzi-terminal-local\/run-1\/g2 [0-9a-f]{40}/, "the clone ref stays");
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false, "the bare ref is untouched");
    assert.deepEqual(await f.stray(), []);
    // A retry of the same capture resumes from its own clone ref and lands the bare ref.
    const retry = await f.capture();
    assert.equal(retry.kind, "captured", JSON.stringify(retry));
    assert.match(f.refsOf(f.bare), /refs\/uzi-terminal-local\/run-1\/g2 [0-9a-f]{40}/);
  });

  // Holds the bare's lock (a real buildTerminalAggregate parked in its afterLink seam) until `release` is called.
  async function heldBareLock(f: Awaited<ReturnType<typeof fixture>>) {
    const empty = run(f.bare, "mktree");
    const env = ["-c", "user.name=a", "-c", "user.email=a@b.c"];
    const base = run(f.bare, "rev-parse", "HEAD");
    const root = run(f.bare, ...env, "commit-tree", empty, "-m", "holder root");
    let started!: () => void;
    const holdStarted = new Promise<void>((res) => { started = res; });
    let release!: () => void;
    const released = new Promise<void>((res) => { release = res; });
    let isReleased = false;
    const holder = f.git.buildTerminalAggregate(f.bare, {
      base, extraRoots: [root], runId: "run-holder", generation: 1,
      seam: { afterLink: async () => { started(); await released; } },
    });
    await holdStarted;
    // A later lock-serialized operation: once it settles, every earlier waiter on the bare lock has run or forfeited.
    const laterOp = async () => {
      const later = run(f.bare, ...env, "commit-tree", empty, "-m", "later root");
      return f.git.buildTerminalAggregate(f.bare, { base, extraRoots: [later], runId: "run-later", generation: 1 });
    };
    return { holder, release: () => { isReleased = true; release(); }, isReleased: () => isReleased, laterOp };
  }

  test("a capture whose deadline lapses while queued behind the bare lock is refused while the lock is still held and mutates nothing later", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const lock = await heldBareLock(f);
    let queued = 0;
    let heldWhenQueued = false;
    // Time is frozen from here: the only way the deadline lapses is the tick issued from the lockQueued seam.
    t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: Date.now() });
    let result;
    try {
      result = await f.capture({ deadlineMs: 5000, seam: { lockQueued: () => {
        queued++;
        heldWhenQueued = !lock.isReleased();
        t.mock.timers.tick(5001);
      } } });
    } finally { t.mock.timers.reset(); }
    assert.equal(queued, 1, "the capture reached the queued lock wait");
    assert.ok(heldWhenQueued);
    assert.deepEqual(result, { kind: "refused", reason: "deadline" });
    assert.equal(lock.isReleased(), false, "refused while the lock was still held");
    const cloneRefLine = f.refsOf(f.root).split("\n").find((l) => l.startsWith(f.ref));
    const commit = cloneRefLine?.split(" ")[1];
    assert.match(commit ?? "", /^[0-9a-f]{40}$/, "record-first: the clone ref stays");
    lock.release();
    assert.ok("tip" in await lock.holder);
    assert.ok("tip" in await lock.laterOp());
    assert.equal(run(f.bare, "for-each-ref", f.ref), "", "no final ref lands after the lock is released");
    assert.equal(run(f.bare, "for-each-ref", "refs/uzi-incoming/"), "", "no staging ref");
    assert.throws(() => run(f.bare, "cat-file", "-e", commit!), "the snapshot commit never reached the bare");
  });

  test("a capture aborted by its signal while queued behind the bare lock is refused aborted while the lock is still held and mutates nothing later", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const lock = await heldBareLock(f);
    const ac = new AbortController();
    let queued = 0;
    const result = await f.capture({ signal: ac.signal, seam: { lockQueued: () => { queued++; ac.abort(); } } });
    assert.equal(queued, 1, "the capture reached the queued lock wait");
    assert.deepEqual(result, { kind: "refused", reason: "aborted" });
    assert.equal(lock.isReleased(), false, "refused while the lock was still held");
    lock.release();
    assert.ok("tip" in await lock.holder);
    assert.ok("tip" in await lock.laterOp());
    assert.equal(run(f.bare, "for-each-ref", f.ref), "");
    assert.equal(run(f.bare, "for-each-ref", "refs/uzi-incoming/"), "");
  });

  test("a failing capturing callback refuses before any clone ref", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const result = await f.capture({ onPhase: async () => { throw new Error("record failed"); } });
    assert.deepEqual(result, { kind: "refused", reason: "phase_callback_failed" });
    assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false);
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  test("a bare ref already holding other work is a conflict, not an overwrite", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    run(f.bare, "update-ref", f.ref, run(f.bare, "rev-parse", "HEAD"));
    const result = await f.capture();
    assert.equal(result.kind, "refused");
    assert.equal(run(f.bare, "rev-parse", f.ref), run(f.bare, "rev-parse", "HEAD"));
  });

  test("a failing re-proof refuses before any ref", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const result = await f.capture({ reprove: async () => false, onPhase: async (p) => { f.calls.push(p); } });
    assert.deepEqual(result, { kind: "refused", reason: "reprove_failed" });
    assert.deepEqual(f.calls, []);
  });

  // Characterization (G8): inside an authenticated capture scope GitCache's captureSynthesisEnvironment only
  // admits the checkpoint synthesis index shape, so the terminal private index would be refused there. This
  // reaches the private helper directly because the scope itself needs a full boundary spawner.
  test("characterization: the capture-scope environment admits uzi-ckpt-idx- indexes but not uzi-term-idx-", async (t) => {
    const f = await fixture(t);
    const environment = (f.git as unknown as { captureSynthesisEnvironment(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv }).captureSynthesisEnvironment.bind(f.git);
    const uuid = "123e4567-e89b-42d3-a456-426614174000";
    assert.doesNotThrow(() => environment({ GIT_INDEX_FILE: path.join(os.tmpdir(), `uzi-ckpt-idx-${uuid}`) }));
    assert.throws(() => environment({ GIT_INDEX_FILE: path.join(os.tmpdir(), `uzi-term-idx-${uuid}`) }), /capture synthesis path refused/);
  });
});

describe("buildTerminalAggregate", () => {
  async function roots(f: Awaited<ReturnType<typeof fixture>>, n: number) {
    const empty = run(f.bare, "mktree");
    const env = ["-c", "user.name=a", "-c", "user.email=a@b.c"];
    return Array.from({ length: n }, (_, i) => run(f.bare, ...env, "commit-tree", empty, "-m", `root ${i}`));
  }

  test("chains two extra roots as ours-merges under the base tree and pins the tip", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const [r1, r2] = await roots(f, 2);
    run(f.bare, "update-ref", f.ref, base);
    const out = await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: [r1!, r2!], runId: "run-1", generation: 2 });
    assert.ok("tip" in out, JSON.stringify(out));
    if (!("tip" in out)) return;
    assert.equal(run(f.bare, "rev-parse", f.ref), out.tip);
    assert.equal(run(f.bare, "rev-parse", `${out.tip}^{tree}`), run(f.bare, "rev-parse", `${base}^{tree}`));
    const parents = run(f.bare, "rev-list", "--parents", "-n", "1", out.tip).split(" ");
    assert.equal(parents.length, 3);
    assert.equal(parents[2], r2);
    for (const root of [base, r1!, r2!]) run(f.bare, "merge-base", "--is-ancestor", root, out.tip);
    assert.equal(run(f.bare, "rev-list", "--merges", "--count", `${out.tip}`, `^${base}`), "2");
    // Rebuilding is idempotent.
    const again = await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: [r1!, r2!], runId: "run-1", generation: 2 });
    assert.deepEqual(again, { tip: out.tip });
  });

  test("no extra roots returns the base and writes nothing", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: [], runId: "run-1", generation: 2 }), { tip: base });
    assert.equal(f.refsOf(f.bare).includes("uzi-terminal-local"), false);
  });

  test("the merge budget refuses before any commit", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const many = await roots(f, 33);
    const before = f.refsOf(f.bare);
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: many, runId: "run-1", generation: 2 }), { refused: "merge_budget" });
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: many.slice(0, 2), existingMerges: 31, runId: "run-1", generation: 2 }), { refused: "merge_budget" });
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: many.slice(0, 2), existingMerges: 30, runId: "run-1", generation: 2 }).then((r) => "tip" in r), true);
    assert.notEqual(f.refsOf(f.bare), before);
  });

  for (const [label, match] of [
    ["the root kind probe", (a: string[]) => a.includes("cat-file")],
    ["the base tree read, after the call's own budget lapsed", (a: string[]) => a.includes("rev-parse")],
  ] as const) {
    test(`an unsafe cleanup from ${label} rejects instead of returning a refusal`, async (t) => {
      const f = await fixture(t);
      const base = run(f.bare, "rev-parse", "HEAD");
      const [r1] = await roots(f, 1);
      const spy = f.git as unknown as { runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
      const original = spy.runGit.bind(f.git);
      const ac = new AbortController();
      t.mock.method(spy, "runGit", async (cwd: string, args: string[], ...rest: unknown[]) => {
        if (match(args)) { ac.abort(); throw new CheckpointUnsafeCleanupError("cancelled without verified disposal"); }
        return original(cwd, args, ...rest);
      });
      await assert.rejects(f.git.buildTerminalAggregate(f.bare, {
        base, extraRoots: [r1!], runId: "run-1", generation: 2, budget: { signal: ac.signal, deadline: Date.now() + 60_000 } }),
        (e) => e instanceof CheckpointUnsafeCleanupError);
    });
  }

  test("a root missing from the bare is refused", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: ["1".repeat(40)], runId: "run-1", generation: 2 }), { refused: "root_missing" });
  });

  // Link commits carry "uzi terminal aggregate <run> g<gen> link <i>"; list every commit in the bare (reachable or not) that does.
  function linkCommits(bare: string, runId: string): string[] {
    const shas = run(bare, "cat-file", "--batch-all-objects", "--batch-check=%(objectname) %(objecttype)")
      .split("\n").filter((l) => l.endsWith(" commit")).map((l) => l.split(" ")[0]!);
    return shas.filter((sha) => run(bare, "cat-file", "-p", sha).includes(`uzi terminal aggregate ${runId} `));
  }
  const settle = () => new Promise<void>((res) => setTimeout(res, 150));

  test("a budgeted call aborted while queued behind the bare lock never runs: refused deadline, no link, no ref", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const [r1, r2] = await roots(f, 2);
    let release!: () => void;
    const held = new Promise<void>((res) => { release = res; });
    let holding!: () => void;
    const holdStarted = new Promise<void>((res) => { holding = res; });
    // The first call owns the lock until afterLink(0) resolves.
    const first = f.git.buildTerminalAggregate(f.bare, {
      base, extraRoots: [r1!, r2!], runId: "run-a", generation: 1,
      seam: { afterLink: async () => { holding(); await held; } },
    });
    await holdStarted;
    const ac = new AbortController();
    const second = f.git.buildTerminalAggregate(f.bare, {
      base, extraRoots: [r1!, r2!], runId: "run-b", generation: 3,
      budget: { signal: ac.signal, deadline: Date.now() + 60_000 },
    });
    await settle();
    ac.abort();
    assert.deepEqual(await second, { refused: "deadline" });
    release();
    assert.ok("tip" in await first);
    await settle();
    assert.equal(run(f.bare, "for-each-ref", "refs/uzi-terminal-local/run-b/g3"), "", "no ref for the aborted waiter");
    assert.deepEqual(linkCommits(f.bare, "run-b"), [], "the aborted waiter wrote no link commit, even after the lock released");
  });

  test("a budget abort between links refuses with deadline and pins no ref", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const [r1, r2] = await roots(f, 2);
    const ac = new AbortController();
    const out = await f.git.buildTerminalAggregate(f.bare, {
      base, extraRoots: [r1!, r2!], runId: "run-1", generation: 2,
      budget: { signal: ac.signal, deadline: Date.now() + 60_000 },
      seam: { afterLink: async (i) => { if (i === 0) ac.abort(); } },
    });
    assert.deepEqual(out, { refused: "deadline" });
    assert.equal(run(f.bare, "for-each-ref", f.ref), "");
    assert.equal(linkCommits(f.bare, "run-1").length, 1, "only link 0 was built before the abort, never link 1");
  });

  test("a budget that lapses before the pin refuses with deadline", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const [r1] = await roots(f, 1);
    const out = await f.git.buildTerminalAggregate(f.bare, {
      base, extraRoots: [r1!], runId: "run-1", generation: 2,
      budget: { signal: new AbortController().signal, deadline: Date.now() + 80 },
      seam: { afterLink: async () => { await new Promise<void>((res) => setTimeout(res, 200)); } },
    });
    assert.deepEqual(out, { refused: "deadline" });
    assert.equal(run(f.bare, "for-each-ref", f.ref), "");
  });

  test("a cooperative budget with time to spare builds and pins the same tip as no budget", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const [r1, r2] = await roots(f, 2);
    const plain = await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: [r1!, r2!], runId: "run-1", generation: 2 });
    run(f.bare, "update-ref", "-d", f.ref);
    const out = await f.git.buildTerminalAggregate(f.bare, {
      base, extraRoots: [r1!, r2!], runId: "run-1", generation: 2,
      budget: { signal: new AbortController().signal, deadline: Date.now() + 60_000 },
    });
    assert.deepEqual(out, plain);
    assert.ok("tip" in out);
    if ("tip" in out) assert.equal(run(f.bare, "rev-parse", f.ref), out.tip);
  });

  test("a pin that holds other work is a conflict", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    const [r1, r2] = await roots(f, 2);
    run(f.bare, "update-ref", f.ref, r2!);
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: [r1!], runId: "run-1", generation: 2 }), { refused: "ref_conflict" });
  });
});

describe("terminal digests and per-commit CI paths", () => {
  async function history(t: test.TestContext) {
    const f = await fixture(t);
    const a = f.g("rev-parse", "HEAD");
    await f.write(".github/workflows/ci.yml", "name: ci\n");
    f.g("add", "-A"); f.g("commit", "-q", "-m", "adds workflow");
    const b = f.g("rev-parse", "HEAD");
    f.g("rm", "-q", ".github/workflows/ci.yml");
    await f.write("other.txt", "other\n");
    f.g("add", "-A"); f.g("commit", "-q", "-m", "reverts workflow");
    const c = f.g("rev-parse", "HEAD");
    const bare = path.join(f.dir, "range.git");
    execFileSync("/usr/bin/git", ["clone", "-q", "--bare", f.root, bare], { env: gitEnv });
    const range: CheckpointRange = { tipSha: c, excludeSha: a };
    return { f, a, b, c, bare, range };
  }

  test("scan digest is deterministic, hex, and sensitive to the range", async (t) => {
    const { f, a, b, c, bare, range } = await history(t);
    const first = await f.git.terminalScanDigest(bare, range, { deadlineMs: 30_000 });
    const second = await f.git.terminalScanDigest(bare, range, { deadlineMs: 30_000 });
    assert.match(first ?? "", /^[0-9a-f]{64}$/);
    assert.equal(first, second);
    const narrower = await f.git.terminalScanDigest(bare, { tipSha: c, excludeSha: b }, { deadlineMs: 30_000 });
    assert.match(narrower ?? "", /^[0-9a-f]{64}$/);
    assert.notEqual(narrower, first);
    const floored = await f.git.terminalScanDigest(bare, { ...range, scanFloorShas: [b] }, { deadlineMs: 30_000 });
    assert.equal(floored, narrower, "a scan floor excludes the same objects as a narrower exclude");
    assert.equal(await f.git.terminalScanDigest(bare, { tipSha: "z".repeat(40), excludeSha: a }, { deadlineMs: 30_000 }), null);
    assert.equal(await f.git.terminalScanDigest(path.join(f.dir, "missing.git"), range, { deadlineMs: 30_000 }), null);
  });

  test("transport digest is equal for packs of the same objects and differs otherwise", async (t) => {
    const { f, a, b, c, bare } = await history(t);
    const pack = (args: string[], rev: string) => execFileSync("/usr/bin/git", ["-C", bare, ...args, "pack-objects", "--revs", "--stdout"],
      { env: gitEnv, input: rev, maxBuffer: 64 * MIB });
    const one = pack([], `${c}\n^${a}\n`);
    const two = pack(["-c", "pack.compression=0"], `${c}\n^${a}\n`);
    assert.notDeepEqual(one, two, "different bytes, same objects");
    const d1 = await f.git.terminalTransportDigest(one, { deadlineMs: 30_000 });
    const d2 = await f.git.terminalTransportDigest(two, { deadlineMs: 30_000 });
    assert.match(d1 ?? "", /^[0-9a-f]{64}$/);
    assert.equal(d1, d2);
    const other = await f.git.terminalTransportDigest(pack([], `${b}\n^${a}\n`), { deadlineMs: 30_000 });
    assert.notEqual(other, d1);
    assert.equal(await f.git.terminalTransportDigest(Buffer.from("not a pack"), { deadlineMs: 30_000 }), null);
    // No repository is touched and nothing is left behind.
    assert.deepEqual((await fs.readdir(os.tmpdir())).filter((n) => n.startsWith("uzi-term-pack-")), []);
  });

  test("per-commit CI paths report an intermediate commit that a later one reverts", async (t) => {
    const { f, a, b, c, bare, range } = await history(t);
    const rows = await f.git.changedCiPathsPerCommit(bare, range, a, { deadlineMs: 30_000 });
    assert.ok(rows);
    const byCommit = new Map(rows!.map((r) => [r.commit, r.paths]));
    assert.deepEqual([...byCommit.keys()].sort(), [b, c].sort());
    assert.deepEqual(byCommit.get(b), [".github/workflows/ci.yml"]);
    assert.deepEqual(byCommit.get(c), ["other.txt"]);
    assert.equal(await f.git.changedCiPathsPerCommit(bare, range, "nothex", { deadlineMs: 30_000 }), null);
    assert.equal(await f.git.changedCiPathsPerCommit(path.join(f.dir, "missing.git"), range, a, { deadlineMs: 30_000 }), null);
  });

  test("per-commit CI paths enforce a cumulative budget and stop early on a flagged commit", async (t) => {
    const f = await fixture(t);
    const base = f.g("rev-parse", "HEAD");
    for (let i = 0; i < 5; i++) {
      for (let j = 0; j < 3; j++) await f.write(`d${i}/f${j}.txt`, "x\n");
      f.g("add", "-A"); f.g("commit", "-q", "-m", `c${i}`);
    }
    const tip = f.g("rev-parse", "HEAD");
    const bare = path.join(f.dir, "many.git");
    execFileSync("/usr/bin/git", ["clone", "-q", "--bare", f.root, bare], { env: gitEnv });
    const range: CheckpointRange = { tipSha: tip, excludeSha: base };
    // Each commit differs from base by 3, 6, ... 15 paths: 45 retained in total.
    const all = await f.git.changedCiPathsPerCommit(bare, range, base, { deadlineMs: 30_000 });
    assert.equal(all?.reduce((n, r) => n + r.paths.length, 0), 45);
    assert.equal(await f.git.changedCiPathsPerCommit(bare, range, base, { deadlineMs: 30_000, caps: { totalPaths: 44 } }), null);
    assert.equal((await f.git.changedCiPathsPerCommit(bare, range, base, { deadlineMs: 30_000, caps: { totalPaths: 45 } }))?.length, 5);
    assert.equal(await f.git.changedCiPathsPerCommit(bare, range, base, { deadlineMs: 30_000, caps: { totalBytes: 100 } }), null);
    // A flagged commit ends the walk and is returned alone, even when the budget would have been exceeded later.
    const flagged = await f.git.changedCiPathsPerCommit(bare, range, base, {
      deadlineMs: 30_000, caps: { totalPaths: 1 }, flag: (paths) => paths.length > 0,
    });
    assert.deepEqual(flagged?.map((r) => r.commit), [tip]);
    assert.equal(flagged?.[0]?.paths.length, 15);
  });

  test("digest helpers agree with the pack the same range produces", async (t) => {
    const { f, a, c, bare, range } = await history(t);
    const pack = execFileSync("/usr/bin/git", ["-C", bare, "pack-objects", "--revs", "--stdout"], { env: gitEnv, input: `${c}\n^${a}\n`, maxBuffer: 64 * MIB });
    const transport = await f.git.terminalTransportDigest(pack, { deadlineMs: 30_000 });
    const scan = await f.git.terminalScanDigest(bare, range, { deadlineMs: 30_000 });
    assert.match(scan ?? "", /^[0-9a-f]{64}$/);
    assert.match(transport ?? "", /^[0-9a-f]{64}$/);
    assert.equal(scan, transport);
  });
});

describe("captureTerminalSnapshot: index-independent verdict, bounded cleanup, unsafe propagation", () => {
  type Fx = Awaited<ReturnType<typeof fixture>>;
  const looseObjects = async (dir: string): Promise<string[]> => {
    const out: string[] = [];
    const objects = path.join(dir, ".git", "objects");
    for (const d of await fs.readdir(objects)) {
      if (!/^[0-9a-f]{2}$/.test(d)) continue;
      for (const n of await fs.readdir(path.join(objects, d))) out.push(d + n);
    }
    return out.sort();
  };
  const absent = (cwd: string, oid: string) => assert.throws(() => run(cwd, "cat-file", "-e", oid), `${oid} must not exist in ${cwd}`);

  /** Commits `rel` (and publishes the commit to the bare's runner ref, the production shape). */
  async function track(f: Fx, rel: string, content: Buffer | string) {
    await f.write(rel, content);
    f.g("add", rel); f.g("commit", "-q", "-m", `tracks ${rel}`);
    const head = f.g("rev-parse", "HEAD");
    run(f.bare, "fetch", "-q", f.root, `+${head}:refs/uzi-runner/main`);
  }

  /**
   * Edits tracked `rel` (same byte length as HEAD's) and forges the real index so `git status` reads clean, WITHOUT leaving
   * the edited blob in the clone's object store: `update-index` records the stat data (and writes the blob), then the entry's
   * oid is patched back to HEAD's and the seeded loose object is removed. Returns the edited content's blob oid.
   */
  async function forgeHidden(f: Fx, rel: string, edited: Buffer | string): Promise<string> {
    await f.write(rel, edited);
    const past = new Date(Date.now() - 60_000);
    await fs.utimes(path.join(f.root, rel), past, past);
    f.g("update-index", rel);
    const oid = f.g("hash-object", rel);
    await fs.rm(path.join(f.root, ".git", "objects", oid.slice(0, 2), oid.slice(2)));
    const idx = await f.indexBytes();
    const needle = Buffer.concat([Buffer.from(rel), Buffer.from([0])]);
    const pathPos = idx.indexOf(needle);
    assert.ok(pathPos > 22, "entry located");
    Buffer.from(f.g("rev-parse", `HEAD:${rel}`), "hex").copy(idx, pathPos - 2 - 20);
    const body = idx.subarray(0, idx.length - 20);
    await fs.writeFile(path.join(f.root, ".git", "index"), Buffer.concat([body, createHash("sha1").update(body).digest()]));
    assert.equal(f.g("status", "--porcelain"), "", "the forgery hides the edit from status");
    absent(f.root, oid);
    return oid;
  }

  test("a forged index hiding an edited tracked auth.json refuses without writing the hidden content", async (t) => {
    const f = await fixture(t);
    await track(f, "auth.json", "{\"t\":\"AAAAAAAA\"}\n");
    const oid = await forgeHidden(f, "auth.json", "{\"t\":\"BBBBBBBB\"}\n");
    const before = await f.state();
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "index_mismatch" });
    assert.deepEqual(f.calls, []);
    absent(f.root, oid);
    absent(f.bare, oid);
    assert.deepEqual(await f.state(), before);
    assert.deepEqual(await f.stray(), []);
    assert.equal(f.refsOf(f.root).includes("uzi-terminal-local"), false);
  });

  test("a forged index hiding an over-limit edit of a tracked file refuses without writing the oversized content", async (t) => {
    const f = await fixture(t);
    await track(f, "big.bin", Buffer.alloc(8 * MIB + 1, 0x61));
    const oid = await forgeHidden(f, "big.bin", Buffer.alloc(8 * MIB + 1, 0x62));
    const result = await f.capture();
    assert.deepEqual(result, { kind: "refused", reason: "index_mismatch" });
    absent(f.root, oid);
    absent(f.bare, oid);
    assert.deepEqual(await f.stray(), []);
  });

  test("a forged index plus a visible new file still refuses and writes nothing hidden", async (t) => {
    const f = await fixture(t);
    await track(f, "auth.json", "{\"t\":\"AAAAAAAA\"}\n");
    const oid = await forgeHidden(f, "auth.json", "{\"t\":\"BBBBBBBB\"}\n");
    await f.write("new.txt", "visible\n");
    assert.deepEqual(await f.capture(), { kind: "refused", reason: "index_mismatch" });
    absent(f.root, oid);
  });

  test("positive control: a clean repo is clean and writes no object", async (t) => {
    const f = await fixture(t);
    await f.write("ignored.log", "abc");
    const objects = await looseObjects(f.root);
    const result = await f.capture();
    assert.equal(result.kind, "clean");
    assert.equal("remnant" in result, false);
    assert.deepEqual(await looseObjects(f.root), objects);
    assert.deepEqual(await f.stray(), []);
  });

  const dirty: Array<[string, (f: Fx) => Promise<void>, string]> = [
    ["an edited tracked file", (f) => f.write("tracked.txt", "edited\n"), "tracked.txt"],
    ["a deleted tracked file", (f) => fs.rm(path.join(f.root, "gone.txt")), "gone.txt"],
    ["a new untracked file", (f) => f.write("brand/new.txt", "new\n"), "brand/new.txt"],
  ];
  for (const [name, mutate, changed] of dirty) {
    test(`positive control: ${name} is still captured`, async (t) => {
      const f = await fixture(t);
      await mutate(f);
      const result = await f.capture();
      assert.equal(result.kind, "captured", JSON.stringify(result));
      if (result.kind !== "captured") return;
      assert.equal("remnant" in result, false);
      assert.equal(run(f.bare, "diff-tree", "-r", "--name-only", result.parent, result.root), changed);
      assert.deepEqual(await f.stray(), []);
    });
  }

  // ---- bounded cleanup ----
  const stagingRefs = (f: Fx) => run(f.bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/");
  const cloneCommit = (f: Fx) => f.refsOf(f.root).split("\n").find((l) => l.startsWith(f.ref))!.split(" ")[1]!;
  /** Every live process whose argv names `update-ref` and this bare repository. */
  async function updateRefChildren(bare: string): Promise<number[]> {
    const pids: number[] = [];
    for (const name of await fs.readdir("/proc")) {
      if (!/^\d+$/.test(name)) continue;
      const argv = (await fs.readFile(`/proc/${name}/cmdline`, "utf8").catch(() => "")).split("\0");
      if (argv.includes("update-ref") && argv.some((a) => a.includes(bare))) pids.push(Number(name));
    }
    return pids;
  }
  const stageRefRe = /^refs\/uzi-incoming\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

  test("a stalled staging-ref delete (real child) is cancelled at the cleanup allowance: bounded, no leftover child, remnant unknown, ref unchanged later", async (t) => {
    if (process.platform !== "linux") { t.skip("inspects the process table"); return; }
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    // Deleting a loose ref takes packed-refs.lock; wait for it indefinitely so the delete is a genuine stalled child.
    await fs.appendFile(path.join(f.bare, "config"), "[core]\n\tpackedRefsTimeout = -1\n");
    const lockFile = path.join(f.bare, "packed-refs.lock");
    const ac = new AbortController();
    let abortedAt = 0;
    let sawChild = false;
    const sampler = setInterval(() => { void updateRefChildren(f.bare).then((p) => { if (p.length > 0) sawChild = true; }); }, 25);
    let result;
    try {
      result = await f.capture({ signal: ac.signal, caps: { cleanupMs: 700 }, seam: { inTransfer: async () => {
        await fs.writeFile(lockFile, "");
        abortedAt = Date.now();
        ac.abort();
      } } });
    } finally { clearInterval(sampler); }
    const returnedAt = Date.now();
    assert.ok(abortedAt > 0, "the transfer seam ran");
    assert.ok(returnedAt - abortedAt < 700 + 2500, `returned within the allowance plus slack (${returnedAt - abortedAt}ms)`);
    assert.ok(sawChild, "the delete really ran as a child that stalled on the lock");
    assert.deepEqual(await updateRefChildren(f.bare), [], "no leftover update-ref child");
    assert.equal(result.kind, "refused", JSON.stringify(result));
    if (result.kind !== "refused") return;
    assert.equal(result.reason, "aborted");
    const staged = stagingRefs(f);
    assert.match(staged, /^refs\/uzi-incoming\/\S+ [0-9a-f]{40}$/, "the staging ref is still there");
    const [stageName, stageOid] = staged.split(" ") as [string, string];
    assert.equal(stageOid, cloneCommit(f));
    const index = result.remnant?.index;
    assert.match(index ?? "", /uzi-term-idx-/, "the shared allowance was spent, so the private index removal did not run and is reported");
    assert.deepEqual(result.remnant, { ref: stageName, expectedOid: stageOid, state: "unknown", index });
    assert.match(stageName, stageRefRe);
    // Release the lock and let every lock-serialized operation drain: nothing may touch the ref afterwards.
    await fs.rm(lockFile);
    const root = run(f.bare, "-c", "user.name=a", "-c", "user.email=a@b.c", "commit-tree", run(f.bare, "mktree"), "-m", "later");
    assert.ok("tip" in await f.git.buildTerminalAggregate(f.bare, { base: run(f.bare, "rev-parse", "HEAD"), extraRoots: [root], runId: "run-later", generation: 1 }));
    assert.equal(stagingRefs(f), staged, "ref state is exactly what was observed at return");
    assert.deepEqual(await updateRefChildren(f.bare), []);
    await fs.rm(index!, { force: true });
  });

  test("a signal-only cleanupBound (no deadline) leaves the full cleanup allowance: no staging ref, no private index", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const result = await f.capture({ cleanupBound: { signal: new AbortController().signal } });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    assert.equal("remnant" in result, false, JSON.stringify(result));
    assert.equal(stagingRefs(f), "");
    assert.deepEqual(await f.stray(), []);
  });

  test("retained: allowance exhausted after the confirming read reports the staging ref at the commit and launches no delete", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    // The seam itself spends the allowance (a cancelled caller bound), so no real time is involved.
    const ac = new AbortController();
    const result = await f.capture({ cleanupBound: { signal: ac.signal }, seam: { afterStagingRead: async () => { ac.abort(); } } });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind !== "captured") return;
    const [stageName, stageOid] = stagingRefs(f).split(" ") as [string, string];
    assert.equal(stageOid, result.root);
    const index = result.remnant?.index;
    assert.match(index ?? "", /uzi-term-idx-/, "the spent allowance also skipped the private index removal");
    assert.deepEqual(result.remnant, { ref: stageName, oid: result.root, state: "retained", index });
    assert.match(stageName, stageRefRe);
    await fs.rm(index!, { force: true });
  });

  test("unknown: a confirming read that observes a different oid reports observed and expected, not retained", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const other = "b".repeat(40);
    const original = spies(f).runGit.bind(f.git);
    let stageReads = 0;
    t.mock.method(spies(f), "runGit", async (cwd: string, args: string[], ...rest: unknown[]) => {
      // The first read of the staging ref is the transfer's own check; the second is the cleanup's confirming read.
      if (args[0] === "for-each-ref" && String(args[args.length - 1]).startsWith("refs/uzi-incoming/") && ++stageReads === 2) return `${other}\n`;
      return original(cwd, args, ...rest);
    });
    const ac = new AbortController();
    const result = await f.capture({ cleanupBound: { signal: ac.signal }, seam: { afterStagingRead: async () => { ac.abort(); } } });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind !== "captured") return;
    const [stageName] = stagingRefs(f).split(" ") as [string];
    assert.deepEqual(result.remnant, { ref: stageName, oid: other, expectedOid: result.root, state: "unknown", index: result.remnant?.index });
    if (result.remnant?.index) await fs.rm(result.remnant.index, { force: true });
  });

  test("a failed private-index .lock removal alone is not reported as an index remnant", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const result = await f.capture({ seam: { indexLockCleanupError: new Error("lock rm failed") } });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    assert.equal("remnant" in result, false, JSON.stringify(result));
    assert.deepEqual(await f.stray(), [], "the index itself was removed");
  });

  test("unknown: allowance exhausted before the confirming read could run", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const ac = new AbortController();
    const result = await f.capture({ signal: ac.signal, cleanupBound: { deadlineAt: Date.now() - 1 }, seam: { inTransfer: async () => { ac.abort(); } } });
    assert.equal(result.kind, "refused", JSON.stringify(result));
    if (result.kind !== "refused") return;
    const [stageName, stageOid] = stagingRefs(f).split(" ") as [string, string];
    assert.deepEqual(result.remnant, { ref: stageName, expectedOid: stageOid, state: "unknown", index: result.remnant?.index });
    assert.match(result.remnant?.index ?? "", /uzi-term-idx-/);
    await fs.rm(result.remnant!.index!, { force: true });
  });

  test("a fetch interrupted before it ran leaves no staging ref and no remnant", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const ac = new AbortController();
    const result = await f.capture({ signal: ac.signal, seam: { beforeFetch: async () => { ac.abort(); } } });
    assert.deepEqual(result, { kind: "refused", reason: "aborted" });
    assert.equal(stagingRefs(f), "");
    assert.deepEqual(await f.stray(), []);
  });

  test("cleanup is not bound to the capture's own signal: an aborted capture still deletes its staging ref", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const ac = new AbortController();
    const result = await f.capture({ signal: ac.signal, seam: { inTransfer: async () => { ac.abort(); } } });
    assert.deepEqual(result, { kind: "refused", reason: "aborted" });
    assert.equal(stagingRefs(f), "");
    assert.deepEqual(await f.stray(), []);
  });

  // ---- unsafe-cleanup propagation ----
  const unsafe = () => new CheckpointUnsafeCleanupError("cancelled without verified disposal");
  const isUnsafe = (e: unknown) => e instanceof CheckpointUnsafeCleanupError;
  type Spy = { runGitAsRunner: (cwd: string | undefined, args: string[], opts?: { timeoutMs?: number }) => Promise<string>;
    runGit: (cwd: string, args: string[], ...rest: unknown[]) => Promise<string> };
  const spies = (f: Fx) => f.git as unknown as Spy;

  test("an unsafe cleanup from the private-index removal propagates from the public method", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    await assert.rejects(f.capture({ seam: { indexCleanupError: unsafe() } }), isUnsafe);
  });

  test("an unsafe cleanup from the enumeration status call is not downgraded to git_failed", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const original = spies(f).runGitAsRunner.bind(f.git);
    t.mock.method(spies(f), "runGitAsRunner", async (cwd: string | undefined, args: string[], opts?: { timeoutMs?: number }) => {
      if (args.includes("status")) throw unsafe();
      return original(cwd, args, opts);
    });
    await assert.rejects(f.capture(), isUnsafe);
    assert.deepEqual(await f.stray(), []);
  });

  test("an unsafe cleanup from the staged blob-size read is not downgraded to staged_unsized", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const original = spies(f).runGitAsRunner.bind(f.git);
    t.mock.method(spies(f), "runGitAsRunner", async (cwd: string | undefined, args: string[], opts?: { timeoutMs?: number }) => {
      if (args.some((a) => a.startsWith("--batch-check"))) throw unsafe();
      return original(cwd, args, opts);
    });
    await assert.rejects(f.capture(), isUnsafe);
    assert.deepEqual(await f.stray(), []);
  });

  test("an unsafe cleanup from a bare child propagates even after the capture's signal aborted", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const ac = new AbortController();
    const original = spies(f).runGit.bind(f.git);
    t.mock.method(spies(f), "runGit", async (cwd: string, args: string[], ...rest: unknown[]) => {
      if (args.includes("fetch")) { ac.abort(); throw unsafe(); }
      return original(cwd, args, ...rest);
    });
    await assert.rejects(f.capture({ signal: ac.signal }), isUnsafe);
    assert.deepEqual(await f.stray(), []);
    assert.equal(stagingRefs(f), "");
  });

  test("an unsafe cleanup from the staging-ref delete propagates and the private index is still removed", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const original = spies(f).runGit.bind(f.git);
    t.mock.method(spies(f), "runGit", async (cwd: string, args: string[], ...rest: unknown[]) => {
      if (args[0] === "update-ref" && args[1] === "-d") throw unsafe();
      return original(cwd, args, ...rest);
    });
    await assert.rejects(f.capture(), isUnsafe);
    assert.deepEqual(await f.stray(), [], "the remaining cleanup step still ran");
  });

  test("the staged blob-size read is bounded by the remaining budget", async (t) => {
    const f = await fixture(t);
    await f.write("new.txt", "brand new\n");
    const original = spies(f).runGitAsRunner.bind(f.git);
    const seen: Array<number | undefined> = [];
    t.mock.method(spies(f), "runGitAsRunner", async (cwd: string | undefined, args: string[], opts?: { timeoutMs?: number }) => {
      if (args.some((a) => a.startsWith("--batch-check"))) seen.push(opts?.timeoutMs);
      return original(cwd, args, opts);
    });
    const result = await f.capture({ deadlineMs: 45_000 });
    assert.equal(result.kind, "captured", JSON.stringify(result));
    assert.equal(seen.length, 1);
    assert.ok(typeof seen[0] === "number" && Number.isFinite(seen[0]) && seen[0] > 0 && seen[0] <= 45_000, `timeoutMs ${seen[0]}`);
  });

  // ---- global excludes ----
  async function withHome(t: test.TestContext, setup: (home: string) => Promise<void>) {
    const home = await fs.mkdtemp(path.join(os.tmpdir(), "term-home-"));
    const previous = process.env.HOME;
    process.env.HOME = home;
    t.after(async () => {
      if (previous === undefined) delete process.env.HOME; else process.env.HOME = previous;
      await fs.rm(home, { recursive: true, force: true });
    });
    await setup(home);
  }

  test("a hostile global excludes file never hides new work: still captured", async (t) => {
    const f = await fixture(t);
    await withHome(t, async (home) => {
      await fs.mkdir(path.join(home, ".config", "git"), { recursive: true });
      await fs.writeFile(path.join(home, ".config", "git", "ignore"), "*\n");
    });
    await f.write("new.txt", "brand new\n");
    const result = await f.capture();
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind === "captured") assert.equal(run(f.bare, "show", `${result.root}:new.txt`), "brand new");
  });

  test("an unreadable global excludes path never changes the verdict", async (t) => {
    const f = await fixture(t);
    await withHome(t, async (home) => {
      // A directory where the file belongs: git cannot read it as an excludes file.
      await fs.mkdir(path.join(home, ".config", "git", "ignore"), { recursive: true });
    });
    await f.write("new.txt", "brand new\n");
    const result = await f.capture();
    assert.equal(result.kind, "captured", JSON.stringify(result));
    if (result.kind === "captured") assert.equal(run(f.bare, "show", `${result.root}:new.txt`), "brand new");
  });
});

describe("terminal index hash script", () => {
  const hashOf = (target: string) => execFileSync(process.execPath, ["-e", TERMINAL_INDEX_HASH_SCRIPT, target],
    { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });

  test("matches the sha256 of a real git index", async (t) => {
    const f = await fixture(t);
    const index = path.join(f.root, ".git", "index");
    assert.equal(hashOf(index), createHash("sha256").update(await fs.readFile(index)).digest("hex"));
  });

  test("a missing path reads as absent", async (t) => {
    const f = await fixture(t);
    assert.equal(hashOf(path.join(f.dir, "no-such-index")), "absent");
  });

  test("an unreadable path (a directory) reads as error", async (t) => {
    const f = await fixture(t);
    assert.equal(hashOf(f.dir), "error");
  });

  test("the script uses a read stream, not readFileSync", () => {
    assert.ok(!TERMINAL_INDEX_HASH_SCRIPT.includes("readFileSync"));
    assert.ok(TERMINAL_INDEX_HASH_SCRIPT.includes("createReadStream"));
  });
});

describe("unsafe cleanup identity through runner git", () => {
  const unsafe = () => new CheckpointUnsafeCleanupError("x");
  const causeChain = (err: unknown): unknown[] => {
    const chain: unknown[] = [];
    for (let e: unknown = err, i = 0; e !== undefined && i < 8; i++) {
      chain.push(e);
      e = (e as { cause?: unknown } | null)?.cause;
    }
    return chain;
  };

  test("(a) a live boundary spawn rejection keeps the typed identity", async () => {
    const f = await credentialFreeGitFixture();
    try {
      const injected = unsafe();
      const got = await f.git.withBoundaryProcessSpawner(async () => { throw injected; }, new AbortController().signal,
        () => f.git.worktreeHead(f.clone)).then(() => undefined, (e: unknown) => e);
      assert.equal(got, injected);
      assert.equal((got as CheckpointUnsafeCleanupError).checkpointBoundary, "unsafe_cleanup");
    } finally { await f.cleanup(); }
  });

  test("(b) a capture-scope disposal failure surfaces the typed unsafe cleanup as the boundary error's cause", async () => {
    const f = await credentialFreeGitFixture();
    try {
      let current!: BoundaryProcessRequest;
      const adapter = boundaryProcessSpawnerForTest("required", async () => {
        const request = current;
        const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
        const exit = new Promise<{ event: "child_exit"; code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ event: "child_exit", code: code ?? 1 }));
        });
        return {
          started: { event: "started", supervisorPid: 300, childPid: 301, subreaper: true,
            nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
          supervisorPid: 300,
          transport: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr },
          snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
          observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
          waitChild: async () => exit,
          dispose: async () => { await exit; throw new Error("disposal failed"); },
          failed: undefined, whenFailed: new Promise<Error>(() => undefined),
        } satisfies CodexRootHandle;
      });
      const safety = createCodexExecutionSafety(f.registry, async () => { throw new Error("unexpected generic spawn"); },
        undefined, undefined, async (request, deadline, cleanup) => { current = request; return adapter(request, deadline, cleanup); });
      const got = await safety.withCredentialFreeCapture(30_000,
        context => f.git.withCredentialFreeCapture(context, () => f.git.worktreeHead(f.clone))).then(() => undefined, (e: unknown) => e);
      const chain = causeChain(got);
      const shape = chain.map(e => `${(e as Error)?.constructor?.name}:${(e as Error)?.message}`).join(" <- ");
      assert.ok(got instanceof Error, shape);
      assert.ok(!(got instanceof CheckpointUnsafeCleanupError), `top level is not itself unsafe-cleanup typed: ${shape}`);
      assert.notEqual((got as { checkpointBoundary?: string }).checkpointBoundary, "unsafe_cleanup", shape);
      assert.ok(chain[1] instanceof CheckpointUnsafeCleanupError, `direct cause is the typed unsafe cleanup: ${shape}`);
    } finally { await f.cleanup(); }
  });

  test("(c1) an already aborted boundary rejects as an abort without spawning", async () => {
    const f = await credentialFreeGitFixture();
    try {
      let spawns = 0;
      const ac = new AbortController();
      ac.abort();
      const got = await f.git.withBoundaryProcessSpawner(async () => { spawns++; throw unsafe(); }, ac.signal,
        () => f.git.worktreeHead(f.clone)).then(() => undefined, (e: unknown) => e);
      assert.ok(got instanceof GitBoundaryAbortError, String(got));
      assert.equal(spawns, 0);
    } finally { await f.cleanup(); }
  });

  test("(c2) an abort during a pending spawn wraps the typed error as the cause", async () => {
    const f = await credentialFreeGitFixture();
    try {
      const injected = unsafe();
      const ac = new AbortController();
      const got = await f.git.withBoundaryProcessSpawner(async () => { ac.abort(); throw injected; }, ac.signal,
        () => f.git.worktreeHead(f.clone)).then(() => undefined, (e: unknown) => e);
      assert.ok(got instanceof GitBoundaryAbortError, String(got));
      assert.ok(causeChain(got).includes(injected), "typed error reachable through .cause");
    } finally { await f.cleanup(); }
  });
});
