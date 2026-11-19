import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from "node:child_process";
import { createRequire } from "node:module";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";

import { entrypointFixture } from "./pod-epoch-entrypoint-fixture.mjs";
import { commandRootCommand, runnerCommand } from "../../agent/src/runner-uid.js";

const require = createRequire(import.meta.url);
const { createRuntime } = require("../../agent/templates/pod-epoch-init.cjs");
const childFile = fileURLToPath(new URL("./pod-epoch-child.mts", import.meta.url));
const loader = "/app/node_modules/tsx/dist/loader.mjs";
const S = "11111111-1111-4111-8111-111111111111";
const T = "22222222-2222-4222-8222-222222222222";
const X = "33333333-3333-4333-8333-333333333333";
// Explicit classification-only injection; retain the tmpfs kernel and real utility.
const io = { ...fs, statfsSync: (...args: Parameters<typeof fs.statfsSync>) =>
  ({ ...fs.statfsSync(...args), type: 0xef53 }) };

function fixture() {
  assert.equal(process.platform, "linux", "image proof requires Linux; never skip");
  assert.equal(process.getuid!(), 0, "image proof requires the shared root window");
  const status = fs.readFileSync("/proc/self/status", "utf8");
  assert.match(status, /^CapEff:\s*00000000000001c3$/m, "exact shared-proof capabilities");
  assert.match(status, /^NoNewPrivs:\s*1$/m);
  const version = spawnSync("/usr/bin/flock", ["--version"], { encoding: "utf8" });
  assert.equal(version.status, 0, version.stderr);
  assert.match(version.stdout, /^flock from util-linux /, "absolute image flock is util-linux");
  const owner = spawnSync("/sbin/apk", ["info", "--who-owns", "/usr/bin/flock"], { encoding: "utf8" });
  assert.equal(owner.status, 0, owner.stderr);
  assert.match(owner.stdout, /owned by flock-/, "apk owns image flock (not BusyBox or PATH substitution)");
  const root = fs.mkdtempSync("/data/epoch-proof-");
  fs.chmodSync(root, 0o2770);
  fs.chownSync(root, 10001, 10001);
  const runtime = createRuntime({ root, io, uid: 10001, gid: 10001 });
  const children: Owned[] = [];
  return { root, runtime,
    async exclusive() {
      const held = await runtime.acquire({ firstMode: "exclusive" });
      if (held.mode !== "exclusive") {
        held.close();
        assert.fail("proof requires actual EX; a fallback SH cannot establish the test precondition");
      }
      return held;
    },
    async child(split = false) { const child = new Owned(root, split); children.push(child); await child.ready(); return child; },
    async close() {
      try { await Promise.all(children.map(child => child.stop())); }
      finally {
        const reclaimed = spawnSync("/bin/chown", ["-R", "-h", "0:0", root]);
        assert.equal(reclaimed.status, 0);
        fs.rmSync(root, { recursive: true });
      }
    },
  };
}

