import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { constants } from "node:fs";
import { MaintenanceResources, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { maintenanceDeltaFixture, canonicalImport } from "./maintenance-delta-fixture.js";

const linux = { skip: process.platform !== "linux" };
const layout = { message: "maintenance_source_layout_unsupported" };

test("delta shallow source: local bare clone of actual shallow origin refuses producer admission", linux, async t => {
  const f = await maintenanceDeltaFixture(t, { shallowSource: true });
  assert.equal(await f.native(f.bare, ["rev-parse", "--is-shallow-repository"]), "true");
  assert.equal((await fs.lstat(path.join(f.bare, "shallow"))).isFile(), true);
  assert.throws(() => execFileSync("/usr/bin/git", ["-C", f.bare, "rev-list", "--objects", "--missing=error", f.base], {
    env: { PATH: "/usr/bin:/bin", GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_SYSTEM: "/dev/null", GIT_SHALLOW_FILE: "/dev/null", GIT_GRAFT_FILE: "/dev/null",
      GIT_NO_REPLACE_OBJECTS: "1", GIT_NO_LAZY_FETCH: "1", GIT_ALLOW_PROTOCOL: "none" }, stdio: "pipe",
  }), "raw closure ignores the shallow decoration and exposes the missing parent");
  await assert.rejects(f.run(), layout);
  assert.equal(f.requests.length, 0, "filesystem precondition refuses before canonical query or boundary Git");
  assert.equal(f.pairs(), 0);
  await f.retained();
});

function shallowQueryOutput(stream: Readable, answer: string): Readable {
  return Readable.from((async function* () {
    for await (const _ of stream) { /* Settle the real query before replacing its bytes. */ }
    if (answer === "error") throw new Error("fixture shallow query pipe failed");
    yield Buffer.from(answer);
  })(), { objectMode: false });
}

for (const answer of ["false\n", "true\n", "unknown\n", "", "error"]) {
  const succeeds = answer === "false\n";
  test(succeeds ? "delta shallow query: wrapped false succeeds and settles both queries" :
    "delta shallow query: absent file with observed Git state " + JSON.stringify(answer) + " refuses", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    let queries = 0;
    f.hooks.stdout = (request, stream) => {
      if (!request.argv.includes("--is-shallow-repository")) return stream;
      queries++;
      for (const key of ["GIT_SHALLOW_FILE", "GIT_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"])
        assert.equal(request.env[key], undefined, key + " absent in canonical query");
      return shallowQueryOutput(stream, answer);
    };
    if (succeeds) assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
    else await assert.rejects(f.run(), layout);
    assert.equal(queries, succeeds ? 2 : 1);
    await assert.rejects(fs.lstat(path.join(f.bare, "shallow")), { code: "ENOENT" });
    assert.equal(f.requests.filter(canonicalImport).length, succeeds ? 1 : 0);
    assert.equal(f.joined(), f.requests.length);
    assert.equal(f.pairs(), succeeds ? 1 : 0);
    await f.retained();
  });
}

test("delta shallow query: state becomes true after proof refuses without a shallow file", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  let queries = 0, proved = false;
  f.cache.afterPhase = async phase => { if (phase === "proof") proved = true; };
  f.hooks.stdout = (request, stream) => {
    if (!request.argv.includes("--is-shallow-repository")) return stream;
    queries++;
    if (queries === 2) assert.equal(proved, true, "second query follows successful proof");
    return shallowQueryOutput(stream, queries === 1 ? "false\n" : "true\n");
  };
  await assert.rejects(f.run(), layout);
  assert.equal(queries, 2);
  assert.equal(proved, true);
  await assert.rejects(fs.lstat(path.join(f.bare, "shallow")), { code: "ENOENT" });
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(f.joined(), f.requests.length);
  assert.equal(f.pairs(), 0);
  await f.retained();
});

test("delta shallow lstat: non-ENOENT failure refuses even though bare is non-shallow", linux, async t => {
  const f = await maintenanceDeltaFixture(t), lstat = fs.lstat.bind(fs);
  let attempted = 0;
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (f.physical(args[0]) === path.join(f.bare, "shallow")) {
      attempted++; throw Object.assign(new Error("fixture inaccessible shallow state"), { code: "EACCES" });
    }
    return lstat(...args);
  });
  await assert.rejects(f.run(), layout);
  assert.equal(attempted, 1);
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await f.retained();
});

