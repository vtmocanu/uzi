import { describe, it, mock, afterEach } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fsp from "node:fs/promises";
import path from "node:path";
import os from "node:os";

import {
  CodexCallbackBroker,
  type CallbackResult,
  type FileopClient,
  type FileopRequest,
  type FileopResponse,
  type RunGrants,
} from "../src/codex/broker.js";
import { wireFileopHelper } from "../src/codex/fileop-client.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

const NOTICE = "Partial search: coverage is incomplete; a missing match does not mean the text is absent.";
const MIB = 1024 * 1024;

const grants: RunGrants = {
  role: "read-only",
  phase: "implement",
  allowedTools: new Set(["Read", "Search"]),
  allowedSkills: new Set(),
  isRoot: true,
};

function brokerFor(fileop: FileopClient, worktreePath = "/checkout", g: RunGrants = grants): CodexCallbackBroker {
  return new CodexCallbackBroker({
    registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
    spawnCommand: async () => { throw new Error("shell must not run"); },
    fileop,
    worktreePath,
    grants: g,
    delegate: async () => { throw new Error("delegation must not run"); },
  });
}

let callCounter = 0;
const call = (broker: CodexCallbackBroker, args: unknown): Promise<CallbackResult> =>
  broker.handleToolCall({ threadId: "t", turnId: "turn", callId: String(++callCounter) }, "uzi_search", args, "root");

type Output = { matches: { path: string; line: number; text: string }[]; truncated: boolean; bytes: number; reason?: string; skippedFiles?: number; notice?: string };
const outputOf = (result: CallbackResult): Output => {
  assert.equal(result.ok, true, JSON.stringify(result));
  return (result as { ok: true; output: Output }).output;
};
const b64 = (text: string | Buffer): string => Buffer.from(text).toString("base64");
const helperErr = (code: string): FileopResponse => ({ ok: false, code, origin: "helper" });

/** An in-memory fileop: directories map to entry lists, files to bodies. Errors carry origin helper. */
type MemEntry = { name: string; type: string };
interface Mem {
  dirs: Map<string, { entries: MemEntry[]; truncated?: boolean }>;
  files: Map<string, Buffer>;
  /** Overrides stat size for a path (to simulate an over-size file without allocating it). */
  sizes?: Map<string, number>;
  hook?: (req: FileopRequest) => FileopResponse | Promise<FileopResponse> | undefined;
}
function memFileop(mem: Mem): FileopClient & { ops: FileopRequest[] } {
  const ops: FileopRequest[] = [];
  return {
    ops,
    op: async (req) => {
      ops.push(req);
      const hooked = mem.hook?.(req);
      if (hooked !== undefined) return hooked;
      if (req.op === "list") {
        const dir = mem.dirs.get(req.path);
        if (dir) return { ok: true, entries: dir.entries, ...(dir.truncated ? { truncated: true } : {}) };
        return mem.files.has(req.path) ? helperErr("E_NOT_DIR") : helperErr("E_NOT_FOUND");
      }
      if (req.op === "stat") {
        const body = mem.files.get(req.path);
        if (body) return { ok: true, exists: true, type: "file", size: mem.sizes?.get(req.path) ?? body.length };
        if (mem.dirs.has(req.path)) return { ok: true, exists: true, type: "dir", size: 0 };
        return { ok: true, exists: false };
      }
      if (req.op === "read") {
        const body = mem.files.get(req.path);
        if (!body) return helperErr(mem.dirs.has(req.path) ? "E_IS_DIR" : "E_NOT_FOUND");
        if (body.length > MIB) return helperErr("E_OVERSIZE");
        return { ok: true, size: body.length, data: b64(body) };
      }
      throw new Error(`unexpected op ${req.op}`);
    },
  };
}
const flat = (files: Record<string, string | Buffer>): Mem => ({
  dirs: new Map([[".", { entries: Object.keys(files).map((name) => ({ name, type: "file" })) }]]),
  files: new Map(Object.entries(files).map(([name, body]) => [name, Buffer.from(body)])),
});

