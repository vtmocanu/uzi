import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import { maintenanceDeltaFixture, gitVerb, canonicalImport } from "./maintenance-delta-fixture.js";

const linux = { skip: process.platform !== "linux" };
type Fixture = Awaited<ReturnType<typeof maintenanceDeltaFixture>>;
async function selected(f: Fixture) {
  const oids = (await f.native(f.clone, ["rev-list", "--objects", "--no-object-names", f.request.tip, "--not", f.base])).split("\n");
  let size = 0;
  for (const oid of oids) size += Number(await f.native(f.clone, ["cat-file", "-s", oid]));
  return { oids, size };
}
async function rejected(f: Fixture, action = f.run, expected?: typeof RecoveryClosureLimitError) {
  const refs = await f.refs();
  if (expected) await assert.rejects(action(), expected); else await assert.rejects(action());
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(await f.refs(), refs);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
}

test("delta selected-range: missing new blob in clone refuses before import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const blob = await f.native(f.clone, ["rev-parse", f.tip + ":new"]);
  await fs.unlink(path.join(f.clone, ".git", "objects", blob.slice(0, 2), blob.slice(2)));
  await rejected(f);
  assert.equal(f.requests.some(r => gitVerb(r) === "pack-objects"), false);
  await assert.rejects(f.native(f.bare, ["cat-file", "-e", f.tip]));
});

test("delta selected-range: missing staged selected object refuses before import", linux, async t => {
  const f = await maintenanceDeltaFixture(t), selection = await selected(f);
  const missing = await f.native(f.clone, ["rev-parse", f.tip + ":new"]);
  const partial = execFileSync("/usr/bin/git", ["-C", f.clone, "pack-objects", "--stdout", "--no-reuse-object",
    "--no-reuse-delta", "--window=0", "--depth=0", "--threads=1"], {
    input: selection.oids.filter(oid => oid !== missing).join("\n") + "\n",
    env: { PATH: "/usr/bin:/bin", HOME: f.dir, GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_SYSTEM: "/dev/null", GIT_NO_REPLACE_OBJECTS: "1", GIT_NO_LAZY_FETCH: "1", GIT_ALLOW_PROTOCOL: "none" },
  });
  const stage = f.cache["stageMaintenancePack"].bind(f.cache);
  let supplied = false;
  t.mock.method(f.cache as any, "stageMaintenancePack", async (...args: Parameters<typeof stage>) => {
    const artifact = await stage(...args);
    await fs.writeFile(artifact.path, partial);
    supplied = true;
    return { ...artifact, byteSize: partial.length, objectCount: partial.readUInt32BE(8) };
  });
  await rejected(f);
  assert.equal(supplied, true);
  assert.equal(partial.readUInt32BE(8), selection.oids.length - 1);
  await assert.rejects(f.native(f.bare, ["cat-file", "-e", f.tip]));
});

test("delta exact index: same count with substituted expected OID refuses at index proof", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const other = await f.native(f.bare, ["rev-parse", f.base + ":base"]);
  const index = f.cache["indexMaintenancePack"].bind(f.cache);
  let substituted = false, proof = false;
  f.cache.beforePhase = async phase => { if (phase === "proof") proof = true; };
  t.mock.method(f.cache as any, "indexMaintenancePack", async (...args: Parameters<typeof index>) => {
    assert.equal(args[2].includes(other), false);
    const changed = [...args[2]];
    changed[changed.length - 1] = other;
    assert.equal(changed.length, args[2].length);
    substituted = true;
    args[2] = changed;
    return index(...args);
  });
  await assert.rejects(f.run(), { message: "maintenance_pack_index_refused" });
  assert.equal(substituted, true);
  assert.equal(proof, false, "exact OID failure occurs before decoded/range proof");
  assert.ok(f.requests.some(r => gitVerb(r) === "index-pack" && !canonicalImport(r)), "real strict index child ran");
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

test("delta zero selection: bare presence without eligible anchor reachability refuses", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  // Trusted fixture setup imports connected bytes without creating any ref.
  await f.native(f.bare, ["fetch", "--no-write-fetch-head", "--no-tags", f.clone, f.tip]);
  const spawn = f.cache["spawnGit"].bind(f.cache);
  let forced = false;
  t.mock.method(f.cache as any, "spawnGit", async (...args: Parameters<typeof spawn>) => {
    const child = await spawn(...args);
    if (f.cache.phase === "selection" && args[1].includes("rev-list")) {
      for await (const _ of child.stdout) { /* Drain and join the real selected range before fault injection. */ }
      assert.equal(await child.exited, 0);
      forced = true;
      return { ...child, stdout: Readable.from([]) };
    }
    return child;
  });
  await rejected(f);
  assert.equal(forced, true);
  assert.equal(f.requests.some(r => ["pack-objects", "index-pack"].includes(gitVerb(r))), false);
  assert.equal(f.pairs(), 0);
});

