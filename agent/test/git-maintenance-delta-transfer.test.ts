import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { constants as fsConstants } from "node:fs";
import path from "node:path";
import { setImmediate as immediate } from "node:timers/promises";
import { maintenanceDeltaFixture, gitVerb, canonicalImport } from "./maintenance-delta-fixture.js";

const linux = { skip: process.platform !== "linux" };
type Fixture = Awaited<ReturnType<typeof maintenanceDeltaFixture>>;

async function refused(f: Fixture, message = "maintenance_source_layout_unsupported") {
  const refs = await f.refs();
  await assert.rejects(f.run(), { message });
  assert.equal(f.requests.filter(canonicalImport).length, 0, "no canonical import dispatched");
  assert.equal(await f.refs(), refs, "no ref write on refusal");
  assert.equal(f.joined(), f.requests.length, "all launched children settled before refusal");
  await f.retained();
}
// Real descriptors and entries; the latch observes terminal ordering without a timer.
for (const { stageFailure, walkFailure } of [
  { stageFailure: false, walkFailure: false },
  { stageFailure: true, walkFailure: false },
  { stageFailure: false, walkFailure: true },
]) {
  test(walkFailure ? "delta close ordering: early walk failure joins held source pin before return" :
    "delta close ordering: held source pin joins before cleanup (stage failure=" + stageFailure + ")", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const open = fs.open.bind(fs), unlink = fs.unlink.bind(fs), rmdir = fs.rmdir.bind(fs);
    let release!: () => void, reached!: () => void;
    const held = new Promise<void>(resolve => { release = resolve; });
    const closing = new Promise<void>(resolve => { reached = resolve; });
    let sourceObjectsFd: number | undefined, injected = false, closeCalls = 0, closed = false;
    let heldCloseObserved = false;
    let terminalPhase = false, settled = false;
    const removed: string[] = [], premature: string[] = [];
    const pinCloses: number[] = [];
    let joinedPins = 0;
    f.cache.afterPhase = async phase => { if (phase === "import") terminalPhase = true; };
    f.cache.beforePhase = async phase => {
      if (walkFailure && phase === "walk") { injected = true; throw new Error("fixture_walk_failed"); }
    };
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      if (args[0] === path.join(f.request.outDir, "proof", "objects", "pack", "stage.pack") && stageFailure) {
        t.mock.method(file, "write", async () => {
          injected = true; throw new Error("fixture_stage_write_failed");
        });
      }
      if (typeof args[1] === "number" && (args[1] & fsConstants.O_DIRECTORY) !== 0) {
        const i = pinCloses.length;
        pinCloses.push(0);
        const close = file.close.bind(file);
        t.mock.method(file, "close", async () => {
          pinCloses[i] = pinCloses[i]! + 1;
          await close(); joinedPins++;
        });
      }
      // The source .git pin precedes its objects pin, both opened through fd paths.
      if (typeof args[0] === "string" && /^\/proc\/self\/fd\/[0-9]+\/objects$/.test(args[0]) &&
          sourceObjectsFd === undefined) {
        sourceObjectsFd = file.fd;
        const close = file.close.bind(file);
        t.mock.method(file, "close", () => {
          closeCalls++; reached();
          const completion = (async () => { await held; await close(); closed = true; })();
          // A transparent promise proxy exposes consumption of the held completion.
          // Unlike an elapsed-turn check, this catches dropping the close promise
          // even when later asynchronous cleanup has not yet reached an unlink.
          return new Proxy(completion, {
            get(target, property) {
              if (property === "then") {
                heldCloseObserved = true;
                return target.then.bind(target);
              }
              return Reflect.get(target, property, target);
            },
          });
        });
      }
      return file;
    });
    t.mock.method(fs, "unlink", async (...args: Parameters<typeof fs.unlink>) => {
      if (typeof args[0] === "string" && args[0].startsWith(f.request.outDir + "/")) {
        if (!closed) premature.push(args[0]);
        assert.equal(closed, true, "source pin close joined before owned unlink");
        removed.push(args[0]);
      }
      return unlink(...args);
    });
    t.mock.method(fs, "rmdir", async (...args: Parameters<typeof fs.rmdir>) => {
      if (typeof args[0] === "string" && args[0].startsWith(f.request.outDir)) {
        if (!closed) premature.push(args[0]);
        assert.equal(closed, true, "source pin close joined before owned rmdir");
      }
      return rmdir(...args);
    });
    const outcome = f.run().then(value => ({ value }), error => ({ error })).finally(() => { settled = true; });
    t.after(async () => { release(); await outcome; });
    await closing;
    await immediate();
    assert.equal(heldCloseObserved, true, "owner consumes the held pin-close completion");
    assert.equal(settled, false, "terminal result waits for held close");
    assert.equal(removed.length, 0, "no owned entry unlinked while close held");
    assert.deepEqual(premature, [], "no cleanup attempted before pin closes join");
    if (walkFailure) await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    else assert.ok((await fs.lstat(path.join(f.request.outDir, "read", "HEAD"))).isFile());
    if (stageFailure) assert.ok((await fs.lstat(path.join(f.request.outDir, "proof", "objects", "pack", "stage.pack"))).isFile());
    if (walkFailure) {
      assert.equal(injected, true, "failure occurs after source pins but before private views");
      assert.equal(f.requests.length, 0);
    } else if (stageFailure) {
      assert.equal(injected, true, "actual stage write failure injected");
      assert.equal(f.requests.filter(canonicalImport).length, 0);
    } else {
      assert.equal(terminalPhase, true, "proof and import finished before pin release");
      assert.equal(f.requests.filter(canonicalImport).length, 1);
    }
    release();
    const result = await outcome;
    assert.equal(closeCalls, 1, "callback and outer finally join one close attempt");
    assert.equal(closed, true);
    assert.equal(joinedPins, pinCloses.length, "every pin close joined");
    assert.ok(pinCloses.every(count => count === 1), "each pin closed once");
    assert.deepEqual(premature, []);
    if (walkFailure) assert.equal(removed.length, 0, "early failure created no owned scratch");
    else assert.ok(removed.length > 0, "cleanup follows joined closes");
    assert.equal(new Set(removed).size, removed.length, "each entry unlinked only once");
    if (walkFailure) {
      assert.ok("error" in result);
      assert.match(String(result.error), /fixture_walk_failed/);
    } else if (stageFailure) {
      assert.ok("error" in result);
      assert.match(String(result.error), /maintenance_pack_staging_refused/);
    } else {
      assert.deepEqual(result, { value: { kind: "imported", sourceSha: f.tip } });
    }
    assert.equal(f.joined(), f.requests.length);
    await f.retained();
  });
}

