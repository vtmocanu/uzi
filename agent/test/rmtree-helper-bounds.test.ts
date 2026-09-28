import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type CommandWrapper, measureRunCaches, rmHomeSubtree, STREAM_PRELUDE } from "../src/rmtree.js";

// PRD #1809, carried-over M1 review notes on the agent-uid helpers:
//  - E2: each helper stops at its OWN in-script wall-time budget (under the uid split the worker
//    cannot kill it), for the removal and, now, the measurement;
//  - E3: a directory of names that cannot be removed is read once per round, not once per batch;
//  - E4: a helper that times out or cannot run is reported in a short line, never with the whole
//    `node -e <script>` command.

const HAS_PROC_FD = fs.existsSync("/proc/self/fd");
const skipReason = "no /proc/self/fd on this host: the pinned helpers refuse here by design";
/** rmtree.ts HELPER_SLACK_MS: a pass's own budget stops this far short of its timeout. */
const HELPER_SLACK_MS = 5_000;

let home: string;
beforeEach(() => {
  home = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-bounds-"));
});
afterEach(() => {
  fs.rmSync(home, { recursive: true, force: true });
});

/** A go build cache of `n` small files across 16 dirs. */
function seedFiles(n: number): void {
  for (let d = 0; d < 16; d++) fs.mkdirSync(path.join(home, ".cache", "go-build", d.toString(16).padStart(2, "0")), { recursive: true });
  for (let i = 0; i < n; i++) {
    fs.writeFileSync(path.join(home, ".cache", "go-build", (i % 16).toString(16).padStart(2, "0"), `${i}-a`), "x");
  }
}

describe("agent-uid helpers stop at their own time budget (E2)", () => {
  it("the removal pass exits on its in-script budget and the drop says so", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedFiles(4000);
    // A deadline that leaves the pass its timeout but no in-script budget at all.
    const deadline = Date.now() + HELPER_SLACK_MS + 1;
    await assert.rejects(rmHomeSubtree(home, ".cache/go-build", { deadline }), /ran out of its budget/);
    assert.ok(fs.existsSync(path.join(home, ".cache", "go-build")), "the rest stays in place");
  });

  it("the measuring pass stops at its in-script budget and reports a truncated (lower-bound) reading", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedFiles(4000);
    const full = await measureRunCaches(home);
    assert.strictEqual(full.truncated, false);
    const cut = await measureRunCaches(home, { deadline: Date.now() + HELPER_SLACK_MS + 1 });
    assert.strictEqual(cut.truncated, true, "the budget ran out: the reading is a lower bound");
    assert.ok(cut.entries < full.entries);
  });
});

describe("drain keeps its cursor across batches (E3)", () => {
  type Drain = (
    dirFd: number,
    limit: number,
    spend: () => boolean,
    skip: Set<string>,
    each: (batch: { name: string; isDir: boolean }[]) => number,
    onFirst?: () => void,
  ) => void;
  const drain = new Function("fs", "FD", `${STREAM_PRELUDE}\nreturn drain;`)(fs, "/proc/self/fd/") as Drain;

  function withDir(n: number, fn: (fd: number) => void): void {
    const dir = path.join(home, "d");
    fs.mkdirSync(dir);
    for (let i = 0; i < n; i++) fs.writeFileSync(path.join(dir, `f${i}`), "");
    const fd = fs.openSync(dir, fs.constants.O_RDONLY | fs.constants.O_DIRECTORY);
    try {
      fn(fd);
    } finally {
      fs.closeSync(fd);
    }
  }

  it("a directory of names that cannot be removed costs one read per name, not N^2/batch", (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    withDir(1000, (fd) => {
      let reads = 0;
      let handed = 0;
      const skip = new Set<string>();
      drain(fd, 64, () => (reads++, true), skip, (batch) => {
        handed += batch.length;
        for (const e of batch) skip.add(e.name);
        return 0; // nothing could be removed
      });
      assert.strictEqual(handed, 1000, "every name was handed on exactly once");
      assert.strictEqual(reads, 1000, "one read per stuck name (the reopen-per-batch version read ~8,000 here)");
    });
  });

  it("a round that removed something is followed by one more round, which ends the drain", (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    withDir(300, (fd) => {
      let reads = 0;
      let rounds = 0;
      drain(
        fd,
        64,
        () => (reads++, true),
        new Set(),
        (batch) => {
          for (const e of batch) fs.unlinkSync(`/proc/self/fd/${fd}/${e.name}`);
          return batch.length;
        },
        () => rounds++,
      );
      assert.strictEqual(rounds, 1, "onFirst runs once, however many rounds");
      assert.strictEqual(reads, 300, "the second round found the directory empty");
      assert.strictEqual(fs.readdirSync(path.join(home, "d")).length, 0);
    });
  });

  it("stops when the budget is spent", (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    withDir(500, (fd) => {
      let reads = 0;
      let handed = 0;
      drain(fd, 64, () => ++reads <= 100, new Set(), (batch) => ((handed += batch.length), 0));
      assert.strictEqual(handed, 100);
    });
  });
});

describe("a failed helper is reported in a short line (E4)", () => {
  const scriptLeak = (msg: string): boolean => /process\.argv|O_NOFOLLOW|opendirSync|-e /.test(msg);

  it("a helper that times out: the removal's and the measurement's errors do not quote the script", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedFiles(1);
    // Ignores the script and outlives its timeout.
    const sleeper: CommandWrapper = () => ({ command: "/bin/sh", args: ["-c", "sleep 5"] });
    const deadline = () => Date.now() + 300;
    const removal = await rmHomeSubtree(home, ".cache/go-build", { wrappers: [sleeper], deadline: deadline() }).then(
      () => assert.fail("the removal must not succeed"),
      (e: Error) => e,
    );
    assert.match(removal.message, /agent-uid helper timed out/);
    assert.ok(!scriptLeak(removal.message) && removal.message.length < 200, `short: ${removal.message}`);
    const measure = await measureRunCaches(home, { wrappers: [sleeper], deadline: deadline() }).then(
      () => assert.fail("the measurement must not succeed"),
      (e: Error) => e,
    );
    assert.match(measure.message, /agent-uid helper timed out/);
    assert.ok(!scriptLeak(measure.message) && measure.message.length < 200, `short: ${measure.message}`);
  });

  it("a helper that cannot run, or exits non-zero without output", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const missing: CommandWrapper = (_c, args) => ({ command: "/nonexistent/uzi-helper", args: [...args] });
    const err = await measureRunCaches(home, { wrappers: [missing] }).then(
      () => assert.fail("must not succeed"),
      (e: Error) => e,
    );
    assert.ok(!scriptLeak(err.message) && err.message.length < 200, `short: ${err.message}`);
    const failing: CommandWrapper = () => ({ command: "/bin/sh", args: ["-c", "exit 9"] });
    const err2 = await measureRunCaches(home, { wrappers: [failing] }).then(
      () => assert.fail("must not succeed"),
      (e: Error) => e,
    );
    assert.strictEqual(err2.message, "agent-uid helper exited 9");
  });
});
