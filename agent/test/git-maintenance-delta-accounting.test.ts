import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { Readable } from "node:stream";
import { createHash } from "node:crypto";
import { MaintenanceResources, type MaintenanceResource } from "../src/recovery-maintenance.js";
import { maintenanceDeltaFixture, canonicalImport } from "./maintenance-delta-fixture.js";

const linux = { skip: process.platform !== "linux" };
// Resolve sites by their enclosing scope and operation, so added lines cannot
// silently redirect an assertion to a different charge.
const source = await fs.readFile(new URL("../src/git.ts", import.meta.url), "utf8");
function site(scope: string, operation: string): number {
  const start = source.indexOf(scope);
  assert.ok(start >= 0, scope);
  const offset = source.indexOf(operation, start);
  assert.ok(offset >= start, operation);
  return source.slice(0, offset).split("\n").length;
}
const sites = {
  owned: site("const validate = async (includeLeaves", "const now = await stat(name,"),
  own: site("const own = (p: string, directory:", 'MaintenanceResources.consume('),
  artifactReserve: site("const own = (p: string, directory:", "if (!administrative)"),
  scratch: site("const scratchCheck = async", "const now = await stat(p,"),
  mkdir: site("const mkdir = async (p: string)", 'charge(p, "adminEntries")'),
  create: site("const write = async (p: string, value:", 'charge(p, "adminEntries")'),
  initial: site("const write = async (p: string, value:", "const created = await file.stat"),
  named: site("const write = async (p: string, value:", "const named = await stat"),
  before: site("const write = async (p: string, value:", "const fd = await file.stat"),
  after: site("const write = async (p: string, value:", "recordOwn(await file.stat"),
  ancestry: site('await infoControl(bare, "alternates", true)', "const s = await stat(p,"),
};
// Stage and index are observed by call window, not by source text, so an edited
// line inside them cannot move the observation off the charge it must pin.
const windowed = ["stage", "index"] as const;
const windowMethod = { stage: "stageMaintenancePack", index: "indexMaintenancePack" } as const;
type Entry = { resource: MaintenanceResource; amount: number };
function observe(t: test.TestContext, cache?: object) {
  const rows = new Map<string, Entry[]>();
  const consume = MaintenanceResources.consume.bind(MaintenanceResources);
  let inside: string | undefined;
  for (const name of cache ? windowed : []) {
    const original = (cache as any)[windowMethod[name]].bind(cache);
    t.mock.method(cache as any, windowMethod[name], async (...args: unknown[]) => {
      const outer = inside; inside = name;
      try { return await original(...args); } finally { inside = outer; }
    });
  }
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    const frames = [...new Error().stack!.matchAll(/\/git\.ts:(\d+):\d+/g)].map(m => Number(m[1]));
    consume(resource, amount);
    if (resource !== "sourceEntries" && resource !== "adminEntries") return;
    if (inside) { const list = rows.get(inside) ?? []; list.push({ resource, amount }); rows.set(inside, list); }
    for (const [name, line] of Object.entries(sites)) if (frames.includes(line)) {
      const list = rows.get(name) ?? []; list.push({ resource, amount }); rows.set(name, list);
    }
  });
  return rows;
}

test("delta accounting: every validation and reservation is source; real creation remains admin, including short writes", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const rows = observe(t, f.cache);
  const controlWrites: Array<[number, number, number]> = [];
  f.hooks.open = (file, p) => {
    if (!["HEAD", "config"].includes(path.basename(p)) || !p.startsWith(f.request.outDir + "/")) return;
    const write = file.write.bind(file);
    t.mock.method(file, "write", async (bytes: Buffer, offset: number, length: number, position: number) => {
      controlWrites.push([offset, length, position]);
      return write(bytes, offset, Math.min(7, length), position);
    });
  };
  const refs = await f.refs();
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  for (const name of [...Object.keys(sites), ...windowed]) {
    const entries = rows.get(name)!;
    assert.ok(entries?.length, "actual Git fixture exercised " + name);
    const category = ["mkdir", "create"].includes(name) ? "adminEntries" : "sourceEntries";
    for (const entry of entries) {
      assert.equal(entry.resource, category, name);
      // The stage/index windows also contain the three-unit-plus own() reserves; their category is pinned, their amounts are not.
      if (name !== "own" && !(windowed as readonly string[]).includes(name)) assert.equal(entry.amount, name === "artifactReserve" ? 3 : 1, name);
    }
  }
  assert.equal(rows.get("mkdir")!.length, 8);
  assert.equal(rows.get("create")!.length, 4);
  assert.equal(rows.get("initial")!.length, 4);
  for (const name of ["named", "before", "after"]) assert.equal(rows.get(name)!.length, controlWrites.length, name);
  assert.ok(controlWrites.some(([offset]) => offset > 0), "short writes retry the remaining tail");
  assert.equal(rows.get("artifactReserve")!.length, 4, "staged and renamed artifacts retain separate three-unit reserves");
  const root = f.request.outDir;
  const dirs = [path.join(root, "read"), path.join(root, "read/objects"), path.join(root, "read/refs"),
    path.join(root, "proof"), path.join(root, "proof/objects"), path.join(root, "proof/refs"), root,
    path.join(root, "proof/objects/pack")];
  const controls = ["read/HEAD", "read/config", "proof/HEAD", "proof/config"].map(p => path.join(root, p));
  const artifacts = ["stage.pack", "stage.idx", "pack-" + "0".repeat(40) + ".pack", "pack-" + "0".repeat(40) + ".idx"]
    .map(p => path.join(root, "proof/objects/pack", p));
  const reserved = rows.get("own")!.map(e => e.amount).sort((a, b) => a - b);
  const expected = [...dirs, ...controls].map(p => p.split("/").length + 3)
    .concat(artifacts.map(p => p.split("/").length)).sort((a, b) => a - b);
  assert.deepEqual(reserved, expected, "all sixteen parent/identity/cleanup reserves survive, including renamed entries");
  assert.equal(await f.refs(), refs);
  assert.equal(f.requests.filter(canonicalImport).length, 1);
  assert.equal(f.joined(), f.requests.length);
  await assert.rejects(fs.lstat(root), { code: "ENOENT" });
  await f.retained();
});

