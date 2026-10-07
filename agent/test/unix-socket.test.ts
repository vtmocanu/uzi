import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn, type ChildProcess } from "node:child_process";
import { listenUnix, shortUnixSocket } from "./unix-socket.js";

// Issue #2044: the TMPDIR a Codex run gets, rebuilt from its parts.
const CODEX_TMPDIR_BYTES = "/tmp/uzi-codex-command-".length + 36 + "/uzi-tmpdir-guard.XXXXXX".length;

// The base the helpers run under: os.tmpdir() itself when it is too long to pad (a real Codex
// run, where it is already Codex-length), else a fresh dir under it padded to exactly that length. Never a wrapper dir under a
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

if (process.platform === "linux") {
  it("successive directory-FD socket lifetimes isolate HTTP endpoints when the FD is reused", async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "socket-reuse-"));
    const cwd = process.cwd();
    const base = path.join(root, "p".repeat(Math.max(1, 104 - Buffer.byteLength(root))));
    fs.mkdirSync(base);
    const makeFixture = (): ReturnType<typeof shortUnixSocket> => {
      try {
        process.chdir(root);
        return shortUnixSocket(base);
      } finally {
        process.chdir(cwd);
      }
    };
    let fixture: ReturnType<typeof shortUnixSocket> | undefined;
    const reserved: number[] = [];
    try {
      fixture = makeFixture();
      const firstSocket = fixture.socket;
      const firstFd = fixture.directoryFd;
      assert.ok(firstFd !== undefined);
      // Reserve lower slots released by HTTP so the next factory reuses this directory FD.
      for (const body of ["first daemon", "second daemon"]) {
        const socket = fixture.socket;
        const server = http.createServer((_req, res) => res.end(body));
        try {
          await listenUnix(server, fixture.socket);
          const response = await new Promise<string>((resolve, reject) => {
            const req = http.get({ socketPath: socket, path: "/ping" }, res => {
              let received = "";
              res.on("data", chunk => received += chunk);
              res.on("end", () => resolve(received));
            });
            req.on("error", reject);
            req.setTimeout(3000, () => req.destroy(new Error("HTTP timeout")));
          });
          assert.equal(response, body);
        } finally {
          server.closeAllConnections();
          await new Promise<void>(resolve => server.close(() => resolve()));
          if (body === "first daemon") {
            // At most firstFd + 1 opens fill lower slots; an open failure aborts this test.
            for (let attempt = 0; attempt <= firstFd; attempt++) {
              const fd = fs.openSync(root, fs.constants.O_RDONLY | fs.constants.O_DIRECTORY);
              reserved.push(fd);
              if (fd > firstFd) break;
            }
          }
          fixture.dispose();
        }
        if (body === "first daemon") {
          fixture = makeFixture();
          assert.equal(fixture.directoryFd, firstFd, "the parent directory FD is recycled");
          assert.ok(path.isAbsolute(fixture.socket));
          assert.notEqual(fixture.socket, firstSocket, "factory lifetimes have distinct HTTP pool keys");
        }
      }
    } finally {
      fixture?.dispose();
      for (const fd of reserved) fs.closeSync(fd);
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("long physical socket round-trips HTTP through owned directory FDs in child and grandchild", async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "socket-fd-"));
    const cwd = process.cwd();
    const base = path.join(root, "p".repeat(Math.max(1, 104 - Buffer.byteLength(root))));
    fs.mkdirSync(base);
    let fixture: ReturnType<typeof shortUnixSocket> | undefined;
    let child: ChildProcess | undefined;
    let childExit: Promise<void> | undefined;
    const body = "owned socket HTTP";
    const server = http.createServer((_req, res) => res.end(body));
    try {
      try {
        process.chdir(root);
        fixture = shortUnixSocket(base);
      } finally {
        process.chdir(cwd);
      }
      const { socket, directoryFd } = fixture;
      assert.ok(directoryFd !== undefined);
      assert.ok(socket.startsWith(`/dev/fd/${directoryFd}/`));
      const childSocket = socket.replace(`/dev/fd/${directoryFd}/`, "/dev/fd/3/");
      assert.ok(path.isAbsolute(socket));
      assert.ok(Buffer.byteLength(socket) <= 103);
      const physicalDir = fs.realpathSync(path.dirname(socket));
      const physical = path.join(physicalDir, path.basename(socket));
      assert.ok(physicalDir.startsWith(`${root}${path.sep}`));
      assert.ok(Buffer.byteLength(physical) > 103);
      assert.ok(Buffer.byteLength(path.relative(root, physical)) <= 103);
      await listenUnix(server, socket);
      assert.equal(fs.realpathSync(socket), physical);
      assert.ok(fs.lstatSync(physical).isSocket());
      assert.equal(fs.statSync(socket).ino, fs.statSync(physical).ino);
      const parentBody = await new Promise<string>((resolve, reject) => {
        const req = http.get({ socketPath: socket, path: "/ping" }, res => {
          let response = "";
          res.on("data", chunk => response += chunk);
          res.on("end", () => resolve(response));
        });
        req.on("error", reject);
        req.setTimeout(3000, () => req.destroy(new Error("HTTP timeout")));
      });
      assert.equal(parentBody, body);
      const request = `
const http = require("node:http");
const req = http.get({ socketPath: ${JSON.stringify(childSocket)}, path: "/ping" }, res => {
  let body = "";
  res.on("data", chunk => body += chunk);
  res.on("end", () => {
    if (res.statusCode !== 200 || body !== ${JSON.stringify(body)}) process.exitCode = 1;
    else process.stdout.write(body + "\\n");
  });
});
req.on("error", err => { console.error(err.message); process.exitCode = 1; });
req.setTimeout(3000, () => req.destroy(new Error("HTTP timeout")));
`;
      const cli = `
const { spawn } = require("node:child_process");
const child = spawn(process.execPath, ["-e", process.argv[1]], {
  cwd: "/", stdio: ["ignore", "inherit", "inherit", 3]
});
process.on("SIGTERM", () => child.kill("SIGTERM"));
child.on("error", err => { console.error(err.message); process.exitCode = 1; });
child.on("exit", (code, signal) => { if (code !== 0 || signal) process.exitCode = 1; });
${request}`;
      child = spawn(process.execPath, ["-e", cli, request], {
        cwd: "/", stdio: ["ignore", "pipe", "pipe", directoryFd],
      });
      let stdout = "";
      let stderr = "";
      child.stdout!.on("data", chunk => stdout += chunk.toString());
      child.stderr!.on("data", chunk => stderr += chunk.toString());
      childExit = new Promise<void>((resolve, reject) => {
        child!.once("error", reject);
        child!.once("close", (code, signal) => {
          if (code === 0 && signal === null) resolve();
          else reject(new Error(`child exit ${code}/${signal}: ${stderr}`));
        });
      });
      await childExit;
      assert.deepEqual(stdout.trim().split("\n"), [body, body]);
      await new Promise<void>((resolve, reject) => server.close(err => err ? reject(err) : resolve()));
      fixture.dispose();
      fixture.dispose();
      assert.throws(() => fs.fstatSync(directoryFd), { code: "EBADF" });
      assert.equal(fs.existsSync(physicalDir), false);
      assert.equal(fs.existsSync(physical), false);
    } finally {
      if (child && child.exitCode === null && child.signalCode === null) child.kill("SIGTERM");
      await childExit?.catch(() => {});
      try {
        if (server.listening) {
          server.closeAllConnections();
          await new Promise<void>(resolve => server.close(() => resolve()));
        }
      } finally {
        fixture?.dispose();
        fs.rmSync(root, { recursive: true, force: true });
      }
    }
  });
}