for (const failure of ["source pin", "stage output", "index output", "index input"]) {
  test("delta close uncertainty: retain owned output after failed " + failure + " close", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const open = fs.open.bind(fs), unlink = fs.unlink.bind(fs);
    const packDir = path.join(f.request.outDir, "proof", "objects", "pack");
    const target = path.join(packDir, failure === "index output" ? "stage.idx" : "stage.pack");
    let injected = false, closeCalls = 0, release: (() => Promise<void>) | undefined;
    const removed: string[] = [];
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const file = await open(...args);
      const pin = failure === "source pin" && typeof args[0] === "string" &&
        /^\/proc\/self\/fd\/[0-9]+\/objects$/.test(args[0]);
      const helper = failure !== "source pin" && args[0] === target &&
        (failure !== "index input" || (f.requests.some(r => gitVerb(r) === "pack-objects") &&
          typeof args[1] === "number" && (args[1] & fsConstants.O_RDWR) === 0));
      if (!injected && (pin || helper)) {
        injected = true;
        const close = file.close.bind(file);
        release = close;
        t.mock.method(file, "close", async () => {
          closeCalls++; throw new Error("fixture_close_uncertain");
        });
      }
      return file;
    });
    t.mock.method(fs, "unlink", async (...args: Parameters<typeof fs.unlink>) => {
      if (typeof args[0] === "string" && args[0].startsWith(f.request.outDir + "/")) removed.push(args[0]);
      return unlink(...args);
    });
    try {
      await assert.rejects(f.run(), { message: failure === "source pin" ? "fixture_close_uncertain" :
        failure === "stage output" ? "maintenance_pack_staging_refused" : "maintenance_pack_index_refused" });
      assert.equal(injected, true, "real handle close failure injected");
      assert.equal(closeCalls, 1, "failed close is never retried");
      if (failure === "source pin") {
        assert.deepEqual(removed, [], "failed global pin close prevents all owned unlink");
        assert.ok((await fs.lstat(path.join(f.request.outDir, "read", "HEAD"))).isFile());
      } else {
        assert.deepEqual(removed, [], "uncertain active I/O prevents all owned unlink");
        assert.equal((removed as string[]).includes(target), false, "uncertain helper output retained");
        assert.ok((await fs.lstat(target)).isFile());
        assert.equal(f.requests.filter(canonicalImport).length, 0);
      }
      assert.equal(f.joined(), f.requests.length);
    } finally { await release?.(); }
  });
}