test("delta accounting: canonical producer raw rows use source once; ordinary family rows retain admin default", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  await f.native(f.bare, ["config", "accounting.extra", "raw row retained before filtering"]);
  const consumeRows = MaintenanceResources.consumeMetadataEntriesIfActive.bind(MaintenanceResources);
  const snapshots: Array<{ category: string; value: string; entries: Entry[] }> = [];
  let current: Entry[] | undefined;
  const consume = MaintenanceResources.consume.bind(MaintenanceResources);
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    consume(resource, amount);
    if (current && (resource === "adminEntries" || resource === "sourceEntries")) current.push({ resource, amount });
  });
  t.mock.method(MaintenanceResources, "consumeMetadataEntriesIfActive", (value: string, separator: "\0" | "\n",
    category: "sourceEntries" | "adminEntries" = "adminEntries") => {
    const entries: Entry[] = [];
    current = entries;
    try { consumeRows(value, separator, category); } finally { current = undefined; }
    if (separator === "\0") snapshots.push({ category, value, entries });
  });
  const ordinary = () => f.inScope(() => f.cache.matchMaintenanceSourceFamily(f.bare, f.request.branch,
    f.request.key, f.source.runId, [f.source]));
  assert.ok(await ordinary());
  assert.ok(snapshots.length > 0);
  assert.ok(snapshots.every(s => s.category === "adminEntries"));
  const ordinarySnapshots = snapshots.length;
  assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
  assert.ok(snapshots.slice(ordinarySnapshots).some(s => s.category === "sourceEntries"));
  for (const snapshot of snapshots) {
    assert.ok(snapshot.value.includes("accounting.extra\nraw row retained before filtering"));
    assert.deepEqual(snapshot.entries, snapshot.value.split("\0").filter(Boolean)
      .map(() => ({ resource: snapshot.category, amount: 1 })), "each raw row charged once before interpretation");
  }
  await f.retained();
});

test("delta accounting: ordinary stage/index retain admin parents, target checks, fsync and isolated environment", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  const rows = observe(t, f.cache);
  const out = path.join(f.cache.recoveryRoot, "ordinary.pack");
  const bytes = Buffer.alloc(32);
  bytes.write("PACK"); bytes.writeUInt32BE(2, 4);
  createHash("sha1").update(bytes.subarray(0, 12)).digest().copy(bytes, 12);
  let syncs = 0;
  f.hooks.open = (file, p) => {
    if (p !== out) return;
    const sync = file.sync.bind(file);
    t.mock.method(file, "sync", async () => { syncs++; await sync(); });
  };
  const stdout = Readable.from([bytes]);
  assert.deepEqual(await f.inScope(() => f.cache["stageMaintenancePack"]({
    stdout, exited: Promise.resolve({ code: 0, stderr: "" }), abort: () => { stdout.destroy(); },
  }, out, 32, f.request.prepare)), { path: out, byteSize: 32, objectCount: 0 });
  const stats = rows.get("stage")!;
  assert.ok(stats.length > 2, "parents and target checks both exercised");
  assert.ok(stats.every(e => e.resource === "adminEntries"));
  assert.ok(stats.some(e => e.amount === 1));
  assert.equal(syncs, 1);
  assert.deepEqual(await fs.readFile(out), bytes);
  assert.equal(f.requests.length, 0);
  const index = await f.inScope(() => f.cache["indexMaintenancePack"]({
    path: out, byteSize: 32, objectCount: 0,
  }, out.replace(".pack", ".idx"), [], f.request.prepare));
  assert.equal(index.objectCount, 0);
  const indexStats = rows.get("index")!;
  assert.ok(indexStats.length > 2);
  assert.ok(indexStats.every(e => e.resource === "adminEntries"));
  assert.ok(indexStats.some(e => e.amount === 1));
  assert.equal(f.requests.length, 1);
  assert.equal(f.requests[0]!.env.GIT_DIR, "/dev/null");
  assert.equal(f.requests[0]!.env.GIT_OBJECT_DIRECTORY, "/dev/null");
  assert.equal(f.requests[0]!.env.GIT_ALTERNATE_OBJECT_DIRECTORIES, "");
  assert.deepEqual(await fs.readFile(out), bytes);
  assert.equal(f.joined(), 1);
  await f.retained();
});

