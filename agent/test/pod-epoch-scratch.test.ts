// Issue #4043 M1: the read-only inputs of the runner-scratch reclaim. Small fixtures only; nothing
// here deletes or spawns.
import assert from "node:assert/strict";
import * as fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import {
  KEPT_REASONS, MAX_ADMITTED_LEAVES, absorbBareConfigValue, absorbJsonPaths, checkScratchFilesystem, classifyBindSources,
  classifyScratchCensus, classifyScratchSettlement, censusEntryUnchanged, devToMajorMinor, evaluateScratchMounts,
  newPathSink, parseScratchCensus, pinScratchRoot, postCensusOf, protectedLeaves, readPodEntryFence, readProtectedStores,
  readScratchMounts, repinScratchRoot, scanScratchProcesses, scratchFilesystemOf, scratchLayout,
} from "../src/pod-epoch-scratch.js";
import type { CensusEntry, ParsedCensus, ScratchIo, ScratchLayout, SettlementInput } from "../src/pod-epoch-scratch.js";

function tmp(t: { after(fn: () => void): void }): string {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "scratch-m1-")));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}
const POD = "11111111-1111-4111-8111-111111111111";
const OTHER_POD = "22222222-2222-4222-8222-222222222222";

// ---- layout gate ------------------------------------------------------------------------------

test("scratchLayout admits exactly the two shipped layouts", () => {
  const split = scratchLayout({ UZI_UID_SPLIT: "1", UZI_RUNNER_TMPDIR: "/data/runner/.tmp/uzi-runner", TMPDIR: "/data/runner/.tmp/uzi-worker" });
  assert.deepEqual(split, { mode: "split", root: "/data/runner/.tmp/uzi-runner", ownerUid: 10002, modes: [0o700] });
  const single = scratchLayout({ TMPDIR: "/data/runner/.tmp" });
  assert.deepEqual(single, { mode: "single", root: "/data/runner/.tmp", ownerUid: 10001, modes: [0o755, 0o3775] });
});

test("scratchLayout refuses every other environment", () => {
  const refused = { refused: "layout_refused" };
  const base = { UZI_UID_SPLIT: "1", UZI_RUNNER_TMPDIR: "/data/runner/.tmp/uzi-runner", TMPDIR: "/data/runner/.tmp/uzi-worker" };
  const cases: NodeJS.ProcessEnv[] = [
    {}, { TMPDIR: "/data/runner" }, { TMPDIR: "/data/runner/.tmp/" }, { TMPDIR: "/tmp" },
    { ...base, UZI_UID_SPLIT: "0" }, { ...base, UZI_UID_SPLIT: undefined },
    { ...base, UZI_RUNNER_TMPDIR: "/data/runner/.tmp/uzi-runner/" }, { ...base, UZI_RUNNER_TMPDIR: undefined },
    { ...base, TMPDIR: "/data/runner/.tmp" }, { ...base, TMPDIR: "/data/runner/.tmp/uzi-runner" },
    { TMPDIR: "/data/runner/.tmp", UZI_UID_SPLIT: "1" },
    { TMPDIR: "/data/runner/.tmp", UZI_RUNNER_TMPDIR: "/data/runner/.tmp" },
    { TMPDIR: "/data/runner/.tmp", UZI_UID_SPLIT: "" },
  ];
  for (const env of cases) assert.deepEqual(scratchLayout(env), refused, JSON.stringify(env));
});

// ---- root pin ---------------------------------------------------------------------------------

function layoutFor(root: string, over: Partial<ScratchLayout> = {}): ScratchLayout {
  const st = fs.statSync(root);
  return { mode: "single", root, ownerUid: st.uid, modes: [st.mode & 0o7777], ...over };
}

test("pinScratchRoot pins by descriptor and reports the bigint dev:ino", t => {
  const root = tmp(t);
  fs.chmodSync(root, 0o755);
  const pin = pinScratchRoot(layoutFor(root));
  assert.ok(!("refused" in pin));
  const st = fs.statSync(root, { bigint: true });
  assert.equal(pin.devIno, `${st.dev}:${st.ino}`);
  assert.equal(pin.dev, String(st.dev));
  assert.equal(pin.ino, String(st.ino));
  // The descriptor keeps naming the inode after the path is renamed away.
  fs.renameSync(root, root + ".moved");
  t.after(() => fs.rmSync(root + ".moved", { recursive: true, force: true }));
  assert.equal(fs.fstatSync(pin.fd, { bigint: true }).ino, st.ino);
  pin.close();
  assert.equal(repinScratchRoot(layoutFor(root + ".moved"), `${st.dev}:${st.ino}`), true);
  assert.equal(repinScratchRoot(layoutFor(root + ".moved"), "1:1"), false);
});

test("pinScratchRoot refuses a symlinked root, a symlinked ancestor and a missing root as root_unpinned", t => {
  const dir = tmp(t);
  fs.mkdirSync(path.join(dir, "real", "child"), { recursive: true });
  fs.symlinkSync(path.join(dir, "real"), path.join(dir, "link"));
  const real = path.join(dir, "real", "child");
  const layout = layoutFor(real);
  assert.ok("devIno" in pinScratchRoot(layout));
  assert.deepEqual(pinScratchRoot({ ...layout, root: path.join(dir, "link", "child") }), { refused: "root_unpinned" });
  assert.deepEqual(pinScratchRoot({ ...layout, root: path.join(dir, "link") }), { refused: "root_unpinned" });
  assert.deepEqual(pinScratchRoot({ ...layout, root: path.join(dir, "absent") }), { refused: "root_unpinned" });
  fs.writeFileSync(path.join(dir, "file"), "");
  assert.deepEqual(pinScratchRoot({ ...layout, root: path.join(dir, "file") }), { refused: "root_unpinned" });
});

test("pinScratchRoot refuses a root with the wrong owner or mode as root_identity", t => {
  const root = tmp(t);
  fs.chmodSync(root, 0o755);
  assert.deepEqual(pinScratchRoot(layoutFor(root, { modes: [0o700] })), { refused: "root_identity" });
  assert.deepEqual(pinScratchRoot(layoutFor(root, { modes: [0o755, 0o3775], ownerUid: fs.statSync(root).uid + 1 })), { refused: "root_identity" });
  assert.ok("devIno" in pinScratchRoot(layoutFor(root, { modes: [0o700, 0o755] })));
});

// ---- pod-entry fence --------------------------------------------------------------------------

test("readPodEntryFence returns ctime minus 2 s for this pod's record", t => {
  const dir = tmp(t);
  const file = path.join(dir, "entry");
  fs.writeFileSync(file, JSON.stringify({ current: POD, previous: null }) + "\n");
  const fence = readPodEntryFence(POD, fs, file);
  assert.ok("t0Ms" in fence);
  assert.ok(Math.abs(fence.t0Ms - (fs.statSync(file).ctimeMs - 2000)) < 2);
  fs.writeFileSync(file, JSON.stringify({ current: POD, previous: OTHER_POD }) + "\n");
  assert.ok("t0Ms" in readPodEntryFence(POD, fs, file));
});