for (const bound of [4096, 4097]) {
  test("delta derived path: real absolute leaf bytes " + bound + " with component 255", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const name = "tmp_" + "x".repeat(251);
    const cloneLength = bound - Buffer.byteLength("/.git/objects/" + name);
    const basename = path.basename(f.clone);
    let parent = path.join(f.dir, "runner");
    const parentLength = cloneLength - Buffer.byteLength("/" + basename);
    while (Buffer.byteLength(parent) < parentLength) {
      const remaining = parentLength - Buffer.byteLength(parent);
      // Leave room for a final nonempty component.
      const size = remaining > 256 ? Math.min(255, remaining - 3) : remaining - 1;
      parent = path.join(parent, "d".repeat(size));
      await fs.mkdir(parent, { mode: 0o700 });
    }
    const clone = path.join(parent, basename);
    assert.equal(Buffer.byteLength(clone), cloneLength);
    await fs.rename(f.clone, clone);
    f.source.clonePath = clone;
    const objects = await fs.open(path.join(clone, ".git", "objects"), "r");
    const leaf = path.join(clone, ".git", "objects", name);
    assert.equal(Buffer.byteLength(leaf), bound);
    assert.equal(Buffer.byteLength(name), 255);
    const descriptorLeaf = "/proc/self/fd/" + objects.fd + "/" + name;
    await fs.writeFile(descriptorLeaf, "fixture");
    let walked = false, lookedUp = false;
    f.cache.afterPhase = async phase => {
      if (phase === "walk") { walked = true; throw new Error("fixture_walk_complete"); }
    };
    const lstat = fs.lstat.bind(fs);
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
      if (typeof args[0] === "string" && args[0].endsWith("/" + name)) lookedUp = true;
      return lstat(...args);
    });
    try {
      await assert.rejects(f.run(), { message: bound === 4096 ? "fixture_walk_complete" : "maintenance_source_layout_unsupported" });
    } finally {
      await fs.unlink(descriptorLeaf);
      await objects.close();
    }
    assert.equal(walked, bound === 4096);
    assert.equal(lookedUp, bound === 4096, "overlong absolute leaf refused before fd-relative lookup");
    assert.equal(f.requests.length, 0, "walk sentinel stops before later family qualification");
  });
}

async function clearAnchors(f: Fixture) {
  for (const ref of ["refs/remotes/origin/main", "refs/heads/main", "refs/uzi-runner/feature"])
    await f.native(f.bare, ["update-ref", "-d", ref]);
}


