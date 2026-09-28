import { after, before, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { REAL_PROCFS_DENIED, SKIP_LOG_ENV, probeProcEnumeration, realProcfsSkip, skipFor } from "./real-procfs.js";

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

/** A stderr stand-in: collects skip lines so unit tests print no `real-procfs skip:` noise. */
function collector(): { lines: string[]; write: (line: string) => void } {
  const lines: string[] = [];
  return { lines, write: (line) => void lines.push(line) };
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

  /**
   * Run the probe on `root` while recording every directory it enumerates and every file read it
   * attempts (readFileSync/openSync/readSync on the fs default export the helper imports). The probe
   * must enumerate the root and never read a file under it.
   */
  function probeRecording(root: string): { result: string | false; enumerated: string[]; reads: string[] } {
    const enumerated: string[] = [];
    const reads: string[] = [];
    const recordingReaddir = (dir: string): string[] => {
      enumerated.push(dir);
      return fs.readdirSync(dir);
    };
    const spies = [
      mock.method(fs, "readFileSync"),
      mock.method(fs, "openSync"),
      mock.method(fs, "readFile"),
      mock.method(fs, "open"),
    ];
    let result: string | false;
    try {
      result = probeProcEnumeration(recordingReaddir, root);
    } finally {
      for (const spy of spies) {
        for (const call of spy.mock.calls) reads.push(String(call.arguments[0]));
        spy.mock.restore();
      }
    }
    return { result, enumerated, reads };
  }

  it("an enumerable root whose self/stat is a directory (EISDIR on read) is not a skip; no file is read", () => {
    const root = fs.mkdtempSync(path.join(tmp, "fake-proc-"));
    // `stat` planted as a DIRECTORY: reading it fails with EISDIR, even as root.
    fs.mkdirSync(path.join(root, "self", "stat"), { recursive: true });
    assert.throws(() => fs.readFileSync(path.join(root, "self", "stat")), { code: "EISDIR" });

    const { result, enumerated, reads } = probeRecording(root);
    assert.equal(result, false);
    assert.deepEqual(enumerated, [root]);
    assert.deepEqual(reads, []);
  });

  it(
    "an enumerable root whose self/stat is unreadable (EACCES on read, as under Landlock) is not a skip; no file is read",
    { skip: process.getuid?.() === 0 ? "running as uid 0, which reads a mode-000 file, so no EACCES file can be planted" : false },
    () => {
      const root = fs.mkdtempSync(path.join(tmp, "fake-proc-"));
      const stat = path.join(root, "self", "stat");
      fs.mkdirSync(path.dirname(stat), { recursive: true });
      fs.writeFileSync(stat, "1 (init) S 0\n");
      fs.chmodSync(stat, 0o000);
      try {
        assert.throws(() => fs.readFileSync(stat), { code: "EACCES" });

        const { result, enumerated, reads } = probeRecording(root);
        assert.equal(result, false);
        assert.deepEqual(enumerated, [root]);
        assert.deepEqual(reads, []);
      } finally {
        fs.chmodSync(stat, 0o600);
      }
    },
  );
});

describe("skipFor: recording a skip", () => {
  it("returns the reason and appends one `label<TAB>reason` line per skip", () => {
    const log = path.join(tmp, "skip-a.log");
    const denied = probeProcEnumeration(throwing("EACCES"), PROC_ROOT);
    assert.equal(typeof denied, "string");

    const err = collector();
    const first = skipFor("A-core lock custody", denied, log, err.write);
    assert.equal(first, `${denied as string} [A-core lock custody]`);
    const second = skipFor("statStartTime", denied, log, err.write);

    assert.equal(fs.readFileSync(log, "utf8"), `A-core lock custody\t${first as string}\nstatStartTime\t${second as string}\n`);
    assert.deepEqual(err.lines, [`real-procfs skip: ${first as string}\n`, `real-procfs skip: ${second as string}\n`]);
  });

  it("flattens tabs and newlines in a label so each record stays one two-field line", () => {
    const log = path.join(tmp, "skip-b.log");
    const err = collector();
    const reason = skipFor("a\tb\nc", "real-procfs-denied: x", log, err.write);
    assert.equal(fs.readFileSync(log, "utf8"), `a b c\t${reason as string}\n`);
    assert.deepEqual(err.lines, [`real-procfs skip: ${reason as string}\n`]);
  });

  it("returns the reason without a log path and writes no file", () => {
    const err = collector();
    assert.equal(skipFor("no log", "real-procfs-denied: x", undefined, err.write), "real-procfs-denied: x [no log]");
    assert.deepEqual(err.lines, ["real-procfs skip: real-procfs-denied: x [no log]\n"]);
  });

  it("records nothing when enumeration is not denied", () => {
    const log = path.join(tmp, "skip-c.log");
    const err = collector();
    assert.equal(skipFor("runs", false, log, err.write), false);
    assert.equal(fs.existsSync(log), false);
    assert.deepEqual(err.lines, []);
  });
});

describe("realProcfsSkip: the env-linked recorder", () => {
  // The env var is spelled LITERALLY here, not via SKIP_LOG_ENV: it is the contract with
  // scripts/real-procfs-skip-check.sh, so renaming the constant (or reading another variable) fails.
  it("appends a forced skip to the file named by UZI_REAL_PROCFS_SKIP_LOG", () => {
    assert.equal(SKIP_LOG_ENV, "UZI_REAL_PROCFS_SKIP_LOG");
    const log = path.join(tmp, "skip-env.log");
    const saved = process.env.UZI_REAL_PROCFS_SKIP_LOG;
    process.env.UZI_REAL_PROCFS_SKIP_LOG = log;
    try {
      const err = collector();
      const reason = realProcfsSkip("env linked", "real-procfs-denied: forced", err.write);
      assert.equal(reason, "real-procfs-denied: forced [env linked]");
      assert.equal(fs.readFileSync(log, "utf8"), "env linked\treal-procfs-denied: forced [env linked]\n");
      assert.deepEqual(err.lines, ["real-procfs skip: real-procfs-denied: forced [env linked]\n"]);
    } finally {
      if (saved === undefined) delete process.env.UZI_REAL_PROCFS_SKIP_LOG;
      else process.env.UZI_REAL_PROCFS_SKIP_LOG = saved;
    }
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
      const got = realProcfsSkip("live", undefined, collector().write);
      assert.equal(got, REAL_PROCFS_DENIED === false ? false : `${REAL_PROCFS_DENIED} [live]`);
    } finally {
      if (saved !== undefined) process.env.UZI_REAL_PROCFS_SKIP_LOG = saved;
    }
  });
});
