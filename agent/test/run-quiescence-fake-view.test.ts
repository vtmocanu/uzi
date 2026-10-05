import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { LimitReachedError } from "../src/limit.js";
import type { ExecutorFactory } from "../src/runner.js";
import {
  LiveAttemptRegistry,
  newRunAttempt,
  procfsTable,
  procfsTableAt,
  quiesceRunAttempt,
  reapProcesses,
  reapRunProcesses,
  setQuiescenceViewForTests,
  type ProcessQuiescence,
  type QuiescenceView,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { statStartTime, workerSpawnNonce } from "../src/worker-spawn-mark.js";
import {
  makeFakeProcRoot,
  plantFakeProc,
  plantInScope,
  plantUnreadableUnattributed,
  scopedRealView,
  withQuiescenceView,
} from "./fake-proc.js";
import { api, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork, worktreeDirFor } from "./runner-harness.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { realProcfsSkip } from "./real-procfs.js";

// issue #1783 — the CI incident, made hermetic. A same-uid non-dumpable process anywhere on the
// worker (on GitHub Actions a setgid ssh-agent: its environ reads back unreadable) is
// `unreadable_unattributed`, and every quiescence proof must FAIL CLOSED on it. Here it is planted
// in a FAKE proc root (the reaper's view, see setQuiescenceViewForTests), so these tests neither
// depend on nor touch the host's process table: each proof site blocks within the fake deadline,
// and the same flows over an empty fake root succeed.

installHarness();

const HAS_LINUX = process.platform === "linux";
const ME = process.getuid?.() ?? 0;
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const UNREADABLE_PID = 4242;

let root: string;
beforeEach(() => {
  root = makeFakeProcRoot();
  setQuiescenceViewForTests({ procRoot: root });
});
afterEach(() => {
  restoreHermeticView();
  fs.rmSync(root, { recursive: true, force: true });
});

const CLONE = "/data/runner/github.com+o+r/issue-17";
const own = newRunAttempt("run-17", 1, CLONE, () => []);

function req(extra: Partial<ScanRequest> = {}): ScanRequest {
  return {
    mode: "own",
    targetUid: ME,
    targetKey: own.cloneKey,
    targetPaths: [CLONE],
    ownMarker: own.marker,
    liveMarkers: [],
    liveRoots: [],
    workerNonce: workerSpawnNonce(),
    ...extra,
  };
}

function assertUnreadableBlocks(r: ProcessQuiescence | undefined): void {
  assert.equal(r?.state, "unverified", r?.detail);
  assert.deepEqual(
    r?.processes.map((p) => [p.pid, p.reason]),
    [[UNREADABLE_PID, "unreadable_unattributed"]],
  );
  assert.deepEqual(r?.killed, [], "an unattributed process is never signalled");
  assert.equal(fs.existsSync(path.join(root, String(UNREADABLE_PID))), true);
}

// ─── the fake root itself ──────────────────────────────────────────────────────────────────

const LINUX_ONLY = process.platform !== "linux" ? "requires a real Linux /proc root" : false;

describe("fake proc root: the reaper's reads", () => {
  it("parses the planted stat (ppid, group, session, field-22 start time) like a real one", async () => {
    plantFakeProc(root, 50, { uid: ME, ppid: 7, pgid: 8, sid: 9, startTime: 123456, env: {}, cwd: "/tmp" });
    const table = procfsTableAt(root);
    assert.deepEqual(table.listPids(), [50]);
    assert.equal(table.readCwd(50), "/tmp");
    assert.match(table.readStatus(50), new RegExp(`^Uid:\\t${ME}\\t`, "m"));
    assert.equal(statStartTime(table.readStat(50)), 123456);
    // An unreadable process whose session is a recorded live root is attributed, not unverified.
    plantFakeProc(root, 51, { uid: ME, sid: 50, env: "unreadable" });
    const r = await reapProcesses(req({ liveRoots: [{ pid: 50, startTime: 123456 }] }));
    assert.equal(r.state, "quiescent", r.detail);
  });
});

// ─── scan / reap / quiesce / helper ────────────────────────────────────────────────────────

describe("an unreadable_unattributed process blocks every process proof, within the fake deadline", { skip: !HAS_LINUX }, () => {
  it("reapProcesses (in-process): unverified, never signalled", async () => {
    plantUnreadableUnattributed(root, UNREADABLE_PID);
    const started = Date.now();
    const r = await reapProcesses(req());
    assert.ok(Date.now() - started < 3_000, "bounded by the view's short deadline, not the 5 s production one");
    assertUnreadableBlocks(r);
  });

  it("control: an own-marked in-scope process is 'killed' (its entry removed) and the reap is quiescent", async () => {
    plantInScope(root, 60, CLONE, { marker: own.marker, cloneKey: own.cloneKey });
    const r = await reapProcesses(req());
    assert.equal(r.state, "quiescent", r.detail);
    assert.deepEqual(r.killed, [60]);
    assert.equal(fs.existsSync(path.join(root, "60")), false);
  });

  it("an unmarked in-scope process is a seed-mode survivor, never killed", async () => {
    plantInScope(root, 61, CLONE);
    const r = await reapProcesses(req({ mode: "seed" }));
    assert.equal(r.state, "survivors");
    assert.deepEqual(r.processes.map((p) => [p.pid, p.reason]), [[61, "unattributed_in_scope"]]);
    assert.equal(fs.existsSync(path.join(root, "61")), true);
  });

  it("quiesceRunAttempt: the process half is unverified", async () => {
    plantUnreadableUnattributed(root, UNREADABLE_PID);
    const registry = new LiveAttemptRegistry();
    registry.add(own);
    const out = await quiesceRunAttempt({
      mode: "own",
      attempt: own,
      cloneKey: own.cloneKey,
      targetPaths: [CLONE],
      processes: true,
      dockerHost: undefined,
      registry,
    });
    assertUnreadableBlocks(out.process);
  });

  it("the REAL helper (spawned single-uid through runnerCommand) reaps the forwarded view: blocked", async () => {
    plantUnreadableUnattributed(root, UNREADABLE_PID);
    const r = await reapRunProcesses(req(), { viaHelper: true });
    assertUnreadableBlocks(r);
  });

  it("control: the real helper 'kills' an own-marked fake process by removing its entry, and is quiescent", async () => {
    plantInScope(root, 62, CLONE, { marker: own.marker, cloneKey: own.cloneKey });
    const r = await reapRunProcesses(req(), { viaHelper: true });
    assert.equal(r.state, "quiescent", r.detail);
    assert.deepEqual(r.killed, [62]);
    assert.equal(fs.existsSync(path.join(root, "62")), false);
  });
});

// ─── view validation ───────────────────────────────────────────────────────────────────────

const HELPER = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "run-quiescence.ts");
const PROC = path.join("/", "proc");