test("readPodEntryFence refuses every invalid record as pod_entry_invalid", t => {
  const dir = tmp(t);
  const file = path.join(dir, "entry");
  const bad = { refused: "pod_entry_invalid" };
  const body = (v: unknown) => fs.writeFileSync(file, typeof v === "string" ? v : JSON.stringify(v) + "\n");
  const cases: Array<[string, () => void]> = [
    ["other pod", () => body({ current: OTHER_POD, previous: null })],
    ["previous is this pod only", () => body({ current: OTHER_POD, previous: POD })],
    ["non-JSON", () => body("{not json")],
    ["array", () => body("[]")],
    ["extra key", () => body({ current: POD, previous: null, extra: 1 })],
    ["missing previous", () => body({ current: POD })],
    ["non-uuid current", () => body({ current: "x", previous: null })],
    ["non-uuid previous", () => body({ current: POD, previous: "x" })],
    ["oversized", () => body(JSON.stringify({ current: POD, previous: null }) + " ".repeat(200))],
    ["non-ASCII", () => body(JSON.stringify({ current: POD, previous: null }) + "é")],
    ["duplicate key text", () => body(`{"current":"${POD}","previous":null,"current":"${POD}"}`)],
    ["empty", () => body("")],
  ];
  for (const [name, arrange] of cases) {
    arrange();
    assert.deepEqual(readPodEntryFence(POD, fs, file), bad, name);
  }
  assert.deepEqual(readPodEntryFence("not-a-uuid", fs, file), bad);
  fs.rmSync(file);
  assert.deepEqual(readPodEntryFence(POD, fs, file), bad, "missing");
  fs.mkdirSync(file);
  assert.deepEqual(readPodEntryFence(POD, fs, file), bad, "directory");
  fs.rmdirSync(file);
  fs.writeFileSync(path.join(dir, "target"), JSON.stringify({ current: POD, previous: null }) + "\n");
  fs.symlinkSync(path.join(dir, "target"), file);
  assert.deepEqual(readPodEntryFence(POD, fs, file), bad, "symlink");
  fs.rmSync(file);
  fs.linkSync(path.join(dir, "target"), file);
  assert.deepEqual(readPodEntryFence(POD, fs, file), bad, "hard link count");
});

// ---- mounts -----------------------------------------------------------------------------------

const makedev = (major: bigint, minor: bigint): bigint =>
  ((major & 0xfffn) << 8n) | ((major & ~0xfffn) << 32n) | (minor & 0xffn) | ((minor & ~0xffn) << 12n);

test("devToMajorMinor decodes the Linux dev_t encoding", () => {
  assert.equal(devToMajorMinor(2049n), "8:1");
  assert.equal(devToMajorMinor(makedev(259n, 3n)), "259:3");
  assert.equal(devToMajorMinor(makedev(8n, 300n)), "8:300");
  assert.equal(devToMajorMinor(makedev(4096n + 7n, 0n)), "4103:0");
  assert.equal(devToMajorMinor(0n), "0:0");
});

const ROOT_DEV = makedev(8n, 1n);
const mount = (id: number, parent: number, dev: string, point: string, rest = "ext4 /dev/sda1 rw") =>
  `${id} ${parent} ${dev} /sub ${point} rw,relatime shared:1 - ${rest}`;
function mountinfo(extra: string[] = [], over: { data?: string; runner?: string } = {}): string {
  return [
    "20 1 0:20 / / rw - overlay overlay rw",
    "25 20 0:5 / /proc rw - proc proc rw",
    over.data ?? mount(30, 20, "8:1", "/data"),
    over.runner ?? mount(31, 30, "8:1", "/data/runner"),
    ...extra,
  ].join("\n") + "\n";
}
const fdinfo = (id: number | string = 31) => `pos:\t0\nflags:\t010000000\nmnt_id:\t${id}\nino:\t5\n`;
const SPLIT_ROOT = "/data/runner/.tmp/uzi-runner";
const SINGLE_ROOT = "/data/runner/.tmp";
const LEAVES = ["alpha", "beta", "a b"];

test("mount rules admit the shipped split and single layouts with no mounted leaves", () => {
  for (const root of [SPLIT_ROOT, SINGLE_ROOT])
    assert.deepEqual(evaluateScratchMounts({ mountinfo: mountinfo(), fdinfo: fdinfo(), rootDev: ROOT_DEV, root, leafNames: LEAVES }),
      { mountedLeaves: new Set() }, root);
});

test("a mount at or inside a leaf keeps that leaf, with octal-escaped mount points unescaped", () => {
  const result = evaluateScratchMounts({
    mountinfo: mountinfo([mount(40, 31, "0:50", SPLIT_ROOT + "/alpha", "tmpfs tmpfs rw"),
      mount(41, 31, "0:51", SPLIT_ROOT + "/beta/deep/er", "tmpfs tmpfs rw"),
      mount(42, 31, "0:52", SPLIT_ROOT + "/a\\040b", "tmpfs tmpfs rw")]),
    fdinfo: fdinfo(), rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES,
  });
  assert.deepEqual(result, { mountedLeaves: new Set(["alpha", "beta", "a b"]) });
});

test("a mount inside the root but outside every leaf is mount_unknown", () => {
  for (const point of [SPLIT_ROOT + "/ghost", SPLIT_ROOT + "/ghost/x"])
    assert.deepEqual(evaluateScratchMounts({ mountinfo: mountinfo([mount(40, 31, "0:50", point, "tmpfs tmpfs rw")]),
      fdinfo: fdinfo(), rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES }), { refused: "mount_unknown" }, point);
  // A mount exactly at the root shadows it.
  assert.deepEqual(evaluateScratchMounts({ mountinfo: mountinfo([mount(40, 31, "0:50", SPLIT_ROOT, "tmpfs tmpfs rw")]),
    fdinfo: fdinfo(), rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES }), { refused: "mount_unknown" });
});

test("an unexpected ancestor mount refuses the phase", () => {
  assert.deepEqual(evaluateScratchMounts({ mountinfo: mountinfo([mount(40, 31, "8:1", "/data/runner/.tmp")]),
    fdinfo: fdinfo(), rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES }), { refused: "mount_unexpected_ancestor" });
  // The same mount is the root itself in the single layout: a shadow, never an admitted shape.
  assert.deepEqual(evaluateScratchMounts({ mountinfo: mountinfo([mount(40, 31, "8:1", "/data/runner/.tmp")]),
    fdinfo: fdinfo(), rootDev: ROOT_DEV, root: SINGLE_ROOT, leafNames: LEAVES }), { refused: "mount_unknown" });
});

