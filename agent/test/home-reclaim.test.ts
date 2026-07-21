import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { reclaimStrandedRunHomes, TERMINAL_RUN_STATUSES } from "../src/home-reclaim.js";
import { restoreTreeWritability } from "../src/rmtree.js";

/**
 * PRD #108 M6, the one-off reclaim. Its danger is the mirror image of the leak:
 * deleting a LIVE run's HOME (Risk 5). So the tests that matter here are the
 * NEGATIVE ones — every way of not-knowing must skip.
 */

const RUN_A = "11111111-1111-4111-8111-111111111111";
const RUN_B = "22222222-2222-4222-8222-222222222222";
const RUN_C = "33333333-3333-4333-8333-333333333333";

let root: string;
/** Far enough in the past to clear the age guard in every test below. */
const OLD = new Date(Date.now() - 24 * 60 * 60_000);

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-reclaim-"));
});

afterEach(async () => {
  await restoreTreeWritability(root).catch(() => undefined);
  fs.rmSync(root, { recursive: true, force: true });
});

/** A stranded HOME with something in it, aged past the guard. */
function makeHome(name: string, age: Date = OLD): string {
  const dir = path.join(root, name);
  fs.mkdirSync(path.join(dir, ".claude", "projects"), { recursive: true });
  fs.writeFileSync(path.join(dir, ".claude", "projects", "session.jsonl"), "{}\n");
  fs.utimesSync(dir, age, age);
  return dir;
}

const statuses = (map: Record<string, string>) => async (runId: string) => map[runId];