for (const kind of ["symlink", "directory", "FIFO"]) {
  test("delta shallow lstat: any present " + kind + " refuses without opening", linux, async t => {
    const f = await maintenanceDeltaFixture(t), name = path.join(f.bare, "shallow");
    const config = await fs.readFile(path.join(f.bare, "config"));
    const target = path.join(f.dir, "shallow-target");
    await fs.writeFile(target, "retained external bytes");
    if (kind === "symlink") await fs.symlink(target, name);
    if (kind === "directory") await fs.mkdir(name);
    if (kind === "FIFO") execFileSync("/usr/bin/mkfifo", [name], { stdio: "pipe" });
    let opened = false;
    f.hooks.open = (_file, p) => { if (p === name || p === target) opened = true; };
    await assert.rejects(f.run(), layout);
    assert.equal(opened, false);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    assert.equal((await fs.lstat(f.clone)).isDirectory(), true);
    assert.deepEqual(await fs.readFile(path.join(f.bare, "config")), config, "custody bytes retained without Git opening hostile shallow state");
    assert.equal(await fs.readFile(target, "utf8"), "retained external bytes");
    const stat = await fs.lstat(name);
    assert.equal(kind === "symlink" ? stat.isSymbolicLink() : kind === "directory" ? stat.isDirectory() : stat.isFIFO(), true);
  });
}

for (const clean of [false, true]) {
  test("delta shallow accounting: both independent checks charged without duplicate launches (clean=" + clean + ")", linux, async t => {
    const f = await maintenanceDeltaFixture(t, { clean }), lstat = fs.lstat.bind(fs);
    let probes = 0;
    t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
      if (f.physical(args[0]) === path.join(f.bare, "shallow")) {
        probes++;
        const [entry, metadata] = f.charges.slice(-2);
        assert.equal(entry?.resource, "sourceEntries"); assert.equal(entry?.amount, 1);
        assert.equal(metadata?.resource, "metadataBytes"); assert.ok(metadata!.amount >= 2048);
      }
      return lstat(...args);
    });
    assert.deepEqual(await f.run(), { kind: clean ? "already_present" : "imported", sourceSha: f.tip });
    assert.equal(f.requests.filter(r => r.argv.includes("--is-shallow-repository")).length, 2);
    assert.ok(probes >= 2, "absence plus all actual pin rechecks are charged");
    assert.equal(f.charges.filter(c => c.resource === "launches").reduce((sum, c) => sum + c.amount, 0), f.requests.length);
    assert.equal(f.pairs(), clean ? 0 : 1);
    await f.retained();
  });
}

for (const only of ["refs/heads/recovered-source", "refs/tags/transient"]) {
  test("delta boundary5b: " + only + " alone never anchors and refuses exact boundary_missing", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    for (const ref of ["refs/remotes/origin/main", "refs/heads/main", "refs/uzi-runner/feature"])
      await f.native(f.bare, ["update-ref", "-d", ref]);
    await f.native(f.bare, ["update-ref", only, f.base]);
    const refs = await f.refs();
    await assert.rejects(f.run(), { message: "maintenance_transfer_boundary_missing" });
    assert.equal(await f.refs(), refs);
    assert.equal(f.requests.filter(canonicalImport).length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}

test("delta boundary5a: stale heads/default does not enlarge selected range with current origin/default", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  await fs.writeFile(path.join(f.seed, "ahead"), "current origin contents\n");
  await f.native(f.seed, ["add", "."]); await f.native(f.seed, ["commit", "-qm", "origin advances"]);
  const current = await f.native(f.seed, ["rev-parse", "HEAD"]);
  await f.native(f.bare, ["fetch", "--no-write-fetch-head", "origin", "main:refs/remotes/origin/main"]);
  assert.equal(await f.native(f.bare, ["rev-parse", "refs/heads/main"]), f.base);
  await f.native(f.clone, ["reset", "--hard", current]);
  await fs.writeFile(path.join(f.clone, "new"), "new selected blob\n");
  await f.native(f.clone, ["add", "."]); await f.native(f.clone, ["commit", "-qm", "new W above current origin"]);
  f.request.tip = await f.native(f.clone, ["rev-parse", "HEAD"]);
  const expected = (await f.native(f.clone, ["rev-list", "--objects", "--no-object-names", f.request.tip, "--not", current])).split("\n");
  const index = f.cache["indexMaintenancePack"].bind(f.cache);
  let observed: string[] = [];
  t.mock.method(f.cache as any, "indexMaintenancePack", async (...args: Parameters<typeof index>) => {
    observed = [...args[2]]; return index(...args);
  });
  const refs = await f.refs();
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.request.tip });
  assert.equal(expected.length, 3);
  assert.deepEqual([...observed].sort(), [...expected].sort());
  assert.equal(observed.includes(current), false);
  assert.equal(await f.refs(), refs);
  await f.retained();
});