test("a wrong or unknown mnt_id, a device mismatch and an overmount are refused", () => {
  const args = { rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES };
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: fdinfo(99) }), { refused: "mount_unknown" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: fdinfo(30) }), { refused: "mount_unexpected_ancestor" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: fdinfo(20) }), { refused: "mount_unexpected_ancestor" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: fdinfo(), rootDev: makedev(8n, 2n) }), { refused: "mount_unknown" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo([mount(40, 31, "8:1", "/data/runner")]), fdinfo: fdinfo() }),
    { refused: "mount_unexpected_ancestor" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo([mount(40, 30, "8:1", "/data/runner")]), fdinfo: fdinfo(40) }),
    { refused: "mount_unexpected_ancestor" });
});

test("/data must be a single mount that is the parent of the enclosing mount on the same device", () => {
  const args = { rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES, fdinfo: fdinfo() };
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo([mount(40, 20, "8:1", "/data")]) }), { refused: "mount_unexpected_ancestor" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo([], { runner: mount(31, 20, "8:1", "/data/runner") }) }),
    { refused: "mount_unexpected_ancestor" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo([], { data: mount(30, 20, "8:2", "/data") }) }),
    { refused: "mount_unexpected_ancestor" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo([], { data: "20 1 0:20 / / rw - overlay overlay rw" }) }),
    { refused: "mount_unknown" }, "duplicate mount id");
});

test("unreadable, oversized or malformed mount text is mount_unknown", () => {
  const args = { rootDev: ROOT_DEV, root: SPLIT_ROOT, leafNames: LEAVES };
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: "pos:\t0\n" }), { refused: "mount_unknown" }, "no mnt_id");
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: fdinfo() + "mnt_id:\t31\n" }), { refused: "mount_unknown" }, "two mnt_id");
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo(), fdinfo: "mnt_id:\tx\n" }), { refused: "mount_unknown" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: "", fdinfo: fdinfo() }), { refused: "mount_unknown" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo() + "garbage\n", fdinfo: fdinfo() }), { refused: "mount_unknown" });
  assert.deepEqual(evaluateScratchMounts({ ...args, mountinfo: mountinfo() + "x".repeat(256 * 1024), fdinfo: fdinfo() }), { refused: "mount_unknown" });
});

test("readScratchMounts reads both files fresh through the io seam and fails closed on an unreadable or oversized one", t => {
  const root = tmp(t);
  const pin = pinScratchRoot(layoutFor(root));
  assert.ok(!("refused" in pin));
  t.after(() => pin.close());
  const mountsFile = "/proc/self/mountinfo", infoFile = `/proc/self/fdinfo/${pin.fd}`;
  const served = (files: Record<string, string | Error>) => {
    const open = new Map<number, Buffer>();
    return { ...fs,
      openSync: (p: string) => {
        const body = files[p];
        if (body === undefined || body instanceof Error) throw body ?? Object.assign(new Error("ENOENT"), { code: "ENOENT" });
        const fd = 1000 + open.size; open.set(fd, Buffer.from(body)); return fd;
      },
      readSync: (fd: number, buf: Buffer, off: number, len: number) => {
        const data = open.get(fd)!; const n = Math.min(len, data.length); data.copy(buf, off, 0, n); open.set(fd, data.subarray(n)); return n;
      },
      closeSync: () => undefined } as unknown as ScratchIo;
  };
  const files = { [mountsFile]: mountinfo(), [infoFile]: fdinfo() };
  // The fixture's /data/runner device is 8:1 and the temp root sits on whatever device it sits on, so
  // the device cross-check decides: the matching device admits, any other is mount_unknown.
  const matching = mountinfo().replace(/8:1/g, devToMajorMinor(BigInt(pin.dev)));
  assert.deepEqual(readScratchMounts(pin, [], SPLIT_ROOT, served({ ...files, [mountsFile]: matching })), { mountedLeaves: new Set() });
  if (devToMajorMinor(BigInt(pin.dev)) !== "8:1") assert.deepEqual(readScratchMounts(pin, [], SPLIT_ROOT, served(files)), { refused: "mount_unknown" });
  assert.deepEqual(readScratchMounts(pin, [], SPLIT_ROOT, served({ ...files, [mountsFile]: new Error("EACCES") })), { refused: "mount_unknown" });
  assert.deepEqual(readScratchMounts(pin, [], SPLIT_ROOT, served({ ...files, [infoFile]: new Error("EACCES") })), { refused: "mount_unknown" });
  assert.deepEqual(readScratchMounts(pin, [], SPLIT_ROOT, served({ ...files, [mountsFile]: matching + "x".repeat(300 * 1024) })), { refused: "mount_unknown" });
});

// ---- filesystem -------------------------------------------------------------------------------

test("only ext4 and XFS pass the filesystem check", () => {
  assert.equal(checkScratchFilesystem(0xef53), "ok");
  assert.equal(checkScratchFilesystem(0x58465342n), "ok");
  for (const type of [0x9123683e, 0x01021994, 0, 0x794c7630, 0xef54n]) assert.equal(checkScratchFilesystem(type), "fs_unsupported");
});

test("scratchFilesystemOf reads statfs through the pinned descriptor and fails closed", t => {
  const root = tmp(t);
  const pin = pinScratchRoot(layoutFor(root));
  assert.ok(!("refused" in pin));
  t.after(() => pin.close());
  const seen: string[] = [];
  const io = (type: number | bigint | Error) => ({ ...fs, statfsSync: (p: string) => {
    seen.push(p);
    if (type instanceof Error) throw type;
    return { type };
  } }) as unknown as ScratchIo;
  assert.equal(scratchFilesystemOf(pin, io(0xef53)), "ok");
  assert.deepEqual(seen, [`/proc/self/fd/${pin.fd}`]);
  assert.equal(scratchFilesystemOf(pin, io(0x9123683e)), "fs_unsupported");
  assert.equal(scratchFilesystemOf(pin, io(new Error("EIO"))), "fs_unsupported");
});

// ---- processes --------------------------------------------------------------------------------

interface FakeProc { uid?: string; gid?: string; cwd?: string; fds?: string[]; status?: string }
function procTree(dir: string, procs: Record<number, FakeProc>): string {
  const proc = path.join(dir, "proc");
  fs.mkdirSync(proc);
  for (const [pid, p] of Object.entries(procs)) {
    const base = path.join(proc, pid);
    fs.mkdirSync(path.join(base, "fd"), { recursive: true });
    fs.writeFileSync(path.join(base, "status"), p.status ?? `Name:\tx\nUid:\t${p.uid ?? "10001\t10001\t10001\t10001"}\nGid:\t${p.gid ?? "10001\t10001\t10001\t10001"}\n`);
    fs.symlinkSync(p.cwd ?? "/", path.join(base, "cwd"));
    (p.fds ?? []).forEach((target, i) => fs.symlinkSync(target, path.join(base, "fd", String(i))));
  }
  return proc;
}