for (const exhausted of [false, true]) {
  test("delta alternate reads: short requests reserve capacity through EOF (exhausted=" + exhausted + ")", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const info = path.join(f.clone, ".git", "objects", "info"), alternate = path.join(info, "alternates");
    assert.deepEqual(await fs.readdir(info), ["alternates"]);
    const length = (await fs.readFile(alternate)).length, capacities: number[] = [];
    const reservation = (length + 1) * (length + 2) / 2;
    let rawReads = 0;
    const opendir = fs.opendir.bind(fs);
    t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
      const dir = await opendir(...args);
      if (f.physical(args[0]) === info) {
        const read = dir.read.bind(dir);
        t.mock.method(dir, "read", async () => {
          rawReads++;
          assert.equal(f.charges.at(-1)?.resource, "sourceBytes");
          assert.equal(f.charges.at(-1)?.amount, 255, "raw-name request reservation");
          return read();
        });
      }
      return dir;
    });
    f.hooks.open = (file, p) => {
      if (p !== alternate) return;
      if (exhausted) {
        const used = f.charges.filter(c => c.resource === "sourceBytes").reduce((n, c) => n + c.amount, 0);
        f.charge("sourceBytes", 256 * 1024 ** 2 - used - (reservation - 1));
      }
      const read = file.read.bind(file);
      t.mock.method(file, "read", async (buffer: Buffer, offset: number, capacity: number, position: number) => {
        capacities.push(capacity);
        assert.equal(f.charges.at(-1)?.resource, "sourceBytes");
        assert.equal(f.charges.at(-1)?.amount, capacity, "charge requested capacity, including a short read");
        return read(buffer, offset, Math.min(1, capacity), position);
      });
    };
    f.cache.afterPhase = async phase => { if (phase === "walk") throw new Error("fixture_walk_complete"); };
    await assert.rejects(f.run(), exhausted ? /maintenance_resource_exhausted|maintenance_token_invalid/ :
      { message: "fixture_walk_complete" });
    if (exhausted) {
      assert.equal(capacities.at(-1), 2, "EOF request denied before an unreserved read");
      assert.equal(capacities.length, length);
    } else {
      assert.deepEqual(capacities, Array.from({ length: length + 1 }, (_, i) => length + 1 - i));
      assert.deepEqual(capacities.slice(-4), [4, 3, 2, 1]);
      assert.equal(capacities.reduce((a, b) => a + b, 0), reservation);
      assert.equal(rawReads, 2, "one raw entry and EOF reserve 2 * 255");
    }
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}

test("delta: connected tip imports only selected objects with canonical prerequisites and no refs", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const refs = await f.refs();
  assert.equal(await f.native(f.clone, ["rev-list", "--objects", "--no-object-names", f.tip, "--not", f.base])
    .then(s => s.split("\n").length), 3, "fixture has exactly commit, tree and blob in delta");
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.equal(await f.native(f.bare, ["rev-parse", f.tip + "^{tree}"]),
    await f.native(f.clone, ["rev-parse", f.tip + "^{tree}"]), "tip is connected in canonical bare");
  assert.equal(await f.native(f.bare, ["show", f.tip + ":base"]), "canonical prerequisite");
  assert.equal(await f.native(f.bare, ["show", f.tip + ":new"]), "new selected blob");
  assert.equal(await f.refs(), refs, "import writes no refs");
  assert.equal(f.pairs(), 1, "reads, metadata and proofs add zero remote pairs");
  const pair = f.trace.indexOf("pair");
  assert.equal(f.trace[pair + 1], "git:index-pack", "no Git metadata between fresh pair and import");
  assert.equal(f.requests.filter(canonicalImport).length, 1);
  const proof = f.requests.find(r => gitVerb(r) === "index-pack" && !canonicalImport(r))!;
  assert.ok(proof, "private staged index proof executed");
  assert.equal(JSON.parse(proof.env.GIT_ALTERNATE_OBJECT_DIRECTORIES!), path.join(f.bare, "objects"),
    "staged-only proof borrows canonical prerequisites");
  const proofRepo = path.join(f.request.outDir, "proof"), proofObjects = path.join(proofRepo, "objects");
  assert.equal(proof.env.GIT_DIR, proofRepo);
  assert.equal(proof.env.GIT_OBJECT_DIRECTORY, proofObjects);
  const decoded = f.requests.filter(r => r.argv.includes("cat-file") && r.argv.includes("--batch"));
  const range = f.requests.filter(r => r.argv.includes("rev-list") && r.env.GIT_DIR === proofRepo);
  assert.equal(decoded.length, 1); assert.equal(range.length, 1);
  for (const request of [...decoded, ...range]) {
    assert.equal(request.env.GIT_OBJECT_DIRECTORY, proofObjects, "proof cannot read clone objects");
    assert.equal(request.env.GIT_ALTERNATE_OBJECT_DIRECTORIES, JSON.stringify(path.join(f.bare, "objects")),
      "only the pinned canonical prerequisites are alternate objects");
  }
  assert.equal(f.joined(), f.requests.length, "all supervised children joined");
  for (const dimension of ["decoded", "closureEntries", "sourceBytes", "sourceEntries", "artifactBytes",
    "metadataBytes", "adminEntries", "sources", "launches", "rpcAttempts"]) {
    assert.ok(f.charges.some(c => c.resource === dimension && c.amount > 0), "charged " + dimension);
  }
  await f.retained();
});

