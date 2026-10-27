import assert from "node:assert/strict";
import * as fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { test } from "node:test";
import { createPodEpoch, restoreEpochRuntimeTmpdir } from "../src/pod-epoch-lock.js";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
const S = "00000000-0000-0000-0000-000000000001";
const T = "00000000-0000-0000-0000-000000000002";
const ledgerName = ".uzi-epoch-consumed";
const epochName = ".uzi-epoch.lock";
const recordName = ".uzi-pod-entry";
const version = { code: 0, output: "flock from util-linux 2.41\n" };
type Options = NonNullable<Parameters<typeof createPodEpoch>[0]>;

function fixture(t: { after(fn: () => void): void }, overrides: Options = {}) {
  const root = fs.mkdtempSync(path.join(scratch, "pod-epoch-test."));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const calls: { args: string[]; fd: number | undefined }[] = [];
  const io = {
    ...fs,
    statfsSync: () => ({ type: 0xef53 }),
  } as unknown as typeof fs;
  const options: Options = {
    root, uid: process.getuid!(), gid: process.getgid!(), io,
    run: async (args, fd) => {
      calls.push({ args, fd });
      return args[0] === "--version" ? version : { code: 0, output: "" };
    },
    ...overrides,
  };
  return { root, calls, options, runtime: createPodEpoch(options) };
}

test("seed is durable and idempotent; S T S cannot rearm the one-shot", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const inode = fs.statSync(path.join(f.root, ledgerName)).ino;
  const recordInode = fs.statSync(path.join(f.root, recordName)).ino;
  await f.runtime.seed(S);
  assert.equal(fs.statSync(path.join(f.root, recordName)).ino, recordInode);
  const s = await f.runtime.acquire();
  assert.equal(s.mode, "shared");
  assert.deepEqual(await s.consume(S), { result: "fresh", mayEvaluate: false });
  s.close();
  await f.runtime.seed(T);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(f.root, recordName), "utf8")), { current: T, previous: S });
  const other = await f.runtime.acquire();
  await other.consume(T);
  other.close();
  const restart = await f.runtime.acquire();
  assert.deepEqual(await restart.consume(S), { result: "existing", mayEvaluate: false });
  restart.close();
  assert.equal(fs.statSync(path.join(f.root, ledgerName)).ino, inode);
  assert.equal(fs.readFileSync(path.join(f.root, ledgerName), "utf8"), "UZIEC01\n" + S + "\n" + T + "\n");
  assert(f.calls.filter((call) => call.fd !== undefined).every((call) => call.args.join(" ").endsWith("-n -E 200 3")));
});

for (const supported of [true, false]) {
  test(`seed sets modes before handing ownership away without FOWNER (supported=${supported})`, async (t) => {
    const f = fixture(t);
    const caller = process.getuid!();
    const worker = caller + 1;
    // Model CHOWN and DAC_OVERRIDE, but no FOWNER: descriptors retain access
    // after chown, while chmod still requires the inode's current owner.
    const owners = new Map<bigint, number>();
    const handoffs: number[] = [];
    const statOwner = (stat: fs.Stats | fs.BigIntStats) => {
      const owner = owners.get(BigInt(stat.ino));
      if (owner !== undefined) {
        Object.defineProperty(stat, "uid", {
          value: typeof stat.uid === "bigint" ? BigInt(owner) : owner,
        });
        Object.defineProperty(stat, "gid", {
          value: typeof stat.gid === "bigint" ? BigInt(process.getgid!()) : process.getgid!(),
        });
      }
      return stat;
    };
    const runtime = createPodEpoch({ ...f.options, uid: worker, io: {
      ...f.options.io,
      statfsSync: () => ({ type: supported ? 0xef53 : 0x6969 }),
      fchownSync: (fd: number, uid: number, gid: number) => {
        assert.equal(gid, process.getgid!());
        const stat = fs.fstatSync(fd, { bigint: true });
        handoffs.push(Number(stat.mode & 0o7777n));
        owners.set(stat.ino, uid);
      },
      fchmodSync: (fd: number, mode: fs.Mode) => {
        const stat = fs.fstatSync(fd, { bigint: true });
        if ((owners.get(stat.ino) ?? caller) !== caller) {
          throw Object.assign(new Error("chmod requires inode ownership without FOWNER"), { code: "EPERM" });
        }
        fs.fchmodSync(fd, mode);
      },
      fstatSync: (...args: Parameters<typeof fs.fstatSync>) => statOwner(fs.fstatSync(...args)),
      lstatSync: (...args: Parameters<typeof fs.lstatSync>) => {
        const stat = fs.lstatSync(...args);
        assert.ok(stat);
        return statOwner(stat);
      },
    } as unknown as typeof fs });
    await runtime.seed(S);
    const ledgerInode = fs.statSync(path.join(f.root, ledgerName)).ino;
    const epochInode = supported ? fs.statSync(path.join(f.root, epochName)).ino : undefined;
    await runtime.seed(T);
    assert.deepEqual(handoffs, supported ? [0o600, 0o600, 0o644, 0o644] : [0o600, 0o644, 0o644]);
    assert.equal(fs.statSync(path.join(f.root, ledgerName)).ino, ledgerInode);
    if (supported) assert.equal(fs.statSync(path.join(f.root, epochName)).ino, epochInode);
    assert.deepEqual(JSON.parse(fs.readFileSync(path.join(f.root, recordName), "utf8")), { current: T, previous: S });
    const held = await runtime.acquire();
    try {
      assert.deepEqual(await held.consume(S), { result: "fresh", mayEvaluate: false });
    } finally { held.close(); }
    assert.equal(fs.readFileSync(path.join(f.root, ledgerName), "utf8"), "UZIEC01\n" + S + "\n");
  });
}

