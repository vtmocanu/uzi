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
import { buildFileopHelper, readBytesReply } from "./codex-search-fakes.js";

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
  /** Overrides the read_bytes ident for a path (default "1:1:0:0"). */
  idents?: Map<string, string>;
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
      // Search never issues the legacy whole-file `read`: it falls through to the throw below.
      if (req.op === "read_bytes") {
        const body = mem.files.get(req.path);
        if (!body) return helperErr(mem.dirs.has(req.path) ? "E_IS_DIR" : "E_NOT_FOUND");
        return readBytesReply(body, req, { size: mem.sizes?.get(req.path), ident: mem.idents?.get(req.path) });
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
        if (req.op === "read_bytes") {
          const lst = await fsp.lstat(abs(req.path));
          if (lst.isSymbolicLink()) return helperErr("E_SYMLINK");
          if (lst.isDirectory()) return helperErr("E_IS_DIR");
          if (!lst.isFile()) return helperErr("E_NOT_FILE");
          const handle = await fsp.open(abs(req.path), "r");
          try {
            const st = await handle.stat({ bigint: true });
            const offset = req.offset ?? 0;
            const buf = Buffer.alloc(Math.min(req.limit ?? 0, Math.max(0, Number(st.size) - offset)));
            const { bytesRead } = await handle.read(buf, 0, buf.length, offset);
            return {
              ok: true, data: b64(buf.subarray(0, bytesRead)), size: Number(st.size), offset,
              ident: `${st.dev}:${st.ino}:${st.mtimeNs}:${st.ctimeNs}`,
            };
          } finally { await handle.close(); }
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
        if (req.op === "read_bytes" && req.path === "b.txt") { held = true; return new Promise<FileopResponse>(() => {}); }
        return undefined;
      };
      const pending = call(brokerFor(memFileop(mem)), { query: "needle" });
      for (let i = 0; i < 1000 && !held; i++) await new Promise((r) => setImmediate(r));
      assert.equal(held, true);
      mock.timers.tick(10_000);
      const out = outputOf(await pending);
      expectPartial(out, "time");
      // a.txt is 7 bytes; the abandoned b.txt request (limit 2) stays charged at its full limit.
      assert.equal(out.bytes, 7 + 2);
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
        if (req.op === "read_bytes" && req.path === "b.txt") {
          mock.timers.tick(10_001);
          return readBytesReply(Buffer.from("other\n"), req);
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
        if (req.op === "read_bytes" && req.path === "b.txt") {
          mock.timers.tick(10_001);
          return readBytesReply(Buffer.from("needle b\n"), req);
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

    it("file_size: a file that reads short of its stat size is skipped and a later file is still searched", async () => {
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

    it("returns a partial file_size result for a target that reads short of its stat size", async () => {
      const mem = flat({ "a.txt": "x" });
      mem.sizes = new Map([["a.txt", MIB + 1]]);
      // The MiB-sized request is refunded down to the single byte actually read.
      assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle", path: "a.txt" }), {
        ok: true,
        output: { matches: [], truncated: true, bytes: 1, reason: "file_size", skippedFiles: 1, notice: NOTICE },
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

  it("does not treat a client-local E_OVERSIZE (no origin) on read_bytes as a skip", async () => {
    const mem = flat({ "a.txt": "needle\n" });
    mem.hook = (req) => (req.op === "read_bytes" ? { ok: false, code: "E_OVERSIZE" } : undefined);
    assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }),
      { ok: false, code: "fileop_denied", message: "file operation denied (E_OVERSIZE)", fileopCode: "E_OVERSIZE" });
  });

  describe("chunked reads", () => {
    const denial = { ok: false, code: "fileop_denied", message: "Search file body unavailable" };
    const reads = (fileop: { ops: FileopRequest[] }) => fileop.ops.filter((op) => op.op === "read_bytes");
    const zeros = (n: number) => Buffer.alloc(n, "z");

    it("scans a 2.5 MiB file with no size limit, in 1 MiB windows, and keeps line numbers across windows", async () => {
      const body = Buffer.concat([Buffer.from("a\nneedle one\n"), zeros(2 * MIB), Buffer.from("\nb\nneedle two"), zeros(MIB / 2)]);
      const fileop = memFileop(flat({ "big.txt": body }));
      const out = outputOf(await call(brokerFor(fileop), { query: "needle" }));
      assert.deepEqual(out.matches, [
        { path: "big.txt", line: 2, text: "needle one" },
        { path: "big.txt", line: 5, text: "needle two" + "z".repeat(MIB / 2) },
      ].map((m) => ({ ...m, text: m.text.slice(0, 256) })));
      assert.equal(out.truncated, false);
      assert.equal(out.skippedFiles, undefined);
      assert.equal(out.bytes, body.length);
      assert.deepEqual(reads(fileop).map((r) => [r.offset, r.limit]), [[0, MIB], [MIB, MIB], [2 * MIB, body.length - 2 * MIB]]);
    });

    it("searches a walk from . that holds a file over 1 MiB without skipping it", async () => {
      const out = outputOf(await call(brokerFor(memFileop(flat({ "big.ts": Buffer.concat([zeros(MIB + 10), Buffer.from("\nneedle")]) }))), { query: "needle", path: "." }));
      assert.deepEqual(out.matches, [{ path: "big.ts", line: 2, text: "needle" }]);
      assert.equal(out.truncated, false);
      assert.equal("skippedFiles" in out, false);
    });

    it("reads a zero-size file with exactly one read_bytes request and refunds it", async () => {
      const fileop = memFileop(flat({ "a.txt": "" }));
      assert.deepEqual(outputOf(await call(brokerFor(fileop), { query: "needle" })), { matches: [], truncated: false, bytes: 0 });
      assert.deepEqual(reads(fileop), [{ op: "read_bytes", path: "a.txt", offset: 0, limit: 1 }]);
    });

    it("denies a zero-size stat followed by a failed open instead of treating the file as empty", async () => {
      for (const code of ["E_SYMLINK", "E_DENIED"]) {
        const mem = flat({ "a.txt": "" });
        mem.hook = (req) => (req.op === "read_bytes" ? helperErr(code) : undefined);
        assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }),
          { ok: false, code: "fileop_denied", message: `file operation denied (${code})`, fileopCode: code });
      }
    });

    it("keeps recorded limits within the 16 MiB budget and each within what remained at dispatch", async () => {
      // a.txt takes 15 MiB; b.txt's stat says 100 bytes but it grew before the read, so the helper's
      // own fstat reports its real size (read only up to its limit, then skipped); c.txt is still
      // searched and gets exactly the remaining MiB-100 before the walk stops on bytes.
      const bBody = zeros(2 * MIB);
      const mem = flat({
        "a.txt": Buffer.concat([Buffer.from("needle\n"), zeros(15 * MIB - 7)]),
        "b.txt": bBody, "c.txt": Buffer.concat([Buffer.from("needle c\n"), zeros(3 * MIB - 9)]),
      });
      mem.sizes = new Map([["b.txt", 100]]);
      mem.hook = (req) => (req.op === "read_bytes" && req.path === "b.txt" ? readBytesReply(bBody, req, { size: bBody.length }) : undefined);
      const fileop = memFileop(mem);
      const out = outputOf(await call(brokerFor(fileop), { query: "needle" }));
      assert.equal(out.reason, "bytes");
      assert.equal(out.skippedFiles, 1, "the grown file is skipped after its first window");
      assert.deepEqual(out.matches.map((m) => m.path), ["a.txt", "c.txt"], "the walk continues to the later file");
      assert.equal(out.bytes, 16 * MIB);
      let used = 0;
      for (const r of reads(fileop)) {
        assert.ok((r.limit ?? 0) >= 1 && (r.limit ?? 0) <= 16 * MIB - used, `limit ${r.limit} exceeds remaining ${16 * MIB - used}`);
        used += r.limit ?? 0;
      }
      assert.equal(used, 16 * MIB);
      assert.deepEqual(reads(fileop).filter((r) => r.path === "b.txt").map((r) => r.limit), [100]);
      assert.deepEqual(reads(fileop).filter((r) => r.path === "c.txt").map((r) => r.limit), [MIB - 100]);
    });

    it("charges nothing for a request rejected by the operations ceiling", async () => {
      const files: Record<string, string> = { "a.txt": "needle\n" };
      for (let i = 0; i < 1100; i++) files[`f${String(i).padStart(4, "0")}`] = "tiny\n";
      const fileop = memFileop(flat(files));
      const out = outputOf(await call(brokerFor(fileop), { query: "needle" }));
      assert.equal(out.reason, "operations");
      assert.equal(fileop.ops.length, 2048, "the 2049th request is never dispatched");
      // list, a.txt and f0000..f1021 (stat + read_bytes each), and the stat of f1022 are admitted; its read_bytes is the rejected 2049th.
      assert.equal(out.bytes, 7 + 1022 * 5);
    });

    it("keeps the full limit charged after a helper E_TIMEOUT and returns the earlier matches as time", async () => {
      const mem = flat({ "a.txt": Buffer.concat([Buffer.from("needle\n"), zeros(MIB + 3)]) });
      mem.hook = (req) => (req.op === "read_bytes" && req.offset! > 0 ? helperErr("E_TIMEOUT") : undefined);
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      assert.deepEqual(out.matches, [{ path: "a.txt", line: 1, text: "needle" }]);
      assert.equal(out.reason, "time");
      assert.equal(out.bytes, MIB + 10);
      assert.equal(out.skippedFiles, 0);
    });

    it("treats a client-local E_TIMEOUT (no origin) as a denial", async () => {
      const mem = flat({ "a.txt": "needle\n" });
      mem.hook = (req) => (req.op === "read_bytes" ? { ok: false, code: "E_TIMEOUT" } : undefined);
      assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }),
        { ok: false, code: "fileop_denied", message: "file operation denied (E_TIMEOUT)", fileopCode: "E_TIMEOUT" });
    });

    it("an abandoned mid-file request stops as time with its limit charged and earlier matches kept", async () => {
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      let held = false;
      const mem = flat({ "a.txt": Buffer.concat([Buffer.from("needle\n"), zeros(MIB)]) });
      mem.hook = (req) => {
        if (req.op === "read_bytes" && req.offset! > 0) { held = true; return new Promise<FileopResponse>(() => {}); }
        return undefined;
      };
      const pending = call(brokerFor(memFileop(mem)), { query: "needle" });
      for (let i = 0; i < 1000 && !held; i++) await new Promise((r) => setImmediate(r));
      assert.equal(held, true);
      mock.timers.tick(10_000);
      const out = outputOf(await pending);
      assert.deepEqual(out.matches.map((m) => m.line), [1]);
      assert.equal(out.reason, "time");
      assert.equal(out.bytes, MIB + 7);
    });

    it("discards a chunk that settles past the deadline and keeps earlier matches", async () => {
      mock.timers.enable({ apis: ["setTimeout", "Date"] });
      const mem = flat({ "a.txt": Buffer.concat([Buffer.from("needle\n"), zeros(MIB)]) });
      mem.hook = (req) => {
        if (req.op !== "read_bytes" || req.offset === 0) return undefined;
        mock.timers.tick(10_001);
        // A valid short reply: 3 of the 7 bytes requested for the final window.
        return readBytesReply(mem.files.get("a.txt")!.subarray(0, MIB + 3), req, { size: MIB + 7 });
      };
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      assert.deepEqual(out.matches.map((m) => m.line), [1]);
      assert.equal(out.reason, "time");
      assert.equal(out.bytes, MIB + 7, "a late success keeps its full charge, with no refund for a short reply");
    });

    it("denies a failure that settles after the deadline instead of reporting time", async () => {
      for (const code of ["E_SYMLINK", "E_DENIED", "E_IO"]) {
        mock.timers.enable({ apis: ["setTimeout", "Date"] });
        const mem = flat({ "a.txt": "needle\n" });
        mem.hook = (req) => {
          if (req.op !== "read_bytes") return undefined;
          mock.timers.tick(10_001);
          return helperErr(code);
        };
        assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "needle" }),
          { ok: false, code: "fileop_denied", message: `file operation denied (${code})`, fileopCode: code });
        mock.timers.reset();
      }
    });

    it("bytes mid-file: scans the carried partial line before stopping", async () => {
      const body = Buffer.concat([Buffer.from("a\n"), zeros(16 * MIB - 2 - 6), Buffer.from("needle"), zeros(4 * MIB)]);
      const out = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": body }))), { query: "needle" }));
      assert.equal(out.reason, "bytes");
      assert.equal(out.bytes, 16 * MIB);
      assert.equal(out.matches.length, 1);
      assert.equal(out.matches[0]!.line, 2);
      assert.equal(out.matches[0]!.text, "z".repeat(125) + "needle");
    });

    it("operations mid-file: stops inside a large file with its earlier matches", async () => {
      const files: Record<string, string | Buffer> = {};
      for (let i = 0; i < 1020; i++) files[`a${String(i).padStart(4, "0")}`] = "tiny\n";
      files["z.txt"] = Buffer.concat([Buffer.from("needle\n"), zeros(8 * MIB)]);
      const fileop = memFileop(flat(files));
      const out = outputOf(await call(brokerFor(fileop), { query: "needle" }));
      assert.deepEqual(out.matches, [{ path: "z.txt", line: 1, text: "needle" }]);
      assert.equal(out.reason, "operations");
      assert.equal(out.bytes, 1020 * 5 + 6 * MIB, "the rejected seventh window of z.txt is not charged");
    });

    it("time mid-file via the helper deadline keeps the carried line scan", async () => {
      const body = Buffer.concat([zeros(MIB - 6), Buffer.from("needle"), zeros(5)]);
      const mem = flat({ "a.txt": body });
      mem.hook = (req) => (req.op === "read_bytes" && req.offset! > 0 ? helperErr("E_TIMEOUT") : undefined);
      const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      assert.equal(out.reason, "time");
      assert.deepEqual(out.matches.map((m) => m.line), [1]);
    });

    it("matches: 21 matches split across windows give 20, and exactly 20 are complete", async () => {
      const needles = (n: number) => Buffer.from("needle\n".repeat(n));
      const over = outputOf(await call(brokerFor(memFileop(flat({
        "a.txt": Buffer.concat([needles(10), zeros(MIB), Buffer.from("\n"), needles(11)]),
      }))), { query: "needle" }));
      assert.equal(over.matches.length, 20);
      assert.equal(over.reason, "matches");
      assert.deepEqual(over.matches.map((m) => m.line), [...Array.from({ length: 10 }, (_, i) => i + 1), ...Array.from({ length: 10 }, (_, i) => i + 12)]);
      const exact = outputOf(await call(brokerFor(memFileop(flat({
        "a.txt": Buffer.concat([needles(10), zeros(MIB), Buffer.from("\n"), needles(10)]),
      }))), { query: "needle" }));
      assert.equal(exact.matches.length, 20);
      assert.equal(exact.truncated, false);
    });

    describe("detected change skips the file, keeps earlier matches, and the walk continues", () => {
      const run = async (mem: Mem) => {
        mem.files.set("c.txt", Buffer.from("needle c\n"));
        mem.dirs.get(".")!.entries.push({ name: "c.txt", type: "file" });
        return outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
      };
      const expectSkipped = (out: Output) => {
        assert.deepEqual([...new Set(out.matches.map((m) => m.path))], ["a.txt", "c.txt"]);
        assert.equal(out.matches.find((m) => m.path === "a.txt")!.line, 1, "the match before the change is kept");
        assert.equal(out.truncated, true);
        assert.equal(out.reason, "file_size");
        assert.equal(out.skippedFiles, 1);
      };
      // a.txt holds a complete match then a partial line ending in the match "needle" at the MiB cut.
      const body = Buffer.concat([Buffer.from("needle\n"), zeros(MIB - 7 - 6), Buffer.from("needle"), zeros(MIB)]);
      const mk = (): Mem => ({ ...flat({ "a.txt": body }) });

      it("later chunk size change", async () => {
        const mem = mk();
        mem.hook = (req) => (req.op === "read_bytes" && req.path === "a.txt" && req.offset! > 0
          ? readBytesReply(body, req, { size: body.length + 1 }) : undefined);
        const out = await run(mem);
        expectSkipped(out);
        assert.deepEqual(out.matches.filter((m) => m.path === "a.txt").map((m) => m.line), [1, 2], "the carried partial line is scanned");
      });

      it("later chunk ident change", async () => {
        const mem = mk();
        mem.hook = (req) => (req.op === "read_bytes" && req.path === "a.txt" && req.offset! > 0
          ? readBytesReply(body, req, { ident: "1:1:0:1" }) : undefined);
        expectSkipped(await run(mem));
      });

      it("short read before the stat size", async () => {
        const mem = mk();
        // The reply keeps the stat size and ident, so only the short read can reveal the change.
        mem.hook = (req) => (req.op === "read_bytes" && req.path === "a.txt" && req.offset! > 0
          ? readBytesReply(body.subarray(0, MIB + 10), req, { size: body.length }) : undefined);
        const out = await run(mem);
        expectSkipped(out);
        assert.equal(out.bytes, MIB + 10 + "needle c\n".length, "the short read refunds the unread part of its limit");
      });

      it("a change with the carried partial line holding the 21st match reports matches and the skip", async () => {
        const head = Buffer.from("needle\n".repeat(20));
        const full = Buffer.concat([head, zeros(MIB - head.length - 6), Buffer.from("needle"), zeros(MIB)]);
        const mem: Mem = flat({ "a.txt": full });
        mem.hook = (req) => (req.op === "read_bytes" && req.path === "a.txt" && req.offset! > 0
          ? readBytesReply(full, req, { ident: "1:1:0:1" }) : undefined);
        const out = outputOf(await call(brokerFor(memFileop(mem)), { query: "needle" }));
        assert.equal(out.matches.length, 20);
        assert.equal(out.truncated, true);
        assert.equal(out.reason, "matches");
        assert.equal(out.skippedFiles, 1);
      });

      it("first chunk size differs from the stat size", async () => {
        const mem = mk();
        mem.hook = (req) => (req.op === "read_bytes" && req.path === "a.txt" && req.offset === 0
          ? readBytesReply(body, req, { size: body.length + 1 }) : undefined);
        const out = await run(mem);
        assert.deepEqual(out.matches.map((m) => m.path), ["c.txt"]);
        assert.equal(out.reason, "file_size");
        assert.equal(out.skippedFiles, 1);
      });
    });

    it("denies data past the helper's reported EOF (zero-size file, one byte returned)", async () => {
      const mem = flat({ "a.txt": "" });
      mem.hook = (req) => (req.op === "read_bytes" ? { ok: true, data: b64("x"), size: 0, offset: 0, ident: "1:1:0:0" } : undefined);
      assert.deepEqual(await call(brokerFor(memFileop(mem)), { query: "x" }), denial);
    });

    describe("bad replies deny the whole search", () => {
      const reply = (make: (req: FileopRequest) => FileopResponse): Mem => {
        const mem = flat({ "a.txt": "needle\n" });
        mem.hook = (req) => (req.op === "read_bytes" ? make(req) : undefined);
        return mem;
      };
      const cases: [string, (req: FileopRequest) => FileopResponse][] = [
        ["non-canonical base64", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), data: "bmVlZGxlCg" })],
        // Right length, alphabet and padding, but the final character carries non-zero trailing bits.
        ["padded base64 with non-zero trailing bits", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), data: "bmVlZGxlCh==" })],
        ["characters outside base64", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), data: "!!!!" })],
        ["data decoding to more than the limit", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), data: b64("needle\nX") })],
        ["encoded data longer than the limit allows", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), data: b64(zeros(4096)) })],
        ["missing data", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), data: undefined })],
        ["offset mismatch", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), offset: 1 })],
        ["missing offset", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), offset: undefined })],
        ["missing size", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), size: undefined })],
        ["fractional size", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), size: 7.5 })],
        ["negative size", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), size: -1 })],
        ["missing ident", (req) => ({ ...readBytesReply(Buffer.from("needle\n"), req), ident: undefined })],
      ];
      for (const [name, make] of cases) {
        it(name, async () => {
          assert.deepEqual(await call(brokerFor(memFileop(reply(make))), { query: "needle" }), denial);
        });
      }
    });

    it("skips a file on a helper E_UNKNOWN_OP, never falls back to read, and keeps searching", async () => {
      const mem = flat({ "a.txt": "needle\n", "b.txt": "needle b\n" });
      mem.hook = (req) => (req.op === "read_bytes" && req.path === "a.txt" ? helperErr("E_UNKNOWN_OP") : undefined);
      const fileop = memFileop(mem);
      const out = outputOf(await call(brokerFor(fileop), { query: "needle" }));
      assert.deepEqual(out.matches.map((m) => m.path), ["b.txt"]);
      assert.equal(out.reason, "file_size");
      assert.equal(out.skippedFiles, 1);
      assert.equal(fileop.ops.some((op) => op.op === "read"), false);
    });

    it("denies a client-local E_UNKNOWN_OP (no origin) instead of skipping", async () => {
      const mem = flat({ "a.txt": "needle\n" });
      mem.hook = (req) => (req.op === "read_bytes" ? { ok: false, code: "E_UNKNOWN_OP" } : undefined);
      const result = await call(brokerFor(memFileop(mem)), { query: "needle" });
      assert.equal(result.ok, false);
    });

    it("never exposes ident or size metadata in the output", async () => {
      const mem = flat({ "a.txt": "needle\n" });
      mem.idents = new Map([["a.txt", "99:98:97:96"]]);
      const result = await call(brokerFor(memFileop(mem)), { query: "needle" });
      assert.deepEqual(outputOf(result), { matches: [{ path: "a.txt", line: 1, text: "needle" }], truncated: false, bytes: 7 });
      assert.doesNotMatch(JSON.stringify(result), /99:98|ident/);
    });

    describe("UTF-8 across windows and at stops", () => {
      it("matches a multibyte character split across a window boundary", async () => {
        const body = Buffer.concat([zeros(MIB - 1), Buffer.from("é\n")]);
        const out = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": body }))), { query: "é" }));
        assert.deepEqual(out.matches, [{ path: "a.txt", line: 1, text: "z".repeat(127) + "é" }]);
      });

      it("a byte ceiling cutting a valid scalar fabricates no U+FFFD match", async () => {
        const body = Buffer.concat([zeros(16 * MIB - 1), Buffer.from("é"), zeros(10)]);
        const out = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": body }))), { query: "�" }));
        assert.equal(out.reason, "bytes");
        assert.deepEqual(out.matches, []);
      });

      it("malformed bytes at a byte ceiling still match U+FFFD", async () => {
        const body = Buffer.concat([zeros(16 * MIB - 2), Buffer.from([0xe0, 0x80]), zeros(10)]);
        const out = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": body }))), { query: "�" }));
        assert.equal(out.reason, "bytes");
        assert.equal(out.matches.length, 1);
        assert.equal(out.matches[0]!.line, 1);
      });

      it("keeps a leading BOM in the matched text", async () => {
        const out = outputOf(await call(brokerFor(memFileop(flat({ "a.txt": "﻿needle\n" }))), { query: "needle" }));
        assert.deepEqual(out.matches, [{ path: "a.txt", line: 1, text: "﻿needle" }]);
      });
    });

    it("a custom child role with Read and Search gets the same large-file result as the root", async () => {
      const mem = flat({ "big.txt": Buffer.concat([zeros(MIB + 5), Buffer.from("\nneedle\n")]) });
      const child: RunGrants = { role: "custom-auditor", phase: "plan", isRoot: false, allowedTools: new Set(["Read", "Search"]), allowedSkills: new Set() };
      const viaChild = await brokerFor(memFileop(mem), "/checkout", child)
        .handleToolCall({ threadId: "t", turnId: "turn", callId: "child" }, "uzi_search", { query: "needle" }, "child");
      const viaRoot = await call(brokerFor(memFileop(mem)), { query: "needle" });
      assert.deepEqual(viaChild, viaRoot);
      assert.deepEqual(outputOf(viaRoot).matches, [{ path: "big.txt", line: 2, text: "needle" }]);
    });
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
        for (const op of ["stat", "read_bytes"]) {
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
    // The helper is built from this tree (the packaged binary may predate read_bytes) into a
    // directory under <repo>/.uzi/scratch, which also holds each test's checkout root.
    const withHelper = async (t: { skip: (m?: string) => void }, fn: (broker: CodexCallbackBroker, root: string) => Promise<void>) => {
      const built = await buildFileopHelper("search-helper-");
      if (!built) return t.skip("Linux openat2 helper");
      const root = path.join(built.dir, "root");
      await fsp.mkdir(root);
      const child = spawn(built.binary, ["--root", root], { stdio: ["pipe", "pipe", "pipe"] });
      // Registered at spawn so an exit (or signal) before teardown is never missed; a launch
      // error settles it too, so teardown cannot wait on an exit event that will never fire.
      const exited = new Promise<void>((resolve) => { child.once("exit", () => resolve()); child.once("error", () => resolve()); });
      const handle = wireFileopHelper({ stdin: child.stdin, stdout: child.stdout });
      try {
        await fn(brokerFor(handle.client, root), root);
      } finally {
        await handle.dispose();
        await exited;
        await fsp.rm(built.dir, { recursive: true, force: true });
      }
    };

    it("searches a file target, skips a listed symlink and denies a symlink target", async (t) => {
      await withHelper(t, async (broker, root) => {
        await fsp.mkdir(path.join(root, "sub"));
        await fsp.writeFile(path.join(root, "a.txt"), "x\nneedle\n");
        await fsp.writeFile(path.join(root, "sub", "b.txt"), "needle too\n");
        await fsp.symlink("a.txt", path.join(root, "link.txt"));
        assert.deepEqual(outputOf(await call(broker, { query: "needle", path: "a.txt" })),
          { matches: [{ path: "a.txt", line: 2, text: "needle" }], truncated: false, bytes: 9 });
        const dirOut = outputOf(await call(broker, { query: "needle" }));
        assert.equal(dirOut.truncated, false);
        assert.deepEqual(dirOut.matches.map((m) => m.path), ["a.txt", "sub/b.txt"]);
        assert.deepEqual(await call(broker, { query: "needle", path: "link.txt" }),
          { ok: false, code: "fileop_denied", message: "file operation denied (E_SYMLINK)", fileopCode: "E_SYMLINK" });
      });
    });

    it("searches a ~2 MiB source file past the first MiB, a literal straddling the MiB boundary once, and handles large symlinks", async (t) => {
      await withHelper(t, async (broker, root) => {
        const uniqueLiteral = "UNIQUE_BEYOND_FIRST_MIB_7f3a";
        const straddleLiteral = "STRADDLE_LITERAL_9c1d";
        const lines: string[] = [];
        let size = 0;
        const push = (line: string) => { lines.push(line); size += Buffer.byteLength(line) + 1; };
        const sourceLine = (i: number) => `export const value_${i} = compute(${i}, "item-${i}", { enabled: ${i % 2 === 0} });`;
        let i = 0;
        // Pad so the straddling literal starts 8 bytes before byte 1048576 on its own line.
        const straddleStart = MIB - 8 - "// ".length;
        while (size + sourceLine(i).length + 1 < straddleStart - 4) push(sourceLine(i++));
        push("//" + "x".repeat(straddleStart - size - 3));
        assert.equal(size, straddleStart);
        push(`// ${straddleLiteral}`);
        const straddleLine = lines.length;
        while (size < MIB + 4096) push(sourceLine(i++));
        push(`// ${uniqueLiteral}`);
        const uniqueLine = lines.length;
        while (size < 2 * MIB) push(sourceLine(i++));
        const text = lines.join("\n") + "\n";
        const at = Buffer.from(text).indexOf(straddleLiteral);
        assert.ok(at < MIB && at + straddleLiteral.length > MIB, "the literal straddles byte 1048576");
        await fsp.mkdir(path.join(root, "src"));
        await fsp.writeFile(path.join(root, "src", "big.ts"), text);
        await fsp.symlink("big.ts", path.join(root, "src", "link.ts"));

        for (const [literal, line] of [[uniqueLiteral, uniqueLine], [straddleLiteral, straddleLine]] as const) {
          for (const scope of ["src/big.ts", "src", "."]) {
            const out = outputOf(await call(broker, { query: literal, path: scope }));
            assert.deepEqual(out.matches, [{ path: "src/big.ts", line, text: `// ${literal}` }], `${literal} in ${scope}`);
            assert.equal(out.truncated, false);
            assert.equal("skippedFiles" in out, false);
          }
        }
        assert.deepEqual(await call(broker, { query: "needle", path: "src/link.ts" }),
          { ok: false, code: "fileop_denied", message: "file operation denied (E_SYMLINK)", fileopCode: "E_SYMLINK" });
      });
    });
  });
});
