import test, { describe } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { spawn } from "node:child_process";
import net from "node:net";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { nullLogger } from "./helpers.js";

const MiB = 1024 ** 2;
type Limits = { sourceBytes?: number; sourceEntries?: number; metadataBytes?: number };
async function fixture(t: test.TestContext) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/wip-input-"));
  const root = path.join(dir, "source");
  await fs.mkdir(root);
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok, JSON.stringify(admitted));
  t.after(async () => {
    await ledger.finish(admitted.value, "transient");
    await fs.rm(dir, { recursive: true, force: true });
  });
  const git = new GitCache(dir, nullLogger());
  const run = <T>(action: () => T) => MaintenanceResources.run(admitted.value, action);
  let launches = 0, joined = 0;
  const metrics = path.join(dir, "metrics.json");
  const owner = async (request: BoundaryProcessRequest, hook = "") => {
    launches++;
    const argv = [...request.argv];
    assert.equal(argv[1], "-e");
    // Test-owned trusted prefix only; the production helper body and argv stay intact.
    argv[2] = `
// Initialize runtime stdio before counting helper-owned filesystem operations.
void process.stdin; void process.stdout; void process.stderr;
const meterFs = require("node:fs");
const metrics = { reads: 0, readBytes: 0, allocated: 0, stats: 0, opens: 0, rows: 0, frames: 0 };
for (const [key, metric] of [["readSync", "reads"], ["lstatSync", "stats"], ["fstatSync", "stats"], ["openSync", "opens"]]) {
  const original = meterFs[key];
  meterFs[key] = function(...args) { metrics[metric]++; const result = original.apply(this, args); if (key === "readSync") metrics.readBytes += result; return result; };
}
const originalAlloc = Buffer.alloc;
Buffer.alloc = function(...args) { metrics.allocated += args[0]; return originalAlloc.apply(this, args); };
const originalDir = meterFs.opendirSync;
meterFs.opendirSync = function(...args) {
  const stream = originalDir.apply(this, args), read = stream.readSync;
  stream.readSync = function(...args) { metrics.rows++; return read.apply(this, args); };
  return stream;
};
const originalJSON = JSON.stringify;
JSON.stringify = function(...args) { metrics.frames++; return originalJSON.apply(this, args); };
process.on("exit", () => meterFs.writeFileSync(${JSON.stringify(metrics)}, originalJSON(metrics)));
` + hook + argv[2];
    const child = spawn(argv[0]!, argv.slice(1), {
      cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"],
    });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => { joined++; resolve({ code: code ?? 1 }); });
    });
    return {
      stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGTERM"); await completed; },
    };
  };
  const proof = (limits: Limits = {}, hook = "", source = root) => run(() =>
    git.withBoundaryProcessSpawner(request => owner(request, hook), new AbortController().signal,
      () => git["maintenanceWipInput"](source, limits)));
  return { root, dir, git, run, proof, owner, launches: () => launches, joined: () => joined,
    metrics: async () => JSON.parse(await fs.readFile(metrics, "utf8")) as
      { reads: number; readBytes: number; allocated: number; stats: number; opens: number; rows: number; frames: number } };
}

