import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { type ChildProcess, spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { reapRunProcesses, scanRunProcesses } from "../src/run-procs.js";

// PRD #1809 D4: process ATTRIBUTION by environment. The Claude CLI spawns every Bash command
// detached (a new session and process group), so the run's processes are found by `HOME=<runHome>`
// in their environment or a working directory inside the run's trees. Seams: a fake proc root (the
// same layout: <pid>/status, stat, environ, cwd); one test reads this host's real one.

const UID = process.getuid?.() ?? 0;
const HAS_PROC = fs.existsSync(`/${"proc"}/self/status`);

let root: string;
let procRoot: string;
let runHome: string;
let worktree: string;
const children: ChildProcess[] = [];

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-procs-"));
  procRoot = path.join(root, "proc");
  runHome = path.join(root, "agent-home", "run-a");
  worktree = path.join(root, "runner", "issue-1");
  for (const d of [procRoot, runHome, worktree]) fs.mkdirSync(d, { recursive: true });
});

afterEach(() => {
  for (const c of children.splice(0)) {
    if (c.exitCode === null && c.signalCode === null) c.kill("SIGKILL");
  }
  fs.rmSync(root, { recursive: true, force: true });
});

interface FakeProc {
  uid?: number;
  ppid?: number;
  state?: string;
  start?: number;
  env?: string[] | "unreadable";
  cwd?: string | "unreadable";
}

/** One fake `<procRoot>/<pid>` entry, laid out as the kernel's. */
function fake(pid: number, p: FakeProc): void {
  const dir = path.join(procRoot, String(pid));
  fs.mkdirSync(dir);
  const uid = p.uid ?? UID;
  fs.writeFileSync(path.join(dir, "status"), `Name:\tgo\nState:\t${p.state ?? "S (sleeping)"}\nPid:\t${pid}\nUid:\t${uid}\t${uid}\t${uid}\t${uid}\n`);
  // Fields after the comm: state ppid pgrp session ... starttime is the 22nd field overall.
  const rest = [p.state?.[0] ?? "S", p.ppid ?? 2, pid, pid, 0, -1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 20, 0, 1, 0, p.start ?? 12345, 0];
  fs.writeFileSync(path.join(dir, "stat"), `${pid} (go test) ${rest.join(" ")}\n`);
  if (p.env === "unreadable") fs.mkdirSync(path.join(dir, "environ"));
  else fs.writeFileSync(path.join(dir, "environ"), Buffer.from((p.env ?? ["PATH=/usr/bin"]).map((e) => `${e}\0`).join("")));
  if (p.cwd !== "unreadable") fs.symlinkSync(p.cwd ?? "/", path.join(dir, "cwd"));
}

const scan = (opts: { maxPids?: number } = {}) => scanRunProcesses(runHome, worktree, { procRoot, workerPid: 1, ...opts });

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

  it("an unreadable same-uid process counts as alive (fail closed); a vanished one is skipped", async () => {
    fake(4106, { env: "unreadable", cwd: "/tmp" });
    fake(4107, { env: ["HOME=/elsewhere"], cwd: "unreadable" });
    fs.mkdirSync(path.join(procRoot, "4108")); // exited between the directory read and the status read
    const r = await scan();
    assert.deepStrictEqual(r.pids.sort(), [4106, 4107]);
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

  it("a helper that cannot run is incomplete, never a throw", async () => {
    const r = await scanRunProcesses(runHome, worktree, {
      procRoot,
      wrappers: [() => ({ command: path.join(root, "no-such-binary"), args: [] })],
    });
    assert.deepStrictEqual(r, { pids: [], complete: false });
  });

  it("the reap SIGKILLs an attributed process by its exact pid, and never an unreadable one", async () => {
    const detached = (): ChildProcess => {
      const c = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
      children.push(c);
      return c;
    };
    const job = detached();
    const unknown = detached();
    const exited = new Promise<NodeJS.Signals | null>((resolve) => job.once("exit", (_code, sig) => resolve(sig)));
    fake(job.pid!, { env: [`HOME=${runHome}`], cwd: "/tmp" });
    fake(unknown.pid!, { env: "unreadable", cwd: "/tmp" });
    const r = await reapRunProcesses(runHome, worktree, { procRoot, workerPid: 1 });
    assert.deepStrictEqual(r.killed, [job.pid]);
    assert.deepStrictEqual(r.left, [unknown.pid], "an unreadable process is reported, never killed");
    assert.strictEqual(await exited, "SIGKILL");
    assert.strictEqual(unknown.exitCode, null);
    assert.strictEqual(unknown.signalCode, null);
  });

  it("the reap never kills a direct child of the worker (its own git or helper)", async () => {
    const c = spawn("sleep", ["30"], { stdio: "ignore" });
    children.push(c);
    fake(c.pid!, { ppid: 777, env: [`HOME=${runHome}`] });
    const r = await reapRunProcesses(runHome, worktree, { procRoot, workerPid: 777 });
    assert.deepStrictEqual(r.killed, []);
    assert.strictEqual(c.exitCode, null);
  });

  it("this host's real proc tree: a detached child with the run's HOME is found and reaped; another HOME is not", async (t) => {
    if (!HAS_PROC) return t.skip("no proc filesystem here");
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
    const reaped = await reapRunProcesses(runHome, undefined, { workerPid: 0 });
    assert.ok(reaped.killed.includes(mine.pid!));
    assert.ok(!reaped.killed.includes(other.pid!));
    assert.strictEqual(await exited, "SIGKILL");
  });
});