function deadline<T>(promise: Promise<T>, label: string, ms = 8000): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(label + " deadline")), ms);
    promise.then(value => { clearTimeout(timer); resolve(value); },
      error => { clearTimeout(timer); reject(error); });
  });
}
class Owned {
  child: ChildProcessWithoutNullStreams;
  lines: AsyncIterator<string>;
  ended: Promise<number | null>;
  stderr = "";
  constructor(root: string, split = false) {
    const identity = split
      ? ["--reuid", "worker", "--regid", "worker", "--init-groups", "--bounding-set", "-all,+setuid,+setgid",
        "--inh-caps", "-all,+setuid,+setgid", "--ambient-caps", "-all,+setuid,+setgid", "--"]
      : ["--reuid", "10001", "--regid", "10001", "--clear-groups", "--bounding-set", "-all",
        "--inh-caps", "-all", "--ambient-caps", "-all", "--"];
    this.child = spawn("/bin/setpriv", [...identity,
      process.execPath, "--import", loader, childFile, root], {
      env: { PATH: "/usr/local/bin:/usr/bin:/bin", TSX_DISABLE_CACHE: "1", TMPDIR: "/tmp/uzi-epoch-bootstrap" },
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.child.stderr.on("data", chunk => { this.stderr += chunk; });
    this.ended = new Promise((resolve, reject) => {
      this.child.once("error", reject);
      this.child.once("close", resolve);
    });
    this.lines = createInterface({ input: this.child.stdout })[Symbol.asyncIterator]();
  }
  async read(ms = 8000) {
    const line = await deadline(this.lines.next(), "owned child response: " + this.stderr, ms);
    assert.equal(line.done, false, "owned child exited: " + this.stderr);
    return JSON.parse(line.value!);
  }
  async ready() {
    const hello = await this.read();
    assert.equal(hello.ready, true);
    assert.equal(hello.uid, 10001);
    assert.equal(hello.pid, this.child.pid, "responder belongs to this exact child");
    assert.equal(hello.classificationOnly, "ext4");
    assert.notEqual(hello.actualFilesystem, 0xef53, "image lane is actual tmpfs, not ext4 proof");
  }
  async raw(request: object, ms?: number) {
    this.child.stdin.write(JSON.stringify(request) + "\n");
    return this.read(ms);
  }
  async ask(request: object, ms?: number): Promise<any> {
    const response = await this.raw(request, ms);
    // The child names the failing op and frame; its stderr carries anything the code under test logged.
    assert.equal(response.error, undefined,
      `${response.error}\nop: ${response.op}\nstack: ${response.stack}\nchild stderr: ${this.stderr}`);
    return response.result;
  }
  killed = false;
  // The root proof process holds no CAP_KILL, so the owner SIGKILLs itself on request.
  async killSelf() {
    this.killed = true;
    this.child.stdin.write('{"op":"selfkill"}\n');
    assert.equal(await deadline(this.ended, "owner SIGKILL join"), null, "owner died by signal: " + this.stderr);
  }
  async stop() {
    if (this.killed) return;
    if (this.child.exitCode === null) this.child.stdin.end();
    assert.equal(await deadline(this.ended, "join owned child"), 0, this.stderr);
  }
  async exit() {
    this.child.stdin.write('{"op":"exit"}\n');
    assert.equal(await deadline(this.ended, "process-exit release"), 0, this.stderr);
  }
}
function flock(root: string, mode: "-s" | "-x"): number | null {
  const fd = fs.openSync(path.join(root, ".uzi-epoch.lock"), "r");
  try {
    const result = spawnSync("/usr/bin/flock", [mode, "-n", "-E", "200", "3"],
      { stdio: ["ignore", "pipe", "pipe", fd], encoding: "utf8", timeout: 5000 });
    assert.equal(result.error, undefined);
    return result.status;
  } finally { fs.closeSync(fd); }
}

test("pod epoch real Node fd survives flock helper exit, excludes peers and releases only with its owner", { timeout: 60000 }, async () => {
  const f = fixture();
  try {
    assert.equal(await f.runtime.seed(X), "updated");
    assert.equal(await f.runtime.seed(S), "updated");
    const a = await f.child();
    const b = await f.child();
    assert.equal(await a.ask({ op: "acquire", mode: "shared" }), "shared");
    assert.equal(flock(f.root, "-x"), 200, "SH excludes EX after helper exit");
    assert.equal(await b.ask({ op: "acquire", mode: "shared" }), "shared", "two real SH workers coexist");
    assert.deepEqual(await a.ask({ op: "unrelated" }), [], "unrelated child inherits no lock descriptor and cannot unlock it");
    assert.equal(flock(f.root, "-x"), 200, "unrelated child exit cannot release parent's lock");
    const before = fs.readFileSync(path.join(f.root, ".uzi-pod-entry"), "utf8");
    const started = performance.now();
    assert.equal(await f.runtime.seed(T), "busy", "seed record EX never waits for held SH");
    assert.ok(performance.now() - started < 3000, "nonblocking seed returns within probe budget");
    assert.equal(fs.readFileSync(path.join(f.root, ".uzi-pod-entry"), "utf8"), before);
    assert.equal(await a.ask({ op: "matches", current: S, previous: X }), false, "SH supplies no entry authority");
    await a.exit();
    assert.equal(flock(f.root, "-x"), 200, "remaining SH owner still excludes EX");
    await b.exit();
    assert.equal(flock(f.root, "-x"), 0, "positive control: process exits release SH");
    const ex = await f.child();
    assert.equal(await ex.ask({ op: "acquire", mode: "exclusive" }), "exclusive");
    assert.equal(await ex.ask({ op: "matches", current: S, previous: X }), true);
    assert.equal(flock(f.root, "-s"), 200, "EX excludes SH");
    assert.equal(flock(f.root, "-x"), 200, "EX excludes EX");
    assert.equal(await ex.ask({ op: "shared" }), "shared");
    assert.equal(flock(f.root, "-s"), 0, "close then SH reacquisition permits another SH");
    assert.equal(flock(f.root, "-x"), 200, "reacquired SH still excludes EX");
    await ex.exit();
    assert.equal(flock(f.root, "-s"), 0, "positive control after EX owner exits");
    assert.equal(flock(f.root, "-x"), 0);
    await descendantKeepsDescriptor(f);
    await parentSigkillWhileMutating(f);
    await directCloseReleases(f);
    await ordinarySpawnsDoNotInherit(f);
  } finally { await f.close(); }
});

type Fixture = ReturnType<typeof fixture>;
async function until(label: string, ready: () => boolean, ms = 5000) {
  const end = performance.now() + ms;
  while (!ready()) {
    assert.ok(performance.now() < end, label);
    await new Promise(resolve => setTimeout(resolve, 50));
  }
}
// The holder self-reports and exits when its release file appears: the root proof process
// cannot read another uid's /proc/<pid>/fd or signal it.
function release(f: Fixture, name: string) { fs.writeFileSync(path.join(f.root, name), ""); }

async function descendantKeepsDescriptor(f: Fixture) {
  const owner = await f.child();
  assert.equal(await owner.ask({ op: "acquire", mode: "exclusive" }), "exclusive");
  const grand = await owner.ask({ op: "mutator", shape: "grandchild", release: path.join(f.root, "release-grand"), finish: true });
  assert.deepEqual(grand.holds, ["3"], "mutating grandchild holds the lock inode on the inherited descriptor");
  await owner.ask({ op: "close" });
  assert.equal(flock(f.root, "-x"), 200, "descendant keeps EX after the owner closed its own copy");
  assert.equal(flock(f.root, "-s"), 200, "descendant keeps excluding SH after the owner closed");
  await owner.exit();
  assert.equal(flock(f.root, "-x"), 200, "descendant keeps EX after the owner exited and the middle process is gone");
  release(f, "release-grand");
  await until("descendant exit releases the lock", () => flock(f.root, "-x") === 0);
}

async function parentSigkillWhileMutating(f: Fixture) {
  const control = await f.child();
  await control.ask({ op: "acquire", mode: "exclusive" });
  await control.killSelf();
  assert.equal(flock(f.root, "-x"), 0, "positive control: SIGKILL of a lone owner releases the lock");
  const owner = await f.child();
  await owner.ask({ op: "acquire", mode: "exclusive" });
  const holder = await owner.ask({ op: "mutator", shape: "direct", release: path.join(f.root, "release-direct"), finish: false });
  assert.deepEqual(holder.holds, ["3"], "mutating child holds the lock inode on the inherited descriptor");
  await owner.killSelf();
  assert.equal(flock(f.root, "-x"), 200, "SIGKILLed parent cannot release EX while its mutating child lives");
  assert.equal(flock(f.root, "-s"), 200, "SH is excluded until the mutating child exits");
  release(f, "release-direct");
  await until("mutating child exit releases the lock", () => flock(f.root, "-x") === 0);
}

async function directCloseReleases(f: Fixture) {
  const owner = await f.child();
  await owner.ask({ op: "acquire", mode: "exclusive" });
  assert.equal(flock(f.root, "-x"), 200, "owner holds EX before close");
  assert.equal(await owner.ask({ op: "close" }), "closed");
  assert.equal(flock(f.root, "-x"), 0, "direct close releases with no inheritor, while the process lives");
  const peer = await f.child();
  assert.equal(await peer.ask({ op: "acquire", mode: "exclusive" }), "exclusive", "peer takes EX after the close");
  assert.equal(flock(f.root, "-s"), 200);
  await peer.exit();
  const shared = await f.child();
  assert.equal(await shared.ask({ op: "acquire", mode: "shared" }), "shared");
  assert.equal(await shared.ask({ op: "close" }), "closed");
  assert.equal(flock(f.root, "-x"), 0, "direct close releases SH too");
  await owner.exit(); await shared.exit();
}

async function ordinarySpawnsDoNotInherit(f: Fixture) {
  const owner = await f.child();
  await owner.ask({ op: "acquire", mode: "exclusive" });
  const spawns = await owner.ask({ op: "ordinary" });
  for (const kind of ["default", "inheritedStdio", "execFile", "shell", "asynchronous"]) {
    assert.deepEqual(spawns[kind], [], kind + " spawn inherits no lock descriptor");
  }
  assert.deepEqual(spawns.viaScope, ["3"], "positive control: the detector sees the descriptor when the scope passes it");
  assert.equal(flock(f.root, "-x"), 200, "owner still holds EX after the spawns");
  await owner.exit();
  assert.equal(flock(f.root, "-x"), 0, "no spawned process kept the lock");
}

test("pod epoch real concurrent consumers preserve S to T to S history and the full immutable ledger bound", { timeout: 60000 }, async () => {
  const f = fixture();
  try {
    assert.equal(await f.runtime.seed(S), "updated");
    const ledger = path.join(f.root, ".uzi-epoch-consumed");
    const initial = fs.lstatSync(ledger);
    const a = await f.child(), b = await f.child();
    await Promise.all([a.ask({ op: "acquire" }), b.ask({ op: "acquire" })]);
    const results = await Promise.all([a.ask({ op: "consume", uid: S }), b.ask({ op: "consume", uid: S })]);
    assert.deepEqual(results.map(r => r.result).sort(), ["existing", "fresh"], "concurrent duplicate append is serialized");
    assert.ok(results.every(r => r.mayEvaluate === false), "SH consumers never evaluate");
    assert.deepEqual(await b.ask({ op: "consume", uid: T }), { result: "fresh", mayEvaluate: false });
    await a.exit(); await b.exit();
    const restart = await f.child();
    await restart.ask({ op: "acquire", mode: "exclusive" });
    assert.deepEqual(await restart.ask({ op: "consume", uid: S }), { result: "existing", mayEvaluate: false },
      "late evidence cannot rearm S after T");
    await restart.exit();
    const records = [S, T, ...Array.from({ length: 4094 }, (_, i) =>
      "aaaaaaaa-aaaa-4aaa-8aaa-" + i.toString(16).padStart(12, "0"))];
    // Capacity fixture changes CONTENTS only, after all consumers exited.
    fs.writeFileSync(ledger, "UZIEC01\n" + records.join("\n") + "\n");
    const full = fs.readFileSync(ledger);
    assert.equal(full.length, 151560, "specified exact logical byte bound");
    const c = await f.child();
    assert.equal(await c.ask({ op: "acquire" }), "shared");
    assert.deepEqual(await c.ask({ op: "consume", uid: X }), { result: "full", mayEvaluate: false });
    assert.equal(flock(f.root, "-x"), 200, "full ledger never bypasses epoch participation");
    assert.ok(fs.readFileSync(ledger).equals(full), "saturation changes no bytes");
    const after = fs.lstatSync(ledger);
    assert.equal(after.ino, initial.ino); assert.equal(after.nlink, 1);
    assert.equal(after.uid, 10001); assert.equal(after.gid, 10001);
    assert.equal(after.mode & 0o7777, 0o600);
    assert.deepEqual(fs.readdirSync(f.root).sort(), [".uzi-epoch-consumed", ".uzi-epoch.lock", ".uzi-pod-entry"].sort(),
      "one ledger inode and no accumulating temporary state");
    await c.exit();
    assert.equal(flock(f.root, "-x"), 0);
  } finally { await f.close(); }
});

test("pod epoch rendered seed and runner-init wait under EX in both real UID modes", { timeout: 60000 }, async () => {
  for (const seed of [true, false]) for (const single of [false, true]) {
    const f = fixture(), sandbox = entrypointFixture(f.root);
    let holder: Awaited<ReturnType<typeof f.runtime.acquire>> | undefined;
    let boot: ReturnType<typeof sandbox.start> | undefined;
    try {
      await f.runtime.seed(S);
      fs.mkdirSync(path.join(f.root, "provision"));
      fs.chownSync(path.join(f.root, "provision"), 10001, 10001);
      fs.writeFileSync(path.join(f.root, "provision/keep"), "protected");
      const rendered = sandbox.initScript(seed);
      const before = protectedSnapshot(f.root);
      holder = await f.exclusive();
      boot = sandbox.start(single, rendered);
      await boot.wait("EPOCH_HOLD_LOCK_CONFLICT");
      assert.ok(protectedSnapshot(f.root) === before, "rendered initializer cannot mutate protected state under EX");
      holder.close(); holder = undefined;
      await boot.join();
      if (seed) {
        assert.equal(fs.readFileSync(path.join(sandbox.root, "seed-target/seed-positive-control"), "utf8"), "seeded");
        assert.equal(fs.existsSync(path.join(f.root, "provision")), false, "cold seed clears only after SH acquired");
        assert.equal(JSON.parse(fs.readFileSync(path.join(f.root, ".uzi-pod-entry"), "utf8")).current, S,
          "busy record EX is not retried after SH wait");
      } else {
        const r = fs.lstatSync(path.join(f.root, "runner-shared"));
        const t = fs.lstatSync(path.join(f.root, "runner-shared/.tmp"));
        assert.equal(r.uid, 10001); assert.equal(t.uid, 10001);
        assert.equal(r.mode & 0o7777, 0o3775);
        assert.equal(fs.readFileSync(path.join(f.root, "provision/keep"), "utf8"), "protected");
      }
      assert.equal(fs.lstatSync(f.root).gid, 10001);
      assert.equal(fs.lstatSync(f.root).mode & 0o2770, 0o2770);
    } finally {
      holder?.close();
      try { await boot?.join(); } finally { sandbox.close(); await f.close(); }
    }
  }
});

// Bounded full metadata/content inventory, ignoring read-only atime changes.
function protectedSnapshot(root: string): string {
  const pending = [root], entries: unknown[] = [];
  let bytes = 0;
  while (pending.length) {
    assert.ok(entries.length < 4096, "proof discovery bound");
    const file = pending.pop()!, s = fs.lstatSync(file);
    let contents: string | undefined;
    if (s.isDirectory()) pending.push(...fs.readdirSync(file).sort().map(name => path.join(file, name)));
    else if (s.isSymbolicLink()) contents = fs.readlinkSync(file);
    else if (s.isFile()) {
      bytes += s.size; assert.ok(bytes <= 8 * 1024 * 1024, "proof content bound");
      contents = fs.readFileSync(file).toString("hex");
    }
    entries.push([path.relative(root, file), s.dev, s.ino, s.uid, s.gid, s.mode, s.nlink,
      s.size, s.mtimeMs, s.ctimeMs, contents]);
  }
  return JSON.stringify(entries);
}

test("pod epoch real entrypoint fences protected initialization under EX and confines both UID bootstrap launches", { timeout: 60000 }, async () => {
  const f = fixture();
  const sandbox = entrypointFixture(f.root);
  let holder: Awaited<ReturnType<typeof f.runtime.acquire>> | undefined;
  const launched: ReturnType<typeof sandbox.start>[] = [];
  try {
    await f.runtime.seed(S);
    for (const directory of ["runner/.tmp", "runner/attempt", "repos/bare", "jobs", "agent-home", "provision"]) {
      fs.mkdirSync(path.join(f.root, directory), { recursive: true });
    }
    for (const file of ["runner/attempt/keep", "repos/bare/keep", "jobs/keep"]) {
      fs.writeFileSync(path.join(f.root, file), "protected");
    }
    // A configured regular token aliases a protected inode, so token metadata
    // is covered by the same fence as migration, jobs, ledger and scratch.
    fs.linkSync(path.join(f.root, "runner/attempt/keep"), path.join(f.root, "token"));
    fs.chmodSync(path.join(f.root, "runner/.tmp"), 0o755);
    fs.chownSync(path.join(f.root, "runner/.tmp"), 10001, 10001);
    const posture = fs.lstatSync(f.root);
    const ledgerBefore = fs.readFileSync(path.join(f.root, ".uzi-epoch-consumed"));
    assert.equal(fs.existsSync(path.join(f.root, ".uzi-migrated-worker-worker")), false);
    for (const [extra, args, cwd] of [
      [{ HOME: f.root }, ["npm", "run", "start"], sandbox.app],
      [{}, ["node", "src/main.ts"], sandbox.app],
      [{}, ["npm", "run", "start"], sandbox.home],
    ] as [Record<string, string>, string[], string][]) {
      const rejected = sandbox.reject(extra, args, cwd);
      assert.notEqual(rejected.status, 0, "participating startup rejects unsupported HOME/CMD/cwd");
      assert.equal(fs.readFileSync(sandbox.log, "utf8"), "", "rejection precedes mutation commands");
    }
    // A planted persistent bootstrap alias is rejected before root writes.
    fs.renameSync(sandbox.bootstrap, sandbox.bootstrap + "-saved");
    fs.symlinkSync(path.join(f.root, "runner/attempt"), sandbox.bootstrap);
    const aliasBefore = protectedSnapshot(f.root);
    assert.notEqual(sandbox.reject().status, 0, "persistent bootstrap alias refuses startup");
    assert.ok(protectedSnapshot(f.root) === aliasBefore, "alias target remains untouched");
    fs.unlinkSync(sandbox.bootstrap);
    fs.renameSync(sandbox.bootstrap + "-saved", sandbox.bootstrap);
    const initializationBefore = protectedSnapshot(f.root);
    holder = await f.exclusive();
    const boot = sandbox.start(); launched.push(boot);
    await boot.wait("EPOCH_LOCK_CONFLICT");
    assert.ok(protectedSnapshot(f.root) === initializationBefore, "all pre-SH validation is read-only, including temporary directory metadata");
    assert.equal(fs.readFileSync(sandbox.log, "utf8"), "", "zero protected mutations while EX excludes initializer SH");
    assert.equal(fs.existsSync(path.join(f.root, ".uzi-migrated-worker-worker")), false, "absent migration sentinel stays absent under EX");
    assert.ok(fs.readFileSync(path.join(f.root, ".uzi-epoch-consumed")).equals(ledgerBefore));
    assert.equal(fs.lstatSync(path.join(f.root, "token")).uid, 0, "token alias metadata untouched under EX");
    holder.close(); holder = undefined;
    await boot.wait("EPOCH_INIT_DONE");
    holder = await f.exclusive();
    const gapBefore = protectedSnapshot(f.root);
    boot.child.stdin.write("continue\n");
    await boot.wait("EPOCH_NPM_READY");
    await boot.wait("EPOCH_MAIN_LOCK_CONFLICT");
    assert.ok(protectedSnapshot(f.root) === gapBefore, "real npm/tsx/main waits without protected writes in reacquisition gap");
    holder.close(); holder = undefined;
    await boot.wait("EPOCH_MAIN_OWNED");
    assert.equal(flock(f.root, "-x"), 200, "actual main retains SH before configuration/admission");
    boot.child.stdin.write("stop\n");
    await boot.join();
    assert.equal(flock(f.root, "-x"), 0, "actual main process exit releases ownership");
    const operations = fs.readFileSync(sandbox.log, "utf8");
    assert.ok(!operations.includes("POSTURE_VIOLATION"), "root posture holds after every initializer operation, even if later restored");
    for (const target of ["runner", "repos", ".uzi-epoch-consumed", "jobs", "token"]) {
      assert.ok(operations.split("\n").some(line => line.startsWith("CHOWN ") && line.includes(f.root + "/" + target)),
        "positive protected mutation after release: " + target);
    }
    assert.equal(fs.existsSync(path.join(f.root, ".uzi-migrated-worker-worker")), true);
    const after = fs.lstatSync(f.root);
    assert.equal(after.ino, posture.ino); assert.equal(after.gid, posture.gid);
    assert.equal(after.mode & 0o7777, posture.mode & 0o7777, "mounted data-root fsGroup posture preserved");
    holder = await f.exclusive();
    const singleBefore = protectedSnapshot(f.root);
    const single = sandbox.start(true); launched.push(single);
    await single.wait("EPOCH_INIT_DONE");
    single.child.stdin.write("continue\n");
    await single.wait("EPOCH_NPM_READY");
    await single.wait("EPOCH_MAIN_LOCK_CONFLICT");
    assert.ok(protectedSnapshot(f.root) === singleBefore, "single-uid main waits without protected writes");
    holder.close(); holder = undefined;
    await single.wait("EPOCH_MAIN_OWNED");
    assert.equal(flock(f.root, "-x"), 200, "single-uid actual main retains SH");
    single.child.stdin.write("stop\n");
    await single.join();
    assert.equal(fs.readFileSync(sandbox.log, "utf8").split("\n").filter(line => line.startsWith("BOOTSTRAP ")).length, 2,
      "both UID exec paths reach the instrumented known npm invocation with private bootstrap scratch and FD9 closed");
  } finally {
    holder?.close();
    // Release EX before joining: never finish with an owned initializer waiting.
    try { await Promise.all(launched.map(boot => boot.join())); }
    finally { sandbox.close(); await f.close(); }
  }
});

test("pod epoch private API CA survives both real UID launch chains without relaxing preload refusal", { timeout: 60000 }, async () => {
  const f = fixture(), sandbox = entrypointFixture(f.root);
  const secret = "/run/uzi-epoch-ca-" + randomUUID();
  const launched: ReturnType<typeof sandbox.start>[] = [];
  let created = false;
  let holder: Awaited<ReturnType<typeof f.runtime.acquire>> | undefined;
  try {
    fs.mkdirSync(secret, { mode: 0o755 }); created = true;
    const generation = path.join(secret, "..generation");
    fs.mkdirSync(generation);
    const ca = path.join(generation, "ca.crt");
    // Public test-only CA; no private key is needed to prove Node loads it.
    // The image's OpenSSL lives under /nix, masked by this lane's empty tmpfs.
    fs.writeFileSync(ca, `-----BEGIN CERTIFICATE-----
MIIBlDCCATugAwIBAgIUYNB2ixWrk3AVkzhcIqyTBCqEdlowCgYIKoZIzj0EAwIw
HzEdMBsGA1UEAwwUcG9kLWVwb2NoLXByaXZhdGUtY2EwIBcNMjYxMDI1MTcyNzAz
WhgPMjEyNjEwMDExNzI3MDNaMB8xHTAbBgNVBAMMFHBvZC1lcG9jaC1wcml2YXRl
LWNhMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEw+4PV9pO6YOSorIWbVXkiGua
72YEn+NdRq2RBVDWkn4hiYA0A8cFmuOriO9k9Uxvj+LvlaAgsJa4KOY8CU0x6qNT
MFEwHQYDVR0OBBYEFOyRYyhou7ybrD0H94lhC/d+GU5qMB8GA1UdIwQYMBaAFOyR
Yyhou7ybrD0H94lhC/d+GU5qMA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwID
RwAwRAIgYSUU8KMJPlJUMpgJc/bqxWWAgYSb+SIiu/ecrfw3i3oCIE9xWEc+hWat
II9AfazIwMDPRH6tsY2oac56bpTHDdCS
-----END CERTIFICATE-----
`);
    fs.chmodSync(ca, 0o440); fs.chownSync(ca, 0, 10001);
    fs.writeFileSync(path.join(generation, "worker_token"), "epoch-test-token", { mode: 0o440 });
    fs.chownSync(path.join(generation, "worker_token"), 0, 10001);
    fs.symlinkSync("..generation", path.join(secret, "..data"));
    for (const file of ["ca.crt", "worker_token"]) fs.symlinkSync("..data/" + file, path.join(secret, file));
    const extra = { NODE_EXTRA_CA_CERTS: path.join(secret, "ca.crt"), UZI_WORKER_TOKEN_FILE: path.join(secret, "worker_token") };
    await f.runtime.seed(S);
    assert.notEqual(sandbox.reject({ ...extra, NODE_OPTIONS: "--require=/data/untrusted.cjs" }).status, 0,
      "private CA never permits executable preloads");
    for (const single of [false, true]) {
      const boot = sandbox.start(single, undefined, extra); launched.push(boot);
      await boot.wait("EPOCH_INIT_DONE");
      holder = await f.exclusive();
      const before = protectedSnapshot(f.root);
      boot.child.stdin.write("continue\n");
      await boot.wait("EPOCH_NPM_READY");
      await boot.wait("EPOCH_MAIN_LOCK_CONFLICT");
      assert.ok(protectedSnapshot(f.root) === before, "CA bootstrap waits without protected writes");
      holder.close(); holder = undefined;
      await boot.wait("EPOCH_MAIN_OWNED");
      assert.equal(flock(f.root, "-x"), 200, "CA worker main retains SH before configuration");
      boot.child.stdin.write("stop\n");
      await boot.join();
    }
    const log = fs.readFileSync(sandbox.log, "utf8");
    assert.equal(log.split("\n").filter(line => line.startsWith("CA_TRUST ")).length, 2,
      "both actual Node trust stores loaded the projected CA");
    assert.ok(!log.includes("POSTURE_VIOLATION"));
    const stat = fs.statSync(ca);
    assert.equal(stat.uid, 0); assert.equal(stat.gid, 10001); assert.equal(stat.mode & 0o7777, 0o440);
  } finally {
    holder?.close();
    try { await Promise.all(launched.map(boot => boot.join())); }
    finally { if (created) fs.rmSync(secret, { recursive: true }); sandbox.close(); await f.close(); }
  }
});

test("pod epoch restores observable persistent worker and runner scratch in both UID modes and refuses invalid saved paths", { timeout: 60000 }, async () => {
  const f = fixture();
  try {
    await f.runtime.seed(S);
    for (const split of [false, true]) {
      for (const [dir, mode] of [["/data/runner", 0o3775], ["/data/runner/.tmp", 0o755],
        ["/data/runner/.tmp/uzi-worker", 0o700], ["/data/runner/.tmp/uzi-runner", 0o700]] as const) {
        if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });
        // Reclaim before chmod: this image intentionally has no FOWNER.
        fs.chownSync(dir, 0, 0); fs.chmodSync(dir, mode);
        fs.chownSync(dir, dir.endsWith("/uzi-runner") ? 10002 : 10001, dir.endsWith("/uzi-runner") ? 10002 : 10001);
      }
      const a = await f.child(split); await a.ask({ op: "acquire" });
      for (const saved of [null, "/tmp/uzi-epoch-bootstrap", "/data/runner"]) {
        assert.match((await a.raw({ op: "restore", split, saved })).error, /invalid saved epoch runtime TMPDIR/);
      }
      const expected = "/data/runner/.tmp" + (split ? "/uzi-worker" : "");
      const restored = await a.ask({ op: "restore", split });
      assert.equal(restored.env, expected, "post-lock process.env.TMPDIR restored");
      assert.equal(restored.os, expected, "post-lock os.tmpdir restored");
      assert.equal(path.dirname(restored.made), expected, "actual worker temp creation uses restored runtime TMPDIR");
      assert.equal(restored.saved, null, "reserved saved path removed");
      const runner = split ? "/data/runner/.tmp/uzi-runner" : expected;
      assert.equal(restored.runner.tmp, runner, "runner child scratch uses persistent lane");
      assert.equal(path.dirname(restored.runner.made), runner);
      await a.exit();
    }
  } finally { await f.close(); }
});

