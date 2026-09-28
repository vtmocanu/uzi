import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type RunProcsOptions, reapRunProcesses, scanRunProcesses } from "../src/run-procs.js";
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
  });

  it("a proc mount with hidepid or subset, or one it cannot confirm, makes the scan incomplete (N-d)", async () => {
    fake(4113, { env: [`HOME=${runHome}`] });
    for (const [mountOpts, superOpts, fstype, want] of [
      ["rw,relatime", "rw", "proc", true],
      ["rw,relatime", "rw,hidepid=0", "proc", true],
      ["rw,relatime", "rw,hidepid=off", "proc", true],
      ["rw,relatime", "rw,hidepid=2", "proc", false],
      ["rw,relatime", "rw,hidepid=invisible", "proc", false],
      ["rw,relatime,hidepid=1", "rw", "proc", false],
      ["rw,relatime", "rw,subset=pid", "proc", false],
      ["rw,relatime", "rw", "tmpfs", false],
    ] as const) {
      mountinfo(mountOpts, superOpts, fstype);
      const r = await scan();
      assert.strictEqual(r.complete, want, `${fstype} ${mountOpts} / ${superOpts}`);
      assert.deepStrictEqual(r.pids, [4113], "what it can see is still reported");
    }
    fs.rmSync(path.join(procRoot, "self", "mountinfo"));
    assert.strictEqual((await scan()).complete, false, "no mountinfo: unknown");
    fs.writeFileSync(path.join(procRoot, "self", "mountinfo"), "21 1 0:5 / / rw - ext4 /dev/sda1 rw\n");
    assert.strictEqual((await scan()).complete, false, "no mount at the proc root: unknown");
  });

  it("a helper that cannot run is incomplete, never a throw", async () => {
    const r = await scanRunProcesses(runHome, worktree, [], {
      procRoot,
      wrappers: [() => ({ command: path.join(root, "no-such-binary"), args: [] })],
    });
    assert.deepStrictEqual(r, { pids: [], complete: false });
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
    assert.strictEqual(await exited, "SIGKILL");
    for (const c of [linkedChild, unrelated]) {
      assert.strictEqual(c.exitCode, null);
      assert.strictEqual(c.signalCode, null);
    }
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
  });

  it("this host's real proc tree: a detached child with the run's HOME is found and reaped; another HOME is not", { skip: realTreeSkip("real proc tree: detached child with the run's HOME") }, async () => {
    const mine = spawn("sleep", ["30"], { detached: true, stdio: "ignore", env: { PATH: process.env.PATH, HOME: runHome } });
    const other = spawn("sleep", ["30"], { detached: true, stdio: "ignore", env: { PATH: process.env.PATH, HOME: path.join(root, "x") } });
    children.push(mine, other);
    await new Promise((r) => setTimeout(r, 100)); // let both exec
    const found = await scanRunProcesses(runHome, undefined);
    assert.ok(found.pids.includes(mine.pid!), `the run's detached child is attributed (${JSON.stringify(found)})`);
    assert.ok(!found.pids.includes(other.pid!), "another HOME is not");
    // The test process is the "worker" here and both children are its direct children, so name
    // another worker pid for the reap.
    const exited = new Promise<NodeJS.Signals | null>((resolve) => mine.once("exit", (_c, sig) => resolve(sig)));
    const reaped = await reapRunProcesses(runHome, undefined, [], { workerPid: 0 });
    assert.ok(reaped.killed.includes(mine.pid!));
    assert.ok(!reaped.killed.includes(other.pid!));
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
    assert.deepStrictEqual(found.pids, [], "nothing of this run is alive");
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
