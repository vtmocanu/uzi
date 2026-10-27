import assert from "node:assert/strict";
import * as fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { createRuntime } from "../templates/pod-epoch-init.cjs";
import { inspectPodEpochBootstrap } from "../src/pod-epoch-bootstrap.js";

const S = "00000000-0000-0000-0000-000000000001";
const T = "00000000-0000-0000-0000-000000000002";
const environment = {
  UZI_POD_UID: S,
  UZI_POD_EPOCH_RUNTIME: "containerd-runc",
  UZI_POD_EPOCH_EVIDENCE_FILE: "/etc/uzi/pod-epoch/evidence",
};
type Options = NonNullable<Parameters<typeof createRuntime>[0]>;

function fixture(t: { after(fn: () => void): void }) {
  fs.mkdirSync(new URL("../../.uzi/scratch/", import.meta.url), { recursive: true });
  const root = fs.mkdtempSync(fileURLToPath(new URL("../../.uzi/scratch/bootstrap.", import.meta.url)));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const calls: string[][] = [];
  const options: Options = {
    root, uid: process.getuid!(), gid: process.getgid!(),
    io: { ...fs, statfsSync: () => ({ type: 0xef53 }) } as unknown as typeof fs,
    run: async (args) => {
      calls.push(args);
      return { code: 0, output: args[0] === "--version" ? "flock from util-linux 2.41\n" : "" };
    },
  };
  return { root, file: path.join(root, ".uzi-epoch-consumed"), calls, options,
    runtime: createRuntime(options) };
}

test("bootstrap reads only its three fields and defaults to SH without a peek", () => {
  let peeks = 0;
  const port = { inspectConsumption: () => { peeks++; return "fresh" as const; } };
  for (const [values, reason] of [
    [{ UZI_POD_EPOCH_RUNTIME: undefined }, "runtime_not_declared"],
    [{ UZI_POD_EPOCH_RUNTIME: "" }, "runtime_not_declared"],
    [{ UZI_POD_EPOCH_RUNTIME: "unknown" }, "runtime_not_supported"],
    [{ UZI_POD_EPOCH_EVIDENCE_FILE: undefined }, "no_projection"],
    [{ UZI_POD_EPOCH_EVIDENCE_FILE: "/tmp/arbitrary" }, "unsupported_projection"],
    [{ UZI_POD_EPOCH_EVIDENCE_FILE: "/run/uzi/pod-epoch/evidence" }, "unsupported_projection"],
  ] as const) {
    const env = new Proxy({ ...environment, ...values }, {
      get(target, key, receiver) {
        assert.ok(["UZI_POD_UID", "UZI_POD_EPOCH_RUNTIME", "UZI_POD_EPOCH_EVIDENCE_FILE"].includes(String(key)));
        return Reflect.get(target, key, receiver);
      },
    });
    const snapshot = inspectPodEpochBootstrap(env, port);
    assert.equal(snapshot.firstMode, "shared");
    assert.equal(snapshot.reason, reason);
    if (reason === "unsupported_projection") assert.equal(snapshot.evidenceFile, undefined);
    assert.ok(Object.isFrozen(snapshot));
  }
  assert.equal(peeks, 0);
  assert.throws(() => inspectPodEpochBootstrap({ ...environment, UZI_POD_UID: "INVALID" }, port), /invalid pod UUID/);
  assert.equal(peeks, 0);
});