test("unsupported storage never opens epoch file; metadata mutex grants no authority", async (t) => {
  const f = fixture(t);
  f.options.io = { ...f.options.io, statfsSync: () => ({ type: 0x6969 }) } as unknown as typeof fs;
  const runtime = createPodEpoch(f.options);
  await runtime.seed(S);
  assert.equal(fs.existsSync(path.join(f.root, epochName)), false);
  const ownership = await runtime.acquire({ firstMode: "exclusive" });
  assert.equal(ownership.mode, "unsupported");
  assert.deepEqual(await ownership.consume(S), { result: "fresh", mayEvaluate: false });
  ownership.close();
  assert(f.calls.filter((call) => call.fd !== undefined).every((call) => call.args[0] === "-x"));
  await runtime.hold("unsupported");
});

test("missing state, wrong mode, hardlink and symlink fail closed without repairs", async (t) => {
  const f = fixture(t);
  await assert.rejects(f.runtime.acquire(), /ENOENT/);
  await f.runtime.seed(S);
  const file = path.join(f.root, epochName);
  fs.chmodSync(file, 0o644);
  await assert.rejects(f.runtime.acquire(), /unsafe/);
  assert.equal(fs.statSync(file).mode & 0o777, 0o644);
  fs.chmodSync(file, 0o600);
  fs.linkSync(file, path.join(f.root, "alias"));
  await assert.rejects(f.runtime.acquire(), /unsafe/);
  fs.unlinkSync(path.join(f.root, "alias"));
  fs.renameSync(file, path.join(f.root, "old"));
  fs.symlinkSync("old", file);
  await assert.rejects(f.runtime.acquire(), /ELOOP/);
});

test("path replacement during acquisition and shell pre-open race are rejected", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const identity = f.runtime.prepare();
  const epoch = path.join(f.root, epochName);
  fs.renameSync(epoch, epoch + ".old");
  fs.writeFileSync(epoch, "", { mode: 0o600 });
  fs.chownSync(epoch, process.getuid!(), process.getgid!());
  const fd = fs.openSync(epoch, "r+");
  try { await assert.rejects(f.runtime.hold(identity, fd), /inode changed/); }
  finally { fs.closeSync(fd); }
  const runtime = createPodEpoch({
    ...f.options,
    run: async (args) => {
      if (args[0] === "--version") return version;
      fs.renameSync(epoch, epoch + ".swapped");
      fs.writeFileSync(epoch, "", { mode: 0o600 });
      fs.chownSync(epoch, process.getuid!(), process.getgid!());
      return { code: 0, output: "" };
    },
  });
  await assert.rejects(runtime.acquire(), /inode changed/);
});

test("device and inode comparison preserves identities above the Number safe range", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const file = path.join(f.root, epochName);
  const actual = fs.statSync(file, { bigint: true });
  const heldInode = 9007199254740992n;
  let namedInode = heldInode;
  function inode(stat: fs.Stats | fs.BigIntStats, value: bigint) {
    Object.defineProperty(stat, "ino", { value: typeof stat.ino === "bigint" ? value : Number(value) });
    return stat;
  }
  const runtime = createPodEpoch({ ...f.options, io: {
    ...f.options.io,
    fstatSync: (...args: Parameters<typeof fs.fstatSync>) => {
      const stat = fs.fstatSync(...args);
      return BigInt(stat.ino) === actual.ino ? inode(stat, heldInode) : stat;
    },
    lstatSync: (...args: Parameters<typeof fs.lstatSync>) => {
      const stat = fs.lstatSync(...args);
      assert.ok(stat);
      return args[0] === file ? inode(stat, namedInode) : stat;
    },
  } as typeof fs });
  const same = await runtime.acquire();
  same.close();
  namedInode++;
  assert.equal(Number(heldInode), Number(namedInode), "Number conversion would hide this replacement");
  await assert.rejects(runtime.acquire(), /inode changed/);
});