/** Run the real helper with `request` on stdin; its one verdict line. */
function helperVerdict(request: unknown): ProcessQuiescence {
  const r = spawnSync(process.execPath, ["--import", import.meta.resolve("tsx"), HELPER, "--uzi-quiesce-helper"], {
    input: JSON.stringify(request),
    cwd: "/",
    env: { PATH: process.env.PATH, TSX_DISABLE_CACHE: "1" },
    encoding: "utf8",
    timeout: 30_000,
  });
  assert.equal(r.status, 0, r.stderr);
  return JSON.parse(r.stdout.trim()) as ProcessQuiescence;
}

const BAD_VIEWS: Array<[string, unknown]> = [
  ["the real proc root", { procRoot: PROC }],
  ["the real proc root, unnormalized", { procRoot: `${PROC}/self/..` }],
  ["a path inside the real proc root", { procRoot: path.join(PROC, "1") }],
  ["a relative root", { procRoot: "fake-proc" }],
  ["a non-string root", { procRoot: 7 }],
  ["a negative deadline", { procRoot: "/tmp/x", deadlineMs: -1 }],
  ["a fractional interval", { procRoot: "/tmp/x", intervalMs: 1.5 }],
  ["an unbounded deadline", { procRoot: "/tmp/x", deadlineMs: 10_001 }],
  ["a deadline not below the helper timeout's margin", { procRoot: "/tmp/x", deadlineMs: 5_001 }],
  ["a zero interval (a spinning reap loop)", { procRoot: "/tmp/x", intervalMs: 0 }],
  ["an interval above the deadline cap", { procRoot: "/tmp/x", intervalMs: 5_001 }],
  ["a non-finite deadline", { procRoot: "/tmp/x", deadlineMs: "Infinity" }],
  ["both shapes at once", { procRoot: "/tmp/x", descendantsOf: 1 }],
  ["a non-pid descendant root", { descendantsOf: 0 }],
  ["a relative pidfile", { descendantsOf: 1, extraPidFiles: ["pids"] }],
  ["null", null],
];