test("scanScratchProcesses keeps leaves held by a cwd or descriptor, including deleted links", t => {
  const proc = procTree(tmp(t), {
    1: { cwd: SPLIT_ROOT + "/alpha/sub" },
    2: { cwd: "/", fds: [SPLIT_ROOT + "/beta (deleted)", "socket:[1]", "/other/place", SPLIT_ROOT + "/alphabet/x"] },
    3: { cwd: SPLIT_ROOT },
  });
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha", "beta", "gamma", "alphabet2"], fs, proc), { held: new Set(["alpha", "beta"]) });
});

test("scanScratchProcesses also matches the /data/runner-shared alias of the root", t => {
  const proc = procTree(tmp(t), {
    1: { cwd: "/data/runner-shared/.tmp/uzi-runner/alpha/sub" },
    2: { cwd: "/", fds: ["/data/runner-shared/.tmp/uzi-runner/beta (deleted)", "/data/runner-shared/.tmp/uzi-runner-2/gamma"] },
  });
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha", "beta", "gamma"], fs, proc), { held: new Set(["alpha", "beta"]) });
  // A root outside /data/runner has no alias.
  assert.deepEqual(scanScratchProcesses("/elsewhere/root", ["alpha"], fs, proc), { held: new Set() });
});

test("scanScratchProcesses refuses a process that is not exactly the worker uid and gid", t => {
  const cases: Array<[string, FakeProc]> = [
    ["real uid", { uid: "10002\t10001\t10001\t10001" }], ["effective uid", { uid: "10001\t10002\t10001\t10001" }],
    ["saved uid", { uid: "10001\t10001\t10002\t10001" }], ["fs uid", { uid: "10001\t10001\t10001\t0" }],
    ["gid", { gid: "10001\t10001\t10001\t10002" }], ["short uid line", { uid: "10001\t10001\t10001" }],
    ["no status lines", { status: "Name:\tx\n" }],
  ];
  for (const [name, p] of cases)
    assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], fs, procTree(tmp(t), { 7: p })), { refused: "not_idle" }, name);
});

test("scanScratchProcesses skips a vanished pid but refuses an unreadable one or an oversized table", t => {
  const dir = tmp(t);
  const proc = procTree(dir, { 5: {}, 6: {} });
  fs.rmSync(path.join(proc, "6", "status"));
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], fs, proc), { held: new Set() });
  const denied = (target: string, code: string) => ({ ...fs, readFileSync: ((p: string, ...rest: unknown[]) => {
    if (p === target) throw Object.assign(new Error(code), { code });
    return (fs.readFileSync as (...a: unknown[]) => unknown)(p, ...rest);
  }) }) as unknown as ScratchIo;
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], denied(path.join(proc, "5", "status"), "EACCES"), proc), { refused: "not_idle" });
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], denied(path.join(proc, "5", "status"), "ESRCH"), proc), { held: new Set() });
  const many = Object.fromEntries(Array.from({ length: 129 }, (_, n) => [n + 1, {}]));
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], fs, procTree(tmp(t), many)), { refused: "not_idle" });
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], fs, path.join(dir, "missing")), { refused: "not_idle" });
});

test("scanScratchProcesses refuses an unreadable descriptor, cwd or fd directory as self_descriptor_unreadable", t => {
  const proc = procTree(tmp(t), { 5: { fds: [SPLIT_ROOT + "/alpha"] } });
  const failing = (what: "readlinkSync" | "readdirSync", match: (p: string) => boolean, code: string) => ({ ...fs, [what]: (p: string, ...rest: unknown[]) => {
    if (match(p)) throw Object.assign(new Error(code), { code });
    return (fs[what] as (...a: unknown[]) => unknown)(p, ...rest);
  } }) as unknown as ScratchIo;
  const refused = { refused: "self_descriptor_unreadable" };
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], failing("readlinkSync", p => p.endsWith("/fd/0"), "EACCES"), proc), refused);
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], failing("readlinkSync", p => p.endsWith("/cwd"), "EACCES"), proc), refused);
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, ["alpha"], failing("readdirSync", p => p.endsWith("/5/fd"), "EACCES"), proc), refused);
  // A descriptor closed between listing and reading is not an error.
  assert.deepEqual(scanScratchProcesses(SPLIT_ROOT, [], failing("readlinkSync", p => p.endsWith("/fd/0"), "ENOENT"), proc), { held: new Set() });
});

// ---- bind references --------------------------------------------------------------------------

test("classifyBindSources classifies ancestors, leaf references and unrelated sources", t => {
  const root = tmp(t);
  fs.mkdirSync(path.join(root, "alpha"));
  const dirSt = { isDirectory: () => true, isSymbolicLink: () => false };
  const shared = path.dirname(root);
  const run = (sources: string[], lstat = (p: string) => (fs.existsSync(p) ? fs.lstatSync(p) : undefined)) => classifyBindSources({ sources, root, lstat, shared });
  assert.deepEqual(run([]), { referenced: new Set() });
  assert.deepEqual(run([root + "-sibling/x"]), { referenced: new Set() });
  // Outside the shared subPath the daemon resolves the source in a filesystem the worker cannot see.
  for (const outside of ["/elsewhere", "/var/run/docker.sock", "/tmp/l"]) assert.deepEqual(run([outside]), { refused: "bind_unknown" }, outside);
  assert.deepEqual(run([path.join(root, "alpha"), path.join(root, "alpha", "sub", "deep")]), { referenced: new Set(["alpha"]) });
  for (const ancestor of [root, path.dirname(root), "/"]) assert.deepEqual(run([ancestor]), { refused: "bind_ancestor" }, ancestor);
  assert.deepEqual(run([root + "-sibling", root]), { refused: "bind_ancestor" });
  // The first component under the root must be a real directory.
  assert.deepEqual(run([path.join(root, "ghost", "x")]), { refused: "bind_unknown" });
  fs.writeFileSync(path.join(root, "file"), "");
  fs.symlinkSync(path.join(root, "alpha"), path.join(root, "link"));
  assert.deepEqual(run([path.join(root, "file")]), { refused: "bind_unknown" });
  assert.deepEqual(run([path.join(root, "link", "x")]), { refused: "bind_unknown" });
  assert.deepEqual(classifyBindSources({ sources: [root + "/alpha"], root, shared, lstat: () => { throw new Error("EIO"); } }), { refused: "bind_unknown" });
  assert.deepEqual(classifyBindSources({ sources: [root + "/alpha"], root, shared, lstat: () => dirSt }), { referenced: new Set(["alpha"]) });
  // The shipped default view is the literal /data/runner: a root outside it refuses every source.
  assert.deepEqual(classifyBindSources({ sources: ["/srv/scratch/alpha"], root: "/srv/scratch", lstat: () => dirSt }), { refused: "bind_unknown" });
});

