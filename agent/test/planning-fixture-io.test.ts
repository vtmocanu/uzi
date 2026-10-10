import { it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { PlanningFixtureIO, planningEnv, planningExec, planningGitEnv } from "./planning-fixture-io.js";
import { RUNNER_UID, runnerPath, runnerTmpdir, uidSplitActive } from "../src/runner-uid.js";
import os from "node:os";

// Linux filenames are raw bytes, so cover an invalid UTF-8 name there; macOS (APFS)
// rejects it with EILSEQ, so other hosts use a valid non-ASCII UTF-8 name.
const name = process.platform === "linux" ? Buffer.from([0xff]) : Buffer.from("\u00e9", "utf8");

for (const ipc of [false, true]) {
  // Native is only the single-UID path; forced IPC always runs, including on single UID.
  it(`planning fixture IO preserves filesystem values (IPC=${ipc})`, {
    skip: !ipc && uidSplitActive() ? "native IO is single UID only" : false,
  }, async t => {
    const io = new PlanningFixtureIO(ipc);
    let root: string | undefined;
    t.after(async () => {
      try { if (root) await io.fs.rm(root, { recursive: true, force: true }); }
      finally { await io.close(); }
    });
    root = await io.temporaryDirectory("planning-io-");
    const raw = Buffer.concat([Buffer.from(root + "/"), name]);
    const bytes = Buffer.from([0xff, 0, 0x80, 10]);
    await io.fs.writeFile(raw, bytes);
    assert.deepEqual(await io.fs.readFile(raw), bytes);
    assert.deepEqual(await io.fs.readdir(root, { encoding: "buffer" }), [name]);
    const stat = await io.fs.stat(raw, { bigint: true });
    assert.equal(stat.size, 4n);
    assert.equal(typeof stat.ino, "bigint");
    assert.equal(typeof stat.mtimeNs, "bigint");
    assert.ok(stat.mtime instanceof Date);
    assert.equal(stat.uid, BigInt(uidSplitActive() ? RUNNER_UID : process.getuid!()));
    assert.equal(stat.isFile(), true);
    assert.equal(stat.isDirectory(), false);
    assert.equal(stat.isSymbolicLink(), false);
    assert.equal(stat.isFIFO(), false);
    assert.equal(stat.isSocket(), false);
    assert.equal(stat.isBlockDevice(), false);
    assert.equal(stat.isCharacterDevice(), false);
    await io.fs.mkdir(path.join(root, "private"), { mode: 0o700 });
    await io.fs.symlink(name, path.join(root, "link"));
    assert.equal((await io.fs.lstat(path.join(root, "link"))).isSymbolicLink(), true);
    if (process.platform !== "win32") {
      await planningExec("mkfifo", [path.join(root, "fifo")], { timeout: 5000 });
      assert.equal((await io.fs.stat(path.join(root, "fifo"))).isFIFO(), true);
    }
    const entries = await io.fs.readdir(root, { withFileTypes: true, encoding: "buffer" });
    assert.equal(entries.find(e => e.name.equals(Buffer.from("private")))!.isDirectory(), true);
    assert.equal(entries.find(e => e.name.equals(Buffer.from("link")))!.isSymbolicLink(), true);
    assert.equal(entries.find(e => e.name.equals(name))!.isFile(), true);
    assert.equal((await io.fs.stat(root)).mode & 0o777, 0o700);
    assert.equal((await io.fs.stat(path.join(root, "private"))).mode & 0o777, 0o700);
    const missing = path.join(root, "missing");
    await assert.rejects(io.fs.stat(missing), error => {
      const e = error as NodeJS.ErrnoException;
      assert.equal(e.code, "ENOENT");
      assert.equal(e.syscall, "stat");
      assert.equal(e.path, missing);
      assert.equal(typeof e.errno, "number");
      assert.match(e.message, /ENOENT/);
      return true;
    });
    await assert.rejects(io.fs.stat(Buffer.concat([raw, Buffer.from("/child")])), { code: "ENOTDIR" });
    await assert.rejects(io.fs.mkdir(path.join(root, "private")), { code: "EEXIST" });
  });
}

for (const operation of ["exit", "disconnect"] as const) {
  it(`planning fixture IPC rejects pending siblings on child ${operation}`, async t => {
    const io = new PlanningFixtureIO(true);
    t.after(async () => { await io.close(); });
    const first = await io.holdForTest();
    const second = await io.holdForTest();
    const rejected = Promise.all([
      assert.rejects(first.pending, /planning fixture/),
      assert.rejects(second.pending, /planning fixture/),
    ]);
    await io.disruptForTest(operation);
    await rejected;
    await assert.rejects(io.fs.stat("/"), /planning fixture/);
    await io.close();
  });
}

it("planning fixture failed setup removes partial state and awaits IPC teardown", async () => {
  const io = new PlanningFixtureIO(true);
  let root = "";
  try {
    await assert.rejects(io.temporaryDirectory("planning-failed-", async directory => {
      root = directory;
      await io.fs.mkdir(path.join(directory, "partial"), { mode: 0o700 });
      await io.fs.writeFile(path.join(directory, "partial", "data"), "partial");
      throw new Error("seed failed");
    }), /seed failed/);
    assert.ok(root);
    await assert.rejects(io.fs.stat(root), { code: "ENOENT" });
  } finally { await io.close(); }
  await assert.rejects(io.fs.stat(root), /closed|disconnected/);
});

it("planning fixture commands use runner identity and deterministic credential-free git identity", async t => {
  const io = new PlanningFixtureIO(true);
  let root: string | undefined;
  t.after(async () => {
    try { if (root) await io.fs.rm(root, { recursive: true, force: true }); }
    finally { await io.close(); }
  });
  root = await io.temporaryDirectory("planning-git-");
  assert.deepEqual(planningEnv(), { PATH: runnerPath(), TMPDIR: runnerTmpdir() ?? os.tmpdir() });
  const env = planningGitEnv();
  assert.equal(env.GIT_AUTHOR_NAME, "Fixture");
  assert.equal(env.GIT_COMMITTER_EMAIL, "fixture@example.invalid");
  const command = await planningExec(process.execPath, ["-e", "process.stdout.write(String(process.getuid()))"], { timeout: 5000 });
  assert.equal(command.stdout.trim(), String(uidSplitActive() ? RUNNER_UID : process.getuid!()));
  await planningExec("git", ["-C", root, "init", "--quiet", "--template="], { timeout: 5000 });
  await planningExec("git", ["-C", root, "commit", "--allow-empty", "-qm", "fixture"], { timeout: 5000 });
  const identity = await planningExec("git", ["-C", root, "show", "-s", "--format=%an <%ae>%n%cn <%ce>"], { timeout: 5000 });
  assert.equal(identity.stdout, "Fixture <fixture@example.invalid>\nFixture <fixture@example.invalid>\n");
  const hashing = planningExec("git", ["-C", root, "hash-object", "--stdin"], { timeout: 30000, encoding: "buffer" });
  hashing.child.stdin?.end("loose\n");
  assert.ok(Buffer.isBuffer((await hashing).stdout));
});