describe("a malformed view is refused by the setter and by the helper", () => {
  for (const [name, view] of BAD_VIEWS) {
    it(`${name}: the setter throws`, () => {
      assert.throws(() => setQuiescenceViewForTests(view as QuiescenceView), /invalid quiescence test view/);
    });
  }

  it("the helper answers 'helper request invalid' for every one of them", { skip: !HAS_LINUX }, () => {
    for (const [name, view] of BAD_VIEWS) {
      const v = helperVerdict({ ...req(), view });
      assert.equal(v.state, "unverified", name);
      assert.equal(v.detail, "helper request invalid", name);
    }
  });

  it("control: the timing bounds themselves are accepted (deadline 0..5000 ms, interval 1..5000 ms)", () => {
    for (const view of [
      { procRoot: root, deadlineMs: 0, intervalMs: 1 },
      { procRoot: root, deadlineMs: 5_000, intervalMs: 5_000 },
      { descendantsOf: process.pid, deadlineMs: 5_000, intervalMs: 1 },
    ]) {
      assert.doesNotThrow(() => setQuiescenceViewForTests(view), JSON.stringify(view));
    }
  });

  it("the real proc root reached through a symlink is refused too", { skip: LINUX_ONLY }, () => {
    const link = path.join(root, "proc-link");
    fs.symlinkSync(PROC, link);
    assert.throws(() => setQuiescenceViewForTests({ procRoot: link }), /invalid quiescence test view/);
    if (HAS_LINUX) assert.equal(helperVerdict({ ...req(), view: { procRoot: link } }).detail, "helper request invalid");
  });

  it("control: a well-formed fake view reaches the helper and is reaped against", { skip: !HAS_LINUX }, () => {
    plantUnreadableUnattributed(root, UNREADABLE_PID);
    const v = helperVerdict({ ...req(), view: { procRoot: root, deadlineMs: 50, intervalMs: 5 } });
    assert.equal(v.state, "unverified", v.detail);
    assert.deepEqual(v.processes.map((p) => p.reason), ["unreadable_unattributed"]);
  });
});

// ─── the scoped real view (real procfs) ────────────────────────────────────────────────────