test("a symlink in any component of a bind source under the shared tree is bind_unknown", t => {
  const dir = tmp(t);
  const root = path.join(dir, "root");
  fs.mkdirSync(path.join(root, "A"), { recursive: true });
  fs.mkdirSync(path.join(root, "B"));
  const lstat = (p: string) => fs.lstatSync(p, { throwIfNoEntry: false });
  const run = (sources: string[]) => classifyBindSources({ sources, root, lstat, shared: dir });
  // (a) R/A/link -> ../B: only A is named, B is the real target.
  fs.symlinkSync("../B", path.join(root, "A", "link"));
  assert.deepEqual(run([path.join(root, "A", "link", "x")]), { refused: "bind_unknown" });
  // (b) a source outside the root that resolves, through a symlink, into R/B.
  fs.symlinkSync(path.join(root, "B"), path.join(dir, "outside"));
  assert.deepEqual(run([path.join(dir, "outside", "x")]), { refused: "bind_unknown" });
  assert.deepEqual(run([path.join(dir, "outside")]), { refused: "bind_unknown" });
  // Any symlink below the shared tree refuses, even one resolving away from the root.
  fs.mkdirSync(path.join(dir, "elsewhere"));
  fs.symlinkSync(path.join(dir, "elsewhere"), path.join(dir, "harmless"));
  assert.deepEqual(run([path.join(dir, "harmless", "sock")]), { refused: "bind_unknown" });
  // A missing tail below a real leaf is classified by its literal path; a missing outside path is unrelated.
  assert.deepEqual(run([path.join(root, "A", "not", "there")]), { referenced: new Set(["A"]) });
  assert.deepEqual(run([path.join(dir, "nowhere", "x")]), { referenced: new Set() });
});

test("a bind source not in normal form is bind_unknown", t => {
  const root = tmp(t);
  for (const source of ["relative/path", "", root + "/alpha/", root + "//alpha", root + "/./alpha", root + "/alpha/../beta", "/data/runner/../x", "/a//b"])
    assert.deepEqual(classifyBindSources({ sources: [source], root, lstat: () => undefined }), { refused: "bind_unknown" }, JSON.stringify(source));
});

// ---- protected records ------------------------------------------------------------------------

test("protectedLeaves compares component-wise after normalisation", () => {
  const root = SPLIT_ROOT;
  const leaves = ["leaf", "other"];
  const hit = (p: string) => [...protectedLeaves([p], root, leaves)];
  assert.deepEqual(hit(root + "/leaf"), ["leaf"]);
  assert.deepEqual(hit(root + "/leaf/deep/er"), ["leaf"]);
  assert.deepEqual(hit(root + "/leaf/../other/x"), ["other"]);
  assert.deepEqual(hit(root + "//leaf/./x"), ["leaf"]);
  assert.deepEqual(hit(root + "/leaf-2"), []);
  assert.deepEqual(hit(root + "/leafy/x"), []);
  assert.deepEqual(hit(root), []);
  assert.deepEqual(hit("/data/runner/.tmp/uzi-runner-2/leaf"), []);
  assert.deepEqual(hit("relative/leaf"), []);
  assert.deepEqual(hit(root + "/unlisted"), []);
  assert.deepEqual([...protectedLeaves([root + "/leaf", root + "/other/x", "/x"], root, leaves)].sort(), ["leaf", "other"]);
});

test("protectedLeaves matches the /data/runner-shared alias and never a string-prefix sibling of the root", () => {
  const leaves = ["leaf", "other"];
  const hit = (p: string) => [...protectedLeaves([p], SPLIT_ROOT, leaves)];
  assert.deepEqual(hit("/data/runner-shared/.tmp/uzi-runner/leaf/x"), ["leaf"]);
  assert.deepEqual(hit("/data/runner-shared/.tmp/uzi-runner/../uzi-runner/other"), ["other"]);
  assert.deepEqual(hit("/data/runner-shared/.tmp/uzi-runner-2/leaf"), []);
  assert.deepEqual(hit(SPLIT_ROOT + "Xleaf/x"), [], "root as a string prefix of a sibling");
  assert.deepEqual(hit(SPLIT_ROOT + "Xleaf"), []);
});

test("absorbJsonPaths collects absolute strings and keys and refuses over its bounds", () => {
  const sink = newPathSink();
  assert.equal(absorbJsonPaths({ "/key": [{ a: "/one", b: "not/abs", c: 5, d: null }], n: "/two" }, sink), true);
  assert.deepEqual([...sink.paths].sort(), ["/key", "/one", "/two"]);
  let deep: unknown = "/x";
  for (let i = 0; i < 70; i++) deep = [deep];
  assert.equal(absorbJsonPaths(deep, newPathSink()), false);
  assert.equal(absorbJsonPaths("/" + "a".repeat(5000), newPathSink()), false);
  assert.equal(absorbJsonPaths(Array.from({ length: 70_000 }, (_, n) => "/p" + n), newPathSink()), false);
});

test("absorbBareConfigValue handles JSON, plain paths, empty and uninterpretable values", () => {
  const sink = newPathSink();
  assert.equal(absorbBareConfigValue("uzi-recovery.b.clone", "/data/runner/slug/issue-1", sink), "ok");
  assert.equal(absorbBareConfigValue("uzi-retained.r.journal", JSON.stringify({ clonePath: "/data/runner/slug/issue-2", n: 1 }), sink), "ok");
  assert.equal(absorbBareConfigValue("uzi-recovery.b2.clone", "", sink), "ok");
  assert.equal(absorbBareConfigValue("uzi-attemptmode.k.enabled", "yes", sink), "ok");
  assert.deepEqual([...sink.paths].sort(), ["/data/runner/slug/issue-1", "/data/runner/slug/issue-2"]);
  for (const key of ["uzi-recovery.b.clone", "uzi-retained.r.journal", "uzi-attempts.b.entry", "uzi-stopped.r.entry", "uzi-recoveryowner.r.record"])
    assert.equal(absorbBareConfigValue(key, "{truncated", newPathSink()), "unknown", key);
  assert.equal(absorbBareConfigValue("uzi-attempts.b.entry", JSON.stringify(Array(70).fill(0).reduce((v: unknown) => [v], "/x")), newPathSink()), "unknown");
});

// ---- protected stores ---------------------------------------------------------------------------

const UUID_A = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const UUID_B = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
function stores(t: { after(fn: () => void): void }): { dir: string; roots: string[]; put(rel: string, body: string | Buffer): void } {
  const dir = tmp(t);
  const roots = ["recovery", "recovery-maintenance", "recovery-settlement", "recovery-archive"].map(n => path.join(dir, n));
  return { dir, roots, put(rel, body) {
    const file = path.join(dir, rel);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, body);
  } };
}