// Issue #4043: the scratch census and strict delete across the real UID split. The worker (uid 10001, with the
// production SETUID/SETGID ambient caps) pins the 0700 runner-owned root through descriptors, reads it only
// through the runner-uid census helper, and removes one runner-owned leaf with the runner-uid strict delete. The
// worker itself cannot list the root, which is why the census is a runner-uid child. Classification is the only
// fake in this lane (tmpfs, not ext4/XFS), so the worker-side filesystem gate is not exercised here.
const SCRATCH_PROGRAM = `
const [scratchUrl, rmUrl, uidUrl, leaf] = process.argv.slice(1);
const fs = await import("node:fs");
const cp = await import("node:child_process");
const scratch = await import(scratchUrl), rm = await import(rmUrl), ids = await import(uidUrl);
const out = { uid: process.getuid(), gid: process.getgid() };
const layout = scratch.scratchLayout(process.env);
out.layout = layout.mode;
const pin = scratch.pinScratchRoot(layout);
out.pinned = !("refused" in pin);
if (!out.pinned) { out.refused = pin.refused; console.log(JSON.stringify(out)); process.exit(0); }
try { fs.readdirSync(layout.root); out.workerCanList = true; } catch (e) { out.workerCanList = false; out.listCode = e.code; }
const raw = await rm.censusScratchRoot(layout.root, pin.devIno, undefined, 60000);
const parsed = scratch.parseScratchCensus(raw, pin.devIno);
out.census = "refused" in parsed ? { refused: parsed.refused, code: raw.code } : { rootUid: parsed.rootUid,
  entries: parsed.entries.map(e => ({ name: e.name, type: e.type, uid: e.uid, foreign: e.foreignUid, incomplete: e.incomplete })) };
const entry = "refused" in parsed ? undefined : parsed.entries.find(e => e.name === leaf);
if (entry) {
  const lim = rm.SCRATCH_DELETE_LIMITS;
  const w = ids.runnerCommand(process.execPath, ["-e", rm.PINNED_SUBTREE_SCRIPT, layout.root, leaf, String(lim.maxEntries),
    String(lim.budgetMs), "remove", entry.devIno, "", "pinned", pin.devIno, "1", String(lim.maxDepth)]);
  const r = cp.spawnSync(w.command, w.args, { env: { PATH: "/usr/bin:/bin" }, cwd: "/", encoding: "utf8", timeout: 60000 });
  out.delete = { status: r.status, stdout: r.stdout, expect: entry.devIno };
}
console.log(JSON.stringify(out));
`;