test("delta: clean anchor-reachable tip returns already_present with zero pack/index/import/ref effects", linux, async t => {
  const f = await maintenanceDeltaFixture(t, { clean: true });
  const refs = await f.refs();
  assert.deepEqual(await f.run(), { kind: "already_present", sourceSha: f.base });
  assert.equal(f.pairs(), 0);
  assert.equal(f.requests.some(r => ["pack-objects", "index-pack"].includes(gitVerb(r))), false);
  assert.equal(await f.refs(), refs);
  assert.equal(f.requests.filter(r => r.argv.includes("--is-shallow-repository")).length, 2);
  assert.equal(f.joined(), f.requests.length);
  assert.ok(f.charges.some(c => c.resource === "sourceEntries"));
  await f.retained();
});

test("delta R8: fresh pair refusal immediately before import prevents actual import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  let observed = 0;
  f.hooks.beforePair = async () => { observed++; throw new Error("fixture_custody_lost"); };
  await refused(f, "fixture_custody_lost");
  assert.equal(observed, 1, "proofs complete before the single attempted pair");
  assert.ok(f.requests.some(r => gitVerb(r) === "cat-file" && r.argv.includes("--batch")));
  assert.equal(f.pairs(), 0);
  await assert.rejects(f.native(f.bare, ["cat-file", "-e", f.tip]), "tip remains absent");
});

for (const only of ["refs/remotes/bundle/main", "refs/heads/other", "refs/uzi-runner/other",
  "refs/remotes/origin/main/descendant"]) {
  test("delta boundary: nonlisted-only " + only + " refuses exact boundary_missing", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    await clearAnchors(f);
    await f.native(f.bare, ["update-ref", only, f.base]);
    await refused(f, "maintenance_transfer_boundary_missing");
    assert.equal(f.pairs(), 0);
  });
}
for (const only of ["refs/remotes/origin/main", "refs/heads/main", "refs/uzi-runner/feature"]) {
  test("delta boundary: eligible family " + only + " anchors and excludes its base closure", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    await clearAnchors(f); await f.native(f.bare, ["update-ref", only, f.base]);
    const refs = await f.refs();
    assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
    assert.equal(await f.refs(), refs);
    const packs = (await fs.readdir(path.join(f.bare, "objects", "pack"))).filter(p => p.endsWith(".idx"));
    assert.equal(packs.length, 1);
    const rows = await f.native(f.bare, ["verify-pack", "-v", path.join(f.bare, "objects", "pack", packs[0]!)]);
    assert.equal(rows.split("\n").filter(row => /^[a-f0-9]{40} /.test(row)).length, 3, "only delta objects packed");
    assert.equal(rows.includes(f.base), false, "canonical base commit excluded");
  });
}

test("delta boundary: recovered-source default is refused before Git", linux, async t => {
  const f = await maintenanceDeltaFixture(t); f.request.defaultBranch = "recovered-source";
  await refused(f); assert.equal(f.requests.length, 0);
});

test("delta boundary: candidate absent from canonical objects is dropped and delivered", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  f.request.original.baseCommit = f.tip;
  await assert.rejects(f.native(f.bare, ["cat-file", "-e", f.tip]), "candidate is clone-only");
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.equal(await f.native(f.bare, ["show", f.tip + ":new"]), "new selected blob");
  await f.retained();
});