test("readProtectedStores treats a missing store as empty and collects paths from every store", async t => {
  const s = stores(t);
  assert.deepEqual(await readProtectedStores(s.roots), { state: "read", paths: [] });
  s.put(`recovery/run-1/${UUID_A}.json`, JSON.stringify({ clonePath: "/data/runner/.tmp/uzi-runner/leaf-1", bundlePath: "/data/recovery/run-1/x.bundle" }));
  s.put("recovery-maintenance/m.json", JSON.stringify([{ p: "/data/runner/.tmp/uzi-runner/leaf-2" }]));
  s.put("recovery-settlement/run-1", JSON.stringify({ "/data/runner/.tmp/uzi-runner/leaf-3": 1 }));
  s.put("recovery-archive/run-1/g1.manifest.json", JSON.stringify({ note: "nothing" }));
  assert.deepEqual(await readProtectedStores(s.roots), { state: "read", paths: [
    "/data/recovery/run-1/x.bundle", "/data/runner/.tmp/uzi-runner/leaf-1", "/data/runner/.tmp/uzi-runner/leaf-2", "/data/runner/.tmp/uzi-runner/leaf-3"] });
});

test("readProtectedStores skips exactly the binary artifacts and parses everything else", async t => {
  const s = stores(t);
  const binary = [`${UUID_A}.bundle`, `${UUID_A}.${UUID_B}.bundle.tmp`, `maintenance-v1-${UUID_A}.bundle`, "g12.bundle", ".g3.0123456789ab.bundle.tmp", ".g3.0123456789ab.publish.tmp"];
  for (const name of binary) s.put(`recovery/run/${name}`, Buffer.from([0x50, 0x41, 0x43, 0x4b, 0, 255, 254]));
  assert.deepEqual(await readProtectedStores(s.roots), { state: "read", paths: [] });
  // Near-misses are not skipped, so their non-JSON bytes are malformed.
  for (const name of [`${UUID_A}.bundle.bak`, "g12.bundle.tmp", ".g3.0123456789ab.manifest.bundle.tmp", `x${UUID_A}.bundle`, "g.bundle", ".g3.xyz.bundle.tmp"]) {
    const near = stores(t);
    near.put(`recovery/run/${name}`, Buffer.from([0x50, 0x41, 0x43, 0x4b]));
    assert.deepEqual(await readProtectedStores(near.roots), { state: "malformed", reason: "store_file_malformed" }, name);
  }
  // A skipped name that is a directory is still descended.
  const dir = stores(t);
  dir.put(`recovery/g1.bundle/inner.json`, JSON.stringify({ p: "/inside" }));
  assert.deepEqual(await readProtectedStores(dir.roots), { state: "read", paths: ["/inside"] });
});

test("readProtectedStores reports malformed, truncated and empty records as malformed", async t => {
  for (const [name, body] of [["truncated", '{"clonePath":"/x'], ["empty", ""], ["bad utf8", Buffer.from([0x7b, 0xff, 0x7d])], ["not json", "hello"]] as const) {
    const s = stores(t);
    s.put("recovery/run/r.json", body);
    assert.deepEqual(await readProtectedStores(s.roots), { state: "malformed", reason: "store_file_malformed" }, name);
  }
});

test("readProtectedStores reports unreadable, special, oversized and over-deep stores as unknown", async t => {
  const link = stores(t);
  fs.mkdirSync(path.join(link.dir, "elsewhere"));
  fs.symlinkSync(path.join(link.dir, "elsewhere"), link.roots[0]!);
  assert.deepEqual(await readProtectedStores(link.roots), { state: "unknown", reason: "store_unreadable" }, "symlinked store");

  const entry = stores(t);
  entry.put("recovery/run/r.json", "{}");
  fs.symlinkSync(path.join(entry.dir, "recovery", "run", "r.json"), path.join(entry.dir, "recovery", "run", "link.json"));
  assert.deepEqual(await readProtectedStores(entry.roots), { state: "unknown", reason: "store_unreadable" }, "symlinked file");

  const big = stores(t);
  big.put("recovery/run/r.json", JSON.stringify({ pad: "x".repeat(1024 * 1024) }));
  assert.deepEqual(await readProtectedStores(big.roots), { state: "unknown", reason: "store_file_cap" });

  const deep = stores(t);
  deep.put(`recovery/${Array.from({ length: 9 }, (_, n) => "d" + n).join("/")}/r.json`, "{}");
  assert.deepEqual(await readProtectedStores(deep.roots), { state: "unknown", reason: "store_depth" });
  const ok = stores(t);
  ok.put(`recovery/${Array.from({ length: 7 }, (_, n) => "d" + n).join("/")}/r.json`, "{}");
  assert.deepEqual(await readProtectedStores(ok.roots), { state: "read", paths: [] });

  const total = stores(t);
  const chunk = JSON.stringify({ pad: "x".repeat(1000 * 1000) });
  for (let n = 0; n < 70; n++) total.put(`recovery/run/r${n}.json`, chunk);
  assert.deepEqual(await readProtectedStores(total.roots), { state: "unknown", reason: "store_byte_cap" });

  const many = stores(t);
  fs.mkdirSync(many.roots[0]!, { recursive: true });
  for (let n = 0; n < 16_385; n++) fs.writeFileSync(path.join(many.roots[0]!, `.g1.${n.toString(16).padStart(12, "0")}.bundle.tmp`), "");
  assert.deepEqual(await readProtectedStores(many.roots), { state: "unknown", reason: "store_entry_cap" });

  const stop = stores(t);
  stop.put("recovery/run/r.json", "{}");
  const interrupt = () => { throw new Error("interrupted"); };
  assert.deepEqual(await readProtectedStores(stop.roots, { interrupt }), { state: "unknown", reason: "store_unreadable" });
});

// ---- census classification ----------------------------------------------------------------------

const ROOT_UID = 10002;
function entry(name: string, over: Partial<CensusEntry> = {}): CensusEntry {
  return { name, type: "dir", devIno: "1:" + name.length, uid: ROOT_UID, fsDiffers: false, foreignUid: false, bytes: 4096,
    sharedBytes: 0, maxCtimeMs: 1000, incomplete: false, ...over };
}
function census(entries: CensusEntry[]): ParsedCensus {
  return { rootDevIno: "7:7", rootUid: ROOT_UID, entries, overflow: false };
}
const NONE = new Set<string>();