// This suite reads the REAL proc root: a sandbox that denies enumerating it skips through the
// shared detector (issue #1863). The fake-root suites above and below keep running there.
describe("the scoped real view lists only this process's descendants and the registered pids", {
  skip: !HAS_LINUX ? "reads procfs (Linux only)" : realProcfsSkip("run-quiescence-fake-view: the scoped real view"),
}, () => {
  it("a real non-descendant in the clone is excluded, and appears once registered by pid or pidfile (within 64 KiB)", async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-scoped-view-"));
    const clone = path.join(tmp, "runner", "github.com+o+r", "issue-scoped");
    fs.mkdirSync(clone, { recursive: true });
    // Backgrounded by a short-lived sh, so it is reparented away from this process: NOT a
    // descendant. `$!` is the node process's own pid.
    const orphan = Number(
      execFileSync("sh", ["-c", '"$0" -e "setInterval(() => {}, 1000)" </dev/null >/dev/null 2>&1 & echo $!', process.execPath], {
        cwd: clone,
        env: { PATH: process.env.PATH },
        encoding: "utf8",
      }).trim(),
    );
    try {
      assert.ok(Number.isSafeInteger(orphan) && orphan > 0);
      const stat = procfsTable.readStat(orphan);
      assert.notEqual(Number(stat.slice(stat.lastIndexOf(")") + 2).split(/\s+/)[1]), process.pid, "the orphan is not our child");
      // Seed mode never signals an unmarked process, and the key and path are unique to this test.
      const seed = req({ mode: "seed", ownMarker: undefined, targetKey: "github.com+o+r/issue-scoped", targetPaths: [clone] });
      const listed = (r: ProcessQuiescence): number[] => r.processes.map((p) => p.pid);

      const plain = await withQuiescenceView(scopedRealView(), () => reapProcesses(seed));
      assert.equal(plain.state, "quiescent", plain.detail);
      assert.ok(!listed(plain).includes(orphan), "a non-descendant is outside the scoped view");

      const byPid = await withQuiescenceView(scopedRealView({ pids: [orphan] }), () => reapProcesses(seed));
      assert.equal(byPid.state, "survivors", byPid.detail);
      assert.deepEqual(byPid.processes.map((p) => [p.pid, p.reason]), [[orphan, "unattributed_in_scope"]]);

      const pidFile = path.join(tmp, "pids");
      fs.writeFileSync(pidFile, `${orphan}\n`);
      const byFile = await withQuiescenceView(scopedRealView({ pidFiles: [pidFile] }), () => reapProcesses(seed));
      assert.deepEqual(listed(byFile), [orphan], byFile.detail);

      // Only the first 64 KiB of a pidfile is read: a pid listed after that is not in the view.
      fs.writeFileSync(pidFile, `${"x\n".repeat(33 * 1024)}${orphan}\n`);
      const pastCap = await withQuiescenceView(scopedRealView({ pidFiles: [pidFile] }), () => reapProcesses(seed));
      assert.equal(pastCap.state, "quiescent", pastCap.detail);

      // A line cut at the 64 KiB cap is dropped, not read as a pid: the orphan's pid followed by one
      // more digit straddles the boundary so that exactly the orphan's pid lies inside the cap. Read
      // as a pid, that fragment would list the orphan.
      const cap = 64 * 1024;
      const digits = String(orphan);
      const padding = `${"x".repeat(cap - digits.length - 1)}\n`;
      fs.writeFileSync(pidFile, `${padding}${digits}7\n`);
      assert.equal(fs.readFileSync(pidFile, "utf8").slice(0, cap).split("\n").at(-1), digits, "the cut leaves the orphan's pid");
      const straddle = await withQuiescenceView(scopedRealView({ pidFiles: [pidFile] }), () => reapProcesses(seed));
      assert.equal(straddle.state, "quiescent", straddle.detail);
      assert.ok(!listed(straddle).includes(orphan), "a truncated fragment is not read as a pid");
      // Control: the same pid ending exactly at the cap, newline included, is read.
      fs.writeFileSync(pidFile, `${"x".repeat(cap - digits.length - 2)}\n${digits}\n`);
      const atCap = await withQuiescenceView(scopedRealView({ pidFiles: [pidFile] }), () => reapProcesses(seed));
      assert.deepEqual(listed(atCap), [orphan], atCap.detail);
      assert.equal(alive(orphan), true, "no scan here signalled the orphan");
    } finally {
      try {
        process.kill(orphan, "SIGKILL");
      } catch {
        // gone
      }
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  });
});