test("delta boundary: non-anchor-reachable candidate is dropped without dropping delta", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  await f.native(f.seed, ["checkout", "--orphan", "unrelated"]);
  await f.native(f.seed, ["commit", "--allow-empty", "-qm", "unrelated"]);
  const unrelated = await f.native(f.seed, ["rev-parse", "HEAD"]);
  await f.native(f.bare, ["fetch", "--no-tags", f.seed, unrelated]);
  f.request.original.baseCommit = unrelated;
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.equal(await f.native(f.bare, ["show", f.tip + ":new"]), "new selected blob");

  const g = await maintenanceDeltaFixture(t), candidate = g.tip, refs = await g.refs();
  await g.native(g.bare, ["fetch", "--no-write-fetch-head", "--no-tags", g.clone, candidate]);
  assert.equal(await g.native(g.bare, ["cat-file", "-t", candidate]), "commit", "candidate is present");
  await assert.rejects(g.native(g.bare, ["merge-base", "--is-ancestor", candidate, g.base]), { code: 1 });
  assert.equal(await g.refs(), refs, "connected candidate has no anchoring ref");
  const candidateObjects = (await g.native(g.clone,
    ["rev-list", "--objects", "--no-object-names", candidate, "--not", g.base])).split("\n");
  assert.ok(candidateObjects.includes(candidate));
  assert.equal(candidateObjects.length, 3, "candidate has its own commit, tree and unique blob");
  await fs.writeFile(path.join(g.clone, "child-only"), "unique child delta blob\n");
  await g.native(g.clone, ["add", "."]); await g.native(g.clone, ["commit", "-qm", "child of unanchored candidate"]);
  const childW = await g.native(g.clone, ["rev-parse", "HEAD"]);
  g.request.tip = childW;
  g.request.original.baseCommit = candidate;
  const expectedSelectedOIDs = (await g.native(g.clone,
    ["rev-list", "--objects", "--no-object-names", childW, "--not", g.base])).split("\n");
  for (const oid of candidateObjects) assert.ok(expectedSelectedOIDs.includes(oid), "full range includes candidate objects");
  const index = g.cache["indexMaintenancePack"].bind(g.cache);
  let actualSelectedOIDs: string[] | undefined;
  t.mock.method(g.cache as any, "indexMaintenancePack", async (...args: Parameters<typeof index>) => {
    actualSelectedOIDs = [...args[2]];
    return index(...args);
  });
  assert.deepEqual(await g.run(), { kind: "imported", sourceSha: childW });
  assert.ok(actualSelectedOIDs);
  assert.deepEqual(new Set(actualSelectedOIDs), new Set(expectedSelectedOIDs), "ineligible candidate excludes no selected objects");
  assert.ok(actualSelectedOIDs.includes(candidate));
  assert.equal(await g.native(g.bare, ["show", childW + ":child-only"]), "unique child delta blob");
  assert.equal(await g.refs(), refs);
  assert.equal(g.joined(), g.requests.length);
  await g.retained();
});

for (const entry of ["unknown", "info/http-alternates", "pack/pack-" + "a".repeat(40) + ".promisor"]) {
  test("delta source allowlist refuses " + entry + " and retains source/custody", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    await fs.writeFile(path.join(f.clone, ".git", "objects", entry), "unsupported\n");
    await refused(f); assert.equal(f.requests.length, 0);
  });
}
for (const target of ["loose", "fanout", "objects", "pack"]) {
  test("delta source no-follow refuses symlink " + target, linux, async t => {
    const f = await maintenanceDeltaFixture(t), objects = path.join(f.clone, ".git", "objects");
    const original = target === "loose" ? path.join(objects, f.tip.slice(0, 2), f.tip.slice(2)) :
      target === "fanout" ? path.join(objects, f.tip.slice(0, 2)) :
      target === "pack" ? path.join(objects, "pack") : objects;
    const moved = path.join(f.dir, "displaced-" + target);
    await fs.rename(original, moved); await fs.symlink(moved, original);
    const identity = await fs.lstat(moved);
    await refused(f);
    assert.equal((await fs.lstat(moved)).ino, identity.ino, "symlink target retained");
  });
}
for (const name of ["loose", "info/packs", ...["pack", "idx", "rev", "mtimes", "bitmap", "keep"]
  .map(ext => "pack/pack-" + "a".repeat(40) + "." + ext)]) {
  test("delta source refuses every hardlinked file family: " + name, linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const objects = path.join(f.clone, ".git", "objects");
    const target = name === "loose" ? path.join(objects, f.tip.slice(0, 2), f.tip.slice(2)) : path.join(objects, name);
    if (name !== "loose") await fs.writeFile(target, "hardlink fixture");
    const alias = path.join(f.dir, "alias");
    await fs.link(target, alias);
    const bytes = await fs.readFile(alias);
    await refused(f, "maintenance_source_alias_refused");
    assert.deepEqual(await fs.readFile(alias), bytes, "foreign hardlink preserved");
    assert.equal((await fs.lstat(target)).nlink, 2);
  });
}