const usDirs = (dir: string): string[] => fs.readdirSync(dir).filter((n) => n.startsWith("us-"));

describe("unix-socket helpers under a Codex-length TMPDIR", () => {
  it("the base is exactly Codex-length, or is os.tmpdir() itself when that is too long to pad", () => {
    assert.ok(Buffer.byteLength(base) === CODEX_TMPDIR_BYTES || base === os.tmpdir(), `base is ${Buffer.byteLength(base)} bytes`);
  });

  it("listenUnix rejects a listen error the length bound does not catch, instead of hanging", async () => {
    const short = shortUnixSocket(base);
    const socket = path.join(short.directoryFd === undefined ? base : path.dirname(short.socket), "us-missing", "d.sock");
    const server = http.createServer();
    try {
      // ENOENT on a plain host; EACCES inside the Landlock command sandbox. Either is a listen error.
      await assert.rejects(listenUnix(server, socket), (err: NodeJS.ErrnoException) => err.syscall === "listen" && typeof err.code === "string");
      assert.equal(server.listening, false);
    } finally {
      short.dispose();
    }
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

  it("shortUnixSocket handles an over-long physical and cwd-relative base with owned cleanup", async () => {
    const long = fs.mkdtempSync(path.join(base, "q".repeat(120)));
    const cwd = process.cwd();
    let fixture: ReturnType<typeof shortUnixSocket> | undefined;
    const server = http.createServer((req, res) => {
      res.writeHead(200);
      res.end(req.url === "/ping" ? "long-base pong" : "unexpected path");
    });
    try {
      if (process.platform !== "linux") {
        assert.throws(() => shortUnixSocket(long), /bytes/);
        assert.deepEqual(usDirs(long), []);
        return;
      }
      fixture = shortUnixSocket(long);
      const { socket, directoryFd } = fixture;
      assert.ok(directoryFd !== undefined);
      assert.ok(socket.startsWith(`/dev/fd/${directoryFd}/`));
      assert.ok(path.isAbsolute(socket));
      assert.ok(Buffer.byteLength(socket) <= 103);
      const physicalDir = fs.realpathSync(path.dirname(socket));
      const physical = path.join(physicalDir, path.basename(socket));
      assert.equal(path.dirname(physicalDir), fs.realpathSync(long));
      assert.ok(path.basename(physicalDir).startsWith("us-"));
      assert.ok(Buffer.byteLength(physical) > 103);
      assert.ok(Buffer.byteLength(path.relative(cwd, physical)) > 103);
      assert.equal(process.cwd(), cwd);
      await listenUnix(server, socket);
      assert.equal(fs.realpathSync(socket), physical);
      assert.ok(fs.lstatSync(physical).isSocket());
      assert.equal(fs.statSync(socket).ino, fs.statSync(physical).ino);
      const body = await new Promise<string>((resolve, reject) => {
        const req = http.get({ socketPath: socket, path: "/ping" }, res => {
          assert.equal(res.statusCode, 200);
          let received = "";
          res.on("data", chunk => received += chunk);
          res.on("end", () => resolve(received));
        });
        req.on("error", reject);
        req.setTimeout(3000, () => req.destroy(new Error("HTTP timeout")));
      });
      assert.equal(body, "long-base pong");
      await new Promise<void>((resolve, reject) => server.close(err => err ? reject(err) : resolve()));
      fixture.dispose();
      fixture.dispose();
      assert.throws(() => fs.fstatSync(directoryFd), { code: "EBADF" });
      assert.equal(fs.existsSync(physicalDir), false);
      assert.equal(fs.existsSync(physical), false);
      assert.deepEqual(usDirs(long), []);
      assert.equal(process.cwd(), cwd);
    } finally {
      try {
        if (server.listening) {
          server.closeAllConnections();
          await new Promise<void>(resolve => server.close(() => resolve()));
        }
      } finally {
        fixture?.dispose();
        fs.rmSync(long, { recursive: true, force: true });
      }
    }
  });

  if (process.platform === "linux") {
    it("shortUnixSocket closes its acquired FD and removes its directory when alias verification fails", t => {
      const long = fs.mkdtempSync(path.join(base, "q".repeat(120)));
      const realpathSync = fs.realpathSync;
      const failure = new Error("injected alias verification failure");
      let openedFd: number | undefined;
      try {
        t.mock.method(fs, "realpathSync", (target: fs.PathLike) => {
          if (typeof target === "string" && target.startsWith("/dev/fd/")) {
            openedFd = Number(path.basename(target));
            assert.ok(fs.fstatSync(openedFd).isDirectory(), "the captured FD was actually opened");
            throw failure;
          }
          return realpathSync(target);
        });
        assert.throws(() => {
          const unexpected = shortUnixSocket(long);
          unexpected.dispose();
        }, err => err === failure);
        const directoryFd = openedFd;
        assert.ok(directoryFd !== undefined);
        assert.throws(() => fs.fstatSync(directoryFd), { code: "EBADF" });
        assert.deepEqual(usDirs(long), []);
      } finally {
        t.mock.restoreAll();
        fs.rmSync(long, { recursive: true, force: true });
      }
    });
  }

  it("shortUnixSocket under a Codex-length base gives a bindable socket within the bound that round-trips a request", async () => {
    const { socket, dispose } = shortUnixSocket(base);
    const server = http.createServer((req, res) => {
      res.writeHead(200);
      res.end(req.url === "/ping" ? "pong /ping" : "unexpected path");
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