test("pod epoch real split: the worker pins the runner scratch root, the runner-uid census reads it and the strict delete removes a leaf", { timeout: 90000 }, async () => {
  assert.equal(process.platform, "linux", "image proof requires Linux; never skip");
  assert.equal(process.getuid!(), 0, "image proof requires the shared root window");
  const runnerRoot = "/data/runner/.tmp/uzi-runner", leaf = "scratch-real-leaf";
  for (const [dir, mode] of [["/data/runner", 0o3775], ["/data/runner/.tmp", 0o755],
    ["/data/runner/.tmp/uzi-worker", 0o700], [runnerRoot, 0o700]] as const) {
    if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });
    // Reclaim before chmod: this image intentionally has no FOWNER.
    fs.chownSync(dir, 0, 0); fs.chmodSync(dir, mode);
    const runner = dir === runnerRoot;
    fs.chownSync(dir, runner ? 10002 : 10001, runner ? 10002 : 10001);
  }
  const leafDir = path.join(runnerRoot, leaf);
  fs.rmSync(leafDir, { recursive: true, force: true });
  fs.mkdirSync(path.join(leafDir, "nested", "deeper"), { recursive: true });
  fs.writeFileSync(path.join(leafDir, "nested", "deeper", "payload"), "x".repeat(4096));
  fs.chownSync(leafDir, 10002, 10002);
  fs.chownSync(path.join(leafDir, "nested"), 10002, 10002);
  fs.chownSync(path.join(leafDir, "nested", "deeper"), 10002, 10002);
  fs.chownSync(path.join(leafDir, "nested", "deeper", "payload"), 10002, 10002);
  try {
    const url = (relative: string) => new URL(relative, import.meta.url).href;
    const worker = spawnSync("/bin/setpriv", ["--reuid", "worker", "--regid", "worker", "--init-groups",
      "--bounding-set", "-all,+setuid,+setgid", "--inh-caps", "-all,+setuid,+setgid", "--ambient-caps", "-all,+setuid,+setgid", "--",
      process.execPath, "--import", loader, "--input-type=module", "-e", SCRATCH_PROGRAM,
      url("../../agent/src/pod-epoch-scratch.ts"), url("../../agent/src/rmtree.ts"), url("../../agent/src/runner-uid.ts"), leaf], {
      env: { PATH: "/usr/local/bin:/usr/bin:/bin", TSX_DISABLE_CACHE: "1", UZI_UID_SPLIT: "1",
        UZI_RUNNER_TMPDIR: runnerRoot, TMPDIR: "/data/runner/.tmp/uzi-worker" },
      encoding: "utf8", timeout: 80000 });
    assert.equal(worker.status, 0, worker.stderr);
    const out = JSON.parse(worker.stdout.trim().split("\n").at(-1)!);
    assert.equal(out.uid, 10001, "the pass runs as the worker");
    assert.equal(out.layout, "split");
    assert.equal(out.pinned, true, "the worker pins the 0700 runner-owned root: " + out.refused);
    assert.equal(out.workerCanList, false, "the worker cannot list the runner root itself");
    assert.equal(out.census.rootUid, 10002, JSON.stringify(out.census));
    const entry = out.census.entries.find((e: { name: string }) => e.name === leaf);
    assert.deepEqual(entry, { name: leaf, type: "dir", uid: 10002, foreign: false, incomplete: false });
    assert.equal(out.delete.status, 0, JSON.stringify(out.delete));
    assert.equal(out.delete.stdout, `UZI-SCRATCH-REMOVED ${out.delete.expect}\n`, "the runner-uid delete reports its own receipt");
    assert.equal(fs.existsSync(leafDir), false);
    assert.equal(fs.existsSync("/data/runner/.tmp/uzi-worker"), true, "the protected container name is untouched");
  } finally { fs.rmSync(leafDir, { recursive: true, force: true }); }
});

// Issue #4420 m4: the run-owned TMPDIR leaf on the real image. The worker (uid 10001, production ambient caps)
// calls prepareRunTmpdir twice (the split helper runs as the runner uid through runnerCommand), stats the leaf,
// lists it through the runner-uid census and removes it with the same strict delete as the test above. This
// drives creation, census and strict delete directly; it does not drive runPodEpochShadow admission.
const RUN_LEAF_PROGRAM = `
const [scratchUrl, rmUrl, uidUrl, tmpUrl, runId, stage] = process.argv.slice(1);
const fs = await import("node:fs");
const cp = await import("node:child_process");
const scratch = await import(scratchUrl), rm = await import(rmUrl), ids = await import(uidUrl), rt = await import(tmpUrl);
const out = { uid: process.getuid() };
const layout = scratch.scratchLayout(process.env);
out.layout = layout.mode;
const leaf = "run-" + runId;
if (stage === "prepare") {
  out.prepared = await rt.prepareRunTmpdir(runId);
  console.log(JSON.stringify(out)); process.exit(0);
}
const pin = scratch.pinScratchRoot(layout);
if ("refused" in pin) { out.refused = pin.refused; console.log(JSON.stringify(out)); process.exit(0); }
const raw = await rm.censusScratchRoot(layout.root, pin.devIno, undefined, 60000);
const parsed = scratch.parseScratchCensus(raw, pin.devIno);
const entry = "refused" in parsed ? undefined : parsed.entries.find(e => e.name === leaf);
out.entry = entry ? { name: entry.name, type: entry.type, uid: entry.uid, foreign: entry.foreignUid, incomplete: entry.incomplete } : null;
if (entry) {
  const lim = rm.SCRATCH_DELETE_LIMITS;
  const w = ids.runnerCommand(process.execPath, ["-e", rm.PINNED_SUBTREE_SCRIPT, layout.root, leaf, String(lim.maxEntries),
    String(lim.budgetMs), "remove", entry.devIno, "", "pinned", pin.devIno, "1", String(lim.maxDepth)]);
  const r = cp.spawnSync(w.command, w.args, { env: { PATH: "/usr/bin:/bin" }, cwd: "/", encoding: "utf8", timeout: 60000 });
  out.delete = { status: r.status, stdout: r.stdout, expect: entry.devIno };
}
console.log(JSON.stringify(out));
`;

type RunLeafCase = { name: string; split: boolean; root: string; rootMode: number; ownerUid: number; tmpdir: string };
const RUN_LEAF_CASES: Record<"split" | "single0755" | "single3775", RunLeafCase> = {
  split: { name: "split", split: true, root: "/data/runner/.tmp/uzi-runner", rootMode: 0o700, ownerUid: 10002, tmpdir: "/data/runner/.tmp/uzi-worker" },
  single0755: { name: "single 0755", split: false, root: "/data/runner/.tmp", rootMode: 0o755, ownerUid: 10001, tmpdir: "/data/runner/.tmp" },
  single3775: { name: "single 03775", split: false, root: "/data/runner/.tmp", rootMode: 0o3775, ownerUid: 10001, tmpdir: "/data/runner/.tmp" },
};

