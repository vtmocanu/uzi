// Issue #3153: ensureRunnerTmpdir (worker boot check of a TMPDIR under <dataDir>/runner).
import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { ensureRunnerTmpdir, RunnerTmpdirError } from "../src/runner-tmpdir.js";

const me = process.getuid?.() ?? 0;

describe("ensureRunnerTmpdir (#3153)", () => {
  let base: string;
  let dataDir: string;
  let runnerRoot: string;

  beforeEach(() => {
    const scratch = process.env.TMPDIR && path.isAbsolute(process.env.TMPDIR) ? process.env.TMPDIR : os.tmpdir();
    base = fs.mkdtempSync(path.join(scratch, "uzi-runner-tmpdir-"));
    dataDir = path.join(base, "data");
    runnerRoot = path.join(dataDir, "runner");
    fs.mkdirSync(runnerRoot, { recursive: true });
  });
  afterEach(() => {
    fs.rmSync(base, { recursive: true, force: true });
  });

  it("is a no-op when TMPDIR is unset", () => {
    assert.deepEqual(ensureRunnerTmpdir({ dataDir, env: {} }), { checked: false, created: false });
  });

  it("is a no-op when TMPDIR is outside the runner root (even a symlink there)", () => {
    const outside = path.join(base, "outside");
    fs.symlinkSync(base, outside);
    assert.equal(ensureRunnerTmpdir({ dataDir, env: { TMPDIR: outside } }).checked, false);
    // a sibling that merely shares the prefix is outside too
    assert.equal(ensureRunnerTmpdir({ dataDir, env: { TMPDIR: `${runnerRoot}-other` } }).checked, false);
  });

  it("creates an absent TMPDIR below the runner root and accepts it", () => {
    const tmp = path.join(runnerRoot, ".tmp");
    const r = ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } });
    assert.deepEqual(r, { checked: true, created: true, path: tmp });
    assert.ok(fs.lstatSync(tmp).isDirectory());
    assert.equal(fs.statSync(tmp).mode & 0o077, 0, "created owner-only");
    // second boot: accepted, not re-created
    assert.equal(ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } }).created, false);
  });

  it("refuses a symlink at TMPDIR and deletes nothing", () => {
    const target = path.join(base, "elsewhere");
    fs.mkdirSync(target);
    fs.writeFileSync(path.join(target, "keep"), "x");
    const tmp = path.join(runnerRoot, ".tmp");
    fs.symlinkSync(target, tmp);
    assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } }), (e: unknown) =>
      e instanceof RunnerTmpdirError && /is a symlink/.test(e.message));
    assert.ok(fs.lstatSync(tmp).isSymbolicLink());
    assert.equal(fs.readFileSync(path.join(target, "keep"), "utf8"), "x");
  });

  it("refuses a regular file at TMPDIR and keeps it", () => {
    const tmp = path.join(runnerRoot, ".tmp");
    fs.writeFileSync(tmp, "data");
    assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } }), (e: unknown) =>
      e instanceof RunnerTmpdirError && /is not a directory/.test(e.message));
    assert.equal(fs.readFileSync(tmp, "utf8"), "data");
  });

  it("refuses when an intermediate component is a symlink", () => {
    const real = path.join(base, "real");
    fs.mkdirSync(real);
    fs.symlinkSync(real, path.join(runnerRoot, ".tmp"));
    const tmp = path.join(runnerRoot, ".tmp", "uzi-worker");
    assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } }), /is a symlink/);
    assert.deepEqual(fs.readdirSync(real), []);
  });

  it("does not create missing parents (no recursive mkdir)", () => {
    const tmp = path.join(runnerRoot, "missing-parent", "uzi-worker");
    assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } }), /does not exist/);
    assert.equal(fs.existsSync(path.join(runnerRoot, "missing-parent")), false);
  });

  it("refuses a directory owned by another uid (simulated via the injectable uid)", () => {
    const tmp = path.join(runnerRoot, ".tmp");
    fs.mkdirSync(tmp);
    assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp }, uid: me + 1 }), (e: unknown) =>
      e instanceof RunnerTmpdirError && /owned by uid/.test(e.message));
    assert.ok(fs.existsSync(tmp));
  });

  it("refuses an owned directory that is not writable", { skip: me === 0 }, () => {
    const tmp = path.join(runnerRoot, ".tmp");
    fs.mkdirSync(tmp);
    fs.chmodSync(tmp, 0o500);
    try {
      assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: tmp } }), /not writable/);
    } finally {
      fs.chmodSync(tmp, 0o700);
    }
  });

  it("when TMPDIR equals the runner root only the real-directory check applies", () => {
    // owner mismatch is ignored (the root is the emptyDir mountpoint under an old controller)
    const r = ensureRunnerTmpdir({ dataDir, env: { TMPDIR: runnerRoot }, uid: me + 1 });
    assert.deepEqual(r, { checked: true, created: false, path: runnerRoot });
    // ...but a symlinked runner root is still refused
    fs.rmSync(runnerRoot, { recursive: true });
    fs.symlinkSync(base, runnerRoot);
    assert.throws(() => ensureRunnerTmpdir({ dataDir, env: { TMPDIR: runnerRoot } }), /is a symlink/);
  });
});