for (const kind of ["differently named loose base alias", "interrupted loose finalize"]) {
  test("delta hardlink9: " + kind + " retains source and custody", linux, async t => {
    const f = await maintenanceDeltaFixture(t), objects = path.join(f.clone, ".git", "objects");
    const oid = kind === "interrupted loose finalize" ? await f.native(f.clone, ["rev-parse", f.tip + ":new"]) : f.tip;
    const original = path.join(objects, oid.slice(0, 2), oid.slice(2));
    const baseBlob = await f.native(f.bare, ["rev-parse", f.base + ":base"]);
    const alias = kind === "interrupted loose finalize" ? path.join(objects, "tmp_interrupted_finalize") :
      path.join(objects, baseBlob.slice(0, 2), baseBlob.slice(2));
    await fs.mkdir(path.dirname(alias), { recursive: true });
    await fs.link(original, alias);
    const bytes = await fs.readFile(original);
    await assert.rejects(f.run(), { message: "maintenance_source_alias_refused" });
    assert.deepEqual(await fs.readFile(original), bytes);
    assert.deepEqual(await fs.readFile(alias), bytes);
    assert.equal((await fs.lstat(original)).nlink, 2);
    assert.equal((await fs.lstat(alias)).ino, (await fs.lstat(original)).ino);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}

test("delta walk10: loose entries exhaust reduced remaining sourceEntries without renewing slot", linux, async t => {
  const f = await maintenanceDeltaFixture(t), loose = path.join(f.clone, ".git", "objects", "aa");
  await fs.mkdir(loose, { recursive: true });
  for (let i = 0; i < 100; i++) await fs.writeFile(path.join(loose, i.toString(16).padStart(38, "0")), "encoded walk fixture");
  let reached = false, exhausted = false, looseStats = 0;
  const consume = MaintenanceResources.consume.bind(MaintenanceResources), lstat = fs.lstat.bind(fs);
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    try { return consume(resource, amount); }
    catch (error) { if (resource === "sourceEntries") exhausted = true; throw error; }
  });
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (f.physical(args[0]).startsWith(loose + "/")) looseStats++;
    return lstat(...args);
  });
  f.cache.beforePhase = async phase => {
    if (phase !== "walk") return;
    reached = true;
    const used = f.charges.filter(c => c.resource === "sourceEntries").reduce((sum, c) => sum + c.amount, 0);
    f.charge("sourceEntries", 65536 - used - 128);
  };
  await assert.rejects(f.run(), /maintenance_(resource_exhausted|token_invalid)/);
  assert.equal(reached, true); assert.equal(exhausted, true); assert.ok(looseStats > 0);
  assert.equal((await fs.readdir(loose)).length >= 100, true);
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await f.retained();
});

