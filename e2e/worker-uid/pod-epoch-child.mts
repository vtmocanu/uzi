// Image-only owned Node process. Classification is the ONLY fake: tmpfs is not
// reclaimed in production. Every descriptor, flock, fsync and UID here is real.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createRequire } from "node:module";
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";
import { setprivArgsForUid, runnerCommand, RUNNER_UID } from "../../agent/src/runner-uid.js";
import { restoreEpochRuntimeTmpdir } from "../../agent/src/pod-epoch-lock.js";

const require = createRequire(import.meta.url);
const { createRuntime } = require("../../agent/templates/pod-epoch-init.cjs");
const root = process.argv[2]!;
const io = { ...fs, statfsSync: (...args: Parameters<typeof fs.statfsSync>) =>
  ({ ...fs.statfsSync(...args), type: 0xef53 }) };
const runtime = createRuntime({ root, io, uid: 10001, gid: 10001 });
let ownership: Awaited<ReturnType<typeof runtime.acquire>> | undefined;
const lockFile = path.join(root, ".uzi-epoch.lock");
// Reports which of this process's own descriptors alias the epoch lock inode. The
// image proof lacks CAP_SYS_PTRACE, so the holder must self-report rather than be
// inspected through /proc from the root test process.
const SCAN = `const fs=require('node:fs');
const t=fs.statSync(process.argv[1]);
const holds=fs.readdirSync('/proc/self/fd').filter(fd=>{
  try{const s=fs.statSync('/proc/self/fd/'+fd);return s.ino===t.ino&&s.dev===t.dev;}catch{return false;}});`;
const DETECT = SCAN + "console.log(JSON.stringify(holds));";
// Holds until the release file exists. The root test process cannot signal a worker-uid process.
const HOLD = SCAN + `process.stdout.write(JSON.stringify({pid:process.pid,holds})+'\\n');
setInterval(()=>{if(fs.existsSync(process.argv[2])||!fs.existsSync(process.argv[1]))process.exit(0);},50);`;
// Starts HOLD as a grandchild that inherits the mutation descriptor from this middle process, then exits.
const MIDDLE = `const c=require('node:child_process').spawn(process.execPath,['-e',${JSON.stringify(HOLD)},process.argv[1],process.argv[2]],
  {stdio:['ignore','inherit','ignore',3]});c.unref();`;
function firstLine(child: ChildProcess): Promise<any> {
  return new Promise((resolve, reject) => {
    let buffer = "";
    const timer = setTimeout(() => reject(new Error("holder startup deadline")), 5000);
    child.once("error", reject);
    child.stdout!.on("data", chunk => {
      buffer += chunk;
      const end = buffer.indexOf("\n");
      if (end < 0) return;
      clearTimeout(timer);
      child.stdout!.destroy();
      child.unref();
      resolve(JSON.parse(buffer.slice(0, end)));
    });
  });
}
// ---- Issue #4263: GitCache.discardRetireScratchParent under the real worker uid ----------------------------------
// Everything here is real except the spawn seam, which only wraps the production argv in a worker-uid shell that
// waits on a release file and then execs it unchanged (so the descriptors, the setpriv runner pass and the helper
// script are the production ones) and which lets the test race the tree at the exact point between pin and removal.
const O_PATH_DIR = 0o10000000 | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
// The wait is bounded (1200 x 0.1 s) so a failed assertion can never leave an orphan spinning with the EX descriptor, and
// the gate variables are removed so the helper starts with exactly the audited environment.
const GATE = ': > "$UZI_GATE_STARTED"; n=0; while [ ! -e "$UZI_GATE_RELEASE" ]; do n=$((n+1)); [ "$n" -le 1200 ] || exit 97; sleep 0.1; done; ' +
  'unset UZI_GATE_STARTED UZI_GATE_RELEASE n; exec "$@"';