test("every kept reason is counted once, in precedence order, and only clean leaves are admitted", () => {
  const entries = [
    entry("uzi-worker"), entry("uzi-runner"), entry("link", { type: "other" }), entry(".dot"), entry("-dash"), entry("sp ace"),
    entry("uzi-quiesce-abc"), entry("foreign", { uid: 0 }), entry("foreign-inner", { foreignUid: true }), entry("mounted", { fsDiffers: true }),
    entry("mounted2"), entry("partial", { incomplete: true }), entry("recent", { maxCtimeMs: 5000 }), entry("recent-edge", { maxCtimeMs: 4000 }),
    entry("recorded"), entry("bound"), entry("held"), entry("clean-b", { bytes: 8192, sharedBytes: 100 }), entry("clean-a", { bytes: 4096 }),
    entry("gone", { type: "absent" }),
  ];
  const result = classifyScratchCensus({ census: census(entries), t0Ms: 4000, mountedLeaves: new Set(["mounted2"]),
    boundLeaves: new Set(["bound"]), protectedLeaves: new Set(["recorded"]), heldLeaves: new Set(["held"]) });
  assert.deepEqual(result.kept, {
    protected_name: 2, not_directory: 2, name_refused: 3, active_helper: 1, foreign_owner: 2, mount: 2, incomplete: 1,
    current_pod: 2, protected_record: 1, bind_referenced: 1, held_open: 1, cap: 0,
  });
  assert.deepEqual(Object.keys(result.kept), [...KEPT_REASONS]);
  assert.deepEqual(result.admitted.map(l => l.name), ["clean-a", "clean-b"]);
  assert.equal(result.examined, entries.length);
  assert.equal(result.eligibleBytes, 4096 + 8192);
  assert.equal(result.sharedBytes, 100);
});

test("a protected name outranks other reasons, and a dot-named directory is refused by the name rule", () => {
  const result = classifyScratchCensus({ census: census([entry("uzi-worker", { uid: 0, incomplete: true }), entry(".hidden")]), t0Ms: 0,
    mountedLeaves: NONE, boundLeaves: NONE, protectedLeaves: NONE, heldLeaves: NONE });
  assert.equal(result.kept.protected_name, 1);
  assert.equal(result.kept.name_refused, 1);
  assert.equal(result.kept.foreign_owner, 0);
  assert.equal(result.admitted.length, 0);
});

test("admission is capped at 512 leaves in name order and the rest are kept as cap", () => {
  const entries = Array.from({ length: MAX_ADMITTED_LEAVES + 3 }, (_, n) => entry("leaf-" + String(n).padStart(4, "0")));
  const result = classifyScratchCensus({ census: census(entries.reverse()), t0Ms: 5000, mountedLeaves: NONE, boundLeaves: NONE,
    protectedLeaves: NONE, heldLeaves: NONE });
  assert.equal(result.admitted.length, 512);
  assert.equal(result.kept.cap, 3);
  assert.equal(result.admitted[0]!.name, "leaf-0000");
  assert.equal(result.admitted.at(-1)!.name, "leaf-0511");
});

test("censusEntryUnchanged flags any difference from the admitted measurement", () => {
  const leaf = { name: "x", devIno: "1:1", bytes: 4096, sharedBytes: 0, maxCtimeMs: 10 };
  const same = entry("x", { devIno: "1:1", maxCtimeMs: 10 });
  assert.equal(censusEntryUnchanged(leaf, same, ROOT_UID), true);
  const changed: Array<Partial<CensusEntry>> = [{ devIno: "1:2" }, { bytes: 4097 }, { sharedBytes: 1 }, { maxCtimeMs: 11 },
    { fsDiffers: true }, { foreignUid: true }, { uid: 0 }, { incomplete: true }, { type: "other" }];
  for (const over of changed) assert.equal(censusEntryUnchanged(leaf, { ...same, ...over }, ROOT_UID), false, JSON.stringify(over));
  assert.equal(censusEntryUnchanged(leaf, undefined, ROOT_UID), false);
});

// ---- census parsing ---------------------------------------------------------------------------

const line = (o: unknown) => JSON.stringify(o);
const goodCensus = [
  line({ root: "7:7", uid: 10002, mode: 448 }),
  line({ name: "a", type: "dir", dev: "7", ino: "11", uid: 10002, fsdiff: false, fuid: false, bytes: 4096, shared: 0, ctime: 5, inc: false }),
  line({ name: "b", type: "absent" }),
  line({ end: true, entries: 2, overflow: false }),
].join("\n") + "\n";

test("parseScratchCensus accepts a closed listing and checks the root identity", () => {
  const parsed = parseScratchCensus({ code: 0, stdout: goodCensus }, "7:7");
  assert.ok(!("refused" in parsed));
  assert.equal(parsed.rootUid, 10002);
  assert.deepEqual(parsed.entries.map(e => [e.name, e.type, e.devIno]), [["a", "dir", "7:11"], ["b", "absent", ""]]);
  assert.deepEqual(parseScratchCensus({ code: 0, stdout: goodCensus }, "7:8"), { refused: "census_identity" });
  assert.deepEqual(parseScratchCensus({ code: 3, stdout: goodCensus.split("\n")[0]! + "\n" }, "7:7"), { refused: "census_identity" });
});

test("an unreadable single-leaf entry parses as incomplete and maps to PostCensus unreadable, never other", () => {
  const stdout = [line({ root: "7:7", uid: 10002, mode: 448 }),
    line({ name: "a", type: "unreadable", dev: "0", ino: "0", uid: -1, fsdiff: false, fuid: false, bytes: 0, shared: 0, ctime: 0, inc: true }),
    line({ end: true, entries: 1, overflow: false })].join("\n") + "\n";
  const parsed = parseScratchCensus({ code: 0, stdout }, "7:7");
  assert.ok(!("refused" in parsed));
  assert.equal(parsed.entries[0]!.type, "unreadable");
  assert.equal(parsed.entries[0]!.incomplete, true);
  assert.deepEqual(postCensusOf(parsed.entries[0], "7:11"), { state: "unreadable" });
  assert.deepEqual(postCensusOf(undefined, "7:11"), { state: "unreadable" });
  assert.deepEqual(postCensusOf(entry("a", { type: "absent" }), "7:11"), { state: "absent" });
  assert.deepEqual(postCensusOf(entry("a", { type: "other", devIno: "7:11" }), "7:11"), { state: "other" });
  assert.deepEqual(postCensusOf(entry("a", { devIno: "7:12" }), "7:11"), { state: "other" });
  assert.deepEqual(postCensusOf(entry("a", { devIno: "7:11", bytes: 90, incomplete: true }), "7:11"), { state: "same", bytes: 90, complete: false });
});

