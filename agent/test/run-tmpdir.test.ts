// Issue #4420: the run-owned TMPDIR leaf. The single-uid path runs for real against a temp-root
// fixture; the split path runs the real runner-side helper script as the current user through an
// identity command wrapper (the same approach as pod-epoch-scratch-census.test.ts).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test, type TestContext } from "node:test";
import { RUN_SCRATCH_LEAF_RE, scratchLayout, type ScratchIo, type ScratchLayout } from "../src/pod-epoch-scratch.js";
import { RUN_TMPDIR_SCRIPT, runAgentHelper, runTmpdirHelper, type CommandWrapper } from "../src/rmtree.js";
import { prepareRunTmpdir, type RunTmpdirTestSeams } from "../src/run-tmpdir.js";
import { buildSdkEnv } from "../src/sdk-env.js";

const UID = process.getuid!();
const ID_A = "123e4567-e89b-42d3-a456-426614174000";
const ID_B = "223e4567-e89b-42d3-a456-426614174001";
const MKTEMP = ["mk", "temp"].join("");
const identity: CommandWrapper = (command, args) => ({ command, args: [...args] });
const modeOf = (p: string) => fs.lstatSync(p).mode & 0o7777;
const idOf = (p: string) => { const st = fs.statSync(p, { bigint: true }); return `${st.dev}:${st.ino}`; };

function fixtureRoot(t: TestContext, mode = 0o755): string {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runtmp-"));
  fs.chmodSync(root, mode);
  t.after(() => { fs.rmSync(root, { recursive: true, force: true }); });
  return root;
}
const single = (root: string, over: Partial<ScratchLayout> = {}): ScratchLayout =>
  ({ mode: "single", root, ownerUid: UID, modes: [0o755, 0o3775], ...over });
const split = (root: string, over: Partial<ScratchLayout> = {}): ScratchLayout =>
  ({ mode: "split", root, ownerUid: UID, modes: [0o700], ...over });
const helper: NonNullable<RunTmpdirTestSeams["helper"]> = (r, e, l, u, ms) => runTmpdirHelper(r, e, l, u, ms, identity);

test("grouping: the Claude-lane env sends a real mktemp -d into <root>/run-<id>/, not the root", async t => {
  const root = fixtureRoot(t);
  const prepared = await prepareRunTmpdir(ID_A, { layout: single(root) });
  assert.deepEqual(prepared, { dir: path.join(root, `run-${ID_A}`) });
  const env = buildSdkEnv({ kind: "oauth", token: "tok" }, root, {}, undefined, undefined, undefined, (prepared as { dir: string }).dir);
  const made = execFileSync(MKTEMP, ["-d"], { env: { PATH: process.env.PATH, TMPDIR: env.TMPDIR } }).toString().trim();
  assert.equal(path.dirname(made), path.join(root, `run-${ID_A}`));
  assert.equal(path.dirname(path.dirname(made)), root);
  assert.equal(modeOf(path.join(root, `run-${ID_A}`)), 0o700);
});

test("concurrent runs get distinct leaves", async t => {
  const root = fixtureRoot(t);
  const [a, b] = await Promise.all([prepareRunTmpdir(ID_A, { layout: single(root) }), prepareRunTmpdir(ID_B, { layout: single(root) })]);
  assert.deepEqual([a, b], [{ dir: path.join(root, `run-${ID_A}`) }, { dir: path.join(root, `run-${ID_B}`) }]);
  assert.deepEqual(fs.readdirSync(root).sort(), [`run-${ID_A}`, `run-${ID_B}`]);
});

test("a layout the gate refuses means no grouping and no directory", async t => {
  const root = fixtureRoot(t);
  assert.deepEqual(await prepareRunTmpdir(ID_A, { env: { TMPDIR: root } }), { skipped: true, reason: "layout_refused" });
  assert.deepEqual(await prepareRunTmpdir(ID_A, { env: {} }), { skipped: true, reason: "layout_refused" });
  assert.equal(scratchLayout({ TMPDIR: root }) && "refused" in scratchLayout({ TMPDIR: root }), true);
  assert.deepEqual(fs.readdirSync(root), []);
});

test("the production layouts are the two shipped roots", async () => {
  const s = scratchLayout({ UZI_UID_SPLIT: "1", UZI_RUNNER_TMPDIR: "/data/runner/.tmp/uzi-runner", TMPDIR: "/data/runner/.tmp/uzi-worker" });
  assert.ok(!("refused" in s) && s.mode === "split" && s.root === "/data/runner/.tmp/uzi-runner");
  const o = scratchLayout({ TMPDIR: "/data/runner/.tmp" });
  assert.ok(!("refused" in o) && o.mode === "single" && o.root === "/data/runner/.tmp");
});

