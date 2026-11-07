import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  defaultRetainedHomeFs, hasRetainedHomeMarker, RetainedHomeMarkerRefusal, readRetainedHomeMarker, removeRetainedHomeMarker, writeRetainedHomeMarker,
  type RetainedHomeFs, type RetainedHomeMarker,
} from "../src/retained-homes.js";
import { parseRunDirName, runDirKey } from "../src/util.js";

const RUN = "11111111-2222-4333-8444-555555555555";
const OTHER = "99999999-2222-4333-8444-555555555555";
let dataDir: string;
let file: string;
beforeEach(() => {
  dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "retained-homes-"));
  file = path.join(dataDir, "retained-homes", `${RUN}.json`);
});
afterEach(() => fs.rmSync(dataDir, { recursive: true, force: true }));

const marker = (over: Partial<RetainedHomeMarker> = {}): RetainedHomeMarker =>
  ({ version: 1, runId: RUN, predecessorGeneration: 1, predecessorAttemptId: "attempt-1", barePath: "/data/git/x.git", ...over });

/** Records every step and fails the n-th call of a named step. */
function spyFs(failAt?: { step: keyof RetainedHomeFs; nth?: number; code?: string }): { fs: RetainedHomeFs; steps: string[] } {
  const steps: string[] = [];
  const counts = new Map<string, number>();
  const wrap = <K extends keyof RetainedHomeFs>(step: K): RetainedHomeFs[K] =>
    (async (...args: unknown[]) => {
      steps.push(`${step}:${typeof args[0] === "string" ? path.basename(args[0]) : ""}`);
      const n = (counts.get(step) ?? 0) + 1;
      counts.set(step, n);
      if (failAt && failAt.step === step && n === (failAt.nth ?? 1)) {
        throw Object.assign(new Error(`injected ${step} failure`), failAt.code ? { code: failAt.code } : {});
      }
      return (defaultRetainedHomeFs[step] as (...a: unknown[]) => unknown)(...args);
    }) as RetainedHomeFs[K];
  return { fs: { mkdir: wrap("mkdir"), syncDir: wrap("syncDir"), createExclusive: wrap("createExclusive"), syncFile: wrap("syncFile"), read: wrap("read"), unlink: wrap("unlink") }, steps };
}

describe("run directory names (issue #3644 plan 3.12)", () => {
  it("runDirKey is the run id without a generation and <runId>.g<N> with one", () => {
    assert.equal(runDirKey(RUN), RUN);
    assert.equal(runDirKey(RUN, 2), `${RUN}.g2`);
    assert.throws(() => runDirKey(RUN, 0));
    assert.throws(() => runDirKey(RUN, 1.5));
  });
  it("parseRunDirName accepts exactly <uuid> and <uuid>.g<N> (N >= 1, no leading zero)", () => {
    assert.deepEqual(parseRunDirName(RUN), { runId: RUN });
    assert.deepEqual(parseRunDirName(`${RUN}.g12`), { runId: RUN, generation: 12 });
    assert.deepEqual(parseRunDirName(runDirKey(RUN, 3)), { runId: RUN, generation: 3 });
    for (const bad of [`${RUN}.g0`, `${RUN}.g01`, `${RUN}.g`, `${RUN}.g2.g3`, `${RUN}.g2x`, `${RUN}g2`, `${RUN}.G2`, `${RUN}.g-1`, ` ${RUN}`, `${RUN}\n`,
      `${RUN}.g99999999999999999999`, "", "run-1", `../${RUN}`, `${RUN}/x`]) {
      assert.equal(parseRunDirName(bad), undefined, JSON.stringify(bad));
    }
  });
});