async function runLeafProof(c: RunLeafCase) {
    assert.equal(process.platform, "linux", "image proof requires Linux; never skip");
    assert.equal(process.getuid!(), 0, "image proof requires the shared root window");
    const runId = randomUUID(), name = `run-${runId}`;
    for (const [dir, mode] of [["/data/runner", 0o3775], ["/data/runner/.tmp", c.split ? 0o755 : c.rootMode],
      ["/data/runner/.tmp/uzi-worker", 0o700], ["/data/runner/.tmp/uzi-runner", 0o700]] as const) {
      if (!fs.existsSync(dir)) fs.mkdirSync(dir, { recursive: true });
      // Reclaim before chmod: this image intentionally has no FOWNER.
      fs.chownSync(dir, 0, 0); fs.chmodSync(dir, mode);
      const runner = dir === "/data/runner/.tmp/uzi-runner";
      fs.chownSync(dir, runner ? 10002 : 10001, runner ? 10002 : 10001);
    }
    const leafDir = path.join(c.root, name);
    fs.rmSync(leafDir, { recursive: true, force: true });
    try {
      const url = (relative: string) => new URL(relative, import.meta.url).href;
      const env: Record<string, string> = { PATH: "/usr/local/bin:/usr/bin:/bin", TSX_DISABLE_CACHE: "1", TMPDIR: c.tmpdir };
      if (c.split) { env.UZI_UID_SPLIT = "1"; env.UZI_RUNNER_TMPDIR = c.root; }
      const asWorker = (stage: string) => {
        const worker = spawnSync("/bin/setpriv", ["--reuid", "worker", "--regid", "worker", "--init-groups",
          "--bounding-set", "-all,+setuid,+setgid", "--inh-caps", "-all,+setuid,+setgid", "--ambient-caps", "-all,+setuid,+setgid", "--",
          process.execPath, "--import", loader, "--input-type=module", "-e", RUN_LEAF_PROGRAM,
          url("../../agent/src/pod-epoch-scratch.ts"), url("../../agent/src/rmtree.ts"), url("../../agent/src/runner-uid.ts"),
          url("../../agent/src/run-tmpdir.ts"), runId, stage], { env, encoding: "utf8", timeout: 80000 });
        assert.equal(worker.status, 0, worker.stderr);
        const out = JSON.parse(worker.stdout.trim().split("\n").at(-1)!);
        assert.equal(out.uid, 10001, "the pass runs as the worker");
        assert.equal(out.layout, c.split ? "split" : "single");
        return out;
      };
      // The root-window test process (root with DAC_OVERRIDE) reads the leaf the worker cannot stat in a 0700 runner root.
      const first = asWorker("prepare");
      assert.deepEqual(first.prepared, { dir: leafDir }, JSON.stringify(first.prepared));
      const st = fs.lstatSync(leafDir);
      assert.equal(st.isDirectory(), true);
      assert.equal(st.uid, c.ownerUid, "the leaf is owned by the layout owner");
      assert.equal(st.mode & 0o7777, 0o700, "the mode is exactly 0700 (setgid cleared)");
      const second = asWorker("prepare");
      assert.deepEqual(second.prepared, { dir: leafDir }, "a second call reuses the leaf");
      assert.equal(fs.lstatSync(leafDir).ino, st.ino, "the second call adopts the same directory");
      const out = asWorker("census-delete");
      assert.equal(out.refused, undefined, out.refused);
      assert.deepEqual(out.entry, { name, type: "dir", uid: c.ownerUid, foreign: false, incomplete: false }, "the census lists the run leaf");
      assert.equal(out.delete.status, 0, JSON.stringify(out.delete));
      assert.equal(out.delete.stdout, `UZI-SCRATCH-REMOVED ${out.delete.expect}\n`);
      assert.equal(fs.existsSync(leafDir), false, "the strict delete removed the run leaf");
    } finally { fs.rmSync(leafDir, { recursive: true, force: true }); }
}

test("pod epoch real split: the run TMPDIR leaf is runner-owned 0700, reused, listed by the census and strictly deleted", { timeout: 90000 }, async () => {
  await runLeafProof(RUN_LEAF_CASES.split);
});
test("pod epoch real single 0755 root: the run TMPDIR leaf is worker-owned 0700, reused, listed by the census and strictly deleted", { timeout: 90000 }, async () => {
  await runLeafProof(RUN_LEAF_CASES.single0755);
});
test("pod epoch real single 03775 root: the run TMPDIR leaf is worker-owned 0700 with setgid cleared, reused, listed by the census and strictly deleted", { timeout: 90000 }, async () => {
  await runLeafProof(RUN_LEAF_CASES.single3775);
});

// ---- Issue #4263: GitCache.discardRetireScratchParent under the real uid split ---------------------------------
// The tree is `<runner root>/.retire-<uuid>/clone`: a worker-owned sticky root, a worker-owned 0700 parent and a
// runner-owned clone. The worker child (uid 10001, the production ambient SETUID/SETGID) runs the production method
// with a real reclaim scope, so the strict `empty-fd` helper really runs as the runner uid (setpriv) with the clone
// pin as fd 3 and the epoch EX lock as fd 4. Only the spawn seam is wrapped (see retireOp in pod-epoch-child.mts):
// a worker-uid shell waits for a release file and execs the unchanged production argv, which lets a test race the
// tree after the pins and before the removal, or kill the worker while the helper is pending.
const WORKER_ID = 10001, RUNNER_ID = 10002, CMD_ID = 10003;
const RETIRE_PARENT = ".retire-0b9f4a52-3c1d-4e8a-9b7e-1f2a3b4c5d6e";

interface RetireTree { base: string; root: string; parent: string; clone: string; outside: string; elsewhere: string }

function own(p: string, uid: number, gid: number, mode?: number) {
  // Root holds CHOWN but not FOWNER or FSETID: chmod while root owns the entry, and with its own group so a setgid bit
  // (a directory made under a setgid parent inherits that parent's group) is kept rather than stripped.
  if (mode !== undefined) { fs.lchownSync(p, 0, 0); fs.chmodSync(p, mode); }
  fs.lchownSync(p, uid, gid);
}
function put(p: string, content: string, uid: number, gid: number, mode: number) {
  fs.writeFileSync(p, content); own(p, uid, gid, mode);
}
function retireTree(populate: (t: RetireTree) => void,
  o: { rootOwner?: [number, number]; rootMode?: number; parentOwner?: [number, number]; parentMode?: number } = {}): RetireTree {
  assert.equal(process.platform, "linux", "image proof requires Linux; never skip");
  assert.equal(process.getuid!(), 0, "image proof requires the shared root window");
  const base = fs.mkdtempSync("/data/retire-proof-");
  const t: RetireTree = { base, root: path.join(base, "root"), parent: "", clone: "", outside: path.join(base, "outside"),
    elsewhere: path.join(base, "elsewhere") };
  t.parent = path.join(t.root, RETIRE_PARENT); t.clone = path.join(t.parent, "clone");
  try {
    fs.chmodSync(base, 0o755);
    for (const dir of [t.root, t.parent, t.clone, t.outside, t.elsewhere]) fs.mkdirSync(dir, { recursive: true });
    put(path.join(t.outside, "keep.txt"), "outside-survives", RUNNER_ID, RUNNER_ID, 0o644);
    own(t.outside, RUNNER_ID, RUNNER_ID, 0o755); own(t.elsewhere, RUNNER_ID, RUNNER_ID, 0o755);
    // Production clone posture: runner:runner 2775 (setgid), like every directory the runner creates inside it.
    own(t.clone, RUNNER_ID, RUNNER_ID, 0o2775);
    populate(t);
    const [pu, pg] = o.parentOwner ?? [WORKER_ID, WORKER_ID];
    own(t.parent, pu, pg, o.parentMode ?? 0o700);
    const [ru, rg] = o.rootOwner ?? [WORKER_ID, RUNNER_ID];
    own(t.root, ru, rg, o.rootMode ?? 0o1775);
    return t;
  } catch (error) { dropTree(t); throw error; }
}
function dropTree(t: RetireTree) {
  spawnSync("/bin/chown", ["-R", "-h", "0:0", t.base]);
  fs.rmSync(t.base, { recursive: true, force: true });
}
/** Mode, owner and content (or link target) of everything under `p`, as root; the comparison key of a byte-identical tree. */
function digest(p: string, rel = "."): string[] {
  const st = fs.lstatSync(p);
  const meta = `${rel}|${st.isDirectory() ? "d" : st.isSymbolicLink() ? "l" : "f"}|${(st.mode & 0o7777).toString(8)}|${st.uid}:${st.gid}`;
  if (st.isSymbolicLink()) return [`${meta}|${fs.readlinkSync(p)}`];
  if (!st.isDirectory()) return [`${meta}|${fs.readFileSync(p, "utf8")}`];
  return [meta, ...fs.readdirSync(p).sort().flatMap(name => digest(path.join(p, name), `${rel}/${name}`))];
}
/** Closes the fixture without letting a join failure hide the assertion that failed the test body. */
async function closeAfter(f: Fixture, primary: unknown, cleanup: () => void) {
  try { await f.close(); }
  catch (error) { if (primary === undefined) throw error; }
  finally { cleanup(); }
}
let retireSeq = 0;
function retireReq(f: Fixture, t: RetireTree, extra: object = {}) {
  const n = ++retireSeq;
  return { op: "retire", root: t.root, parentName: RETIRE_PARENT, cache: path.join(f.root, "cache"), race: "none",
    outside: t.outside, elsewhere: t.elsewhere,
    gate: { started: path.join(f.root, `gate-${n}-started`), release: path.join(f.root, `gate-${n}-release`) }, ...extra };
}
async function retireOwner(f: Fixture) {
  await f.runtime.seed(S);
  const owner = await f.child(true);
  assert.equal(await owner.ask({ op: "acquire", mode: "exclusive" }), "exclusive");
  return owner;
}
const POPULATED = (t: RetireTree) => {
  const { clone, outside } = t;
  fs.mkdirSync(path.join(clone, "nested", "deeper"), { recursive: true });
  fs.mkdirSync(path.join(clone, "shared"));
  put(path.join(clone, "top.txt"), "t", RUNNER_ID, RUNNER_ID, 0o644);
  put(path.join(clone, "nested", "deeper", "payload"), "p".repeat(4096), RUNNER_ID, RUNNER_ID, 0o644);
  // A runner-cmd-owned, group-runner-writable file inside a runner-owned directory: the runner owns the directory, so it can unlink it.
  put(path.join(clone, "shared", "cmd.txt"), "from runner-cmd", CMD_ID, RUNNER_ID, 0o660);
  // Judgment A: a runner-cmd-owned directory whose group is runner and group-writable. The runner can write and search it
  // through the group, so it unlinks the runner-cmd file inside and then removes the directory from its runner-owned parent.
  fs.mkdirSync(path.join(clone, "cmd-group"));
  put(path.join(clone, "cmd-group", "owned.txt"), "from runner-cmd", CMD_ID, CMD_ID, 0o640);
  own(path.join(clone, "cmd-group"), CMD_ID, RUNNER_ID, 0o2775);
  fs.symlinkSync(outside, path.join(clone, "nested", "outward")); fs.lchownSync(path.join(clone, "nested", "outward"), RUNNER_ID, RUNNER_ID);
  for (const dir of ["nested", "nested/deeper", "shared"]) own(path.join(clone, dir), RUNNER_ID, RUNNER_ID, 0o2775);
};

