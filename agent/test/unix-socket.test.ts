import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { listenUnix, shortUnixSocket } from "./unix-socket.js";

// Issue #2044: the TMPDIR a Codex run gets, rebuilt from its parts.
const CODEX_TMPDIR_BYTES = "/tmp/uzi-codex-command-".length + 36 + "/uzi-tmpdir-guard.XXXXXX".length;

// The base the helpers run under: os.tmpdir() itself when it is already Codex-length (a real
// Codex run), else a fresh dir under it padded to exactly that length. Never a wrapper dir under a
// Codex TMPDIR: that would add bytes a fixture under the same TMPDIR never pays.
let base: string;
let owned: string | undefined;

before(() => {
  const tmp = os.tmpdir();
  const pad = CODEX_TMPDIR_BYTES - Buffer.byteLength(tmp) - "/XXXXXX".length;
  if (pad >= 1) {
    owned = fs.mkdtempSync(path.join(tmp, "c".repeat(pad)));
    base = owned;
  } else {
    base = tmp;
  }
});

after(() => {
  if (owned !== undefined) fs.rmSync(owned, { recursive: true, force: true });
});

const usDirs = (dir: string): string[] => fs.readdirSync(dir).filter((n) => n.startsWith("us-"));

describe("unix-socket helpers under a Codex-length TMPDIR", () => {
  it("the base is as long as a Codex TMPDIR (or is os.tmpdir() itself)", () => {
    assert.ok(Buffer.byteLength(base) >= Math.min(CODEX_TMPDIR_BYTES, Buffer.byteLength(os.tmpdir())));
  });

  it("listenUnix rejects an over-long path promptly, naming its byte length", async () => {
    const dir = fs.mkdtempSync(path.join(base, "uzi-attempt-docker-"));
    const socket = path.join(dir, "docker.sock");
    const server = http.createServer();
    try {
      await assert.rejects(listenUnix(server, socket), (err: Error) => err.message.includes(`${Buffer.byteLength(socket)} bytes`));
      assert.equal(server.listening, false);
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("shortUnixSocket throws for a base too long to fit and leaves no us- directory", () => {
    const long = fs.mkdtempSync(path.join(base, "q".repeat(120)));
    try {
      assert.throws(() => shortUnixSocket(long), /bytes/);
      assert.deepEqual(usDirs(long), []);
    } finally {
      fs.rmSync(long, { recursive: true, force: true });
    }
  });

  it("shortUnixSocket under a Codex-length base gives a bindable socket within the bound that round-trips a request", async () => {
    const { socket, dispose } = shortUnixSocket(base);
    const server = http.createServer((req, res) => {
      res.writeHead(200);
      res.end(`pong ${req.url}`);
    });
    try {
      assert.ok(Buffer.byteLength(socket) <= 103);
      await listenUnix(server, socket);
      const body = await new Promise<string>((resolve, reject) => {
        const req = http.request({ socketPath: socket, path: "/ping" }, (res) => {
          assert.equal(res.statusCode, 200);
          const chunks: Buffer[] = [];
          res.on("data", (c: Buffer) => chunks.push(c));
          res.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
        });
        req.on("error", reject);
        req.end();
      });
      assert.equal(body, "pong /ping");
    } finally {
      await new Promise<void>((r) => {
        server.closeAllConnections();
        server.close(() => r());
      });
      dispose();
    }
    assert.equal(fs.existsSync(path.dirname(socket)), false);
  });
});