/** Real-fs fileop mimicking the Go helper's wire semantics (errors carry origin helper). */
function fsFileop(root: string): FileopClient {
  const abs = (p: string) => path.join(root, p);
  const codeOf = (e: unknown): FileopResponse => {
    const code = (e as NodeJS.ErrnoException).code;
    return helperErr(code === "ENOTDIR" ? "E_NOT_DIR" : code === "ENOENT" ? "E_NOT_FOUND" : code === "ELOOP" ? "E_SYMLINK" : "E_IO");
  };
  return {
    op: async (req) => {
      try {
        if (req.op === "list") {
          const st = await fsp.lstat(abs(req.path));
          if (st.isSymbolicLink()) return helperErr("E_SYMLINK");
          if (!st.isDirectory()) return helperErr("E_NOT_DIR");
          const entries: MemEntry[] = [];
          const dir = await fsp.opendir(abs(req.path));
          for await (const d of dir) {
            entries.push({ name: d.name, type: d.isDirectory() ? "dir" : d.isFile() ? "file" : d.isSymbolicLink() ? "symlink" : "other" });
          }
          return { ok: true, entries };
        }
        if (req.op === "stat") {
          let st;
          try { st = await fsp.lstat(abs(req.path)); } catch (e) {
            if ((e as NodeJS.ErrnoException).code === "ENOENT") return { ok: true, exists: false };
            throw e;
          }
          const type = st.isFile() ? "file" : st.isDirectory() ? "dir" : st.isSymbolicLink() ? "symlink" : "other";
          return { ok: true, exists: true, type, size: st.size };
        }
        if (req.op === "read") {
          const st = await fsp.lstat(abs(req.path));
          if (st.isSymbolicLink()) return helperErr("E_SYMLINK");
          if (st.isDirectory()) return helperErr("E_IS_DIR");
          if (!st.isFile()) return helperErr("E_NOT_FILE");
          if (st.size > MIB) return helperErr("E_OVERSIZE");
          const data = await fsp.readFile(abs(req.path));
          return { ok: true, size: data.length, data: b64(data) };
        }
      } catch (e) {
        return codeOf(e);
      }
      throw new Error(`unexpected op ${req.op}`);
    },
  };
}

afterEach(() => { mock.timers.reset(); });

/** A fixture whose root holds a.txt (one early match) plus directories of symlink/other
 *  entries, totalling exactly `total` listed entries. Every listing is complete and holds at
 *  most 4096 entries, the real helper's per-listing maximum (fileop.go maxListEntries). */
function spreadEntries(total: number): Mem {
  const perDir = 4000;
  const dirCount = Math.ceil((total - 1) / (perDir + 1));
  const mem = flat({ "a.txt": "needle\n" });
  const root: MemEntry[] = [{ name: "a.txt", type: "file" }];
  let remaining = total - 1 - dirCount;
  for (let d = 0; d < dirCount; d++) {
    const name = `d${d}`;
    root.push({ name, type: "dir" });
    const n = Math.min(perDir, remaining);
    remaining -= n;
    mem.dirs.set(name, { entries: Array.from({ length: n }, (_, i) => ({ name: `l${String(i).padStart(5, "0")}`, type: i % 2 ? "symlink" : "other" })) });
  }
  assert.equal(remaining, 0);
  mem.dirs.set(".", { entries: root });
  for (const { entries } of mem.dirs.values()) assert.ok(entries.length <= 4096);
  return mem;
}