test("only contention retries; epoch and ledger have distinct finite bounds", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  let clock = 0;
  let attempts = 0;
  let conflict = true;
  const runtime = createPodEpoch({
    ...f.options,
    now: () => clock,
    sleep: async (ms) => { clock += ms; },
    run: async (args) => {
      if (args[0] === "--version") return version;
      attempts++;
      return { code: conflict ? 200 : 0, output: "" };
    },
  });
  await assert.rejects(runtime.acquire(), /timeout/);
  assert.equal(clock, 120000);
  assert.equal(attempts, 2400);
  conflict = false;
  const held = await runtime.acquire();
  conflict = true;
  const start = clock;
  await assert.rejects(held.consume(S), /timeout/);
  assert.equal(clock - start, 5000);
  held.close();
  const broken = createPodEpoch({ ...f.options, run: async (args) =>
    args[0] === "--version" ? version : { code: 1, output: "" } });
  await assert.rejects(broken.acquire(), /flock failed: 1/);
});

test("future EX is explicit; busy EX falls back to SH; reacquisition closes old fd", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  let busy = true;
  const modes: string[] = [];
  const runtime = createPodEpoch({ ...f.options, run: async (args) => {
    if (args[0] === "--version") return version;
    modes.push(args[0]!);
    return { code: args[0] === "-x" && busy ? 200 : 0, output: "" };
  } });
  const shared = await runtime.acquire({ firstMode: "exclusive" });
  assert.equal(shared.mode, "shared");
  shared.close();
  assert.deepEqual(modes, ["-x", "-s"]);
  busy = false;
  const exclusive = await runtime.acquire({ firstMode: "exclusive" });
  assert.deepEqual(await exclusive.consume(S), { result: "fresh", mayEvaluate: true });
  await exclusive.reacquireShared();
  assert.equal(exclusive.mode, "shared");
  exclusive.close();
  assert.throws(() => exclusive.mode, /closed/);
});

test("full ledger and existing UID fsync without append; exact bound is 151560", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const records = Array.from({ length: 4096 }, (_, i) =>
    "00000000-0000-0000-0000-" + i.toString(16).padStart(12, "0"));
  const contents = "UZIEC01\n" + records.join("\n") + "\n";
  assert.equal(Buffer.byteLength(contents), 151560);
  fs.writeFileSync(path.join(f.root, ledgerName), contents);
  let synced = 0;
  const runtime = createPodEpoch({ ...f.options, io: {
    ...f.options.io,
    fsyncSync: () => { synced++; },
  } as typeof fs });
  const ownership = await runtime.acquire();
  assert.deepEqual(await ownership.consume(S), { result: "existing", mayEvaluate: false });
  assert.deepEqual(await ownership.consume("ffffffff-ffff-ffff-ffff-ffffffffffff"), { result: "full", mayEvaluate: false });
  assert.equal(synced, 2);
  assert.equal(fs.readFileSync(path.join(f.root, ledgerName), "utf8"), contents);
  ownership.close();
});

test("torn, duplicate, uppercase, oversize and invalid header reject without repair", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  for (const contents of ["", "UZIEC01\n" + S, "UZIEC01\n" + S + "\n" + S + "\n",
    "UZIEC01\nFFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF\n", "x".repeat(151561)]) {
    fs.writeFileSync(path.join(f.root, ledgerName), contents);
    const held = await f.runtime.acquire();
    await assert.rejects(held.consume(S));
    held.close();
    assert.equal(fs.readFileSync(path.join(f.root, ledgerName), "utf8"), contents);
  }
});

test("short append and uncertain fsync are fatal; complete unfsynced append settles on retry", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const short = createPodEpoch({ ...f.options, io: {
    ...f.options.io, writeSync: () => 1,
  } as unknown as typeof fs });
  const h = await short.acquire();
  await assert.rejects(h.consume(S), /short consumption/);
  h.close();
  const failed = createPodEpoch({ ...f.options, io: {
    ...f.options.io, fsyncSync: () => { throw new Error("EIO"); },
  } as typeof fs });
  const a = await failed.acquire();
  await assert.rejects(a.consume(S), /EIO/);
  a.close();
  const b = await f.runtime.acquire();
  assert.deepEqual(await b.consume(S), { result: "existing", mayEvaluate: false });
  b.close();
});

