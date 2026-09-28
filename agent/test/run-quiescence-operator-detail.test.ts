import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import {
  describeProcesses,
  newRunAttempt,
  reapProcesses,
  reapRunProcesses,
  sanitizeForLog,
  setQuiescenceViewForTests,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { workerSpawnNonce } from "../src/worker-spawn-mark.js";
import { makeFakeProcRoot, plantFakeProc, plantUnreadableUnattributed } from "./fake-proc.js";
import { recordingLogger } from "./helpers.js";
import { api, fakeGitlab, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork, worktreeDirFor } from "./runner-harness.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";

// issue #1783 M2 — operator visibility. When a proof is blocked by an unreadable runner-uid process
// that nothing attributes, the verdict's detail (and so the run's failure_reason, capped at 160
// characters by RunResidueBlockedError / CloneResidueBlockedError) and the "not quiescent" warn line
// name the pid and its comm (from the world-readable `stat`) and say it could not be attributed, so
// an operator knows what to kill. Every process-supplied string stays sanitized. Hermetic: every
// process here is planted in a FAKE proc root.

installHarness();

const HAS_LINUX = process.platform === "linux";
const ME = process.getuid?.() ?? 0;
const PID = 4242;
/** Every control, DEL, C1, line/paragraph separator and bidi code point sanitizeForLog replaces. */
// eslint-disable-next-line no-control-regex
const UNSAFE = /[\u0000-\u001f\u007f-\u009f\u061c\u200e\u200f\u2028\u2029\u202a-\u202e\u2066-\u2069]/u;

let root: string;
beforeEach(() => {
  root = makeFakeProcRoot();
  setQuiescenceViewForTests({ procRoot: root });
});
afterEach(() => {
  restoreHermeticView();
  fs.rmSync(root, { recursive: true, force: true });
});

const CLONE = "/data/runner/github.com+o+r/issue-18";
const own = newRunAttempt("run-18", 1, CLONE, () => []);

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

function plantUnreadable(pid: number, comm: string): void {
  plantFakeProc(root, pid, { uid: ME, comm, env: "unreadable", cwd: "/" });
}

/** Rewrite a planted process's `status` Name line, leaving its `stat` comm alone. */
function setStatusName(pid: number, name: string): void {
  const file = path.join(root, String(pid), "status");
  fs.writeFileSync(file, fs.readFileSync(file, "utf8").replace(/^Name:.*$/m, `Name:\t${name}`));
}

function assertClean(text: string, label: string): void {
  assert.ok(!UNSAFE.test(text), `${label} carries no control or bidi character: ${JSON.stringify(text)}`);
}

/** Comms that try to close their own entry and name another pid an operator might kill. */
const SPOOF_COMMS = ["x); pid 1 (y", "a) (b); pid 7 ("];
/** A double-quoted string with `"` and `\` backslash-escaped: how the detail renders a comm. */
const QUOTED = /"(?:[^"\\]|\\.)*"/g;

/** The `pid N` tokens a reader finds OUTSIDE the detail's quoted comms. */
function pidTokensOutsideComms(detail: string): string[] {
  return detail.replace(QUOTED, "<comm>").match(/pid \d+/g) ?? [];
}