test("fresh bootstrap chooses EX before acquire; durable consume alone enables evaluation", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  f.calls.length = 0;
  const before = fs.statSync(f.file);
  const opens: number[] = [];
  let bytes = 0;
  let closed = 0;
  const readonly = createRuntime({ ...f.options, io: {
    ...f.options.io,
    openSync: (file: fs.PathLike, flags: number) => {
      assert.equal(file, f.file);
      opens.push(flags);
      return fs.openSync(file, flags);
    },
    readSync: (fd: number, buffer: Buffer, offset: number, length: number, position: number) => {
      bytes += length;
      return fs.readSync(fd, buffer, offset, length, position);
    },
    closeSync: (fd: number) => { closed++; fs.closeSync(fd); },
    writeSync: () => { throw new Error("unexpected write"); },
    fsyncSync: () => { throw new Error("unexpected fsync"); },
  } as unknown as typeof fs });
  for (const runtime of ["containerd-runc", "crio-crun"]) {
    assert.equal(inspectPodEpochBootstrap({ ...environment, UZI_POD_EPOCH_RUNTIME: runtime }, readonly).firstMode, "exclusive");
  }
  assert.equal(f.calls.length, 0);
  assert.equal(closed, 2);
  assert.ok(opens.every((flags) => flags === (fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK)));
  // Two snapshots each request MAX_BYTES+1 plus a final bounded EOF probe.
  assert.ok(bytes <= 2 * (2 * 151561));
  assert.equal(fs.statSync(f.file).ino, before.ino);
  assert.equal(fs.statSync(f.file).size, before.size);
  assert.equal(fs.readFileSync(f.file, "utf8"), "UZIEC01\n");
  const snapshot = inspectPodEpochBootstrap(environment, f.runtime);
  assert.equal(snapshot.evidenceFile, "/etc/uzi/pod-epoch/evidence");
  assert.equal(snapshot.firstMode, "exclusive");
  const held = await f.runtime.acquire(snapshot);
  try {
    assert.equal(held.mode, "exclusive");
    assert.deepEqual(await held.consume(S), { result: "fresh", mayEvaluate: true });
    assert.deepEqual(await held.consume(S), { result: "existing", mayEvaluate: false });
  } finally { held.close(); }
});

test("S consumes, T consumes, then S restarts with late evidence and selects SH", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const inode = fs.statSync(f.file).ino;
  for (const uid of [S, T]) {
    const held = await f.runtime.acquire();
    try { assert.deepEqual(await held.consume(uid), { result: "fresh", mayEvaluate: false }); }
    finally { held.close(); }
  }
  f.calls.length = 0;
  const snapshot = inspectPodEpochBootstrap(environment, f.runtime);
  assert.equal(snapshot.firstMode, "shared");
  assert.equal(snapshot.reason, "already_consumed");
  assert.equal(f.calls.length, 0);
  const held = await f.runtime.acquire(snapshot);
  try { assert.deepEqual(await held.consume(S), { result: "existing", mayEvaluate: false }); }
  finally { held.close(); }
  assert.equal(fs.statSync(f.file).ino, inode);
  assert.equal(fs.readFileSync(f.file, "utf8"), "UZIEC01\n" + S + "\n" + T + "\n");
});

test("4096 records select SH and consume full without append or inode replacement", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const records = Array.from({ length: 4096 }, (_, i) =>
    "10000000-0000-0000-0000-" + i.toString(16).padStart(12, "0"));
  fs.writeFileSync(f.file, "UZIEC01\n" + records.join("\n") + "\n");
  const before = fs.statSync(f.file);
  assert.equal(before.size, 151560);
  f.calls.length = 0;
  const snapshot = inspectPodEpochBootstrap(environment, f.runtime);
  assert.equal(snapshot.reason, "consumption_full");
  assert.equal(snapshot.firstMode, "shared");
  assert.equal(f.calls.length, 0);
  assert.equal(f.runtime.inspectConsumption(records[0]!), "existing");
  const held = await f.runtime.acquire(snapshot);
  try { assert.deepEqual(await held.consume(S), { result: "full", mayEvaluate: false }); }
  finally { held.close(); }
  assert.equal(fs.statSync(f.file).ino, before.ino);
  assert.equal(fs.statSync(f.file).size, before.size);
});

