import { it, describe } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, mkdir, writeFile, rm, rename, symlink } from "node:fs/promises";
import { resolve, join } from "node:path";
import { CodexCallbackBroker, rangeScanBytes, type FileopRequest, type FileopResponse } from "../src/codex/broker.js";
import { FileopHelperClient } from "../src/codex/fileop-client.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

function assertContentEqual(actual: unknown, expected: string): void {
  assert.equal(typeof actual, "string");
  const content = actual as string;
  const length = Math.max(content.length, expected.length);
  for (let index = 0; index < length; index++) {
    if (content[index] !== expected[index]) {
      const start = Math.max(0, index - 16), end = index + 17;
      assert.fail(`content mismatch at index ${index} (lengths ${content.length}/${expected.length}): actual ${JSON.stringify(content.slice(start, end))}, expected ${JSON.stringify(expected.slice(start, end))}`);
    }
  }
}

const cap = 64 * 1024;
const oversize: FileopResponse = { ok: false, origin: "helper", code: "E_OVERSIZE" };
const body = (content: string | Buffer): FileopResponse => {
  const bytes = Buffer.from(content);
  return { ok: true, size: bytes.length, data: bytes.toString("base64") };
};
const suffix = (overrides: Partial<FileopResponse> = {}): FileopResponse => ({
  ...body("last\n"), size: 2 * 1024 * 1024, tail: 1, fromEnd: true, linesReturned: 1,
  partialFirstLine: false, truncated: true, incomplete: false, scannedBytes: 32768, ...overrides,
});
function rig(op: (request: FileopRequest) => Promise<FileopResponse>, worktreePath = "/checkout") {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const calls: FileopRequest[] = [];
  const broker = new CodexCallbackBroker({
    registry, fileop: { op: async r => { calls.push(r); return op(r); } }, worktreePath,
    spawnCommand: async () => ({ code: 0, stdout: "", stderr: "" }),
    grants: { role: "coder", phase: "implement", allowedTools: new Set(["Read"]), allowedSkills: new Set(), isRoot: true },
    allowedRoles: new Set(), delegate: async () => ({ ok: false, code: "unused", message: "unused" }),
  });
  let id = 0;
  return { calls, registry, broker, read: (args: Record<string, unknown> = {}) =>
    broker.handleToolCall({ threadId: "t", turnId: "u", callId: String(++id) }, "uzi_read", { path: "x", tail: 1, ...args }, "root") };
}
function scripted(responses: FileopResponse[]) {
  let index = 0;
  return rig(async () => responses[Math.min(index++, responses.length - 1)]!);
}
function expected(content: string, size: number, tail: number, linesReturned: number, truncated = false, partialFirstLine = false) {
  return { ok: true, output: { size, content, tail, fromEnd: true, linesReturned, partialFirstLine, truncated } };
}

function assertTailResult(actual: unknown, expected: unknown, message?: string): void {
  const { output: actualOutput, ...actualRest } = actual as { output: Record<string, unknown> };
  const { output: expectedOutput, ...expectedRest } = expected as { output: Record<string, unknown> };
  assert.ok(actualOutput, message);
  const { content: actualContent, ...actualMetadata } = actualOutput;
  const { content: expectedContent, ...expectedMetadata } = expectedOutput;
  assertContentEqual(actualContent, expectedContent as string);
  assert.deepEqual(actualRest, expectedRest, message);
  assert.deepEqual(actualMetadata, expectedMetadata, message);
}