describe("writeRetainedHomeMarker: the durability order", () => {
  it("creates the directory, fsyncs the data dir, writes and fsyncs the file, fsyncs the directory, then reads back", async () => {
    const { fs: spy, steps } = spyFs();
    await writeRetainedHomeMarker(dataDir, marker(), spy);
    assert.deepEqual(steps, ["mkdir:retained-homes", `syncDir:${path.basename(dataDir)}`, `createExclusive:${RUN}.json`, "syncDir:retained-homes", `read:${RUN}.json`]);
    const st = fs.statSync(file);
    assert.equal(st.mode & 0o777, 0o600);
    assert.equal(fs.statSync(path.dirname(file)).mode & 0o777, 0o700);
    assert.deepEqual(JSON.parse(fs.readFileSync(file, "utf8")), marker());
  });

  for (const step of ["mkdir", "syncDir", "createExclusive", "read"] as const) {
    it(`a failure at ${step} rejects`, async () => {
      const { fs: spy } = spyFs({ step });
      await assert.rejects(writeRetainedHomeMarker(dataDir, marker(), spy), /injected/);
    });
  }
  it("a failure of the data dir fsync stops before any marker file exists", async () => {
    const { fs: spy } = spyFs({ step: "syncDir", nth: 1 });
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker(), spy), /injected/);
    assert.equal(fs.existsSync(file), false);
  });
  it("a failure of the directory fsync after the file was written rejects, and the file stays (it still protects)", async () => {
    const { fs: spy } = spyFs({ step: "syncDir", nth: 2 });
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker(), spy), /injected/);
    assert.equal(fs.existsSync(file), true);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
  });

  it("an identical existing marker is re-synced (file, then directory) before it is accepted", async () => {
    await writeRetainedHomeMarker(dataDir, marker());
    const { fs: spy, steps } = spyFs();
    await writeRetainedHomeMarker(dataDir, marker(), spy);
    assert.deepEqual(steps, ["mkdir:retained-homes", `syncDir:${path.basename(dataDir)}`, `createExclusive:${RUN}.json`, `read:${RUN}.json`, `syncFile:${RUN}.json`, "syncDir:retained-homes", `read:${RUN}.json`]);
  });
  it("a failing re-sync of an existing marker refuses", async () => {
    await writeRetainedHomeMarker(dataDir, marker());
    const { fs: spy } = spyFs({ step: "syncFile" });
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker(), spy), /injected/);
  });
  it("a differing marker refuses and is left in place", async () => {
    await writeRetainedHomeMarker(dataDir, marker());
    const before = fs.readFileSync(file, "utf8");
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker({ predecessorAttemptId: "attempt-2" })), (e: unknown) => e instanceof RetainedHomeMarkerRefusal && /different or partial/.test(e.message));
    assert.equal(fs.readFileSync(file, "utf8"), before);
  });
  it("a partial marker (a write that died mid-way) refuses and still protects", async () => {
    fs.mkdirSync(path.dirname(file), { mode: 0o700 });
    fs.writeFileSync(file, '{"version":1,"runId"', { mode: 0o600 });
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker()), /different or partial/);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
  });
  it("refuses a symlinked marker name (O_NOFOLLOW) and a retained-homes path that is a symlink", async () => {
    fs.mkdirSync(path.dirname(file), { mode: 0o700 });
    fs.symlinkSync("/etc/hostname", file);
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker()));
    fs.rmSync(path.dirname(file), { recursive: true });
    fs.mkdirSync(path.join(dataDir, "elsewhere"));
    fs.symlinkSync(path.join(dataDir, "elsewhere"), path.dirname(file));
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker()), /not a directory/);
    assert.deepEqual(fs.readdirSync(path.join(dataDir, "elsewhere")), []);
  });
  it("refuses a run id that is not a UUID", async () => {
    await assert.rejects(writeRetainedHomeMarker(dataDir, marker({ runId: "../x" })), /UUID/);
  });
});

