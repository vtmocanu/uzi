// Issue #3153: the runner-shared volume layout under REAL uids. run.sh starts this file once, as
// root with exactly the entrypoint's startup capability set (CHOWN, DAC_OVERRIDE, SETPCAP, SETUID,
// SETGID; no FOWNER, no FSETID), no network, on tmpfs /data and /nix. Root is the identity the
// controller's runner-init container and the worker entrypoint's root window run as; every
// other identity is created the way production creates it: setpriv with the argv of
// agent/src/runner-uid.ts (setprivArgsForUid) for runner 10002 and runner-cmd 10003, the
// entrypoint's own final drop for the split worker, and a capless 10001 for the single-uid
// worker. Nothing here is a stub: the scripts under test are the controller's golden runner-init
// script and the working-tree entrypoint, run unmodified except for the path substitutions
// named in adaptedInit.
//
// Cases that need the volume mount boundary (dind sees only the runner-shared subPath, and `..`
// at a bind root does not reach the PVC) cannot be reproduced without CAP_SYS_ADMIN and are NOT
// claimed here; see the report that accompanies this file.
import assert from "node:assert/strict";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { after, test } from "node:test";
import { fileURLToPath } from "node:url";
import { COMMAND_UID, RUNNER_UID, WORKER_UID, setprivArgsForUid } from "../../agent/src/runner-uid.js";

const here = (relative: string): string => fileURLToPath(new URL(relative, import.meta.url));
const GOLDEN = here("../../controller/internal/kube/testdata/runner-shared-init.sh");
const ENTRYPOINT = here("../../agent/templates/entrypoint.sh");
const PROBE = here("./runner-shared-probe.mjs");
const TMPDIR_PROBE = here("./runner-shared-tmpdir-probe.mts");
const RETENTION_PROBE = here("./runner-shared-retention-probe.mts");
const TSX = "/app/node_modules/tsx/dist/loader.mjs";
const REAL_MARKER = "/etc/uzi-runner-layout";
const TOKEN = "/var/tmp/uzi-runner-shared-real-token";
const SETPRIV = "/bin/setpriv";
const ROOT_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
const DIND_UID = 1000;
const FOREIGN_UID = 101000; // a rootless daemon's subuid
const FSGROUP = 10001;

// ---------------------------------------------------------------------------------------------
// Identities, spawned the production way.
// ---------------------------------------------------------------------------------------------
const NOCAPS = ["--bounding-set", "-all", "--inh-caps", "-all", "--ambient-caps", "-all", "--"];
const identities = {
  /** The entrypoint's final drop, verbatim: worker with its image groups and only SETUID/SETGID. */
  workerSplit: ["--reuid", "worker", "--regid", "worker", "--init-groups",
    "--bounding-set", "-all,+setuid,+setgid", "--inh-caps", "-all,+setuid,+setgid",
    "--ambient-caps", "-all,+setuid,+setgid", "--"],
  /** The single-uid (runAsUser) worker: no groups beyond its own, no capabilities. */
  workerSingle: ["--reuid", String(WORKER_UID), "--regid", String(WORKER_UID), "--clear-groups", ...NOCAPS],
  /** runner-uid.ts: `setpriv --reuid runner --regid runner --init-groups ...`. */
  runner: setprivArgsForUid(RUNNER_UID),
  /** runner-uid.ts: the numeric runner-cmd form, also `--init-groups`. */
  command: setprivArgsForUid(COMMAND_UID),
  /** The dind daemon uid with the pod fsGroup as a supplementary group. */
  dind: ["--reuid", String(DIND_UID), "--regid", String(DIND_UID), "--groups", String(FSGROUP), ...NOCAPS],
  /** Any other uid that writes into the shared tree (daemon-written content). */
  foreign: (uid: number) => ["--reuid", String(uid), "--regid", String(uid), "--clear-groups", ...NOCAPS],
};