describe("tail Read ordinary and independent descriptor validation", () => {
  it("validates presence and conflicts before any helper call", async () => {
    for (const args of [
      ...[undefined, null, "1", 0, -1, 1.5, 2001, Infinity, NaN, Number.MAX_SAFE_INTEGER + 1].map(tail => ({ tail })),
      ...[undefined, null, 1, 200].flatMap(value => [{ offset: value }, { limit: value }]),
    ]) {
      const h = scripted([body("a\n")]);
      const result = await h.read(args);
      assert.equal(result.ok, false);
      if (!result.ok) assert.equal(result.code, "bad_args");
      assert.equal(h.calls.length, 0);
      assert.equal(h.registry.inFlightCallbackCount(), 0);
    }
  });

  for (const [raw, tail, content, lines, truncated, partial] of [
    ["", 1, "", 0, false, false],
    ["a\nb\n", 1, "b\n", 1, true, false],
    ["a\nb", 2, "a\nb", 2, false, false],
    ["\ufeffa\r\n\r\n尾", 2000, "\ufeffa\r\n\r\n尾", 3, false, false],
    ["\n\n", 1, "\n", 1, true, false],
    ["a".repeat(cap) + "\nlast\n", 2, "last\n", 1, true, false],
    ["a".repeat(cap - 6) + "\nlast\n", 2, "a".repeat(cap - 6) + "\nlast\n", 2, false, false],
    ["a\n" + "😀".repeat(cap / 4) + "é", 2, "😀".repeat(cap / 4 - 1) + "é", 1, true, true],
    ["x".repeat(cap) + "\n", 1, "x".repeat(cap - 1) + "\n", 1, true, true],
  ] as const) {
    it("selects small-body tail " + JSON.stringify([String(raw).slice(0, 12), tail]), async () => {
      const h = scripted([body(raw)]);
      assertTailResult(await h.read({ tail }), expected(content, Buffer.byteLength(raw), tail, lines, truncated, partial));
      assert.deepEqual(h.calls, [{ op: "read", path: "x" }]);
    });
  }

  it("rejects binary anywhere in the ordinary body and claimed truncated prefixes without fallback", async () => {
    for (const bytes of [Buffer.from([0, 10, 97, 10]), Buffer.from([255, 10, 97, 10])]) {
      const h = scripted([body(bytes)]);
      const result = await h.read();
      assert.equal(result.ok, false);
      if (!result.ok) assert.equal(result.code, "bad_args");
      assert.equal(h.calls.length, 1);
    }
    for (const truncated of [true, null, "false"]) {
      const h = scripted([{ ...body("prefix\n"), truncated } as FileopResponse]);
      assert.equal((await h.read()).ok, false);
      assert.equal(h.calls.length, 1);
    }
  });

  it("rejects invalid optional truncated types through the client while preserving forward reads", async () => {
    for (const fields of [
      ...[null, "true", 1, {}, []].map(truncated => ({ truncated })),
      {}, { truncated: false }, { truncatedInvalid: true }, { truncated: false, truncatedInvalid: true },
    ]) {
      const inbound = new PassThrough(), outbound = new PassThrough();
      const client = new FileopHelperClient({ inbound, outbound });
      outbound.on("data", chunk => {
        const request = JSON.parse(String(chunk));
        inbound.write(JSON.stringify({ id: request.id, ...body("a\n"), ...fields }) + "\n");
      });
      const h = rig(r => client.op(r));
      try {
        const result = await h.read();
        if (Object.hasOwn(fields, "truncated") && fields.truncated !== false) {
          assert.deepEqual(result, {
            ok: false, code: "fileop_denied", message: "file operation denied (E_MALFORMED)", fileopCode: "E_MALFORMED",
          });
        } else {
          assertTailResult(result, expected("a\n", 2, 1, 1));
        }
        assert.deepEqual(h.calls, [{ op: "read", path: "x" }]);
        for (const args of [{ path: "x" }, { path: "x", offset: 1, limit: 200 }]) {
          assert.deepEqual(await h.broker.handleToolCall(
            { threadId: "t", turnId: "u", callId: JSON.stringify(args) }, "uzi_read", args, "root",
          ), {
            ok: true, output: { size: 2, content: "a\n", offset: 1, linesReturned: 1, partialLastLine: false, truncated: false },
          });
        }
        assert.deepEqual(h.calls, Array.from({ length: 3 }, () => ({ op: "read", path: "x" })));
      } finally { client.close(); inbound.destroy(); outbound.destroy(); }
    }
  });

  it("falls back exactly once on helper-origin oversize, using only the second descriptor", async () => {
    const h = scripted([oversize, suffix({ offset: 99, partialLastLine: true })]);
    assertTailResult(await h.read(), expected("last\n", 2 * 1024 * 1024, 1, 1, true));
    assert.deepEqual(h.calls, [{ op: "read", path: "x" }, { op: "read_tail", path: "x", tail: 1 }]);
    assert.equal(h.registry.inFlightCallbackCount(), 0);
    for (const first of [
      { ok: false, code: "E_OVERSIZE" }, { ok: false, origin: "forged", code: "E_OVERSIZE" },
      { ok: false, origin: "helper", code: "E_IO" }, { ok: true, size: 5, data: "!" },
    ]) {
      const denied = scripted([first as FileopResponse, suffix()]);
      assert.equal((await denied.read()).ok, false);
      assert.equal(denied.calls.length, 1);
    }
    const ordinaryNotText = await scripted([{ ok: false, code: "E_NOT_TEXT" }]).read();
    assert.equal(ordinaryNotText.ok, false);
    if (!ordinaryNotText.ok) assert.equal(ordinaryNotText.code, "bad_args");
    for (const code of ["E_OVERSIZE", "E_NOT_TEXT", "E_IO", "E_TIMEOUT"]) {
      const h = scripted([oversize, { ok: false, code }]);
      const result = await h.read();
      assert.equal(result.ok, false);
      if (!result.ok) assert.equal(result.code, code === "E_NOT_TEXT" ? "bad_args" : "fileop_denied");
      assert.equal(h.calls.length, 2);
    }
  });

  it("accepts complete whole-line cap stops far below cap, partial UTF8, empty EOF and budget stops", async () => {
    const cases: [FileopResponse, number, unknown][] = [
      [suffix({ tail: 2, scannedBytes: cap + 32768 }), 2, expected("last\n", 2 * 1024 * 1024, 2, 1, true)],
      [suffix({ ...body(""), size: 0, scannedBytes: 0, linesReturned: 0, truncated: false }), 1, expected("", 0, 1, 0)],
      [suffix({ ...body("😀".repeat(cap / 4 - 1) + "é"), size: cap + 10, scannedBytes: cap + 10, partialFirstLine: true }),
        1, expected("😀".repeat(cap / 4 - 1) + "é", cap + 10, 1, 1, true, true)],
      [suffix({ size: rangeScanBytes + 1, tail: 2, incomplete: true, reason: "scan_budget", scannedBytes: rangeScanBytes }), 2,
        { ...expected("last\n", rangeScanBytes + 1, 2, 1, true), output: {
          ...(expected("last\n", rangeScanBytes + 1, 2, 1, true).output), incomplete: true, reason: "scan_budget", scannedBytes: rangeScanBytes } }],
    ];
    for (const [response, tail, result] of cases) assertTailResult(await scripted([oversize, response]).read({ tail }), result);
  });

  it("rejects malformed required metadata, encoding, count and proof combinations, also through client coercion", async () => {
    const mutations: Record<string, unknown>[] = [
      ...["data", "size", "tail", "fromEnd", "linesReturned", "partialFirstLine", "truncated", "incomplete", "scannedBytes"]
        .flatMap(key => [undefined, null, "wrong"].map(value => ({ [key]: value }))),
      ...[-1, 1.5, Infinity, NaN, Number.MAX_SAFE_INTEGER + 1, 4].map(size => ({ size })),
      ...[-1, 0, 2, 1.5].map(linesReturned => ({ linesReturned })),
      { tail: 2 }, { fromEnd: false }, { data: "bGFzdAo" }, { data: "bGFzdAp=" },
      { data: "a===", linesReturned: 0 }, { data: "A".repeat(4 * Math.ceil(cap / 3) + 1) },
      { data: Buffer.alloc(cap + 1, 97).toString("base64") },
      { data: "", linesReturned: 0 }, { truncated: false }, { partialFirstLine: true },
      ...[0, 5, -1, 1.5, rangeScanBytes + 1, 3 * 1024 * 1024].map(scannedBytes => ({ scannedBytes })),
      { reason: undefined }, { reason: null }, { reason: "scan_budget" },
      { incomplete: true }, { incomplete: true, reason: "scan_budget", scannedBytes: rangeScanBytes, size: rangeScanBytes },
      { incomplete: true, reason: "scan_budget", scannedBytes: rangeScanBytes, size: rangeScanBytes + 1 },
      { tail: 2, scannedBytes: cap }, // N not reached, no evidence of output-cap stop
    ];
    for (const mutation of mutations) {
      const response = { ...suffix(), ...mutation } as FileopResponse;
      const result = await scripted([oversize, response]).read({ tail: mutation.tail === 2 ? 2 : 1 });
      assert.equal(result.ok, false, "invalid tail fields: " + Object.keys(mutation).join(", "));
      // JSON omits an optional undefined reason; that wire response is valid.
      if (Object.hasOwn(mutation, "reason") && mutation.reason === undefined) continue;
      const inbound = new PassThrough(), outbound = new PassThrough();
      const client = new FileopHelperClient({ inbound, outbound });
      outbound.on("data", chunk => {
        const request = JSON.parse(String(chunk));
        inbound.write(JSON.stringify({ id: request.id, ...(request.op === "read" ? oversize : response) }) + "\n");
      });
      try {
        assert.equal((await rig(r => client.op(r)).read({ tail: mutation.tail === 2 ? 2 : 1 })).ok, false, "invalid wire tail fields: " + Object.keys(mutation).join(", "));
      } finally { client.close(); inbound.destroy(); outbound.destroy(); }
    }
    for (const raw of [Buffer.from([255]), Buffer.from([0])]) {
      const h = scripted([oversize, suffix({ ...body(raw), size: 2 * 1024 * 1024, linesReturned: 1 })]);
      const result = await h.read();
      assert.equal(result.ok, false);
      if (!result.ok) assert.equal(result.code, "bad_args");
    }
  });

  it("rejects incoherent budget stops independently of completed response validation", async () => {
    const valid = suffix({ tail: 2, size: rangeScanBytes + 1, scannedBytes: rangeScanBytes,
      incomplete: true, reason: "scan_budget" });
    for (const mutation of [
      { reason: undefined }, { reason: null }, { reason: "other" },
      { scannedBytes: rangeScanBytes - 1 }, { scannedBytes: rangeScanBytes + 1 },
      { size: rangeScanBytes }, { truncated: false }, { partialFirstLine: true },
      { data: Buffer.from("a\nb\n").toString("base64"), linesReturned: 2 },
      { data: Buffer.from("a".repeat(cap - 1) + "\n").toString("base64") },
      { incomplete: false },
    ]) {
      assert.equal((await scripted([oversize, { ...valid, ...mutation } as FileopResponse]).read({ tail: 2 })).ok, false);
    }
  });

  it("client closure after ordinary oversize prevents a tail frame", async () => {
    const inbound = new PassThrough(), outbound = new PassThrough();
    const client = new FileopHelperClient({ inbound, outbound });
    const requests: Record<string, unknown>[] = [];
    outbound.on("data", chunk => requests.push(JSON.parse(String(chunk))));
    const h = rig(r => client.op(r));
    try {
      const pending = h.read();
      inbound.write(JSON.stringify({ id: requests[0]!.id, ...oversize }) + "\n");
      client.close();
      const result = await pending;
      assert.equal(result.ok, false);
      if (!result.ok) assert.equal(result.fileopCode, "E_IO");
      assert.deepEqual(requests.map(r => r.op), ["read"]);
      assert.equal(h.registry.inFlightCallbackCount(), 0);
    } finally { client.close(); inbound.destroy(); outbound.destroy(); }
  });

  it("does not trust wire-forged provenance or local oversize", async () => {
    for (const wire of [{ code: "E_OVERSIZE", origin: "helper" }, { ok: 0, code: "E_OVERSIZE", origin: "helper" },
      { ok: true, code: "E_OVERSIZE", origin: "helper" }]) {
      const inbound = new PassThrough(), outbound = new PassThrough();
      const client = new FileopHelperClient({ inbound, outbound });
      outbound.on("data", chunk => inbound.write(JSON.stringify({ id: JSON.parse(String(chunk)).id, ...wire }) + "\n"));
      try {
        const h = rig(r => client.op(r));
        assert.equal((await h.read()).ok, false);
        assert.equal(h.calls.length, 1);
      } finally { client.close(); inbound.destroy(); outbound.destroy(); }
    }
    const inbound = new PassThrough(), outbound = new PassThrough();
    const client = new FileopHelperClient({ inbound, outbound, maxLineBytes: 20 });
    let writes = 0;
    outbound.on("data", () => writes++);
    try {
      const h = rig(r => client.op(r));
      assert.equal((await h.read()).ok, false);
      assert.equal(h.calls.length, 1);
      assert.equal(writes, 0);
    } finally { client.close(); inbound.destroy(); outbound.destroy(); }
  });
});