test("parseScratchCensus refuses a failed exit, a missing end line and every malformed field", () => {
  const rows = goodCensus.trimEnd().split("\n");
  const without = (i: number) => rows.filter((_, n) => n !== i).join("\n") + "\n";
  const swap = (i: number, v: unknown) => rows.map((r, n) => (n === i ? line(v) : r)).join("\n") + "\n";
  const entryRow = JSON.parse(rows[1]!) as Record<string, unknown>;
  assert.deepEqual(parseScratchCensus({ code: 1, stdout: goodCensus }, "7:7"), { refused: "census_failed" });
  assert.deepEqual(parseScratchCensus({ code: 7, stdout: goodCensus }, "7:7"), { refused: "census_failed" });
  const malformed: Array<[string, string]> = [
    ["empty", ""], ["no end line", without(3)], ["no root line", without(0)], ["not json", goodCensus + "{"],
    ["count mismatch", swap(3, { end: true, entries: 3, overflow: false })], ["bad overflow", swap(3, { end: true, entries: 2, overflow: "no" })],
    ["duplicate name", swap(2, { name: "a", type: "absent" })], ["slash in name", swap(1, { ...entryRow, name: "a/b" })],
    ["empty name", swap(1, { ...entryRow, name: "" })], ["unknown type", swap(1, { ...entryRow, type: "sock" })],
    ["negative bytes", swap(1, { ...entryRow, bytes: -1 })], ["float bytes", swap(1, { ...entryRow, bytes: 1.5 })],
    ["string uid", swap(1, { ...entryRow, uid: "1" })], ["non-decimal ino", swap(1, { ...entryRow, ino: "0x1" })],
    ["missing flag", swap(1, { ...entryRow, inc: undefined })], ["array row", swap(1, [])],
  ];
  for (const [name, stdout] of malformed) assert.deepEqual(parseScratchCensus({ code: 0, stdout }, "7:7"), { refused: "census_malformed" }, name);
});

// ---- settlement -------------------------------------------------------------------------------

function settle(over: Partial<SettlementInput>): ReturnType<typeof classifyScratchSettlement> {
  return classifyScratchSettlement({ joined: true, spawned: true, code: 0, signal: null, stdout: "UZI-SCRATCH-REMOVED 1:2\n", expect: "1:2",
    rootRepinOk: true, mountsOk: true, postCensus: { state: "absent" }, bytesBefore: 8192, ...over });
}
const same = (bytes: number, complete = true) => ({ state: "same", bytes, complete }) as const;
const UNRESOLVED = { outcome: "unresolved" };

test("a receipt with the name absent or re-pointed settles removed with the revalidated bytes", () => {
  assert.deepEqual(settle({}), { outcome: "removed", bytes: 8192 });
  assert.deepEqual(settle({ stdout: "UZI-SCRATCH-REMOVED 1:2" }), { outcome: "removed", bytes: 8192 });
  assert.deepEqual(settle({ postCensus: { state: "other" } }), { outcome: "removed", bytes: 8192 });
});

test("a receipt is unresolved unless every proof beside it holds", () => {
  assert.deepEqual(settle({ postCensus: same(0) }), UNRESOLVED, "receipt but the name still pins expect");
  assert.deepEqual(settle({ postCensus: { state: "unreadable" } }), UNRESOLVED);
  assert.deepEqual(settle({ rootRepinOk: false }), UNRESOLVED);
  assert.deepEqual(settle({ mountsOk: false }), UNRESOLVED);
  assert.deepEqual(settle({ joined: false }), UNRESOLVED);
  for (const stdout of ["", "UZI-SCRATCH-REMOVED 1:3\n", "UZI-SCRATCH-REMOVED 1:2\n\n", " UZI-SCRATCH-REMOVED 1:2\n", "UZI-SCRATCH-REMOVED 1:2 extra\n", "x\nUZI-SCRATCH-REMOVED 1:2\n"])
    assert.deepEqual(settle({ stdout, postCensus: { state: "absent" } }), UNRESOLVED, JSON.stringify(stdout));
  assert.deepEqual(settle({ stdout: "", postCensus: { state: "absent" } }), UNRESOLVED, "no receipt and the name is absent");
  assert.deepEqual(settle({ stdout: "", postCensus: { state: "other" } }), UNRESOLVED, "no receipt and the name is another inode");
  assert.deepEqual(settle({ code: 4, postCensus: { state: "absent" } }), UNRESOLVED, "a receipt-shaped stdout on a non-zero exit is no receipt");
});

test("no receipt and the name still pinning expect settles residue by exit code", () => {
  const residue = (code: number | null, over: Partial<SettlementInput> = {}) =>
    settle({ code, stdout: "", postCensus: same(3000), ...over });
  assert.deepEqual(residue(2), { outcome: "residue", bytesLowerBound: 5192, kind: "changed", stopPhase: true });
  assert.deepEqual(residue(3), { outcome: "residue", bytesLowerBound: 5192, kind: "changed", stopPhase: true });
  for (const code of [4, 8, 9]) assert.deepEqual(residue(code), { outcome: "residue", bytesLowerBound: 5192, kind: "retained", stopPhase: false }, String(code));
  for (const code of [0, 1, 5, 6, 7, 10, 137]) assert.deepEqual(residue(code), { outcome: "residue", bytesLowerBound: 5192, kind: "helper_failed", stopPhase: true }, String(code));
  assert.deepEqual(residue(null, { signal: "SIGKILL" }), { outcome: "residue", bytesLowerBound: 5192, kind: "helper_failed", stopPhase: true });
  assert.deepEqual(residue(null, { spawned: false }), { outcome: "residue", bytesLowerBound: 5192, kind: "helper_failed", stopPhase: true });
  assert.deepEqual(residue(4, { postCensus: same(9000) }), { outcome: "residue", bytesLowerBound: 0, kind: "retained", stopPhase: false });
});

test("residue needs a complete post-census, an intact root and mounts", () => {
  const base = { code: 4, stdout: "" };
  assert.deepEqual(settle({ ...base, postCensus: same(100, false) }), UNRESOLVED);
  assert.deepEqual(settle({ ...base, postCensus: { state: "unreadable" } }), UNRESOLVED);
  assert.deepEqual(settle({ ...base, postCensus: same(100), rootRepinOk: false }), UNRESOLVED);
  assert.deepEqual(settle({ ...base, postCensus: same(100), mountsOk: false }), UNRESOLVED);
  assert.deepEqual(settle({ ...base, postCensus: same(100), joined: false }), UNRESOLVED);
});

test("no valid receipt with any stdout is unresolved even when the name still pins expect", () => {
  const cases: Array<[string, Partial<SettlementInput>]> = [
    ["malformed receipt", { code: 0, stdout: "UZI-SCRATCH-REMOVED garbage\n" }],
    ["receipt for another dev:ino", { code: 0, stdout: "UZI-SCRATCH-REMOVED 1:3\n" }],
    ["right receipt with a non-zero exit", { code: 4, stdout: "UZI-SCRATCH-REMOVED 1:2\n" }],
    ["extra trailing text", { code: 0, stdout: "UZI-SCRATCH-REMOVED 1:2\nextra" }],
  ];
  for (const [name, over] of cases) assert.deepEqual(settle({ ...over, postCensus: same(3000) }), UNRESOLVED, name);
  assert.deepEqual(settle({ code: 4, stdout: "", postCensus: same(3000) }),
    { outcome: "residue", bytesLowerBound: 5192, kind: "retained", stopPhase: false }, "empty stdout still settles residue");
});
