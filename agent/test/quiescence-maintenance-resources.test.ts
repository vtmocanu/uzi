import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import childProcess, { type ChildProcess } from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import fs from "node:fs";
import path from "node:path";
import http from "node:http";
import { RecoveryMaintenance, MaintenanceResources } from "../src/recovery-maintenance.js";
import { reapRunProcesses, runnerHelperTmp, procfsTableAt, teardownDocker, type ScanRequest } from "../src/run-quiescence.js";

const turn = () => new Promise<void>(resolve => setImmediate(resolve));
const request: ScanRequest = {
  mode: "capture", targetUid: 12345, targetKey: "test", targetPaths: ["/test"],
  liveMarkers: [], liveRoots: [], workerNonce: "test",
};
const verdict = JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: "" }) + "\n";
const byteCap = 256 * 1024 ** 2;
function child() {
  return Object.assign(new EventEmitter(), {
    pid: undefined, stdout: new PassThrough(), stderr: new PassThrough(), stdin: new PassThrough(),
    kill: () => true,
  });
}
async function fixture(t: test.TestContext) {
  const dir = await fs.promises.mkdtemp(path.resolve("../.uzi/scratch/quiescence-resource-"));
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "resource-fixture" });
  const result = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(result.ok);
  const slot = result.value;
  t.after(async () => { await ledger.finish(slot, "transient"); await fs.promises.rm(dir, { recursive: true, force: true }); });
  return { dir, run: <T>(action: () => T) => MaintenanceResources.run(slot, action) };
}
function mockNative(t: test.TestContext, spawn: typeof childProcess.spawn) {
  t.mock.method(childProcess, "spawn", spawn);
  syncBuiltinESMExports();
  t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
}

test("producer launch quota refuses native TMP and scanner before either actual spawn", async t => {
  const f = await fixture(t);
  let launches = 0;
  mockNative(t, (() => { launches++; throw new Error("unexpected spawn"); }) as typeof childProcess.spawn);
  await f.run(async () => {
    MaintenanceResources.consume("launches", 512);
    await assert.rejects(runnerHelperTmp("/private-base").make(), /maintenance_resource_exhausted/);
    assert.equal((await reapRunProcesses(request, { viaHelper: true,
      helperTmp: { make: () => "/private-base", remove: () => {} },
      spawnHelper: () => { launches++; throw new Error("unexpected scanner"); } })).state, "unverified");
  });
  assert.equal(launches, 0);
});

test("native mktemp, scanner and joined rm each debit the same actual launch counter", async t => {
  const f = await fixture(t);
  const commands: string[] = [];
  mockNative(t, ((command: string) => {
    commands.push(command);
    const c = child();
    setImmediate(() => {
      c.stdout.end(command === "mktemp" ? "/private-base/uzi-quiesce-controlled\n" : "");
      setImmediate(() => c.emit("close", 0, null));
    });
    return c as unknown as ChildProcess;
  }) as typeof childProcess.spawn);
  await f.run(async () => {
    MaintenanceResources.consume("launches", 509);
    const tmp = runnerHelperTmp("/private-base", (command, args) => ({ command, args }));
    const result = await reapRunProcesses(request, { viaHelper: true, helperTmp: tmp,
      spawnHelper: () => {
        const c = child();
        setImmediate(() => { c.stdout.end(verdict); setImmediate(() => c.emit("close", 0, null)); });
        return c as unknown as ChildProcess;
      } });
    assert.equal(result.state, "quiescent");
    assert.deepEqual(commands, ["mktemp", "rm"]);
    assert.throws(() => MaintenanceResources.consume("launches", 1), /maintenance_resource_exhausted/);
  });
});

for (const stream of ["stdout", "stderr"] as const) {
  test("raw " + stream + " refuses before read outside ALS and joins error through actual close", async t => {
    const f = await fixture(t), c = child();
    let settled = false, rawReads = 0, removals = 0;
    const pending = f.run(() => reapRunProcesses(request, { viaHelper: true,
      helperTmp: { make: () => "/private-base", remove: () => { removals++; } },
      spawnHelper: () => c as unknown as ChildProcess,
    })).then(result => { settled = true; return result; });
    await turn();
    // The request has already been charged. Leave three actual bytes in the shared allowance.
    f.run(() => {
      // A zero-byte source is serialized with the existing reportDeadline; count the actual write once.
      const inputBytes = c.stdin.readableLength;
      MaintenanceResources.consume("metadataBytes", byteCap - inputBytes - 3);
    });
    const read = c[stream].read.bind(c[stream]);
    t.mock.method(c[stream], "read", (n?: number) => {
      if (n && n > 0) rawReads += n;
      return read(n);
    });
    c[stream].write(Buffer.from("four")); // emitter is deliberately outside the slot context
    await turn();
    c.emit("error", new Error("controlled error"));
    await turn();
    assert.equal(rawReads, 0, "four-byte read must be refused before consumption with only three left");
    assert.equal(c[stream].readableLength, 4);
    assert.equal(settled, false);
    c.stdout.write(verdict);
    c.emit("close", 0, null);
    assert.equal((await pending).state, "unverified");
    assert.equal(removals, 0, "exhaustion must not admit fresh cleanup");
  });
}

