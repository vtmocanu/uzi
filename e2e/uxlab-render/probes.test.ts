import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import vm from "node:vm";
import { createHash } from "node:crypto";
import { constants } from "node:fs";
import { boundaryProbe, homeSnapshot } from "./probes.js";

const exec = promisify(execFile);
async function fixture(t: { after: (fn: () => Promise<void>) => void }) {
  const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-home-", import.meta.url)));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  return dir;
}
const snapshot = (home: string) => exec(process.execPath, ["-e", homeSnapshot], {
  env: { HOME: home }, timeout: 10_000, maxBuffer: 4 * 1024 * 1024,
});
test("standalone HOME snapshot records identity/mtime, hashes bytes and never follows links", async (t) => {
  const home = await fixture(t);
  await fs.writeFile(path.join(home, "file"), "hello");
  await fs.symlink("/does-not-exist", path.join(home, "link"));
  const first = (await snapshot(home)).stdout;
  assert.equal((await snapshot(home)).stdout, first);
  const rows = JSON.parse(first);
  const file = rows.find((row: unknown[]) => row[0] === "file");
  assert.equal(file[3], "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824");
  assert.equal(file[2].uid, String(process.getuid!()));
  assert.equal(file[2].mtimeNs, String((await fs.stat(path.join(home, "file"), { bigint: true })).mtimeNs));
  assert.equal(rows.find((row: unknown[]) => row[0] === "link")[3], "/does-not-exist");
  await fs.writeFile(path.join(home, "file"), "changed");
  assert.notEqual((await snapshot(home)).stdout === first, true, "snapshot must detect changed bytes");
});
test("standalone HOME snapshot enforces per-file, total, depth and enumeration budgets", async (t) => {
  const home = await fixture(t), file = path.join(home, "large");
  await fs.writeFile(file, "");
  await fs.truncate(file, 4194305);
  await assert.rejects(snapshot(home), /HOME byte limit/);
  await fs.unlink(file);
  for (let i = 0; i < 5; i++) {
    await fs.writeFile(path.join(home, String(i)), "");
    await fs.truncate(path.join(home, String(i)), 4194304);
  }
  await assert.rejects(snapshot(home), /HOME byte limit/);
  for (let i = 0; i < 5; i++) await fs.unlink(path.join(home, String(i)));
  const deep = path.join(home, ...Array(65).fill("d"));
  await fs.mkdir(deep, { recursive: true });
  await assert.rejects(snapshot(home), /HOME depth limit/);
  await fs.rm(path.join(home, "d"), { recursive: true });
  // Batches bound fixture setup allocations; the CLI must stop at entry 8193.
  for (let i = 0; i < 8193; i += 128)
    await Promise.all(Array.from({ length: Math.min(128, 8193 - i) }, (_, n) =>
      fs.writeFile(path.join(home, String(i + n)), "")));
  await assert.rejects(snapshot(home), /HOME entry limit/);
});
test("boundary script calibrates only disposable writes and closes unexpected real-file opens", async () => {
  const sentinel = "/nix/uxlab-sentinel-abc";
  for (const scenario of ["off", "required", "unexpected-db", "unexpected-sentinel"]) {
    const calls: string[] = [], errors: string[] = [];
    const state = { argv: ["node", sentinel, scenario === "off" ? "off" : "required"], exitCode: 0 };
    const fakeFS = {
      lstat: async () => ({ isFile: () => true, dev: 1n, ino: 2n, uid: 10002n,
        gid: 10002n, mode: 0o666n, mtimeNs: 1n, size: 0n }),
      open: async (file: string, flags: number) => {
        assert.equal(flags, constants.O_RDWR);
        calls.push("open:" + file);
        if ((file === sentinel && scenario !== "required") ||
            (file.endsWith("db.sqlite") && scenario === "unexpected-db")) return {
          close: async () => { calls.push("close:" + file); },
          write: async () => { calls.push("write:" + file); return { bytesWritten: 1 }; },
        };
        throw Object.assign(new Error("denied"), { code: "EACCES" });
      },
    };
    await vm.runInNewContext(boundaryProbe, {
      require: (name: string) => {
        if (name === "node:fs/promises") return fakeFS;
        if (name === "node:fs") return { constants };
        if (name === "node:path") return path;
        if (name === "node:crypto") return { createHash };
        throw new Error("unexpected dependency");
      },
      process: state, Buffer,
      console: { log: () => {}, error: (error: string) => { errors.push(error); } },
    }, { timeout: 1000 });
    assert.equal(state.exitCode, scenario.startsWith("unexpected") ? 1 : 0, scenario);
    assert.equal(calls.filter(call => call.startsWith("write:")).join(), scenario === "off" ? "write:" + sentinel : "");
    if (scenario === "unexpected-db") {
      assert.equal(calls.at(-1), "close:/nix/var/nix/db/db.sqlite");
      assert.equal(errors.length, 1);
    }
    if (scenario === "unexpected-sentinel") assert.equal(calls.at(-1), "close:" + sentinel);
  }
});