test("pod epoch real retire scratch: the runner-uid empty-fd helper reclaims a populated clone, the worker removes clone and parent and an outward symlink target survives", { timeout: 150000 }, async () => {
  const f = fixture(); let t: RetireTree | undefined;
  let primary: unknown;
  try {
    t = retireTree(POPULATED);
    const owner = await retireOwner(f);
    const out = await owner.ask(retireReq(f, t), 90000);
    assert.deepEqual(out.result, { kind: "removed" }, JSON.stringify(out));
    assert.equal(out.spawns, 1, "exactly one runner pass");
    assert.equal(out.pending, 0); assert.equal(out.stopped, false); assert.deepEqual(out.unsettled, []);
    assert.equal(fs.existsSync(t.parent), false, "parent and clone are gone");
    assert.deepEqual(fs.readdirSync(t.root), [], "nothing else was created or left under the root");
    assert.equal(fs.readFileSync(path.join(t.outside, "keep.txt"), "utf8"), "outside-survives", "the symlink target outside the clone survived");
    assert.equal(flock(f.root, "-x"), 200, "the owner still holds EX after the discard");
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => t && dropTree(t)); }
});

test("pod epoch real retire scratch: the production retire writer keeps the scratch parent 0700 under the setgid 3775 runner root and the discard removes it", { timeout: 200000 }, async () => {
  const f = fixture(); let base: string | undefined;
  let primary: unknown;
  try {
    // The entrypoint posture for the runner root (agent/templates/entrypoint.sh): worker:runner, sticky and setgid.
    base = fs.mkdtempSync("/data/retire-prod-");
    fs.chmodSync(base, 0o755); fs.chownSync(base, WORKER_ID, WORKER_ID);
    const runnerRoot = path.join(base, "runner");
    fs.mkdirSync(runnerRoot);
    own(runnerRoot, WORKER_ID, RUNNER_ID, 0o3775);
    assert.equal(fs.statSync(runnerRoot).mode & 0o7777, 0o3775, "the runner root carries sticky and setgid");
    const owner = await retireOwner(f);
    const made = await owner.ask({ op: "retire-production", dataDir: base }, 120000);
    assert.equal(made.runnerRoot, runnerRoot);
    assert.equal(made.canonicalGone, true, "the clone moved into the scratch parent");
    assert.equal(made.cloneExists, true);
    assert.match(made.bindings, /uzi-retire-scratch\./, "the parent is durably bound");
    assert.equal(made.parentUid, WORKER_ID);
    assert.equal(made.parentMode.toString(8), "700", "a setgid root must not leave 02700 on the scratch parent");
    for (const dir of ["outside", "elsewhere"]) fs.mkdirSync(path.join(base, dir));
    const t: RetireTree = { base, root: runnerRoot, parent: path.join(runnerRoot, made.parentName), clone: path.join(runnerRoot, made.parentName, "clone"),
      outside: path.join(base, "outside"), elsewhere: path.join(base, "elsewhere") };
    const out = await owner.ask({ ...retireReq(f, t), parentName: made.parentName }, 90000);
    assert.deepEqual(out.result, { kind: "removed" }, JSON.stringify(out));
    assert.equal(fs.existsSync(t.parent), false, "parent and clone are gone");
  } catch (e) { primary = e; throw e; } finally {
    await closeAfter(f, primary, () => base && dropTree({ base } as RetireTree));
  }
});

test("pod epoch real retire scratch: directories the runner cannot list or modify survive byte-identical as residue with no chmod", { timeout: 150000 }, async () => {
  const f = fixture(); let t: RetireTree | undefined;
  let primary: unknown;
  try {
    t = retireTree(tree => {
      const tracked = path.join(tree.clone, "tracked");
      fs.mkdirSync(path.join(tracked, "hidden", "sub"), { recursive: true });
      fs.mkdirSync(path.join(tracked, "drop")); fs.mkdirSync(path.join(tracked, "locked"));
      put(path.join(tracked, "plain.txt"), "removable", RUNNER_ID, RUNNER_ID, 0o644);
      put(path.join(tracked, "hidden", "data.txt"), "runner-cmd-secret", CMD_ID, CMD_ID, 0o600);
      put(path.join(tracked, "hidden", "sub", "x"), "x", CMD_ID, CMD_ID, 0o600);
      put(path.join(tracked, "drop", "y.txt"), "dropped", CMD_ID, CMD_ID, 0o600);
      // A runner-OWNED directory without write permission: a chmod walk (the non-strict mode) would add it and empty it.
      put(path.join(tracked, "locked", "kept.txt"), "runner-owned-in-0500", RUNNER_ID, RUNNER_ID, 0o644);
      own(path.join(tracked, "hidden", "sub"), CMD_ID, CMD_ID, 0o700);
      own(path.join(tracked, "hidden"), CMD_ID, CMD_ID, 0o700);
      // Group runner may write and search but not list: the runner cannot enumerate the entry it would have to unlink.
      own(path.join(tracked, "drop"), CMD_ID, RUNNER_ID, 0o730);
      own(path.join(tracked, "locked"), RUNNER_ID, RUNNER_ID, 0o500);
      own(tracked, RUNNER_ID, RUNNER_ID, 0o2775);
    });
    const before = digest(t.clone);
    assert.ok(before.includes("./tracked|d|2775|10002:10002") && before.includes("./tracked/locked|d|500|10002:10002"),
      "the fixture mirrors the production runner:runner 2775 setgid directories and holds the 0500 runner-owned one");
    const hidden = path.join(t.clone, "tracked", "hidden"), drop = path.join(t.clone, "tracked", "drop");
    assert.ok(digest(hidden).length >= 4 && digest(drop).length === 2, "fixture holds content");
    const owner = await retireOwner(f);
    const out = await owner.ask(retireReq(f, t), 90000);
    assert.equal(out.result.kind, "residue", JSON.stringify(out));
    assert.equal(out.result.reason, "helper_remaining");
    assert.equal(out.result.exit, 4);
    assert.equal(out.spawns, 1, "exactly one runner pass");
    assert.equal(out.pending, 0, "the ticket is settled"); assert.deepEqual(out.unsettled, []);
    // The whole clone (modes including directories, owners, content) differs only by the file the runner could remove.
    assert.deepEqual(digest(t.clone), before.filter(line => !line.startsWith("./tracked/plain.txt|")),
      "only the runner-removable file went; every other mode, owner and byte (0700, 0730, 0500) is unchanged");
    assert.equal(fs.existsSync(path.join(t.clone, "tracked", "plain.txt")), false, "what the runner could remove was removed");
    assert.equal(fs.readFileSync(path.join(drop, "y.txt"), "utf8"), "dropped", "the unlistable 0730 directory kept its content");
    assert.equal(fs.existsSync(t.parent), true, "the parent is kept");
    assert.equal(fs.statSync(t.clone).isDirectory(), true, "the clone is kept");
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => t && dropTree(t)); }
});

test("pod epoch real retire scratch: a runner-cmd directory the runner can list but not unlink from survives byte-identical as residue", { timeout: 150000 }, async () => {
  const f = fixture(); let t: RetireTree | undefined;
  let primary: unknown;
  try {
    t = retireTree(tree => {
      const tracked = path.join(tree.clone, "tracked");
      fs.mkdirSync(path.join(tracked, "readonly"), { recursive: true });
      put(path.join(tracked, "readonly", "cmd.txt"), "runner-cmd-content", CMD_ID, CMD_ID, 0o644);
      // Owner and group runner-cmd, 0755: the runner has only other r-x, so it lists the file and cannot unlink it.
      own(path.join(tracked, "readonly"), CMD_ID, CMD_ID, 0o755);
      own(tracked, RUNNER_ID, RUNNER_ID, 0o2775);
    });
    const before = digest(t.clone);
    assert.ok(before.some(line => line.startsWith("./tracked/readonly/cmd.txt|")), "fixture holds the file");
    const owner = await retireOwner(f);
    const out = await owner.ask(retireReq(f, t), 90000);
    assert.equal(out.result.kind, "residue", JSON.stringify(out));
    assert.equal(out.result.reason, "helper_remaining");
    assert.equal(out.result.exit, 4);
    assert.equal(out.spawns, 1, "exactly one runner pass");
    assert.equal(out.pending, 0, "the ticket is settled"); assert.deepEqual(out.unsettled, []);
    assert.deepEqual(digest(t.clone), before, "the whole clone is byte-identical: nothing was chmodded, re-owned or removed");
    assert.equal(fs.readFileSync(path.join(t.clone, "tracked", "readonly", "cmd.txt"), "utf8"), "runner-cmd-content");
    assert.equal(fs.existsSync(t.parent), true, "the parent is kept");
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => t && dropTree(t)); }
});