test("delta walk10b: every walked loose entry stat and open is charged as sourceEntries with its own path", linux, async t => {
  const f = await maintenanceDeltaFixture(t), loose = path.join(f.clone, ".git", "objects", "aa");
  await fs.mkdir(loose, { recursive: true });
  for (let i = 0; i < 5; i++) await fs.writeFile(path.join(loose, i.toString(16).padStart(38, "0")), "encoded walk fixture");
  const lstat = fs.lstat.bind(fs), observed: string[] = [];
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (f.physical(args[0]).startsWith(loose + "/")) {
      const [entry, metadata] = f.charges.slice(-2);
      assert.equal(entry?.resource, "sourceEntries"); assert.equal(entry?.amount, 1);
      assert.equal(metadata?.resource, "metadataBytes");
      assert.equal(metadata?.amount, 2048 + Buffer.byteLength(String(args[0])), "the charge immediately preceding names this entry");
      observed.push(String(args[0]));
    }
    return lstat(...args);
  });
  const open = fs.open.bind(fs), opened: string[] = [];
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    if (f.physical(args[0]).startsWith(loose + "/")) {
      // importMaintenanceTip's own charge() helper, default category, immediately before the open.
      const [entry, metadata] = f.charges.slice(-2);
      assert.equal(entry?.resource, "sourceEntries"); assert.equal(entry?.amount, 1);
      assert.equal(metadata?.resource, "metadataBytes");
      assert.equal(metadata?.amount, 2048 + Buffer.byteLength(String(args[0])));
      opened.push(String(args[0]));
    }
    return open(...args);
  });
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.ok(opened.length >= 5, "each loose entry open was observed: " + opened.length);
  assert.ok(observed.length >= 5, "each loose entry stat was observed: " + observed.length);
  await f.retained();
});

for (const attack of ["parent symlink", "hardlinked write target"]) {
  test("delta scratch F: " + attack + " before control create refuses without touching foreign bytes", linux, async t => {
    const f = await maintenanceDeltaFixture(t), parent = path.join(f.request.outDir, "read");
    const foreign = path.join(f.dir, "foreign-private-parent"), marker = path.join(foreign, "HEAD");
    await fs.mkdir(foreign); await fs.writeFile(marker, "foreign bytes");
    const mkdir = fs.mkdir.bind(fs), open = fs.open.bind(fs);
    let inserted = false, attempted = 0;
    const observedFlags: Parameters<typeof fs.open>[1][] = [];
    t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
      const result = await mkdir(...args);
      if (args[0] === parent) {
        inserted = true;
        if (attack === "parent symlink") { await fs.rename(parent, parent + "-original"); await fs.symlink(foreign, parent); }
        else await fs.link(marker, path.join(parent, "HEAD"));
      }
      return result;
    });
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      if (args[0] === path.join(parent, "HEAD")) {
        attempted++;
        observedFlags.push(args[1]);
      }
      return open(...args);
    });
    await assert.rejects(f.run());
    assert.equal(inserted, true);
    assert.equal(attempted, attack === "parent symlink" ? 0 : 1);
    assert.equal(observedFlags.length, attack === "parent symlink" ? 0 : 1);
    if (attack === "hardlinked write target") {
      assert.equal(typeof observedFlags[0], "number");
      assert.equal((Number(observedFlags[0]) & constants.O_NOFOLLOW) !== 0 && (Number(observedFlags[0]) & constants.O_EXCL) !== 0, true);
    }
    assert.equal(await fs.readFile(marker, "utf8"), "foreign bytes");
    if (attack === "hardlinked write target") assert.equal((await fs.lstat(marker)).nlink, 2);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}

for (const dissociated of [false, true]) {
  test("delta layout17: selection excludes physical base objects (dissociated=" + dissociated + ")", linux, async t => {
    const f = await maintenanceDeltaFixture(t, { dissociated });
    const base = (await f.native(f.bare, ["rev-list", "--objects", "--no-object-names", f.base])).split("\n");
    const index = f.cache["indexMaintenancePack"].bind(f.cache);
    let selected: string[] = [];
    t.mock.method(f.cache as any, "indexMaintenancePack", async (...args: Parameters<typeof index>) => {
      selected = [...args[2]]; return index(...args);
    });
    const refs = await f.refs();
    assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
    assert.equal(selected.length, 3);
    assert.equal(selected.some(oid => base.includes(oid)), false);
    assert.equal(await f.refs(), refs);
    if (dissociated) await assert.rejects(fs.lstat(path.join(f.clone, ".git", "objects", "info", "alternates")), { code: "ENOENT" });
    else assert.equal(await fs.readFile(path.join(f.clone, ".git", "objects", "info", "alternates"), "utf8"), path.join(f.bare, "objects") + "\n");
    await f.retained();
  });
}