describe("reclaimStrandedRunHomes (PRD #108 M6)", () => {
  it("removes a terminal run's stranded HOME, including read-only (0555) directories", async (t) => {
    if (process.getuid?.() === 0) {
      t.skip("running as uid 0 — the 0555 part of this fixture is inert for root");
      return;
    }
    const dir = makeHome(RUN_A);
    const mod = path.join(dir, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
    fs.mkdirSync(mod, { recursive: true });
    fs.writeFileSync(path.join(mod, "benchmark_test.go"), "package inf\n");
    fs.chmodSync(mod, 0o555);
    fs.utimesSync(dir, OLD, OLD);
    assert.strictEqual((fs.lstatSync(mod).mode & 0o777).toString(8), "555", "fixture must be read-only at sweep time");

    const summary = await reclaimStrandedRunHomes(root, statuses({ [RUN_A]: "completed" }), nullLogger());

    assert.strictEqual(fs.existsSync(dir), false);
    assert.strictEqual(summary.removed, 1);
    assert.strictEqual(summary.examined, 1);
  });

  it("removes every terminal status and NO non-terminal one", async () => {
    // Pin the vocabulary against the runs.status CHECK (migration 00020) rather
    // than trusting the set literal to have stayed in step with it.
    assert.deepStrictEqual([...TERMINAL_RUN_STATUSES].sort(), ["cancelled", "completed", "failed"]);
    const nonTerminal = ["queued", "claimed", "running", "awaiting_approval"];
    for (const status of [...TERMINAL_RUN_STATUSES, ...nonTerminal]) {
      fs.rmSync(root, { recursive: true, force: true });
      fs.mkdirSync(root, { recursive: true });
      const dir = makeHome(RUN_A);
      const summary = await reclaimStrandedRunHomes(root, statuses({ [RUN_A]: status }), nullLogger());
      const terminal = TERMINAL_RUN_STATUSES.has(status);
      assert.strictEqual(fs.existsSync(dir), !terminal, `status ${status}: HOME removal must be ${terminal}`);
      assert.strictEqual(summary.removed, terminal ? 1 : 0, `status ${status}`);
      assert.strictEqual(summary.skippedNotTerminal, terminal ? 0 : 1, `status ${status}`);
    }
  });

  it("skips when the status lookup THROWS — an unreachable api must never authorise a delete", async () => {
    const dir = makeHome(RUN_A);
    const summary = await reclaimStrandedRunHomes(
      root,
      async () => {
        throw new Error("POST /api/worker/chat/runs/x returned 503: upstream unavailable");
      },
      nullLogger(),
    );
    assert.ok(fs.existsSync(dir), "an api failure must leave the HOME alone");
    assert.strictEqual(summary.removed, 0);
    assert.strictEqual(summary.skippedStatusUnknown, 1);
  });

  it("skips when the run is unknown (404 → undefined) rather than assuming it is garbage", async () => {
    const dir = makeHome(RUN_A);
    const summary = await reclaimStrandedRunHomes(root, async () => undefined, nullLogger());
    assert.ok(fs.existsSync(dir));
    assert.strictEqual(summary.skippedStatusUnknown, 1);
  });

  it("skips a recently-modified HOME even when the api reports it terminal", async () => {
    const dir = makeHome(RUN_A, new Date());
    let asked = 0;
    const summary = await reclaimStrandedRunHomes(
      root,
      async () => {
        asked += 1;
        return "completed";
      },
      nullLogger(),
    );
    assert.ok(fs.existsSync(dir), "the age guard is checked BEFORE the status, and is on its own sufficient to skip");
    assert.strictEqual(asked, 0, "a too-recent HOME must not even cost an api round-trip");
    assert.strictEqual(summary.skippedTooRecent, 1);
  });

  it("ignores anything that is not a run-id directory, and never follows a symlink", async () => {
    fs.writeFileSync(path.join(root, "stray.txt"), "x\n");
    fs.mkdirSync(path.join(root, "not-a-uuid"));
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-reclaim-outside-"));
    fs.writeFileSync(path.join(outside, "precious"), "keep\n");
    // A symlink NAMED like a run id, pointing out of the volume: the nastiest
    // shape, because the name filter alone would wave it through.
    fs.symlinkSync(outside, path.join(root, RUN_B), "dir");
    fs.utimesSync(root, OLD, OLD);

    try {
      const summary = await reclaimStrandedRunHomes(root, async () => "completed", nullLogger());
      assert.strictEqual(summary.examined, 0, "nothing here is a candidate");
      assert.strictEqual(summary.removed, 0);
      assert.strictEqual(summary.skippedNotRunDir, 3);
      assert.ok(fs.existsSync(path.join(outside, "precious")), "the symlink target is untouched");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("stops at the per-boot budget instead of round-tripping an unbounded volume", async () => {
    for (const id of [RUN_A, RUN_B, RUN_C]) makeHome(id);
    let asked = 0;
    const summary = await reclaimStrandedRunHomes(
      root,
      async () => {
        asked += 1;
        return "completed";
      },
      nullLogger(),
      { maxEntries: 2 },
    );
    assert.strictEqual(summary.examined, 2);
    assert.strictEqual(asked, 2);
    assert.strictEqual(summary.removed, 2);
    assert.strictEqual(fs.readdirSync(root).length, 1, "the third is left for the next boot");
    // Audit N1: the remainder must be REPORTED, not silently absent from every
    // counter. Without this an operator reading `examined: 500, removed: 3` cannot
    // tell a volume of 503 from one of 50,000 — exactly when they need to know.
    assert.strictEqual(summary.unexamined, 1);
    assert.strictEqual(summary.stoppedEarly, "budget");
  });

  it("accounts for EVERY candidate — the buckets sum to examined, and unexamined covers the rest", async () => {
    // One of each outcome plus a budget cut, so the arithmetic is exercised rather
    // than asserted on a trivial case.
    makeHome(RUN_A); // terminal -> removed
    makeHome(RUN_B); // running  -> skippedNotTerminal
    makeHome(RUN_C, new Date()); // fresh -> skippedTooRecent
    const status: Record<string, string> = { [RUN_A]: "completed", [RUN_B]: "running", [RUN_C]: "completed" };
    const summary = await reclaimStrandedRunHomes(root, async (id) => status[id], nullLogger());

    const bucketed =
      summary.removed +
      summary.skippedTooRecent +
      summary.skippedStatusUnknown +
      summary.skippedNotTerminal +
      summary.failed;
    assert.strictEqual(bucketed, summary.examined, "every examined directory lands in exactly one bucket");
    assert.strictEqual(summary.unexamined, 0, "nothing was left over");
    assert.strictEqual(summary.stoppedEarly, undefined);
  });

  it("BAILS OUT after consecutive status failures instead of blocking startup on a dead api", async () => {
    // The blocking audit finding. With the api unreachable NOTHING can be
    // reclaimed — every candidate resolves to unknown and skips — so continuing
    // only spends time the worker needs for registration and orphan recovery.
    // Against a HANGING api each lookup costs a full HTTP timeout.
    for (let i = 0; i < 20; i++) {
      makeHome(`4d4762cf-0000-4000-8000-${String(i).padStart(12, "0")}`);
    }
    let asked = 0;
    const summary = await reclaimStrandedRunHomes(
      root,
      async () => {
        asked += 1;
        throw new Error("connect ETIMEDOUT 10.0.0.1:8080");
      },
      nullLogger(),
      { maxConsecutiveFailures: 3 },
    );

    assert.strictEqual(asked, 3, `must stop after 3 failures, made ${asked} round-trips against 20 candidates`);
    assert.strictEqual(summary.stoppedEarly, "api_unreachable");
    assert.strictEqual(summary.unexamined, 17, "and it reports how much it did not get to");
    assert.strictEqual(summary.removed, 0);
    assert.strictEqual(fs.readdirSync(root).length, 20, "nothing is deleted when the api cannot be reached");
  });

  it("does not bail on ISOLATED failures — the streak has to be consecutive", async () => {
    const ids = [RUN_A, RUN_B, RUN_C];
    for (const id of ids) makeHome(id);
    let n = 0;
    const summary = await reclaimStrandedRunHomes(
      root,
      async () => {
        n += 1;
        if (n === 1) throw new Error("one unlucky request");
        return "completed";
      },
      nullLogger(),
      { maxConsecutiveFailures: 3 },
    );
    assert.strictEqual(summary.stoppedEarly, undefined, "a single failure must not abandon the sweep");
    assert.strictEqual(summary.examined, 3);
    assert.strictEqual(summary.removed, 2);
    assert.strictEqual(summary.skippedStatusUnknown, 1);
  });

  it("holds a wall-clock deadline even when the api answers slowly but successfully", async () => {
    for (const id of [RUN_A, RUN_B, RUN_C]) makeHome(id);
    // A fake clock: every read advances 40ms, so the deadline is crossed after the
    // first candidate without the test sleeping for real.
    let clock = 1_000_000;
    const summary = await reclaimStrandedRunHomes(root, async () => "completed", nullLogger(), {
      deadlineMs: 100,
      now: () => {
        clock += 40;
        return clock;
      },
    });
    assert.strictEqual(summary.stoppedEarly, "deadline");
    assert.ok(summary.unexamined > 0, "the deadline must leave work for the next boot rather than blocking startup");
  });

  it("is a quiet no-op when the HOME root does not exist yet (fresh volume)", async () => {
    const missing = path.join(root, "agent-home");
    const summary = await reclaimStrandedRunHomes(missing, async () => "completed", nullLogger());
    assert.deepStrictEqual(summary, {
      examined: 0,
      removed: 0,
      skippedNotRunDir: 0,
      skippedTooRecent: 0,
      skippedStatusUnknown: 0,
      skippedNotTerminal: 0,
      failed: 0,
      unexamined: 0,
    });
  });

  it("asks about exactly the run ids it is about to touch", async () => {
    makeHome(RUN_A);
    makeHome(RUN_B);
    const asked: string[] = [];
    await reclaimStrandedRunHomes(
      root,
      async (runId) => {
        asked.push(runId);
        return "running";
      },
      nullLogger(),
    );
    assert.deepStrictEqual(asked.sort(), [RUN_A, RUN_B].sort());
    // And the directory it asked about is the one named by that run id.
    for (const id of asked) assert.ok(await fsp.stat(path.join(root, id)));
  });
});