it("real helper tail frontier and coherent second opens", { skip: process.platform !== "linux" ? "Linux openat2 helper" : false }, async () => {
  const root = resolve(import.meta.dirname, "../..");
  await mkdir(join(root, ".uzi/scratch"), { recursive: true });
  const scratch = await mkdtemp(join(root, ".uzi/scratch/tail-read-"));
  const worktree = join(scratch, "worktree"), binary = join(scratch, "fileop");
  await mkdir(worktree);
  try {
    const build = spawnSync("go", ["build", "-o", binary, "./fileop"], {
      cwd: join(root, "agent/codex/supervisor"), timeout: 60000, encoding: "utf8",
      env: { ...process.env, GOTOOLCHAIN: "local", GOFLAGS: "-buildvcs=false -mod=vendor" },
    });
    assert.equal(build.status, 0, build.stderr);
    const helper = spawn(binary, ["--root", worktree], { stdio: ["pipe", "pipe", "pipe"] });
    const exited = once(helper, "exit");
    const client = new FileopHelperClient({ inbound: helper.stdout, outbound: helper.stdin });
    let between: (() => Promise<void>) | undefined;
    const h = rig(async request => {
      const response = await client.op(request);
      if (request.op === "read" && response.code === "E_OVERSIZE" && between) {
        const mutate = between; between = undefined; await mutate();
      }
      return response;
    }, worktree);
    const path = join(worktree, "x");
    const initial = "x".repeat(1024 * 1024) + "\nold\n";
    try {
      for (const bad of [Buffer.from([0]), Buffer.from([255])]) {
        await writeFile(path, Buffer.concat([bad, Buffer.from("\n" + initial)]));
        assertTailResult(await h.read(), expected("old\n", bad.length + 1 + Buffer.byteLength(initial), 1, 1, true));
        await writeFile(path, Buffer.concat([bad, Buffer.from("\nold\n")]));
        const small = await h.read();
        assert.equal(small.ok, false);
        if (!small.ok) assert.equal(small.code, "bad_args");
      }
      for (const mode of ["grow", "shrink", "replace", "delete", "symlink", "fifo"] as const) {
        await rm(path, { force: true }); await writeFile(path, initial);
        const next = mode === "grow" ? "z".repeat(2 * 1024 * 1024) + "\nnew\n" : "small\n";
        between = async () => {
          if (mode === "replace") {
            await writeFile(join(worktree, "replacement"), next); await rename(join(worktree, "replacement"), path);
          } else if (mode === "grow" || mode === "shrink") await writeFile(path, next);
          else {
            await rm(path);
            if (mode === "symlink") await symlink("target", path);
            if (mode === "fifo") assert.equal(spawnSync("mkfifo", [path], { timeout: 5000 }).status, 0);
          }
        };
        h.calls.length = 0;
        const result = await h.read();
        assert.deepEqual(h.calls.map(r => r.op), ["read", "read_tail"], mode);
        if (mode === "grow" || mode === "shrink" || mode === "replace") {
          assertTailResult(result, expected(mode === "grow" ? "new\n" : next, Buffer.byteLength(next), 1, 1, mode === "grow"), mode);
        } else {
          assert.equal(result.ok, false, mode);
          if (!result.ok) assert.equal(result.code, "fileop_denied", mode);
        }
      }
      await rm(path, { force: true });
      await writeFile(path, "x".repeat(rangeScanBytes + 1) + "\nlast\n");
      // The output cap precedes the larger shared scan budget in the current helper.
      assertTailResult(await h.read({ tail: 2 }), expected("last\n", rangeScanBytes + 7, 2, 1, true));
      await writeFile(path, "x".repeat(1024 * 1024) + "\n" + "😀".repeat(cap / 4) + "é");
      assertTailResult(await h.read(), expected("😀".repeat(cap / 4 - 1) + "é", 1024 * 1024 + 1 + cap + 2, 1, 1, true, true));
    } finally {
      client.close(); helper.stdin.end();
      const [code] = await exited; assert.equal(code, 0);
    }
  } finally { await rm(scratch, { recursive: true, force: true }); }
});