describe("reading the marker", () => {
  it("absent, then a valid marker round-trips", async () => {
    assert.deepEqual(await readRetainedHomeMarker(dataDir, RUN), { kind: "absent" });
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), false);
    await writeRetainedHomeMarker(dataDir, marker());
    assert.deepEqual(await readRetainedHomeMarker(dataDir, RUN), { kind: "marker", marker: marker() });
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
  });

  const plant = (setup: () => void): void => { fs.mkdirSync(path.dirname(file), { mode: 0o700, recursive: true }); setup(); };
  const cases: Array<[string, () => void]> = [
    ["a FIFO", () => execFileSync("mkfifo", [file])],
    ["a 0666 file", () => { fs.writeFileSync(file, JSON.stringify(marker())); fs.chmodSync(file, 0o666); }],
    ["a file with a second hard link", () => { fs.writeFileSync(file, JSON.stringify(marker()), { mode: 0o600 }); fs.linkSync(file, path.join(dataDir, "second-link")); }],
    ["an oversized file", () => fs.writeFileSync(file, "x".repeat(70000), { mode: 0o600 })],
    ["a symlink", () => fs.symlinkSync(path.join(dataDir, "nowhere"), file)],
    ["a directory", () => fs.mkdirSync(file)],
    ["malformed JSON", () => fs.writeFileSync(file, "{not json", { mode: 0o600 })],
    ["another run's marker", () => fs.writeFileSync(file, JSON.stringify(marker({ runId: OTHER })), { mode: 0o600 })],
    ["a marker with an extra field", () => fs.writeFileSync(file, JSON.stringify({ ...marker(), extra: 1 }), { mode: 0o600 })],
    ["a marker without a bare path", () => fs.writeFileSync(file, JSON.stringify({ ...marker(), barePath: undefined }), { mode: 0o600 })],
  ];
  for (const [name, setup] of cases) {
    it(`${name} is unreadable and counts as present`, async () => {
      plant(setup);
      assert.equal((await readRetainedHomeMarker(dataDir, RUN)).kind, "unreadable");
      assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
      assert.equal(await hasRetainedHomeMarker(dataDir, RUN, { barePath: "/data/git/x.git", attemptId: "attempt-1" }), false, "never positive evidence");
    });
  }
  it("a directory error other than ENOENT counts as present (retained-homes is a regular file)", async () => {
    fs.writeFileSync(path.join(dataDir, "retained-homes"), "not a directory");
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
  });
  it("an injected EACCES counts as present; an injected ENOENT-null does not", async () => {
    const eacces: RetainedHomeFs = { ...defaultRetainedHomeFs, read: async () => { throw Object.assign(new Error("denied"), { code: "EACCES" }); } };
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN, undefined, eacces), true);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN, undefined, { ...defaultRetainedHomeFs, read: async () => null }), false);
  });

  it("with a binding, only a marker recording exactly that bare path and predecessor attempt is evidence", async () => {
    await writeRetainedHomeMarker(dataDir, marker());
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN, { barePath: "/data/git/x.git", attemptId: "attempt-1" }), true);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN, { barePath: "/data/git/y.git", attemptId: "attempt-1" }), false);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN, { barePath: "/data/git/x.git", attemptId: "attempt-2" }), false);
  });
});

describe("removeRetainedHomeMarker", () => {
  it("removes the marker and syncs the directory; a missing marker or directory is fine", async () => {
    await writeRetainedHomeMarker(dataDir, marker());
    const { fs: spy, steps } = spyFs();
    await removeRetainedHomeMarker(dataDir, RUN, spy);
    assert.deepEqual(steps, [`unlink:${RUN}.json`, "syncDir:retained-homes"]);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), false);
    await removeRetainedHomeMarker(dataDir, RUN);
    fs.rmSync(path.dirname(file), { recursive: true });
    await removeRetainedHomeMarker(dataDir, RUN);
  });
  it("a failing unlink throws with the marker still present; a failing directory sync throws after the marker is already gone", async () => {
    await writeRetainedHomeMarker(dataDir, marker());
    await assert.rejects(removeRetainedHomeMarker(dataDir, RUN, spyFs({ step: "unlink" }).fs), /injected/);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), true);
    await assert.rejects(removeRetainedHomeMarker(dataDir, RUN, spyFs({ step: "syncDir" }).fs), /injected/);
    assert.equal(await hasRetainedHomeMarker(dataDir, RUN), false, "the unlink had succeeded; only its durability is unproven");
  });
});