test("delta source8: gitfile is refused without following its real gitdir", linux, async t => {
  const f = await maintenanceDeltaFixture(t), actual = path.join(f.dir, "original-gitdir");
  await fs.rename(path.join(f.clone, ".git"), actual);
  const body = "gitdir: " + actual + "\n";
  await fs.writeFile(path.join(f.clone, ".git"), body);
  let followed = false;
  f.hooks.open = (_file, p) => { if (p === actual || p.startsWith(actual + "/")) followed = true; };
  await assert.rejects(f.run(), layout);
  assert.equal(followed, false);
  assert.equal(await fs.readFile(path.join(f.clone, ".git"), "utf8"), body);
  assert.ok((await fs.lstat(actual)).isDirectory());
  assert.equal(f.requests.length, 0);
  await f.retained();
});

test("delta source format: actual SHA-256 source tip refuses SHA-1 maintenance contract", linux, async t => {
  const f = await maintenanceDeltaFixture(t), actual = path.join(f.dir, "retained-sha1-gitdir");
  await fs.rename(path.join(f.clone, ".git"), actual);
  await f.native(f.clone, ["init", "-q", "--object-format=sha256", "-b", "main"]);
  await f.native(f.clone, ["add", "."]); await f.native(f.clone, ["commit", "-qm", "sha256 source"]);
  f.request.tip = await f.native(f.clone, ["rev-parse", "HEAD"]);
  assert.equal(f.request.tip.length, 64);
  await assert.rejects(f.run(), layout);
  assert.ok((await fs.lstat(actual)).isDirectory());
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await f.retained();
});