function alive(pid: number): boolean {
  try {
    return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

// ─── runner flows over the real primitive ──────────────────────────────────────────────────

function journal(iid: number): string | undefined {
  try {
    const raw = execFileSync("git", ["-C", git.barePathFor(fx.originPath), "config", "--local", "--get", `uzi-recovery.agent/issue-${iid}.clone`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
    return raw || undefined;
  } catch {
    return undefined;
  }
}

function trackingSha(iid: number): string | null {
  try {
    return execFileSync("git", ["-C", git.barePathFor(fx.originPath), "rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
  } catch {
    return null;
  }
}

function commitWork(tree: string): void {
  fs.writeFileSync(path.join(tree, "WORK.txt"), "work\n");
  execFileSync("git", ["-C", tree, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

const lastFailed = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;
const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));

/** A factory whose executor runs `body`, counting the starts. */
function factoryOf(body: (ctx: RunContext) => Promise<ExecutorResult>): { factory: ExecutorFactory; started: () => number } {
  let started = 0;
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx) => {
        started++;
        return body(ctx);
      },
    },
  });
  return { factory, started: () => started };
}

/** Each runner flow, once with the unreadable process planted (`blocked`) and once without. */
describe("runner flows over the real quiescence primitive and a fake proc root", { skip: !HAS_LINUX }, () => {
  for (const blocked of [true, false]) {
    const label = blocked ? "unreadable_unattributed planted" : "empty fake root (control)";
    const plant = (): void => {
      if (blocked) plantUnreadableUnattributed(root, UNREADABLE_PID);
    };

    it(`canonical reseed, ${label}`, async () => {
      const iid = blocked ? 3901 : 3902;
      const canonical = worktreeDirFor(iid);
      fs.mkdirSync(path.join(canonical, "old"), { recursive: true });
      plant();
      const { factory, started } = factoryOf(async () => {
        throw new Error("stop after the seed");
      });
      const claim = gitlabClaim(iid);
      await runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { recoveryRetryMs: 5 }).execute(claim);
      if (blocked) {
        assert.equal(lastFailed(claim.run_id)?.fail_origin, "worker_residue_blocked");
        assert.match(String(lastFailed(claim.run_id)?.failure_reason), /canonical runner clone path could not be freed/);
        assert.equal(started(), 0, "no model started");
        assert.equal(fs.existsSync(path.join(canonical, "old")), true, "nothing was freed");
      } else {
        assert.equal(started(), 1, "the reseed went through and the model started");
        assert.notEqual(lastFailed(claim.run_id)?.fail_origin, "worker_residue_blocked");
      }
    });

    it(`terminal retire, ${label}`, async () => {
      const iid = blocked ? 3911 : 3912;
      plant();
      const { factory } = factoryOf(async () => {
        throw new Error("agent crashed");
      });
      const claim = gitlabClaim(iid);
      await runnerWith(factory, fakeGitlab().gitlab).execute(claim);
      assert.ok(statuses(claim.run_id).includes("failed"));
      assert.equal(fs.existsSync(worktreeDirFor(iid)), blocked, blocked ? "the clone is kept" : "the clone is retired");
      assert.equal(journal(iid) !== undefined, blocked, blocked ? "the journal is kept" : "the journal is cleared");
    });

    it(`limit park, ${label}`, async () => {
      const iid = blocked ? 3921 : 3922;
      plant();
      const { factory } = factoryOf(async (ctx) => {
        commitWork(ctx.worktreePath);
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      });
      const claim = gitlabClaim(iid, { wait_on_limit: true });
      await runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1 }).execute(claim);
      assert.ok(statuses(claim.run_id).includes("limit_wait"), "the park stands either way");
      if (blocked) {
        assert.equal(trackingSha(iid), null, "no fetch-back: the sink body was skipped");
        assert.ok(journal(iid), "the journal still points at the kept clone");
        assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      } else {
        assert.notEqual(trackingSha(iid), null, "the work was fetched back into the tracking ref");
      }
    });

    it(`wall park (capture), ${label}`, async () => {
      const iid = blocked ? 3931 : 3932;
      plant();
      const outcomes: unknown[] = [];
      const { factory } = factoryOf(async (ctx) => {
        commitWork(ctx.worktreePath);
        const o = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
        outcomes.push(o);
        return o === "parked" || o === "undeliverable"
          ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
          : { branch: ctx.branch };
      });
      const claim = gitlabClaim(iid);
      await runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1 }).execute(claim);
      assert.deepEqual(outcomes, ["parked"], "the wall park stands");
      const body = api.wallParkRequests[0]!.body as { head: string; published: boolean };
      assert.ok(!statuses(claim.run_id).includes("failed"));
      if (blocked) {
        assert.equal(body.head, "", "nothing was captured");
        assert.equal(body.published, false);
        assert.equal(trackingSha(iid), null);
        assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      } else {
        assert.equal(body.head.length, 40, "a verified capture");
        assert.equal(trackingSha(iid), body.head);
      }
    });

    it(`finalize, ${label}`, async () => {
      const iid = blocked ? 3941 : 3942;
      simulateCommittedWork();
      plant();
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const { factory } = factoryOf(async (ctx) => ({ branch: ctx.branch, summary: "done" }));
      const claim = gitlabClaim(iid);
      await runnerWith(factory, gitlab).execute(claim);
      if (blocked) {
        assert.equal(lastFailed(claim.run_id)?.fail_origin, "worker_residue_blocked");
        assert.equal(mrCalls.length, 0, "nothing was published");
        assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
      } else {
        assert.ok(statuses(claim.run_id).includes("completed"), "the run completes");
        assert.equal(mrCalls.length, 1);
        assert.equal(fs.existsSync(worktreeDirFor(iid)), false, "the clone is retired");
      }
    });
  }
});