describe("Search partial results", () => {
  it("searches beyond the old ceilings (40 dirs, 300 entries, a 70 KiB file with an early match)", async () => {
    const mem: Mem = { dirs: new Map(), files: new Map() };
    const rootEntries: MemEntry[] = [{ name: "big.txt", type: "file" }];
    mem.files.set("big.txt", Buffer.from("needle\n" + "x".repeat(70 * 1024)));
    for (let i = 0; i < 40; i++) {
      const name = `d${String(i).padStart(2, "0")}`;
      rootEntries.push({ name, type: "dir" });
      mem.dirs.set(name, { entries: Array.from({ length: 7 }, (_, j) => ({ name: `f${j}.txt`, type: "file" })) });
      for (let j = 0; j < 7; j++) mem.files.set(`${name}/f${j}.txt`, Buffer.from("plain\n"));
    }
    mem.dirs.set(".", { entries: rootEntries });
    const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
    assert.equal(out.truncated, false);
    assert.deepEqual(out.matches, [{ path: "big.txt", line: 1, text: "needle" }]);
  });

  describe("ceiling matrix", () => {
    const expectPartial = (out: Output, reason: string, skippedFiles = 0) => {
      assert.deepEqual(out.matches.map((m) => m.path), ["a.txt"]);
      assert.equal(out.truncated, true);
      assert.equal(out.reason, reason);
      assert.equal(out.skippedFiles, skippedFiles);
      assert.equal(out.notice, NOTICE);
    };

    it("directories", async () => {
      const mem = flat({ "a.txt": "needle\n" });
      const entries: MemEntry[] = [{ name: "a.txt", type: "file" }];
      for (let i = 0; i < 1030; i++) {
        const name = `d${String(i).padStart(4, "0")}`;
        entries.push({ name, type: "dir" });
        mem.dirs.set(name, { entries: [] });
      }
      mem.dirs.set(".", { entries });
      expectPartial(outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" })), "directories");
    });

    it("entries", async () => {
      // Freeze the clock so the real 10 s deadline cannot preempt this ceiling on a slow runner.
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      const mem = spreadEntries(16400);
      const fileop = memFileop(mem);
      expectPartial(outputOf(await call(brokerFor(fileop), { query: "needle" })), "entries");
      assert.equal(fileop.ops.filter((op) => op.op !== "list").length, 2, "stat and read of a.txt only: no op for symlink/other entries");
      assert.ok(fileop.ops.filter((op) => op.op === "list").length > 4, "entries accumulate across several complete listings");
    });

    it("listing", async () => {
      const mem = flat({ "a.txt": "needle\n", "b.txt": "nothing\n" });
      mem.dirs.get(".")!.truncated = true;
      expectPartial(outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" })), "listing");
    });

    it("operations", async () => {
      const files: Record<string, string> = { "a.txt": "needle\n" };
      for (let i = 0; i < 1100; i++) files[`f${String(i).padStart(4, "0")}`] = "tiny\n";
      expectPartial(outputOf(await call(brokerFor(memFileop(flat(files))), { query: "needle" })), "operations");
    });

    it("bytes", async () => {
      const filler = Buffer.alloc(MIB - 7, "z");
      const files: Record<string, Buffer> = { "a.txt": Buffer.concat([Buffer.from("needle\n"), filler]) };
      for (let i = 0; i < 16; i++) files[`b${String(i).padStart(2, "0")}.txt`] = Buffer.alloc(MIB, "z");
      const out = outputOf(await call(brokerFor(memFileop(flat(files))), { query: "needle" }));
      expectPartial(out, "bytes");
      assert.equal(out.bytes, 16 * MIB);
    });

    it("time, race: a held op past the deadline", async () => {
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      let held = false;
      const mem = flat({ "a.txt": "needle\n", "b.txt": "x\n" });
      mem.hook = (req) => {
        if (req.op === "read" && req.path === "b.txt") { held = true; return new Promise<FileopResponse>(() => {}); }
        return undefined;
      };
      const pending = call(brokerFor(memFileop(mem)), { query: "needle" });
      for (let i = 0; i < 1000 && !held; i++) await new Promise((r) => setImmediate(r));
      assert.equal(held, true);
      mock.timers.tick(10_000);
      expectPartial(outputOf(await pending), "time");
    });

    it("time, final recheck after a listing that settles normally past the deadline", async () => {
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      const mem = flat({ "a.txt": "needle\n" });
      mem.dirs.get(".")!.entries.push({ name: "z", type: "dir" });
      mem.dirs.set("z", { entries: [] });
      mem.hook = (req) => {
        if (req.op === "list" && req.path === "z") { mock.timers.tick(10_001); return { ok: true, entries: [] }; }
        return undefined;
      };
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      expectPartial(out, "time");
    });

    it("time, final read settles normally past the deadline", async () => {
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      const mem = flat({ "a.txt": "needle\n", "b.txt": "other\n" });
      mem.hook = (req) => {
        if (req.op === "read" && req.path === "b.txt") {
          mock.timers.tick(10_001);
          return { ok: true, size: 6, data: b64("other\n") };
        }
        return undefined;
      };
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      expectPartial(out, "time");
    });

    it("time, pre-scan recheck: a read settling past the deadline drops that file's matches", async () => {
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      const mem = flat({ "a.txt": "needle\n", "b.txt": "needle b\n" });
      mem.hook = (req) => {
        if (req.op === "read" && req.path === "b.txt") {
          mock.timers.tick(10_001);
          return { ok: true, size: 9, data: b64("needle b\n") };
        }
        return undefined;
      };
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      expectPartial(out, "time");
      assert.equal(out.matches.length, 1);
    });

    it("entries boundary: exactly 16384 entries are not capped, 16385 are", async () => {
      // Freeze the clock so the real 10 s deadline cannot preempt this ceiling on a slow runner.
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      const exact = outputOf(await call(brokerFor(memFileop(spreadEntries(16384))), { query: "needle" }));
      assert.equal(exact.truncated, false);
      assert.equal(exact.reason, undefined);
      const over = outputOf(await call(brokerFor(memFileop(spreadEntries(16385))), { query: "needle" }));
      assert.equal(over.truncated, true);
      assert.equal(over.reason, "entries");
    });

    it("the first stop reason wins: a truncated listing is not replaced by the matches ceiling", async () => {
      const mem = flat({ "a.txt": Array.from({ length: 21 }, () => "needle").join("\n") });
      mem.dirs.get(".")!.truncated = true;
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      assert.equal(out.matches.length, 20);
      assert.equal(out.truncated, true);
      assert.equal(out.reason, "listing");
    });

    it("file_size: an over-size file is skipped and a later file is still searched", async () => {
      const mem = flat({ "a.txt": "needle\n", "b.bin": "huge", "c.txt": "needle again\n" });
      mem.sizes = new Map([["b.bin", MIB + 1]]);
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      assert.deepEqual(out.matches.map((m) => m.path), ["a.txt", "c.txt"]);
      assert.equal(out.truncated, true);
      assert.equal(out.reason, "file_size");
      assert.equal(out.skippedFiles, 1);
      assert.equal(out.notice, NOTICE);
    });

    it("matches: 21 matches give 20, and exactly 20 are complete", async () => {
      const lines = (n: number) => Array.from({ length: n }, () => "needle").join("\n");
      const over = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": lines(21) }))), { query: "needle" }));
      assert.equal(over.matches.length, 20);
      assert.equal(over.truncated, true);
      assert.equal(over.reason, "matches");
      assert.equal(over.skippedFiles, 0);
      assert.equal(over.notice, NOTICE);
      const body = lines(20);
      const exact = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": body }))), { query: "needle" }));
      assert.deepEqual(exact, {
        matches: Array.from({ length: 20 }, (_, i) => ({ path: "a.txt", line: i + 1, text: "needle" })),
        truncated: false,
        bytes: Buffer.byteLength(body),
      });
    });
  });

  it("returns an empty incomplete result when a ceiling stops the search with no matches", async () => {
    const mem = flat({});
    mem.dirs.get(".")!.truncated = true;
    assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }), {
      ok: true,
      output: { matches: [], truncated: true, bytes: 0, reason: "listing", skippedFiles: 0, notice: NOTICE },
    });
  });

  it("denies a missing or non-array listing as an error, not a partial result", async () => {
    for (const reply of [{ ok: true }, { ok: true, entries: "nope" }] as unknown as FileopResponse[]) {
      const result = await call(brokerFor({ op: async () => reply }), { query: "needle" });
      assert.deepEqual(result, { ok: false, code: "fileop_denied", message: "Search listing unavailable" });
    }
  });

  describe("file target", () => {
    it("searches one file's matches", async () => {
      const mem = flat({ "a.txt": "x\nneedle here\n" });
      assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle", path: "a.txt" }), {
        ok: true,
        output: { matches: [{ path: "a.txt", line: 2, text: "needle here" }], truncated: false, bytes: 14 },
      });
    });

    it("returns a partial file_size result for an over-size target", async () => {
      const mem = flat({ "a.txt": "x" });
      mem.sizes = new Map([["a.txt", MIB + 1]]);
      assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle", path: "a.txt" }), {
        ok: true,
        output: { matches: [], truncated: true, bytes: 0, reason: "file_size", skippedFiles: 1, notice: NOTICE },
      });
    });

    it("denies a non-regular target (stat type other)", async () => {
      const mem = flat({});
      mem.hook = (req) => {
        if (req.op === "list") return helperErr("E_NOT_DIR");
        if (req.op === "stat") return { ok: true, exists: true, type: "other", size: 0 };
        return undefined;
      };
      const result = await call(brokerFor(memFileop(mem)), { query: "needle", path: "pipe" });
      assert.deepEqual(result, { ok: false, code: "fileop_denied", message: "Search target is not a regular file" });
    });

    it("returns the missing path's error", async () => {
      assert.deepEqual(await call(brokerFor(memFileop(flat({}))), { query: "needle", path: "gone" }),
        { ok: false, code: "fileop_denied", message: "file operation denied (E_NOT_FOUND)", fileopCode: "E_NOT_FOUND" });
    });

    it("does not treat a client-local E_NOT_DIR (no origin) as a file target", async () => {
      const mem = flat({ "a.txt": "needle\n" });
      mem.hook = (req) => (req.op === "list" ? { ok: false, code: "E_NOT_DIR" } : undefined);
      const result = await call(brokerFor(memFileop(mem)), { query: "needle", path: "a.txt" });
      assert.deepEqual(result, { ok: false, code: "fileop_denied", message: "file operation denied (E_NOT_DIR)", fileopCode: "E_NOT_DIR" });
    });

    it("denies a helper E_NOT_DIR on a subdirectory instead of searching it as a file", async () => {
      const mem = flat({ "sub": "needle\n" });
      mem.dirs.get(".")!.entries = [{ name: "sub", type: "dir" }];
      mem.hook = (req) => (req.op === "list" && req.path === "sub" ? helperErr("E_NOT_DIR") : undefined);
      const result = await call(brokerFor(memFileop(mem)), { query: "needle" });
      assert.deepEqual(result, { ok: false, code: "fileop_denied", message: "file operation denied (E_NOT_DIR)", fileopCode: "E_NOT_DIR" });
    });
  });

  it("does not treat a client-local E_OVERSIZE (no origin) on read as a skip", async () => {
    const mem = flat({ "a.txt": "needle\n" });
    mem.hook = (req) => (req.op === "read" ? { ok: false, code: "E_OVERSIZE" } : undefined);
    assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }),
      { ok: false, code: "fileop_denied", message: "file operation denied (E_OVERSIZE)", fileopCode: "E_OVERSIZE" });
  });

  it("skips helper E_OVERSIZE, truncated reads and bodies over the per-file cap", async () => {
    const mem = flat({ "a.txt": "needle\n", "b.txt": "x", "c.txt": "x", "d.txt": "x" });
    mem.hook = (req) => {
      if (req.op !== "read") return undefined;
      if (req.path === "b.txt") return helperErr("E_OVERSIZE");
      if (req.path === "c.txt") return { ok: true, data: b64("x"), truncated: true };
      if (req.path === "d.txt") return { ok: true, data: b64(Buffer.alloc(MIB + 1, "x")) };
      return undefined;
    };
    const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
    assert.equal(out.reason, "file_size");
    assert.equal(out.skippedFiles, 3);
    assert.deepEqual(out.matches.map((m) => m.path), ["a.txt"]);
  });

  describe("skips and denials", () => {
    const fsTree = async (fn: (root: string) => Promise<void>) => {
      const root = await fsp.mkdtemp(path.join(await fsp.realpath(os.tmpdir()), "search-fs-"));
      try { await fn(root); } finally { await fsp.rm(root, { recursive: true, force: true }); }
    };

    it("skips listed symlinks, FIFOs and .git, and completes", async () => {
      await fsTree(async (root) => {
        await fsp.writeFile(path.join(root, "a.txt"), "needle\n");
        await fsp.writeFile(path.join(root, "target.txt"), "needle\n");
        await fsp.symlink("target.txt", path.join(root, "link.txt"));
        await fsp.mkdir(path.join(root, ".git"));
        await fsp.writeFile(path.join(root, ".git", "config"), "needle\n");
        execFileSync("mkfifo", [path.join(root, "pipe")]);
        const out = outputOf(await call(brokerFor(fsFileop(root), root), { query: "needle" }));
        assert.equal(out.truncated, false);
        assert.deepEqual(out.matches.map((m) => m.path), ["a.txt", "target.txt"]);
      });
    });

    it("denies a direct symlink target, a direct FIFO target and a missing path", async () => {
      await fsTree(async (root) => {
        await fsp.writeFile(path.join(root, "target.txt"), "needle\n");
        await fsp.symlink("target.txt", path.join(root, "link.txt"));
        execFileSync("mkfifo", [path.join(root, "pipe")]);
        const broker = brokerFor(fsFileop(root), root);
        assert.deepEqual(await call(broker, { query: "needle", path: "link.txt" }),
          { ok: false, code: "fileop_denied", message: "file operation denied (E_SYMLINK)", fileopCode: "E_SYMLINK" });
        assert.deepEqual(await call(broker, { query: "needle", path: "pipe" }),
          { ok: false, code: "fileop_denied", message: "Search target is not a regular file" });
        assert.deepEqual(await call(broker, { query: "needle", path: "missing" }),
          { ok: false, code: "fileop_denied", message: "file operation denied (E_NOT_FOUND)", fileopCode: "E_NOT_FOUND" });
      });
    });

    it("denies E_ESCAPE/E_DENIED lists and in-walk E_IO/E_DENIED stat or read", async () => {
      for (const code of ["E_ESCAPE", "E_DENIED"]) {
        const result = await call(brokerFor({ op: async () => helperErr(code) }), { query: "needle" });
        assert.deepEqual(result, { ok: false, code: "fileop_denied", message: `file operation denied (${code})`, fileopCode: code });
      }
      for (const code of ["E_IO", "E_DENIED"]) {
        for (const op of ["stat", "read"]) {
          const mem = flat({ "a.txt": "needle\n" });
          mem.hook = (req) => (req.op === op ? helperErr(code) : undefined);
          const result = await call(brokerFor(memFileop(mem)), { query: "needle" });
          assert.deepEqual(result, { ok: false, code: "fileop_denied", message: `file operation denied (${code})`, fileopCode: code });
        }
      }
    });

    it("denies a stat without a valid size, including exists:false", async () => {
      const mem = flat({ "a.txt": "needle\n" });
      mem.hook = (req) => (req.op === "stat" ? { ok: true, exists: false } : undefined);
      assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }),
        { ok: false, code: "fileop_denied", message: "Search file size unavailable" });
    });
  });

  describe("query", () => {
    it("rejects a 257-byte query", async () => {
      const result = await call(brokerFor(memFileop(flat({}))), { query: "q".repeat(257) });
      assert.deepEqual(result, { ok: false, code: "bad_args", message: "Search requires a query of at most 256 bytes" });
    });

    it("matches regex metacharacters literally", async () => {
      const mem = flat({ "a.txt": "ab\na.*(b\nxxxx\n" });
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "a.*(b" }));
      assert.deepEqual(out.matches, [{ path: "a.txt", line: 2, text: "a.*(b" }]);
    });

    it("counts a trailing segment after the last newline as a line", async () => {
      const out = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": "x\n\nneedle" }))), { query: "needle" }));
      assert.deepEqual(out.matches, [{ path: "a.txt", line: 3, text: "needle" }]);
    });
  });

  describe("grants", () => {
    it("returns the same result for the cross-checker and a custom role and leaves the grants unchanged", async () => {
      const mk = (role: string): RunGrants => ({
        role, phase: "plan", isRoot: true, allowedTools: new Set(["Read", "Search"]), allowedSkills: new Set(),
      });
      const mem = flat({ "a.txt": "needle\n" });
      mem.dirs.get(".")!.truncated = true;
      const a = mk("cross-checker");
      const b = mk("custom-auditor");
      const ra = await call(brokerFor(memFileop(mem), "/checkout", a), { query: "needle" });
      const rb = await call(brokerFor(memFileop(mem), "/checkout", b), { query: "needle" });
      assert.deepEqual(ra, rb);
      assert.equal(outputOf(ra).truncated, true);
      for (const g of [a, b]) {
        assert.deepEqual([...g.allowedTools], ["Read", "Search"]);
        assert.equal(g.allowedSkills.size, 0);
      }
    });

    it("denies Search for a grant without it", async () => {
      const g: RunGrants = { ...grants, allowedTools: new Set(["Read"]) };
      const result = await call(brokerFor(memFileop(flat({})), "/checkout", g), { query: "needle" });
      assert.equal(result.ok, false);
      assert.equal((result as { code: string }).code, "denied_tool");
    });
  });

  describe("real fileop helper", () => {
    it("searches a file target, skips a listed symlink and denies a symlink target", async (t) => {
      const bin = "/usr/local/bin/uzi-codex-fileop";
      try { await fsp.access(bin); } catch { return t.skip("packaged fileop binary unavailable"); }
      const root = await fsp.mkdtemp(path.join(await fsp.realpath(os.tmpdir()), "search-helper-"));
      await fsp.mkdir(path.join(root, "sub"));
      await fsp.writeFile(path.join(root, "a.txt"), "x\nneedle\n");
      await fsp.writeFile(path.join(root, "sub", "b.txt"), "needle too\n");
      await fsp.symlink("a.txt", path.join(root, "link.txt"));
      const child = spawn(bin, ["--root", root], { stdio: ["pipe", "pipe", "pipe"] });
      const handle = wireFileopHelper({ stdin: child.stdin, stdout: child.stdout });
      try {
        const broker = brokerFor(handle.client, root);
        assert.deepEqual(outputOf(await call(broker, { query: "needle", path: "a.txt" })),
          { matches: [{ path: "a.txt", line: 2, text: "needle" }], truncated: false, bytes: 9 });
        const dirOut = outputOf(await call(broker, { query: "needle" }));
        assert.equal(dirOut.truncated, false);
        assert.deepEqual(dirOut.matches.map((m) => m.path), ["a.txt", "sub/b.txt"]);
        assert.deepEqual(await call(broker, { query: "needle", path: "link.txt" }),
          { ok: false, code: "fileop_denied", message: "file operation denied (E_SYMLINK)", fileopCode: "E_SYMLINK" });
      } finally {
        await handle.dispose();
        await new Promise<void>((resolve) => { if (child.exitCode !== null) resolve(); else child.once("exit", () => resolve()); });
        await fsp.rm(root, { recursive: true, force: true });
      }
    });
  });
});