for (const defect of ["missing", "empty", "torn", "header", "duplicate", "symlink", "hardlink", "mode", "uid", "gid", "oversize"]) {
  test("unsafe ledger is uncertain then fatal under ownership: " + defect, async (t) => {
    const f = fixture(t);
    await f.runtime.seed(S);
    let options = f.options;
    switch (defect) {
      case "missing": fs.unlinkSync(f.file); break;
      case "empty": fs.writeFileSync(f.file, ""); break;
      case "torn": fs.writeFileSync(f.file, "UZIEC01\n" + S); break;
      case "header": fs.writeFileSync(f.file, "INVALID\n"); break;
      case "duplicate": fs.writeFileSync(f.file, "UZIEC01\n" + S + "\n" + S + "\n"); break;
      case "symlink": fs.renameSync(f.file, f.file + ".old"); fs.symlinkSync(f.file + ".old", f.file); break;
      case "hardlink": fs.linkSync(f.file, f.file + ".alias"); break;
      case "mode": fs.chmodSync(f.file, 0o644); break;
      case "uid":
      case "gid": {
        // Model foreign ledger ownership without requiring privileged chown.
        options = { ...options, io: { ...options.io,
          fstatSync: (fd: number, opts: { bigint: true }) => {
            const stat = fs.fstatSync(fd, opts);
            if (stat.ino === fs.statSync(f.file, { bigint: true }).ino) {
              Object.defineProperty(stat, defect, { value: BigInt((defect === "uid" ? process.getuid!() : process.getgid!()) + 1) });
            }
            return stat;
          },
        } as unknown as typeof fs };
        break;
      }
      case "oversize": fs.writeFileSync(f.file, Buffer.alloc(151561, 65)); break;
    }
    const runtime = createRuntime(options);
    f.calls.length = 0;
    const snapshot = inspectPodEpochBootstrap(environment, runtime);
    assert.equal(snapshot.firstMode, "shared");
    assert.equal(snapshot.reason, "consumption_uncertain");
    assert.equal(f.calls.length, 0);
    assert.throws(() => runtime.inspectConsumption("bad"), /invalid pod UUID/);
    const held = await runtime.acquire(snapshot);
    try { await assert.rejects(held.consume(S)); }
    finally { held.close(); }
  });
}

for (const race of ["size", "inode"]) {
  test("read race returns uncertain and closes descriptor: " + race, async (t) => {
    const f = fixture(t);
    await f.runtime.seed(S);
    let changed = false;
    let closed = 0;
    let requested = 0;
    const runtime = createRuntime({ ...f.options, io: {
      ...f.options.io,
      readSync: (fd: number, buffer: Buffer, offset: number, length: number, position: number) => {
        requested += length;
        const n = fs.readSync(fd, buffer, offset, length, position);
        if (!changed) {
          changed = true;
          if (race === "size") fs.appendFileSync(f.file, T + "\n");
          else {
            fs.renameSync(f.file, f.file + ".old");
            fs.writeFileSync(f.file, "UZIEC01\n", { mode: 0o600 });
          }
        }
        return n;
      },
      closeSync: (fd: number) => { closed++; fs.closeSync(fd); },
    } as unknown as typeof fs });
    f.calls.length = 0;
    assert.equal(runtime.inspectConsumption(S), "uncertain");
    assert.equal(closed, 1);
    assert.ok(requested <= 3 * 151561);
    assert.equal(f.calls.length, 0);
  });
}

test("unsupported storage remains native gated before epoch open and grants no evaluation", async (t) => {
  const f = fixture(t);
  await f.runtime.seed(S);
  const events: string[] = [];
  const runtime = createRuntime({ ...f.options, io: {
    ...f.options.io,
    statfsSync: () => { events.push("statfs"); return { type: 0x6969 }; },
    openSync: (file: fs.PathLike, flags: number) => {
      assert.notEqual(file, path.join(f.root, ".uzi-epoch.lock"));
      events.push("ledger");
      return fs.openSync(file, flags);
    },
  } as unknown as typeof fs });
  const snapshot = inspectPodEpochBootstrap(environment, runtime);
  assert.equal(snapshot.firstMode, "exclusive");
  events.length = 0;
  const held = await runtime.acquire(snapshot);
  try {
    assert.deepEqual(events, ["statfs"]);
    assert.equal(held.mode, "unsupported");
    assert.deepEqual(await held.consume(S), { result: "fresh", mayEvaluate: false });
  } finally { held.close(); }
});