test("a run id that is not a lowercase uuid is refused, never repaired", async t => {
  const root = fixtureRoot(t);
  for (const id of [ID_A.toUpperCase(), "not-a-uuid", `${ID_A}/..`, `${ID_A}x`, "", "../x"]) {
    assert.deepEqual(await prepareRunTmpdir(id, { layout: single(root) }), { skipped: true, reason: "run_id_invalid" }, id);
  }
  assert.deepEqual(fs.readdirSync(root), []);
  assert.ok(RUN_SCRATCH_LEAF_RE.test(`run-${ID_A}`) && !RUN_SCRATCH_LEAF_RE.test(`run-${ID_A}\n`));
});

test("root refusals: wrong owner, wrong mode, symlinked ancestor, missing root", async t => {
  const root = fixtureRoot(t);
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root, { ownerUid: UID + 1 }) }), { skipped: true, reason: "root_identity" });
  fs.chmodSync(root, 0o777);
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root) }), { skipped: true, reason: "root_identity" });
  fs.chmodSync(root, 0o755);
  const link = `${root}-link`;
  fs.symlinkSync(root, link);
  t.after(() => { fs.rmSync(link, { force: true }); });
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(link) }), { skipped: true, reason: "root_unpinned" });
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(path.join(root, "absent")) }), { skipped: true, reason: "root_unpinned" });
  assert.deepEqual(fs.readdirSync(root), []);
});

test("a fresh leaf under a setgid 03775 root ends exactly 0700", async t => {
  const root = fixtureRoot(t, 0o3775);
  if ((modeOf(root) & 0o7777) !== 0o3775) { t.skip("this filesystem will not hold 03775"); return; }
  const prepared = await prepareRunTmpdir(ID_A, { layout: single(root) });
  assert.ok("dir" in prepared);
  assert.equal(modeOf(path.join(root, `run-${ID_A}`)), 0o700);
});

test("an existing exact-0700 leaf is reused and never chmodded", async t => {
  const root = fixtureRoot(t);
  const leaf = path.join(root, `run-${ID_A}`);
  fs.mkdirSync(leaf, { mode: 0o700 });
  fs.writeFileSync(path.join(leaf, "keep"), "x");
  const chmods: string[] = [];
  const io = { ...fs, fchmodSync: (...a: Parameters<typeof fs.fchmodSync>) => { chmods.push(String(a[1])); return fs.fchmodSync(...a); } } as unknown as ScratchIo;
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root), io }), { dir: leaf });
  assert.deepEqual(chmods, []);
  assert.equal(fs.readFileSync(path.join(leaf, "keep"), "utf8"), "x");
});

test("an existing leaf with a different mode, a symlink, or a foreign owner falls back untouched", async t => {
  const root = fixtureRoot(t);
  const leaf = path.join(root, `run-${ID_A}`);
  for (const mode of [0o2700, 0o755, 0o750, 0o1700]) {
    fs.mkdirSync(leaf);
    fs.chmodSync(leaf, mode);
    const want = modeOf(leaf);
    assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root) }), { skipped: true, reason: "leaf_refused" }, mode.toString(8));
    assert.equal(modeOf(leaf), want, "never repaired");
    fs.rmSync(leaf, { recursive: true });
  }
  const elsewhere = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runtmp-target-"));
  t.after(() => { fs.rmSync(elsewhere, { recursive: true, force: true }); });
  fs.chmodSync(elsewhere, 0o700);
  fs.symlinkSync(elsewhere, leaf);
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root) }), { skipped: true, reason: "leaf_refused" });
  assert.equal(modeOf(elsewhere), 0o700);
  fs.rmSync(leaf);
  fs.mkdirSync(leaf, { mode: 0o700 });
  // Foreign owner: the leaf's descriptor stat reports another uid (this process cannot chown).
  const opened = new Set<number>();
  const io = {
    ...fs,
    openSync: (p: string, ...rest: unknown[]) => {
      const fd = (fs.openSync as (...a: unknown[]) => number)(p, ...rest);
      if (p.endsWith(`/run-${ID_A}`)) opened.add(fd);
      return fd;
    },
    fstatSync: (fd: number, o: { bigint: true }) => {
      const st = fs.fstatSync(fd, o);
      return opened.has(fd) ? { isDirectory: () => st.isDirectory(), uid: st.uid + 1n, mode: st.mode } : st;
    },
  } as unknown as ScratchIo;
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root), io }), { skipped: true, reason: "leaf_refused" });
});

test("a root that cannot hold the leaf (mkdir fails) skips, never throws", async t => {
  const root = fixtureRoot(t);
  const io = { ...fs, mkdirSync: () => { throw Object.assign(new Error("x"), { code: "EACCES" }); } } as unknown as ScratchIo;
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: single(root), io }), { skipped: true, reason: "leaf_create_failed" });
});

test("split mode: the runner helper creates the leaf, then reuses it", async t => {
  const root = fixtureRoot(t, 0o700);
  const leaf = path.join(root, `run-${ID_A}`);
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: split(root), helper }), { dir: leaf });
  assert.equal(modeOf(leaf), 0o700);
  fs.writeFileSync(path.join(leaf, "keep"), "x");
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: split(root), helper }), { dir: leaf });
  assert.equal(fs.readFileSync(path.join(leaf, "keep"), "utf8"), "x");
});