describe("issue #1783 M2: an unattributed unreadable process is named in the verdict", { skip: !HAS_LINUX }, () => {
  it("the detail names the pid and comm first and says it could not be attributed", async () => {
    plantUnreadableUnattributed(root, PID);
    const r = await reapProcesses(req());
    assert.equal(r.state, "unverified");
    assert.equal(r.detail, `runner-uid pid ${PID} "ssh-agent" could not be attributed (env/cwd unreadable)`);
    assert.deepEqual(describeProcesses(r), [
      { pid: PID, uid: ME, comm: "ssh-agent", cwd: "/", reason: "unreadable_unattributed", attribution: "could not be attributed" },
    ]);
  });

  it("the comm comes from the world-readable stat, falling back to the status Name line", async () => {
    plantUnreadable(PID, "from-stat");
    setStatusName(PID, "from-status");
    assert.deepEqual((await reapProcesses(req())).processes.map((p) => p.comm), ["from-stat"]);
    fs.rmSync(path.join(root, String(PID), "stat"));
    assert.deepEqual((await reapProcesses(req())).processes.map((p) => p.comm), ["from-status"]);
  });

  it("a comm holding ')' and spaces parses whole, and the stat fields after it still attribute", async () => {
    plantUnreadable(PID, "a) (b c");
    const r = await reapProcesses(req());
    assert.deepEqual(r.processes.map((p) => p.comm), ["a) (b c"]);
    assert.match(r.detail, /^runner-uid pid 4242 "a\) \(b c" could not be attributed/);
    // The same comm on a process whose session is a recorded live root: the ppid/pgrp/session
    // fields after the LAST ")" are what attribute it.
    plantFakeProc(root, 50, { uid: ME, startTime: 777, env: {}, cwd: "/tmp" });
    plantFakeProc(root, PID, { uid: ME, comm: "a) (b c", sid: 50, env: "unreadable" });
    assert.equal((await reapProcesses(req({ liveRoots: [{ pid: 50, startTime: 777 }] }))).state, "quiescent");
  });

  it("a hostile comm (bidi override, BEL, ')') is sanitized in the detail, the entry and the log fields", async () => {
    const hostile = "\u202ex\u0007)y";
    plantUnreadable(PID, hostile);
    const r = await reapProcesses(req());
    assert.equal(r.processes[0]!.comm, "?x?)y");
    assert.ok(r.detail.startsWith(`runner-uid pid ${PID} "?x?)y" could not be attributed`), r.detail);
    assertClean(r.detail, "detail");
    assertClean(JSON.stringify(describeProcesses(r)), "log fields");
  });

  it("the helper's verdict crosses the uid boundary sanitized", async () => {
    plantUnreadable(PID, "\u202eevil\u2028comm");
    const r = await reapRunProcesses(req(), { viaHelper: true });
    assert.equal(r.state, "unverified", r.detail);
    assert.match(r.detail, /^runner-uid pid 4242 "\?evil\?comm" could not be attributed/);
    assertClean(r.detail, "helper detail");
    assertClean(JSON.stringify(r.processes), "helper processes");
  });

  it("many pids: two are named, the rest counted, and the first pid + a 64-char comm survive the 160 cap", async () => {
    const long = "c".repeat(64);
    for (let i = 0; i < 6; i++) plantUnreadable(PID + i, i === 0 ? long : `p${i}`);
    const r = await reapProcesses(req());
    assert.equal(r.processes.length, 6);
    assert.match(r.detail, /; pid 4243 "p1" \(env\/cwd unreadable\); \+4 more$/);
    const capped = sanitizeForLog(r.detail, 160);
    assert.ok(capped.includes(`runner-uid pid ${PID} "${long}" could not be attributed`), capped);
  });

  for (const spoof of SPOOF_COMMS) {
    it(`a comm spoofing another entry (${spoof}) cannot make the detail name a second pid`, async () => {
      plantUnreadable(PID, spoof);
      const r = await reapProcesses(req());
      assert.deepEqual(r.processes.map((p) => p.comm), [spoof]);
      assert.equal(r.detail, `runner-uid pid ${PID} ${JSON.stringify(spoof)} could not be attributed (env/cwd unreadable)`);
      assert.deepEqual(pidTokensOutsideComms(r.detail), [`pid ${PID}`], r.detail);
      const quoted = r.detail.match(QUOTED) ?? [];
      assert.equal(quoted.length, 1, r.detail);
      assert.equal(JSON.parse(quoted[0]!), spoof, "the quoted comm reads back as the process's own comm");
    });
  }

  it("a comm of quotes and backslashes stays one quoted string, and the worst case survives the 160 cap", async () => {
    plantUnreadable(PID, 'q"\\"); pid 1 ("');
    const r = await reapProcesses(req());
    assert.deepEqual(pidTokensOutsideComms(r.detail), [`pid ${PID}`], r.detail);
    assert.equal(JSON.parse((r.detail.match(QUOTED) ?? [])[0]!), 'q"\\"); pid 1 ("');
    // Worst case: every character a `"` (each escaped to two) and over the 64-character cap.
    const worst = '"'.repeat(70);
    plantUnreadable(4194304, worst);
    fs.rmSync(path.join(root, String(PID)), { recursive: true, force: true });
    const w = await reapProcesses(req());
    const entry = `runner-uid pid 4194304 "${'\\"'.repeat(64)}..."`;
    assert.equal(entry.length, 156, "the first pid and its comm end at most 156 characters in");
    assert.ok(sanitizeForLog(w.detail, 160).startsWith(entry), w.detail);
  });

  it("other unverified reasons are described sensibly", async () => {
    plantUnreadable(PID, "ssh-agent");
    fs.writeFileSync(path.join(root, String(PID + 1)), "not a directory");
    plantFakeProc(root, PID + 2, { uid: ME, env: {}, cwd: "/" });
    fs.writeFileSync(path.join(root, String(PID + 2), "status"), "garbage\n");
    const r = await reapProcesses(req());
    const reasons = Object.fromEntries(r.processes.map((p) => [p.pid, p.reason]));
    assert.deepEqual(reasons, { [PID]: "unreadable_unattributed", [PID + 1]: "status_unreadable", [PID + 2]: "status_unparsable" });
    assert.match(r.detail, /^runner-uid pid 4242 "ssh-agent" could not be attributed \(env\/cwd unreadable\); pid 4243 "\?" \(status unreadable\); \+1 more$/);
  });
});