test("unsafe pod entry is never replaced and unique temporary is removed on fsync failure", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const record = path.join(f.root, recordName);
  fs.chmodSync(record, 0o600);
  await assert.rejects(f.runtime.seed(T), /unsafe/);
  assert.equal(JSON.parse(fs.readFileSync(record, "utf8")).current, S);
  fs.chmodSync(record, 0o644);
  const runtime = createPodEpoch({ ...f.options, io: {
    ...f.options.io,
    fsyncSync: (fd: number) => {
      if ((fs.fstatSync(fd).mode & 0o777) === 0o644) throw new Error("record EIO");
      fs.fsyncSync(fd);
    },
  } as typeof fs });
  await assert.rejects(runtime.seed(T), /record EIO/);
  assert.equal(JSON.parse(fs.readFileSync(record, "utf8")).current, S);
  assert.deepEqual(fs.readdirSync(f.root).sort(), [ledgerName, epochName, recordName].sort());
});

test("abort ends polling and pending consumption cannot lose its descriptor", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const controller = new AbortController();
  const runtime = createPodEpoch({ ...f.options, sleep: async () => { controller.abort(); },
    run: async (args) => args[0] === "--version" ? version : { code: 200, output: "" } });
  await assert.rejects(runtime.acquire({ signal: controller.signal }), /abort/i);
  let release!: () => void;
  const blocked = createPodEpoch({ ...f.options, run: async (args) => {
    if (args[0] === "--version") return version;
    if (args[0] === "-x") await new Promise<void>((resolve) => { release = resolve; });
    return { code: 0, output: "" };
  } });
  const h = await blocked.acquire();
  const consumption = h.consume(S);
  assert.throws(() => h.close(), /pending/);
  await new Promise<void>((resolve) => setImmediate(resolve));
  release();
  await consumption;
  h.close();
});

test("aborting a real in-flight helper joins child close before rejecting", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const controller = new AbortController();
  let joined = false;
  let started = false;
  const runtime = createPodEpoch({ ...f.options, run: undefined,
    spawnProcess: (() => {
      // Literal inert program, never executes a command payload under test.
      const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], {
        stdio: ["ignore", "pipe", "pipe"],
      });
      child.once("spawn", () => { started = true; controller.abort(new Error("stop helper")); });
      child.once("close", () => { joined = true; });
      return child;
    }) as unknown as typeof spawn,
  });
  await assert.rejects(runtime.acquire({ signal: controller.signal }), /stop helper/);
  assert.equal(started, true);
  assert.equal(joined, true);
});

test("TMPDIR restore validates fixed lane and private worker metadata before mutating env", async () => {
  const io = { lstatSync: () => ({
    isDirectory: () => true, isSymbolicLink: () => false, uid: 10001, gid: 10001, mode: 0o40700,
  }) } as unknown as typeof fs;
  const environment = { UZI_UID_SPLIT: "1", TMPDIR: "/tmp/uzi-epoch-bootstrap", UZI_EPOCH_RUNTIME_TMPDIR: "/data/runner/.tmp/uzi-worker" };
  const ownership = { mode: "shared" } as Awaited<ReturnType<ReturnType<typeof createPodEpoch>["acquire"]>>;
  restoreEpochRuntimeTmpdir(ownership, environment, io);
  assert.equal(environment.TMPDIR, "/data/runner/.tmp/uzi-worker");
  assert.equal(environment.UZI_EPOCH_RUNTIME_TMPDIR, undefined);
  assert.throws(() => restoreEpochRuntimeTmpdir(ownership, { UZI_EPOCH_RUNTIME_TMPDIR: "/data/runner" }, io));
  const unsafe = { ...io, lstatSync: () => ({ isDirectory: () => false }) } as unknown as typeof fs;
  assert.throws(() => restoreEpochRuntimeTmpdir(ownership, {
    UZI_UID_SPLIT: "1", UZI_EPOCH_RUNTIME_TMPDIR: "/data/runner/.tmp/uzi-worker",
  }, unsafe), /unsafe/);
});