test("exact remaining raw bytes can be read once; buffered extra data cannot revive verdict", async t => {
  const f = await fixture(t), c = child();
  let bytesRead = 0;
  const pending = f.run(() => reapRunProcesses(request, { viaHelper: true,
    helperTmp: { make: () => "/private-base", remove: () => {} },
    spawnHelper: () => c as unknown as ChildProcess,
  }));
  await turn();
  f.run(() => MaintenanceResources.consume("metadataBytes", byteCap - c.stdin.readableLength - 3));
  const read = c.stdout.read.bind(c.stdout);
  t.mock.method(c.stdout, "read", (n?: number) => {
    if (n && n > 0) bytesRead += n;
    return read(n);
  });
  c.stdout.write(Buffer.from("abc"));
  await turn();
  assert.equal(bytesRead, 3);
  c.stdout.write(Buffer.from("d"));
  await turn();
  assert.equal(bytesRead, 3);
  assert.equal(c.stdout.readableLength, 1);
  c.emit("close", 0, null);
  assert.equal((await pending).state, "unverified");
});

test("request quota is reserved before stdin delivery and cancellation still joins close", async t => {
  const f = await fixture(t), c = child();
  let settled = false;
  const pending = f.run(() => {
    MaintenanceResources.consume("metadataBytes", byteCap);
    return reapRunProcesses(request, { viaHelper: true,
      helperTmp: { make: () => "/private-base", remove: () => {} },
      spawnHelper: () => c as unknown as ChildProcess,
    });
  }).then(result => { settled = true; return result; });
  await turn();
  assert.equal(c.stdin.readableLength, 0);
  assert.equal(settled, false);
  c.stdout.write(verdict);
  c.emit("close", 0, null);
  assert.equal((await pending).state, "unverified");
});

for (const field of ["processes", "killed"] as const) {
  test("decoded helper " + field + " rows are charged before validation and sanitization", async t => {
    const f = await fixture(t), c = child();
    const pending = f.run(() => {
      MaintenanceResources.consume("adminEntries", 4096 - request.targetPaths.length);
      return reapRunProcesses(request, { viaHelper: true,
        helperTmp: { make: () => "/private-base", remove: () => {} },
        spawnHelper: () => c as unknown as ChildProcess,
      });
    });
    await turn();
    c.stdout.end(JSON.stringify({ state: "quiescent", detail: "", processes: [], killed: [],
      [field]: field === "processes" ? [{ pid: 1 }] : [1] }) + "\n");
    await turn();
    c.emit("close", 0, null);
    assert.equal((await pending).state, "unverified");
  });
}

test("native TMP raw quota refusal does not settle on error before owned close", async t => {
  const f = await fixture(t), c = child();
  mockNative(t, (() => c as unknown as ChildProcess) as typeof childProcess.spawn);
  let settled = false;
  const pending = f.run(() => {
    MaintenanceResources.consume("metadataBytes", byteCap - 3);
    return runnerHelperTmp("/private-base", (command, args) => ({ command, args })).make();
  }).then(() => { settled = true; return "resolved"; }, () => { settled = true; return "rejected"; });
  await turn();
  c.stdout.write(Buffer.from("four"));
  await turn();
  assert.equal(c.stdout.readableLength, 4);
  c.emit("error", new Error("controlled TMP error"));
  await turn();
  assert.equal(settled, false);
  c.emit("close", 0, null);
  assert.equal(await pending, "rejected");
});

test("native empty proc table charges its single EOF visit exactly once", async t => {
  const f = await fixture(t);
  const root = path.join(f.dir, "proc");
  await fs.promises.mkdir(root);
  await f.run(async () => {
    MaintenanceResources.consume("adminEntries", 4095);
    const result = await reapRunProcesses(request, { viaHelper: false, table: procfsTableAt(root) });
    assert.equal(result.state, "quiescent");
    assert.throws(() => MaintenanceResources.consume("adminEntries", 1), /maintenance_resource_exhausted/);
  });
});

test("native proc enumeration reserves an administrative visit before even EOF", async t => {
  const f = await fixture(t);
  let reads = 0;
  await f.run(async () => {
    MaintenanceResources.consume("adminEntries", 4096);
    const root = path.join(f.dir, "proc");
    await fs.promises.mkdir(root);
    const opendir = fs.opendirSync.bind(fs);
    t.mock.method(fs, "opendirSync", (...args: Parameters<typeof fs.opendirSync>) => {
      const dir = opendir(...args);
      const read = dir.readSync.bind(dir);
      dir.readSync = () => { reads++; return read(); };
      return dir;
    });
    const result = await reapRunProcesses(request, { viaHelper: false, table: procfsTableAt(root) });
    assert.equal(result.state, "unverified");
    assert.equal(reads, 0);
  });
});

