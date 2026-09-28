import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { REAL_PROCFS_DENIED, probeProcEnumeration, realProcfsSkip, skipFor } from "./real-procfs.js";

// issue #1863 — the shared real-procfs detector. A test that reads the real process table skips
// ONLY when enumerating the proc root is denied (EACCES/EPERM, as in the Landlock command sandbox);
// every other outcome runs the test, and every skip is recorded for scripts/real-procfs-skip-check.sh.

const PROC_ROOT = path.join("/", "proc");

function errnoError(code: string): NodeJS.ErrnoException {
  const err: NodeJS.ErrnoException = new Error(`${code}: injected, scandir '${PROC_ROOT}'`);
  err.code = code;
  return err;
}

function throwing(code: string): (dir: string) => never {
  return () => {
    throw errnoError(code);
  };
}

let tmp: string;
before(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-real-procfs-"));
});
after(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

describe("probeProcEnumeration", () => {
  for (const code of ["EACCES", "EPERM"]) {
    it(`a denied enumeration (${code}) is a skip naming the token, the errno and the root`, () => {
      const reason = probeProcEnumeration(throwing(code), PROC_ROOT);
      assert.equal(typeof reason, "string");
      assert.match(reason as string, /real-procfs-denied/);
      assert.ok((reason as string).includes(code), reason as string);
      assert.ok((reason as string).includes(PROC_ROOT), reason as string);
    });
  }

  it("an enumerable root is not a skip", () => {
    assert.equal(probeProcEnumeration(() => ["1", "self"], PROC_ROOT), false);
  });

  for (const code of ["ENOENT", "ENOTDIR", "EIO"]) {
    it(`any other readdir error (${code}) is not a skip: the test runs and fails loudly`, () => {
      assert.equal(probeProcEnumeration(throwing(code), PROC_ROOT), false);
    });
  }

  it("a non-errno throw is not a skip", () => {
    assert.equal(
      probeProcEnumeration(() => {
        throw new Error("no code");
      }, PROC_ROOT),
      false,
    );
  });

  it("an enumerable root with an unreadable self/stat is not a skip, and only the root is enumerated", () => {
    const root = fs.mkdtempSync(path.join(tmp, "fake-proc-"));
    // `stat` planted as a DIRECTORY: reading it fails with EISDIR, even as root.
    fs.mkdirSync(path.join(root, "self", "stat"), { recursive: true });
    assert.throws(() => fs.readFileSync(path.join(root, "self", "stat")), { code: "EISDIR" });

    const enumerated: string[] = [];
    const recordingReaddir = (dir: string): string[] => {
      enumerated.push(dir);
      return fs.readdirSync(dir);
    };
    assert.equal(probeProcEnumeration(recordingReaddir, root), false);
    assert.deepEqual(enumerated, [root]);
  });
});

describe("skipFor: recording a skip", () => {
  it("returns the reason and appends one `label<TAB>reason` line per skip", () => {
    const log = path.join(tmp, "skip-a.log");
    const denied = probeProcEnumeration(throwing("EACCES"), PROC_ROOT);
    assert.equal(typeof denied, "string");

    const first = skipFor("A-core lock custody", denied, log);
    assert.equal(first, `${denied as string} [A-core lock custody]`);
    const second = skipFor("statStartTime", denied, log);

    assert.equal(fs.readFileSync(log, "utf8"), `A-core lock custody\t${first as string}\nstatStartTime\t${second as string}\n`);
  });

  it("flattens tabs and newlines in a label so each record stays one two-field line", () => {
    const log = path.join(tmp, "skip-b.log");
    const reason = skipFor("a\tb\nc", "real-procfs-denied: x", log);
    assert.equal(fs.readFileSync(log, "utf8"), `a b c\t${reason as string}\n`);
  });

  it("returns the reason without a log path and writes no file", () => {
    assert.equal(skipFor("no log", "real-procfs-denied: x", undefined), "real-procfs-denied: x [no log]");
  });

  it("records nothing when enumeration is not denied", () => {
    const log = path.join(tmp, "skip-c.log");
    assert.equal(skipFor("runs", false, log), false);
    assert.equal(fs.existsSync(log), false);
  });
});

describe("the live probe on this host", () => {
  it("REAL_PROCFS_DENIED agrees with a direct enumeration of the proc root", () => {
    let code: string | undefined;
    try {
      fs.readdirSync(PROC_ROOT);
    } catch (err) {
      code = (err as NodeJS.ErrnoException).code ?? "unknown";
    }
    if (code === undefined) {
      assert.equal(REAL_PROCFS_DENIED, false);
    } else if (code === "EACCES" || code === "EPERM") {
      assert.equal(typeof REAL_PROCFS_DENIED, "string");
      assert.ok((REAL_PROCFS_DENIED as string).includes(code));
    } else {
      assert.equal(REAL_PROCFS_DENIED, false);
    }
  });

  it("realProcfsSkip returns the live verdict, labelled", () => {
    // Record nowhere: this is not a real skip, so it must not reach the wrapper's skip log.
    const saved = process.env.UZI_REAL_PROCFS_SKIP_LOG;
    delete process.env.UZI_REAL_PROCFS_SKIP_LOG;
    try {
      const got = realProcfsSkip("live");
      assert.equal(got, REAL_PROCFS_DENIED === false ? false : `${REAL_PROCFS_DENIED} [live]`);
    } finally {
      if (saved !== undefined) process.env.UZI_REAL_PROCFS_SKIP_LOG = saved;
    }
  });
});
