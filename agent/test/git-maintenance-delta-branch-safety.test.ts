import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createLogger } from "../src/log.js";
import { maintenanceDeltaFixture, canonicalImport } from "./maintenance-delta-fixture.js";

const linux = { skip: process.platform !== "linux" };
const reason = "maintenance_source_layout_unsupported";
// Enumerate every C0/C1/DEL and bidi mark/embedding/override/isolate, plus
// line/paragraph separators and an ANSI sequence, without raw source controls.
const unsafe = [
  ...Array.from({ length: 32 }, (_, i) => String.fromCodePoint(i)),
  ...Array.from({ length: 33 }, (_, i) => String.fromCodePoint(0x7f + i)),
  ...[0x061c, 0x200e, 0x200f, 0x2028, 0x2029, 0x202a, 0x202b, 0x202c,
    0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069].map(c => String.fromCodePoint(c)),
  "\u001b[31mforged",
];

for (const field of ["branch", "defaultBranch"] as const) {
  test("delta branch safety: " + field + " rejects controls before logs, Git, FS or callbacks", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const log = createLogger("debug");
    Object.defineProperty(f.cache, "log", { value: log });
    let effects = 0;
    f.hooks.prepare = async () => { effects++; };
    f.hooks.beforePair = async () => { effects++; };
    f.cache.beforePhase = async () => { effects++; };
    const calls: string[] = [];
    const lstat = fs.lstat.bind(fs), open = fs.open.bind(fs), mkdir = fs.mkdir.bind(fs),
      opendir = fs.opendir.bind(fs), rename = fs.rename.bind(fs), unlink = fs.unlink.bind(fs), rmdir = fs.rmdir.bind(fs);
    // Observe every filesystem entry point used by the importer, including cleanup.
    const observations = [
      t.mock.method(fs, "lstat", (...args: Parameters<typeof fs.lstat>) => { calls.push("lstat"); return lstat(...args); }),
      t.mock.method(fs, "open", (...args: Parameters<typeof fs.open>) => { calls.push("open"); return open(...args); }),
      t.mock.method(fs, "mkdir", (...args: Parameters<typeof fs.mkdir>) => { calls.push("mkdir"); return mkdir(...args); }),
      t.mock.method(fs, "opendir", (...args: Parameters<typeof fs.opendir>) => { calls.push("opendir"); return opendir(...args); }),
      t.mock.method(fs, "rename", (...args: Parameters<typeof fs.rename>) => { calls.push("rename"); return rename(...args); }),
      t.mock.method(fs, "unlink", (...args: Parameters<typeof fs.unlink>) => { calls.push("unlink"); return unlink(...args); }),
      t.mock.method(fs, "rmdir", (...args: Parameters<typeof fs.rmdir>) => { calls.push("rmdir"); return rmdir(...args); }),
    ];
    const lines: string[] = [];
    const stdout = t.mock.method(process.stdout, "write", ((line: string | Uint8Array) => {
      lines.push(String(line)); return true;
    }) as typeof process.stdout.write);
    const stderr = t.mock.method(process.stderr, "write", ((line: string | Uint8Array) => {
      lines.push(String(line)); return true;
    }) as typeof process.stderr.write);
    try {
      for (const control of unsafe) {
        const name = "saved-" + control + "-branch";
        f.request[field] = name;
        await assert.rejects(f.run(), (error: unknown) => {
          assert.ok(error instanceof Error);
          assert.equal(error.message, reason);
          return true;
        }, field + " " + JSON.stringify(control));
        assert.equal(lines.length, 0, "importer emits nothing, including escaped unsafe names");
        assert.deepEqual(calls, [], "no filesystem observation or mutation");
        assert.equal(f.requests.length, 0, "no Git dispatch");
        assert.equal(effects, 0, "no owner callback or transfer phase");
        assert.equal(f.pairs(), 0, "no custody effect");
        assert.deepEqual(f.trace, []);
        assert.ok(f.charges.every(c => c.resource === "metadataBytes"), "only request validation is charged");
        // Exercise the real rendering of the fixed reason after the boundary refuses.
        log.warn("recovery: terminal origin refused", { reason });
        assert.equal(lines.length, 1);
        assert.equal(lines[0]!.includes(name), false);
        const rendered = JSON.parse(lines[0]!);
        assert.equal(rendered.reason, reason);
        assert.deepEqual(Object.keys(rendered).sort(), ["level", "msg", "reason", "ts"]);
        lines.length = 0;
      }
    } finally {
      stdout.mock.restore(); stderr.mock.restore();
      for (const observation of observations) observation.mock.restore();
    }
    await f.retained();
  });

  test("delta branch safety: previously stored " + field + " is revalidated", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    const name = "saved-" + String.fromCodePoint(0x202e) + "-branch";
    await f.native(f.bare, ["update-ref", "refs/heads/" + name, f.base]);
    await f.native(f.bare, ["config", "uzi-inventory.context", JSON.stringify({ [field]: name })]);
    const stored = JSON.parse(await f.native(f.bare, ["config", "--get", "uzi-inventory.context"]));
    f.request[field] = stored[field];
    const refs = await f.refs();
    await assert.rejects(f.run(), { message: reason });
    assert.equal(f.requests.length, 0);
    assert.equal(f.pairs(), 0);
    assert.equal(await f.refs(), refs);
    await f.retained();
  });
}

for (const name of [
  "caf\u00e9/\u4e3b\u9898-\ud83d\ude80\u00a0release",
  Array.from({ length: 7 }, (_, i) => "x".repeat(160) + i).join("/"),
]) {
  test("delta branch safety: valid Unicode/NBSP or long names retain literal anchors", linux, async t => {
    const f = await maintenanceDeltaFixture(t);
    // Move the stored family to the Unicode branch, so both request fields
    // exercise real family validation and exact ref selection.
    for (const [prefix, suffix] of [["uzi-attempts.", ".entry"], ["uzi-recovery.", ".clone"]]) {
      const key = prefix + "feature" + suffix;
      const value = await f.native(f.bare, ["config", "--get", key]);
      await f.native(f.bare, ["config", prefix + name + suffix, value]);
      await f.native(f.bare, ["config", "--unset", key]);
    }
    for (const ref of ["refs/remotes/origin/main", "refs/heads/main", "refs/uzi-runner/feature"])
      await f.native(f.bare, ["update-ref", "-d", ref]);
    await f.native(f.bare, ["update-ref", "refs/remotes/origin/" + name, f.base]);
    await f.native(f.bare, ["update-ref", "refs/uzi-runner/" + name, f.base]);
    f.request.branch = name; f.request.defaultBranch = name;
    const refs = await f.refs();
    assert.deepEqual(await f.run(), { kind: "imported", sourceSha: f.tip });
    const selection = f.requests.find(r => r.argv.includes("for-each-ref"));
    assert.ok(selection);
    assert.ok(selection.argv.includes("refs/remotes/origin/" + name));
    assert.ok(selection.argv.includes("refs/uzi-runner/" + name));
    assert.equal(f.requests.filter(canonicalImport).length, 1);
    assert.equal(await f.refs(), refs);
    assert.equal((await fs.lstat(f.clone)).isDirectory(), true, "source retained");
    assert.equal(await f.native(f.bare, ["config", "--get", "uzi-recovery." + name + ".clone"]),
      JSON.stringify({ runId: f.source.runId, clonePath: f.clone, attemptId: f.source.attemptId }));
  });
}