// Runs as the runner uid; fd 3 is the clone pin and fd 4 the epoch EX lock (inherited through setpriv).
const FD_PROBE = [
  'const fs=require("node:fs");const out={uid:process.getuid(),gid:process.getgid(),fds:{}};',
  'for (const fd of [3,4]) { const s=fs.fstatSync(fd,{bigint:true}); out.fds[fd]={ id:s.dev+":"+s.ino, dir:s.isDirectory() }; }',
  'const code=(f)=>{try{f();return "opened";}catch(e){return e.code;}};',
  'out.reopen={',
  ' parentDotDot:code(()=>fs.closeSync(fs.openSync("/proc/self/fd/3/..",fs.constants.O_RDONLY|fs.constants.O_DIRECTORY))),',
  ' listDotDot:code(()=>fs.readdirSync("/proc/self/fd/3/..")),',
  ' siblingViaDotDot:code(()=>fs.closeSync(fs.openSync("/proc/self/fd/3/../clone",fs.constants.O_RDONLY|fs.constants.O_DIRECTORY))),',
  ' statViaDotDot:code(()=>fs.lstatSync("/proc/self/fd/3/../clone")) };',
  'process.stdout.write(JSON.stringify(out));',
].join("\n");
// A shell lists its own whole descriptor table (a Node process holds runtime descriptors of its own). The glob's directory
// descriptor is closed before the test, so a surviving entry is a real one; readlink is the only process spawned.
// A runner-uid racer: against the clone pin at fd 3 it repeatedly moves a top-level directory aside, plants a symlink to an
// outside directory under its name, then undoes it, until the clone is empty or its own bounded clock ends. Errors are
// expected (the helper is deleting the same entries) and ignored; it only counts the swaps that did land.
const RACER = [
  'const fs=require("node:fs");const outside=process.argv[1];const end=Date.now()+8000;let swaps=0;const R="/proc/self/fd/3/";',
  'const names=[];for(let i=0;i<4;i++)names.push("r"+i);',
  'let seenNonEmpty=false;let ready=false;',
  'while(Date.now()<end){',
  ' let left;try{left=fs.readdirSync(R);}catch{break;}',
  ' if(!ready){ready=true;fs.writeSync(1,"READY\\n");}',
  ' if(left.length>0)seenNonEmpty=true; else if(seenNonEmpty)break;',
  ' for(const n of names){try{fs.renameSync(R+n,R+n+".aside");fs.symlinkSync(outside,R+n);swaps++;',
  '   fs.unlinkSync(R+n);fs.renameSync(R+n+".aside",R+n);}catch{}}',
  '}',
  'process.stdout.write(JSON.stringify({swaps}));',
].join("\n");
const FD_LIST = 'for p in /proc/$$/fd/*; do [ -L "$p" ] || continue; printf "%s " "${p##*/}"; readlink "$p"; done';
async function retireOp(req: { root: string; parentName: string; cache: string; race: string; outside: string; elsewhere: string;
  gate?: { started: string; release: string; hold?: boolean }; probe?: boolean; lockFile?: string;
  mark?: { bare: string; uuid: string; settledRaw: string } }) {
  process.env.UZI_UID_SPLIT = "1";
  const { GitCache } = await import("../../agent/src/git.js");
  const { processGroupAlive } = await import("../../agent/src/tick-spawner.js");
  const rr = await import("../../agent/src/runner-uid.js");
  const logger: any = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
    redactDiagnostic: (text: string) => text, child() { return logger; } };
  const git: any = new GitCache(req.cache, logger);
  const parent = path.join(req.root, req.parentName), clone = path.join(parent, "clone");
  const rootPin = fs.openSync(req.root, O_PATH_DIR);
  const mntId = (fd: number) => /^mnt_id:\s+(\d+)$/m.exec(fs.readFileSync(`/proc/self/fdinfo/${fd}`, "utf8"))![1]!;
  const ids = (p: string) => { try { return fs.lstatSync(p, { bigint: true }); } catch { return undefined; } };
  let spawns = 0;
  let racer: Promise<{ swaps: number; status: number | null }> | undefined;
  git.epochNativeTestDependencies = {
    spawn: (command: string, args: string[], options: any) => {
      spawns++;
      const child = req.gate
        ? spawn("/bin/sh", ["-c", GATE, "sh", command, ...args],
          { ...options, env: { ...options.env, UZI_GATE_STARTED: req.gate.started, UZI_GATE_RELEASE: req.gate.release } })
        : spawn(command, args, options);
      // Registered before the production listeners, so the swap lands after the helper exited and before the worker's removal.
      if (req.race === "clone-dir-after") child.once("exit", () => swapClone("dir"));
      return child;
    },
    alive: (pid: number, identity: any) => processGroupAlive(pid, identity, true),
    // Production kill semantics for identity "command": a signal not confirmed while the group lives is an error.
    kill: (pid: number, identity: any) => {
      if (!rr.killRunnerGroupOnly(pid, true) && processGroupAlive(pid, identity, true)) throw new Error("epoch group signal unconfirmed");
    },
    // joinMs is deliberately omitted: GitCache falls back to its production EPOCH_NATIVE_JOIN_MS (not exported).
  };
  const scope = ownership.beginReclaim();
  let discarded = false;
  const waitFor = async (file: string) => {
    // Stops as soon as the discard returned (a refusal never spawns), so no timer outlives the op.
    for (let i = 0; i < 1200 && !discarded && !fs.existsSync(file); i++) await new Promise(resolve => setTimeout(resolve, 50));
    if (!fs.existsSync(file)) throw new Error("gate never started");
  };
  // The runner cannot walk the worker-private parent by name, so it reaches the clone through an inherited descriptor.
  const runnerSh = (script: string, ...args: string[]) => {
    const pinned = fs.openSync(clone, O_PATH_DIR);
    try {
      const w = runnerCommand("/bin/sh", ["-c", script, "sh", ...args]);
      const r = spawnSync(w.command, w.args, { env: { PATH: "/usr/bin:/bin" }, encoding: "utf8", timeout: 10000,
        stdio: ["ignore", "pipe", "pipe", pinned] });
      if (r.status !== 0) throw new Error("runner racer failed: " + r.stderr);
    } finally { fs.closeSync(pinned); }
  };
  const swapClone = (plant: "symlink" | "dir") => {
    fs.renameSync(clone, path.join(parent, "moved"));
    if (plant === "symlink") fs.symlinkSync(req.outside, clone); else fs.mkdirSync(clone);
  };
  const race = async () => {
    if (req.race === "clone-symlink-before") swapClone("symlink");
    else if (req.race === "clone-dir-before") swapClone("dir");
    else if (req.race === "concurrent") {
      // Started asynchronously; the caller releases the gate only after the racer's READY line (its first successful
      // readdir of the clone), so the helper cannot win the start race and leave the racer nothing to swap.
      const pinned = fs.openSync(clone, O_PATH_DIR);
      const w = runnerCommand(process.execPath, ["-e", RACER, req.outside]);
      const child = spawn(w.command, w.args, { env: { PATH: "/usr/bin:/bin" }, stdio: ["ignore", "pipe", "pipe", pinned] });
      fs.closeSync(pinned);
      let out = "";
      child.stdout!.on("data", chunk => { out += chunk; });
      racer = new Promise(resolve => child.once("close", status => {
        let swaps = -1; try { swaps = JSON.parse(out.replace(/^READY\n/, "")).swaps; } catch { /* reported as -1 */ }
        resolve({ swaps, status });
      }));
      // Bounded (15 s): a racer that never reports ready fails the case rather than hanging or silently racing nothing.
      for (let i = 0; i < 300 && !out.startsWith("READY\n"); i++) await new Promise(resolve => setTimeout(resolve, 50));
      if (!out.startsWith("READY\n")) throw new Error("runner racer never reported ready");
    } else if (req.race === "descendant") {
      // The runner moves one subtree out of the clone and plants a symlink to an outside directory in its place.
      runnerSh('mv /proc/self/fd/3/nested "$1/moved-out" && ln -s "$2" /proc/self/fd/3/nested', req.elsewhere, req.outside);
    }
  };
  const rootStat = fs.lstatSync(req.root, { bigint: true });
  const st = (p: string) => ids(p);
  const input = {
    scope, root: req.root, rootPin, rootDev: String(rootStat.dev), rootMntId: mntId(rootPin), parentName: req.parentName,
    parentIno: String(st(parent)?.ino ?? 1n), cloneIno: String(st(clone)?.ino ?? 1n),
    deadline: Date.now() + 120_000, maxEntries: 2_000_000, maxDepth: 256, signal: new AbortController().signal,
    live: () => undefined,
    // With a bound bare, the real `removing` transition (config write plus the production syncs) runs before the helper spawns.
    beforeEffect: async () => {
      if (!req.mark) return;
      const { bare, uuid, settledRaw } = req.mark;
      await git.withRetireScratchBare(bare, { signal: new AbortController().signal, deadline: Date.now() + 60_000 },
        (locked: any) => locked.markRemoving(uuid, settledRaw));
    },
  };
  try {
    if (req.probe) {
      const lockStat = fs.statSync(req.lockFile!, { bigint: true });
      const clonePin = fs.openSync(clone, O_PATH_DIR);
      try {
        const launch = async (w: { command: string; args: string[] }) => {
          const joined = await git.spawnJoinedEpochChild(scope, w.command, w.args, {
            env: { PATH: "/usr/bin:/bin" }, cwd: "/", identity: "command", signals: [], deadline: Date.now() + 30_000,
            timeout: 5000, extraFd: clonePin,
          });
          if (!joined.spawned) throw new Error("probe not spawned");
          joined.ticket.settle();
          return joined;
        };
        const joined = await launch(runnerCommand(process.execPath, ["-e", FD_PROBE]));
        const listed = await launch(runnerCommand("/bin/sh", ["-c", FD_LIST]));
        return { probe: JSON.parse(joined.stdout), exit: joined.code || listed.code, stderr: joined.stderr + listed.stderr, list: listed.stdout,
          cloneId: `${st(clone)!.dev}:${st(clone)!.ino}`, lockId: `${lockStat.dev}:${lockStat.ino}` };
      } finally { fs.closeSync(clonePin); }
    }
    if (req.gate) {
      void waitFor(req.gate.started).then(async () => {
        await race();
        if (!req.gate!.hold) fs.writeFileSync(req.gate!.release, "");
      }).catch(() => { fs.writeFileSync(req.gate!.release, ""); });
    }
    let result;
    try { result = await git.discardRetireScratchParent(input); } finally { discarded = true; }
    const raced = racer === undefined ? undefined : await Promise.race([racer,
      new Promise<never>((_, reject) => setTimeout(() => reject(new Error("racer did not finish")), 30_000).unref())]);
    return { raced, result, spawns, pending: scope.pendingMutations, stopped: scope.stopped(), unsettled: [...git.unsettledDeletes.keys()] };
  } finally {
    try { if (!scope.stopped() && scope.pendingMutations === 0) scope.finish(); } catch { /* reported by the next op */ }
  }
}
// Creates a bare repository as this (worker) uid and writes one canonical settled binding for the parent with git config.
async function retireBind(req: { cache: string; bare: string; uuid: string; binding: object }) {
  const { retireBindingKey, retireBindingValue } = await import("../../agent/src/git.js");
  fs.mkdirSync(req.cache, { recursive: true });
  const git = (...args: string[]) => execFileSync("git", args, { encoding: "utf8", env: { PATH: "/usr/bin:/bin", HOME: os.tmpdir() } });
  git("init", "--bare", "--quiet", req.bare);
  const value = retireBindingValue(req.binding as any);
  git("-C", req.bare, "config", "--local", retireBindingKey(req.uuid), value);
  return value;
}
// The successor's view: a fresh GitCache reads the bare back and evaluates the binding with the exported pure decision.
async function retireEval(req: { cache: string; bare: string; uuid: string; runnerRoot: string }) {
  const { GitCache, evaluateRetireBinding } = await import("../../agent/src/git.js");
  const logger: any = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
    redactDiagnostic: (text: string) => text, child() { return logger; } };
  const git: any = new GitCache(req.cache, logger);
  const read = await git.readRetireBindings(req.bare);
  const evidence = await git.withRetireScratchBare(req.bare, { signal: new AbortController().signal, deadline: Date.now() + 60_000 },
    (locked: any) => locked.evidence());
  const exam = evaluateRetireBinding({ evidence, uuid: req.uuid, runnerRoot: req.runnerRoot });
  return { read, exam };
}
// The production retire writer in the real image: a GitCache over a data dir whose runner root is worker:runner 3775
// (the entrypoint posture, so the new scratch parent inherits setgid) seeds a runner clone from a local origin and runs
// retireRunnerClone down its EXDEV scratch branch. Only the cross-device rename into the holding root is simulated
// (a test-only fs.promises.rename wrapper). Reports what the production code left behind, as the worker uid.
async function retireProduction(req: { dataDir: string }) {
  // The failing step rides in the error so CI names it, not only the innermost message.
  let step = "setup";
  try { return await retireProductionSteps(req, name => { step = name; }); }
  catch (error) { throw new Error(`retire-production failed at ${step}: ${String(error)}`, { cause: error }); }
}
async function retireProductionSteps(req: { dataDir: string }, at: (step: string) => void) {
  process.env.UZI_UID_SPLIT = "1";
  // The worker posture main.ts pairs with the uid split (agent/src/main.ts: `if (uidSplitActive()) process.umask(0o002)`):
  // worker-made runner-clone parents stay group-runner-writable, so the runner-uid clone can create its work tree there.
  process.umask(0o002);
  const { GitCache } = await import("../../agent/src/git.js");
  const { formatAttemptId } = await import("../../agent/src/attempt-path.js");
  const logger: any = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
    redactDiagnostic: (text: string) => text, child() { return logger; } };
  const env = { PATH: "/usr/bin:/bin", HOME: os.tmpdir(), GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  const plain = (cwd: string, ...args: string[]) => execFileSync("git",
    ["-C", cwd, "-c", "user.email=fixture@example.com", "-c", "user.name=fixture", "-c", "commit.gpgsign=false", ...args], { encoding: "utf8", env });
  const origin = path.join(req.dataDir, "origin");
  fs.mkdirSync(origin);
  execFileSync("git", ["init", "-q", "-b", "main", origin], { env });
  fs.writeFileSync(path.join(origin, "README.md"), "one\n");
  plain(origin, "add", "README.md");
  plain(origin, "commit", "-q", "-m", "commit 1");
  const attemptId = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
  const runId = "run-retire-production";
  const git: any = new GitCache(req.dataDir, logger);
  at("ensureClone");
  const bare = await git.ensureClone(origin);
  const branch = "agent/issue-4263";
  at("createOrAttachRunnerClone");
  const clone = await git.createOrAttachRunnerClone(bare, 4263, { beforeFree: async () => {}, isLive: () => false }, runId, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  at("markRecoveryCapture");
  await git.markRecoveryCapture(bare, clone.path, branch, runId, attemptId);
  at("retireRunnerClone");
  const runnerRoot = path.join(req.dataDir, "runner");
  const holdingRoot = path.join(req.dataDir, "runner-quarantine");
  const rename = fs.promises.rename;
  (fs.promises as { rename: typeof fs.promises.rename }).rename = (async (from: fs.PathLike, to: fs.PathLike) => {
    if (String(to).startsWith(holdingRoot)) throw Object.assign(new Error("cross device"), { code: "EXDEV" });
    return rename(from, to);
  }) as typeof fs.promises.rename;
  try {
    await git.retireRunnerClone(bare, clone.path, branch, runId, { discard: true, attemptId, dispositionClass: "owner_terminal" });
  } finally { (fs.promises as { rename: typeof fs.promises.rename }).rename = rename; }
  at("readback");
  const parents = fs.readdirSync(runnerRoot).filter(n => n.startsWith(".retire-"));
  if (parents.length !== 1) throw new Error("expected exactly one retire scratch parent, saw " + JSON.stringify(parents));
  const parentPath = path.join(runnerRoot, parents[0]!);
  const pst = fs.lstatSync(parentPath);
  const bindings = execFileSync("git", ["-C", bare, "config", "--local", "--get-regexp", "^uzi-retire-scratch\\."], { encoding: "utf8", env }).trim();
  return { runnerRoot, parentName: parents[0]!, parentMode: pst.mode & 0o7777, parentUid: pst.uid,
    rootMode: fs.lstatSync(runnerRoot).mode & 0o7777, cloneExists: fs.existsSync(path.join(parentPath, "clone")),
    canonicalGone: !fs.existsSync(clone.path), bindings };
}
// ---- Issue #4486: the group-writable managed bare proof under the real worker uid, runner uids and getfacl ------------
// Setup runs the production writers as the worker (umask 002 under a setgid repos root): the real runner-group probe,
// a bare made by ensureClone and a runner `clone --shared` attempt. The proof runs the production
// proveEpochWorktreeContent with the trust the real probe issued and the real /usr/bin/getfacl: no ACL, statfs or
// getfacl seam is injected anywhere. The test (root) mutates the fixture between the two ops.
type ManagedBareFixture = { git: any; bare: string; clonePath: string; selector: { runId: string; generation: number; attempt: string };
  trust: { state: string; reason?: string; gid?: number } };
const managedBareFixtures = new Map<string, ManagedBareFixture>();
async function managedBareSetup(req: { dataDir: string }) {
  process.env.UZI_UID_SPLIT = "1";
  process.umask(0o002);
  const { GitCache } = await import("../../agent/src/git.js");
  const { formatAttemptId } = await import("../../agent/src/attempt-path.js");
  const { probeRunnerGroupTrust } = await import("../../agent/src/runner-group-trust.js");
  const { commandRootCommand } = await import("../../agent/src/runner-uid.js");
  const logger: any = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
    redactDiagnostic: (text: string) => text, child() { return logger; } };
  const env = { PATH: "/usr/bin:/bin", HOME: os.tmpdir(), GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  const plain = (cwd: string, ...args: string[]) => execFileSync("git",
    ["-C", cwd, "-c", "user.email=fixture@example.com", "-c", "user.name=fixture", "-c", "commit.gpgsign=false", ...args], { encoding: "utf8", env });
  // The real probe, once, as the production worker; the two wrapped children's status text is captured for the report.
  const trust = await probeRunnerGroupTrust();
  const status = (w: { command: string; args: string[] }) =>
    spawnSync(w.command, w.args, { env: { PATH: "/usr/bin:/bin" }, encoding: "utf8", timeout: 10000 }).stdout;
  const statuses = { runner: status(runnerCommand("/bin/cat", ["/proc/self/status"], true)),
    command: status(commandRootCommand("/bin/cat", ["/proc/self/status"])) };
  const origin = path.join(req.dataDir, "origin");
  fs.mkdirSync(origin);
  execFileSync("git", ["init", "-q", "-b", "main", origin], { env });
  fs.writeFileSync(path.join(origin, "README.md"), "one\n");
  plain(origin, "add", "README.md");
  plain(origin, "commit", "-q", "-m", "commit 1");
  const attemptId = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
  const runId = "run-managed-bare";
  const git: any = new GitCache(req.dataDir, logger, undefined, { managedBareGroupTrust: trust });
  const bare = await git.ensureClone(origin);
  const branch = "agent/issue-4486";
  const clone = await git.createOrAttachRunnerClone(bare, 4486, { beforeFree: async () => {}, isLive: () => false }, runId, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, branch, runId, attemptId);
  const st = fs.lstatSync(bare);
  managedBareFixtures.set(req.dataDir, { git, bare, clonePath: clone.path,
    selector: { runId, generation: 1, attempt: attemptId }, trust: trust as any });
  return { bare, clonePath: clone.path, trust: { ...(trust as object) }, statuses, bareMode: st.mode & 0o7777, bareGid: st.gid, bareUid: st.uid,
    isIssued: (await import("../../agent/src/runner-group-trust.js")).isIssuedManagedBareGroupTrust(trust) };
}
async function managedBareProve(req: { dataDir: string }) {
  const fx = managedBareFixtures.get(req.dataDir);
  if (!fx) throw new Error("no managed bare fixture for " + req.dataDir);
  const scope = ownership.beginReclaim();
  const started = Date.now();
  try {
    const verdict = await fx.git.proveEpochWorktreeContent({ scope, barePath: fx.bare, selector: fx.selector, clonePath: fx.clonePath,
      signal: new AbortController().signal, deadline: Date.now() + 120_000 });
    return { proven: verdict.proven, reason: verdict.reason ?? null, elapsedMs: Date.now() - started,
      cloneExists: fs.existsSync(fx.clonePath), pending: scope.pendingMutations, stopped: scope.stopped() };
  } finally {
    try { if (!scope.stopped() && scope.pendingMutations === 0) scope.finish(); } catch { /* reported by the next op */ }
  }
}
console.log(JSON.stringify({ ready: true, pid: process.pid, uid: process.getuid!(),
  actualFilesystem: Number(fs.statfsSync(root).type), classificationOnly: "ext4" }));
for await (const line of createInterface({ input: process.stdin })) {
  const request = JSON.parse(line);
  try {
    let result: unknown;
    switch (request.op) {
      case "seed": result = await runtime.seed(request.uid); break;
      case "acquire": {
        // Production posture (agent/src/main.ts: `const controller = new AbortController()`, passed to
        // `epochRuntime.acquire`): the ownership retains this signal and re-checks it on every reclaim
        // assertion, so it must live as long as the process. A timer-backed signal expired 5 s after the
        // acquire and failed a later reclaim with TimeoutError. Only the pending acquire stays bounded.
        const lifetime = new AbortController();
        const bound = setTimeout(() =>
          lifetime.abort(new DOMException("epoch acquire exceeded 5000 ms", "TimeoutError")), 5000);
        try { ownership = await runtime.acquire({ firstMode: request.mode, signal: lifetime.signal }); }
        finally { clearTimeout(bound); }
        result = ownership.mode; break;
      }
      case "consume": result = await ownership.consume(request.uid); break;
      case "matches": result = ownership.matchesEntry(request.current, request.previous); break;
      case "shared": await ownership.reacquireShared(); result = ownership.mode; break;
      case "close": ownership.close(); ownership = undefined; result = "closed"; break;
      case "exit": process.exit(0); break; // intentionally no close: kernel exit release
      case "selfkill": process.kill(process.pid, "SIGKILL"); break; // no response, no close
      case "mutator": {
        // The production path: reclaim scope, mutation ticket, explicit inheritStdio.
        const scope = ownership.beginReclaim();
        const ticket = scope.beginMutation();
        const stdio = scope.inheritStdio(["ignore", "pipe", "ignore"]);
        const program = request.shape === "grandchild" ? MIDDLE : HOLD;
        const holder = spawn(process.execPath, ["-e", program, lockFile, request.release], { stdio });
        result = await firstLine(holder);
        if (request.finish) { ticket.settle(); scope.finish(); }
        break;
      }
      case "ordinary": {
        // Ordinary run/SDK-style spawns while a reclaim scope and mutation ticket are live.
        const scope = ownership.beginReclaim();
        const ticket = scope.beginMutation();
        const detect = (options: object) => JSON.parse(spawnSync(process.execPath, ["-e", DETECT, lockFile],
          { encoding: "utf8", timeout: 5000, ...options }).stdout);
        const asynchronous = await new Promise<string[]>((resolve, reject) => {
          const c = spawn(process.execPath, ["-e", DETECT, lockFile]);
          let out = "";
          c.stdout.on("data", chunk => { out += chunk; });
          c.once("error", reject);
          c.once("close", () => resolve(JSON.parse(out)));
        });
        result = {
          default: detect({}),
          inheritedStdio: detect({ stdio: ["inherit", "pipe", "inherit"] }),
          execFile: JSON.parse(execFileSync(process.execPath, ["-e", DETECT, lockFile], { encoding: "utf8", timeout: 5000 })),
          shell: JSON.parse(spawnSync("/bin/sh", ["-c", 'exec "$0" -e "$1" "$2"', process.execPath, DETECT, lockFile],
            { encoding: "utf8", timeout: 5000 }).stdout),
          asynchronous,
          // Positive control: only the explicit scope option passes the descriptor.
          viaScope: detect({ stdio: scope.inheritStdio(["ignore", "pipe", "inherit"]) }),
        };
        ticket.settle(); scope.finish();
        break;
      }
      case "unrelated": {
        // Default child stdio cannot inherit Node's private epoch descriptor.
        const child = spawnSync(process.execPath, ["-e", `
          const fs = require('node:fs');
          const target = process.argv[1];
          const matches = fs.readdirSync('/proc/self/fd').filter(fd => {
            try { return fs.readlinkSync('/proc/self/fd/' + fd) === target; }
            catch { return false; }
          });
          console.log(JSON.stringify(matches));
        `, path.join(root, ".uzi-epoch.lock")], { encoding: "utf8", timeout: 5000 });
        if (child.status !== 0) throw new Error(child.stderr);
        result = JSON.parse(child.stdout); break;
      }
      case "restore": {
        const expected = "/data/runner/.tmp" + (request.split ? "/uzi-worker" : "");
        process.env.UZI_UID_SPLIT = request.split ? "1" : "0";
        process.env.TMPDIR = "/tmp/uzi-epoch-bootstrap";
        if (request.saved === null) delete process.env.UZI_EPOCH_RUNTIME_TMPDIR;
        else process.env.UZI_EPOCH_RUNTIME_TMPDIR = request.saved ?? expected;
        restoreEpochRuntimeTmpdir(ownership);
        const made = fs.mkdtempSync(path.join(os.tmpdir(), "epoch-worker-"));
        const runnerTmp = request.split ? "/data/runner/.tmp/uzi-runner" : expected;
        const runnerProgram = ["-e",
          "const fs=require('node:fs'),os=require('node:os');console.log(JSON.stringify({tmp:os.tmpdir(),made:fs.mkdtempSync(os.tmpdir()+'/epoch-runner-')}))"];
        const child = spawnSync(request.split ? "/bin/setpriv" : process.execPath,
          request.split ? [...setprivArgsForUid(RUNNER_UID), process.execPath, ...runnerProgram] : runnerProgram,
          { env: { PATH: "/usr/local/bin:/usr/bin:/bin", TMPDIR: runnerTmp }, encoding: "utf8", timeout: 5000 });
        if (child.status !== 0) throw new Error(child.stderr);
        result = { env: process.env.TMPDIR, os: os.tmpdir(), made,
          saved: process.env.UZI_EPOCH_RUNTIME_TMPDIR ?? null, runner: JSON.parse(child.stdout) };
        break;
      }
      case "retire":
        if (request.detached) {
          // The worker is killed while the helper runs, so the reply cannot wait for the discard.
          void retireOp(request).catch(error => { process.stderr.write("detached retire: " + String(error) + "\n" + String((error as Error)?.stack ?? "") + "\n"); });
          result = "launched";
        } else result = await retireOp(request);
        break;
      case "retire-bind": result = await retireBind(request); break;
      case "retire-eval": result = await retireEval(request); break;
      case "retire-production": result = await retireProduction(request); break;
      case "managed-bare-setup": result = await managedBareSetup(request); break;
      case "managed-bare-prove": result = await managedBareProve(request); break;
      default: throw new Error("unknown request");
    }
    console.log(JSON.stringify({ result }));
  } catch (error) {
    // `error` stays String(error) (a test regex-matches it); `op` and `stack` name the failing request and frame.
    console.log(JSON.stringify({ error: String(error), op: request.op, stack: String((error as Error)?.stack ?? "") }));
  }
}