test("delta declared C: compressible selected body above C refuses before decoded producer", linux, async t => {
  const f = await maintenanceDeltaFixture(t, { decodedLimit: 2048 });
  await fs.writeFile(path.join(f.clone, "new"), "x".repeat(4096));
  await f.native(f.clone, ["add", "."]); await f.native(f.clone, ["commit", "-qm", "large compressible selected body"]);
  f.request.tip = await f.native(f.clone, ["rev-parse", "HEAD"]);
  await rejected(f, f.run, RecoveryClosureLimitError);
  assert.equal(f.requests.some(r => r.argv.includes("--batch")), false);
  assert.equal(f.requests.some(r => r.argv.includes("pack-objects")), false);
});

test("delta cache: canonical verified hit cannot authenticate newly staged same-size body", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  await f.native(f.bare, ["fetch", "--no-write-fetch-head", "--no-tags", f.clone, f.tip]);
  const spawn = f.cache["spawnGit"].bind(f.cache);
  let tampered = false, entriesBefore = 0, cache: { entries: number } | undefined;
  t.mock.method(f.cache as any, "spawnGit", async (...args: Parameters<typeof spawn>) => {
    const child = await spawn(...args);
    if (f.cache.phase === "proof" && args[1].includes("cat-file") && args[1].includes("--batch")) {
      const parts: Buffer[] = [];
      for await (const raw of child.stdout) parts.push(Buffer.from(raw));
      assert.equal(await child.exited, 0);
      const body = Buffer.concat(parts), first = body.indexOf(10) + 1;
      assert.ok(first > 0 && first < body.length);
      body[first] = body[first]! ^ 1;
      tampered = true;
      return { ...child, stdout: Readable.from([body]) };
    }
    return child;
  });
  await rejected(f, () => f.inScope(async () => {
    assert.equal(await f.cache.verifyRecoveryClosure(f.bare, f.tip), true);
    cache = (f.cache as any).recoveryOperations.getStore().closureCache;
    entriesBefore = cache!.entries;
    assert.ok(entriesBefore > 0, "ordinary canonical closure was positively verified");
    return f.cache.importMaintenanceTip(f.request);
  }));
  assert.equal(tampered, true, "new staged bodies must still receive decoded authentication");
  assert.equal(cache!.entries, entriesBefore, "failed staged batch publishes no entries");
});

test("delta proof range: the proof-phase selected range failing refuses before any canonical import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const spawn = f.cache["spawnGit"].bind(f.cache);
  let forced = false;
  t.mock.method(f.cache as any, "spawnGit", async (...args: Parameters<typeof spawn>) => {
    const child = await spawn(...args);
    if (f.cache.phase === "proof" && args[1].includes("rev-list") && args[1].includes("--missing=error")) {
      for await (const _ of child.stdout) { /* Drain and join the real proof range before fault injection. */ }
      assert.equal(await child.exited, 0);
      forced = true;
      return { ...child, stdout: Readable.from([]) };
    }
    return child;
  });
  await rejected(f);
  assert.equal(forced, true, "the staged-plus-alternate proof range ran and was failed");
  assert.equal(f.requests.some(r => gitVerb(r) === "index-pack" && canonicalImport(r)), false);
  await assert.rejects(f.native(f.bare, ["cat-file", "-e", f.tip]), "no selected object reached the canonical store");
});