// ─── runner flows: the failure_reason and the warn line ────────────────────────────────────

const lastFailed = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;

function factoryOf(body: (ctx: RunContext) => Promise<ExecutorResult>): ExecutorFactory {
  return (runId) => ({ homeDir: path.join(homeDir, runId), executor: { run: body } });
}

function notQuiescentLines(lines: unknown[]): Array<Record<string, unknown>> {
  return (lines as Array<Record<string, unknown>>).filter((l) => l.msg === "run clone is not quiescent");
}

describe("issue #1783 M2: the run's failure_reason and warn line name the blocking process", { skip: !HAS_LINUX }, () => {
  it("finalize: RunResidueBlockedError's reason names pid and comm; the warn line marks it unattributed", async () => {
    simulateCommittedWork();
    plantUnreadableUnattributed(root, PID);
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(3951);
    await runnerWith(factoryOf(async (ctx) => ({ branch: ctx.branch, summary: "done" })), fakeGitlab().gitlab, undefined, logger).execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    const reason = String(failed?.failure_reason);
    assert.ok(reason.includes(`pid ${PID} "ssh-agent" could not be attributed`), reason);
    const warn = notQuiescentLines(lines).find((l) => l.site === "finalize");
    assert.ok(warn, "the finalize proof logged its block");
    assert.match(String(warn.detail), /pid 4242 "ssh-agent" could not be attributed/);
    assert.deepEqual(
      (warn.processes as Array<Record<string, unknown>>).map((p) => [p.pid, p.comm, p.attribution]),
      [[PID, "ssh-agent", "could not be attributed"]],
    );
  });

  it("canonical reseed: CloneResidueBlockedError's reason names pid and comm", async () => {
    const iid = 3952;
    fs.mkdirSync(path.join(worktreeDirFor(iid), "old"), { recursive: true });
    plantUnreadableUnattributed(root, PID);
    const claim = gitlabClaim(iid);
    await runnerWith(
      factoryOf(async () => {
        throw new Error("stop after the seed");
      }),
      fakeGitlab().gitlab,
      undefined,
      undefined,
      { recoveryRetryMs: 5 },
    ).execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    const reason = String(failed?.failure_reason);
    assert.match(reason, /canonical runner clone path could not be freed/);
    assert.ok(reason.includes(`pid ${PID} "ssh-agent" could not be attributed`), reason);
  });

  it("a hostile comm never reaches the failure_reason or the warn line raw", async () => {
    simulateCommittedWork();
    plantUnreadable(PID, "\u202ex\u0007)y");
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(3953);
    await runnerWith(factoryOf(async (ctx) => ({ branch: ctx.branch, summary: "done" })), fakeGitlab().gitlab, undefined, logger).execute(claim);
    const reason = String(lastFailed(claim.run_id)?.failure_reason);
    assert.ok(reason.includes(`pid ${PID} "?x?)y" could not be attributed`), reason);
    assertClean(reason, "failure_reason");
    const warns = notQuiescentLines(lines);
    assert.ok(warns.length > 0);
    assertClean(JSON.stringify(warns), "warn lines");
  });

  it("a spoofing comm cannot make the failure_reason name a second pid", async () => {
    simulateCommittedWork();
    plantUnreadable(PID, SPOOF_COMMS[0]!);
    const claim = gitlabClaim(3955);
    await runnerWith(factoryOf(async (ctx) => ({ branch: ctx.branch, summary: "done" })), fakeGitlab().gitlab).execute(claim);
    const reason = String(lastFailed(claim.run_id)?.failure_reason);
    assert.ok(reason.includes(`pid ${PID} ${JSON.stringify(SPOOF_COMMS[0])} could not be attributed`), reason);
    assert.deepEqual(pidTokensOutsideComms(reason), [`pid ${PID}`], reason);
  });

  it("many long-comm pids: the failure_reason still carries the first pid and its whole comm", async () => {
    simulateCommittedWork();
    const long = "k".repeat(64);
    for (let i = 0; i < 5; i++) plantUnreadable(PID + i, long);
    const claim = gitlabClaim(3954);
    await runnerWith(factoryOf(async (ctx) => ({ branch: ctx.branch, summary: "done" })), fakeGitlab().gitlab).execute(claim);
    const reason = String(lastFailed(claim.run_id)?.failure_reason);
    assert.ok(reason.includes(`pid ${PID} "${long}" could not be attributed`), reason);
  });
});