const hostVersion = spawnSync("/usr/bin/flock", ["--version"], { encoding: "utf8" });
const utilLinux = hostVersion.status === 0 && hostVersion.stdout.startsWith("flock from util-linux ");
test("real host utility must identify as util-linux", async (t) => {
  if (utilLinux) { assert.match(hostVersion.stdout, /^flock from util-linux /); return; }
  const f = fixture(t);
  await f.runtime.seed(S);
  const runtime = createPodEpoch({ ...f.options, run: undefined });
  await assert.rejects(runtime.acquire(), /util-linux|flock/);
});

test("two-process kernel fd lifetime (BusyBox adapter is not the util-linux image proof)", { skip: process.platform !== "linux" }, async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  // Only the adapter substitutes BusyBox's flags/exit code. The worker's trusted
  // utility check is tested separately above and correctly rejects this host.
  const runtime = createPodEpoch({ ...f.options, run: utilLinux ? undefined : async (args, fd) => {
    if (args[0] === "--version") return version;
    const result = spawnSync("/usr/bin/flock", [args[0]!, "-n", "3"], {
      stdio: ["ignore", "ignore", "ignore", fd!],
    });
    if (result.error) throw result.error;
    if (result.status !== 0 && result.status !== 1) throw new Error("BusyBox fixture failed");
    return { code: result.status === 1 ? 200 : 0, output: "" };
  } });
  const held = await runtime.acquire();
  const contender = async () => {
    const child = spawn(process.execPath, ["-e", `
      const fs = require('node:fs');
      const { spawnSync } = require('node:child_process');
      const fd = fs.openSync(process.argv[1], fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
      const result = spawnSync('/usr/bin/flock', ${JSON.stringify(utilLinux ? ['-x','-n','-E','200','3'] : ['-x','-n','3'])}, { stdio: ['ignore','ignore','ignore',fd] });
      fs.closeSync(fd);
      process.exit(result.status ?? 2);
    `, path.join(f.root, epochName)], { stdio: "ignore" });
    const [code] = await once(child, "close");
    return code;
  };
  assert.equal(await contender(), utilLinux ? 200 : 1);
  assert.equal(await runtime.seed(T), "busy");
  const overlapping = await runtime.acquire();
  overlapping.close();
  held.close();
  assert.equal(await contender(), 0);
});

test("record corruption defers authority without repairing history or skipping SH", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const record = path.join(f.root, recordName);
  fs.writeFileSync(record, '{"current":');
  assert.equal(await f.runtime.seed(T), "invalid");
  assert.equal(fs.readFileSync(record, "utf8"), '{"current":');
  const shared = await f.runtime.acquire();
  assert.equal(shared.mode, "shared");
  assert.deepEqual(await shared.consume(T), { result: "fresh", mayEvaluate: false });
  shared.close();
});

test("entry tripwire catches replacement followed by a successful successor init", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  await f.runtime.seed(T);
  const exclusive = await f.runtime.acquire({ firstMode: "exclusive" });
  assert.equal(exclusive.matchesEntry(T, S), true);
  fs.renameSync(path.join(f.root, epochName), path.join(f.root, "old-lock"));
  // Simulate the out-of-band replacement; production seed never replaces it.
  fs.writeFileSync(path.join(f.root, epochName), "", { mode: 0o600 });
  fs.chownSync(path.join(f.root, epochName), process.getuid!(), process.getgid!());
  const U = "00000000-0000-0000-0000-000000000003";
  assert.equal(await f.runtime.seed(U), "updated");
  assert.equal(exclusive.matchesEntry(T, S), false);
  exclusive.close();
});

test("single-uid runtime scratch is restored and bootstrap npm overrides do not reach run children", () => {
  const io = { lstatSync: () => ({
    isDirectory: () => true, isSymbolicLink: () => false,
    uid: 10001, gid: 10001, mode: 0o43775,
  }) } as unknown as typeof fs;
  const environment: NodeJS.ProcessEnv = {
    TMPDIR: "/tmp/uzi-epoch-bootstrap", UZI_EPOCH_RUNTIME_TMPDIR: "/data/runner/.tmp",
    npm_config_cache: "/tmp/uzi-epoch-bootstrap", npm_config_ignore_scripts: "true", TSX_DISABLE_CACHE: "1",
  };
  const ownership = { mode: "shared" } as Awaited<ReturnType<ReturnType<typeof createPodEpoch>["acquire"]>>;
  restoreEpochRuntimeTmpdir(ownership, environment, io);
  assert.equal(environment.TMPDIR, "/data/runner/.tmp");
  assert.equal(environment.npm_config_cache, undefined);
  assert.equal(environment.npm_config_ignore_scripts, undefined);
  assert.equal(environment.TSX_DISABLE_CACHE, undefined);
});
