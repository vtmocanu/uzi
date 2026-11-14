// Terminal-preservation capture primitives of GitCache (production-dark): captureTerminalSnapshot,
// buildTerminalAggregate, terminalScanDigest, terminalTransportDigest and changedCiPathsPerCommit,
// exercised against real git fixtures (a real clone and a real bare).
import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, type CheckpointRange, type TerminalSnapshotOptions } from "../src/git.js";
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
  const root = path.join(dir, "clone");
  await fs.mkdir(root);
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

  for (const name of [".netrc", "auth.json", ".credentials.json", ".claude.json", ".git-credentials", "id_rsa", "id_ed25519", "cert.pem", "deploy.key", "sub/.netrc"]) {
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

  test("a filter attribute on a changed path is refused before staging", async (t) => {
    const f = await fixture(t);
    await f.write(".gitattributes", "*.dat filter=lfsx\n");
    await f.write("a.dat", "payload\n");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.deepEqual(result, { kind: "refused", reason: "filter_attribute" });
    assert.deepEqual(f.calls, []);
  });

  test("a nested repository is refused", async (t) => {
    const f = await fixture(t);
    const nested = path.join(f.root, "sub");
    await fs.mkdir(nested);
    run(nested, "init", "-q");
    await fs.writeFile(path.join(nested, "x"), "x");
    run(nested, "-c", "user.name=a", "-c", "user.email=a@b.c", "add", "x");
    run(nested, "-c", "user.name=a", "-c", "user.email=a@b.c", "commit", "-q", "-m", "n");
    const result = await f.capture({ seam: { beforeStaging: async () => { f.calls.push("staging"); } } });
    assert.equal(result.kind, "refused");
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
    assert.equal(result.kind, "refused");
    assert.deepEqual(f.calls, []);
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

  test("a root missing from the bare is refused", async (t) => {
    const f = await fixture(t);
    const base = run(f.bare, "rev-parse", "HEAD");
    assert.deepEqual(await f.git.buildTerminalAggregate(f.bare, { base, extraRoots: ["1".repeat(40)], runId: "run-1", generation: 2 }), { refused: "root_missing" });
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

  test("digest helpers agree with the pack the same range produces", async (t) => {
    const { f, a, c, bare, range } = await history(t);
    const pack = execFileSync("/usr/bin/git", ["-C", bare, "pack-objects", "--revs", "--stdout"], { env: gitEnv, input: `${c}\n^${a}\n`, maxBuffer: 64 * MIB });
    const transport = await f.git.terminalTransportDigest(pack, { deadlineMs: 30_000 });
    const scan = await f.git.terminalScanDigest(bare, range, { deadlineMs: 30_000 });
    assert.equal(scan, transport);
  });
});