test("delta accounting: raw producer config exhaustion rejects before semantic filtering", linux, async t => {
  const f = await maintenanceDeltaFixture(t);
  await f.native(f.bare, ["config", "accounting.extra", "ignored semantic key"]);
  const consumeRows = MaintenanceResources.consumeMetadataEntriesIfActive.bind(MaintenanceResources);
  let reached = false, attempted = 0, expected = 0;
  const consume = MaintenanceResources.consume.bind(MaintenanceResources);
  let counting = false;
  t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
    if (counting && resource === "sourceEntries") attempted++;
    return consume(resource, amount);
  });
  t.mock.method(MaintenanceResources, "consumeMetadataEntriesIfActive", (value: string, separator: "\0" | "\n",
    category: "sourceEntries" | "adminEntries" = "adminEntries") => {
    if (category !== "sourceEntries") return consumeRows(value, separator, category);
    reached = true;
    assert.ok(value.includes("accounting.extra\nignored semantic key"));
    expected = value.split("\0").filter(Boolean).length;
    const used = f.charges.filter(c => c.resource === "sourceEntries").reduce((n, c) => n + c.amount, 0);
    consume("sourceEntries", 65536 - used - expected + 1);
    counting = true;
    try { return consumeRows(value, separator, category); } finally { counting = false; }
  });
  const refs = await f.refs();
  await assert.rejects(f.run(), /maintenance_(resource_exhausted|token_invalid)/);
  assert.equal(reached, true);
  assert.equal(attempted, expected, "last raw row refuses before any filtering or subsequent Git launch");
  assert.equal(f.requests.filter(canonicalImport).length, 0);
  assert.equal(await f.refs(), refs);
  assert.equal(f.joined(), f.requests.length);
  await f.retained();
});

for (const target of ["mkdir", "create", "initial", "named", "before", "after", "scratch", "ancestry", "owned", "stage", "index"] as const) {
  test("delta accounting: exhausted precharge blocks actual " + target + " and retains custody", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const consume = MaintenanceResources.consume.bind(MaintenanceResources);
    let reached = false, inside: string | undefined;
    for (const name of windowed) {
      const original = (f.cache as any)[windowMethod[name]].bind(f.cache);
      t.mock.method(f.cache as any, windowMethod[name], async (...args: unknown[]) => {
        const outer = inside; inside = name;
        try { return await original(...args); } finally { inside = outer; }
      });
    }
    t.mock.method(MaintenanceResources, "consume", (resource: MaintenanceResource, amount: number) => {
      const frames = [...new Error().stack!.matchAll(/\/git\.ts:(\d+):\d+/g)].map(m => Number(m[1]));
      const here = target === "stage" || target === "index" ? inside === target : frames.includes(sites[target]);
      if (!reached && here && ["sourceEntries", "adminEntries"].includes(resource)) {
        reached = true;
        const used = f.charges.filter(c => c.resource === resource).reduce((n, c) => n + c.amount, 0);
        consume(resource, (resource === "sourceEntries" ? 65536 : 4096) - used);
      }
      return consume(resource, amount);
    });
    const refs = await f.refs();
    await assert.rejects(f.run(), /maintenance_(resource_exhausted|token_invalid)/);
    assert.equal(reached, true, "original slot exhausted at named operation");
    assert.equal(await f.refs(), refs);
    assert.equal(f.requests.filter(canonicalImport).length, 0);
    assert.equal(f.joined(), f.requests.length);
    if (target === "initial") {
      assert.ok((await fs.lstat(path.join(f.request.outDir, "read/HEAD"))).isFile(),
        "exclusive creation preceded the refused identity acquisition; unregistered control is retained");
    } else await assert.rejects(fs.lstat(f.request.outDir), { code: "ENOENT" });
    await f.retained();
  });
}