test("native proc file reserves full bounded buffer before read, failure cannot hide as vanished PID", async t => {
  const f = await fixture(t);
  const root = path.join(f.dir, "proc");
  await fs.promises.mkdir(path.join(root, "17777"), { recursive: true });
  await fs.promises.writeFile(path.join(root, "17777", "status"), "Name:\tfixture\nUid:\t12345\n");
  let reads = 0;
  const read = fs.readSync.bind(fs);
  t.mock.method(fs, "readSync", ((...args: Parameters<typeof fs.readSync>) => {
    reads++;
    return read(...args);
  }) as typeof fs.readSync);
  const result = await f.run(() => {
    MaintenanceResources.consume("metadataBytes", byteCap - 64 * 1024);
    return reapRunProcesses(request, { viaHelper: false, table: procfsTableAt(root) });
  });
  assert.equal(result.state, "unverified");
  assert.equal(reads, 0);
});

for (const resource of ["adminEntries", "metadataBytes"] as const) {
  test("injected proc " + resource + " exhaustion is unverified before interpreting rows", async t => {
    const f = await fixture(t);
    let statuses = 0;
    const result = await f.run(() => {
      MaintenanceResources.consume(resource, resource === "adminEntries" ? 4096 : byteCap);
      return reapRunProcesses(request, { viaHelper: false, table: {
        listPids: () => [17777],
        readStatus: () => { statuses++; return "Name:\tfixture\nUid:\t12345\n"; },
        readStat: () => "", readEnviron: () => "", readCwd: () => "",
      } });
    });
    assert.equal(result.state, "unverified");
    assert.equal(statuses, resource === "adminEntries" ? 0 : 1);
  });
}

test("Docker request counter refuses the actual transport before dispatch", async t => {
  const f = await fixture(t);
  let requests = 0;
  t.mock.method(http, "request", () => { requests++; throw new Error("unexpected transport"); });
  const result = await f.run(() => {
    MaintenanceResources.consume("rpcAttempts", 32);
    return teardownDocker({ dockerHost: "tcp://example.invalid:2375", targetPaths: ["/test"] });
  });
  assert.equal(result.state, "docker_error");
  assert.equal(requests, 0);
});

test("Docker raw quota cancellation owns request and response until both close", async t => {
  const f = await fixture(t);
  const req = Object.assign(new EventEmitter(), { end: () => {}, destroy: () => {} });
  const res = Object.assign(new PassThrough(), { statusCode: 200 });
  t.mock.method(res, "destroy", () => res);
  let receive!: (response: http.IncomingMessage) => void;
  t.mock.method(http, "request", ((_options: unknown, cb: typeof receive) => {
    receive = cb;
    return req;
  }) as unknown as typeof http.request);
  let settled = false, reads = 0;
  const pending = f.run(() => {
    MaintenanceResources.consume("metadataBytes", byteCap - 1);
    return teardownDocker({ dockerHost: "tcp://example.invalid:2375", targetPaths: ["/test"] });
  }).then(result => { settled = true; return result; });
  receive(res as unknown as http.IncomingMessage);
  const read = res.read.bind(res);
  t.mock.method(res, "read", (n?: number) => { if (n && n > 0) reads += n; return read(n); });
  res.write(Buffer.from("[]"));
  await turn();
  assert.equal(reads, 0);
  assert.equal(settled, false);
  req.emit("error", new Error("controlled transport"));
  req.emit("close");
  await turn();
  assert.equal(settled, false);
  res.emit("close");
  assert.equal((await pending).state, "docker_error");
});

test("Docker container and mount rows consume the shared administrative quota", async t => {
  const f = await fixture(t);
  let requests = 0;
  const req = Object.assign(new EventEmitter(), { end: () => {}, destroy: () => {} });
  t.mock.method(http, "request", ((_options: unknown, receive: (r: http.IncomingMessage) => void) => {
    requests++;
    const res = Object.assign(new PassThrough(), { statusCode: 200 });
    setImmediate(() => {
      receive(res as unknown as http.IncomingMessage);
      res.end(JSON.stringify([{ Id: "fixture", Mounts: [{ Source: "/test" }] }]));
      setImmediate(() => req.emit("close"));
    });
    return req;
  }) as unknown as typeof http.request);
  const result = await f.run(() => {
    MaintenanceResources.consume("adminEntries", 4095);
    return teardownDocker({ dockerHost: "tcp://example.invalid:2375", targetPaths: ["/test"] });
  });
  assert.equal(result.state, "docker_error");
  assert.equal(requests, 1, "mount exhaustion must not dispatch deletion");
});
