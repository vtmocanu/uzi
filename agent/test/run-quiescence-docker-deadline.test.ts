import { describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { dockerRunningContainers, recordMaintenanceBootstrap, teardownDocker, terminalMaintenanceProof } from "../src/run-quiescence.js";

async function listen(server: http.Server): Promise<number> {
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return (server.address() as { port: number }).port;
}

async function shutdown(server: http.Server): Promise<void> {
  server.closeAllConnections();
  await new Promise<void>((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
}

describe("teardownDocker overall deadline", () => {
  it("no new deletion starts after the overall deadline", async () => {
    let clock = 0;
    let deletions = 0;
    const server = http.createServer((req, res) => {
      if (req.method === "GET") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify(Array.from({ length: 8 }, (_, i) => ({ Id: `c${i}`, Mounts: [{ Source: "/clone" }] }))));
      } else {
        deletions++;
        clock += 5000;
        res.writeHead(204);
        res.end();
      }
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    try {
      const address = server.address() as { port: number };
      const result = await teardownDocker({
        dockerHost: `tcp://127.0.0.1:${address.port}`,
        targetPaths: ["/clone"],
        now: () => clock,
        deadlineMs: 15000,
      });
      assert.equal(result.state, "docker_error");
      assert.ok(deletions <= 3, `started ${deletions} deletions, simulated elapsed ${clock}ms`);
    } finally {
      await new Promise<void>((resolve, reject) => server.close((err) => err ? reject(err) : resolve()));
    }
  });

  it("a listing whose body trickles forever is cut off at the overall deadline, not the inactivity timeout", { timeout: 10_000 }, async () => {
    const timers = new Set<NodeJS.Timeout>();
    const server = http.createServer((_req, res) => {
      res.writeHead(200, { "content-type": "application/json" });
      res.write("[");
      const t = setInterval(() => res.write(" "), 100);
      timers.add(t);
      res.on("close", () => clearInterval(t));
    });
    const port = await listen(server);
    try {
      const started = Date.now();
      const result = await teardownDocker({
        dockerHost: `tcp://127.0.0.1:${port}`,
        targetPaths: ["/clone"],
        deadlineMs: 1500,
        requestTimeoutMs: 5000,
      });
      const elapsed = Date.now() - started;
      assert.equal(result.state, "docker_error");
      assert.ok(elapsed < 3000, `teardown took ${elapsed}ms against a 1500ms deadline`);
    } finally {
      for (const t of timers) clearInterval(t);
      await shutdown(server);
    }
  });

  it("a listing that sends headers then stalls is cut off at the overall deadline", { timeout: 10_000 }, async () => {
    const server = http.createServer((_req, res) => {
      res.writeHead(200, { "content-type": "application/json" });
      res.flushHeaders();
    });
    const port = await listen(server);
    try {
      const started = Date.now();
      const result = await teardownDocker({
        dockerHost: `tcp://127.0.0.1:${port}`,
        targetPaths: ["/clone"],
        deadlineMs: 1500,
        requestTimeoutMs: 5000,
      });
      const elapsed = Date.now() - started;
      assert.equal(result.state, "docker_error");
      assert.ok(elapsed < 3000, `teardown took ${elapsed}ms against a 1500ms deadline`);
    } finally {
      await shutdown(server);
    }
  });

  it("the inter-pass sleep is capped at the time left and no listing starts once the deadline has passed", { timeout: 10_000 }, async () => {
    const deadlineMs = 2500;
    let clock = 0;
    const slept: Array<{ ms: number; remaining: number }> = [];
    const lateGets: number[] = [];
    let gets = 0;
    const server = http.createServer((req, res) => {
      if (req.method === "GET") {
        gets++;
        if (clock >= deadlineMs) lateGets.push(clock);
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify([{ Id: "c0", Mounts: [{ Source: "/clone" }] }]));
      } else {
        clock += 100;
        res.writeHead(204);
        res.end();
      }
    });
    const port = await listen(server);
    try {
      const result = await teardownDocker({
        dockerHost: `tcp://127.0.0.1:${port}`,
        targetPaths: ["/clone"],
        now: () => clock,
        deadlineMs,
        intervalMs: 1000,
        sleep: async (ms) => {
          slept.push({ ms, remaining: deadlineMs - clock });
          clock += ms;
        },
      });
      assert.equal(result.state, "docker_error");
      // The loop-top check must stop it, not a GET dispatched with a zero timeout that dies in flight.
      assert.match(result.detail, /deadline passed before the container listing/);
      assert.ok(slept.length > 0, "the daemon never went clean, so teardown must have slept between passes");
      for (const s of slept) assert.ok(s.ms <= s.remaining, `slept ${s.ms}ms with only ${s.remaining}ms left`);
      assert.deepEqual(lateGets, [], `listing(s) started at or after the deadline, at simulated ${lateGets.join(", ")}ms`);
      assert.equal(gets, 3);
    } finally {
      await shutdown(server);
    }
  });

  it("a responsive daemon still yields docker_unconfirmed after two clean listings", async () => {
    let listings = 0;
    const deleted = new Set<string>();
    const server = http.createServer((req, res) => {
      if (req.method === "GET") {
        listings++;
        const live = ["c0", "c1"].filter((id) => !deleted.has(id));
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify(live.map((Id) => ({ Id, Mounts: [{ Source: "/clone/sub" }] }))));
      } else {
        deleted.add(decodeURIComponent((req.url ?? "").split("/")[2]!.split("?")[0]!));
        res.writeHead(204);
        res.end();
      }
    });
    const port = await listen(server);
    try {
      const result = await teardownDocker({
        dockerHost: `tcp://127.0.0.1:${port}`,
        targetPaths: ["/clone"],
        intervalMs: 10,
      });
      assert.equal(result.state, "docker_unconfirmed");
      assert.deepEqual([...result.removed].sort(), ["c0", "c1"]);
      assert.equal(listings, 3);
    } finally {
      await shutdown(server);
    }
  });
});

describe("dockerRequest cancellation is joined", () => {
  // The plain (unscoped) request used to reject BEFORE the socket's close event; a caller that then reported the step settled
  // left a closing socket behind. The rejection must follow the request's close (and the response's, when one arrived).
  for (const peer of ["silent", "headers then stalled body"] as const) {
    it(`an abort while the peer is ${peer} settles only after the request and response closed`, { timeout: 10_000 }, async () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "docker-join-"));
      const sock = path.join(dir, "d.sock");
      const sockets = new Set<net.Socket>();
      const server = peer === "silent"
        ? net.createServer(c => { sockets.add(c); c.on("error", () => undefined); })
        : http.createServer((_req, res) => { res.writeHead(200, { "content-type": "application/json" }); res.flushHeaders(); });
      await new Promise<void>(resolve => server.listen(sock, resolve));
      const realRequest = http.request;
      let requestClosed = false, responseSeen = false, responseClosed = false;
      (http as { request: unknown }).request = (...args: unknown[]) => {
        const req = (realRequest as (...a: unknown[]) => http.ClientRequest)(...args);
        req.on("close", () => { requestClosed = true; });
        req.on("response", r => { responseSeen = true; r.on("close", () => { responseClosed = true; }); });
        return req;
      };
      try {
        const controller = new AbortController();
        setTimeout(() => controller.abort(), 300);
        assert.equal(await dockerRunningContainers(`unix://${sock}`, controller.signal), "unknown");
        assert.equal(requestClosed, true, "the request socket was still closing when the call settled");
        if (peer !== "silent") assert.ok(responseSeen);
        assert.equal(responseClosed, responseSeen, "the response was still closing when the call settled");
      } finally {
        (http as { request: unknown }).request = realRequest;
        if (server instanceof http.Server) server.closeAllConnections();
        for (const c of sockets) c.destroy();
        await new Promise<void>(resolve => server.close(() => resolve()));
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });
  }
});