// The swaps below are staged by the spawn seam AFTER the pins and BEFORE the helper runs (the two "before" clone swaps and
// the descendant swap) or when the helper process exits and before the worker's removal (the "after" clone swap). None is
// a mid-walk race against the helper's own traversal: that TOCTOU is covered by the hermetic `rechecked` seam tests.
test("pod epoch real retire scratch: a clone swapped around the helper run is never followed or removed and a descendant swapped before the helper is unlinked, never followed", { timeout: 240000 }, async () => {
  const f = fixture();
  let primary: unknown;
  const trees: RetireTree[] = [];
  const mk = (populate = POPULATED) => { const t = retireTree(populate); trees.push(t); return t; };
  try {
    const owner = await retireOwner(f);
    const survivors = (t: RetireTree) => assert.equal(fs.readFileSync(path.join(t.outside, "keep.txt"), "utf8"), "outside-survives");
    for (const [race, plant] of [["clone-symlink-before", "symlink"], ["clone-dir-before", "dir"], ["clone-dir-after", "dir"]] as const) {
      const t = mk();
      const out = await owner.ask(retireReq(f, t, { race }), 90000);
      assert.equal(out.result.kind, "residue", race + " " + JSON.stringify(out));
      assert.equal(out.result.reason, "clone_identity_changed", race);
      assert.equal(out.spawns, 1, race + ": exactly one runner pass");
      assert.equal(out.pending, 0, race); assert.deepEqual(out.unsettled, [], race);
      assert.equal(fs.existsSync(t.parent), true, race + ": the parent is kept");
      assert.equal(fs.lstatSync(t.clone).isSymbolicLink(), plant === "symlink", race + ": the planted entry is untouched");
      assert.equal(fs.existsSync(path.join(t.parent, "moved")), true, race + ": the bound clone survives under its new name");
      survivors(t);
      assert.deepEqual(fs.readdirSync(t.outside).sort(), ["keep.txt"], race + ": nothing was written outside");
    }
    // A descendant swapped for a symlink to an outside directory, with the real subtree moved out: the helper unlinks the
    // symlink and never follows it. The clone is then genuinely empty, so removal is correct; the proof is that nothing outside went.
    const t = mk();
    const out = await owner.ask(retireReq(f, t, { race: "descendant" }), 90000);
    assert.deepEqual(out.result, { kind: "removed" }, JSON.stringify(out));
    assert.equal(out.spawns, 1, "exactly one runner pass");
    assert.equal(out.pending, 0); assert.deepEqual(out.unsettled, []);
    assert.equal(fs.existsSync(t.parent), false, "parent and clone are gone");
    survivors(t);
    assert.deepEqual(fs.readdirSync(t.outside).sort(), ["keep.txt"], "the symlink target was not emptied");
    assert.equal(fs.readFileSync(path.join(t.elsewhere, "moved-out", "deeper", "payload"), "utf8"), "p".repeat(4096), "the moved-out subtree is untouched");
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => { for (const t of trees) dropTree(t); }); }
});

test("pod epoch real retire scratch: the helper child sees only the clone pin at fd 3 and the EX lock at fd 4 and cannot reopen the worker-private parent", { timeout: 150000 }, async () => {
  const f = fixture(); let t: RetireTree | undefined;
  let primary: unknown;
  try {
    t = retireTree(POPULATED);
    const owner = await retireOwner(f);
    const out = await owner.ask({ ...retireReq(f, t), gate: undefined, probe: true, lockFile: path.join(f.root, ".uzi-epoch.lock") }, 90000);
    assert.equal(out.exit, 0, JSON.stringify(out));
    assert.equal(out.probe.uid, RUNNER_ID, "the helper runs as the runner uid");
    assert.deepEqual(out.probe.fds, { 3: { id: out.cloneId, dir: true }, 4: { id: out.lockId, dir: false } });
    assert.deepEqual(out.probe.reopen, { parentDotDot: "EACCES", listDotDot: "EACCES", siblingViaDotDot: "EACCES", statViaDotDot: "EACCES" },
      "the runner cannot open or traverse the worker-private parent through the pinned clone");
    // The full descriptor table of an identically wrapped second launch, read by a shell (a Node probe owns runtime descriptors of its
    // own, so only a shell can prove that nothing beyond stdio, the clone pin and the lock was inherited).
    const table = new Map<string, string>();
    for (const line of out.list.trimEnd().split("\n")) { const [fd, ...target] = line.split(" "); table.set(fd!, target.join(" ")); }
    assert.deepEqual([...table.keys()].sort(), ["0", "1", "2", "3", "4"], "no descriptor at all at 5 or above, of any type: " + out.list);
    for (const fd of ["0", "1", "2"]) assert.match(table.get(fd)!, /^(socket|pipe):\[\d+\]$/, "fd " + fd + " is the spawn stdio pipe");
    assert.equal(table.get("3"), t.clone, "fd 3 is the clone pin");
    assert.equal(table.get("4"), path.join(f.root, ".uzi-epoch.lock"), "fd 4 is the epoch lock");
    assert.equal(fs.existsSync(path.join(t.clone, "top.txt")), true, "the probe changes nothing");
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => t && dropTree(t)); }
});

test("pod epoch real retire scratch: a worker killed mid-helper leaves the binding at removing, so a successor keeps the parent and removes nothing", { timeout: 200000 }, async () => {
  const f = fixture(); let t: RetireTree | undefined;
  let primary: unknown;
  let releaseFile: string | undefined;
  try {
    t = retireTree(POPULATED);
    const owner = await retireOwner(f);
    const cache = path.join(f.root, "cache"), bare = path.join(cache, "bare.git");
    const uuid = RETIRE_PARENT.slice(".retire-".length);
    // A canonical settled binding in a real bare repository created by the worker uid.
    const settledRaw = await owner.ask({ op: "retire-bind", cache, bare, uuid, binding: {
      v: 1, class: "verified_successor", attemptId: "20261117T064707Z-g1-b72b1aee67da0c9f", runId: "run-4263",
      branch: "agent/issue-4263", clonePath: "/data/runner/proof/issue-4263", phase: "settled",
      parentIno: String(fs.statSync(t.parent).ino), cloneIno: String(fs.statSync(t.clone).ino), pod: "" } });
    assert.equal(JSON.parse(settledRaw).phase, "settled");
    const req = retireReq(f, t, { mark: { bare, uuid, settledRaw } });
    releaseFile = req.gate.release;
    assert.equal(await owner.ask({ ...req, gate: { ...req.gate, hold: true }, detached: true }, 90000), "launched");
    // The gate file appears only after the production removing transition ran and the helper child was spawned.
    await until("the helper child is pending with its descriptors", () => fs.existsSync(req.gate.started), 60000);
    assert.equal(flock(f.root, "-x"), 200, "EX is held while the worker lives");
    await owner.killSelf();
    assert.equal(flock(f.root, "-x"), 200, "the SIGKILLed worker cannot release EX while its helper child lives");
    fs.writeFileSync(req.gate.release, "");
    await until("the helper exit releases the lock", () => flock(f.root, "-x") === 0, 60000);
    assert.deepEqual(fs.readdirSync(t.clone), [], "the orphaned helper only emptied the pinned clone");
    assert.equal(fs.existsSync(t.parent), true, "the dead worker removed nothing else");
    // The successor is a fresh worker-uid process: it reads the binding back and decides.
    const successor = await f.child(true);
    const seen = await successor.ask({ op: "retire-eval", cache, bare, uuid, runnerRoot: t.root }, 60000);
    const removingRaw = JSON.stringify({ ...JSON.parse(settledRaw), phase: "removing" });
    assert.equal(seen.read.state, "read");
    assert.equal(seen.read.bindings.length, 1);
    assert.equal(seen.read.bindings[0].raw, removingRaw, "the removing value was durably written before the helper spawned");
    assert.equal(seen.read.bindings[0].parsed.phase, "removing");
    assert.deepEqual(seen.exam, { kind: "keep", reason: "interrupted_removal" }, "the successor keeps the parent");
    assert.equal(fs.existsSync(t.parent), true, "no second removal was attempted");
    assert.equal(fs.readFileSync(path.join(t.outside, "keep.txt"), "utf8"), "outside-survives");
  } catch (e) { primary = e; throw e; } finally {
    // Never leave a gated helper holding EX for a failed assertion; the gate shell also gives up on its own.
    if (releaseFile) { try { fs.writeFileSync(releaseFile, ""); } catch { /* the fixture root may be gone */ } }
    await closeAfter(f, primary, () => t && dropTree(t));
  }
});

// Best effort and nondeterministic by nature (the helper's `<sync>` seam is forbidden in production launches, so no
// deterministic mid-walk swap exists). What is asserted is outcome-invariant: whatever interleaving happens, nothing
// outside the clone is touched and the result is removed or residue.
test("pod epoch real retire scratch: a runner-uid racer swapping directories for outward symlinks while the helper walks a large tree never causes a deletion outside the clone", { timeout: 240000 }, async () => {
  const f = fixture(); let t: RetireTree | undefined;
  let primary: unknown;
  try {
    t = retireTree(tree => {
      POPULATED(tree);
      for (let d = 0; d < 4; d++) {
        const dir = path.join(tree.clone, "r" + d);
        fs.mkdirSync(dir); own(dir, RUNNER_ID, RUNNER_ID, 0o2775);
        for (let i = 0; i < 50; i++) put(path.join(dir, "f" + i), "x", RUNNER_ID, RUNNER_ID, 0o644);
      }
      for (let d = 0; d < 40; d++) {
        const dir = path.join(tree.clone, "bulk" + d);
        fs.mkdirSync(dir); own(dir, RUNNER_ID, RUNNER_ID, 0o2775);
        for (let i = 0; i < 250; i++) put(path.join(dir, "f" + i), "x", RUNNER_ID, RUNNER_ID, 0o644);
      }
    });
    const owner = await retireOwner(f);
    const out = await owner.ask(retireReq(f, t, { race: "concurrent" }), 150000);
    assert.ok(out.raced.swaps > 0, "the racer landed at least one swap: " + JSON.stringify(out.raced));
    assert.ok(["removed", "residue"].includes(out.result.kind), JSON.stringify(out));
    assert.equal(out.spawns, 1, "exactly one runner pass");
    assert.equal(out.pending, 0); assert.deepEqual(out.unsettled, []);
    assert.equal(fs.readFileSync(path.join(t.outside, "keep.txt"), "utf8"), "outside-survives");
    assert.deepEqual(fs.readdirSync(t.outside).sort(), ["keep.txt"], "nothing outside the clone was emptied or written");
    assert.deepEqual(fs.readdirSync(t.elsewhere), [], "nothing was written elsewhere");
    if (out.result.kind === "removed") assert.equal(fs.existsSync(t.parent), false);
    else assert.equal(fs.existsSync(t.parent), true, "residue keeps the parent");
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => t && dropTree(t)); }
});

