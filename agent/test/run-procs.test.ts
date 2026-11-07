import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { INCOMPLETE_REASONS, type RunProcsOptions, helperRejectionReason, reapRunProcesses, scanRunProcesses } from "../src/run-procs.js";
import type { CommandWrapper } from "../src/rmtree.js";
import { realProcfsSkip } from "./real-procfs.js";

// PRD #1809 D4: process ATTRIBUTION by environment. The Claude CLI spawns every Bash command
// detached (a new session and process group), so the run's processes are found by `HOME=<runHome>`
// in their environment or a working directory inside the run's trees. An unreadable process
// (non-dumpable) counts only when its stat links it to one of those, or to a CLI pid. Seams: a fake
// proc root (the same layout: <pid>/status, stat, environ, cwd, and self/mountinfo); some tests read
// this host's real one.

const UID = process.getuid?.() ?? 0;
const PROC = `/${"proc"}`;
const HAS_PROC = fs.existsSync(`${PROC}/self/status`);
/** The skip for a test of this host's REAL proc tree: none, "no proc filesystem", or (a sandbox
 *  that denies enumerating the proc root, issue #1863) the shared detector's recorded reason. */
const realTreeSkip = (label: string): string | false =>
  !HAS_PROC ? "no proc filesystem here" : realProcfsSkip(`run process attribution: ${label}`);

let root: string;
let procRoot: string;
let runHome: string;
let worktree: string;
const children: ChildProcess[] = [];
/** Pids a test started indirectly (grandchildren), killed in afterEach. */
const strays: number[] = [];

/** Write the fake root's `self/mountinfo`: one proc mount at the fake root, with these options. */
function mountinfo(mountOpts = "rw,nosuid,nodev,noexec,relatime", superOpts = "rw", fstype = "proc"): void {
  fs.mkdirSync(path.join(procRoot, "self"), { recursive: true });
  fs.writeFileSync(
    path.join(procRoot, "self", "mountinfo"),
    `21 1 0:5 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n22 21 0:21 / ${procRoot} ${mountOpts} shared:12 - ${fstype} proc ${superOpts}\n`,
  );
}

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-procs-"));
  procRoot = path.join(root, "proc");
  runHome = path.join(root, "agent-home", "run-a");
  worktree = path.join(root, "runner", "issue-1");
  for (const d of [procRoot, runHome, worktree]) fs.mkdirSync(d, { recursive: true });
  mountinfo();
});

afterEach(() => {
  for (const c of children.splice(0)) {
    if (c.exitCode === null && c.signalCode === null) c.kill("SIGKILL");
  }
  for (const pid of strays.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // already gone
    }
  }
  fs.rmSync(root, { recursive: true, force: true });
});

interface FakeProc {
  uid?: number;
  ppid?: number;
  pgrp?: number;
  session?: number;
  state?: string;
  start?: number;
  env?: string[] | "unreadable";
  cwd?: string | "unreadable";
}

/** The fake `stat` line: fields after the comm are state ppid pgrp session ...; starttime is the
 *  22nd field overall. */
function statLine(pid: number, p: FakeProc): string {
  const rest = [p.state?.[0] ?? "S", p.ppid ?? 2, p.pgrp ?? pid, p.session ?? pid, 0, -1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 20, 0, 1, 0, p.start ?? 12345, 0];
  return `${pid} (go test) ${rest.join(" ")}\n`;
}

/** One fake `<procRoot>/<pid>` entry, laid out as the kernel's. */
function fake(pid: number, p: FakeProc): void {
  const dir = path.join(procRoot, String(pid));
  fs.mkdirSync(dir);
  const uid = p.uid ?? UID;
  fs.writeFileSync(path.join(dir, "status"), `Name:\tgo\nState:\t${p.state ?? "S (sleeping)"}\nPid:\t${pid}\nUid:\t${uid}\t${uid}\t${uid}\t${uid}\n`);
  fs.writeFileSync(path.join(dir, "stat"), statLine(pid, p));
  if (p.env === "unreadable") fs.mkdirSync(path.join(dir, "environ"));
  else fs.writeFileSync(path.join(dir, "environ"), Buffer.from((p.env ?? ["PATH=/usr/bin"]).map((e) => `${e}\0`).join("")));
  if (p.cwd !== "unreadable") fs.symlinkSync(p.cwd ?? "/", path.join(dir, "cwd"));
}

const scan = (opts: RunProcsOptions = {}, spawned: number[] = []) =>
  scanRunProcesses(runHome, worktree, spawned, { procRoot, workerPid: 1, ...opts });

/** A python3 that can call prctl, or undefined. */
function python(): string | undefined {
  for (const p of ["python3", "/usr/bin/python3"]) {
    try {
      execFileSync(p, ["-c", "import ctypes; ctypes.CDLL(None).prctl"], { stdio: "ignore" });
      return p;
    } catch {
      // try the next
    }
  }
  return undefined;
}

/** Python that makes itself non-dumpable (PR_SET_DUMPABLE = 4, value 0), then writes its pid. */
const NONDUMPABLE =
  "import ctypes, os, sys, time\nctypes.CDLL(None).prctl(4, 0, 0, 0, 0)\nopen(sys.argv[1], 'w').write(str(os.getpid()))\ntime.sleep(30)\n";

