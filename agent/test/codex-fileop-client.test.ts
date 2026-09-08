import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";

import {
  FileopHelperClient,
  spawnFileopHelper,
  type FileopProcess,
} from "../src/codex/fileop-client.js";
import type { FileopResponse } from "../src/codex/broker.js";

// PRD #1171 (M3, milestone 2) — the production NDJSON fileop client. Every test drives
// the client over IN-MEMORY streams (no real helper binary): a `toHelper` PassThrough
// captures the request frames the client writes, and a `toClient` PassThrough injects
// scripted response lines. This exercises the real id correlation, framing, timeout and
// fail-closed transport handling.

/** Buffers newline-delimited request frames the client writes, with an awaitable next(). */
class RequestSink {
  private readonly lines: string[] = [];
  private readonly waiters: ((line: string) => void)[] = [];
  private buf = "";
  constructor(stream: PassThrough) {
    stream.setEncoding("utf8");
    stream.on("data", (chunk: string) => {
      this.buf += chunk;
      for (;;) {
        const nl = this.buf.indexOf("\n");
        if (nl < 0) break;
        const line = this.buf.slice(0, nl);
        this.buf = this.buf.slice(nl + 1);
        const w = this.waiters.shift();
        if (w) w(line);
        else this.lines.push(line);
      }
    });
  }
  next(): Promise<Record<string, unknown>> {
    return new Promise((resolve) => {
      const take = (line: string): void => resolve(JSON.parse(line) as Record<string, unknown>);
      const queued = this.lines.shift();
      if (queued !== undefined) take(queued);
      else this.waiters.push(take);
    });
  }
}

function makeClient(opts: { maxLineBytes?: number; requestTimeoutMs?: number } = {}): {
  client: FileopHelperClient;
  requests: RequestSink;
  respond: (r: Record<string, unknown>) => void;
  endHelper: () => void;
} {
  const toHelper = new PassThrough();
  const toClient = new PassThrough();
  const client = new FileopHelperClient({
    inbound: toClient,
    outbound: toHelper,
    maxLineBytes: opts.maxLineBytes,
    requestTimeoutMs: opts.requestTimeoutMs,
  });
  return {
    client,
    requests: new RequestSink(toHelper),
    respond: (r) => toClient.write(`${JSON.stringify(r)}\n`),
    endHelper: () => toClient.end(),
  };
}

describe("FileopHelperClient: request/response round trip", () => {
  it("writes a framed request and resolves the correlated response", async () => {
    const h = makeClient();
    const p = h.client.op({ op: "read", path: "src/x.ts" });
    const req = await h.requests.next();
    assert.equal(req.op, "read");
    assert.equal(req.path, "src/x.ts");
    assert.equal(typeof req.id, "number");
    h.respond({ id: req.id, ok: true, size: 5, data: Buffer.from("hello").toString("base64") });
    const res = await p;
    assert.equal(res.ok, true);
    assert.equal(res.size, 5);
    assert.equal(res.data, Buffer.from("hello").toString("base64"));
  });

  it("forwards the apply op's old+data fields on the wire", async () => {
    const h = makeClient();
    const p = h.client.op({ op: "apply", path: "f", old: "b1", data: "b2" });
    const req = await h.requests.next();
    assert.equal(req.op, "apply");
    assert.equal(req.old, "b1");
    assert.equal(req.data, "b2");
    h.respond({ id: req.id, ok: true, size: 9 });
    assert.equal((await p).ok, true);
  });

  it("forwards rename's newPath and omits absent optional fields", async () => {
    const h = makeClient();
    const p = h.client.op({ op: "rename", path: "a", newPath: "b" });
    const req = await h.requests.next();
    assert.equal(req.newPath, "b");
    assert.equal("data" in req, false);
    assert.equal("old" in req, false);
    h.respond({ id: req.id, ok: true });
    assert.equal((await p).ok, true);
  });

  it("correlates concurrent ops by id even when responses arrive OUT OF ORDER", async () => {
    const h = makeClient();
    const p1 = h.client.op({ op: "read", path: "one" });
    const p2 = h.client.op({ op: "read", path: "two" });
    const r1 = await h.requests.next();
    const r2 = await h.requests.next();
    assert.notEqual(r1.id, r2.id);
    // Respond to the SECOND first.
    h.respond({ id: r2.id, ok: true, data: "two-body" });
    h.respond({ id: r1.id, ok: true, data: "one-body" });
    assert.equal((await p1).data, "one-body");
    assert.equal((await p2).data, "two-body");
  });

  it("ignores an id-less frame and an unknown-id frame (liveness), then resolves the match", async () => {
    const h = makeClient();
    const p = h.client.op({ op: "stat", path: "f" });
    const req = await h.requests.next();
    h.respond({ ok: true }); // id-less: ignored
    h.respond({ id: 99999, ok: true }); // unknown id: ignored
    h.respond({ id: req.id, ok: true, exists: true, type: "file", size: 1 });
    const res = await p;
    assert.equal(res.exists, true);
    assert.equal(res.type, "file");
  });

  it("coerces a list response's entries defensively (drops malformed entries)", async () => {
    const h = makeClient();
    const p = h.client.op({ op: "list", path: "." });
    const req = await h.requests.next();
    h.respond({
      id: req.id,
      ok: true,
      truncated: false,
      entries: [{ name: "a", type: "file" }, { name: 5, type: "dir" }, { bogus: true }],
    });
    const res = await p;
    assert.deepEqual(res.entries, [{ name: "a", type: "file" }]);
    assert.equal(res.truncated, false);
  });
});