// Issue #4486: a group-writable (setgid 2775) managed bare under the split. Everything is real: the worker child's
// runner-group probe, the production bare and runner clone writers, the production proof and /usr/bin/getfacl (no
// observer, getfacl or statfs seam). The root test process only builds directories and mutates the fixture between the
// child's setup and prove ops; the runner identities are launched through the production wrapper argv.
const SETPRIV_BIN = "/bin/setpriv";
const CODEX_SESSION = 10004;
const clean = { PATH: "/usr/bin:/bin" };
function launch(w: { command: string; args: string[] }) {
  return spawnSync(w.command, w.args, { env: clean, encoding: "utf8", timeout: 20000 });
}
function asWorker(argv: string[]) {
  return launch({ command: SETPRIV_BIN, args: ["--reuid", String(WORKER_ID), "--regid", String(WORKER_ID), "--clear-groups", "--", ...argv] });
}
function asRunner(argv: string[]) { return launch(runnerCommand(argv[0]!, argv.slice(1), true)); }
function asCommandRoot(argv: string[]) {
  const saved = process.env.UZI_UID_SPLIT;
  process.env.UZI_UID_SPLIT = "1";
  try { return launch(commandRootCommand(argv[0]!, argv.slice(1))); }
  finally { if (saved === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = saved; }
}
const ACL_ENTRY = "u:10002:rwx";
const listAcl = (target: string) => asWorker(["/usr/bin/getfacl", "--absolute-names", "--physical", "--skip-base", "--numeric", "--", target]);

test("pod epoch real managed bare: a setgid 2775 bare is admitted under the real runner-group probe and getfacl, runner uids cannot write or set ACLs, and an ACL or a wrong group refuses the proof", { timeout: 200000 }, async () => {
  const f = fixture();
  const bases: string[] = [];
  let primary: unknown;
  try {
    const owner = await retireOwner(f);
    // The production layout under the split: the data dir and a setgid 2775 worker:worker repos root, a worker:runner 3775 runner root.
    const make = async (beforeBare?: (repos: string) => void) => {
      const base = fs.mkdtempSync("/data/managed-bare-"); bases.push(base);
      fs.chmodSync(base, 0o755); fs.chownSync(base, WORKER_ID, WORKER_ID);
      const repos = path.join(base, "repos"); fs.mkdirSync(repos); own(repos, WORKER_ID, WORKER_ID, 0o2775);
      const runnerRoot = path.join(base, "runner"); fs.mkdirSync(runnerRoot); own(runnerRoot, WORKER_ID, RUNNER_ID, 0o3775);
      beforeBare?.(repos);
      const setup = await owner.ask({ op: "managed-bare-setup", dataDir: base }, 150000);
      const detail = () => JSON.stringify(setup.trust) + "\nrunner status:\n" + setup.statuses.runner + "command status:\n" + setup.statuses.command;
      assert.equal(setup.trust.state, "trusted", "the real probe in the image under the split must be trusted: " + detail());
      assert.equal(setup.isIssued, true, "the trust is a value the probe issued");
      assert.equal(setup.trust.gid, WORKER_ID);
      assert.equal(setup.bareMode.toString(8), "2775", "a bare made under a setgid repos root with umask 002");
      assert.equal(setup.bareGid, WORKER_ID); assert.equal(setup.bareUid, WORKER_ID);
      return { base, repos, bare: setup.bare as string, dataDir: base };
    };
    const prove = (m: { dataDir: string }) => owner.ask({ op: "managed-bare-prove", dataDir: m.dataDir }, 150000);
    const refused = async (label: string, m: { dataDir: string }) => {
      const verdict = await prove(m);
      assert.equal(verdict.proven, false, label + ": " + JSON.stringify(verdict));
      assert.equal(verdict.reason, "completeness_unknown", label + ": " + JSON.stringify(verdict));
      assert.equal(verdict.cloneExists, true, label + ": the proof deletes nothing");
    };

    // a. Admission, and (b) the runner identities cannot write into the objects directory.
    const admitted = await make();
    const verdict = await prove(admitted);
    assert.deepEqual({ proven: verdict.proven, reason: verdict.reason }, { proven: true, reason: null }, JSON.stringify(verdict));
    assert.equal(listAcl(admitted.bare).stdout, "", "the admitted bare carries no extended ACL");
    for (const [label, run] of [["runner", asRunner], ["runner-cmd", asCommandRoot]] as const) {
      for (const script of [`mkdir ${admitted.bare}/objects/zz`, `: > ${admitted.bare}/objects/zz-file`]) {
        const r = run(["/bin/sh", "-c", `id -u; ${script}`]);
        assert.equal(r.stdout.trim(), String(label === "runner" ? RUNNER_ID : CMD_ID), `${label}: the identity the operation ran as`);
        assert.notEqual(r.status, 0, `${label}: ${script} must fail: ${r.stdout}${r.stderr}`);
        assert.match(r.stderr, /Permission denied/, `${label}: ${script}: ${r.stderr}`);
      }
      assert.deepEqual(fs.readdirSync(path.join(admitted.bare, "objects")).filter(n => n.startsWith("zz")), [], label + " created nothing");
    }

    // c. An ACL keeps the allowance off: access ACL on a store path, a default ACL inherited from the repos root, and a
    // non-inherited ACL on the repos root itself. Each starts from a fresh fixture, and the real getfacl is shown to see it.
    const access = await make();
    const set = asWorker(["/usr/bin/setfacl", "-m", ACL_ENTRY, path.join(access.bare, "objects")]);
    assert.equal(set.status, 0, "the owner sets an ACL: " + set.stderr);
    assert.match(listAcl(path.join(access.bare, "objects")).stdout, /user:10002:rwx/);
    await refused("access ACL on objects", access);

    const inherited = await make(repos => {
      const r = asWorker(["/usr/bin/setfacl", "-d", "-m", ACL_ENTRY, repos]);
      assert.equal(r.status, 0, "the owner sets a default ACL: " + r.stderr);
    });
    assert.match(listAcl(path.join(inherited.bare, "objects")).stdout, /user:10002:rwx/, "the bare inherited the ACL");
    await refused("ACL inherited from the repos root", inherited);

    const parent = await make();
    const onRoot = asWorker(["/usr/bin/setfacl", "-m", ACL_ENTRY, parent.repos]);
    assert.equal(onRoot.status, 0, "the owner sets an ACL on the repos root: " + onRoot.stderr);
    assert.match(listAcl(parent.repos).stdout, /user:10002:rwx/);
    assert.equal(listAcl(parent.bare).stdout, "", "the bare itself carries none");
    await refused("ACL on the repos root", parent);

    // d. The runner identities cannot set an ACL; the owner can, which also proves the tooling and the filesystem work.
    const target = await make();
    for (const [label, run] of [["runner", asRunner], ["runner-cmd", asCommandRoot]] as const) {
      const r = run(["/bin/sh", "-c", `id -u; /usr/bin/setfacl -m ${ACL_ENTRY} ${target.bare}`]);
      assert.equal(r.stdout.trim(), String(label === "runner" ? RUNNER_ID : CMD_ID), `${label}: the identity the operation ran as`);
      assert.notEqual(r.status, 0, `${label} setfacl must fail: ${r.stdout}${r.stderr}`);
      assert.match(r.stderr, /Operation not permitted/, `${label}: ${r.stderr}`);
    }
    assert.equal(listAcl(target.bare).stdout, "", "no runner ACL landed");
    const control = asWorker(["/usr/bin/setfacl", "-m", ACL_ENTRY, target.bare]);
    assert.equal(control.status, 0, "control: the owner sets the same ACL (setfacl and ACL support are required): " + control.stderr);
    assert.match(listAcl(target.bare).stdout, /user:10002:rwx/, "control: the ACL is observable");

    // e. A bare of the wrong group at 2775 is refused: the runner group and the codex-session group.
    for (const gid of [RUNNER_ID, CMD_ID, CODEX_SESSION]) {
      const wrong = await make();
      own(wrong.bare, WORKER_ID, gid, 0o2775);
      assert.equal(fs.statSync(wrong.bare).mode & 0o7777, 0o2775);
      await refused(`bare of group ${gid}`, wrong);
    }
  } catch (e) { primary = e; throw e; } finally {
    await closeAfter(f, primary, () => { for (const b of bases) dropTree({ base: b } as RetireTree); });
  }
});

test("pod epoch real retire scratch: another uid owning the root or the parent is refused with nothing launched", { timeout: 150000 }, async () => {
  const f = fixture();
  let primary: unknown;
  const trees: RetireTree[] = [];
  try {
    const owner = await retireOwner(f);
    const cases: Array<[string, string, Parameters<typeof retireTree>[1]]> = [
      ["root owned by another uid", "root_posture", { rootOwner: [RUNNER_ID, RUNNER_ID] }],
      ["root without the sticky bit", "root_posture", { rootMode: 0o775 }],
      ["parent owned by another uid but openable by the worker", "wrong_owner", { parentOwner: [RUNNER_ID, WORKER_ID], parentMode: 0o770 }],
    ];
    for (const [label, reason, options] of cases) {
      const t = retireTree(POPULATED, options); trees.push(t);
      const before = digest(t.base);
      const out = await owner.ask(retireReq(f, t), 90000);
      assert.deepEqual(out.result, { kind: "refused", reason }, label + " " + JSON.stringify(out));
      assert.equal(out.spawns, 0, label + ": nothing launched");
      assert.equal(out.pending, 0, label); assert.deepEqual(out.unsettled, [], label);
      assert.deepEqual(digest(t.base), before, label + ": nothing touched");
    }
  } catch (e) { primary = e; throw e; } finally { await closeAfter(f, primary, () => { for (const t of trees) dropTree(t); }); }
});