async function pidFrom(file: string): Promise<number> {
  const end = Date.now() + 10_000;
  for (;;) {
    try {
      const pid = Number(fs.readFileSync(file, "utf8"));
      if (pid > 0) return pid;
    } catch {
      // not yet
    }
    if (Date.now() > end) throw new Error(`no pid in ${file}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

/** True when this uid really cannot read a pid's environ (non-dumpable here, no CAP_SYS_PTRACE). */
function environUnreadable(pid: number): boolean {
  try {
    fs.readFileSync(`${PROC}/${pid}/environ`);
    return false;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === "EACCES";
  }
}

describe("run process attribution (PRD #1809 D4)", () => {
  it("a detached background job (its own process group) with HOME=<runHome> keeps the run not quiet", async () => {
    fake(4101, { env: ["PATH=/usr/bin", `HOME=${runHome}`, "GOFLAGS=-mod=mod"], cwd: "/tmp" });
    const r = await scan();
    assert.deepStrictEqual(r.pids, [4101]);
    assert.strictEqual(r.complete, true);
    assert.deepStrictEqual(r.incompleteReasons, [], "a complete scan names no reason");
  });

  it("a process of another run (a different HOME, cwd elsewhere) is not this run's", async () => {
    fake(4102, { env: [`HOME=${path.join(root, "agent-home", "run-b")}`], cwd: path.join(root, "runner", "issue-2") });
    fake(4103, { env: [`HOME=${runHome}x`, `XHOME=${runHome}`, `OLDHOME=${runHome}`], cwd: `${worktree}-sibling` });
    const r = await scan();
    assert.deepStrictEqual(r.pids, []);
    assert.strictEqual(r.complete, true, "a clean scan is a proven quiet point");
  });

  it("a working directory inside the worktree or the HOME attributes the process", async () => {
    fake(4104, { cwd: path.join(worktree, "api") });
    fake(4105, { cwd: runHome });
    const r = await scan();
    assert.deepStrictEqual(r.pids.sort(), [4104, 4105]);
  });

  it("an UNLINKED unreadable same-uid process (a non-dumpable agent of anything else) is not the run's; a vanished one is skipped", async () => {
    fake(4106, { env: "unreadable", cwd: "/tmp" });
    fake(4107, { env: ["HOME=/elsewhere"], cwd: "unreadable" });
    fs.mkdirSync(path.join(procRoot, "4108")); // exited between the directory read and the status read
    const r = await scan();
    assert.deepStrictEqual(r.pids, []);
    assert.strictEqual(r.complete, true, "an unrelated non-dumpable process does not deny the quiet point");
    assert.deepStrictEqual(r.incompleteReasons, []);
  });

  it("an unreadable process sharing a session or process group with an attributed one is the run's", async () => {
    fake(4201, { env: [`HOME=${runHome}`], session: 900, pgrp: 4201 });
    fake(4202, { env: "unreadable", cwd: "unreadable", session: 900, pgrp: 4202 });
    fake(4203, { cwd: path.join(worktree, "web"), session: 4203, pgrp: 950 });
    fake(4204, { env: "unreadable", cwd: "/tmp", session: 4204, pgrp: 950 });
    fake(4205, { env: "unreadable", cwd: "/tmp", session: 4205, pgrp: 4205 }); // unrelated
    const r = await scan();
    assert.deepStrictEqual(r.pids.sort(), [4201, 4202, 4203, 4204]);
    assert.strictEqual(r.complete, true);
  });

  it("an unreadable process whose parent chain reaches an attributed process (through another uid) or a CLI pid is the run's", async () => {
    fake(4301, { env: [`HOME=${runHome}`] });
    fake(4303, { uid: UID + 1, ppid: 4301 }); // another uid's: not judged, but its stat is walked
    fake(4302, { env: "unreadable", cwd: "unreadable", ppid: 4303 });
    fake(4304, { env: "unreadable", cwd: "unreadable", ppid: 5555 }); // 5555: the run's CLI, not listed
    fake(4305, { env: "unreadable", cwd: "unreadable", ppid: 6666 }); // another parent entirely
    const r = await scan({}, [5555]);
    assert.deepStrictEqual(r.pids.sort(), [4301, 4302, 4304]);
    const noCli = await scan();
    assert.deepStrictEqual(noCli.pids.sort(), [4301, 4302], "without the CLI pid, its unreadable child is unlinked");
  });

  it("the worker's own session and process group never link, and the parent walk stops at the worker", async () => {
    fake(50, { session: 50, pgrp: 50 }); // the worker
    fake(4501, { env: [`HOME=${runHome}`], ppid: 50, session: 50, pgrp: 50 }); // the worker's git child in the run's HOME
    fake(4502, { env: "unreadable", cwd: "unreadable", ppid: 50, session: 50, pgrp: 50 });
    const r = await scan({ workerPid: 50 });
    assert.deepStrictEqual(r.pids, [4501]);
  });

  it("another uid's process is not this pass's to judge, and a zombie holds nothing", async () => {
    fake(4109, { uid: UID + 1, env: [`HOME=${runHome}`] });
    fake(4110, { state: "Z (zombie)", env: [`HOME=${runHome}`] });
    const r = await scan();
    assert.deepStrictEqual(r.pids, []);
  });

  it("a pass that runs out of its pid budget is incomplete (unknown, so not quiet)", async () => {
    fake(4111, {});
    fake(4112, {});
    const r = await scan({ maxPids: 1 });
    assert.strictEqual(r.complete, false);
    assert.deepStrictEqual(r.incompleteReasons, ["pid_budget"]);
  });

  it("a proc mount with hidepid or subset, or one it cannot confirm, makes the scan incomplete (N-d)", async () => {
    fake(4113, { env: [`HOME=${runHome}`] });
    for (const [mountOpts, superOpts, fstype, want, reason] of [
      ["rw,relatime", "rw", "proc", true, undefined],
      ["rw,relatime", "rw,hidepid=0", "proc", true, undefined],
      ["rw,relatime", "rw,hidepid=off", "proc", true, undefined],
      ["rw,relatime", "rw,hidepid=2", "proc", false, "mount_hidden"],
      ["rw,relatime", "rw,hidepid=invisible", "proc", false, "mount_hidden"],
      ["rw,relatime,hidepid=1", "rw", "proc", false, "mount_hidden"],
      ["rw,relatime", "rw,subset=pid", "proc", false, "mount_hidden"],
      ["rw,relatime", "rw", "tmpfs", false, "mount_not_proc"],
    ] as const) {
      mountinfo(mountOpts, superOpts, fstype);
      const r = await scan();
      assert.strictEqual(r.complete, want, `${fstype} ${mountOpts} / ${superOpts}`);
      assert.deepStrictEqual(r.pids, [4113], "what it can see is still reported");
      assert.deepStrictEqual(r.incompleteReasons, reason ? [reason] : [], `${fstype} ${mountOpts} / ${superOpts}`);
    }
    const mountCase = async (label: string, reason: string) => {
      const r = await scan();
      assert.strictEqual(r.complete, false, label);
      assert.deepStrictEqual(r.pids, [4113], `${label}: what it can see is still reported`);
      assert.deepStrictEqual(r.incompleteReasons, [reason], label);
    };
    const info = path.join(procRoot, "self", "mountinfo");
    fs.rmSync(info);
    await mountCase("no mountinfo: unknown", "mount_unreadable");
    fs.writeFileSync(info, "21 1 0:5 / / rw - ext4 /dev/sda1 rw\n");
    await mountCase("no mount at the proc root: unknown", "mount_missing");
    fs.writeFileSync(info, "21 1 0:5 / / rw shared:1 ext4 /dev/sda1 rw\n");
    await mountCase("a line with no separator: malformed", "mount_malformed");
    fs.writeFileSync(info, "21 1 0:5 / - ext4 /dev/sda1 rw\n");
    await mountCase("a line with too few fields: malformed", "mount_malformed");
    fs.writeFileSync(info, "#".repeat(4 * 1024 * 1024 + 1));
    await mountCase("a mountinfo over its read bound: unknown", "mount_unreadable");
  });

  it("a helper that cannot run is incomplete, never a throw", async () => {
    const r = await scanRunProcesses(runHome, worktree, [], {
      procRoot,
      wrappers: [() => ({ command: path.join(root, "no-such-binary"), args: [] })],
    });
    assert.deepStrictEqual(r, { pids: [], complete: false, incompleteReasons: ["helper_failed"] });
  });

  it("the reap SIGKILLs an attributed process by its exact pid, and never an unreadable one, linked or not", async () => {
    const detached = (): ChildProcess => {
      const c = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
      children.push(c);
      return c;
    };
    const job = detached();
    const linkedChild = detached();
    const unrelated = detached();
    const exited = new Promise<NodeJS.Signals | null>((resolve) => job.once("exit", (_code, sig) => resolve(sig)));
    fake(job.pid!, { env: [`HOME=${runHome}`], cwd: "/tmp", session: 7000 });
    fake(linkedChild.pid!, { env: "unreadable", cwd: "/tmp", session: 7000 });
    fake(unrelated.pid!, { env: "unreadable", cwd: "/tmp" });
    const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1 });
    assert.deepStrictEqual(r.killed, [job.pid]);
    // The second round no longer sees the killed job (its fake entry stays, but kill mode re-reads
    // it; the link it gave is kept across rounds either way).
    assert.deepStrictEqual(r.left, [linkedChild.pid], "a linked unreadable process is reported, never killed");
    assert.deepStrictEqual(r.leftReasons, { [linkedChild.pid!]: "unreadable" }, "classified as unreadable");
    assert.strictEqual(await exited, "SIGKILL");
    for (const c of [linkedChild, unrelated]) {
      assert.strictEqual(c.exitCode, null);
      assert.strictEqual(c.signalCode, null);
    }
  });

  describe("managed Go work dir (GOTMPDIR) attribution", () => {
    it("a process that reset HOME but carries GOTMPDIR inside the run's leaf (or below it) is attributed", async () => {
      const leaf = `${runHome}/.uzi-go-work/g2`;
      fake(4601, { env: ["HOME=/elsewhere", `GOTMPDIR=${leaf}`], cwd: "/tmp" });
      fake(4602, { env: ["HOME=/elsewhere", `GOTMPDIR=${leaf}/go-build123`], cwd: "/tmp" });
      fake(4603, { env: [`GOTMPDIR=${leaf}`, "HOME=/elsewhere"], cwd: "/tmp" }); // entry first in environ
      const r = await scan();
      assert.deepStrictEqual(r.pids.sort(), [4601, 4602, 4603]);
      assert.strictEqual(r.complete, true);
    });

    it("lookalikes and other runs' GOTMPDIR values are not attributed", async () => {
      const other = path.join(root, "agent-home", "run-b");
      const cases: string[][] = [
        [`GOTMPDIR=${runHome}/.uzi-go-work/g2x`],
        [`GOTMPDIR=${runHome}/.uzi-go-work/gfoo`],
        [`GOTMPDIR=${runHome}/.uzi-go-work/g0`],
        [`GOTMPDIR=${runHome}/.uzi-go-work/g02`],
        [`GOTMPDIR=${runHome}/.uzi-go-workX/g2`],
        [`XGOTMPDIR=${runHome}/.uzi-go-work/g2`],
        [`GOTMPDIR=${other}/.uzi-go-work/g2`],
        [`GOTMPDIR=${runHome}/.uzi-go-work`],
        [`GOTMPDIR=${runHome}/.uzi-go-work/`],
        [`GOTMPDIR=${runHome}/.uzi-go-work-x/g1`],
        [`GOTMPDIR=${runHome}/g1/x`],
        [`GOTMPDIR=${runHome}/.uzi-go-work/g9007199254740993`], // above MAX_SAFE_INTEGER
        [`GOTMPDIR=${runHome}/.uzi-go-work/g9999999999999999`],
      ];
      cases.forEach((env, i) => fake(4700 + i, { env: ["HOME=/elsewhere", ...env], cwd: "/tmp" }));
      const r = await scan();
      assert.deepStrictEqual(r.pids, []);
      assert.strictEqual(r.complete, true);
    });

    it("the largest safe-integer generation is still attributed", async () => {
      fake(4650, { env: ["HOME=/elsewhere", `GOTMPDIR=${runHome}/.uzi-go-work/g9007199254740991`], cwd: "/tmp" });
      const r = await scan();
      assert.deepStrictEqual(r.pids, [4650]);
    });

    it("the reap does not kill a process whose GOTMPDIR generation is not a safe integer", async () => {
      const cs = [spawn("sleep", ["30"], { detached: true, stdio: "ignore" }), spawn("sleep", ["30"], { detached: true, stdio: "ignore" })];
      children.push(...cs);
      fake(cs[0]!.pid!, { env: ["HOME=/elsewhere", `GOTMPDIR=${runHome}/.uzi-go-work/g9007199254740993`], cwd: "/tmp" });
      fake(cs[1]!.pid!, { env: ["HOME=/elsewhere", `GOTMPDIR=${runHome}/.uzi-go-work/g9999999999999999/go-build1`], cwd: "/tmp" });
      const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1 });
      assert.deepStrictEqual(r.killed, []);
      for (const c of cs) {
        assert.strictEqual(c.exitCode, null);
        assert.strictEqual(c.signalCode, null);
      }
    });

    it("the reap SIGKILLs a GOTMPDIR-attributed process", async () => {
      const c = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
      children.push(c);
      const exited = new Promise<NodeJS.Signals | null>((resolve) => c.once("exit", (_code, sig) => resolve(sig)));
      fake(c.pid!, { env: ["HOME=/elsewhere", `GOTMPDIR=${runHome}/.uzi-go-work/g2/go-build123`], cwd: "/tmp" });
      const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1 });
      assert.deepStrictEqual(r.killed, [c.pid]);
      assert.strictEqual(await exited, "SIGKILL");
    });
  });

  it("the reap does not kill a pid whose start time changed between the round and the kill (pid reuse, N-c)", async () => {
    const c = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
    children.push(c);
    const pid = c.pid!;
    fake(pid, { env: [`HOME=${runHome}`], cwd: "/tmp", start: 1000 });
    // The helper's second open of this pid's stat (the re-verify right before the kill) sees a new
    // start time: the pid was recycled between the round and the kill. A --require preload in the
    // helper process rewrites the file at exactly that open.
    const statPath = path.join(procRoot, String(pid), "stat");
    const preload = path.join(root, "recycle.cjs");
    fs.writeFileSync(
      preload,
      `const fs = require("node:fs");
const orig = fs.openSync;
let opens = 0, busy = false;
fs.openSync = function (p, ...rest) {
  if (p === ${JSON.stringify(statPath)} && !busy) {
    opens++;
    if (opens === 2) { busy = true; fs.writeFileSync(p, ${JSON.stringify(statLine(pid, { start: 2000 }))}); busy = false; }
  }
  return orig.call(this, p, ...rest);
};
`,
    );
    const recycling: CommandWrapper = (command, args) => ({ command, args: ["--require", preload, ...args] });
    const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: [recycling] });
    assert.deepStrictEqual(r.killed, [], "a recycled pid is never killed");
    assert.deepStrictEqual(r.left, [pid], "it is reported as unresolved instead");
    assert.deepStrictEqual(r.leftReasons, { [pid]: "restarted" }, "a changed start time is classified restarted");
    assert.ok(fs.readFileSync(statPath, "utf8").includes(" 2000 "), "precondition: the start time did change mid-reap");
    assert.strictEqual(c.exitCode, null);
    assert.strictEqual(c.signalCode, null);
    // Control: with no recycling the same process IS killed.
    fs.writeFileSync(statPath, statLine(pid, { start: 1000 }));
    const control = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1 });
    assert.deepStrictEqual(control.killed, [pid]);
  });

  it("the reap never kills a direct child of the worker (its own git or helper)", async () => {
    const c = spawn("sleep", ["30"], { stdio: "ignore" });
    children.push(c);
    fake(c.pid!, { ppid: 777, env: [`HOME=${runHome}`] });
    const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 777 });
    assert.deepStrictEqual(r.killed, []);
    assert.strictEqual(c.exitCode, null);
    assert.deepStrictEqual(r.left, [c.pid], "the worker's child survives the reap");
    assert.deepStrictEqual(r.leftReasons, { [c.pid!]: "worker_child" }, "classified worker_child, not by its attribution");
  });

  it("a SIGKILL that fails other than ESRCH leaves the pid classified kill_failed", async () => {
    const c = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
    children.push(c);
    const pid = c.pid!;
    fake(pid, { env: [`HOME=${runHome}`], cwd: "/tmp" });
    // The helper never really signals: a preload makes its process.kill fail with EPERM.
    const preload = path.join(root, "eperm.cjs");
    fs.writeFileSync(
      preload,
      `const orig = process.kill;
process.kill = function (pid, sig) {
  if (sig === "SIGKILL") { const e = new Error("operation not permitted"); e.code = "EPERM"; throw e; }
  return orig.call(this, pid, sig);
};
`,
    );
    const denying: CommandWrapper = (command, args) => ({ command, args: ["--require", preload, ...args] });
    const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: [denying] });
    assert.deepStrictEqual(r.killed, []);
    assert.deepStrictEqual(r.left, [pid]);
    assert.deepStrictEqual(r.leftReasons, { [pid]: "kill_failed" });
    assert.strictEqual(c.exitCode, null);
    assert.strictEqual(c.signalCode, null);
  });

  it("a survivor class the helper does not define is dropped from the reason map", async () => {
    const script = path.join(root, "bogus-class.cjs");
    fs.writeFileSync(
      script,
      'process.stdout.write("A\\t4242\\tbogus\\nA\\t4243\\trestarted\\nH\\t" + JSON.stringify({ scanned: 2, truncated: false, unresolved: 0, procMount: "ok" }) + "\\n");\n',
    );
    const r = await reapRunProcesses(runHome, worktree, [], {
      procRoot,
      wrappers: [() => ({ command: process.execPath, args: [script] })],
    });
    assert.deepStrictEqual(r.left.sort(), [4242, 4243]);
    assert.deepStrictEqual(r.leftReasons, { 4243: "restarted" });
  });

  it("a helper that cannot be started reads as incomplete with an empty reason map", async () => {
    const r = await reapRunProcesses(runHome, worktree, [], {
      procRoot,
      wrappers: [() => ({ command: path.join(root, "no-such-binary"), args: [] })],
    });
    assert.deepStrictEqual(r, { killed: [], left: [], leftReasons: {}, complete: false, incompleteReasons: ["helper_failed"] });
  });

  it("this host's real proc tree: a detached child with the run's HOME is found and reaped; another HOME is not", { skip: realTreeSkip("real proc tree: detached child with the run's HOME") }, async () => {
    const mine = spawn("sleep", ["30"], { detached: true, stdio: "ignore", env: { PATH: process.env.PATH, HOME: runHome } });
    const other = spawn("sleep", ["30"], { detached: true, stdio: "ignore", env: { PATH: process.env.PATH, HOME: path.join(root, "x") } });
    children.push(mine, other);
    await new Promise((r) => setTimeout(r, 100)); // let both exec
    const found = await scanRunProcesses(runHome, undefined);
    assert.ok(found.pids.includes(mine.pid!), `the run's detached child is attributed (${JSON.stringify(found)})`);
    assert.ok(!found.pids.includes(other.pid!), `another HOME is not (${JSON.stringify(found)})`);
    assert.strictEqual(found.complete, true, `the real scan completed, so the negative above is proven (${JSON.stringify(found)})`);
    // The test process is the "worker" here and both children are its direct children, so name
    // another worker pid for the reap.
    const exited = new Promise<NodeJS.Signals | null>((resolve) => mine.once("exit", (_c, sig) => resolve(sig)));
    const reaped = await reapRunProcesses(runHome, undefined, [], { workerPid: 0 });
    assert.ok(reaped.killed.includes(mine.pid!), `the reap killed the run's child (${JSON.stringify(reaped)})`);
    assert.ok(!reaped.killed.includes(other.pid!), `the reap spared another HOME (${JSON.stringify(reaped)})`);
    assert.strictEqual(await exited, "SIGKILL");
  });

  it("this host's real proc tree: an unrelated NON-DUMPABLE process does not make the run busy", { skip: realTreeSkip("real proc tree: unrelated NON-DUMPABLE process") }, async (t) => {
    const py = python();
    if (!py) return t.skip("no python3 with ctypes here to call prctl(PR_SET_DUMPABLE, 0)");
    const pidFile = path.join(root, "nd.pid");
    const nd = spawn(py, ["-c", NONDUMPABLE, pidFile], {
      detached: true,
      stdio: "ignore",
      cwd: "/tmp",
      env: { PATH: process.env.PATH, HOME: path.join(root, "unrelated") },
    });
    children.push(nd);
    assert.strictEqual(await pidFrom(pidFile), nd.pid);
    if (!environUnreadable(nd.pid!)) return t.skip("a non-dumpable process's environ is readable by this uid here");
    const found = await scanRunProcesses(runHome, worktree);
    assert.ok(!found.pids.includes(nd.pid!), `the unrelated non-dumpable process is not the run's (${JSON.stringify(found)})`);
    assert.deepStrictEqual(found.pids, [], `nothing of this run is alive (${JSON.stringify(found)})`);
    assert.strictEqual(found.complete, true, `the real scan completed: a scan that failed outright is not a quiet point (${JSON.stringify(found)})`);
  });

  it("this host's real proc tree: a NON-DUMPABLE child of an attributed process IS the run's", { skip: realTreeSkip("real proc tree: NON-DUMPABLE child of an attributed process") }, async (t) => {
    const py = python();
    if (!py) return t.skip("no python3 with ctypes here to call prctl(PR_SET_DUMPABLE, 0)");
    const pidFile = path.join(root, "child.pid");
    // A detached shell of the run (HOME = the run's), whose child makes itself non-dumpable.
    const sh = spawn("sh", ["-c", '"$0" -c "$1" "$2" & wait', py, NONDUMPABLE, pidFile], {
      detached: true,
      stdio: "ignore",
      cwd: "/tmp",
      env: { PATH: process.env.PATH, HOME: runHome },
    });
    children.push(sh);
    const child = await pidFrom(pidFile);
    strays.push(child);
    if (!environUnreadable(child)) return t.skip("a non-dumpable process's environ is readable by this uid here");
    const found = await scanRunProcesses(runHome, worktree);
    assert.ok(found.pids.includes(sh.pid!), `the run's shell is attributed by HOME (${JSON.stringify(found)})`);
    assert.ok(found.pids.includes(child), `its non-dumpable child is linked by ppid/session (${JSON.stringify(found)})`);
  });
});