describe("terminalMaintenanceProof under a caller bound", () => {
  it("its process scan and inter-scan sleep end at the bound, and nothing starts once the bound has passed", { timeout: 20_000 }, async () => {
    recordMaintenanceBootstrap([]);
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "proof-bound-"));
    const sock = path.join(dir, "d.sock");
    const sockets = new Set<net.Socket>();
    let accepted = 0;
    const server = net.createServer(c => { accepted++; sockets.add(c); c.resume(); c.on("error", () => undefined); });
    await new Promise<void>(resolve => server.listen(sock, resolve));
    try {
      // Three scans 250 ms apart take ~650 ms unbounded; the bound must cut that short (the scan itself is not quiescent here,
      // so the daemon is never reached: its own bound is covered by the dockerRunningContainers tests).
      const window = 300;
      const started = Date.now();
      const bound = { signal: AbortSignal.timeout(window), deadline: started + window };
      assert.equal(await terminalMaintenanceProof(3, `unix://${sock}`, bound), false, "no proof was given");
      const elapsed = Date.now() - started;
      assert.ok(elapsed < window + 250, `proof returned ${elapsed} ms into a ${window} ms bound`);
      const seen = accepted;
      const late = Date.now();
      assert.equal(await terminalMaintenanceProof(3, `unix://${sock}`, { signal: AbortSignal.timeout(1), deadline: late - 1 }), false);
      assert.ok(Date.now() - late < 250);
      await new Promise(r => setTimeout(r, 50));
      assert.equal(accepted, seen, "no request after the bound passed");
    } finally {
      for (const c of sockets) c.destroy();
      await new Promise<void>(resolve => server.close(() => resolve()));
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