interface Ran { status: number | null; stdout: string; stderr: string }
function runAs(who: string[], command: string, args: string[], env: Record<string, string> = {}): Ran {
  const r = spawnSync(SETPRIV, [...who, command, ...args], {
    encoding: "utf8", timeout: 90_000, env: { PATH: "/usr/local/bin:/usr/bin:/bin", ...env },
  });
  return { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
}

interface ProbeOutcome {
  results: { op: string; path: string; ok: boolean; code?: string }[];
  uid: number; gid: number; groups: number[];
}
function probeAs(who: string[], ops: object[]): ProbeOutcome {
  const r = runAs(who, process.execPath, [PROBE, JSON.stringify(ops)]);
  assert.equal(r.status, 0, `probe failed: ${r.stderr}`);
  return JSON.parse(r.stdout.trim().split("\n").at(-1)!) as ProbeOutcome;
}
const denied = (outcome: ProbeOutcome, index: number): string | undefined =>
  outcome.results[index]!.ok ? undefined : outcome.results[index]!.code;

/** A long-lived writer started as `who`, appending to `file` from its own cwd; resolves once it has written. */
const writers: ChildProcess[] = [];
async function startWriter(who: string[], dir: string, file: string): Promise<{ file: string; pid: number }> {
  // stdin is the stop control: root has no CAP_KILL, so it cannot signal another uid's process, but
  // a writer exits by itself when its stdin closes (the probe's `hold`).
  const child = spawn(SETPRIV, [...who, process.execPath, PROBE, JSON.stringify([{ op: "hold", path: file }])],
    { cwd: dir, stdio: ["pipe", "pipe", "inherit"], env: { PATH: "/usr/local/bin:/usr/bin:/bin" } });
  writers.push(child);
  await new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.stdout!.once("data", () => resolve());
    child.once("exit", (code) => reject(new Error(`writer exited early (${code})`)));
  });
  return { file, pid: child.pid! };
}
/** Close every writer's stdin and wait for each to exit (bounded: 5 s each, then the leaf fails). */
async function stopWriters(): Promise<void> {
  for (const child of writers.splice(0)) {
    if (child.exitCode !== null || child.signalCode !== null) continue;
    const exited = new Promise<void>((resolve) => child.once("exit", () => resolve()));
    child.stdin?.end();
    await Promise.race([exited, new Promise<void>((_, reject) => setTimeout(() => reject(new Error(`writer ${child.pid} did not exit after its stdin closed`)), 5000))]);
  }
}
async function growsPast(file: string, bytes: number): Promise<number> {
  for (let i = 0; i < 250; i++) {
    const size = fs.existsSync(file) ? fs.statSync(file).size : 0;
    if (size > bytes) return size;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  assert.fail(`${file} did not grow past ${bytes} bytes: the writer is not alive`);
}

// ---------------------------------------------------------------------------------------------
// Environment, fixtures and snapshots.
// ---------------------------------------------------------------------------------------------
function procField(name: string): string {
  const status = fs.readFileSync("/proc/self/status", "utf8");
  return status.match(new RegExp(`^${name}:\\s*(.*)$`, "m"))?.[1]?.trim() ?? "";
}
function requireRootWindow(): void {
  assert.equal(process.platform, "linux");
  assert.equal(process.getuid?.(), 0, "prerequisite: the entrypoint root window runs as uid 0");
  assert.equal(BigInt("0x" + procField("CapEff")), 0x1c3n, "prerequisite: exactly CHOWN, DAC_OVERRIDE, SETGID, SETUID, SETPCAP");
  assert.equal(procField("NoNewPrivs"), "1", "prerequisite: no-new-privileges");
  const mounts = fs.readFileSync("/proc/self/mountinfo", "utf8").split("\n");
  assert.ok(mounts.some((line) => /^\S+ \S+ \S+ \S+ \/data \S+.* - tmpfs /.test(line)), "prerequisite: /data is tmpfs");
  assert.equal(fs.readFileSync(REAL_MARKER, "utf8").trim(), "runner-shared-v1", "prerequisite: the image carries the layout marker");
}

/** Remove everything under /data. Root has no FOWNER, so entries other uids own are reclaimed
 *  (CAP_CHOWN) before removal; /data is this container's own tmpfs, verified by requireRootWindow. */
function resetData(): void {
  requireRootWindow();
  for (const name of fs.readdirSync("/data")) {
    const entry = path.join("/data", name);
    spawnSync("/bin/chown", ["-R", "-h", "0:0", entry]);
    fs.rmSync(entry, { recursive: true, force: true });
  }
}
function newBase(label: string): string {
  requireRootWindow();
  const base = fs.mkdtempSync(`/data/rsreal-${label}-`);
  fs.chmodSync(base, 0o755);
  return base;
}

/** The controller's golden runner-init script with the two paths it hard-codes substituted. */
function adaptedInit(base: string, seed: string, marker: string): string {
  const golden = fs.readFileSync(GOLDEN, "utf8");
  const adapted = golden.replaceAll("/data-seed", seed).replace("M=" + REAL_MARKER, `M=${marker}`);
  assert.ok(!adapted.includes("/data-seed"), "every seed path was substituted");
  assert.ok(adapted.includes(`M=${marker}\n`), "the marker path was substituted");
  assert.ok(adapted.includes(`R=${seed}/runner-shared\n`) && adapted.includes(`T=${seed}/runner-shared/.tmp\n`));
  const file = path.join(base, `init-${fs.readdirSync(base).length}.sh`);
  fs.writeFileSync(file, adapted, { mode: 0o755 });
  return file;
}
function runInit(script: string, who?: string[]): Ran {
  return who ? runAs(who, "/bin/sh", [script]) : (() => {
    const r = spawnSync("/bin/sh", [script], { encoding: "utf8", timeout: 60_000, env: { PATH: ROOT_PATH } });
    return { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
  })();
}

interface Snap { [rel: string]: string }
/** lstat-only description of a tree: type, owner, mode, inode and (regular files) size. Entries
 *  `skip` names (a live writer's log, a transpiler cache) are left out entirely. */
function snapshot(root: string, skip: (rel: string) => boolean = () => false): Snap {
  const out: Snap = {};
  const visit = (abs: string, rel: string): void => {
    if (rel !== "" && skip(rel)) return;
    const st = fs.lstatSync(abs);
    const type = st.isSymbolicLink() ? "link" : st.isDirectory() ? "dir" : "file";
    let line = `${type} ${st.uid}:${st.gid} ${(st.mode & 0o7777).toString(8)} ino=${st.ino}`;
    if (type === "file") line += ` size=${st.size}`;
    if (type === "link") line += ` -> ${fs.readlinkSync(abs)}`;
    out[rel === "" ? "." : rel] = line;
    if (type === "dir") for (const name of fs.readdirSync(abs).sort()) visit(path.join(abs, name), path.posix.join(rel, name));
  };
  visit(root, "");
  return out;
}
/** Report the first difference only (bounded output). */
function assertSame(actual: Snap, expected: Snap, label: string): void {
  const keys = [...new Set([...Object.keys(expected), ...Object.keys(actual)])].sort();
  for (const key of keys) {
    if (actual[key] !== expected[key]) assert.fail(`${label}: first difference at ${key}: expected ${expected[key] ?? "(absent)"}, got ${actual[key] ?? "(absent)"}`);
  }
}
const isLive = (rel: string): boolean => rel.endsWith("live.log");
/** Root has no CAP_FOWNER, so entries other uids own are reclaimed (CAP_CHOWN) before removal. */
function reclaimAndRemove(p: string): void {
  spawnSync("/bin/chown", ["-R", "-h", "0:0", p]);
  fs.rmSync(p, { recursive: true, force: true });
}
const mode = (p: string): number => fs.lstatSync(p).mode & 0o7777;
const owner = (p: string): string => { const st = fs.lstatSync(p); return `${st.uid}:${st.gid}`; };

/** Build a steady-state Docker-lane volume: runner-init's output renamed into place as the
 *  subPath, then one entrypoint boot so every one-time sentinel exists. */
function buildVolume(tmpdir: string): void {
  resetData();
  fs.mkdirSync("/data/.pvc", { mode: 0o755 });
  const init = adaptedInit("/data", "/data/.pvc", REAL_MARKER);
  const r = runInit(init);
  assert.equal(r.status, 0, r.stderr);
  fs.renameSync("/data/.pvc/runner-shared", "/data/runner");
  fs.rmdirSync("/data/.pvc");
  fs.rmSync(init);
  const first = boot(tmpdir, "true");
  assert.equal(first.status, 0, first.stderr);
}

function ensureToken(): void {
  if (!fs.existsSync(TOKEN)) fs.writeFileSync(TOKEN, "token\n", { mode: 0o444 });
}
/** One real boot: the working-tree entrypoint as root, the real drop, tini, then `script` as the worker. */
function boot(tmpdir: string, script: string): Ran {
  ensureToken();
  const r = spawnSync("/bin/sh", [ENTRYPOINT, "/bin/sh", "-c", script], {
    encoding: "utf8", timeout: 90_000,
    env: { PATH: ROOT_PATH, TMPDIR: tmpdir, UZI_WORKER_TOKEN_FILE: TOKEN },
  });
  return { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
}
/** The worker command of a live boot: the probe's `hold` on $TMPDIR/live.log (ends when stdin closes). */
const liveWorkload = ["/bin/sh", "-c", `exec ${process.execPath} ${PROBE} "[{\\"op\\":\\"hold\\",\\"path\\":\\"$TMPDIR/live.log\\"}]"`];
/** A boot whose worker command stays alive, appending to $TMPDIR/live.log, until its stdin closes. */
async function bootLive(tmpdir: string, logDir: string): Promise<ChildProcess> {
  ensureToken();
  const child = spawn("/bin/sh", [ENTRYPOINT, ...liveWorkload], {
    stdio: ["pipe", "ignore", "inherit"], env: { PATH: ROOT_PATH, TMPDIR: tmpdir, UZI_WORKER_TOKEN_FILE: TOKEN },
  });
  writers.push(child);
  await growsPast(path.join(logDir, "live.log"), 0);
  return child;
}

after(stopWriters);

// ---------------------------------------------------------------------------------------------
// Leaves.
// ---------------------------------------------------------------------------------------------
test("runner-init refuses a missing, wrong or symlinked layout marker before any write", { timeout: 100_000 }, () => {
  resetData();
  const base = newBase("marker");
  const outside = path.join(base, "outside");
  fs.mkdirSync(outside);
  fs.writeFileSync(path.join(outside, "keep"), "k");
  const good = path.join(base, "marker-good");
  fs.writeFileSync(good, "runner-shared-v1\n");
  const bad: [string, () => string][] = [
    ["missing", () => path.join(base, "marker-missing")],
    ["wrong content", () => { const p = path.join(base, "marker-wrong"); fs.writeFileSync(p, "runner-shared-v0\n"); return p; }],
    ["empty", () => { const p = path.join(base, "marker-empty"); fs.writeFileSync(p, ""); return p; }],
    ["a symlink to a correct marker", () => { const p = path.join(base, "marker-link"); fs.symlinkSync(good, p); return p; }],
    ["a directory", () => { const p = path.join(base, "marker-dir"); fs.mkdirSync(p); return p; }],
  ];
  for (const [name, make] of bad) {
    const seed = path.join(base, `seed-${name.replace(/\W+/g, "-")}`);
    fs.mkdirSync(seed, { mode: 0o755 });
    fs.mkdirSync(path.join(seed, "runner"), { mode: 0o755 });
    fs.writeFileSync(path.join(seed, "runner", "old"), "old");
    const before = snapshot(seed);
    const r = runInit(adaptedInit(base, seed, make()));
    assert.equal(r.status, 1, `${name}: ${r.stderr}`);
    assert.match(r.stderr, /uzi-runner-init: worker image lacks runner-shared layout support \(want runner-shared-v1\); refusing/, name);
    assertSame(snapshot(seed), before, `marker ${name}: the seed volume`);
    assert.deepEqual(fs.readdirSync(outside), ["keep"]);
  }
  // Control: the image's own marker passes the gate, so the refusals above are the marker's doing.
  const seed = path.join(base, "seed-control");
  fs.mkdirSync(seed, { mode: 0o755 });
  const ok = runInit(adaptedInit(base, seed, REAL_MARKER));
  assert.equal(ok.status, 0, ok.stderr);
});

test("runner-init as root builds worker:runner 3775 with a worker:worker 0755 .tmp and leaves old and sibling data untouched", { timeout: 100_000 }, () => {
  resetData();
  const base = newBase("fresh");
  const seed = path.join(base, "seed");
  fs.mkdirSync(seed, { mode: 0o755 });
  // Data a pre-#3153 volume already holds: the old root-owned runner directory and a sibling.
  const old = path.join(seed, "runner");
  fs.mkdirSync(path.join(old, "issue-1"), { recursive: true });
  fs.writeFileSync(path.join(old, "issue-1", "WORK"), "old clone");
  fs.chmodSync(old, 0o755);
  const sibling = path.join(seed, "data");
  fs.mkdirSync(path.join(sibling, "repos"), { recursive: true });
  fs.writeFileSync(path.join(sibling, "repos", "bare"), "bare cache");
  fs.chownSync(sibling, WORKER_UID, WORKER_UID);
  fs.chownSync(path.join(sibling, "repos"), WORKER_UID, WORKER_UID);
  const oldBefore = snapshot(old);
  const siblingBefore = snapshot(sibling);
  const seedBefore = snapshot(seed);
  const script = adaptedInit(base, seed, REAL_MARKER);

  for (const round of ["fresh", "second run"]) {
    const r = runInit(script);
    assert.equal(r.status, 0, `${round}: ${r.stderr}`);
    assert.match(r.stdout, /uzi-runner-init: runner-shared ready/);
    const shared = path.join(seed, "runner-shared");
    assert.equal(owner(shared), `${WORKER_UID}:${RUNNER_UID}`, `${round}: runner-shared is worker:runner`);
    assert.equal(mode(shared).toString(8), "3775", `${round}: runner-shared mode`);
    assert.equal(owner(path.join(shared, ".tmp")), `${WORKER_UID}:${WORKER_UID}`, `${round}: .tmp is worker:worker`);
    assert.equal(mode(path.join(shared, ".tmp")).toString(8), "755", `${round}: .tmp mode`);
    assert.deepEqual(fs.readdirSync(shared), [".tmp"]);
    assertSame(snapshot(old), oldBefore, `${round}: the old root-owned runner directory`);
    assertSame(snapshot(sibling), siblingBefore, `${round}: sibling data`);
    const seedNow = snapshot(seed);
    for (const key of Object.keys(seedBefore)) assert.equal(seedNow[key], seedBefore[key], `${round}: seed entry ${key}`);
  }
});

test("runner-init refuses a hostile runner-shared or .tmp symlink or file without following or re-owning anything", { timeout: 100_000 }, () => {
  resetData();
  const base = newBase("hostile");
  const victim = path.join(base, "victim");
  fs.mkdirSync(victim, { mode: 0o700 });
  fs.writeFileSync(path.join(victim, "secret"), "s");
  const victimBefore = snapshot(victim);
  const cases: [string, (seed: string) => void, RegExp][] = [
    ["runner-shared symlink to the seed root", (s) => fs.symlinkSync(".", path.join(s, "runner-shared")), /is a symlink; refusing to follow it/],
    ["runner-shared symlink to a sibling", (s) => fs.symlinkSync(victim, path.join(s, "runner-shared")), /is a symlink; refusing to follow it/],
    ["runner-shared is a file", (s) => fs.writeFileSync(path.join(s, "runner-shared"), "f"), /exists and is not a directory/],
    ["real runner-shared with a .tmp symlink", (s) => { fs.mkdirSync(path.join(s, "runner-shared"), { mode: 0o755 }); fs.symlinkSync(victim, path.join(s, "runner-shared", ".tmp")); }, /\.tmp is a symlink; refusing to follow it/],
    ["real runner-shared with a .tmp symlink to its parent", (s) => { fs.mkdirSync(path.join(s, "runner-shared"), { mode: 0o755 }); fs.symlinkSync("..", path.join(s, "runner-shared", ".tmp")); }, /\.tmp is a symlink; refusing to follow it/],
    ["real runner-shared with a .tmp file", (s) => { fs.mkdirSync(path.join(s, "runner-shared"), { mode: 0o755 }); fs.writeFileSync(path.join(s, "runner-shared", ".tmp"), "f"); }, /\.tmp exists and is not a directory/],
  ];
  for (const [name, plant, message] of cases) {
    const seed = path.join(base, `seed-${name.replace(/\W+/g, "-")}`);
    fs.mkdirSync(seed, { mode: 0o755 });
    plant(seed);
    const before = snapshot(seed);
    const r = runInit(adaptedInit(base, seed, REAL_MARKER));
    assert.equal(r.status, 1, `${name}: ${r.stderr}`);
    assert.match(r.stderr, message, name);
    assertSame(snapshot(seed), before, `${name}: nothing under the seed was created, followed or re-owned`);
    assertSame(snapshot(victim), victimBefore, `${name}: the symlink target`);
  }
});

test("runner-init as the worker uid requires ownership by the current uid and fails closed on anything else", { timeout: 100_000 }, () => {
  resetData();
  const base = newBase("single");
  const fresh = path.join(base, "fresh");
  fs.mkdirSync(fresh, { mode: 0o755 });
  fs.chownSync(fresh, WORKER_UID, WORKER_UID);
  const ok = runInit(adaptedInit(base, fresh, REAL_MARKER), identities.workerSingle);
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(ok.stdout, /runner-shared ready/);
  const shared = path.join(fresh, "runner-shared");
  assert.equal(owner(shared), `${WORKER_UID}:${WORKER_UID}`);
  assert.ok((mode(shared) & 0o1000) !== 0, "the sticky bit is set by the owner");
  assert.equal(owner(path.join(shared, ".tmp")), `${WORKER_UID}:${WORKER_UID}`);
  const again = runInit(adaptedInit(base, fresh, REAL_MARKER), identities.workerSingle);
  assert.equal(again.status, 0, again.stderr);

  // A volume a root-run init (or an older release) already owns: the capless uid cannot take it over.
  const rootOwned = path.join(base, "root-owned");
  fs.mkdirSync(rootOwned, { mode: 0o755 });
  fs.chownSync(rootOwned, WORKER_UID, WORKER_UID);
  fs.mkdirSync(path.join(rootOwned, "runner-shared"), { mode: 0o755 }); // created by root
  fs.mkdirSync(path.join(rootOwned, "runner-shared", ".tmp"), { mode: 0o755 });
  const before = snapshot(rootOwned);
  const refused = runInit(adaptedInit(base, rootOwned, REAL_MARKER), identities.workerSingle);
  assert.equal(refused.status, 1, refused.stderr);
  assert.match(refused.stderr, /must be owned by uid 10001 with .* sticky, and could not be made so/);
  assertSame(snapshot(rootOwned), before, "a root-owned runner-shared is left exactly as found");

  // A symlink is refused in this mode as well, and nothing it points at is touched.
  const linked = path.join(base, "linked");
  fs.mkdirSync(linked, { mode: 0o755 });
  fs.chownSync(linked, WORKER_UID, WORKER_UID);
  const victim = path.join(base, "single-victim");
  fs.mkdirSync(victim, { mode: 0o700 });
  fs.symlinkSync(victim, path.join(linked, "runner-shared"));
  const victimBefore = snapshot(victim);
  const link = runInit(adaptedInit(base, linked, REAL_MARKER), identities.workerSingle);
  assert.equal(link.status, 1, link.stderr);
  assert.match(link.stderr, /is a symlink; refusing to follow it/);
  assertSame(snapshot(victim), victimBefore, "symlink target");
});

test("the runner, runner-cmd and dind uids cannot replace .tmp or runner-shared entries, and their groups exclude 10001", { timeout: 100_000 }, () => {
  resetData();
  const base = newBase("ident");
  // /data is worker:10001 2775 in production (the pod fsGroup); runner-shared lives beneath it.
  const data = path.join(base, "data");
  fs.mkdirSync(data, { mode: 0o755 });
  fs.chmodSync(data, 0o2775); // before the hand-over: root has no CAP_FOWNER once the worker owns it
  fs.chownSync(data, WORKER_UID, FSGROUP);
  const init = runInit(adaptedInit(base, data, REAL_MARKER));
  assert.equal(init.status, 0, init.stderr);
  const shared = path.join(data, "runner-shared");
  const tmp = path.join(shared, ".tmp");
  // The worker creates run directories the way main.ts has it run: umask 002.
  const rundir = path.join(shared, "issue-1.attempt-a");
  const made = probeAs(identities.workerSplit, [
    { op: "mkdir", path: rundir, mode: 0o775 },
    { op: "create", path: path.join(tmp, "worker-file") },
    { op: "create", path: path.join(rundir, "work") },
  ]);
  assert.deepEqual(made.results.map((r) => r.ok), [true, true, true]);
  assert.equal(owner(rundir), `${WORKER_UID}:${RUNNER_UID}`, "a worker-made run directory inherits group runner");

  const before = { shared: snapshot(shared), data: snapshot(data) };
  const cases: [string, string[], number[], number[]][] = [
    // name, identity, groups that must be present, groups that must be absent
    ["runner", identities.runner, [RUNNER_UID], [FSGROUP]],
    ["runner-cmd", identities.command, [COMMAND_UID, RUNNER_UID], [FSGROUP]],
    ["dind", identities.dind, [DIND_UID, FSGROUP], []],
  ];
  for (const [name, who, present, absent] of cases) {
    const attempts: [string, object][] = [
      ["rename .tmp away", { op: "rename", from: tmp, to: tmp + ".moved" }],
      ["rmdir .tmp", { op: "rmdir", path: tmp }],
      ["unlink .tmp", { op: "unlink", path: tmp }],
      ["plant a symlink at .tmp", { op: "symlink", target: "/", path: tmp }],
      ["rename a worker-owned run directory away", { op: "rename", from: rundir, to: rundir + ".moved" }],
      ["delete the worker's file inside .tmp", { op: "unlink", path: path.join(tmp, "worker-file") }],
      ["create inside .tmp", { op: "create", path: path.join(tmp, "planted") }],
    ];
    if (name !== "dind") {
      // Replacing runner-shared itself in its parent: neither identity has the pod fsGroup, so
      // the group-writable parent gives it nothing. (The dind uid does hold the fsGroup, but it
      // only ever sees the subPath mount, which a process in a container cannot replace; that
      // mount-namespace property is not reproducible here and is not claimed.)
      attempts.push(["rename runner-shared away in /data", { op: "rename", from: shared, to: shared + ".moved" }]);
      attempts.push(["plant a symlink beside runner-shared in /data", { op: "symlink", target: "/", path: path.join(data, "evil") }]);
    }
    const outcome = probeAs(who, attempts.map(([, op]) => op));
    assert.equal(outcome.uid, name === "runner" ? RUNNER_UID : name === "runner-cmd" ? COMMAND_UID : DIND_UID, `${name}: the child really runs as that uid`);
    const effective = [outcome.gid, ...outcome.groups]; // the primary gid plus the supplementary groups
    for (const group of present) assert.ok(effective.includes(group), `${name}: groups ${effective} lack ${group}`);
    for (const group of absent) assert.ok(!effective.includes(group), `${name}: groups ${effective} must not include ${group}`);
    for (const [index, [what]] of attempts.entries()) {
      assert.notEqual(denied(outcome, index), undefined, `${name}: "${what}" must be denied`);
    }
  }
  assertSame(snapshot(shared), before.shared, "runner-shared after every replacement attempt");
  assertSame(snapshot(data), before.data, "the data root after every replacement attempt");

  // The pinned side effect (ADR-3153): a process holding only the fsGroup cannot create entries at
  // the top of runner-shared, nor inside a worker-made run directory: both are group `runner`,
  // other r-x. The runner and runner-cmd uids, which are in group runner, can.
  const dind = probeAs(identities.dind, [
    { op: "create", path: path.join(shared, "dind-top") },
    { op: "create", path: path.join(rundir, "dind-file") },
    { op: "mkdir", path: path.join(rundir, "dind-dir") },
  ]);
  assert.deepEqual(dind.results.map((r) => r.code), ["EACCES", "EACCES", "EACCES"], "dind (uid 1000 + fsGroup 10001) cannot create where runner-group dirs are 2775");
  for (const [name, who] of [["runner", identities.runner], ["runner-cmd", identities.command]] as const) {
    const ok = probeAs(who, [{ op: "create", path: path.join(rundir, `${name}-file`) }, { op: "mkdir", path: path.join(shared, `${name}-dir`) }]);
    assert.deepEqual(ok.results.map((r) => r.ok), [true, true], `${name} can create in a worker-made run directory and at the runner-shared top`);
  }
});

test("a fresh .tmp is created by the entrypoint and by ensureRunnerTmpdir in both uid modes, and a planted symlink or file is refused", { timeout: 110_000 }, () => {
  resetData();
  // Split mode, entrypoint: runner-init's output without its .tmp (an empty runner-shared).
  fs.mkdirSync("/data/.pvc", { mode: 0o755 });
  const init = adaptedInit("/data", "/data/.pvc", REAL_MARKER);
  assert.equal(runInit(init).status, 0);
  spawnSync("/bin/chown", ["-h", "0:0", "/data/.pvc/runner-shared/.tmp"]); // root has no CAP_FOWNER: reclaim, then remove
  fs.rmdirSync("/data/.pvc/runner-shared/.tmp");
  fs.renameSync("/data/.pvc/runner-shared", "/data/runner");
  fs.rmdirSync("/data/.pvc");
  fs.rmSync(init);
  assert.deepEqual(fs.readdirSync("/data/runner"), []);
  const created = boot("/data/runner/.tmp", "true");
  assert.equal(created.status, 0, created.stderr);
  assert.equal(owner("/data/runner/.tmp"), `${WORKER_UID}:${WORKER_UID}`);
  assert.equal(mode("/data/runner/.tmp").toString(8), "755");
  assert.equal(owner("/data/runner/.tmp/uzi-worker"), `${WORKER_UID}:${WORKER_UID}`);
  assert.equal(owner("/data/runner/.tmp/uzi-runner"), `${RUNNER_UID}:${RUNNER_UID}`);
  assert.equal(mode("/data/runner/.tmp/uzi-worker").toString(8), "700");

  // A symlink or a file planted at .tmp is refused with nothing followed or re-owned.
  const victim = "/data/victim";
  fs.mkdirSync(victim, { mode: 0o700 });
  fs.writeFileSync(path.join(victim, "secret"), "s");
  const victimBefore = snapshot(victim);
  for (const [kind, plant, message] of [
    ["symlink", () => fs.symlinkSync(victim, "/data/runner/.tmp"), /\.tmp is a symlink/],
    ["file", () => fs.writeFileSync("/data/runner/.tmp", "f"), /\.tmp exists and is not a directory/],
  ] as const) {
    reclaimAndRemove("/data/runner/.tmp");
    plant();
    const before = snapshot("/data/runner");
    const refused = boot("/data/runner/.tmp", "true");
    assert.notEqual(refused.status, 0, `${kind}: ${refused.stderr}`);
    assert.match(refused.stderr, message, kind);
    assertSame(snapshot("/data/runner"), before, `${kind}: the runner root`);
    assertSame(snapshot(victim), victimBefore, `${kind}: the target`);
  }

  // ensureRunnerTmpdir, the worker's own boot check, in both postures: the capless single-uid
  // worker, and the split worker (image groups, SETUID/SETGID ambient).
  for (const [mode_, who] of [["single-uid", identities.workerSingle], ["split", identities.workerSplit]] as const) {
    resetData();
    const base = newBase(`tmpdir-${mode_}`);
    const dataDir = path.join(base, "dd");
    const root = path.join(dataDir, "runner");
    fs.mkdirSync(root, { recursive: true, mode: 0o755 });
    fs.chownSync(dataDir, WORKER_UID, WORKER_UID);
    fs.chmodSync(root, 0o3775);
    fs.chownSync(root, WORKER_UID, mode_ === "split" ? RUNNER_UID : WORKER_UID);
    const cache = path.join(base, "tsx-cache");
    fs.mkdirSync(cache, { mode: 0o700 });
    fs.chownSync(cache, WORKER_UID, WORKER_UID);
    const run = (): { ok: boolean; message?: string; created?: boolean; uid: number } => {
      const r = runAs(who, process.execPath, ["--import", TSX, TMPDIR_PROBE, JSON.stringify({ dataDir, tmpdir: path.join(root, ".tmp") })],
        { TMPDIR: cache, HOME: cache, ...(mode_ === "split" ? { UZI_UID_SPLIT: "1" } : {}) });
      assert.equal(r.status, 0, r.stderr);
      return JSON.parse(r.stdout.trim().split("\n").at(-1)!);
    };
    const fresh = run();
    assert.equal(fresh.ok, true, `${mode_}: ${JSON.stringify(fresh)}`);
    assert.equal(fresh.created, true, `${mode_}: the empty runner-shared gets a fresh .tmp`);
    assert.equal(fresh.uid, WORKER_UID);
    // Created below the setgid runner-shared, so the group is runner under the split (its mode is 0700:
    // the group grants nothing) and the worker's own group single-uid.
    assert.equal(owner(path.join(root, ".tmp")), `${WORKER_UID}:${mode_ === "split" ? RUNNER_UID : WORKER_UID}`, mode_);
    assert.equal((mode(path.join(root, ".tmp")) & 0o777).toString(8), "700", mode_); // setgid is inherited from runner-shared
    assert.equal(run().created, false, `${mode_}: a second boot finds it`);
    for (const [kind, plant, message] of [
      ["symlink", () => fs.symlinkSync(base, path.join(root, ".tmp")), /is a symlink/],
      ["file", () => fs.writeFileSync(path.join(root, ".tmp"), "f"), /is not a directory/],
    ] as const) {
      reclaimAndRemove(path.join(root, ".tmp"));
      plant();
      const before = snapshot(base, (rel) => rel.startsWith("tsx-cache"));
      const refused = run();
      assert.equal(refused.ok, false, `${mode_} ${kind}`);
      assert.match(refused.message ?? "", message, `${mode_} ${kind}`);
      assert.match(refused.message ?? "", /runner tmpdir refused to start/);
      assertSame(snapshot(base, (rel) => rel.startsWith("tsx-cache")), before, `${mode_} ${kind}: nothing followed or created`);
    }
  }
});

test("per-uid scratch under a live writer: a mismatched owner is refused untouched, an expected owner is adopted with writer exclusion not established", { timeout: 110_000 }, async () => {
  buildVolume("/data/runner/.tmp");
  const leaves = { worker: "/data/runner/.tmp/uzi-worker", runner: "/data/runner/.tmp/uzi-runner" };
  try {
    // The expected state first: both leaves exist, 0700, owned by their users (the build's boot made them).
    assert.equal(owner(leaves.worker), `${WORKER_UID}:${WORKER_UID}`);
    assert.equal(owner(leaves.runner), `${RUNNER_UID}:${RUNNER_UID}`);

    for (const [which, foreignUid] of [["worker", DIND_UID], ["runner", FOREIGN_UID]] as const) {
      // A leaf owned by a foreign uid, written to by a live process of that uid (a lingering
      // old pod's or a daemon's writer).
      const leaf = leaves[which];
      const original = owner(leaf);
      spawnSync("/bin/chown", ["-h", `${foreignUid}:${foreignUid}`, leaf]);
      const writer = await startWriter(identities.foreign(foreignUid), leaf, path.join(leaf, "live.log"));
      const size = await growsPast(writer.file, 0);
      const before = snapshot(leaf, isLive);
      const refused = boot("/data/runner/.tmp", "true");
      assert.notEqual(refused.status, 0, `${which}: ${refused.stderr}`);
      assert.match(refused.stderr, new RegExp(`refusing to start: per-uid scratch ${leaf} cannot be safely adopted \\(owner is \\S+, expected ${which}\\)`));
      assert.match(refused.stderr, /contents preserved; see ADR-3153 operator procedure/);
      assert.equal(owner(leaf), `${foreignUid}:${foreignUid}`, `${which}: never re-owned`);
      assert.equal(mode(leaf).toString(8), "700", `${which}: never re-moded`);
      assertSame(snapshot(leaf, isLive), before, `${which}: contents`);
      assert.ok(await growsPast(writer.file, size) > size, `${which}: the writer was neither stopped nor starved`);
      await stopWriters();
      spawnSync("/bin/chown", ["-h", original, leaf]);
    }

    // Expected owners, 0700, with live writers of their own uids: adopted, preserved, announced.
    const workerWriter = await startWriter(identities.foreign(WORKER_UID), leaves.worker, path.join(leaves.worker, "live.log"));
    const runnerWriter = await startWriter(identities.foreign(RUNNER_UID), leaves.runner, path.join(leaves.runner, "live.log"));
    const sizes = [await growsPast(workerWriter.file, 0), await growsPast(runnerWriter.file, 0)];
    const before = { worker: snapshot(leaves.worker, isLive), runner: snapshot(leaves.runner, isLive) };
    const adopted = boot("/data/runner/.tmp", "true");
    assert.equal(adopted.status, 0, adopted.stderr);
    for (const leaf of Object.values(leaves)) {
      assert.match(adopted.stderr, new RegExp(`per-uid scratch adopted \\(contents preserved, writer exclusion not established\\): ${leaf} entries=\\d+ previous_mode=700`));
    }
    assertSame(snapshot(leaves.worker, isLive), before.worker, "adopted worker leaf");
    assertSame(snapshot(leaves.runner, isLive), before.runner, "adopted runner leaf");
    assert.ok(await growsPast(workerWriter.file, sizes[0]!) > sizes[0]!, "the worker-leaf writer keeps writing");
    assert.ok(await growsPast(runnerWriter.file, sizes[1]!) > sizes[1]!, "the runner-leaf writer keeps writing");
  } finally {
    await stopWriters();
  }
});

test("old-layout and new-layout entrypoints overlap on one data directory without re-owning, wiping or following into each other's tree", { timeout: 115_000 }, async () => {
  const NEW = "/data/runner/.tmp";
  const OLD = "/data/runner";
  buildVolume(NEW);
  try {
    // State both layouts leave behind: the old pod's scratch leaves at the runner root, a
    // runner-owned run directory, and the new layout's scratch under .tmp (made by the build boot).
    const oldBoot = boot(OLD, "true");
    assert.equal(oldBoot.status, 0, oldBoot.stderr);
    const run = "/data/runner/issue-9.attempt-a";
    fs.mkdirSync(run, { mode: 0o2775 });
    fs.writeFileSync(path.join(run, "WORK"), "a clone");
    spawnSync("/bin/chown", ["-R", "-h", `${RUNNER_UID}:${RUNNER_UID}`, run]);
    const newTree = (): Snap => snapshot("/data/runner/.tmp", isLive);
    const otherTrees = (): Snap => {
      const all = snapshot("/data/runner", (rel) => isLive(rel));
      return Object.fromEntries(Object.entries(all).filter(([key]) => key !== ".tmp" && !key.startsWith(".tmp/")));
    };
    const expectNew = newTree();
    const expectOther = otherTrees();
    assert.equal(owner("/data/runner/uzi-worker"), `${WORKER_UID}:${WORKER_UID}`, "the old layout's leaf exists");

    // Sequence: new, old, new, old; each boot must leave the other layout's tree exactly as it was.
    for (const [name, tmpdir] of [["new", NEW], ["old", OLD], ["new", NEW], ["old", OLD]] as const) {
      const r = boot(tmpdir, "true");
      assert.equal(r.status, 0, `${name}: ${r.stderr}`);
      assertSame(newTree(), expectNew, `after a ${name}-layout boot: .tmp`);
      assertSame(otherTrees(), expectOther, `after a ${name}-layout boot: the runner root and run directories`);
    }

    // Overlap: the new layout's workload is live (appending as the worker) while the old layout boots.
    await bootLive(NEW, "/data/runner/.tmp/uzi-worker");
    const logPath = "/data/runner/.tmp/uzi-worker/live.log";
    const size = await growsPast(logPath, 0);
    const overlapOld = boot(OLD, "true");
    assert.equal(overlapOld.status, 0, overlapOld.stderr);
    assertSame(newTree(), expectNew, "the live new-layout tree after an overlapping old-layout boot");
    assertSame(otherTrees(), expectOther, "the old-layout tree after the overlap");
    assert.ok(await growsPast(logPath, size) > size, "the live new-layout writer was not disturbed");
    await stopWriters();

    // And the reverse: an old-layout workload live, then a new-layout boot.
    await bootLive(OLD, "/data/runner/uzi-worker");
    const oldLog = "/data/runner/uzi-worker/live.log";
    const oldSize = await growsPast(oldLog, 0);
    const overlapNew = boot(NEW, "true");
    assert.equal(overlapNew.status, 0, overlapNew.stderr);
    assertSame(newTree(), expectNew, "the new-layout tree after an overlapping boot");
    assert.ok(await growsPast(oldLog, oldSize) > oldSize, "the live old-layout writer was not disturbed");
    await stopWriters();

    // A hostile link between the layouts is refused and followed nowhere: the old layout's
    // scratch name pointing at the new layout's scratch directory.
    reclaimAndRemove("/data/runner/uzi-worker");
    fs.symlinkSync("/data/runner/.tmp", "/data/runner/uzi-worker");
    const refused = boot(OLD, "true");
    assert.notEqual(refused.status, 0, refused.stderr);
    assert.match(refused.stderr, /uzi-worker cannot be safely adopted \(is a symlink\)/);
    assertSame(newTree(), expectNew, "the new-layout tree after the refusal");
  } finally {
    await stopWriters();
  }
});

test("foreign-owned run-directory content is retained, counted and logged by the production retention delete in split and single-uid modes", { timeout: 115_000 }, async () => {
  requireRootWindow();
  for (const mode_ of ["split", "single-uid"] as const) {
    resetData();
    const split = mode_ === "split";
    const base = newBase(`retain-${mode_}`);
    const dataDir = path.join(base, "dd");
    fs.mkdirSync(dataDir, { mode: 0o755 });
    fs.chownSync(dataDir, WORKER_UID, WORKER_UID);
    // runner-init's volume, renamed to the name GitCache derives its runner root from.
    const init = adaptedInit(base, dataDir, REAL_MARKER);
    assert.equal(runInit(init).status, 0);
    const root = path.join(dataDir, "runner");
    fs.renameSync(path.join(dataDir, "runner-shared"), root);
    const cache = path.join(base, "tsx-cache");
    fs.mkdirSync(cache, { mode: 0o700 });
    fs.chownSync(cache, WORKER_UID, WORKER_UID);

    const repoDir = path.join(root, "github.com+o+r");
    const target = path.join(repoDir, "issue-9.attempt-a");
    const worker = split ? identities.workerSplit : identities.workerSingle;
    // The agent's side of the tree: the owning uid (runner when split, the worker when not).
    const owningUid = split ? identities.runner : identities.workerSingle;
    assert.ok(probeAs(worker, [{ op: "mkdir", path: repoDir, mode: 0o775 }]).results[0]!.ok);
    const out = path.join(target, "out");
    assert.deepEqual(probeAs(owningUid, [
      { op: "mkdir", path: target, mode: 0o755 }, { op: "create", path: path.join(target, "own.txt") },
      { op: "mkdir", path: out, mode: 0o777 },
    ]).results.map((r) => r.ok), [true, true, true]);
    // Daemon-written content, foreign to the deleting uid: uid 0 (a rootful daemon), the dind uid
    // and a subuid. Each has a directory that holds files; one is non-writable and one is sticky.
    for (const uid of [DIND_UID, FOREIGN_UID]) {
      const ok = probeAs(identities.foreign(uid), [
        { op: "mkdir", path: path.join(out, `blocked-${uid}`), mode: 0o755 },
        { op: "create", path: path.join(out, `blocked-${uid}`, "file") },
        { op: "mkdir", path: path.join(out, `readonly-${uid}`), mode: 0o755 },
        { op: "create", path: path.join(out, `readonly-${uid}`, "file") },
        { op: "chmod", path: path.join(out, `readonly-${uid}`), mode: 0o555 },
        { op: "create", path: path.join(out, `loose-${uid}`) },
      ]);
      assert.ok(ok.results.every((r) => r.ok), JSON.stringify(ok.results));
    }
    fs.mkdirSync(path.join(out, "blocked-0"), { mode: 0o755 });
    fs.writeFileSync(path.join(out, "blocked-0", "file"), "root-written");
    fs.mkdirSync(path.join(out, "sticky"), { mode: 0o1777 });
    fs.chmodSync(path.join(out, "sticky"), 0o1777);
    assert.ok(probeAs(identities.foreign(FOREIGN_UID), [{ op: "create", path: path.join(out, "sticky", "foreign") }]).results[0]!.ok);
    const foreignKeys = (s: Snap): Snap => Object.fromEntries(Object.entries(s).filter(([key]) =>
      /^out\/(blocked|readonly|sticky)/.test(key) && !key.endsWith("live.log")));
    const planted = snapshot(target, isLive);
    assert.match(planted["out/blocked-0"]!, /^dir 0:/, "the rootful daemon's directory is root-owned");
    // A live foreign writer in a directory the deleting uid cannot modify.
    const writer = await startWriter(identities.foreign(FOREIGN_UID), path.join(out, `blocked-${FOREIGN_UID}`), path.join(out, `blocked-${FOREIGN_UID}`, "live.log"));
    const writerSize = await growsPast(writer.file, 0);

    const probe = (guard: "allow" | "veto"): { split: boolean; uid: number; deleted: boolean; before: number; after: number; lines: { msg: string; path?: string; code?: string; retainedUndeletable?: number }[] } => {
      const r = runAs(worker, process.execPath, ["--import", TSX, RETENTION_PROBE, JSON.stringify({ dataDir, parent: repoDir, target, guard })],
        { TMPDIR: cache, HOME: cache, ...(split ? { UZI_UID_SPLIT: "1" } : {}) });
      assert.equal(r.status, 0, r.stderr);
      return JSON.parse(r.stdout.trim().split("\n").at(-1)!);
    };

    // A writer guard that vetoes: nothing is attempted, so nothing is removed or counted.
    const vetoed = probe("veto");
    assert.equal(vetoed.uid, WORKER_UID);
    assert.equal(vetoed.split, split);
    assert.equal(vetoed.deleted, false);
    assert.equal(vetoed.after, vetoed.before, "a veto is not an undeletable failure");
    assertSame(snapshot(target, isLive), planted, `${mode_}: a vetoed delete removes nothing`);

    // Guards open: the delete runs as the owning uid and cannot remove the foreign content.
    const real = probe("allow");
    assert.equal(real.deleted, false, `${mode_}: the artifact stays retained`);
    assert.equal(real.after, real.before + 1, `${mode_}: the counter is incremented once`);
    const line = real.lines.find((l) => l.msg === "runner clone retention: could not delete (not permitted); keeping it");
    assert.ok(line, `${mode_}: the specific diagnostic is logged: ${JSON.stringify(real.lines)}`);
    assert.equal(line.path, target);
    assert.ok(line.code === "EACCES" || line.code === "EPERM", `${mode_}: errno ${line.code}`);
    assert.equal(line.retainedUndeletable, real.after);
    assert.ok(fs.existsSync(target), `${mode_}: the run directory is still there`);
    // Foreign content: present, still foreign-owned, same modes and inodes (no re-own, no chmod).
    assertSame(foreignKeys(snapshot(target, isLive)), foreignKeys(planted), `${mode_}: foreign-owned content`);
    for (const name of ["blocked-0", `blocked-${DIND_UID}`, `readonly-${DIND_UID}`, `blocked-${FOREIGN_UID}`, `readonly-${FOREIGN_UID}`, "sticky"]) {
      assert.ok(fs.existsSync(path.join(out, name)), `${mode_}: ${name} retained`);
    }
    assert.ok(await growsPast(writer.file, writerSize) > writerSize, `${mode_}: the live writer is undisturbed`);
    await stopWriters();
  }
});