for (const clean of [false, true]) {
  test("delta shallow: existing file refuses without opening (clean=" + clean + ")", linux, async t => {
    const f = await maintenanceDeltaFixture(t, { clean });
    const shallow = path.join(f.bare, "shallow");
    await fs.writeFile(shallow, f.base + "\n");
    const open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      assert.equal(typeof args[0] === "string" && args[0].endsWith("/shallow"), false, "shallow is never opened");
      return open(...args);
    });
    await refused(f); assert.equal(f.pairs(), 0);
  });
}
test("delta shallow: file inserted after private proof refuses before accepted import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  f.cache.afterPhase = async phase => {
    if (phase === "proof") await fs.writeFile(path.join(f.bare, "shallow"), f.base + "\n");
  };
  await refused(f);
});
test("delta shallow: canonical queries have independent environment with no private/shallow override", linux, async t => {
  const f = await maintenanceDeltaFixture(t, { clean: true });
  assert.deepEqual(await f.run(), { kind: "already_present", sourceSha: f.base });
  const queries = f.requests.filter(r => r.argv.includes("--is-shallow-repository"));
  assert.equal(queries.length, 2);
  for (const r of queries) for (const key of ["GIT_SHALLOW_FILE", "GIT_DIR", "GIT_OBJECT_DIRECTORY",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES"]) assert.equal(r.env[key], undefined, key + " omitted");
});

test("delta proof: corrupt new loose object bytes refuse before import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const object = path.join(f.clone, ".git", "objects", f.tip.slice(0, 2), f.tip.slice(2));
  await fs.chmod(object, 0o600);
  await fs.writeFile(object, "corrupt");
  const refs = await f.refs();
  await assert.rejects(f.run());
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(await f.refs(), refs);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});
test("delta pins: canonical config changed after proof refuses before import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  f.cache.afterPhase = async phase => {
    if (phase === "proof") await fs.appendFile(path.join(f.bare, "config"), "\n[fixture]\nchanged = true\n");
  };
  await refused(f);
});
test("delta cancellation: cancellation after proof joins every child and prevents import", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  f.cache.afterPhase = async phase => { if (phase === "proof") f.controller.abort(new Error("fixture_cancelled")); };
  await refused(f, "fixture_cancelled");
});

test("delta scratch F: precreated symlink write target is never followed or removed", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const foreign = path.join(f.dir, "foreign");
  await fs.writeFile(foreign, "keep foreign bytes");
  const open = fs.open.bind(fs);
  let attacked = false;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (args[0] === path.join(f.request.outDir, "read", "HEAD") && !attacked) {
      attacked = true; await fs.symlink(foreign, args[0]);
    }
    return open(...args);
  });
  const refs = await f.refs();
  await assert.rejects(f.run(), { code: "EEXIST" });
  assert.equal(attacked, true);
  assert.equal(await fs.readFile(foreign, "utf8"), "keep foreign bytes");
  assert.equal((await fs.lstat(path.join(f.request.outDir, "read", "HEAD"))).isSymbolicLink(), true);
  assert.equal(await f.refs(), refs);
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  await f.retained();
});
test("delta scratch: active pack inode swap prevents a second short write", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const target = path.join(f.request.outDir, "proof", "objects", "pack", "stage.pack");
  const open = fs.open.bind(fs);
  let writes = 0;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] === target) {
      const write = file.write.bind(file);
      t.mock.method(file, "write", async (bytes: Buffer, offset: number, length: number, position: number) => {
        writes++;
        const result = await write(bytes, offset, writes === 1 ? Math.min(length, 1) : length, position);
        if (writes === 1) {
          await fs.unlink(target);
          await fs.writeFile(target, "foreign pack bytes");
        }
        return result;
      });
    }
    return file;
  });
  await refused(f, "maintenance_pack_staging_refused");
  assert.equal(writes, 1, "replacement is detected before the second fd write");
  assert.equal(await fs.readFile(target, "utf8"), "foreign pack bytes", "foreign inode survives cleanup");
  assert.equal(f.requests.filter(r => gitVerb(r) === "pack-objects").length, 1, "producer was actually launched");
  assert.equal(f.requests.some(r => gitVerb(r) === "index-pack"), false);
});

