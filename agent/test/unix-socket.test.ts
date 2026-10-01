import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { listenUnix, shortUnixSocket } from "./unix-socket.js";

// Issue #2044: the TMPDIR a Codex run gets, rebuilt from its parts.
const CODEX_TMPDIR_BYTES = "/tmp/uzi-codex-command-".length + 36 + "/uzi-tmpdir-guard.XXXXXX".length;

let root: string;
let base: string;

before(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "us-t-"));
  base = root;
  const pad = CODEX_TMPDIR_BYTES - Buffer.byteLength(root) - 1;
  if (pad >= 1) {
    base = path.join(root, "p".repeat(pad));
    fs.mkdirSync(base);
  }
});

after(() => {
  fs.rmSync(root, { recursive: true, force: true });
});

const usDirs = (dir: string): string[] => fs.readdirSync(dir).filter((n) => n.startsWith("us-"));

describe("unix-socket helpers under a Codex-length TMPDIR", () => {
  it("the base is at least as long as a Codex TMPDIR", () => {
    assert.ok(Buffer.byteLength(base) >= CODEX_TMPDIR_BYTES);
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
    const long = path.join(base, "q".repeat(120));
    fs.mkdirSync(long);
    assert.throws(() => shortUnixSocket(long), /bytes/);
    assert.deepEqual(usDirs(long), []);
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