describe("raw maintenance WIP input", { skip: process.platform !== "linux" && "Linux /proc capture required" }, () => {

test("raw proof includes unchanged, ignored and binary bytes; root metadata is count-only", async t => {
  const f = await fixture(t);
  await fs.mkdir(path.join(f.root, ".git"));
  await fs.writeFile(path.join(f.root, ".git", "not-source"), "private");
  await fs.writeFile(path.join(f.root, ".gitignore"), "ignored\n");
  await fs.writeFile(path.join(f.root, "unchanged"), "tracked");
  await fs.writeFile(path.join(f.root, "ignored"), "ignored");
  await fs.writeFile(path.join(f.root, "binary"), Buffer.from([0, 255, 0]));
  const proof = () => f.proof({ sourceBytes: MiB, sourceEntries: 512, metadataBytes: MiB });
  const first = await proof();
  assert.deepEqual(await proof(), first);
  await fs.writeFile(path.join(f.root, ".git", "not-source"), "different");
  assert.deepEqual(await proof(), first);
  // Raw reads include every source byte, including ignored/binary content, but no root .git bytes.
  const rawBytes = 8 + 7 + 7 + 3;
  assert.equal((await f.metrics()).readBytes, rawBytes);
  let previous = first;
  for (const [leaf, replacement] of [
    ["unchanged", Buffer.from("changed")],
    ["ignored", Buffer.from("IGNORED")],
    ["binary", Buffer.from([0, 254, 0])],
  ] as const) {
    await fs.writeFile(path.join(f.root, leaf), replacement);
    const next = await proof();
    assert.notEqual(next.digest, previous.digest, `${leaf} same-size content must change digest`);
    assert.equal(next.entries, previous.entries);
    assert.equal(next.bytes, previous.bytes, `${leaf} same-size content preserves byte allowance`);
    assert.equal((await f.metrics()).readBytes, rawBytes);
    previous = next;
  }
  await fs.appendFile(path.join(f.root, "binary"), "x");
  const grown = await proof();
  assert.notEqual(grown.digest, previous.digest);
  assert.equal(grown.entries, previous.entries);
  assert.equal(grown.bytes, previous.bytes + 1);
  assert.equal((await f.metrics()).readBytes, rawBytes + 1);
  await fs.writeFile(path.join(f.root, "new"), "");
  assert.ok((await proof()).entries > grown.entries);
  assert.equal(f.launches(), f.joined());
});

test("traverse-only ancestors do not require directory read authority", async t => {
  const f = await fixture(t);
  await fs.writeFile(path.join(f.root, "file"), "content");
  await fs.chmod(f.dir, 0o300);
  try {
    if (process.getuid?.() !== 0) await assert.rejects(fs.opendir(f.dir), { code: "EACCES" });
    const proof = await f.proof();
    assert.match(proof.digest, /^[a-f0-9]{64}$/);
    assert.equal(f.launches(), f.joined());
  } finally { await fs.chmod(f.dir, 0o700); }
});

test("unrelated sibling namespace changes preserve source evidence", async t => {
  const f = await fixture(t), file = path.join(f.root, "file");
  await fs.writeFile(file, "content");
  const expected = await f.proof();
  const hook = `
const siblingFs = require("node:fs"), siblingRead = siblingFs.readSync;
let created = false;
siblingFs.readSync = function(...args) {
  const result = siblingRead.apply(this, args);
  if (!created) { created = true; siblingFs.mkdirSync(${JSON.stringify(path.join(f.dir, "unrelated"))}); }
  return result;
};
`;
  assert.deepEqual(await f.proof({}, hook), expected);
  assert.equal(f.launches(), f.joined());
});

test("raw symlink target is hashed without opening its target", async t => {
  const f = await fixture(t), target = path.join(f.dir, "outside");
  await fs.writeFile(target, "outside");
  await fs.symlink(target, path.join(f.root, "link"));
  const first = await f.proof();
  assert.equal((await f.metrics()).reads, 0);
  await fs.writeFile(target, "changed outside");
  assert.deepEqual(await f.proof(), first);
  await fs.unlink(path.join(f.root, "link"));
  await fs.symlink(target + "-missing", path.join(f.root, "link"));
  assert.notEqual((await f.proof()).digest, first.digest);
});

for (const [resource, cap, limits] of [
  ["sourceBytes", 256 * MiB, { sourceBytes: 1 }],
  ["sourceEntries", 65536, { sourceEntries: 1 }],
  ["metadataBytes", 256 * MiB, { metadataBytes: 1 }],
  ["adminEntries", 4096, {}],
] as const) test(`${resource} exhaustion refuses before dispatch`, async t => {
  const f = await fixture(t);
  f.run(() => MaintenanceResources.consume(resource, cap));
  await assert.rejects(f.proof(limits), /maintenance_/);
  assert.equal(f.launches(), 0);
});

test("limits are positive safe integers capped at the private defaults", async t => {
  const f = await fixture(t);
  for (const [key, cap] of [["sourceBytes", 64 * MiB], ["sourceEntries", 16384], ["metadataBytes", 32 * MiB]] as const) {
    for (const value of [0, -1, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, cap + 1])
      await assert.rejects(f.proof({ [key]: value }), /maintenance_wip_input_refused/);
  }
  assert.equal(f.launches(), 0);
  await f.proof(); // Inclusive defaults.
});

test("repeated proofs share full precharges without summary refunds", async t => {
  const f = await fixture(t);
  for (let i = 0; i < 4; i++) await f.proof();
  await assert.rejects(f.proof(), /maintenance_/);
  assert.equal(f.launches(), 4);
  assert.equal(f.joined(), 4);
});

test("metadata precharges are shared and never refunded after tiny summaries", async t => {
  const f = await fixture(t);
  f.run(() => MaintenanceResources.consume("metadataBytes", 254 * MiB - 65536));
  const limits = { sourceBytes: 255, sourceEntries: 5, metadataBytes: MiB };
  await f.proof(limits);
  await f.proof(limits);
  await assert.rejects(f.proof(limits), /maintenance_/);
  assert.equal(f.launches(), 2);
});

test("root verification enforces the precharged 256 administrative visits at actual IO", async t => {
  const f = await fixture(t);
  const components = f.root.slice(1).split("/").length;
  assert.ok(components < 60);
  const deep = path.join(f.root, ...Array<string>(60 - components).fill("d"));
  await fs.mkdir(deep, { recursive: true });
  await assert.rejects(f.proof({}, "", deep), /maintenance_/);
  const metrics = await f.metrics();
  // Empty-source verification performs three fstats outside the admin allowance.
  // Root revalidation must then stop at the 256th administrative syscall.
  assert.equal(metrics.opens + metrics.stats, 256 + 3);
  assert.equal(metrics.rows, 1);
  assert.equal(metrics.frames, 0);
});

test("discovery, unscoped and missing owner refuse with zero launches", async t => {
  const f = await fixture(t);
  await assert.rejects(f.git.withBoundaryProcessSpawner(f.owner, new AbortController().signal,
    () => f.git["maintenanceWipInput"](f.root)), /maintenance_/);
  await assert.rejects(f.run(() => f.git["maintenanceWipInput"](f.root)), /maintenance_/);
  await assert.rejects(MaintenanceResources.runDiscovery(() =>
    f.git.withBoundaryProcessSpawner(f.owner, new AbortController().signal,
      () => f.git["maintenanceWipInput"](f.root))), /maintenance_/);
  assert.equal(f.launches(), 0);
});

test("child byte quota refuses before raw directory read and file buffer allocation", async t => {
  const f = await fixture(t);
  await fs.writeFile(path.join(f.root, "file"), "payload");
  await assert.rejects(f.proof({ sourceBytes: 254 }), /maintenance_/);
  assert.equal((await f.metrics()).rows, 0);
  assert.equal((await f.metrics()).reads, 0);
  assert.equal((await f.metrics()).allocated, 0);
  await assert.rejects(f.proof({ sourceBytes: 256 }), /maintenance_/);
  assert.equal((await f.metrics()).reads, 0);
  assert.equal((await f.metrics()).allocated, 0);
});

test("empty tree exact byte and cumulative visit limits; no renewed revalidation allowance", async t => {
  const f = await fixture(t);
  const exact = await f.proof({ sourceBytes: 255, sourceEntries: 5 });
  assert.equal(exact.bytes, 255);
  assert.equal(exact.entries, 5);
  await assert.rejects(f.proof({ sourceBytes: 255, sourceEntries: 4 }), /maintenance_/);
  await fs.mkdir(path.join(f.root, "dir"));
  await fs.writeFile(path.join(f.root, "dir", "file"), "x");
  const complete = await f.proof();
  const expected = await f.metrics();
  await assert.rejects(f.proof({ sourceEntries: complete.entries - 1 }), /maintenance_/);
  const refused = await f.metrics();
  assert.equal(refused.stats, expected.stats - 1 - f.root.slice(1).split("/").length,
    "refuse the final source stat before syscall, then abort without ancestor revalidation");
});

test("child metadata exhaustion precedes root IO and JSON frame allocation", async t => {
  const f = await fixture(t);
  await assert.rejects(f.proof({ metadataBytes: 32767 }), /maintenance_/);
  assert.equal((await f.metrics()).opens, 0);
  assert.equal((await f.metrics()).stats, 0);
  await fs.writeFile(path.join(f.root, "file"), "x");
  // A tiny envelope cannot reach source allocation, even with abundant source quota.
  await assert.rejects(f.proof({ metadataBytes: 32768 }), /maintenance_/);
  assert.equal((await f.metrics()).frames, 0);
  assert.equal((await f.metrics()).allocated, 0);
});

for (const kind of ["nested metadata", "bare layout", "root bare layout", "invalid UTF8", "parent link", "special file"] as const)
  test(`refuses ${kind}`, async t => {
    const f = await fixture(t);
    let source = f.root;
    if (kind === "nested metadata") {
      await fs.mkdir(path.join(f.root, "nested", ".git"), { recursive: true });
    } else if (kind === "bare layout" || kind === "root bare layout") {
      const bare = kind === "bare layout" ? path.join(f.root, "nested") : f.root;
      await fs.mkdir(bare, { recursive: true });
      for (const name of ["HEAD", "objects", "refs"]) await fs.writeFile(path.join(bare, name), "");
    } else if (kind === "invalid UTF8") {
      await fs.writeFile(Buffer.concat([Buffer.from(f.root + "/"), Buffer.from([255])]), "");
    } else if (kind === "parent link") {
      const link = path.join(f.dir, "linked");
      await fs.symlink(f.root, link);
      source = link;
    } else {
      const server = net.createServer();
      await new Promise<void>((resolve, reject) => {
        server.once("error", reject); server.listen(path.relative(process.cwd(), path.join(f.root, "socket")), resolve);
      });
      t.after(() => new Promise<void>((resolve, reject) => server.close(e => e ? reject(e) : resolve())));
    }
    await assert.rejects(f.proof({}, "", source), /maintenance_/);
    assert.equal(f.launches(), f.joined());
  });

for (const namespace of [false, true]) test(`refuses ${namespace ? "namespace" : "file"} mutation inside trusted helper`, async t => {
  const f = await fixture(t), file = path.join(f.root, "file");
  await fs.writeFile(file, "original");
  const hook = `
const mutationFs = require("node:fs"), originalRead = mutationFs.readSync;
let mutated = false;
mutationFs.readSync = function(...args) {
  const result = originalRead.apply(this, args);
  if (!mutated) {
    mutated = true;
    ${namespace ? `mutationFs.renameSync(${JSON.stringify(file)}, ${JSON.stringify(file + "-moved")});` : ""}
    mutationFs.writeFileSync(${JSON.stringify(file)}, "changed");
  }
  return result;
};
`;
  await assert.rejects(f.proof({}, hook), /maintenance_/);
  assert.equal(f.joined(), 1);
});

});