for (const name of ["objects/info/alternates", "objects/info/http-alternates", "info/alternates"]) {
  test("delta canonical1: present " + name + " refuses without opening", linux, async t => {
    const f = await maintenanceDeltaFixture(t), control = path.join(f.bare, name);
    const target = path.join(f.dir, "foreign-canonical-objects");
    await fs.mkdir(target); await fs.mkdir(path.dirname(control), { recursive: true });
    await fs.writeFile(control, target + "\n");
    let opened = false;
    f.hooks.open = (_file, p) => { if (p === control || p === target || p.startsWith(target + "/")) opened = true; };
    await assert.rejects(f.run(), layout);
    assert.equal(opened, false);
    assert.equal(await fs.readFile(control, "utf8"), target + "\n");
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}

for (const kind of ["symlink", "inode swap"]) {
  test("delta canonical2: ancestor " + kind + " after proof refuses before import", linux, async t => {
    const f = await maintenanceDeltaFixture(t), refs = await f.refs(), repos = path.dirname(f.bare);
    const retainedBare = path.join(repos + "-original", path.basename(f.bare));
    let swapped = false, afterProof = -1;
    f.cache.afterPhase = async phase => {
      if (phase !== "proof") return;
      await fs.rename(repos, repos + "-original");
      if (kind === "symlink") await fs.symlink(repos + "-original", repos); else await fs.mkdir(repos);
      swapped = true; afterProof = f.requests.length;
    };
    await assert.rejects(f.run(), layout);
    assert.equal(swapped, true);
    assert.equal(f.requests.length, afterProof);
    assert.equal(f.requests.filter(canonicalImport).length, 0);
    assert.equal(f.pairs(), 0);
    assert.equal(f.joined(), f.requests.length);
    assert.equal(await f.native(retainedBare, ["for-each-ref", "--format=%(refname) %(objectname)"]), refs);
    assert.match(await f.native(retainedBare, ["config", "--get", "uzi-recovery.feature.clone"]), /"runId":"run"/,
      "original custody descriptor retained at pinned repository's surviving name");
    assert.equal((await fs.lstat(f.clone)).isDirectory(), true);
  });
}

test("delta canonical import F: post-import pack-directory swap retains residue without accepted handoff", linux, async t => {
  const f = await maintenanceDeltaFixture(t), pack = path.join(f.bare, "objects", "pack"), refs = await f.refs();
  let swapped = false;
  f.cache.afterPhase = async phase => {
    if (phase !== "import") return;
    await fs.rename(pack, pack + "-original"); await fs.mkdir(pack);
    swapped = true;
  };
  await assert.rejects(f.run(), layout);
  assert.equal(swapped, true);
  assert.equal(f.requests.filter(canonicalImport).length, 1);
  assert.ok((await fs.readdir(pack + "-original")).some(name => name.endsWith(".pack")));
  assert.deepEqual(await fs.readdir(pack), [], "no rollback, deletion or repair of uncertain canonical entries");
  assert.equal(await f.refs(), refs);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

test("delta ordinary caller: public family scanner cannot receive the producer config-read adapter", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const ordinary = () => f.cache.matchMaintenanceSourceFamily(f.bare, f.request.branch, f.request.key,
    f.source.runId, [f.source]);
  assert.ok(await ordinary(), "ordinary discovery family qualifies with its existing reader");
  let adapterCalls = 0;
  const match = await f.cache.matchMaintenanceSourceFamily(f.bare, f.request.branch, f.request.key,
    f.source.runId, [f.source], { configRead: async () => { adapterCalls++; throw new Error("producer adapter reached ordinary caller"); } } as any);
  assert.ok(match, "ordinary call still uses its own canonical config reader");
  assert.equal(adapterCalls, 0);
  assert.equal(f.pairs(), 0, "ordinary family reads acquire no producer hold RPCs");
  assert.equal(f.requests.length, 0, "ordinary call does not borrow producer process owner");
  await f.retained();
});

for (const { label, name } of [
  { label: "punctuation", name: "release+qa@v2" },
  { label: "Unicode NBSP", name: "caf\u00e9\u00a0release" },
  { label: "long literal", name: Array.from({ length: 7 }, (_, i) => "x".repeat(160) + i).join("/") },
]) {
  test("delta literal anchor: repository-valid " + label + " default keeps the eligible origin anchor", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    await f.native(f.bare, ["check-ref-format", "refs/heads/" + name]);
    for (const ref of ["refs/remotes/origin/main", "refs/heads/main", "refs/uzi-runner/feature"])
      await f.native(f.bare, ["update-ref", "-d", ref]);
    await f.native(f.bare, ["update-ref", "refs/remotes/origin/" + name, f.base]);
    f.request.defaultBranch = name;
    const refs = await f.refs();
    assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
    assert.equal(await f.refs(), refs);
    assert.equal(f.requests.filter(canonicalImport).length, 1);
    await f.retained();
  });
}

test("delta shallow independence: present file refuses even if Git query would report false", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  await fs.writeFile(path.join(f.bare, "shallow"), f.base + "\n");
  let queried = 0;
  f.hooks.stdout = (request, stream) => {
    if (!request.argv.includes("--is-shallow-repository")) return stream;
    queried++;
    return Readable.from((async function* () {
      for await (const _ of stream) { /* hypothetical false query cannot override filesystem presence */ }
      yield Buffer.from("false\n");
    })());
  };
  await assert.rejects(f.run(), layout);
  assert.equal(queried, 0, "the independent no-follow presence check must refuse first");
  assert.equal(f.requests.length, 0);
  assert.equal(f.pairs(), 0);
  await f.retained();
});

for (const name of ["bad..name", "bad@{ref", "bad*glob", "nested/.hidden", "nested/component.lock/leaf", "bad\u0000name"]) {
  test("delta literal anchor: malformed ref " + JSON.stringify(name) + " refuses before any Git", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    f.request.defaultBranch = name;
    await assert.rejects(f.run(), layout);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}

for (const when of ["open", "completed read"]) {
  test("delta hardlink9: alias observed at " + when + " keeps the fixed alias refusal", linux, async t => {
    const f = await maintenanceDeltaFixture(t), objects = path.join(f.clone, ".git", "objects");
    const target = path.join(objects, "info", when === "open" ? "packs" : "alternates"), alias = path.join(f.dir, "late-alias");
    if (when === "open") await fs.writeFile(target, "counted pack metadata\n");
    const bytes = await fs.readFile(target);
    let linked = false;
    f.hooks.open = async (file, p) => {
      if (p !== target) return;
      if (when === "open") { await fs.link(target, alias); linked = true; }
      else {
        const read = file.read.bind(file);
        t.mock.method(file, "read", async (b: Buffer, o: number, n: number, at: number) => {
          const result = await read(b, o, n, at);
          if (!linked) { await fs.link(target, alias); linked = true; }
          return result;
        });
      }
    };
    await assert.rejects(f.run(), { message: "maintenance_source_alias_refused" });
    assert.equal(linked, true);
    assert.deepEqual(await fs.readFile(alias), bytes);
    assert.equal((await fs.lstat(target)).nlink, 2);
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    await f.retained();
  });
}