// issue #2953: an incomplete scan names WHICH existing condition made it so, from a fixed vocabulary
// (diagnostics only: the reasons never change `complete`, the pids, or the reap).
describe("run process scan incompleteness reasons (issue #2953)", () => {
  /** A wrapper that loads `preload` into the helper (node --require). */
  const preloading = (name: string, body: string): CommandWrapper => {
    const preload = path.join(root, name);
    fs.writeFileSync(preload, body);
    return (command, args) => ({ command, args: ["--require", preload, ...args] });
  };
  /** A fake helper that prints `out` and exits `code`. */
  const fakeHelper = (name: string, out: string, code = 0): CommandWrapper => {
    const script = path.join(root, name);
    fs.writeFileSync(script, `process.stdout.write(${JSON.stringify(out)}); process.exitCode = ${code};\n`);
    return () => ({ command: process.execPath, args: [script] });
  };
  const summary = (fields: Record<string, unknown>) =>
    `H\t${JSON.stringify({ scanned: 1, truncated: false, unresolved: 0, procMount: "ok", ...fields })}\n`;
  /** The invariant the real functions keep: incomplete exactly when a reason is named. */
  const consistent = (r: { complete: boolean; incompleteReasons?: string[] }) => {
    assert.ok(Array.isArray(r.incompleteReasons), "the real functions always set the reasons");
    assert.strictEqual(r.complete, r.incompleteReasons.length === 0, `complete iff no reason (${JSON.stringify(r)})`);
    for (const reason of r.incompleteReasons) assert.ok((INCOMPLETE_REASONS as readonly string[]).includes(reason), reason);
  };

  it("a pass that runs out of its time budget is incomplete with time_budget", async () => {
    fake(4601, {});
    // The helper reads the clock once for its deadline; every later read is far past it.
    const late = preloading("late.cjs", "const real = Date.now; let n = 0; Date.now = () => (n++ === 0 ? real() : real() + 1e9);\n");
    const r = await scan({ wrappers: [late] });
    consistent(r);
    assert.deepStrictEqual(r.incompleteReasons, ["time_budget"]);
  });

  it("a directory read that fails mid-listing is incomplete with dir_read", async () => {
    fake(4602, {});
    const failing = preloading(
      "readdir.cjs",
      'const fs = require("node:fs"); fs.Dir.prototype.readSync = function () { const e = new Error("io"); e.code = "EIO"; throw e; };\n',
    );
    const r = await scan({ wrappers: [failing] });
    consistent(r);
    assert.deepStrictEqual(r.incompleteReasons, ["dir_read"]);
  });

  it("a pid whose status or stat cannot be read, while it still exists, names which read failed", async () => {
    fake(4603, {});
    fake(4604, {});
    const denied = [path.join(procRoot, "4603", "status"), path.join(procRoot, "4604", "stat")];
    const deny = preloading(
      "eacces.cjs",
      `const fs = require("node:fs"); const orig = fs.openSync; const denied = ${JSON.stringify(denied)};
fs.openSync = function (p, ...rest) { if (denied.includes(p)) { const e = new Error("denied"); e.code = "EACCES"; throw e; } return orig.call(this, p, ...rest); };
`,
    );
    const r = await scan({ wrappers: [deny] });
    consistent(r);
    assert.deepStrictEqual(r.incompleteReasons, ["status_read", "stat_read"]);
  });

  it("an oversized status or stat, a status with no Uid line, and a malformed stat each name their cause", async () => {
    for (const [label, plant, reason] of [
      ["oversized status", (dir: string) => fs.appendFileSync(path.join(dir, "status"), "x".repeat(70_000)), "status_oversize"],
      ["oversized stat", (dir: string) => fs.writeFileSync(path.join(dir, "stat"), "x".repeat(70_000)), "stat_oversize"],
      ["no Uid line", (dir: string) => fs.writeFileSync(path.join(dir, "status"), "Name:\tgo\nState:\tS (sleeping)\n"), "uid_missing"],
      ["malformed stat", (dir: string) => fs.writeFileSync(path.join(dir, "stat"), "4605 (go) S 1 2\n"), "stat_malformed"],
    ] as const) {
      fs.rmSync(path.join(procRoot, "4605"), { recursive: true, force: true });
      fake(4605, { env: [`HOME=${runHome}`] });
      plant(path.join(procRoot, "4605"));
      const r = await scan();
      consistent(r);
      assert.deepStrictEqual(r.pids, [], `${label}: an unresolved pid is not attributed`);
      assert.deepStrictEqual(r.incompleteReasons, [reason], label);
    }
  });

  it("the kill pass's re-verify uncertainty is a survivor, never an incompleteness reason; a separately unresolved pid still is", async () => {
    // The re-verify's status read (the second open of this pid's status) is oversized: the pid
    // stays as an unreadable survivor, and the scan itself stays complete.
    const statusPath = path.join(procRoot, "4801", "status");
    const corrupt = preloading(
      "reverify.cjs",
      `const fs = require("node:fs"); const orig = fs.openSync; let opens = 0, busy = false;
fs.openSync = function (p, ...rest) {
  if (p === ${JSON.stringify(statusPath)} && !busy) { opens++; if (opens === 2) { busy = true; fs.appendFileSync(p, "x".repeat(70000)); busy = false; } }
  return orig.call(this, p, ...rest);
};
`,
    );
    fake(4801, { env: [`HOME=${runHome}`] });
    const alone = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: [corrupt] });
    consistent(alone);
    assert.deepStrictEqual(alone.killed, []);
    assert.deepStrictEqual(alone.left, [4801]);
    assert.deepStrictEqual(alone.leftReasons, { 4801: "unreadable" });
    assert.strictEqual(alone.complete, true, "the re-verify does not change completeness");
    assert.deepStrictEqual(alone.incompleteReasons, []);

    fs.rmSync(path.join(procRoot, "4801"), { recursive: true });
    fake(4801, { env: [`HOME=${runHome}`] });
    fake(4802, {});
    fs.writeFileSync(path.join(procRoot, "4802", "status"), "Name:\tgo\nState:\tS (sleeping)\n");
    const both = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: [corrupt] });
    consistent(both);
    assert.deepStrictEqual(both.left, [4801]);
    assert.deepStrictEqual(both.leftReasons, { 4801: "unreadable" });
    assert.strictEqual(both.complete, false);
    assert.deepStrictEqual(both.incompleteReasons, ["uid_missing"], "only the listing's unresolved pid names a reason");
  });

  it("a kill pass that exhausts its pid budget on a later round is incomplete with pid_budget", async () => {
    const c = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
    children.push(c);
    fake(c.pid!, { env: [`HOME=${runHome}`], cwd: "/tmp" });
    // Round 1 looks at its one pid and kills it; round 2's first pid is over the budget.
    const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, maxPids: 1 });
    consistent(r);
    assert.deepStrictEqual(r.killed, [c.pid]);
    assert.strictEqual(r.complete, false);
    assert.deepStrictEqual(r.incompleteReasons, ["pid_budget"]);
  });

  it("helper exits name their cause: an unreadable proc root, bad arguments, any other exit", async () => {
    const missing = await scanRunProcesses(runHome, worktree, [], { procRoot: path.join(root, "no-proc"), workerPid: 1 });
    consistent(missing);
    assert.deepStrictEqual(missing, { pids: [], complete: false, incompleteReasons: ["proc_root_unreadable"] });
    const relative = await scanRunProcesses(runHome, worktree, [], { procRoot: "relative/proc", workerPid: 1 });
    assert.deepStrictEqual(relative, { pids: [], complete: false, incompleteReasons: ["helper_bad_arguments"] });
    const other = await scan({ wrappers: [fakeHelper("exit3.cjs", summary({}), 3)] });
    assert.deepStrictEqual(other, { pids: [], complete: false, incompleteReasons: ["helper_exit"] });
  });

  it("malformed helper output names its shape", async () => {
    for (const [out, reason] of [
      ["A\t4242\thome\n", "no_summary"],
      ["H\t{not json\n", "malformed_summary"],
      [`A\tx\thome\n${summary({})}`, "malformed_line"],
      [`Z\t4242\thome\n${summary({})}`, "unknown_record"],
    ] as const) {
      const r = await scan({ wrappers: [fakeHelper("out.cjs", out)] });
      consistent(r);
      assert.strictEqual(r.complete, false, reason);
      assert.deepStrictEqual(r.incompleteReasons, [reason], reason);
    }
  });

  it("summary reasons: a flag with no explaining token falls back; a clean flag's token and unknown tokens are dropped", async () => {
    const old = await scan({ wrappers: [fakeHelper("old.cjs", summary({ truncated: true, unresolved: 2, procMount: "hidden" }))] });
    consistent(old);
    assert.deepStrictEqual(old.incompleteReasons, ["truncated", "unresolved", "mount_unknown"], "an older helper with no why");
    const stray = await scan({ wrappers: [fakeHelper("stray.cjs", `A\t4242\thome\n${summary({ why: ["pid_budget", "mount_hidden", "uid_missing"] })}`)] });
    assert.deepStrictEqual(stray, { pids: [4242], complete: true, incompleteReasons: [] }, "tokens for clean flags never make a scan incomplete");
    const foreign = await scan({ wrappers: [fakeHelper("foreign.cjs", summary({ truncated: true, why: ["/home/u/.ssh/id_rsa", "HOME=/x", 7, "pid_budget"] }))] });
    assert.deepStrictEqual(foreign.incompleteReasons, ["pid_budget"], "only allowlisted tokens pass");
    const onlyForeign = await scan({ wrappers: [fakeHelper("foreign2.cjs", summary({ unresolved: 1, why: ["secret text"] }))] });
    assert.deepStrictEqual(onlyForeign.incompleteReasons, ["unresolved"], "an unexplained flag falls back");
  });

  it("a hostile helper summary with a huge why array neither throws nor drops what other passes found", async () => {
    const clean = fakeHelper("clean.cjs", `K\t4242\thome\nA\t4343\tunreadable\n${summary({})}`);
    const huge = fakeHelper("huge.cjs", summary({ truncated: true, why: Array.from({ length: 200_000 }, () => "pid_budget") }));
    const r = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: [clean, huge] });
    consistent(r);
    assert.deepStrictEqual(r, { killed: [4242], left: [4343], leftReasons: { 4343: "unreadable" }, complete: false, incompleteReasons: ["pid_budget"] });
  });

  it("reasons aggregate across passes in canonical order, and survive a later throw", async () => {
    const exit7 = fakeHelper("e7.cjs", "", 7);
    const budgetOut = fakeHelper("pb.cjs", summary({ truncated: true, why: ["pid_budget"] }));
    const r = await scan({ wrappers: [exit7, budgetOut] });
    consistent(r);
    assert.deepStrictEqual(r.incompleteReasons, ["pid_budget", "proc_root_unreadable"]);

    const throwing = { *[Symbol.iterator]() { yield exit7; throw new Error("boom"); } } as unknown as CommandWrapper[];
    const thrown = await scan({ wrappers: throwing });
    consistent(thrown);
    assert.deepStrictEqual(thrown, { pids: [], complete: false, incompleteReasons: ["proc_root_unreadable", "scan_exception"] });
    const thrownReap = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: throwing });
    assert.deepStrictEqual(thrownReap, { killed: [], left: [], leftReasons: {}, complete: false, incompleteReasons: ["proc_root_unreadable", "scan_exception"] });
  });

  it("a maintenance budget that runs out between passes reads maintenance_budget, keeping earlier reasons", async () => {
    let now = 0;
    const budget = new MaintenanceBudget(60_000, () => now);
    const exit7 = fakeHelper("e7b.cjs", "", 7);
    const expiring = { *[Symbol.iterator]() { yield exit7; now = 120_000; yield exit7; } } as unknown as CommandWrapper[];
    const r = await MaintenanceBudget.run(budget, () => scan({ wrappers: expiring }));
    consistent(r);
    assert.deepStrictEqual(r, { pids: [], complete: false, incompleteReasons: ["proc_root_unreadable", "maintenance_budget"] });
  });

  it("helper rejections map by their exact fixed messages, never echoing the text", () => {
    for (const [msg, reason] of [
      ["agent-uid helper timed out (killed by SIGKILL)", "helper_timeout"],
      ["agent-uid helper timed out (killed by a signal)", "helper_timeout"],
      ["maintenance helper operation expired", "helper_timeout"],
      ["maintenance helper timed out", "helper_timeout"],
      ["agent-uid helper printed more than 1048576 bytes", "helper_output_overflow"],
      ["agent-uid helper timed out or could not run", "helper_failed"],
      ["agent-uid helper could not run or was not waited for (ENOENT)", "helper_failed"],
      ["agent-uid helper ended by SIGTERM", "helper_failed"],
      ["maintenance helper cancelled", "helper_failed"],
      ["maintenance deadline exhausted", "helper_failed"],
    ] as const) assert.strictEqual(helperRejectionReason(new Error(msg)), reason, msg);
    assert.strictEqual(helperRejectionReason("not an error"), "helper_failed");
  });
});