test("split mode: the worker accepts only exit 0 with the exact receipt", async t => {
  const root = fixtureRoot(t, 0o700);
  const run = (code: number, stdout: string): NonNullable<RunTmpdirTestSeams["helper"]> => async () => ({ code, stdout });
  const dev = idOf(root);
  const cases: Array<[number, string, string]> = [
    [0, "", "helper_receipt"], [0, `UZI-RUN-TMP created ${dev}`, "helper_receipt"], [0, `UZI-RUN-TMP maybe ${dev}\n`, "helper_receipt"],
    [0, `UZI-RUN-TMP created ${dev}\nextra\n`, "helper_receipt"], [3, "", "root_identity"], [10, "", "helper_failed"], [1, `UZI-RUN-TMP created ${dev}\n`, "helper_failed"],
  ];
  for (const [code, stdout, reason] of cases) {
    assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: split(root), helper: run(code, stdout) }), { skipped: true, reason }, `${code} ${stdout}`);
  }
  const boom: NonNullable<RunTmpdirTestSeams["helper"]> = async () => { throw new Error("spawn"); };
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: split(root), helper: boom }), { skipped: true, reason: "helper_failed" });
});

test("split mode: a root identity mismatch inside the helper exits 3 and creates nothing", async t => {
  const root = fixtureRoot(t, 0o700);
  const wrong: NonNullable<RunTmpdirTestSeams["helper"]> = (r, _e, l, u, ms) => runTmpdirHelper(r, "1:1", l, u, ms, identity);
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: split(root), helper: wrong }), { skipped: true, reason: "root_identity" });
  assert.deepEqual(fs.readdirSync(root), []);
});

test("split mode: a root swapped while the helper ran fails the re-pin", async t => {
  const root = fixtureRoot(t, 0o700);
  const swapping: NonNullable<RunTmpdirTestSeams["helper"]> = async (r, e, l, u, ms) => {
    const result = await helper(r, e, l, u, ms);
    fs.renameSync(root, `${root}-moved`);
    fs.mkdirSync(root, { mode: 0o700 });
    t.after(() => { fs.rmSync(`${root}-moved`, { recursive: true, force: true }); });
    return result;
  };
  assert.deepEqual(await prepareRunTmpdir(ID_A, { layout: split(root), helper: swapping }), { skipped: true, reason: "root_repin" });
});

async function script(root: string, leaf: string, over: { expect?: string; uid?: number } = {}) {
  return runAgentHelper(identity, RUN_TMPDIR_SCRIPT, [root, over.expect ?? idOf(root), leaf, String(over.uid ?? UID)], 60_000);
}

test("the helper script: receipts, exit codes and no repair", async t => {
  const root = fixtureRoot(t, 0o700);
  const name = `run-${ID_A}`;
  const made = await script(root, name);
  assert.equal(made.code, 0);
  assert.match(made.stdout, /^UZI-RUN-TMP created \d+:\d+\n$/);
  assert.equal(made.stdout, `UZI-RUN-TMP created ${idOf(path.join(root, name))}\n`);
  assert.equal((await script(root, name)).stdout, `UZI-RUN-TMP reused ${idOf(path.join(root, name))}\n`);
  assert.equal((await script(root, name, { uid: UID + 1 })).code, 10, "foreign owner");
  fs.chmodSync(path.join(root, name), 0o755);
  const loose = await script(root, name);
  assert.deepEqual([loose.code, loose.stdout], [11, ""], "an existing wide leaf is refused");
  assert.equal(modeOf(path.join(root, name)), 0o755, "and left as found");
  const other = `run-${ID_B}`;
  fs.symlinkSync(root, path.join(root, other));
  assert.equal((await script(root, other)).code, 9, "a symlink leaf");
  for (const bad of ["run-X", `run-${ID_A.toUpperCase()}`, "../x", "a/b", ""]) assert.equal((await script(root, bad)).code, 6, bad);
  assert.equal((await script("relative", name, { expect: "1:1" })).code, 6);
  assert.equal((await script(path.join(root, "absent"), name, { expect: "1:1" })).code, 2);
  assert.equal((await script(root, name, { expect: "1:1" })).code, 3);
  const link = `${root}-link`;
  fs.symlinkSync(root, link);
  t.after(() => { fs.rmSync(link, { force: true }); });
  assert.equal((await script(link, name)).code, 3, "a symlinked root");
});

test("the helper script creates a fresh leaf 0700 under a setgid root", async t => {
  const root = fixtureRoot(t, 0o3775);
  if (modeOf(root) !== 0o3775) { t.skip("this filesystem will not hold 03775"); return; }
  assert.equal((await script(root, `run-${ID_A}`)).code, 0);
  assert.equal(modeOf(path.join(root, `run-${ID_A}`)), 0o700);
});
