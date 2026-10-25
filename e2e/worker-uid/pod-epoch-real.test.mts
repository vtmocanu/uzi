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
  async read() {
    const line = await deadline(this.lines.next(), "owned child response: " + this.stderr);
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
  async raw(request: object) {
    this.child.stdin.write(JSON.stringify(request) + "\n");
    return this.read();
  }
  async ask(request: object): Promise<any> {
    const response = await this.raw(request);
    assert.equal(response.error, undefined, response.error);
    return response.result;
  }
  async stop() {
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
  } finally { await f.close(); }
});

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