describe("run process passes with a gate deadline (issue #3980)", () => {
  /** A fake helper that prints `out` and exits 0, counting how many times it was wrapped (admitted). */
  const countingHelper = (name: string, out: string, seen: { args: string[][] }): CommandWrapper => {
    const script = path.join(root, name);
    fs.writeFileSync(script, `process.stdout.write(${JSON.stringify(out)});\n`);
    return (_command, args) => { seen.args.push([...args]); return { command: process.execPath, args: [script] }; };
  };
  const summary = `H\t${JSON.stringify({ scanned: 1, truncated: false, unresolved: 0, procMount: "ok", why: [] })}\n`;
  /** The pass duration the worker passed the helper: argv after `-e <script>` is mode, proc root, home, worktree, pids, duration. */
  const durationOf = (args: string[]): number => Number(args[2 + 5]);

  it("a pass is admitted only while time is left: past the deadline no helper is wrapped and the scan is unknown", async () => {
    const seen = { args: [] as string[][] };
    const r = await scan({ wrappers: [countingHelper("late-a.cjs", summary, seen)], deadlineAt: Date.now() - 1 });
    assert.deepStrictEqual(seen.args, []);
    assert.strictEqual(r.complete, false);
    assert.deepStrictEqual(r.incompleteReasons, ["time_budget"]);
    const reaped = await reapRunProcesses(runHome, worktree, [], { procRoot, workerPid: 1, wrappers: [countingHelper("late-b.cjs", summary, seen)], deadlineAt: Date.now() - 1 });
    assert.deepStrictEqual(seen.args, []);
    assert.strictEqual(reaped.complete, false);
  });

  it("a pass's budget is the time left, not its own 10 s / 20 s default", async () => {
    const seen = { args: [] as string[][] };
    const deadlineAt = Date.now() + 700;
    const r = await scan({ wrappers: [countingHelper("cap.cjs", summary, seen)], deadlineAt });
    assert.strictEqual(r.complete, true);
    assert.strictEqual(seen.args.length, 1);
    assert.ok(durationOf(seen.args[0]!) <= 700 && durationOf(seen.args[0]!) > 0, `duration ${durationOf(seen.args[0]!)}`);
    const plain = { args: [] as string[][] };
    await scan({ wrappers: [countingHelper("nocap.cjs", summary, plain)] });
    assert.strictEqual(durationOf(plain.args[0]!), 10_000, "without a deadline the default budget is unchanged");
  });

  it("a hung first pass is killed at the deadline, joined, and the second uid's pass is not admitted", async () => {
    const hang = path.join(root, "hang.cjs");
    fs.writeFileSync(hang, "setInterval(() => {}, 1000);\n");
    const hanging: CommandWrapper = () => ({ command: process.execPath, args: [hang] });
    const seen = { args: [] as string[][] };
    const started = Date.now();
    const r = await scan({ wrappers: [hanging, countingHelper("second.cjs", summary, seen)], deadlineAt: started + 400 });
    const took = Date.now() - started;
    assert.strictEqual(r.complete, false);
    assert.deepStrictEqual(r.incompleteReasons, ["time_budget", "helper_timeout"].filter((x) => r.incompleteReasons?.includes(x as never)));
    assert.ok(r.incompleteReasons?.includes("helper_timeout"));
    assert.deepStrictEqual(seen.args, [], "the later pass was not admitted after the deadline");
    assert.ok(took >= 350 && took < 4000, `bounded by the deadline, not the helper's 15 s: ${took} ms`);
  });

  it("with time left every pass runs as before", async () => {
    fake(4701, { cwd: path.join(worktree, "src") });
    const a = await scan();
    const b = await scan({ deadlineAt: Date.now() + 60_000 });
    assert.deepStrictEqual(b.pids, a.pids);
    assert.strictEqual(b.complete, a.complete);
  });
});