test("delta scratch: earlier private HEAD mutation during staging prevents every later Git launch", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const target = path.join(f.request.outDir, "proof", "objects", "pack", "stage.pack");
  const open = fs.open.bind(fs);
  let launchesAtMutation: number | undefined;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const file = await open(...args);
    if (args[0] === target) {
      const write = file.write.bind(file);
      t.mock.method(file, "write", async (bytes: Buffer, offset: number, length: number, position: number) => {
        const result = await write(bytes, offset, length, position);
        if (launchesAtMutation === undefined) {
          launchesAtMutation = f.requests.length;
          await fs.appendFile(path.join(f.request.outDir, "read", "HEAD"), "changed\n");
        }
        return result;
      });
    }
    return file;
  });
  const refs = await f.refs();
  await assert.rejects(f.run(), /maintenance_(source_layout_unsupported|pack_staging_refused|pack_index_refused)/);
  assert.ok(launchesAtMutation !== undefined, "mutation happened while producer was staging");
  assert.equal(f.requests.length, launchesAtMutation, "no later Git launch, including canonical config acquisition");
  assert.equal(f.requests.filter(r => gitVerb(r) === "pack-objects").length, 1);
  assert.equal(f.requests.some(r => gitVerb(r) === "index-pack"), false);
  assert.equal(f.joined(), f.requests.length, "in-flight producer joined before refusal");
  assert.equal(await f.refs(), refs);
  await f.retained();
});

test("delta scratch F: disposal preserves a foreign entry", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const foreign = path.join(f.request.outDir, "foreign");
  f.cache.afterPhase = async phase => { if (phase === "proof") await fs.writeFile(foreign, "foreign"); };
  const refs = await f.refs();
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.equal(await fs.readFile(foreign, "utf8"), "foreign");
  assert.equal(await f.refs(), refs);
  assert.equal(f.requests.filter(canonicalImport).length, 1);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

for (const foreignRoot of [".git", "objects"]) {
  test("delta source device: modeled foreign " + foreignRoot + " tree refuses even with matching descendants", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const root = path.join(f.clone, ".git", ...(foreignRoot === "objects" ? ["objects"] : []));
    const foreign = (p: string) => p === root || p.startsWith(root + "/");
    const project = <T extends { dev: bigint | number }>(s: T): T => {
      s.dev = typeof s.dev === "bigint" ? s.dev + 1n : s.dev + 1;
      return s;
    };
    const lstat = fs.lstat.bind(fs);
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
      const s = await lstat(...args);
      return foreign(f.physical(args[0])) ? project(s) : s;
    });
    f.hooks.open = (file, p) => {
      if (!foreign(p)) return;
      const stat = file.stat.bind(file);
      t.mock.method(file, "stat", async (...args: Parameters<typeof file.stat>) => project((await stat(...args))!));
    };
    // Model only st_dev for the complete object-store subtree. Every real byte,
    // identity, permission and child process otherwise uses the actual fixture.
    await refused(f);
    assert.equal(f.requests.length, 0, "foreign source store refused before Git");
  });
}

test("delta scratch F: parent inode swap after destination-absence probe prevents rename", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const parent = path.join(f.request.outDir, "proof", "objects", "pack");
  const lstat = fs.lstat.bind(fs), rename = fs.rename.bind(fs);
  let swapped = false, attempted = 0;
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    try { return await lstat(...args); }
    catch (error) {
      const p = args[0];
      if (!swapped && (error as NodeJS.ErrnoException).code === "ENOENT" && typeof p === "string" &&
          p.startsWith(parent + "/pack-") && p.endsWith(".pack")) {
        swapped = true;
        await rename(parent, parent + "-original");
        await fs.mkdir(parent, { mode: 0o700 });
      }
      throw error;
    }
  });
  t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
    if (args[0] === path.join(parent, "stage.pack")) attempted++;
    return rename(...args);
  });
  const error = await f.run().then(() => undefined, failure => failure);
  assert.ok(error instanceof Error);
  assert.equal(swapped, true, "the intended probe interval was reached");
  assert.equal(attempted, 0, "parent must be rechecked after target probe before rename dispatch");
  assert.equal(error.message, "maintenance_source_layout_unsupported");
  assert.ok((await fs.lstat(path.join(parent + "-original", "stage.pack"))).isFile(), "uncertain original artifact retained");
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(f.pairs(), 0);
  await f.retained();
});