test("delta cache: repeated proof of unchanged staged bytes shares only settled operation entries", linux, async t => {
  const probe = await maintenanceDeltaFixture(t), size = (await selected(probe)).size;
  const f = await maintenanceDeltaFixture(t, { decodedLimit: size });
  assert.equal((await selected(f)).size, size);
  const observe = f.cache["observeMaintenanceTransferPhase"].bind(f.cache);
  let repeats = 0;
  t.mock.method(f.cache as any, "observeMaintenanceTransferPhase", async (phase: Parameters<typeof observe>[0], action: () => Promise<unknown>) => {
    if (phase !== "proof") return observe(phase, action);
    return observe(phase, async () => { await action(); repeats++; return action(); });
  });
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.equal(repeats, 1);
  assert.equal(f.requests.filter(r => r.argv.includes("cat-file") && r.argv.includes("--batch")).length, 1);
  assert.equal(f.charges.filter(c => c.resource === "decoded").reduce((sum, c) => sum + c.amount, 0), size);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

test("delta cache: fresh operations rehash newly staged bytes and never inherit prior-operation trust", linux, async t => {
  const f = await maintenanceDeltaFixture(t), size = (await selected(f)).size;
  const initialEntries: number[] = [];
  f.cache.beforePhase = async phase => {
    if (phase === "walk") initialEntries.push((f.cache as any).recoveryOperations.getStore().closureCache.entries);
  };
  for (let i = 0; i < 2; i++) assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.deepEqual(initialEntries, [0, 0], "fresh operations begin without prior verified entries");
  assert.equal(f.requests.filter(r => r.argv.includes("cat-file") && r.argv.includes("--batch")).length, 2);
  assert.equal(f.charges.filter(c => c.resource === "decoded").reduce((sum, c) => sum + c.amount, 0), size * 2);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

test("delta decoded C: compressible body exceeds remaining original C after nested verified history", linux, async t => {
  const addCompressible = async (f: Fixture) => {
    await fs.writeFile(path.join(f.clone, "new"), "x".repeat(4096));
    await f.native(f.clone, ["add", "."]); await f.native(f.clone, ["commit", "-qm", "compressible decoded delta"]);
    f.request.tip = await f.native(f.clone, ["rev-parse", "HEAD"]);
  };
  const probe = await maintenanceDeltaFixture(t);
  await addCompressible(probe);
  const selection = await selected(probe);
  const baseOids = (await probe.native(probe.bare, ["rev-list", "--objects", "--no-object-names", probe.base])).split("\n");
  let baseSize = 0;
  for (const oid of baseOids) baseSize += Number(await probe.native(probe.bare, ["cat-file", "-s", oid]));
  const f = await maintenanceDeltaFixture(t, { decodedLimit: selection.size });
  await addCompressible(f);
  const actual = await selected(f);
  assert.equal(actual.size, selection.size);
  const blob = await f.native(f.clone, ["rev-parse", f.request.tip + ":new"]);
  const index = actual.oids.indexOf(blob);
  assert.ok(index >= 0);
  let beforeBody = 0, afterBody = 0;
  for (let i = 0; i < actual.oids.length; i++) {
    const size = Number(await f.native(f.clone, ["cat-file", "-s", actual.oids[i]!]));
    if (i < index) beforeBody += size;
    if (i > index) afterBody += size;
  }
  assert.ok(afterBody < baseSize, "nested work must exhaust C during this compressible object");
  const packed = execFileSync("/usr/bin/git", ["-C", f.clone, "pack-objects", "--stdout", "--no-reuse-object",
    "--no-reuse-delta", "--window=0", "--depth=0", "--threads=1"], { input: blob + "\n", stdio: ["pipe", "pipe", "pipe"],
    env: { PATH: "/usr/bin:/bin", GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
  assert.ok(packed.length < 128, "the actual Git PACK body is highly compressible");
  let nested = false, budget: { limit: number; delivered: number } | undefined;
  f.hooks.beforeSpawn = async request => {
    if (!nested && f.cache.phase === "proof" && request.argv.includes("cat-file") && request.argv.includes("--batch")) {
      nested = true;
      budget = (f.cache as any).recoveryOperations.getStore().closureBudget;
      assert.equal(budget!.delivered, 0, "both selected metadata prechecks passed before nested decoded work");
      assert.equal(await f.cache.verifyRecoveryClosure(f.bare, f.base), true);
      assert.equal(budget!.delivered, baseSize);
    }
  };
  await rejected(f, f.run, RecoveryClosureLimitError);
  assert.equal(nested, true);
  assert.ok(f.requests.filter(r => r.argv.includes("cat-file") && r.argv.includes("--batch")).length >= 2);
  assert.equal(budget!.limit, selection.size, "no renewed or raised C");
  assert.ok(budget!.delivered >= baseSize + beforeBody);
  assert.ok(budget!.delivered < baseSize + beforeBody + 4096, "C refuses inside the compressible decoded body");
  assert.ok(budget!.delivered <= budget!.limit, "consumed partial work is charged and never refunded");
});

test("delta cancellation: complete frames before clean child settlement seed no cache", linux, async t => {
  const f = await maintenanceDeltaFixture(t), refs = await f.refs();
  let enter!: () => void, release!: () => void, decodedEnd!: () => void;
  const entered = new Promise<void>(resolve => { enter = resolve; }), held = new Promise<void>(resolve => { release = resolve; });
  const decodedEOF = new Promise<void>(resolve => { decodedEnd = resolve; });
  t.after(release);
  let hold = false, settled = false, cache: { entries: number } | undefined;
  f.cache.beforePhase = async phase => {
    if (phase === "proof") cache = (f.cache as any).recoveryOperations.getStore().closureCache;
  };
  f.hooks.beforeSpawn = async request => {
    if (f.cache.phase === "proof" && request.argv.includes("cat-file") && request.argv.includes("--batch")) {
      hold = true;
      f.hooks.holdStderr = true;
    }
  };
  f.hooks.stderrDestroy = (error, callback) => { void held.then(() => callback(error)); };
  f.hooks.completed = async () => { if (hold) enter(); };
  const spawn = f.cache["spawnGit"].bind(f.cache);
  t.mock.method(f.cache as any, "spawnGit", async (...args: Parameters<typeof spawn>) => {
    const child = await spawn(...args);
    if (f.cache.phase === "proof" && args[1].includes("cat-file") && args[1].includes("--batch")) {
      child.stdout.once("end", decodedEnd);
    }
    return child;
  });
  const outcome = f.run().then(value => ({ value }), error => ({ error })).finally(() => { settled = true; });
  try {
    await Promise.race([Promise.all([decodedEOF, entered]), outcome.then(() => { throw new Error("missed decoded child settlement latch"); })]);
    // spawnGit gated stdout EOF permits verifyClosureFrames to finish; raw stderr still holds child.exited.
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.ok(cache);
    assert.equal(cache.entries, 0);
    assert.equal(f.hooks.stderr!.destroyed, false, "raw stderr remains open before cancellation");
    assert.equal(f.hooks.stderr!.closed, false);
    f.controller.abort();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(settled, false, "refusal joins the held actual child");
    assert.equal(f.hooks.stderr!.destroyed, true, "cancellation requests raw stderr disposal");
    assert.equal(f.hooks.stderr!.closed, false, "raw asynchronous stderr disposal remains held");
    assert.equal(cache.entries, 0, "no publication before clean settlement");
  } finally {
    f.controller.abort();
    release();
    await outcome;
  }
  const result = await outcome;
  assert.ok("error" in result && result.error instanceof Error);
  assert.equal(f.hooks.stderr!.closed, true);
  assert.equal(await f.refs(), refs);
  assert.equal(cache!.entries, 0);
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

test("delta post-import: missing selected range leaves imported and unknown objects without authority", linux, async t => {
  const f = await maintenanceDeltaFixture(t), refs = await f.refs();
  const packDir = path.join(f.bare, "objects", "pack"), marker = path.join(packDir, "unknown-residue");
  let importing = false, injected = false, packPath = "", packBytes: Buffer | undefined;
  f.hooks.beforeSpawn = async request => { if (canonicalImport(request)) importing = true; };
  f.hooks.completed = async () => {
    if (!importing || injected) return;
    injected = true;
    const names = await fs.readdir(packDir);
    const indices = names.filter(name => name.endsWith(".idx"));
    assert.equal(indices.length, 1, "fixture imports one new canonical pack");
    packPath = path.join(packDir, indices[0]!.replace(/\.idx$/, ".pack"));
    packBytes = await fs.readFile(packPath);
    await fs.unlink(path.join(packDir, indices[0]!));
    await fs.writeFile(marker, "unknown object bytes must never be rolled back");
  };
  await assert.rejects(f.run());
  assert.equal(injected, true);
  assert.equal(f.requests.filter(canonicalImport).length, 1);
  assert.deepEqual(await fs.readFile(packPath), packBytes, "imported pack retained after failed range check");
  assert.equal(await fs.readFile(marker, "utf8"), "unknown object bytes must never be rolled back");
  assert.equal(await f.refs(), refs);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

test("delta documented residual4b: missing excluded blob passes range walk while full closure fails", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const blob = await f.native(f.bare, ["rev-parse", f.base + ":base"]);
  await fs.unlink(path.join(f.bare, "objects", blob.slice(0, 2), blob.slice(2)));
  const rows = await f.native(f.clone, ["rev-list", "--objects", "--missing=error", "--no-object-names", f.tip, "--not", f.base]);
  assert.ok(rows.split("\n").includes(f.tip));
  assert.equal(rows.split("\n").includes(blob), false);
  await assert.rejects(f.native(f.clone, ["rev-list", "--objects", "--missing=error", f.tip]), { code: 128 });
  // This intentionally broken invariant is residual evidence, never importer
  // success/full-closure evidence. C4 owns the paired archive backstop test.
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await f.retained();
});