describe("FileopHelperClient: fail-closed transport handling", () => {
  it("resolves in-flight ops E_IO when the helper stream ends", async () => {
    const h = makeClient();
    const p = h.client.op({ op: "read", path: "f" });
    await h.requests.next();
    h.endHelper();
    const res = await p;
    assert.equal(res.ok, false);
    assert.equal(res.code, "E_IO");
  });

  it("resolves E_IO for any op after close()", async () => {
    const h = makeClient();
    h.client.close();
    const res = await h.client.op({ op: "read", path: "f" });
    assert.equal(res.ok, false);
    assert.equal(res.code, "E_IO");
  });

  it("resolves E_OVERSIZE for a request whose framed line exceeds the cap, without writing", async () => {
    const h = makeClient({ maxLineBytes: 64 });
    const p = h.client.op({ op: "write", path: "f", data: "z".repeat(200) });
    const res = await p;
    assert.equal(res.ok, false);
    assert.equal(res.code, "E_OVERSIZE");
  });

  it("resolves E_TIMEOUT when no response arrives within the deadline", async () => {
    const h = makeClient({ requestTimeoutMs: 40 });
    const p = h.client.op({ op: "read", path: "slow" });
    await h.requests.next();
    const res = await p;
    assert.equal(res.ok, false);
    assert.equal(res.code, "E_TIMEOUT");
  });
});

describe("FileopHelperClient: malformed response desync", () => {
  it("fails closed on a non-JSON response line", async () => {
    const toHelper = new PassThrough();
    const toClient = new PassThrough();
    const client = new FileopHelperClient({ inbound: toClient, outbound: toHelper });
    const p = client.op({ op: "read", path: "f" });
    toClient.write("this is not json\n");
    const res = await p;
    assert.equal(res.ok, false);
    assert.equal(res.code, "E_IO");
  });
});

describe("spawnFileopHelper: production process wiring", () => {
  it("spawns the fileop binary with the trusted --root argv and wires a working client", async () => {
    let captured: { command: string; args: readonly string[]; env: NodeJS.ProcessEnv | undefined } | undefined;
    const toHelper = new PassThrough();
    const toClient = new PassThrough();
    let stdinEnded = false;
    const fakeProc: FileopProcess = {
      stdin: Object.assign(toHelper, { end: () => { stdinEnded = true; } }) as unknown as FileopProcess["stdin"],
      stdout: toClient,
    };
    const scrubbedEnv: NodeJS.ProcessEnv = { PATH: "/codex-cmd/bin", TMPDIR: "/codex-cmd/tmp" };
    const handle = spawnFileopHelper(
      { fileopBin: "/opt/uzi-codex/bin/uzi-codex-fileop", worktreePath: "/work/tree", env: scrubbedEnv },
      {
        spawnProcess: (command, args, env) => {
          captured = { command, args, env };
          return fakeProc;
        },
      },
    );
    // Single-uid (UZI_UID_SPLIT unset in the test env): commandRootCommand passes the
    // command through unwrapped, so the argv is exactly the trusted, model-free target.
    assert.ok(captured);
    assert.equal(captured.command, "/opt/uzi-codex/bin/uzi-codex-fileop");
    assert.deepEqual(captured.args, ["--root", "/work/tree"]);
    // The command-root env is EXACTLY the caller's scrubbed env — never the worker's
    // ambient process.env (a cross-root credential-read gap). Proves spawnFileopHelper
    // forwards spec.env verbatim and does not fall back to the inherited environment.
    assert.deepEqual(captured.env, scrubbedEnv);

    // The wired client speaks over the fake stdio.
    const reqSink = new RequestSink(toHelper);
    const p = handle.client.op({ op: "stat", path: "x" });
    const req = await reqSink.next();
    toClient.write(`${JSON.stringify({ id: req.id, ok: true, exists: false } satisfies Record<string, unknown>)}\n`);
    const res: FileopResponse = await p;
    assert.equal(res.exists, false);

    await handle.dispose();
    assert.equal(stdinEnded, true);
    // After dispose the client is closed and fails closed.
    assert.equal((await handle.client.op({ op: "read", path: "x" })).code, "E_IO");
  });

  it("throws when the spawned process lacks a stdio channel", () => {
    assert.throws(
      () =>
        spawnFileopHelper(
          { fileopBin: "/bin/x", worktreePath: "/w", env: {} },
          { spawnProcess: () => ({ stdin: null, stdout: null }) },
        ),
      /missing a stdin\/stdout channel/,
    );
  });
});
